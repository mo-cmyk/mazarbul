package doctor

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
	bbolterrors "go.etcd.io/bbolt/errors"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/rewriter"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/ladybug"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
	"github.com/mo-cmyk/mazarbul/internal/provider/anthropic"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// Subsystem identifiers used by the checks below. Centralised so the CLI
// render layer and the runner agree on a finite set; rendering groups rows
// by exact string match.
const (
	subsystemStorage  = "storage"
	subsystemMCP      = "mcp"
	subsystemProvider = "provider"
	subsystemLogging  = "logging"
	subsystemMemory   = "memory"
)

// providerNameOllama is the user-facing identifier for the Ollama
// provider. Centralised because the doctor config-driven selection
// compares against it from three different keys.
const providerNameOllama = "ollama"

// providerNameAnthropic mirrors providerNameOllama for the Anthropic
// branch; only one occurrence today but pinned for symmetry.
const providerNameAnthropic = "anthropic"

// embedProviderNone is the explicit-disabled sentinel in defaults.embed
// (see internal/daemon/wiring.go for the daemon-side counterpart). When
// the user picked Skip at init, the doctor emits a WARN naming the
// recovery command rather than running Ollama probes that would
// otherwise FAIL on a machine that has no daemon installed.
const embedProviderNone = "none"

// chatProviderNone is the explicit "no server-side chat model" sentinel in
// defaults.chat, the recommended setup where the calling agent reasons. The
// doctor reports it as OK; only an unset defaults.chat warns.
const chatProviderNone = "none"

// defaultAnthropicChatModel is the fallback model used by the
// chat-provider ping when [providers.anthropic].model is empty. Matches
// the recommended default in the init wizard so doctor and init agree.
const defaultAnthropicChatModel = "claude-opus-4-7"

// DefaultChecks builds the standard check catalog for dataDir. It reads
// the user config to drive provider-check selection: only providers that
// appear in defaults.chat / defaults.embed are probed. A missing config
// emits a single config.load WARN telling the user to run maz init.
//
// DataDir must be resolved by the caller (the cobra --data-dir flag,
// falling back to config.Defaults().DataDir) so doctor reports against
// the same directory every other maz command touches.
func DefaultChecks(dataDir string) []Check {
	shared := &sharedSQLite{path: sqliteIndexPath(dataDir)}
	checks := []Check{
		newDataDirCheck(dataDir),
		newDataDirHygieneCheck(dataDir),
		newSQLiteOpenCheck(shared),
		newSQLiteVecCheck(shared),
		newSQLiteFTSCheck(shared),
		newEventLogDriftCheck(dataDir, shared),
		newLadybugCheck(dataDir),
		newMCPUnixCheck(dataDir),
		newMCPHealthzCheck(dataDir),
		newLoggingCheck(),
		// watch_fs liveness reads only a pidfile / launchd job, so it is
		// config-independent and must register before the config-missing
		// early return below (the watcher runs fine without config.toml).
		newWatchFSCheck(dataDir),
		// Source freshness reads only the connector_watermarks projection via
		// the shared read-only handle, so it is config-independent too: register
		// before the early return so it runs even without config.toml (a dir
		// that has ingested from connectors but never wrote a full config still
		// wants the staleness WARN).
		newSourceFreshnessCheck(shared),
		// self<->principal identity link reads only the relations +
		// principals projections and the on-disk self binding, so it is
		// config-independent and registers before the config-missing early
		// return below. It is the first check that carries a remediation
		// (`maz doctor fix`).
		newSelfPrincipalLinkCheck(dataDir, shared),
		// Advisory data-quality checks read only the entities / relations
		// projections via the shared read-only handle, so they are
		// config-independent and MUST register before the config-missing early
		// return — a freshly-imported store with no config.toml yet is exactly
		// where duplicate entities / active edges are most likely to exist.
		newDuplicateEntitiesCheck(shared),
		newDuplicateActiveEdgesCheck(shared),
		// Cross-device duplicate candidates written by the post-consume dedup
		// sweep. Config-independent: a dir with no config.toml may still have
		// received team-scope events that produced candidate rows.
		newDupCandidatesCheck(shared),
		// Open supersede conflicts: functional slots where two devices
		// independently superseded the same relation and both arms are still open.
		// Config-independent: any synced dir may accumulate conflicts.
		newSupersedeConflictsCheck(shared),
		// Genesis trust-anchor check: verifies the pin in state.toml matches the
		// fold result and the TEAM.json self-signature. Config-independent because
		// it reads only state.toml + SQLite + the sync repo mirror.
		newGenesisPinCheck(dataDir, shared),
	}

	cfg, err := config.LoadUserConfig(dataDir)
	if err != nil {
		checks = append(checks, configMissingCheck(err))
		return checks
	}

	checks = append(checks, providerChecks(dataDir, &cfg)...)
	checks = append(checks,
		newExtractorProviderCheck(&cfg),
		newResolverPredicatesCheck(shared),
		newComposerPromptCheck(),
	)
	checks = append(checks, SyncChecks(dataDir)...)
	// Config-load anti-leak check for connector-sourced content (#211) and
	// admin-facing pending-regrant check — both wired here (not in SyncChecks)
	// because they reuse the shared read-only SQLite handle.
	checks = append(checks,
		newConnectorScopeEncryptionCheck(dataDir, shared),
		newPendingRegrantCheck(shared),
	)
	return checks
}

// newExtractorProviderCheck verifies the configured chat provider slot is
// valid for extraction and that the extract-v1 prompt template registers
// without error. It is config-only: no network call is made. The heavier
// provider ping is handled by the existing provider.chat.ping row.
func newExtractorProviderCheck(cfg *config.UserConfig) CheckFunc {
	return CheckFunc{
		N:   "extractor.provider",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			reg := prompts.New(nil)
			if err := extractor.RegisterExtractPrompt(ctx, reg); err != nil {
				return StatusFail, "", fmt.Errorf("register extract-v1: %w", err)
			}

			chat := cfg.Defaults.Chat
			switch chat {
			case chatProviderNone:
				// Intentional: the recommended setup. The calling agent does
				// the reasoning and writes structure through the MCP tools.
				return StatusOK, "chat=none (intended): your agent does the reasoning; server-side extraction and `maz ask` are off", nil
			case "":
				return StatusWarn, "defaults.chat unset; extractor disabled. Set it to none (your agent reasons), anthropic, or ollama", nil
			}

			table := cfg.Providers[chat]
			model := table.ChatModel
			if model == "" {
				model = table.Model
			}
			if model == "" {
				return StatusWarn, fmt.Sprintf("chat=%s but no chat_model set; run `maz init`", chat), nil
			}

			return StatusOK, fmt.Sprintf("chat=%s model=%s; extract-v1 registered", chat, model), nil
		},
	}
}

// newResolverPredicatesCheck opens the SQLite writer pool via the shared
// handle and counts rows in the predicates table. Migration 0006 seeds 10
// functional predicates; fewer rows indicate an incomplete migration or a
// truncated seed. The query is O(1) via the SQLite stat table.
func newResolverPredicatesCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "resolver.predicates_loaded",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
			}

			var total, functional int
			err = store.Reader().QueryRowContext(ctx,
				`SELECT count(*), sum(case when kind = 'functional' then 1 else 0 end) FROM predicates`,
			).Scan(&total, &functional)
			if err != nil {
				// table missing means migration 0006 has not been applied
				if strings.Contains(err.Error(), "no such table") {
					return StatusFail, "predicates migration 0006 not applied; run `maz rebuild` or re-init", nil
				}
				return StatusFail, "", fmt.Errorf("query predicates: %w", err)
			}

			if total < 10 {
				return StatusWarn, fmt.Sprintf("predicates table present but underseeded (%d rows; expected >= 10)", total), nil
			}

			return StatusOK, fmt.Sprintf("%d predicates loaded (%d functional)", total, functional), nil
		},
	}
}

// newComposerPromptCheck verifies that all eight memory-layer prompt
// templates register against a fresh in-memory registry without error.
// This confirms the template bodies parse cleanly and that no accidental
// version conflict exists between packages at startup.
func newComposerPromptCheck() CheckFunc {
	return CheckFunc{
		N:   "composer.prompt_registered",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			reg := prompts.New(nil)

			if err := extractor.RegisterExtractPrompt(ctx, reg); err != nil {
				return StatusFail, "", fmt.Errorf("register extract-v1: %w", err)
			}
			if err := resolver.RegisterPrompts(ctx, reg); err != nil {
				return StatusFail, "", fmt.Errorf("register resolver prompts: %w", err)
			}
			if err := composer.RegisterComposePrompt(ctx, reg); err != nil {
				return StatusFail, "", fmt.Errorf("register compose-v1: %w", err)
			}
			if err := rewriter.RegisterPrompt(ctx, reg); err != nil {
				return StatusFail, "", fmt.Errorf("register query-rewrite-v1: %w", err)
			}

			return StatusOK, "all 8 memory-layer templates registerable", nil
		},
	}
}

// configMissingCheck emits a single WARN row when no config.toml is on
// disk yet. We do not promote it to FAIL because a freshly-installed
// binary that has not been initialised is the expected first-run state.
func configMissingCheck(loadErr error) CheckFunc {
	detail := "no config.toml; run `maz init`"
	if !errors.Is(loadErr, config.ErrUserConfigNotFound) {
		detail = "load config: " + loadErr.Error()
	}
	return CheckFunc{
		N:   "config.load",
		Sub: subsystemProvider,
		Fn: func(_ context.Context) (Status, string, error) {
			return StatusWarn, detail, nil
		},
	}
}

// providerChecks selects the provider-side checks to register based on
// the user's configured chat and embed slots. The intent is to report
// the actual configured surface rather than a generic checklist: an
// Ollama-only user should never see a phantom "Anthropic key missing"
// row, and an Anthropic-only user should not be told to install BGE-M3.
func providerChecks(dataDir string, cfg *config.UserConfig) []Check {
	var out []Check

	// Use the raw config values so doctor matches the daemon resolver
	// exactly. The daemon does case-sensitive comparisons against
	// "ollama" / "none" (internal/daemon/wiring.go:buildEmbedder); a
	// hand-edited `defaults.embed = "None"` is rejected at daemon
	// startup, so reporting it as a healthy disabled state here would
	// lie. The wizard always writes lowercase, so production configs
	// are unaffected.
	chatProvider := cfg.Defaults.Chat
	embedProvider := cfg.Defaults.Embed
	ollamaTable := cfg.Providers[providerNameOllama]
	anthropicTable := cfg.Providers[providerNameAnthropic]

	if chatProvider == providerNameAnthropic {
		out = append(out,
			newAnthropicSecretCheck(),
			newAnthropicPingCheck(&anthropicTable),
		)
	}

	if embedProvider == embedProviderNone {
		out = append(out, newEmbedDisabledCheck())
	}

	// Mirror the daemon's empty-embed → ollama fallback so doctor's
	// catalog matches what the daemon will actually try to load. A
	// legacy or hand-written config with an omitted embed default
	// makes the daemon contact Ollama; without this fallback doctor
	// would silently skip the Ollama probes that would otherwise
	// explain a startup failure.
	effectiveEmbed := embedProvider
	if effectiveEmbed == "" {
		effectiveEmbed = providerNameOllama
	}

	// Surface unsupported embed values as FAIL. The daemon's
	// buildEmbedder rejects anything outside {"", "ollama", "none"}
	// (case-sensitive); without this row a typo like `defaults.embed
	// = "None"` would slip through both the disabled WARN and the
	// Ollama probes, letting doctor look clean while the daemon
	// refuses to start.
	if effectiveEmbed != providerNameOllama && effectiveEmbed != embedProviderNone {
		out = append(out, newEmbedUnsupportedCheck(embedProvider))
	}

	// Ollama checks run iff a slot actually resolves to it. The
	// pre-#105 heuristic that flagged Ollama via residual per-table
	// fields is gone: `maz init --embed=none` writes no
	// [providers.ollama] section, and a stale field on an
	// Anthropic+none install would otherwise cause spurious FAILs
	// against a daemon the user never asked for.
	wantOllama := chatProvider == providerNameOllama || effectiveEmbed == providerNameOllama
	if wantOllama {
		out = append(out, newOllamaDaemonCheck(&ollamaTable))
		if ollamaTable.EmbedModel != "" {
			out = append(out, newOllamaEmbedModelCheck(&ollamaTable))
		}
		// The embedder.mode summary row only makes sense when Ollama is the
		// embedder (not a chat-only Ollama install). It collapses the daemon +
		// embed-model probes into the single full/degraded answer for recall.
		if effectiveEmbed == providerNameOllama {
			out = append(out, newEmbedderModeCheck(&ollamaTable))
		}
		if chatProvider == providerNameOllama {
			out = append(out, newOllamaPingCheck(&ollamaTable))
		}
		// Supervisor check: only when the wizard started Ollama for the user
		// and set managed=true. The state is inferred from the sidecar PID
		// file because doctor runs out-of-process (separate from the daemon).
		if ollamaTable.Managed {
			dir := dataDir
			// Supervisor + parallelism rows ship together when managed:
			// supervisor surfaces lifecycle state; the parallelism
			// informational row surfaces whether the managed daemon
			// carried OLLAMA_NUM_PARALLEL so the operator can correlate
			// observed throughput with the config knob. Always OK; never blocks.
			out = append(out,
				newOllamaSupervisorCheck(dir, func() daemon.OllamaSupervisorState {
					return inferOllamaSupervisorState(dir)
				}),
				newOllamaParallelismCheck(&ollamaTable),
			)
		}
	}

	return out
}

// newEmbedDisabledCheck emits a single WARN row reminding the user that
// recall is non-functional and naming the command to re-enable it. WARN
// (not FAIL) because the disabled state is a valid configuration: an
// Anthropic-only chat install with no recall is a supported M1 shape.
func newEmbedDisabledCheck() CheckFunc {
	return CheckFunc{
		N:   "embed.disabled",
		Sub: subsystemProvider,
		Fn: func(_ context.Context) (Status, string, error) {
			return StatusWarn, "embeddings disabled (defaults.embed=none); recall returns E_VALIDATION until `maz init --embed=ollama`", nil
		},
	}
}

// newEmbedUnsupportedCheck emits a FAIL row for a defaults.embed value
// that the daemon's case-sensitive resolver will reject at startup
// (anything outside {"", "ollama", "none"}). Reporting it here gives the
// user one clear action — fix the typo — instead of a confusing daemon
// crash log followed by a clean doctor board.
func newEmbedUnsupportedCheck(value string) CheckFunc {
	return CheckFunc{
		N:   "embed.unsupported",
		Sub: subsystemProvider,
		Fn: func(_ context.Context) (Status, string, error) {
			return StatusFail, fmt.Sprintf(
				"defaults.embed=%q not supported (want \"ollama\" or \"none\"); daemon will refuse to start",
				value,
			), nil
		},
	}
}

// anthropicChatModel resolves the Anthropic chat model from the user's
// config, falling back to the recommended default when the table is
// empty (e.g., the user only filled defaults.chat).
func anthropicChatModel(t *config.ProviderTable) string {
	if t.Model != "" {
		return t.Model
	}
	return defaultAnthropicChatModel
}

// dataDirProbeFile is the name written into the data directory by the
// datadir.writable check. Kept short and obviously-Mazarbul-owned so a
// user noticing it in their dir understands what it is.
const dataDirProbeFile = ".maz-doctor"

// newDataDirCheck verifies the data directory exists with the canonical
// 0700 mode and that the current process can write a probe file inside
// it. Failure here means every other storage check will fail with the
// same root cause; surfacing it as its own row gives the user a clear
// hint rather than a wall of identical permission-denied errors.
func newDataDirCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "datadir.writable",
		Sub: subsystemStorage,
		Fn: func(_ context.Context) (Status, string, error) {
			if dataDir == "" {
				return StatusFail, "", errors.New("data dir is empty")
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return StatusFail, "", fmt.Errorf("mkdir %s: %w", dataDir, err)
			}
			probe := filepath.Join(dataDir, dataDirProbeFile)
			if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
				return StatusFail, "", fmt.Errorf("write probe %s: %w", probe, err)
			}
			defer removeProbe(probe)
			// probe is a path the check just created under a caller-supplied
			// data directory; the gosec G304 warning about variable inclusion
			// does not apply because we are not opening attacker-controlled paths.
			if _, err := os.ReadFile(probe); err != nil { //nolint:gosec // probe path is owned by this check
				return StatusFail, "", fmt.Errorf("read probe %s: %w", probe, err)
			}
			info, err := os.Stat(dataDir)
			if err != nil {
				return StatusFail, "", fmt.Errorf("stat %s: %w", dataDir, err)
			}
			mode := info.Mode().Perm()
			if mode != 0o700 {
				return StatusWarn, fmt.Sprintf("%s (mode %#o, want 0700)", dataDir, mode), nil
			}
			return StatusOK, dataDir, nil
		},
	}
}

// newDataDirHygieneCheck reports whether the data dir is marked
// against the macOS Tahoe (26.x) corespotlightd + Time Machine
// freeze regressions. The daemon applies both markers at OpenWiring,
// so a WARN here means an upgrade from an older Mazarbul left the
// dir unmarked, or a TM exclusion was manually cleared. Non-Darwin
// hosts skip the xattr probe and only check the marker file.
func newDataDirHygieneCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "datadir.hygiene",
		Sub: subsystemStorage,
		Fn: func(_ context.Context) (Status, string, error) {
			marker := filepath.Join(dataDir, ".metadata_never_index")
			if _, err := os.Stat(marker); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return StatusWarn, "Spotlight marker missing (macOS Tahoe corespotlightd may freeze the kernel; run any maz command to self-heal)", nil
				}
				return StatusFail, "", fmt.Errorf("stat marker: %w", err)
			}
			if msg, ok := tmExclusionStatus(dataDir); !ok {
				return StatusWarn, msg, nil
			}
			return StatusOK, dataDir, nil
		},
	}
}

// removeProbe deletes the data-dir probe file, swallowing ENOENT but
// reporting any other failure to stderr so a CI environment that strips
// permissions mid-check still surfaces the problem.
func removeProbe(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "doctor: remove probe %s: %v\n", path, err)
	}
}

// sqliteIndexPath returns the SQLite database file path under dataDir.
// Kept private so the three sqlite checks agree on the same location.
func sqliteIndexPath(dataDir string) string {
	return filepath.Join(dataDir, "index.db")
}

// sharedSQLite lazily opens a single sqlite.Store and hands it to the
// three sqlite checks. Doing one open instead of three avoids racing
// the WAL pragma across concurrent goroutines (the runner schedules
// checks in parallel by default). The store is opened on first acquire
// and never closed for the lifetime of the doctor invocation; the OS
// reclaims the file handles when the one-shot CLI exits, matching how
// every other maz subcommand uses SQLite.
type sharedSQLite struct {
	path string
	mu   sync.Mutex
	once sync.Once
	open *sqlite.Store
	err  error
}

// acquire returns the lazily-opened store, or the open error if it
// failed. Safe to call from multiple goroutines: sync.Once guarantees
// the open runs exactly once.
func (s *sharedSQLite) acquire(ctx context.Context) (*sqlite.Store, error) {
	s.once.Do(func() {
		s.open, s.err = sqlite.Open(ctx, s.path)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open, s.err
}

// newSQLiteOpenCheck verifies the relational store opens cleanly and
// the writer pool pings. Subsequent sqlite checks reuse the same
// store via sharedSQLite, so this check is the only one that performs
// the heavyweight open+migrate work.
func newSQLiteOpenCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "sqlite.open",
		Sub: subsystemStorage,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", err
			}
			if err := store.Writer().PingContext(ctx); err != nil {
				return StatusFail, "", fmt.Errorf("ping writer: %w", err)
			}
			return StatusOK, s.path, nil
		},
	}
}

// newSQLiteVecCheck loads the sqlite-vec extension by re-running the
// vec_version() probe the driver's ConnectHook already exercises on
// every connection. Reporting the version string in Detail gives the
// user something verifiable to paste into a bug report.
func newSQLiteVecCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "sqlite.vec",
		Sub: subsystemStorage,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
			}
			var version string
			if err := store.Reader().QueryRowContext(ctx, "SELECT vec_version()").Scan(&version); err != nil {
				return StatusFail, "", fmt.Errorf("vec_version: %w", err)
			}
			// vec_version() returns "v0.1.6"; some builds return "0.1.6".
			// Normalise so the rendered detail always begins with a single "v".
			version = strings.TrimPrefix(version, "v")
			return StatusOK, "v" + version, nil
		},
	}
}

// newSQLiteFTSCheck verifies the episodes_fts virtual table is present.
// FTS5 is loaded as a SQLite contrib extension; if the build is missing
// it, the migration runner would have failed at sqlite.Open, so this
// check is mostly a smoke test that the migrations actually ran.
func newSQLiteFTSCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "sqlite.fts",
		Sub: subsystemStorage,
		Fn: func(ctx context.Context) (Status, string, error) {
			store, err := s.acquire(ctx)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
			}
			var n int
			err = store.Reader().QueryRowContext(
				ctx,
				"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='episodes_fts'",
			).Scan(&n)
			if err != nil {
				return StatusFail, "", fmt.Errorf("probe episodes_fts: %w", err)
			}
			if n == 0 {
				return StatusFail, "episodes_fts virtual table missing (migration 0002 not applied)", nil
			}
			return StatusOK, "episodes_fts present", nil
		},
	}
}

// ladybugPath returns the LadybugDB database root under dataDir. The
// dispatcher uses the same convention; we redeclare it here rather than
// importing dispatcher to keep doctor independent of the kernel writer.
func ladybugPath(dataDir string) string {
	return filepath.Join(dataDir, "graph.ladybug")
}

// ladybugProbeFile is the name of the temporary probe file written by
// newLadybugCheck to confirm the data directory is writable before the
// LadybugDB C layer is invoked. The LadybugDB C library calls abort()
// when it cannot open its database, producing a SIGABRT that kills the
// process — a panic recover() cannot intercept a C-level abort. The
// probe file write is the safe pre-flight that short-circuits before
// the C layer is reached on a read-only directory.
const ladybugProbeFile = ".maz-doctor-lbug"

// newLadybugCheck opens the graph store, runs `RETURN 1`, and closes.
// LadybugDB is single-process; if the daemon is running, the second
// open will fail and we surface WARN (not FAIL) so the user can tell
// a real corruption from a friendly lock.
//
// The lbug C library reports lock contention via the generic "failed
// to open database with status 1" — no English keyword in the
// message. We treat ANY open error as lock contention when a live
// daemon owns the dataDir; this is the friendly-lock case. With the
// daemon stopped, the same error is propagated as a real FAIL.
//
// Read-only data directory: the C library calls abort() on a
// permission-denied open, which sends SIGABRT and kills the process.
// To prevent this, the check probes writability via a temp file before
// invoking OpenLadybug. A non-writable dir returns StatusFail with an
// actionable message instead of crashing.
func newLadybugCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "ladybug.open",
		Sub: subsystemStorage,
		Fn: func(ctx context.Context) (Status, string, error) {
			// Pre-flight: confirm the data dir is writable before calling
			// OpenLadybug. The LadybugDB C library abort()s on a
			// permission-denied open — a SIGABRT that Go's recover() cannot
			// intercept. Fail early with an actionable message instead.
			probe := filepath.Join(dataDir, ladybugProbeFile)
			if werr := os.WriteFile(probe, []byte("ok"), 0o600); werr != nil {
				if errors.Is(werr, os.ErrPermission) {
					return StatusFail,
						"data dir is not writable (permission denied); ladybug.open skipped to avoid C-layer abort — fix dir permissions first",
						nil
				}
				return StatusFail, "", fmt.Errorf("probe data dir writable: %w", werr)
			}
			removeProbe(probe)

			conn, err := ladybug.OpenLadybug(ctx, ladybugPath(dataDir))
			if err != nil {
				if mazDaemonAliveForDataDir(dataDir) {
					return StatusWarn, "graph.ladybug locked (daemon holds the handle; this is expected)", nil
				}
				lower := strings.ToLower(err.Error())
				if strings.Contains(lower, "lock") || strings.Contains(lower, "in use") {
					return StatusWarn, "graph.ladybug locked by another process", nil
				}
				return StatusFail, "", err
			}
			defer closeLadybugConn(conn)
			res, err := conn.Execute(ctx, "RETURN 1", nil)
			if err != nil {
				return StatusFail, "", fmt.Errorf("execute RETURN 1: %w", err)
			}
			if cerr := res.Close(); cerr != nil {
				return StatusFail, "", fmt.Errorf("close result: %w", cerr)
			}
			return StatusOK, ladybugPath(dataDir), nil
		},
	}
}

// mazDaemonAliveForDataDir reports whether a maz daemon is currently running
// for the given dataDir with a valid PID file + token. Used by
// file-handle-contended checks (e.g. ladybug.open) to distinguish "the daemon
// owns the lock" from real corruption: with the daemon running, a second open
// IS expected to fail, and the failure must surface as WARN rather than FAIL
// so a healthy system does not look broken to the operator.
//
// The PID file is resolved per-data-dir via daemon.DataDirPIDPath so an
// isolated daemon instance (non-default --data-dir) is correctly detected
// rather than falling through to the global-temp-dir path.
func mazDaemonAliveForDataDir(dataDir string) bool {
	info, err := daemon.ReadPID(daemon.DataDirPIDPath(dataDir))
	if err != nil {
		return false
	}
	return info.Running && daemon.IsMazDaemon(info.PID, info.Token)
}

// closeLadybugConn is the shared deferred-close helper for the LadybugDB
// check. Errors are logged but not promoted to check failures: the open
// already succeeded.
func closeLadybugConn(conn *ladybug.Conn) {
	if err := conn.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "doctor: close ladybug: %v\n", err)
	}
}

// newMCPUnixCheck stat-and-dials the Mazarbul Unix socket. ENOENT (no
// socket file) is WARN because the daemon may simply be off; ECONNREFUSED
// on a present socket is also WARN. EACCES surfaces as FAIL because
// that is an actionable misconfiguration (wrong owner, sticky bits).
func newMCPUnixCheck(dataDir string) CheckFunc {
	socketPath := mcp.DefaultUnixSocket(dataDir)
	return CheckFunc{
		N:   "mcp.unix",
		Sub: subsystemMCP,
		Fn: func(ctx context.Context) (Status, string, error) {
			info, err := os.Stat(socketPath)
			if errors.Is(err, os.ErrNotExist) {
				return StatusWarn, "socket not present (daemon not running?)", nil
			}
			if err != nil {
				return StatusFail, "", fmt.Errorf("stat %s: %w", socketPath, err)
			}
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, "unix", socketPath)
			if err != nil {
				if errors.Is(err, os.ErrPermission) {
					return StatusFail, "", fmt.Errorf("dial %s: %w", socketPath, err)
				}
				return StatusWarn, fmt.Sprintf("dial %s: %v", socketPath, err), nil
			}
			if cerr := conn.Close(); cerr != nil {
				fmt.Fprintf(os.Stderr, "doctor: close unix conn: %v\n", cerr)
			}
			mode := info.Mode().Perm()
			if mode != 0o600 {
				return StatusWarn, fmt.Sprintf("%s reachable but mode %#o (want 0600)", socketPath, mode), nil
			}
			return StatusOK, socketPath, nil
		},
	}
}

// newMCPHealthzCheck GETs /healthz over the daemon's Unix socket — the
// default transport — and validates the canonical {"status":"ok"}
// response. The MCP server registers the handler at mcp.HealthPath on both
// transports; the constant ensures doctor and the server agree on the URL.
// /healthz is token-exempt, so the probe needs no credential.
func newMCPHealthzCheck(dataDir string) CheckFunc {
	socket := mcp.DefaultUnixSocket(dataDir)
	return CheckFunc{
		N:   "mcp.healthz",
		Sub: subsystemMCP,
		Fn: func(ctx context.Context) (Status, string, error) {
			url := "http://unix" + mcp.HealthPath
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return StatusFail, "", fmt.Errorf("build request: %w", err)
			}
			client := mcp.UnixHTTPClient(socket, 2*time.Second)
			resp, err := client.Do(req)
			if err != nil {
				return StatusWarn, fmt.Sprintf("GET %s (socket %s): %v", url, socket, err), nil
			}
			defer closeHTTPBody(resp)
			if resp.StatusCode != http.StatusOK {
				return StatusFail, fmt.Sprintf("GET %s: HTTP %d", url, resp.StatusCode), nil
			}
			var body mcp.HealthResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				return StatusFail, fmt.Sprintf("decode response: %v", err), nil
			}
			if body.Status != "ok" {
				return StatusFail, fmt.Sprintf("status=%q want ok", body.Status), nil
			}
			return StatusOK, url, nil
		},
	}
}

// closeHTTPBody is the shared deferred-close helper for HTTP-bodied
// checks. Identical pattern to closeSQLiteStore.
func closeHTTPBody(resp *http.Response) {
	if err := resp.Body.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "doctor: close response body: %v\n", err)
	}
}

// newAnthropicSecretCheck verifies the Anthropic key is reachable via
// the env-then-keychain chain. We never log the value; the Detail line
// reports the key length so the user can confirm the right secret made
// it into the keychain (and so a 0-byte entry surfaces as a precise
// failure rather than as a confusing 401 later).
func newAnthropicSecretCheck() CheckFunc {
	return CheckFunc{
		N:   "provider.anthropic.secret",
		Sub: subsystemProvider,
		Fn: func(ctx context.Context) (Status, string, error) {
			chain := secret.NewChain(secret.NewEnvProvider(), secret.NewKeychainProvider())
			v, err := chain.Get(ctx, secret.NameAnthropicAPIKey)
			if err != nil {
				if errors.Is(err, secret.ErrNotFound) {
					return StatusFail, "no Anthropic key in env or keychain; run `maz secret set anthropic.api_key`", nil
				}
				return StatusFail, "", fmt.Errorf("read secret: %w", err)
			}
			if v == "" {
				return StatusFail, "Anthropic key is empty", nil
			}
			return StatusOK, fmt.Sprintf("%d-char key resolved", len(v)), nil
		},
	}
}

// newAnthropicPingCheck issues a 1-token completion against the
// configured Anthropic model. Costs ~$0.00001/run; the command's help
// text mentions this so users are not surprised. The call surfaces auth,
// network reachability, and model availability in a single signal.
//
// A 401 Unauthorized response appends a `maz secret set anthropic.api_key`
// hint to the FAIL detail so the operator knows exactly how to remedy a
// missing or rotated key without hunting through docs.
func newAnthropicPingCheck(t *config.ProviderTable) CheckFunc {
	model := anthropicChatModel(t)
	baseURL := t.BaseURL // empty → anthropic.New uses the default Anthropic endpoint
	return CheckFunc{
		N:   "provider.chat.ping",
		Sub: subsystemProvider,
		Fn: func(ctx context.Context) (Status, string, error) {
			client, err := anthropic.New(&anthropic.Options{
				BaseURL:        baseURL,
				SecretProvider: secret.NewChain(secret.NewEnvProvider(), secret.NewKeychainProvider()),
				MaxRetries:     1,
				Timeout:        4 * time.Second,
			})
			if err != nil {
				return StatusFail, "", fmt.Errorf("anthropic client: %w", err)
			}
			req := &anthropic.CompleteRequest{
				Model:     model,
				MaxTokens: 1,
				Messages: []anthropic.Message{
					{Role: anthropic.RoleUser, Content: []anthropic.ContentBlock{anthropic.TextBlock("ping")}},
				},
			}
			if _, err := client.Complete(ctx, req); err != nil {
				// 401 Unauthorized means the API key is missing or invalid.
				// Surface the exact recovery command so the operator is not
				// left guessing: run `maz secret set anthropic.api_key`.
				var apiErr *anthropic.APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
					return StatusFail,
						"ping " + model + ": 401 Unauthorized — run `maz secret set anthropic.api_key` to update the key",
						nil
				}
				return StatusFail, "", fmt.Errorf("ping %s: %w", model, err)
			}
			return StatusOK, "model " + model, nil
		},
	}
}

// newOllamaPingCheck issues a 1-token chat completion against the
// configured Ollama chat model. Used only when defaults.chat=ollama;
// the Anthropic-flavoured ping fills the same role for Anthropic chat.
// Both checks use the same name (provider.chat.ping) because exactly
// one is ever registered.
func newOllamaPingCheck(t *config.ProviderTable) CheckFunc {
	baseURL := t.BaseURL
	if baseURL == "" {
		baseURL = ollama.DefaultBaseURL
	}
	chatModel := t.ChatModel
	return CheckFunc{
		N:   "provider.chat.ping",
		Sub: subsystemProvider,
		Fn: func(ctx context.Context) (Status, string, error) {
			if chatModel == "" {
				return StatusFail, "", errors.New(
					"providers.ollama.chat_model is empty; set it in config.toml or rerun maz init")
			}
			client, err := ollama.New(ctx, ollama.Options{
				BaseURL:   baseURL,
				ChatModel: chatModel,
			})
			if err != nil {
				// Availability problems no longer break the daemon: under #236
				// the chat provider degrades to extraction-disabled (recall +
				// remember still work), matching the embedder's degraded path,
				// so they are WARN, not FAIL (see classifyChatProbeErr). Only a
				// malformed config (ErrInvalidConfig) stays FAIL because the
				// daemon genuinely refuses to boot on it.
				if errors.Is(err, ollama.ErrInvalidConfig) {
					return StatusFail, "", fmt.Errorf("ollama client: %w", err)
				}
				if errors.Is(err, ollama.ErrModelMissing) {
					return StatusWarn, fmt.Sprintf(
						"chat model %q not installed; extraction + ask paused (recall + remember still work). Run: ollama pull %s",
						chatModel, chatModel,
					), nil
				}
				return StatusWarn, fmt.Sprintf(
					"chat backend unreachable: %v; extraction + ask paused (recall + remember still work) until Ollama is back",
					err,
				), nil
			}
			req := &ollama.CompleteRequest{
				Model:     chatModel,
				MaxTokens: 1,
				Messages: []ollama.Message{
					{Role: ollama.RoleUser, Content: []ollama.ContentBlock{ollama.TextBlock("ping")}},
				},
			}
			if _, err := client.Complete(ctx, req); err != nil {
				// Cold-start of a multi-GB local model regularly exceeds
				// the doctor's 5s per-check budget on its first call —
				// Ollama has to mmap weights, allocate Metal buffers,
				// and warm the inference graph. A deadline-exceeded
				// here is a warming-up signal, NOT a model failure, so
				// surface as WARN with the next-call hint rather than
				// FAIL.
				if errors.Is(err, context.DeadlineExceeded) {
					return StatusWarn, chatModel + " warming up (cold-start exceeded doctor timeout; rerun in ~30s)", nil
				}
				return StatusFail, "", fmt.Errorf("ping %s: %w", chatModel, err)
			}
			return StatusOK, "model " + chatModel, nil
		},
	}
}

// newOllamaDaemonCheck constructs an Ollama client, which triggers the
// /api/tags probe baked into ollama.New, and reports the count of
// installed models. The model-missing error is treated by the next
// check (ollama.embed-model) so this row is purely "is the daemon up?".
func newOllamaDaemonCheck(t *config.ProviderTable) CheckFunc {
	baseURL := t.BaseURL
	if baseURL == "" {
		baseURL = ollama.DefaultBaseURL
	}
	return CheckFunc{
		N:   "provider.ollama.daemon",
		Sub: subsystemProvider,
		Fn: func(ctx context.Context) (Status, string, error) {
			c, err := ollama.New(ctx, ollama.Options{BaseURL: baseURL})
			if err != nil {
				if errors.Is(err, ollama.ErrModelMissing) {
					return StatusOK, "daemon reachable at " + baseURL + " (default embed model not installed; see next row)", nil
				}
				return StatusWarn, fmt.Sprintf(
					"daemon at %s unreachable: %v; recall runs degraded (FTS-only BM25) until it is back",
					baseURL, err,
				), nil
			}
			models, err := c.ListModels(ctx)
			if err != nil {
				return StatusWarn, fmt.Sprintf("list models: %v", err), nil
			}
			return StatusOK, fmt.Sprintf("%s; %d models installed", baseURL, len(models)), nil
		},
	}
}

// newOllamaParallelismCheck reports whether the managed `ollama serve`
// child was spawned with OLLAMA_NUM_PARALLEL set. Always StatusOK; the
// row is purely informational and never blocks (the user opted in
// deliberately and knows their GPU memory budget better than we do).
// Zero (the documented default) renders as "unset"; non-zero renders
// the exact value so the user can correlate config.toml with observed
// concurrent-call throughput.
func newOllamaParallelismCheck(t *config.ProviderTable) CheckFunc {
	return CheckFunc{
		N:   "provider.ollama.parallelism",
		Sub: subsystemProvider,
		Fn: func(_ context.Context) (Status, string, error) {
			if t.NumParallel <= 0 {
				return StatusOK, "num_parallel unset; Ollama default (1 parallel slot)", nil
			}
			return StatusOK, fmt.Sprintf("num_parallel=%d (OLLAMA_NUM_PARALLEL passed to managed child)", t.NumParallel), nil
		},
	}
}

// newOllamaEmbedModelCheck verifies the configured embed model is
// installed. The check delegates to ollama.New (which calls
// modelInstalled internally) and parses the wrapped ErrModelMissing so
// the user sees the exact `ollama pull <model>` command they need.
//
// A missing model is WARN, not FAIL: under #236 recall degrades to FTS-only
// (BM25) instead of hard-failing, so the daemon still serves working — if
// lexical-only — recall. The row names the degraded state and the command to
// restore full hybrid. FAIL is reserved for states that genuinely break the
// daemon, which this no longer does.
func newOllamaEmbedModelCheck(t *config.ProviderTable) CheckFunc {
	baseURL := t.BaseURL
	if baseURL == "" {
		baseURL = ollama.DefaultBaseURL
	}
	embedModel := t.EmbedModel
	return CheckFunc{
		N:   "provider.ollama.embed-model",
		Sub: subsystemProvider,
		Fn: func(ctx context.Context) (Status, string, error) {
			_, err := ollama.New(ctx, ollama.Options{
				BaseURL:    baseURL,
				EmbedModel: embedModel,
			})
			if err != nil {
				// A malformed BaseURL is fatal in the daemon (ErrInvalidConfig),
				// so it must FAIL here too rather than masquerade as a graceful
				// degradation. Mirrors newEmbedderModeCheck.
				if errors.Is(err, ollama.ErrInvalidConfig) {
					return StatusFail, "", fmt.Errorf("malformed embedder config; daemon will refuse to start: %w", err)
				}
				if errors.Is(err, ollama.ErrModelMissing) {
					return StatusWarn, fmt.Sprintf(
						"model %q not installed; recall runs degraded (FTS-only BM25). Run: ollama pull %s to restore full hybrid",
						embedModel, embedModel,
					), nil
				}
				return StatusWarn, fmt.Sprintf("probe embed model: %v", err), nil
			}
			return StatusOK, embedModel + " installed", nil
		},
	}
}

// newEmbedderModeCheck is the single authoritative "what mode is recall in?"
// summary row (#236). The per-provider rows above (daemon reachable,
// embed-model installed) carry the diagnostic detail; this row gives an
// operator the one-line answer they actually want:
//
//   - full hybrid (OK)   — embedder reachable + model installed: vector + FTS + graph.
//   - degraded FTS-only (WARN) — embedder probe fails: recall still serves lexical
//     (BM25) hits, just without the semantic lane.
//
// The explicit opt-out mode (defaults.embed=none) is reported by
// newEmbedDisabledCheck, so this row is only registered when an Ollama embedder
// is actually configured. It never FAILs: every mode it covers still serves
// some recall, so a degraded embedder is a caveat, not a broken daemon.
func newEmbedderModeCheck(t *config.ProviderTable) CheckFunc {
	baseURL := t.BaseURL
	if baseURL == "" {
		baseURL = ollama.DefaultBaseURL
	}
	embedModel := t.EmbedModel
	return CheckFunc{
		N:   "embedder.mode",
		Sub: subsystemProvider,
		Fn: func(ctx context.Context) (Status, string, error) {
			// Mirror the daemon's resolveEmbedder probe: full hybrid requires
			// the daemon reachable AND the embed model installed. Any probe
			// failure (unreachable or missing model) degrades recall to
			// FTS-only rather than breaking it (#236).
			_, err := ollama.New(ctx, ollama.Options{BaseURL: baseURL, EmbedModel: embedModel})
			if err != nil {
				// A malformed BaseURL is fatal in the daemon (classifyEmbedderErr
				// treats ErrInvalidConfig as a boot-refusing config error), so
				// reporting "degraded FTS-only" here would mislead an operator
				// into thinking the daemon is up. Mirror the daemon: FAIL.
				if errors.Is(err, ollama.ErrInvalidConfig) {
					return StatusFail, "", fmt.Errorf("malformed embedder config; daemon will refuse to start: %w", err)
				}
				// Any other probe failure (unreachable / model not pulled) is a
				// successful mode determination — recall degrades to FTS-only,
				// not a check error. Detail lives in the provider.ollama.* rows.
				return StatusWarn, "degraded: FTS-only (BM25) — embedder unavailable (see provider.ollama.* rows). Full hybrid resumes automatically when it is back", nil
			}
			return StatusOK, "full hybrid (vector + FTS + graph)", nil
		},
	}
}

// newLoggingCheck ensures the daemon log file path is writable. The
// resolution rules live in config.DaemonLogFile (XDG_CACHE_HOME or
// ~/.cache/mazarbul, overridable via MAZ_LOG_FILE). The check opens with
// O_APPEND|O_CREATE so an existing log is not truncated.
func newLoggingCheck() CheckFunc {
	return CheckFunc{
		N:   "logging.writable",
		Sub: subsystemLogging,
		Fn: func(_ context.Context) (Status, string, error) {
			path := config.DaemonLogFile()
			if path == "" {
				return StatusFail, "no daemon log path resolved", nil
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return StatusFail, "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
			}
			// path comes from config.DaemonLogFile (an XDG-derived value or
			// MAZ_LOG_FILE the user owns); the gosec G304 warning about
			// variable inclusion does not apply.
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path resolved from owned config
			if err != nil {
				return StatusFail, "", fmt.Errorf("open %s: %w", path, err)
			}
			if cerr := f.Close(); cerr != nil {
				return StatusFail, "", fmt.Errorf("close %s: %w", path, cerr)
			}
			return StatusOK, path, nil
		},
	}
}

// newOllamaSupervisorCheck reports the health of the Ollama lifecycle
// supervisor. It is registered only when providers.ollama.managed == true.
//
// StateFunc is injectable so tests can stub each of the four
// OllamaSupervisorState values without spawning real subprocesses. The
// production value reads the sidecar PID file and probes liveness to
// infer the running supervisor's state from outside the daemon process.
func newOllamaSupervisorCheck(dataDir string, stateFunc func() daemon.OllamaSupervisorState) CheckFunc {
	return CheckFunc{
		N:   "provider.ollama.supervisor",
		Sub: subsystemProvider,
		Fn: func(_ context.Context) (Status, string, error) {
			state := stateFunc()
			switch state {
			case daemon.StateHealthy:
				return StatusOK, "ollama supervisor: healthy", nil
			case daemon.StateRestartPending:
				return StatusWarn, "ollama supervisor: restart pending (child crashed; supervisor is restarting)", nil
			case daemon.StateFailing:
				return StatusFail, "ollama supervisor: failing (too many crashes; run `maz doctor` for details or restart ollama manually)", nil
			default: // StateNotSupervised or unknown
				return StatusWarn, fmt.Sprintf("ollama supervisor: not supervised (sidecar PID file absent at %s)", dataDir), nil
			}
		},
	}
}

// inferOllamaSupervisorState reads the sidecar PID file under dataDir
// and probes liveness to produce an OllamaSupervisorState suitable for
// display by newOllamaSupervisorCheck when running out-of-process (i.e.
// maz doctor, which cannot call the in-process supervisor's State()
// method directly). The inference is conservative:
//
//   - No PID file or unreadable -> StateNotSupervised
//   - failing marker present -> StateFailing (supervisor gave up restarting)
//   - PID alive -> StateHealthy
//   - PID dead (process gone) -> StateRestartPending (supervisor will restart it)
func inferOllamaSupervisorState(dataDir string) daemon.OllamaSupervisorState {
	// Check the failing marker before the PID file: when the supervisor
	// enters StateFailing it writes the marker and stops restarting, so the
	// PID file is stale/dead and would otherwise be misreported as
	// StateRestartPending.
	if daemon.FailingMarkerExists(dataDir) {
		return daemon.StateFailing
	}
	pidPath := filepath.Join(dataDir, "ollama-serve.pid")
	info, err := daemon.ReadPID(pidPath)
	if err != nil {
		return daemon.StateNotSupervised
	}
	if info.Running {
		return daemon.StateHealthy
	}
	return daemon.StateRestartPending
}

// boltIndexPath returns the absolute path to the bbolt sidecar. Mirrors the
// path logic in FileEventLog.indexPath (internal/kernel/file_eventlog_index.go)
// so doctor and the kernel agree on the same file without importing the kernel
// writer package into the doctor layer.
func boltIndexPath(dataDir string) string {
	return filepath.Join(dataDir, "events", "index.bolt")
}

// openBoltReadOnly opens the bbolt sidecar at path in read-only mode with a
// 500 ms timeout. Read-only mode does not acquire the bbolt write lock, so an
// already-running daemon is not blocked. Returns os.ErrNotExist when path does
// not exist, and bbolterrors.ErrTimeout when the lock is contended for longer
// than 500 ms (indicating the daemon holds a write lock).
func openBoltReadOnly(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{
		ReadOnly: true,
		Timeout:  500 * time.Millisecond,
	})
	if err != nil {
		return nil, fmt.Errorf("bolt open %s: %w", path, err)
	}
	return db, nil
}

// readBoltLastEventUID returns the hex-encoded last raw-event UID from
// the bbolt sidecar. Used as a diagnostic field in error messages from
// the drift check; the raw-event count it used to return is no longer
// authoritative since the kind-aware scan replaced it.
//
// Returns "" when the bucket is absent (sidecar present but unpopulated).
func readBoltLastEventUID(db *bolt.DB) (string, error) {
	var lastKeyHex string
	if verr := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		if k, _ := c.Last(); k != nil {
			lastKeyHex = hex.EncodeToString(k)
		}
		return nil
	}); verr != nil {
		return "", fmt.Errorf("bolt view: %w", verr)
	}
	return lastKeyHex, nil
}

// driftDetail formats the Result.Detail string for the drift check based on
// the counts from both sides. Extracted from newEventLogDriftCheck to reduce
// cognitive complexity.
//
// With the kind-aware scan, eventCount is the episode-kind count from JSONL
// (not the raw bolt count). The last-UID equality check that used to gate the
// "in sync" branch was dropped: the last raw-bolt UID can be any kind in a
// multi-kind log, so equality with the last episode UID is no longer a valid
// invariant.
func driftDetail(eventCount, episodeCount int) (Status, string) {
	if eventCount == 0 && episodeCount == 0 {
		return StatusOK, "0 events, 0 episodes, in sync"
	}
	delta := eventCount - episodeCount
	switch {
	case delta == 0:
		return StatusOK,
			fmt.Sprintf("%d episode events, %d episodes, in sync", eventCount, episodeCount)
	case delta >= 1 && delta <= 10:
		return StatusWarn,
			fmt.Sprintf(
				"%d events, %d episodes (delta %d); run `maz rebuild`",
				eventCount, episodeCount, delta,
			)
	case delta > 10:
		return StatusFail,
			fmt.Sprintf(
				"%d events, %d episodes (delta %d); derived store is stale -- run `maz rebuild`",
				eventCount, episodeCount, delta,
			)
	default: // delta < 0: episodes > events
		return StatusFail,
			fmt.Sprintf(
				"%d episodes exceeds %d log events (delta %d); index may be corrupt",
				episodeCount, eventCount, delta,
			)
	}
}

// newEventLogDriftCheck compares the episode-kind event count from the
// JSONL log against the SQLite episodes table. The bbolt sidecar still
// supplies the last raw-event UID for diagnostic output, but the
// equality invariant runs on episode-kind events only — heterogeneous
// kinds (entity, mention, relation, prompt.rendered, ...) Live in the
// same log and would otherwise produce a false-positive "derived store
// is stale" against operators who have run any non-episode flow. A
// delta of zero is the healthy state; any deviation surfaces as WARN
// or FAIL with a `maz rebuild` hint.
//
// Algorithm: one bbolt Stats() call for the raw count + cursor-last,
// plus a streaming JSONL scan filtered by `kind == "episode"`, plus
// two SQL scalar queries for the episode count and last UID. The JSONL
// scan is O(events) but the file is tiny in practice (M1 corpora hold
// hundreds, not millions); a JSONL truncation cap protects against
// pathological logs in the unlikely future where this stops being true.
func newEventLogDriftCheck(dataDir string, s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "storage.drift",
		Sub: subsystemStorage,
		Fn: func(ctx context.Context) (Status, string, error) {
			episodeCount, _, err := queryEpisodeStats(ctx, s)
			if err != nil {
				return StatusFail, "", err
			}

			boltPath := boltIndexPath(dataDir)
			db, berr := openBoltReadOnly(boltPath)
			if berr != nil {
				return driftBoltOpenError(berr, episodeCount)
			}
			defer closeBoltDB(db)

			if _, rerr := readBoltLastEventUID(db); rerr != nil {
				return StatusFail, "", fmt.Errorf("read bolt sidecar: %w", rerr)
			}

			episodeEventCount, scanErr := countEpisodeEvents(ctx, dataDir)
			if scanErr != nil {
				return StatusFail, "", fmt.Errorf("scan event log for episode kind: %w", scanErr)
			}

			st, detail := driftDetail(episodeEventCount, episodeCount)
			return st, detail, nil
		},
	}
}

// countEpisodeEvents walks <dataDir>/events/**/*.jsonl and returns the
// number of lines whose TOP-LEVEL `kind` field equals "episode". Decoding
// (rather than substring matching) is the load-bearing invariant: a
// non-episode event whose payload embeds a nested object with kind:
// "episode" — for example a prompt.rendered record whose payload quotes a
// sample episode schema — would otherwise inflate the count and trigger
// a false "storage stale" alarm.
//
// Returns 0 when the events directory is absent (no events written yet).
func countEpisodeEvents(ctx context.Context, dataDir string) (int, error) {
	root := filepath.Join(dataDir, "events")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("stat events dir: %w", err)
	}

	var total int
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("ctx: %w", cerr)
		}
		n, ferr := countEpisodeEventsInFile(ctx, path)
		if ferr != nil {
			return ferr
		}
		total += n
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("walk events dir: %w", walkErr)
	}
	return total, nil
}

// countEpisodeEventsInFile scans one JSONL file line-by-line and counts
// the lines whose top-level `kind` field equals ontology.KindEpisode. A
// malformed line (truncated write mid-flush, partial fsync after a crash)
// is skipped silently so a single bad record cannot wedge the check.
func countEpisodeEventsInFile(ctx context.Context, path string) (int, error) {
	// #nosec G304 -- path comes from WalkDir over the doctor's dataDir.
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// JSONL lines can be longer than the default 64 KiB when payload
	// is verbose (e.g. long-form episodes). 1 MiB is comfortably above
	// what any realistic episode payload reaches.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var n int
	for scanner.Scan() {
		if cerr := ctx.Err(); cerr != nil {
			return 0, fmt.Errorf("ctx: %w", cerr)
		}
		var hdr struct {
			Kind string `json:"kind"`
		}
		if jerr := json.Unmarshal(scanner.Bytes(), &hdr); jerr != nil {
			continue
		}
		if hdr.Kind == ontology.KindEpisode {
			n++
		}
	}
	if serr := scanner.Err(); serr != nil {
		return 0, fmt.Errorf("scan %s: %w", path, serr)
	}
	return n, nil
}

// queryEpisodeStats reads the episode count and last episode UID from SQLite
// in a single read-only transaction so both values come from the same snapshot.
// Two separate QueryRowContext calls outside a transaction can observe different
// snapshots if the daemon commits between them, producing mixed state.
//
// The last UID is selected with ORDER BY UPPER(uid) DESC (not create_time DESC
// and not the raw uid column). SQLite byte collation treats lowercase ASCII
// characters (0x61-0x7A) as greater than uppercase (0x41-0x5A), so a raw
// ORDER BY uid DESC on a mixed-case store would pick an older lowercase UID
// over a newer uppercase one. UPPER(uid) forces the sort to canonical uppercase
// Crockford base32, which agrees with bbolt's binary-key order (the ULID bytes
// encode the same timestamp ordering). After fetching, the raw stored value is
// still normalized through ulid.Parse so the comparison field is always the
// canonical uppercase string, matching boltUIDHex's output.
//
// Extracted to reduce cognitive complexity in the drift check closure.
func queryEpisodeStats(ctx context.Context, s *sharedSQLite) (count int, lastUID string, err error) {
	store, err := s.acquire(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("open sqlite: %w", err)
	}
	tx, err := store.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			err = fmt.Errorf("rollback: %w", rerr)
		}
	}()
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM episodes",
	).Scan(&count); err != nil {
		return 0, "", fmt.Errorf("count episodes: %w", err)
	}
	var raw string
	if err := tx.QueryRowContext(ctx,
		"SELECT uid FROM episodes ORDER BY UPPER(uid) DESC LIMIT 1",
	).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("last episode uid: %w", err)
	}
	// Normalize the fetched UID to canonical uppercase Crockford base32.
	// This makes the comparison with the bbolt binary key consistent even when
	// caller-supplied UIDs used lowercase base32 characters. ulid.Parse accepts
	// both; ulid.ULID.String() always produces the uppercase canonical form.
	if raw != "" {
		if id, perr := ulid.Parse(raw); perr == nil {
			raw = id.String()
		}
	}
	return count, raw, nil
}

// driftBoltOpenError maps a bolt.Open error to a doctor Status. Extracted to
// reduce cognitive complexity in the drift check closure.
func driftBoltOpenError(err error, episodeCount int) (Status, string, error) {
	if errors.Is(err, os.ErrNotExist) {
		if episodeCount == 0 {
			return StatusOK, "0 events, 0 episodes, in sync", nil
		}
		return StatusWarn, "event-log index absent; run `maz rebuild` to reconcile", nil
	}
	if errors.Is(err, bbolterrors.ErrTimeout) {
		return StatusWarn, "event-log index locked (daemon running?)", nil
	}
	return StatusFail, "", fmt.Errorf("open bolt sidecar: %w", err)
}

// closeBoltDB closes a bolt.DB, logging any error to stderr. Used as a
// deferred close helper so the drift check does not swallow close errors.
func closeBoltDB(db *bolt.DB) {
	if err := db.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "doctor: close bolt sidecar: %v\n", err)
	}
}
