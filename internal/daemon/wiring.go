package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/capacity"
	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	"github.com/mo-cmyk/mazarbul/internal/sync/syncidentity"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
	"github.com/mo-cmyk/mazarbul/internal/team"
)

// MinimalBootEnv names the env var that forces OpenWiring into the
// most-stripped boot mode the daemon supports: only the write-only
// `remember` tool registers; the embedder is never probed; recall is
// disabled. Set this when the daemon refuses to boot normally on macOS
// (host freeze, kernel panic during embedder probe, sqlite-vec extension
// load crash) to isolate which optional opener is the culprit.
//
// Example: `MAZ_MINIMAL_BOOT=1 ./bin/maz daemon start`.
const MinimalBootEnv = "MAZ_MINIMAL_BOOT"

// DeferEmbedderEnv names the env var that defers the Ollama HTTP probe
// out of OpenWiring's synchronous path. The orchestrator + dispatcher
// still open all four storage layers; only the embedder client (and the
// recall tool that depends on it) is skipped. Lighter than
// MAZ_MINIMAL_BOOT: useful when the freeze evidence points specifically
// at the embedder probe but the kernel storage stack is known healthy.
const DeferEmbedderEnv = "MAZ_DEFER_EMBEDDER"

// minimalBoot reports whether the operator set MAZ_MINIMAL_BOOT.
// Wrapped in a helper so tests can flip it without leaking the parsing
// rule across files.
func minimalBoot() bool { return envTruthy(MinimalBootEnv) }

// deferEmbedder reports whether the operator set MAZ_DEFER_EMBEDDER.
// Also implied by MinimalBoot, since the embedder is one of the things
// minimal mode strips.
func deferEmbedder() bool { return envTruthy(DeferEmbedderEnv) || minimalBoot() }

// envTruthy reports whether the named env var is set to a non-empty,
// non-disabled value. "0" and "false" count as off so a user can write
// `MAZ_MINIMAL_BOOT=0` to override an inherited environment.
func envTruthy(name string) bool {
	v := os.Getenv(name)
	if v == "" || v == "0" || v == "false" {
		return false
	}
	return true
}

// embedProviderOllama is the provider key the wizard writes for the M1
// default local embedder. It matches the key UserConfig.Defaults.Embed
// carries when `maz init` runs Ollama through the model-selection step.
const embedProviderOllama = "ollama"

// embedProviderNone is the explicit-disabled sentinel `maz init` writes
// when the user picks Skip at the embed step. BuildEmbedder returns a
// nil embedder for this value; the recall tool registers its disabled
// variant so the wire surface carries a clear error rather than failing
// at first use.
const embedProviderNone = "none"

// errUnsupportedEmbedProvider sentinels a configuration error in the
// embed-provider name. OpenWiring distinguishes this class from a probe
// failure: a misnamed provider is the operator's fault and fails the
// daemon fast; an unreachable Ollama is environmental and downgrades to
// write-only mode.
var errUnsupportedEmbedProvider = errors.New("daemon: unsupported embed provider")

// embedderMode classifies why embedder resolution returned the embedder it did,
// so OpenWiring can register the right recall surface. It splits the two cases
// that used to collapse into a single nil embedder (#236): an explicit config
// opt-out keeps the hard disabled stub, while a transient miss degrades to
// FTS-only recall instead of hard-failing.
type embedderMode int

const (
	// A live embedder is configured; recall runs full hybrid (vector + FTS + graph).
	embedderActive embedderMode = iota
	// The configured embedder is transiently unreachable (e.g. Ollama down) or
	// its probe was deferred; recall degrades to FTS(+graph)-only and advertises
	// degraded:true.
	embedderDegraded
	// Embeddings are disabled by config (defaults.embed=none or ""); #105's
	// explicit opt-out. Recall stays the disabled stub (E_VALIDATION).
	embedderOptOut
)

// WiringConfig parameterises OpenWiring. DataDir is required; Logger is
// optional (a nop logger is substituted). Future fields will plumb a
// secret provider, per-provider overrides, and tracing hooks.
type WiringConfig struct {
	DataDir string
	Logger  *zap.Logger

	// BootTracer marks each phase to disk with fsync before the phase
	// begins, so a kernel panic or host freeze leaves the trace ending
	// on the offending phase. Optional; nil turns every Mark into a
	// no-op.
	BootTracer *BootTracer

	// Secrets, when non-nil, overrides the default env→keychain secret chain.
	// Production leaves it nil (the daemon embeds the OS keychain). Tests and
	// alternative hosts inject an isolated provider so two wirings on one
	// machine do not share a keychain namespace — the seam that lets a
	// two-device team-sync E2E run hermetically in one process with genuinely
	// separate device identities + DEK rings.
	Secrets secret.Provider
}

// Wired holds the kernel + tool plumbing the supervisor and the
// foreground `maz mcp serve` share. Close shuts everything down in
// reverse open order. The struct stays exported (rather than hiding
// behind an interface) because both callers need the typed Tools slice
// to feed mcp.Server.Register.
type Wired struct {
	// Orchestrator owns the FileEventLog + four-layer Dispatcher. The
	// remember tool writes through Orchestrator.Log; the dispatcher
	// fans events into SQLite + vector + FTS + LadybugDB. Owned here
	// so the supervisor's Close path shuts it down LIFO.
	Orchestrator *orchestrator.Orchestrator

	// Embedder is the dense-vector source HybridQuery and the
	// dispatcher both consume. M1 ships *ollama.Client wrapped by the
	// orchestrator's CachingEmbedder. Nil when defaults.embed=none
	// or a transient probe failure downgraded the daemon.
	Embedder kernel.Embedder

	// Chat is the configured chat provider (extraction, resolution,
	// composition, ask). Nil when defaults.chat is none/empty or the
	// provider was unreachable at boot. Exposed so the serving daemon can
	// pre-warm a local chat model off the request path.
	Chat kernel.ChatProvider

	// ChatLocal reports whether Chat is a local Ollama provider (as opposed
	// to Anthropic). Only a local model is worth pre-warming; warming a
	// remote provider would issue a billable API call for no latency win.
	ChatLocal bool

	// ChatModel is the configured chat model id (e.g. "qwen2.5:7b"), used as
	// the model for the pre-warm completion. Empty when Chat is nil.
	ChatModel string

	// ConflictDetector scans functional relation slots for concurrent
	// supersedes and emits relationSupersedeConflict events. Built by
	// buildConflictDetectorOrNil in OpenWiring; consumed by the sync
	// supervisor's post-graft pass and by getEntityContext's conflict
	// annotation. Nil when storage is not the production dispatcher.
	ConflictDetector conflictDetector

	// Query is the hybrid retrieval composer. Recall reads through it;
	// Traverse currently returns ErrNotImplemented (M2 lands LadybugDB).
	// Nil when the embedder is nil (no HybridQuery can be built).
	Query *kernel.HybridQuery

	// Secrets is the resolved secret provider chain (env -> keychain).
	// Callers pass it to mcp.Options.Secrets so every tool-call context
	// carries a usable SecretProvider without a global.
	Secrets mcp.SecretProvider

	// Tools is the slice the caller registers on mcp.Server. M1 carries
	// recall + remember; the remaining four (forget, relate, traverse,
	// review) follow in the M1 closeout.
	Tools []mcp.Tool

	// Stats is the in-process runtime-counter store backing the
	// getUsageSnapshot MCP tool, the `maz status` / `maz watch` usage
	// pane, and (later) cost rollups. Initialised in OpenWiring;
	// nil-tolerant downstream.
	Stats stats.Store

	// Capacity reports per-storage-layer on-disk byte totals. Used by
	// the getCapacity MCP tool and the `maz status` summary.
	Capacity capacity.Reporter

	// Jobs is the in-process background-job registry backing the
	// listJobs / cancelJob MCP tools and the `maz watch` jobs pane.
	Jobs *jobs.Registry

	// Projector is the single vault Projector instance shared between the MCP
	// document tools (writeDocument, editDocument, materializeVault) and the
	// Writeback worker started by startVaultWatcher. Both consumers MUST use this
	// field rather than calling vault.NewProjector independently, so that I3
	// (single serialized writer, one mutex, one sidecar owner) genuinely holds
	// and I2 misclassification of a later revision as a "pending edit" is
	// impossible. Nil when storage is not the production dispatcher or vault is
	// not configured.
	Projector *vault.Projector

	// ProseExtractor is the optional LLM-backed prose extraction hook set by
	// wireMemoryLayer when a ChatProvider is configured. Nil when extraction
	// is disabled (no chat provider, or minimal-boot mode). Consumed by
	// startVaultWatcher to wire prose extraction into the vault Importer.
	ProseExtractor document.ProseExtractor

	// Importer is the single shared document Importer instance. Stored here
	// so wireMemoryLayer can late-bind the ProseExtractor onto it after the
	// chat provider is available (registerNavigationTools builds the importer
	// before the memory layer is wired).
	Importer *document.Importer

	// ResourceAssembler is the brief assembler built by registerContextTools.
	// Stored here so wireAndBuildServer can call RegisterContextResources on
	// the MCP server after construction. Nil when storage is not the
	// production dispatcher or the navigator could not be built.
	ResourceAssembler mcp.ContextAssembler

	// ResourceDocReader is the document navigator surface built by
	// registerContextTools, stored for RegisterContextResources. Nil when
	// ResourceAssembler is nil.
	ResourceDocReader mcp.DocumentReader

	// InstructionComposer recomputes the MCP `initialize` instructions per
	// session (base manual + bootstrap pointer + active-preset section) for
	// Channel A (#214). Built by registerContextTools over the brief assembler;
	// wireAndBuildServer threads it into mcp.Options. Nil when storage is not the
	// production dispatcher (the navigator/assembler could not be built), in which
	// case the MCP server falls back to the static base instructions.
	InstructionComposer func(ctx context.Context) (string, error)

	closers []func() error
}

// Close tears down every resource OpenWiring opened, in reverse order.
// The orchestrator owns the per-layer closers (SQLite, vector, FTS,
// LadybugDB, FileEventLog sidecar); its Close joins any layer-close
// failures internally. Subsequent calls are a no-op.
func (w *Wired) Close(_ context.Context) error {
	if w == nil {
		return nil
	}
	var errs []error
	for i := len(w.closers) - 1; i >= 0; i-- {
		if err := w.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	w.closers = nil
	if joined := errors.Join(errs...); joined != nil {
		return fmt.Errorf("daemon: wiring close: %w", joined)
	}
	return nil
}

// secretProviderAdapter bridges secret.Provider (Get method) to
// mcp.SecretProvider (Secret method) so the daemon's secret chain can
// be threaded onto every MCP tool-call context without importing the
// mcp package from the secret package.
type secretProviderAdapter struct{ p secret.Provider }

func (a secretProviderAdapter) Secret(ctx context.Context, name string) (string, error) {
	v, err := a.p.Get(ctx, name)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			return "", fmt.Errorf("daemon: secret %q: %w", name, mcp.ErrSecretNotFound)
		}
		return "", fmt.Errorf("daemon: secret %q: %w", name, err)
	}
	return v, nil
}

// OpenWiring constructs the kernel-side dependencies the MCP tools need
// and assembles the registered tool slice. The returned *Wired carries
// every resource the caller must register and, eventually, Close.
//
// Open order: user config -> embedder -> ontology registry ->
// orchestrator (which opens SQLite + LadybugDB + vector + FTS and wires
// them under the FileEventLog) -> HybridQuery (reads through the
// dispatcher's existing handles) -> tool slice. A partial-failure path
// closes anything already opened before returning the error so the
// caller never sees a half-constructed *Wired.
//
// Reflects a real degraded-mode option (no embedder, no chat, minimal
// boot); further extraction would obscure the ordering invariant.
//
//nolint:gocyclo,gocognit // boot ordering is intentionally linear: each branch
func OpenWiring(ctx context.Context, cfg WiringConfig) (wired *Wired, err error) {
	if cfg.DataDir == "" {
		return nil, errors.New("daemon: WiringConfig.DataDir required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	applyDataDirHygieneOrWarn(cfg.DataDir, logger)
	// Sweep orphaned online-rebuild shadow dirs and warn on an interrupted swap
	// before the store opens (and on every serving-stack reload).
	cleanStaleRebuildArtifacts(cfg.DataDir, logger)

	// Refuse to start on a legacy log that predates per-writer signing: the
	// daemon never rewrites events under itself (single-writer invariant), so
	// the operator must run `maz migrate sign-events` (daemon stopped) first.
	if needs, nerr := kernel.LogNeedsSigning(cfg.DataDir); nerr != nil {
		return nil, fmt.Errorf("daemon: inspect event log signatures: %w", nerr)
	} else if needs {
		return nil, errors.New("daemon: event log contains unsigned events; stop the daemon and run `maz migrate sign-events` to sign them before starting")
	}

	var secretChain secret.Provider = secret.DefaultChain()
	if cfg.Secrets != nil {
		secretChain = cfg.Secrets
	}
	secrets := secretProviderAdapter{p: secretChain}

	cfg.BootTracer.Mark(PhaseConfigLoad)
	user, err := loadUserConfigOrDefault(cfg.DataDir, logger)
	if err != nil {
		return nil, err
	}

	emb, embMode, err := maybeResolveEmbedder(ctx, cfg, user, logger)
	if err != nil {
		return nil, err
	}

	chat, err := buildChatProvider(ctx, user, secretChain, logger)
	if err != nil {
		return nil, err
	}

	// w.Embedder is set after orchestrator construction so the field
	// holds the cache-wrapped embedder the dispatcher and HybridQuery
	// actually share, not the raw *ollama.Client (which is wrapped by
	// orchestrator.New). Initialising to the raw client here would lie
	// to any future caller that consumes Wired.Embedder.
	w := &Wired{Secrets: secrets}
	// Record the chat provider + its locality so the serving daemon can
	// pre-warm a local chat model on boot. ChatLocal gates the warm to Ollama
	// so an Anthropic provider is never hit with a billable warm-up call.
	w.Chat = chat
	if chat != nil && user.Defaults.Chat == chatProviderOllama {
		w.ChatLocal = true
		w.ChatModel = user.Providers[chatProviderOllama].ChatModel
	}
	defer func() {
		// On any error after this point the partial state is rolled
		// back so the caller never sees a half-wired struct. The
		// defer is intentionally installed AFTER buildEmbedder: that
		// call constructs only an in-memory *ollama.Client (no Close
		// surface, no disk handle, no goroutines), so a probe failure
		// there leaks nothing and the early return path is correct
		// without rollback. Every step BELOW this point opens an OS
		// resource, hence the defer.
		//
		// Any close failure during rollback joins the original error
		// so the operator sees both the original cause and any
		// cleanup trouble it triggered.
		if err == nil {
			return
		}
		if closeErr := w.Close(ctx); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	// Stats must be built BEFORE the orchestrator so the dispatcher
	// receives the sink through its Config and IncDedup calls from
	// the Phase 2 mem0-parity sync dedup branch land in the same
	// in-memory Store that the Usage panel reads. Capacity + Jobs
	// stay below; they are unrelated to the orchestrator construction.
	w.Stats = stats.NewInMemoryStore()

	orch, err := buildOrchestrator(ctx, cfg, emb, &user, w.Stats, secretChain, logger)
	if err != nil {
		return nil, err
	}
	w.Orchestrator = orch
	w.Embedder = orch.Embedder
	w.closers = append(w.closers, orch.Close)

	// Capacity + Jobs back the read-only usage MCP tools and the
	// `maz watch` jobs pane. Both are nil-tolerant downstream; we
	// always initialise so consumers can render without a probe.
	w.Capacity = capacity.NewReporter(cfg.DataDir)
	// NewRegistryWithStore replays the trailing 24 h of job records from
	// $DATA_DIR/jobs.jsonl on boot, so the Jobs view survives daemon
	// restarts. Plain NewRegistry would lose history on every reload.
	w.Jobs = jobs.NewRegistryWithStore(cfg.DataDir)
	// Close drains the persist channel and joins the writeLoop goroutine
	// so an in-flight jobs.jsonl append finishes before the daemon exits.
	// No-op when the registry was constructed without persistence.
	w.closers = append(w.closers, func() error {
		w.Jobs.Close()
		return nil
	})

	// Install the Stats sink on the FileEventLog so Append's IncEvent
	// and IncBytes counters land in w.Stats — the same instance the
	// Usage panel reads. Without this every remember + every internal
	// event-log write would leave the Usage cards stuck at zero.
	orch.Log.SetStats(w.Stats)
	// Install the JobStarter so background extraction batches surface
	// in the Jobs panel alongside the rebuild producer.
	orch.Log.SetJobStarter(w.Jobs.AsStarter())

	// Tools registered unconditionally are write-only (don't need an
	// embedder). remember falls into that bucket; recall depends on
	// HybridQuery, which rejects a nil embedder, so the disabled
	// variant is registered when no embedder is live.
	cfg.BootTracer.Mark(PhaseToolRegister)
	// idleStatus / waitForIdle expose background-work counters so
	// benchmarks (and any agent that wants a "ingest then measure")
	// pattern) can poll progress between a burst of remembers and a
	// recall. Vec reporter is the dispatcher when the orchestrator's
	// storage is one; a nil reporter is treated as "always idle" for
	// that lane, which is the right behavior for tests that wire a
	// fake storage.
	vecReporter := vecReporterFor(orch)
	w.Tools = []mcp.Tool{
		tools.NewRemember(orch.Log),
		tools.NewGetUsageSnapshot(w.Stats),
		tools.NewGetCapacity(w.Capacity),
		tools.NewListJobs(w.Jobs),
		tools.NewCancelJob(w.Jobs),
		tools.NewIdleStatus(orch.Log, vecReporter),
		tools.NewWaitForIdle(orch.Log, vecReporter),
	}

	if minimalBoot() {
		logger.Warn("daemon: minimal boot — registering only `remember`",
			zap.String("env", MinimalBootEnv),
		)
		w.Tools = append(w.Tools, tools.NewRecallDisabled())
		wired = w
		return wired, nil
	}

	// forget is a full write-mode tool; in MinimalBoot we keep the
	// surface to remember + recall-disabled only.
	w.Tools = append(w.Tools, tools.NewForget(orch.Log))

	// LLM-free structured navigation surface (seed-then-traverse). These read
	// straight from the SQLite projection via the Navigator, so they are wired
	// here — before the embedder check — and remain available in degraded
	// (no-embedder) mode: an agent can still walk the graph when recall is
	// disabled. relate is the write-side counterpart (direct relation assert).
	// Skipped only when storage is not the production dispatcher (test fakes).
	vaultPath := config.VaultPath(cfg.DataDir, &user)
	if nerr := registerNavigationTools(ctx, w, orch, vaultPath, cfg.DataDir, secretChain, logger, user.Ontology, user.Sync, config.ResolveFreshnessPolicy(user.Connectors)); nerr != nil {
		return nil, nerr
	}
	// Team-scope membership lifecycle (#213): team_create/grant/revoke. Wired
	// after navigation tools (it needs the same production dispatcher) and given
	// the raw secret chain (Get+Set) so the service can cache per-epoch DEKs in
	// the keychain. Skipped (with a warn) when storage is not the dispatcher.
	registerTeamTools(w, orch, secretChain, cfg.DataDir, logger)

	// Recall surface depends on the embedder mode (#236):
	//   - embedderOptOut:   defaults.embed=none/"" (explicit #105 opt-out) →
	//     disabled stub (E_VALIDATION), discoverable but inert.
	//   - embedderDegraded: transient miss (Ollama unreachable / not-yet-pulled
	//     model) → FTS(+graph)-only HybridQuery that serves BM25 hits and
	//     advertises degraded:true, instead of hard-failing on first recall.
	// Both skip the memory layer (extraction/resolver/ask needs the embedder to
	// store entity vectors) but still fall through to the embedder-independent
	// tail (vault watcher + sync) below — a no-embedder install must still seal
	// and sync every scope.
	if embMode != embedderActive {
		if rerr := registerRecallWithoutLiveEmbedder(ctx, w, orch, embMode, vaultPath, &user, logger); rerr != nil {
			return nil, rerr
		}
		// wireMemoryLayer (which registers definePredicate) is skipped without a
		// live embedder; register the standalone authoring variant so the schema
		// surface stays symmetric with defineEntityKind (parity with the no-chat
		// branch below).
		registerDefinePredicateDegraded(ctx, w, orch, logger)
	} else {
		cfg.BootTracer.Mark(PhaseHybridQuery)
		hybrid, err := buildHybridQuery(ctx, orch)
		if err != nil {
			return nil, err
		}
		applyRecallTuning(hybrid, user.Recall, logger)
		// Install the Stats sink so Recall's IncRecall counter lands in the
		// same w.Stats instance the Usage panel reads.
		hybrid.SetStats(w.Stats)
		// Install the passive-reinforcement recorder (issue #231): recall emits
		// debounced, batched memoryAccessed events through the event log so the
		// memory_strength projection reinforces frequently-recalled memories.
		accessRecorder := newMemoryAccessRecorder(orch.Log, logger)
		hybrid.SetAccessRecorder(accessRecorder)
		w.closers = append(w.closers, accessRecorder.Close)
		w.Query = hybrid
		// Pass the FileEventLog as the BackgroundWaiter so recall drains
		// async extraction + resolution before searching. Without this the
		// LongMemEval feeder times out: remembers schedule background LLM
		// tool-use that takes 17-24s/call, and recall fired immediately
		// after the feed loop would see empty entity / mention tables.
		w.Tools = append(w.Tools, tools.NewRecall(hybrid).
			WithBackgroundWaiter(orch.Log).
			WithEventLookup(orch.Log).
			WithFreshnessPolicy(config.ResolveFreshnessPolicy(user.Connectors)))

		// getContext assembles the cold-start brief from the navigator (pinned +
		// recent documents, key entities) and hybrid recall (query-relevant hits).
		// Registered here, after hybrid is built, so the query lane is wired.
		registerContextTools(w, orch, hybrid, logger, w.ConflictDetector)

		if chat != nil {
			if wireErr := wireMemoryLayer(ctx, w, orch, hybrid, chat, user, cfg.DataDir, logger); wireErr != nil {
				return nil, fmt.Errorf("daemon: wire memory layer: %w", wireErr)
			}
		} else {
			logger.Info("daemon: chat provider disabled; extractor + resolver + ask tool skipped")
			// wireMemoryLayer (which registers definePredicate) is skipped without a
			// chat provider; register the standalone authoring variant so the schema
			// surface stays symmetric with defineEntityKind. See the no-embedder
			// branch above for the rationale.
			registerDefinePredicateDegraded(ctx, w, orch, logger)
		}
	}

	// Embedder-independent tail — runs in BOTH the degraded (no-embedder) and
	// full modes. Start the vault watcher AFTER wireMemoryLayer so
	// w.ProseExtractor is set when prose extraction is available; in degraded or
	// no-chat mode the watcher still runs with prose extraction skipped
	// (w.ProseExtractor remains nil).
	if user.Vault.Watch {
		if werr := startVaultWatcher(ctx, w, orch, vaultPath, logger); werr != nil {
			return nil, werr
		}
	}

	// Sync is independent of the embedder: sealing, pushing, and the team
	// consume/decode loop must run even when embeddings are disabled. Keeping
	// this in the common tail (not the embedder-present branch) fixes the bug
	// where a `defaults.embed=none` install silently never sealed or synced any
	// scope because OpenWiring returned early in the degraded branch.
	//
	// The dedup sweep is wired here — after navigation tools opened the
	// dispatcher's SQLite + vector handles — so the sweep shares the same
	// read/write pools the rest of the daemon uses. Nil when storage is not
	// the production dispatcher (test fakes) or any handle fails to open.
	dedupSweep := buildDedupSweepOrNil(ctx, orch, orch.Log, logger)
	w.ConflictDetector = buildConflictDetectorOrNil(ctx, orch, orch.Log, logger)
	if err := startSyncSupervisor(ctx, w, cfg.DataDir, orch.Log, user.Sync, secretChain,
		teamRecipientSourceFor(orch), orch.Log, teamMembershipReaderFor(orch),
		gitIdentitySourceFor(orch), dedupSweep, w.ConflictDetector,
		genesisAnchorKeySourceFor(orch), logger); err != nil {
		return nil, err
	}

	wired = w
	return wired, nil
}

// startSyncSupervisor builds and starts the Moria sync supervisor.
// Extracted from OpenWiring to keep cyclomatic complexity under the
// linter gate. A nil supervisor is the documented "no Watchword + no
// remote" no-op sentinel; errors here are fatal (corrupt Watchword
// would silently break audits, better to refuse boot).
func startSyncSupervisor(
	ctx context.Context,
	w *Wired,
	dataDir string,
	source chunk.EventSource,
	syncCfg config.SyncTable,
	secrets secret.Provider,
	recipientSrc teamRecipientSource,
	grafter teamGrafter,
	members teamMembershipReader,
	idSource gitIdentitySource,
	sweep *DedupSweep,
	det conflictDetector,
	genesisKeys genesisAnchorKeySource,
	logger *zap.Logger,
) error {
	syncCfg = applyEncryptionMigration(ctx, dataDir, syncCfg, secrets, logger)
	deviceID := resolveDeviceTag(ctx, secrets, logger)
	selfPrincipalUID, uidErr := config.SelfPrincipalUID(dataDir)
	if uidErr != nil {
		// An unreadable state file is non-fatal: identity resolution falls back
		// to git config / the sentinel, so sync still works without authorship.
		logger.Warn("daemon: could not read self principal for sync identity", zap.Error(uidErr))
	}
	// Resolved once, here, and baked into the supervisor's AuthorName/AuthorMail
	// below. A later hot-reload of [sync.identity] (config.toml / state.toml)
	// updates the merged config but does NOT reach the already-constructed git
	// transport — changing your commit identity requires a daemon restart.
	id := syncidentity.Resolve(ctx, syncCfg.Identity, deviceID, selfPrincipalUID, idSource, logger)
	logger.Info("daemon: sync commit identity resolved",
		zap.String("name", id.Name), zap.String("email", id.Email), zap.String("mode", id.Mode))
	sv, err := newSyncSupervisor(ctx, &syncSupervisorConfig{
		DataDir:          dataDir,
		DeviceID:         deviceID,
		Source:           source,
		Sync:             syncCfg,
		SecretChain:      secrets,
		Recipients:       recipientSrc,
		Grafter:          grafter,
		Members:          members,
		DedupSweep:       sweep,
		ConflictDetector: det,
		GenesisKeys:      genesisKeys,
		AuthorName:       id.Name,
		AuthorMail:       id.Email,
		CommitMode:       id.Mode,
		SelfPrincipalUID: selfPrincipalUID,
		CoAuthors:        syncidentity.CoAuthorResolverOrNil(idSource),
		Logger:           logger,
	})
	if err != nil {
		return fmt.Errorf("daemon: wire sync supervisor: %w", err)
	}
	if sv != nil {
		sv.Start(ctx)
		w.closers = append(w.closers, sv.Close)
	}
	return nil
}

// buildConflictDetectorOrNil constructs the post-consume supersede-conflict
// detector when the orchestrator's storage is the production dispatcher.
// Returns nil (disabled) when storage is a test fake or any resource fails.
func buildConflictDetectorOrNil(ctx context.Context, orch *orchestrator.Orchestrator, log kernel.EventLog, logger *zap.Logger) conflictDetector {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return nil
	}
	reader, rerr := disp.SQLiteReader()
	if rerr != nil {
		logger.Warn("daemon: conflict detector sqlite reader unavailable; conflict detection disabled", zap.Error(rerr))
		return nil
	}
	reg, rerr := resolver.NewRegistry(ctx, reader)
	if rerr != nil {
		logger.Warn("daemon: conflict detector predicate registry unavailable; conflict detection disabled", zap.Error(rerr))
		return nil
	}
	return newSyncConflictDetector(reader, log, reg, logger)
}

// buildDedupSweepOrNil constructs the post-consume dedup sweep when the
// orchestrator's storage is the production dispatcher. Returns nil (disabled)
// when storage is a test fake or any resource fails to open.
func buildDedupSweepOrNil(ctx context.Context, orch *orchestrator.Orchestrator, appender sweepAppender, logger *zap.Logger) *DedupSweep {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return nil
	}
	reader, rerr := disp.SQLiteReader()
	if rerr != nil {
		logger.Warn("daemon: dedup sweep sqlite reader unavailable; sweep disabled", zap.Error(rerr))
		return nil
	}
	writer, werr := disp.OpenSQLite(ctx)
	if werr != nil {
		logger.Warn("daemon: dedup sweep sqlite writer unavailable; sweep disabled", zap.Error(werr))
		return nil
	}
	vec, verr := disp.OpenVector(ctx)
	if verr != nil {
		// Vector lane optional — degrade to exact-name + identifier only.
		logger.Warn("daemon: dedup sweep name-KNN lane disabled (vector open failed); exact-name + identifier dedup still active",
			zap.Error(verr))
		vec = nil
	}
	var emb kernel.Embedder
	if orch.Embedder != nil {
		emb = orch.Embedder
	}
	lu := entitymatch.NewLookup(reader, vec, emb, 0, 0)
	return NewDedupSweep(reader, writer, appender, lu, logger)
}

// buildOrchestrator marks the ontology + orchestrator boot phases and
// constructs orch with an optional embedder. Extracted from OpenWiring
// to keep cyclomatic complexity under the linter threshold.
//
// Emb may be nil (defer-embedder branch or transient probe miss); the
// orchestrator.Config.Embedder field is interface-typed, so the field
// is left zero when emb is nil to avoid stamping a typed-nil onto the
// interface (a typed-nil leaks as non-nil interface, which would
// crash downstream nil-checks).
func buildOrchestrator(ctx context.Context, cfg WiringConfig, emb embedder.Embedder, user *config.UserConfig, st stats.Store, secrets secret.Provider, logger *zap.Logger) (*orchestrator.Orchestrator, error) {
	cfg.BootTracer.Mark(PhaseOntology)
	ont, err := buildOntology(ctx)
	if err != nil {
		return nil, err
	}
	orchCfg := orchestrator.Config{
		DataDir:  cfg.DataDir,
		Ontology: ont,
		Logger:   logger,
		Stats:    st,
	}
	if user != nil {
		orchCfg.DedupSemanticThreshold = user.Ingestion.DedupSemanticThreshold
		orchCfg.StrictAttributes = user.Ontology.StrictAttributes
		orchCfg.StrictDomainRange = user.Ontology.StrictDomainRange
	}
	if emb != nil {
		orchCfg.Embedder = emb
	}
	cfg.BootTracer.Mark(PhaseOrchestratorNew)
	orch, err := orchestrator.New(ctx, orchCfg)
	if err != nil {
		return nil, fmt.Errorf("daemon: open orchestrator: %w", err)
	}

	// Late-bind: attach the SQLite reader to the ontology now that the
	// dispatcher has opened the pool. Before this point ValidateEntityKind
	// is a no-op; after this point it queries entity_kinds.
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if ok {
		reader, rerr := disp.SQLiteReader()
		if rerr != nil {
			return nil, fmt.Errorf("daemon: sqlite reader for ontology attach: %w", rerr)
		}
		if aerr := ont.AttachStore(reader); aerr != nil {
			return nil, fmt.Errorf("daemon: attach ontology store: %w", aerr)
		}

		// Install the team-scope replay verifier: device public keys resolve
		// from the devices table via the dispatcher. Personal/project events
		// skip verification (the scope guard in Rebuild gates it), so this is
		// dormant until team scopes (issue #213) introduce team-scope events.
		orch.Log.SetVerifier(identity.NewVerifier(disp))

		// Install the live team-scope write gate: a team-scope write by a
		// non-member is rejected with kernel.ErrPermission at the append
		// boundary (the genesis self-grant is the one bootstrap exception).
		// Backed by the dispatcher's team_membership_acl projection.
		orch.Log.SetTeamWriteAuthority(disp)

		// Install the role-at-position checker for the reader-content gate on
		// live graft and Rebuild replay. TeamMemberRoleAt re-folds only the
		// membership events below the verified event's UID, so both paths
		// reach the same verdict regardless of arrival order.
		orch.Log.SetTeamRoleChecker(disp)

		// Install the convergent device-revocation checker. DeviceRevokedAt
		// queries device_revocations with event_uid < beforeEventUID so the
		// verdict is position-pure: every peer drops events by revoked devices
		// with UID > revocation UID, regardless of arrival order.
		orch.Log.SetDeviceRevokeChecker(disp)

		// Install the genesis trust anchor so the membership fold is pinned
		// to the known genesis principal rather than the TOFU lowest-ULID root.
		// A missing pin (fresh device, pre-B3 state.toml) is non-fatal: the
		// fold falls back to TOFU as before. The doctor check will WARN in that
		// case so the operator knows to run `maz team pin` to anchor.
		if uid, kid, ok, perr := config.LoadTeamGenesisPin(cfg.DataDir); perr != nil {
			logger.Warn("daemon: could not load team genesis pin from state.toml (non-fatal; fold falls back to TOFU)",
				zap.Error(perr),
			)
		} else if ok {
			disp.SetGenesisPin(&ontology.GenesisPin{
				PrincipalUID: uid,
				SigKeyID:     kid,
			})
			logger.Debug("daemon: team genesis pin installed",
				zap.String("principal_uid", uid),
				zap.String("sig_key_id", kid),
			)
		}

		// Append genesis events on first boot (idempotent: checks for
		// 'person' row before writing). A seed failure is non-fatal: warn
		// and continue so a transient SQLite hiccup does not brick boot.
		writer, werr := disp.OpenSQLite(ctx)
		if werr != nil {
			logger.Warn("daemon: could not obtain sqlite writer for ontology seed",
				zap.Error(werr),
			)
		} else if serr := kernelboot.SeedOntology(ctx, orch.Log, writer); serr != nil {
			logger.Warn("daemon: ontology seed failed (non-fatal; retries on next boot)",
				zap.Error(serr),
			)
		} else {
			// Identity must seed AFTER ontology so the 'person' kind exists
			// for the principal's materialized entity, and it reuses the same
			// writer pool. It also installs the real Watchword-derived signer
			// on the live log for every subsequent write.
			seedIdentityOrWarn(ctx, orch.Log, writer, secrets, cfg.DataDir, logger)
		}
	}

	logTuningApplied(user, logger)
	return orch, nil
}

// seedIdentityOrWarn runs the genesis identity ceremony and installs the
// real event signer. Every failure mode is non-fatal: a missing Watchword
// (pre-wizard) defers genesis, and any other error is logged and retried on
// the next boot rather than bricking startup.
func seedIdentityOrWarn(ctx context.Context, log kernel.EventLog, writer *sql.DB, secrets secret.Provider, dataDir string, logger *zap.Logger) {
	idStore := identity.New(secrets)
	anchorer := identity.NewGitHubAnchorer()
	if serr := kernelboot.SeedIdentity(ctx, log, writer, idStore, anchorer, dataDir); serr != nil {
		logger.Warn("daemon: identity seed failed (non-fatal; retries on next boot)",
			zap.Error(serr),
		)
		return
	}
	// Best-effort: upgrade a principal that was minted without a GitHub anchor
	// (offline / no `gh` / no published key at genesis) now that connectivity may
	// have returned. Idempotent — a no-op once anchored or if still unreachable.
	if uerr := kernelboot.UpgradeGitHubAnchor(ctx, log, writer, idStore, anchorer, dataDir); uerr != nil {
		logger.Warn("daemon: github anchor upgrade failed (non-fatal; retries on next boot)",
			zap.Error(uerr),
		)
	}
	// Best-effort: link the local-only self anchor (uid="self", "Me") to the bound
	// principal's portable person entity so the graph shows one identity instead
	// of an isolated stub. Idempotent — a no-op once linked or before genesis.
	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, writer, dataDir); lerr != nil {
		logger.Warn("daemon: self-principal link failed (non-fatal; retries on next boot)",
			zap.Error(lerr),
		)
	}
}

// logTuningApplied emits one INFO line per non-default ingestion/ontology knob
// the user enabled. Split out of buildOrchestrator so the latter stays under
// the gocyclo threshold as more knobs are threaded through.
func logTuningApplied(user *config.UserConfig, logger *zap.Logger) {
	if user == nil {
		return
	}
	if user.Ingestion.DedupSemanticThreshold > 0 {
		logger.Info("daemon: ingestion tuning applied",
			zap.String("knob", "dedup_semantic_threshold"),
			zap.Float64("value", user.Ingestion.DedupSemanticThreshold),
		)
	}
	if user.Ontology.StrictAttributes {
		logger.Info("daemon: ontology tuning applied",
			zap.String("knob", "strict_attributes"),
			zap.Bool("value", true),
		)
	}
	if user.Ontology.StrictDomainRange {
		logger.Info("daemon: ontology tuning applied",
			zap.String("knob", "strict_domain_range"),
			zap.Bool("value", true),
		)
	}
}

// maybeResolveEmbedder returns the embedder client, honouring the two
// env-switches that bypass the Ollama HTTP probe entirely:
// MAZ_DEFER_EMBEDDER and MAZ_MINIMAL_BOOT. Returning a nil embedder
// (with nil error) makes recall register as Disabled but lets the
// daemon boot — the operator-facing escape hatch when the embedder
// probe is the suspected freeze source. Extracted from OpenWiring to
// keep its cognitive complexity below the linter threshold.
func maybeResolveEmbedder(ctx context.Context, cfg WiringConfig, user config.UserConfig, logger *zap.Logger) (embedder.Embedder, embedderMode, error) { //nolint:gocritic // called once at boot; user is large but cheap to copy here
	if deferEmbedder() {
		logger.Warn("daemon: embedder probe deferred by env; recall degrades to FTS-only",
			zap.String("env", DeferEmbedderEnv),
			zap.Bool("minimalBoot", minimalBoot()),
		)
		return nil, embedderDegraded, nil
	}
	cfg.BootTracer.Mark(PhaseEmbedderProbe)
	return resolveEmbedder(ctx, user, logger)
}

// resolveEmbedder runs buildEmbedder and maps the outcome to an embedderMode.
// The contract:
//   - (client, embedderActive, nil): probe succeeded; full hybrid recall.
//   - (nil, _, err): fatal error (configuration, missing model, or
//     cancellation); the daemon refuses to start so the operator can fix the
//     underlying problem.
//   - (nil, embedderDegraded, nil): a transient probe failure (Ollama
//     unreachable). OpenWiring registers FTS-only recall instead of disabling
//     it, so the "offline binary" serves BM25 hits on first contact (#236).
//   - (nil, embedderOptOut, nil): defaults.embed=none/"" — #105's explicit
//     opt-out. OpenWiring registers the disabled recall stub.
//
// The degraded/opt-out split is what makes "Ollama down" a graceful
// degradation (FTS-only) rather than a hard failure or a misleading "disabled".
func resolveEmbedder(ctx context.Context, user config.UserConfig, logger *zap.Logger) (embedder.Embedder, embedderMode, error) { //nolint:gocritic // called once at daemon startup; copy cost negligible vs. pointer-aliasing risk
	emb, err := buildEmbedder(ctx, user, logger)
	if classifyErr := classifyEmbedderErr(err, logger); classifyErr != nil {
		// Fatal: the caller checks err first, so this mode is never read — but
		// embedderActive would be a misleading label for "startup failed", so
		// return the non-active sentinel.
		return nil, embedderDegraded, classifyErr
	}
	if err != nil {
		// Transient: classify decided this is recoverable and warn-logged it.
		// Recall degrades to FTS-only rather than disabling (#236).
		//nolint:nilerr // intentional: a transient miss becomes the degraded mode, not a returned error.
		return nil, embedderDegraded, nil
	}
	if emb == nil {
		// buildEmbedder returned (nil, nil): explicit config opt-out (#105).
		return nil, embedderOptOut, nil
	}
	return emb, embedderActive, nil
}

// classifyEmbedderErr decides whether a buildEmbedder failure must
// fail the daemon or downgrade to write-only mode. The split draws the
// line at "can this ever work as configured?": a genuine config typo or
// a caller-cancelled boot is fatal and surfaced loudly; an availability
// problem (Ollama down, or the model simply not pulled yet) degrades to
// FTS-only so the "offline binary" still serves recall on first contact
// (#236). Degradation is never silent — recall responses carry
// degraded:true and `maz doctor` warns with the actionable fix.
//
// Returns:
//   - nil for err == nil (happy path; caller uses the live embedder).
//   - nil for availability failures (caller degrades to FTS-only):
//   - a transient probe failure (Ollama unreachable), and
//   - ollama.ErrModelMissing — the embed model is not pulled. This is an
//     availability gap, not a config bug: `ollama pull <model>` plus a
//     `maz rebuild` restores full hybrid, and refusing to boot here would
//     wall a fresh install's very first recall (the #236 launch gate).
//   - non-nil for fatal classes (cannot work as configured / aborted):
//   - errUnsupportedEmbedProvider (config: defaults.embed key unknown to M1),
//   - ollama.ErrInvalidConfig (config: BaseURL malformed, returned by
//     ollama.New BEFORE any HTTP probe — pinning Greptile finding on
//     #119 that pre-probe config errors must not be swallowed), and
//   - context.Canceled / context.DeadlineExceeded (caller asked us to
//     stop).
func classifyEmbedderErr(err error, logger *zap.Logger) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errUnsupportedEmbedProvider),
		errors.Is(err, ollama.ErrInvalidConfig),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return err
	}
	if errors.Is(err, ollama.ErrModelMissing) {
		logger.Warn("daemon: embed model not installed; recall degrades to FTS-only (BM25). Run `ollama pull <embed-model>` then `maz rebuild` to restore full hybrid",
			zap.Error(err),
		)
		return nil
	}
	logger.Warn("daemon: embedder unavailable, starting in degraded write-only mode",
		zap.Error(err),
	)
	return nil
}

// vecReporterFor returns the dispatcher as a tools.VecReporter when the
// orchestrator's storage is the production dispatcher, or nil (treated as
// "always idle") for test fakes. Extracted from OpenWiring to keep that
// function's cyclomatic complexity under the linter threshold.
func vecReporterFor(orch *orchestrator.Orchestrator) tools.VecReporter {
	if disp, ok := orch.Storage.(*dispatcher.Dispatcher); ok {
		return disp
	}
	return nil
}

// registerNavigationTools builds the Navigator over the dispatcher's SQLite
// reader and appends the LLM-free navigation verbs (get/neighbors/relations/
// entities/introspect) plus the direct-write relate to w.Tools. When storage
// is not the production dispatcher (a test fake), it logs and skips the surface
// rather than failing the boot. A fresh buildOntology supplies introspect's
// event-type list: it is deterministic and self-contained, so re-deriving it
// here avoids threading the orchestrator's registry through every caller.
// The vaultPath argument is the resolved markdown-vault directory for the
// document projector/importer. The ont argument carries the user-configured
// thresholds for the dedup checker; zero values fall back to the dedup package
// defaults.
func registerNavigationTools(ctx context.Context, w *Wired, orch *orchestrator.Orchestrator, vaultPath, dataDir string, secrets secret.Provider, logger *zap.Logger, ont config.OntologyTable, sync config.SyncTable, freshness config.FreshnessPolicy) error {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		logger.Warn("daemon: navigation tools skipped (storage is not the production dispatcher)",
			zap.String("storage_type", fmt.Sprintf("%T", orch.Storage)))
		return nil
	}
	reader, err := disp.SQLiteReader()
	if err != nil {
		return fmt.Errorf("daemon: dispatcher sqlite reader for navigation: %w", err)
	}
	// AllScopes: base navigator for MCP navigation tools; each tool narrows per-request via navWithScopes.
	nav, err := navigate.NewAllScopes(reader)
	if err != nil {
		return fmt.Errorf("daemon: build navigator: %w", err)
	}
	// vaultNav spans ALL four scopes (incl. draft) — used ONLY by the document
	// importer (scope-exact dedup for draft) and the vault projector (SyncAll
	// materializes every scope's files to <root>/<scope>/). It never widens the
	// recall/navigation read set: `nav` above stays draft-blind.
	vaultNav, err := navigate.NewVaultScopes(reader)
	if err != nil {
		return fmt.Errorf("daemon: build vault navigator: %w", err)
	}
	ontSchema, err := buildOntology(ctx)
	if err != nil {
		return fmt.Errorf("daemon: ontology for introspect: %w", err)
	}
	// Dynamic-ontology authoring surface (P4). defineEntityKind runs the
	// reuse-before-mint dedup check; it degrades to lexical-only when no
	// embedder is live (orch.Embedder nil in degraded write mode). A typed-nil
	// embedder.Embedder would defeat dedup's `embedder == nil` guard, so pass
	// the interface only when it is genuinely set. User-configured thresholds
	// from [ontology] dedup_high_threshold / dedup_medium_threshold are
	// threaded through dedup.Options; zero values use the package defaults.
	var dedupEmb dedup.Embedder
	if orch.Embedder != nil {
		dedupEmb = orch.Embedder
	}
	deduper := dedup.NewWithOptions(nav, dedupEmb, dedup.Options{
		ReuseThreshold:     ont.DedupHighThreshold,
		MintAliasThreshold: ont.DedupMediumThreshold,
	})
	// entityMatcher is the shared create-time dedup lookup wired into
	// createEntity so a bulk mint refuses to duplicate an existing entity. It
	// reuses the resolver's exact-name + name-embedding + identifier lanes. A nil
	// embedder (degraded write mode) or a failed vector open degrades it to
	// exact-name + identifier matching — exact duplicates are still blocked.
	var matchEmb kernel.Embedder
	if orch.Embedder != nil {
		matchEmb = orch.Embedder
	}
	matchVec, verr := disp.OpenVector(ctx)
	if verr != nil {
		logger.Warn("daemon: entity-dedup name-vector lane disabled (vector open failed); exact-name + identifier dedup still active",
			zap.Error(verr))
		matchVec = nil
	}
	// Thread the SAME user-configured thresholds the resolver's deduper uses
	// ([ontology] dedup_high_threshold / dedup_medium_threshold) so the direct
	// create path and the extraction path apply identical reuse/advisory cutoffs;
	// a zero value (unconfigured) falls back to the package defaults inside
	// NewLookup, matching dedup.NewWithOptions.
	entityMatcher := entitymatch.NewLookup(reader, matchVec, matchEmb, ont.DedupHighThreshold, ont.DedupMediumThreshold)
	// selfBinder rebinds the "self" sentinel to the concrete principal UID on
	// the write-side tools (relate/fact) so first-person writes persist the
	// shareable principal rather than the local-only literal. Bound here, after
	// genesis has populated state.toml in buildOrchestrator.
	binder := config.NewSelfBinder(dataDir)
	// Give the already-wired forget tool (registered before this nav-dependent
	// block) a read-side existence guard so it returns E_NOT_FOUND on a bogus or
	// wrong-kind uid instead of fabricating a tombstone for a record that never
	// existed. Skipped silently in minimal-boot / test-fake wiring (no nav).
	for _, t := range w.Tools {
		if ft, ok := t.(*tools.Forget); ok {
			ft.SetExistenceChecker(nav)
		}
	}
	relateTool := tools.NewRelate(orch.Log, nav)
	relateTool.SetSelfBinder(binder)
	// SetNameNavigator enables display-name resolution for subject/object: passing
	// "Bilbo Baggins" resolves to the entity UID the same way maz get does.
	relateTool.SetNameNavigator(nav)
	// unrelate is relate's temporal inverse: it END-DATES an edge via a
	// relationRetracted tombstone (valid_until) so a relationship that ended is
	// recorded without lying (a parallel dead edge) or destroying history
	// (forget). nav resolves the target by relations/{uid} or by an open
	// (subject, predicate, object) triple; the binder rebinds a "self" endpoint.
	unrelateTool := tools.NewUnrelate(orch.Log, nav)
	unrelateTool.SetSelfBinder(binder)
	factTool := tools.NewFact(orch.Log, nav)
	factTool.SetSelfBinder(binder)
	// SetNameNavigator enables display-name resolution for the fact subject.
	factTool.SetNameNavigator(nav)
	// retractFact is fact's temporal inverse (#2): it CLOSES the current value of
	// a slot via a factRetracted tombstone so a fact that stopped being true is
	// recorded without overwriting it or destroying history (forget). nav resolves
	// the target by facts/{uid} or by an open (subject, stateKey) slot; the binder
	// rebinds a "self" subject.
	retractFactTool := tools.NewRetractFact(orch.Log, nav)
	retractFactTool.SetSelfBinder(binder)
	// Read-side self-rebind: get/neighbors/relations resolve `self` to the bound
	// principal so first-person reads keep working after genesis binds identity
	// (the seeded "self" entity is left behind once writes move to the principal).
	getTool := tools.NewGet(nav)
	getTool.SetSelfBinder(binder)
	neighborsTool := tools.NewNeighbors(nav)
	neighborsTool.SetSelfBinder(binder)
	relationsTool := tools.NewRelations(nav)
	relationsTool.SetSelfBinder(binder)
	// Pull-based ingestion (#211): one orchestrator over the event log (orch.Log,
	// single-writer), the SQLite reader (dedup + watermark resume) and the job
	// registry (cancellable backfill). connectSource/ingestSource drive it;
	// listSources reads the same reader's watermarks. The secret chain supplies
	// the optional github.token that enables the PR-prose stream.
	ingestReader := ingest.NewSQLReader(reader)
	ingestOrch := ingest.NewOrchestrator(orch.Log, ingestReader, w.Jobs, logger)
	// Refuse connect/ingest into a scope that syncs as plaintext up front: the
	// #204 leak gate would refuse to seal connector:<name> content into a
	// plaintext chunk anyway, but only at seal time (events stuck pending). A
	// scope with no configured remote is local-only — no chunk, no leak — so the
	// guard fires only when a remote is bound AND encryption is off.
	plaintextGuard := func(scope kernel.Scope) bool {
		rt, ok := sync.Remotes[string(scope)]
		return ok && !rt.EncryptedOrDefault()
	}
	// exportCanvas (Unlisted) backs `maz doc canvas`: it walks the same scoped
	// navigator and serializes an N-hop subgraph to JSON Canvas. self-rebind so a
	// `self` seed resolves to the bound principal, like neighbors/relations.
	canvasTool := tools.NewExportCanvas(nav)
	canvasTool.SetSelfBinder(binder)
	w.Tools = append(w.Tools,
		getTool,
		neighborsTool,
		relationsTool,
		tools.NewEntities(nav, w.Stats),
		tools.NewCount(nav),
		tools.NewListPresets(nav),
		tools.NewQuery(reader),
		tools.NewIntrospect(nav, ontSchema),
		relateTool,
		// relateBatch asserts N (subject, predicate, object) edges in ONE
		// atomic append, so a graph seed lands as a single projection commit.
		// Shares relateTool's resolver/binder/nameNav so both tools behave
		// identically for endpoint resolution; construct AFTER relateTool.
		tools.NewRelateBatch(relateTool),
		unrelateTool,
		// Trust promotion (#212): flip a connector-proposed relation to asserted.
		// The scoped navigator pre-checks existence + current status (E_NOT_FOUND
		// on a typo, idempotent no-op when already asserted); the flip itself is a
		// relationPromoted event through orch.Log (single-writer lock).
		tools.NewPromoteRelation(orch.Log, nav),
		// External-source provenance (#207): getSourceRef reads through the same
		// scoped navigator; recordSourceRef appends a sourceRefRecorded event via
		// orch.Log so it respects the single-writer event-log lock. Provenance is
		// self-asserted/unverified — getSourceRef labels it explicitly.
		tools.NewGetSourceRef(nav),
		tools.NewRecordSourceRef(orch.Log),
		// Pull-based ingestion (#211): connect a local source and backfill, pull
		// deltas, or list connected sources. Ingested content is tagged
		// connector:<name> so the #204 leak gate + #211 resolver trust gate engage.
		tools.NewConnectSource(ingestOrch, secrets, plaintextGuard, logger),
		tools.NewIngestSource(ingestOrch, secrets, plaintextGuard, logger),
		tools.NewListSources(ingestReader),
		// listDupCandidates surfaces the entity_dup_candidates table populated by
		// the post-consume dedup sweep. Agents inspect it and call mergeEntities to
		// consolidate; the CLI equivalent is `maz review dups`.
		tools.NewListDupCandidates(reader),
		// Document / markdown-vault surface (P1 reads, P2 writes). Reads go
		// through the same navigator; the importer writes through orch.Log so
		// it respects the daemon's single-writer event-log lock. One importer
		// and one projector are shared by every document verb AND by the
		// Writeback goroutine in startVaultWatcher (which reuses w.Projector).
		// One projector -> one sync.Mutex -> one sidecar owner: I3 genuinely
		// serializes all materialization across both paths.
		tools.NewReadDocument(nav),
		tools.NewListDocuments(nav),
		tools.NewGetBacklinks(nav),
		canvasTool,
		// lintDocuments serves `maz doc lint` through the daemon. It used to open
		// the store in-process and die with a cryptic "status 1" whenever a daemon
		// held the lock (the normal case); routing it here makes it work like every
		// other document read. The resolver getter is read LAZILY at invoke time:
		// the semantic name resolver is wired onto w.Importer later (in
		// wireMemoryLayer, after this registration), so the lint dead-link pass
		// can suppress a stale snapshot link a semantically-close entity now
		// satisfies. Returns nil when the semantic lane is disabled.
		tools.NewLintDocuments(nav, func() document.SemanticNameResolver {
			if w.Importer != nil {
				return w.Importer.SemanticResolver()
			}
			return nil
		}, logger),
		// documentHistory reads the full revision chain from the event log
		// (orch.Log); the documents projection keeps only head state. Powers
		// `maz doc log` / `maz doc diff`.
		tools.NewDocumentHistory(orch.Log, nav),
		// Fact: direct attribute assertion for structured scalar state slots.
		// Landed by #159; wired here alongside document tools.
		factTool,
		retractFactTool,
		tools.NewDefineEntityKind(orch.Log, deduper, nav, ont.Governed()),
		tools.NewDefineSchemaWithLister(orch.Log, nav),
		// NewCreateEntity is registered further down, in the document-tools append,
		// because it needs the document Importer as its link reconciler (G6) and the
		// importer is constructed after this block. Tool order is irrelevant (the
		// registry is keyed by name).
		tools.NewUpdateEntity(orch.Log, nav),
		// AddIdentifier attaches an alias/identifier to an existing entity so a
		// rename via updateEntity does not orphan the old surface form (#1).
		tools.NewAddIdentifier(orch.Log, nav),
		tools.NewMergeEntity(orch.Log, nav),
		// DeleteEntity hard-deletes an entity (and cascades its relations); the
		// safe-delete guard and cascade both read through nav.
		tools.NewDeleteEntity(orch.Log, nav),
		// ShareToTeam re-publishes a personal-scope record (entity, fact, or
		// relation) as ordinary team-scoped events. The personal original is
		// unchanged; both copies coexist under separate scope lenses. Entity
		// dedup against the team scope runs by strong identifier first (via
		// entityMatcher), then falls back to exact display-name match.
		// Requires admitted team membership — a reader-role caller surfaces
		// E_PERMISSION from mapAppendError.
		tools.NewShareToTeam(orch.Log, nav).WithMatcher(entityMatcher),
		// External references (#208): reference mints-or-returns an external_ref
		// entity for a URI (deduped within scope via the scoped navigator),
		// appending through orch.Log so it respects the single-writer event-log
		// lock. The has_primary_source edge is a separate relate() call.
		tools.NewReference(orch.Log, nav),
		// Agent-mediated external capture (captureExternal): the agent fetches an
		// item from its own live connector and stores a pointer + summary +
		// freshness anchor through orch.Log (single-writer). nav resolves prior
		// captures (idempotency/supersede) and external_ref existence; secrets holds
		// the per-install HMAC key; the freshness policy drives the TTL + the
		// ephemeral write-gate. Refuses project scope and ephemeral connectors.
		tools.NewCaptureExternal(orch.Log, nav, secrets, freshness),
		tools.NewDescribeType(nav),
		// Dynamic lessons + feedback loop (Lane E): agentFeedback captures durable
		// friction/lessons as a signed agentFeedbackRecorded event through orch.Log.
		// feedbackReview is the unlisted human-override channel (pin/edit/veto) — not
		// an agent action. feedbackQuery reads the agent_feedback projection directly
		// for triage/export. Recurring clusters auto-promote into the per-session
		// "## Known gotchas & lessons" instructions section (see newInstructionComposer).
		tools.NewAgentFeedback(orch.Log, reader),
		tools.NewFeedbackReview(orch.Log, reader),
		tools.NewFeedbackQuery(reader),
		// Draft-staging workflow (Lane F): promoteDraft / discardDraft / draftStage
		// are Unlisted tools that back the `maz scope draft` CLI surface. They use
		// orch.Log for writes (single-writer lock) and nav for draft-scoped reads.
		tools.NewPromoteDraft(orch.Log, nav),
		tools.NewDiscardDraft(orch.Log, nav),
		tools.NewDraftStage(nav),
	)
	// WithSelfBinder rebinds an "entity: self" frontmatter attribution to the
	// concrete principal UID, mirroring the relate/fact tools, so document
	// authorship syncs as the shareable principal rather than the local literal.
	importer := document.NewImporter(orch.Log, vaultNav, logger).WithSelfBinder(binder)
	// Store the importer so wireMemoryLayer can late-bind the ProseExtractor
	// onto it after the chat provider is available. All tools below capture the
	// same *document.Importer pointer, so a later WithProseExtractor call
	// (which mutates the struct in place) is visible to them without re-wiring.
	w.Importer = importer
	// Build the projector once and store it on w so startVaultWatcher can reuse
	// the same instance. One Projector -> one sync.Mutex -> one sidecar owner;
	// I3 (single serialized writer) genuinely holds across both the MCP document
	// tools and the Writeback goroutine.
	w.Projector = vault.NewProjector(vaultNav, vaultPath, logger)
	w.Tools = append(w.Tools,
		// createEntity is wired HERE (not with the other entity tools above) so it
		// can take the document Importer as its link reconciler: minting an entity
		// re-resolves every existing document's dangling about:/[[wikilink]] that now
		// name-matches the new entity and emits the previously-missing edges
		// (best-effort, idempotent; closes G6 — createEntity used to leave those
		// links dangling until the next document re-import). The importer is the same
		// pointer captured below, so the late WithProseExtractor bind is visible too.
		tools.NewCreateEntity(orch.Log, nav).WithLinkReconciler(importer).WithDedup(entityMatcher).WithLogger(logger),
		tools.NewImportDocuments(importer, vaultPath, w.Projector),
		// WithCollisionChecker(nav) enables the non-fatal title-shadowing
		// warning: a write whose document title matches a non-document entity
		// reports a warning without mutating the body. nil-safe; nav satisfies
		// the writeCollisionChecker (ListEntities) surface.
		tools.NewWriteDocument(importer, w.Projector).WithCollisionChecker(nav),
		tools.NewEditDocument(nav, importer, w.Projector).WithCollisionChecker(nav),
		tools.NewMaterializeVault(w.Projector),
		// Destructive / structural-mutation tools (UNLISTED: hidden from
		// tools/list, always dispatchable via tools/call by name).
		tools.NewDeleteDocument(importer, w.Projector),
		tools.NewMoveDocument(nav, importer, w.Projector),
	)
	return nil
}

// registerTeamTools wires the team-scope membership lifecycle (#213):
// team_create (genesis), team_grant, and team_revoke. The team.Service it
// builds caches per-epoch DEKs in the keychain (hence the raw secret chain,
// not the Get-only mcp adapter), appends signed team events through orch.Log
// (the append boundary enforces member authority), and reads the derived ACL
// through the dispatcher. Skipped — with a warn, matching the navigation-tool
// degradation — when storage is not the production dispatcher.
func registerTeamTools(w *Wired, orch *orchestrator.Orchestrator, secrets secret.Provider, dataDir string, logger *zap.Logger) {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		logger.Warn("daemon: team tools skipped (storage is not the production dispatcher)",
			zap.String("storage_type", fmt.Sprintf("%T", orch.Storage)))
		return
	}
	svc, err := team.New(&team.Deps{
		Appender:   orch.Log,
		WrapKeys:   identity.New(secrets),
		Secrets:    secrets,
		Membership: teamMembershipAdapter{disp: disp},
		Self:       config.NewSelfBinder(dataDir),
		Config:     team.NewConfigFileMutatorWithIdentity(dataDir, identity.New(secrets)),
		Devices:    deviceListerAdapter{disp: disp},
	})
	if err != nil {
		logger.Warn("daemon: team tools unavailable", zap.Error(err))
		return
	}
	w.Tools = append(w.Tools,
		tools.NewTeamCreate(svc),
		tools.NewTeamJoin(svc),
		tools.NewTeamGrant(svc),
		tools.NewTeamRevoke(svc),
		tools.NewTeamMembers(svc),
		tools.NewTeamKey(svc),
		tools.NewTeamRoleSet(svc),
		tools.NewTeamDeviceList(svc),
		tools.NewTeamDeviceRevoke(svc),
	)
}

// deviceListerAdapter maps the dispatcher's device queries onto the
// team.DeviceLister interface, converting dispatcher.DeviceInfo to
// team.DeviceInfo so the dispatcher stays free of a team package import.
type deviceListerAdapter struct{ disp *dispatcher.Dispatcher }

func (a deviceListerAdapter) ListDevices(ctx context.Context, principalUID string) ([]team.DeviceInfo, error) {
	rows, err := a.disp.ListDevices(ctx, principalUID)
	if err != nil {
		return nil, err
	}
	out := make([]team.DeviceInfo, len(rows))
	for i := range rows {
		r := &rows[i]
		out[i] = team.DeviceInfo{
			DeviceID:      r.DeviceID,
			PrincipalUID:  r.PrincipalUID,
			SigKeyID:      r.SigKeyID,
			DeviceLabel:   r.DeviceLabel,
			VouchSigKeyID: r.VouchSigKeyID,
			CreateTime:    r.CreateTime,
			RevokedAt:     r.RevokedAt,
			RevokedReason: r.RevokedReason,
		}
	}
	return out, nil
}

func (a deviceListerAdapter) GetDeviceByID(ctx context.Context, deviceID string) (team.DeviceInfo, bool, error) {
	r, found, err := a.disp.GetDeviceByID(ctx, deviceID)
	if err != nil || !found {
		return team.DeviceInfo{}, found, err
	}
	return team.DeviceInfo{
		DeviceID:      r.DeviceID,
		PrincipalUID:  r.PrincipalUID,
		SigKeyID:      r.SigKeyID,
		DeviceLabel:   r.DeviceLabel,
		VouchSigKeyID: r.VouchSigKeyID,
		CreateTime:    r.CreateTime,
		RevokedAt:     r.RevokedAt,
		RevokedReason: r.RevokedReason,
	}, true, nil
}

// teamMembershipAdapter maps the dispatcher's ACL queries onto the
// team.MembershipReader interface, keeping the low-level dispatcher free of a
// dependency on the higher-level team package.
type teamMembershipAdapter struct{ disp *dispatcher.Dispatcher }

func (a teamMembershipAdapter) GenesisOwner(ctx context.Context) (string, error) {
	return a.disp.GenesisOwner(ctx)
}

func (a teamMembershipAdapter) IsTeamMember(ctx context.Context, principalUID string) (bool, error) {
	return a.disp.IsTeamMember(ctx, principalUID)
}

func (a teamMembershipAdapter) MemberRole(ctx context.Context, principalUID string) (string, bool, error) {
	return a.disp.TeamMemberRole(ctx, principalUID)
}

func (a teamMembershipAdapter) CurrentDEKEpoch(ctx context.Context) (int, error) {
	return a.disp.CurrentDEKEpoch(ctx)
}

func (a teamMembershipAdapter) PrincipalDisplayName(ctx context.Context, principalUID string) (string, error) {
	return a.disp.PrincipalDisplayName(ctx, principalUID)
}

// PrincipalGitIdentity surfaces the principal's recorded GitHub identity for
// the sync transport's bring-your-own-git author resolution.
func (a teamMembershipAdapter) PrincipalGitIdentity(ctx context.Context, principalUID string) (login string, userID int64, displayName string, ok bool, err error) {
	return a.disp.PrincipalGitIdentity(ctx, principalUID)
}

// CoAuthor renders a peer principal's "Name <noreply-email>" line for the
// Co-authored-by trailer. Peers are always attributed via their GitHub
// no-reply email (their real git-config email is unknowable here, and noreply
// still attributes them on GitHub). Returns ok=false when the principal has no
// recorded GitHub login, so a malformed author line is never emitted.
func (a teamMembershipAdapter) CoAuthor(ctx context.Context, principalUID string) (string, bool) {
	login, userID, displayName, ok, err := a.disp.PrincipalGitIdentity(ctx, principalUID)
	if err != nil || !ok {
		return "", false
	}
	email := identity.NoreplyEmail(login, userID)
	if email == "" {
		return "", false
	}
	name := displayName
	if name == "" {
		name = identity.SanitizeGitHubLogin(login)
	}
	if name == "" {
		return "", false
	}
	return name + " <" + email + ">", true
}

func (a teamMembershipAdapter) AdmittedMembers(ctx context.Context) ([]team.Member, error) {
	rows, err := a.disp.AdmittedMembers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]team.Member, len(rows))
	for i, r := range rows {
		out[i] = team.Member{
			PrincipalUID:  r.PrincipalUID,
			WrapPublicKey: r.WrapPublicKey,
			DEKEpoch:      r.DEKEpoch,
			IsGenesis:     r.IsGenesis,
			Role:          r.Role,
		}
	}
	return out, nil
}

// TeamRecipients implements teamRecipientSource: it renders the admitted ACL
// into recipients.jsonl rows (wrapped DEKs + wrap keys + epoch; no PII) for the
// sync supervisor to stage. It uses the PER-EPOCH recipient rows (one row per
// (member, granted epoch)), not the ACL's collapsed current-epoch row, so a
// joining device unwraps the full DEK ring and can read the entire history — the
// full-history grant guarantee. CreateTime is left zero — the ACL does not
// record it, and a fixed value keeps the rendered file byte-stable across runs.
func (a teamMembershipAdapter) TeamRecipients(ctx context.Context) ([]recipients.Row, error) {
	rows, err := a.disp.AdmittedRecipientRows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]recipients.Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, recipients.Row{
			PrincipalUID:  r.PrincipalUID,
			WrapPublicKey: r.WrapPublicKey,
			DEKEpoch:      r.DEKEpoch,
			WrappedDEK:    r.WrappedDEK,
		})
	}
	return out, nil
}

// teamRecipientSourceFor returns a recipient source backed by the production
// dispatcher, or nil when storage is a test fake (the supervisor then seals
// team chunks but stages no recipients file).
func teamRecipientSourceFor(orch *orchestrator.Orchestrator) teamRecipientSource {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return nil
	}
	return teamMembershipAdapter{disp: disp}
}

// teamMembershipReaderFor adapts the dispatcher onto the consume loop's
// recipients ⊆ ACL cross-check. Returns nil when storage is not the
// production dispatcher (a test fake) — the cross-check is then skipped.
func teamMembershipReaderFor(orch *orchestrator.Orchestrator) teamMembershipReader {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return nil
	}
	return teamMembershipAdapter{disp: disp}
}

// gitIdentitySourceFor adapts the dispatcher onto the sync transport's
// bring-your-own-git author resolution + Co-authored-by trailer. Returns nil
// when storage is not the production dispatcher (a test fake) — the resolver
// then falls back to git config / the Mazarbul sentinel and emits no
// Co-authored-by lines.
func gitIdentitySourceFor(orch *orchestrator.Orchestrator) gitIdentitySource {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return nil
	}
	return teamMembershipAdapter{disp: disp}
}

// genesisAnchorKeySourceFor adapts the dispatcher onto the consume loop's
// TEAM.json anchor verification. Returns nil when storage is not the
// production dispatcher (a test fake) — the TEAM.json pin import is then
// disabled entirely, so an unverifiable anchor is never persisted.
func genesisAnchorKeySourceFor(orch *orchestrator.Orchestrator) genesisAnchorKeySource {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return nil
	}
	return disp
}

// registerContextTools wires the getContext tool: a brief assembler over the
// navigator (pinned + recent documents, key entities) and an optional recall
// lane. A nil hybrid is allowed — in degraded (no-embedder) mode the brief still
// serves pinned/recent documents and key entities; only the query-relevant
// section is disabled. Skipped when storage is not the production dispatcher (a
// test fake), matching registerNavigationTools' degradation.
func registerContextTools(w *Wired, orch *orchestrator.Orchestrator, hybrid *kernel.HybridQuery, logger *zap.Logger, det conflictDetector) {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return
	}
	reader, err := disp.SQLiteReader()
	if err != nil {
		logger.Warn("daemon: getContext skipped (sqlite reader unavailable)", zap.Error(err))
		return
	}
	// AllScopes: base navigator for getContext; scope narrowed per-request via WithScopeResolver.
	nav, err := navigate.NewAllScopes(reader)
	if err != nil {
		logger.Warn("daemon: getContext skipped (navigator build failed)", zap.Error(err))
		return
	}
	// Pass an untyped nil Recaller when there is no hybrid: handing a typed-nil
	// *kernel.HybridQuery to the interface would make assembler.recall != nil and
	// nil-deref on the first queried brief. The assembler guards an untyped-nil
	// recaller and simply omits the relevant section.
	var recall contextbrief.Recaller
	if hybrid != nil {
		recall = hybrid
	}
	// Rank "Key entities" by recall usage (recallCount DESC) rather than
	// create_time DESC. w.Stats is always non-nil at this call site (initialised
	// at the top of OpenWiring before either the degraded or the full branch).
	// StatsRanker falls back to recency automatically when the store has no
	// usage data yet (cold start), so the section never goes empty.
	ranker := contextbrief.NewStatsRanker(w.Stats, nav, stats.Last7Days)
	// Scope resolver (#206): each getContext build derives a scope-bounded reader
	// + ranker from the per-request ScopeSet (Request.Scopes, default
	// {project,personal}). nav.WithScopes REPLACES the span, so an explicit
	// {team} request widens past the default. Without this the brief's
	// document/entity sections would read the base navigator's AllScopes span and
	// leak cross-scope rows; the recall lane is bounded separately via
	// RecallRequest.Scopes.
	assembler := contextbrief.New(nav, recall).
		WithEntityRanker(ranker).
		WithPresetReader(presetBriefReader{nav: nav}).
		WithLogger(logger).
		WithScopeResolver(func(scopes kernel.ScopeSet) (contextbrief.Reader, contextbrief.EntityRanker) {
			sn := nav.WithScopes(scopes)
			return sn, contextbrief.NewStatsRanker(w.Stats, sn, stats.Last7Days)
		})
	gc := tools.NewGetContext(assembler)
	if hybrid != nil {
		// The query lane runs the same HybridQuery as recall, so drain async
		// extraction + resolution before a queried brief — exactly as recall
		// does — or a brief built right after a write burst would miss hits.
		gc = gc.WithBackgroundWaiter(orch.Log)
	}
	// getIndex assembles the master vault directory listing (index.md). It
	// shares the same Assembler as getContext; both are unlisted from
	// tools/list — index is driven by `maz context index`, not agent navigation.
	gi := tools.NewGetIndex(assembler)
	// getEntityContext (#1): a single-entity dossier sharing the same Assembler as
	// getContext (only the seed differs — one entity UID rather than the session).
	// Its Mentioned-in lane runs the same HybridQuery as recall, so drain async
	// extraction + resolution first, exactly as getContext/recall do.
	gec := tools.NewGetEntityContext(assembler, nav)
	if hybrid != nil {
		gec = gec.WithBackgroundWaiter(orch.Log)
	}
	// Wire the conflict checker so getEntityContext annotates cards for entities
	// involved in open supersede conflicts. The syncConflictDetector satisfies
	// the tools.entityConflictChecker interface via structural typing; nil when
	// storage is not the production dispatcher (degraded mode) — silently skipped.
	if det != nil {
		gec = gec.WithConflictChecker(det)
	}
	// getContext + getIndex + getEntityContext, plus two brief-domain feature sets
	// registered here (not registerNavigationTools) so they survive degraded mode —
	// none needs an embedder, and each read tool narrows nav to the request scope:
	//   Insight Inbox (#11): list hot insights + user-dismiss. dismissInsight
	//   appends an insightDismissed event through orch.Log (single-writer lock);
	//   the projection sets the terminal 'dismissed' state (never re-promoted).
	//   Context pin/correct (#15): the cold-start brief feedback loop — steering
	//   pins, corrections, and convergent unpin tombstones append through orch.Log;
	//   contextPins lists the live pins the Assembler merges into the brief.
	w.Tools = append(w.Tools, gc, gi, gec,
		tools.NewListInsights(nav),
		tools.NewDismissInsight(orch.Log, nav),
		tools.NewContextPin(orch.Log),
		tools.NewContextCorrect(orch.Log, nav),
		tools.NewContextUnpin(orch.Log, nav),
		tools.NewContextPinList(nav),
	)

	// Store adapters for MCP resource registration (hot/index/pinned + documents
	// template). wireAndBuildServer calls RegisterContextResources on the server
	// after it is constructed, using these values. Nil-safe: if either is nil
	// RegisterContextResources silently skips.
	w.ResourceAssembler = assemblerAdapter{a: assembler}
	w.ResourceDocReader = navigatorAdapter{n: nav}

	// Channel A (#214): per-session MCP `initialize` instructions composed from
	// the same assembler. wireAndBuildServer threads this into mcp.Options so a
	// new connection re-reads the active-preset section live.
	w.InstructionComposer = newInstructionComposer(assembler, reader)
}

// registerRecallWithoutLiveEmbedder wires the recall surface when no live
// embedder is available, then starts the vault watcher. For embedderDegraded it
// builds an FTS(+graph)-only HybridQuery (nil embedder → the dense-vector and
// name-vector lanes are statically disabled in the kernel) and registers a
// working `recall` that advertises degraded:true; for embedderOptOut it
// registers the disabled stub (#105). The memory layer stays off in both cases
// (extraction needs embeddings); OpenWiring early-returns after this. Extracted
// to keep OpenWiring's cognitive complexity under the linter threshold.
func registerRecallWithoutLiveEmbedder(ctx context.Context, w *Wired, orch *orchestrator.Orchestrator, mode embedderMode, vaultPath string, user *config.UserConfig, logger *zap.Logger) error {
	if mode == embedderDegraded {
		logger.Warn("daemon: embedder unavailable; recall degraded to FTS-only (BM25)")
		hybrid, err := buildHybridQuery(ctx, orch)
		if err != nil {
			return err
		}
		applyRecallTuning(hybrid, user.Recall, logger)
		hybrid.SetStats(w.Stats)
		w.Query = hybrid
		w.Tools = append(w.Tools, tools.NewRecall(hybrid).
			WithBackgroundWaiter(orch.Log).
			WithEventLookup(orch.Log).
			WithFreshnessPolicy(config.ResolveFreshnessPolicy(user.Connectors)))
		// getContext's query-relevant lane runs through the degraded hybrid; its
		// pinned/recent/entity sections never needed embeddings.
		registerContextTools(w, orch, hybrid, logger, w.ConflictDetector)
	} else { // embedderOptOut
		// Info, not Warn: disabling embeddings (defaults.embed=none) is a
		// deliberate operator choice, not an anomaly. Warn here was stderr noise
		// on every in-process CLI command run without a daemon. Mirrors the Info
		// at the embedderOptOut sentinel below.
		logger.Info("daemon: recall registered as disabled variant (embeddings disabled by config)")
		w.Tools = append(w.Tools, tools.NewRecallDisabled())
		// A nil hybrid disables only getContext's query-relevant section; the
		// pinned/recent/entity sections still render from the navigator.
		registerContextTools(w, orch, nil, logger, w.ConflictDetector)
	}
	// Start the vault watcher either way: wikilinks and frontmatter links are
	// resolved deterministically; prose extraction is skipped (w.ProseExtractor
	// is nil without the memory layer).
	if user.Vault.Watch {
		if werr := startVaultWatcher(ctx, w, orch, vaultPath, logger); werr != nil {
			return werr
		}
	}
	return nil
}

// buildHybridQuery pulls the dispatcher's SQLite reader, vector layer,
// and FTS layer out of the orchestrator's Storage and feeds them into
// kernel.NewHybridQuery alongside the embedder. The type assertion on
// orch.Storage matches the orchestrator's contract that Storage is
// always a *dispatcher.Dispatcher in production; a future refactor that
// promotes SQLiteReader to the kernel.Storage interface would let this
// helper drop the assertion.
func buildHybridQuery(ctx context.Context, orch *orchestrator.Orchestrator) (*kernel.HybridQuery, error) {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		return nil, fmt.Errorf("daemon: orchestrator storage is %T, want *dispatcher.Dispatcher", orch.Storage)
	}
	reader, err := disp.SQLiteReader()
	if err != nil {
		return nil, fmt.Errorf("daemon: dispatcher sqlite reader: %w", err)
	}
	vec, err := disp.OpenVector(ctx)
	if err != nil {
		return nil, fmt.Errorf("daemon: dispatcher vector: %w", err)
	}
	fts, err := disp.OpenFTS(ctx)
	if err != nil {
		return nil, fmt.Errorf("daemon: dispatcher fts: %w", err)
	}
	graph, err := disp.OpenGraph(ctx)
	if err != nil {
		return nil, fmt.Errorf("daemon: dispatcher graph: %w", err)
	}
	hybrid, err := kernel.NewHybridQuery(orch.Embedder, vec, fts, reader, kernel.WithGraphConn(graph))
	if err != nil {
		return nil, fmt.Errorf("daemon: build hybrid query: %w", err)
	}
	// Wire the anchor navigator (Feature 11). The kernel's anchorNavigator
	// interface requires NeighborUIDs; navigate.Navigator exposes Neighbors
	// (returns typed Neighbor slices). navigatorBridge adapts the two.
	// AllScopes: anchor navigator for hybrid query BFS expansion; spans all user scopes for best anchor coverage.
	nav, nerr := navigate.NewAllScopes(reader)
	if nerr != nil {
		// Non-fatal: anchor-scoped recall gracefully degrades to full recall
		// when no navigator is wired, so a nav construction error must not
		// prevent the daemon from serving other tools.
		_ = nerr // logged by the caller via wiring diagnostics
	} else {
		hybrid.SetNavigator(&navigatorBridge{nav: nav})
	}
	return hybrid, nil
}

// navigatorBridge adapts *navigate.Navigator to the kernel's unexported
// anchorNavigator interface by converting typed Neighbor slices from
// navigate.Navigator.Neighbors into plain UID strings, keeping the kernel
// free from importing the navigate package and avoiding an import cycle.
type navigatorBridge struct{ nav *navigate.Navigator }

// NeighborUIDs satisfies kernel.anchorNavigator.
func (b *navigatorBridge) NeighborUIDs(ctx context.Context, seed string, depth int) ([]string, error) {
	neighbors, err := b.nav.Neighbors(ctx, seed, depth, navigate.RelationFilter{})
	if err != nil {
		return nil, err
	}
	uids := make([]string, len(neighbors))
	for i := range neighbors {
		uids[i] = neighbors[i].Entity.UID
	}
	return uids, nil
}

// loadUserConfigOrDefault returns the persisted UserConfig when present
// or a synthesized default that points at the M1 Ollama embedder. The
// fallback lets the daemon boot on a fresh data directory without
// requiring the user to have run `maz init` first; the Ollama probe in
// buildEmbedder still surfaces a clear error if the daemon is not
// installed locally.
func loadUserConfigOrDefault(dataDir string, logger *zap.Logger) (config.UserConfig, error) {
	user, err := config.LoadUserConfig(dataDir)
	switch {
	case errors.Is(err, config.ErrUserConfigNotFound):
		// Debug, not Warn: the in-process CLI path re-runs wiring on every
		// invocation, so a Warn here spammed stderr on each command for a
		// config-less store. `maz doctor` is the surface that reports missing
		// config; the built-in defaults are a deliberate, valid fallback.
		logger.Debug("daemon: user config absent, using built-in M1 defaults",
			zap.String("dataDir", dataDir),
		)
		return defaultUserConfig(), nil
	case err != nil:
		return config.UserConfig{}, fmt.Errorf("daemon: load user config: %w", err)
	}
	// Empty Defaults.Embed is the explicit "no embed provider" sentinel
	// the wizard writes for the skip-embeddings branch (#102). We do not
	// silently rewrite it to "ollama" — the user explicitly opted out
	// and the daemon must respect that. buildEmbedder returns nil when
	// the slot is empty; OpenWiring branches on that to skip embed-only
	// surfaces.

	// Overlay the machine-managed state.toml on top of the user-curated
	// config.toml so operator-level overrides (auto_push flip, per-scope
	// URL change, model selection) take effect at boot without forcing
	// the user to hand-edit config.toml. state.toml is written by `maz
	// config set` and the daemon's own background writers; config.toml
	// is the surface humans edit by hand or via `maz config edit`. Absent state is treated as the
	// no-op merge; any read error other than not-found is fatal because
	// it could silently invert intended behaviour.
	state, serr := config.LoadUserState(dataDir)
	if serr != nil {
		if !errors.Is(serr, config.ErrUserStateNotFound) {
			return config.UserConfig{}, fmt.Errorf("daemon: load user state: %w", serr)
		}
		return user, nil
	}
	return config.Merge(user, state), nil
}

// defaultUserConfig returns the synthesised UserConfig the daemon falls
// back to when no config.toml exists yet. The shape matches what `maz
// init` writes for a fresh Ollama install so a later `maz init` run
// produces a no-op rather than rewriting the daemon's view of the
// world.
func defaultUserConfig() config.UserConfig {
	return config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Providers: map[string]config.ProviderTable{
			embedProviderOllama: {},
		},
		Defaults: config.DefaultsTable{Embed: embedProviderOllama},
	}
}

// buildEmbedder resolves the active embed provider and constructs its
// client. The config contract is that Defaults.Embed names a key in the
// Providers map; the value at that key carries the per-provider
// settings (BaseURL, EmbedModel, ...). Two opt-out sentinels both
// return (nil, nil): an empty Defaults.Embed (the value `maz init`
// writes for the skip-embeddings branch in #102) and the explicit
// "none" key (#105). Callers must handle the nil-embedder case —
// recall registers its disabled variant; the kernel HybridQuery is
// skipped entirely.
//
// M1 only understands "ollama" and "none". A future PR adds a `type`
// discriminator to ProviderTable so a user can rename their table (e.g.
// `defaults.embed = "ollama-local"` with `[providers.ollama-local]`)
// without forking the resolver; until that lands, any other key is
// rejected here with a clear error so a silent misconfiguration cannot
// poison the vector index with the wrong embedding dimension.
//
// Returns the kernel.Embedder interface so the (nil, nil) opt-out path
// has a concrete typed value to flow into *Wired.Embedder. The interface
// return is required for the disabled state and intentional; the
// (nil, nil) return is a first-class result documented in the godoc
// above ("returns (nil, nil)") and is how callers distinguish the
// opt-out path from a build failure.
func buildEmbedder(ctx context.Context, user config.UserConfig, logger *zap.Logger) (embedder.Embedder, error) { //nolint:gocritic // called once at daemon startup; copy cost negligible vs. pointer-aliasing risk
	key := user.Defaults.Embed
	if key == "" {
		return nil, nil //nolint:nilnil // empty Defaults.Embed is the documented "embeddings disabled" sentinel
	}
	if key == embedProviderNone {
		logger.Info("daemon: embeddings disabled by config (defaults.embed=none); recall will return E_VALIDATION")
		return nil, nil //nolint:nilnil // documented sentinel for the disabled state; callers branch on Embedder==nil.
	}
	if key != embedProviderOllama {
		return nil, fmt.Errorf(
			"%w: embed provider %q "+
				"(M1 understands %q and %q; rename your [providers.%s] table "+
				"to [providers.%s], set defaults.embed = %q, or pick %q to disable embeddings in config.toml)",
			errUnsupportedEmbedProvider, key, embedProviderOllama, embedProviderNone,
			key, embedProviderOllama, embedProviderOllama, embedProviderNone,
		)
	}

	prov := user.Providers[key]
	opts := ollama.Options{
		BaseURL:    prov.BaseURL,
		EmbedModel: prov.EmbedModel,
	}
	logger.Info("daemon: building ollama embedder",
		zap.String("providerKey", key),
		zap.String("baseURL", opts.BaseURL),
		zap.String("embedModel", opts.EmbedModel),
	)
	client, err := ollama.New(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("daemon: build ollama client: %w", err)
	}
	return client, nil
}

// startVaultWatcher wires the P7 bidirectional file-watch pair: an fsnotify
// watcher that ingests edited vault files, and a writeback worker that
// re-materializes files when a revision is appended via a non-watcher path
// (MCP tools, sync). Both goroutines are started and their Close hooks are
// registered on w.closers so the daemon shuts them down cleanly.
//
// Only called when user.Vault.Watch is true (opt-in OFF by default). If
// storage is not the production dispatcher (test fake) the watcher is skipped,
// matching registerNavigationTools' degradation guard.
func startVaultWatcher(ctx context.Context, w *Wired, orch *orchestrator.Orchestrator, vaultPath string, logger *zap.Logger) error {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		logger.Warn("daemon: vault watcher skipped (storage is not the production dispatcher)",
			zap.String("storage_type", fmt.Sprintf("%T", orch.Storage)))
		return nil
	}
	reader, err := disp.SQLiteReader()
	if err != nil {
		return fmt.Errorf("daemon: vault watcher sqlite reader: %w", err)
	}
	// VaultScopes (all four incl. draft): the vault watcher's importer and
	// projector must dedup + materialize every scope, including draft, to the
	// partitioned vault (<root>/<scope>/). This navigator is confined to the
	// vault surface and never feeds recall/navigation.
	nav, err := navigate.NewVaultScopes(reader)
	if err != nil {
		return fmt.Errorf("daemon: vault watcher navigator: %w", err)
	}

	// Ensure the vault directory exists before we try to watch it.
	if mkErr := os.MkdirAll(vaultPath, 0o700); mkErr != nil {
		return fmt.Errorf("daemon: vault watcher mkdir %q: %w", vaultPath, mkErr)
	}

	// Reuse the SINGLE importer that registerNavigationTools built and stored on
	// w.Importer rather than minting a second one. Both the MCP document tools
	// and this watcher must share one *document.Importer so its per-document
	// prose-extraction lock actually serializes across the two paths: an MCP
	// edit and an fsnotify import of the same document now contend on the same
	// proseLocks map, so they cannot both read the pre-append edge set and both
	// persist a duplicate "mentions" edge. A separate importer here would have
	// its own lock map and reopen that race. w.Importer is nil only on the
	// non-production path (registerNavigationTools skipped); fall back to a
	// fresh importer there, where there is no second writer to race with.
	importer := w.Importer
	if importer == nil {
		importer = document.NewImporter(orch.Log, nav, logger)
	}
	// Wire prose extraction when a ChatProvider was configured (w.ProseExtractor
	// is non-nil). Nil is the documented "extraction disabled" sentinel.
	// WithProseExtractor mutates in place and is idempotent, so re-applying it
	// to the shared importer (already late-bound by wireMemoryLayer) is a no-op.
	if w.ProseExtractor != nil {
		importer = importer.WithProseExtractor(w.ProseExtractor)
	}
	// Reuse the projector built by registerNavigationTools (stored on w.Projector)
	// rather than minting a second one. Two independent Projectors over the same
	// vault path each have their own sync.Mutex and their own in-memory snapshot
	// of the .mazarbul/state.json sidecar; concurrent SyncPath calls from both
	// would race on the sidecar via os.Rename (last-write-wins clobber), silently
	// producing I2 misclassification (a later revision looks like a "pending
	// edit"). One projector, one mutex, one sidecar owner: I3 genuinely holds.
	if w.Projector == nil {
		// registerNavigationTools skipped (not a production dispatcher); build a
		// standalone projector so the watcher still operates correctly.
		w.Projector = vault.NewProjector(nav, vaultPath, logger)
	}
	writeback := vault.NewWriteback(w.Projector, logger)

	watcherCtx, cancelWatcher := context.WithCancel(ctx)
	watcher, err := vault.NewWatcher(vault.WatcherConfig{
		Root:     vaultPath,
		Ingester: importer,
		Notifier: writeback,
		Log:      logger,
	})
	if err != nil {
		cancelWatcher()
		return fmt.Errorf("daemon: vault watcher init: %w", err)
	}

	writebackCtx, cancelWriteback := context.WithCancel(ctx)

	// Start the writeback worker first so it is ready to receive notifications
	// from the watcher's first reconcile pass.
	go writeback.Run(writebackCtx)
	go func() {
		if runErr := watcher.Run(watcherCtx); runErr != nil {
			logger.Warn("Vault watcher exited with error", zap.Error(runErr))
		}
	}()

	logger.Info("Vault file-watcher started",
		zap.String("vaultPath", vaultPath))

	// w.closers run in REVERSE registration order (LIFO). Register the writeback
	// closer first and the watcher closer last so the watcher is torn down first
	// on shutdown: no new reconcile → Notify calls can be fired once the
	// writeback is stopping. (Writeback.Notify is already shutdown-safe — it
	// becomes a no-op after Stop and never sends on a closed channel — but
	// ordering the teardown this way avoids dropping a final in-flight update.)
	stopWatcher := func() error { cancelWatcher(); return nil }
	stopWriteback := func() error { cancelWriteback(); writeback.Stop(); return nil }
	w.closers = append(w.closers, stopWriteback, stopWatcher)
	return nil
}
