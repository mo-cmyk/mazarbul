package doctor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
)

// runCheck executes a single Check under a fresh context and a generous
// timeout, returning the raw status/detail/err. Tests use this to call
// the package-private check constructors without going through Runner.
func runCheck(t *testing.T, c Check) (Status, string, error) {
	t.Helper()
	// 30s, not 3s: the sqlite-open check opens a fresh DB and applies the FULL
	// (and growing) migration chain, which under parallel CGO-lock contention
	// (e.g. the Stop gate running many heavy packages at once) legitimately
	// exceeds a few seconds. A genuinely hung check is caught by the runner's own
	// per-check timeout test (TestRunner_PerCheckTimeout, 20ms), not this helper.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.Run(ctx)
}

// TestDataDirCheck_OK verifies the happy path: tmp dir is writable
// with mode 0700.
func TestDataDirCheck_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	c := newDataDirCheck(dir)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if !strings.Contains(detail, dir) {
		t.Errorf("detail %q missing data dir %q", detail, dir)
	}
	// Probe file must be cleaned up.
	if _, err := os.Stat(filepath.Join(dir, dataDirProbeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("probe file leaked: %v", err)
	}
}

// TestDataDirCheck_Empty: empty data dir is FAIL.
func TestDataDirCheck_Empty(t *testing.T) {
	t.Parallel()
	st, _, _ := runCheck(t, newDataDirCheck(""))
	if st != StatusFail {
		t.Errorf("status = %q, want %q", st, StatusFail)
	}
}

// TestDataDirCheck_WrongMode: dir with permissive mode warns rather
// than fails (the data is still reachable; mode is an audit issue).
func TestDataDirCheck_WrongMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	st, detail, _ := runCheck(t, newDataDirCheck(dir))
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "0755") {
		t.Errorf("detail %q missing mode hint", detail)
	}
}

// newSharedForTest constructs a sharedSQLite pointed at a fresh
// per-test data directory. The three sqlite checks consume *sharedSQLite
// so the test path mirrors production.
func newSharedForTest(t *testing.T) *sharedSQLite {
	t.Helper()
	return &sharedSQLite{path: sqliteIndexPath(t.TempDir())}
}

// TestSQLiteOpenCheck_OK opens a fresh SQLite db under a tmp dir and
// expects OK with the index.db path in detail. The SQLite layer's
// migration runner doubles as a smoke test here.
func TestSQLiteOpenCheck_OK(t *testing.T) {
	t.Parallel()
	s := newSharedForTest(t)
	st, detail, err := runCheck(t, newSQLiteOpenCheck(s))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if !strings.Contains(detail, "index.db") {
		t.Errorf("detail %q missing index.db", detail)
	}
}

// TestSQLiteVecCheck_OK exercises the vec_version() probe.
func TestSQLiteVecCheck_OK(t *testing.T) {
	t.Parallel()
	s := newSharedForTest(t)
	st, detail, err := runCheck(t, newSQLiteVecCheck(s))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q", st, StatusOK)
	}
	if !strings.HasPrefix(detail, "v") {
		t.Errorf("detail %q expected version prefix", detail)
	}
}

// TestSQLiteFTSCheck_OK verifies the FTS5 virtual table is present
// after the migration runner finishes.
func TestSQLiteFTSCheck_OK(t *testing.T) {
	t.Parallel()
	s := newSharedForTest(t)
	st, detail, err := runCheck(t, newSQLiteFTSCheck(s))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
}

// TestSQLiteShared_SingleOpen confirms sharedSQLite opens at most once
// under concurrent acquisitions and the three checks see the same
// store. Catches a regression of the WAL-pragma race that motivated
// the shared design.
func TestSharedSQLite_SingleOpen(t *testing.T) {
	t.Parallel()
	s := newSharedForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const n = 5
	stores := make([]any, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			stores[i], errs[i] = s.acquire(ctx)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("acquire[%d]: %v", i, err)
		}
	}
	first := stores[0]
	for i, st := range stores[1:] {
		if st != first {
			t.Errorf("acquire[%d] returned a different store than acquire[0]", i+1)
		}
	}
}

// TestLadybugCheck_OK opens a fresh LadybugDB and runs RETURN 1.
func TestLadybugCheck_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, detail, err := runCheck(t, newLadybugCheck(dir))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
}

// TestMCPUnixCheck_Absent: no socket file -> WARN.
func TestMCPUnixCheck_Absent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, detail, _ := runCheck(t, newMCPUnixCheck(dir))
	if st != StatusWarn {
		t.Errorf("status = %q, want %q", st, StatusWarn)
	}
	if !strings.Contains(detail, "not present") {
		t.Errorf("detail %q missing absence hint", detail)
	}
}

// TestMCPUnixCheck_Reachable spins up an in-process unix listener at
// the expected socket path and asserts the check reports OK.
func TestMCPUnixCheck_Reachable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socketPath := mcp.DefaultUnixSocket(dir)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	go acceptOne(l)

	st, detail, err := runCheck(t, newMCPUnixCheck(dir))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
}

// acceptOne accepts a single connection and closes it. Used by the
// unix-reachable test so the check's DialContext succeeds without
// hanging the listener goroutine.
func acceptOne(l net.Listener) {
	conn, err := l.Accept()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// TestMCPHealthzCheck_OK: spin up an httptest server returning the
// canonical HealthResponse and assert OK.
// startUnixHealthzServer starts an HTTP server on the Unix socket of a
// fresh data dir and returns that data dir, so newMCPHealthzCheck(dataDir)
// — which dials mcp.DefaultUnixSocket(dataDir) — reaches it. The healthz
// check moved to the socket when the TCP transport became opt-in.
func startUnixHealthzServer(t *testing.T, h http.Handler) string {
	t.Helper()
	// A short temp dir (not t.TempDir(), whose path embeds the long test
	// name) keeps the socket path under the macOS sun_path 104-byte cap.
	dataDir, err := os.MkdirTemp("", "mazhz")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", mcp.DefaultUnixSocket(dataDir))
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return dataDir
}

func TestMCPHealthzCheck_OK(t *testing.T) {
	t.Parallel()
	dataDir := startUnixHealthzServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != mcp.HealthPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(mcp.HealthResponse{Status: "ok", StartTime: time.Now().UTC()})
	}))
	st, detail, err := runCheck(t, newMCPHealthzCheck(dataDir))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
}

// TestMCPHealthzCheck_WrongStatus: a 200 with status="dead" is FAIL.
func TestMCPHealthzCheck_WrongStatus(t *testing.T) {
	t.Parallel()
	dataDir := startUnixHealthzServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "dead"})
	}))
	st, detail, _ := runCheck(t, newMCPHealthzCheck(dataDir))
	if st != StatusFail {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusFail, detail)
	}
}

// TestMCPHealthzCheck_Unreachable: a data dir whose socket is absent
// (no daemon) yields WARN (daemon may simply be off).
func TestMCPHealthzCheck_Unreachable(t *testing.T) {
	t.Parallel()
	st, detail, _ := runCheck(t, newMCPHealthzCheck(t.TempDir()))
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
}

// TestOllamaDaemonCheck_Reachable spins up a fake /api/tags server and
// asserts the daemon check reports OK with the model count.
func TestOllamaDaemonCheck_Reachable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		// Probe (inside ollama.New) needs DefaultEmbedModel to exist or it
		// short-circuits to the ErrModelMissing branch and never reaches
		// ListModels. Advertise the default plus a chat model to land in
		// the "2 models installed" path under test.
		fmt.Fprintf(w, `{"models":[{"name":%q},{"name":"llama3:8b"}]}`+"\n", ollama.DefaultEmbedModel)
	}))
	defer srv.Close()
	c := newOllamaDaemonCheck(&config.ProviderTable{BaseURL: srv.URL})
	st, detail, _ := runCheck(t, c)
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if !strings.Contains(detail, "2 models") {
		t.Errorf("detail %q missing model count", detail)
	}
}

// TestOllamaDaemonCheck_Unreachable: an unreachable daemon yields
// WARN (not FAIL) because doctor cannot tell "down for maintenance"
// from "never installed" without poking the user.
func TestOllamaDaemonCheck_Unreachable(t *testing.T) {
	t.Parallel()
	c := newOllamaDaemonCheck(&config.ProviderTable{BaseURL: "http://127.0.0.1:1"})
	st, _, _ := runCheck(t, c)
	if st != StatusWarn {
		t.Errorf("status = %q, want %q", st, StatusWarn)
	}
}

// TestOllamaEmbedModelCheck_Missing: a daemon that does not advertise the
// configured embed model yields WARN (not FAIL) — under #236 recall degrades
// to FTS-only rather than breaking the daemon — and the detail carries both
// the degraded-state note and the exact `ollama pull` recovery command.
func TestOllamaEmbedModelCheck_Missing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"models":[{"name":"llama3:8b"}]}`)
	}))
	defer srv.Close()
	c := newOllamaEmbedModelCheck(&config.ProviderTable{BaseURL: srv.URL, EmbedModel: "bge-m3"})
	st, detail, _ := runCheck(t, c)
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "ollama pull bge-m3") {
		t.Errorf("detail %q missing pull hint", detail)
	}
	if !strings.Contains(detail, "FTS-only") {
		t.Errorf("detail %q should name the degraded FTS-only state", detail)
	}
}

// TestOllamaEmbedModelCheck_Present: an installed embed model yields OK.
func TestOllamaEmbedModelCheck_Present(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"models":[{"name":"bge-m3:latest"}]}`)
	}))
	defer srv.Close()
	c := newOllamaEmbedModelCheck(&config.ProviderTable{BaseURL: srv.URL, EmbedModel: "bge-m3"})
	st, detail, _ := runCheck(t, c)
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
}

// TestEmbedderModeCheck_Full: a reachable daemon advertising the configured
// embed model yields OK with the full-hybrid summary.
func TestEmbedderModeCheck_Full(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"models":[{"name":"bge-m3:latest"}]}`)
	}))
	defer srv.Close()
	c := newEmbedderModeCheck(&config.ProviderTable{BaseURL: srv.URL, EmbedModel: "bge-m3"})
	st, detail, _ := runCheck(t, c)
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if !strings.Contains(detail, "full hybrid") {
		t.Errorf("detail %q should name the full-hybrid mode", detail)
	}
}

// TestEmbedderModeCheck_Degraded: an unreachable embedder yields WARN with the
// degraded FTS-only summary — recall still works, so it never FAILs.
func TestEmbedderModeCheck_Degraded(t *testing.T) {
	t.Parallel()
	c := newEmbedderModeCheck(&config.ProviderTable{BaseURL: "http://127.0.0.1:1", EmbedModel: "bge-m3"})
	st, detail, _ := runCheck(t, c)
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "FTS-only") {
		t.Errorf("detail %q should name the degraded FTS-only state", detail)
	}
}

// TestEmbedderModeCheck_MalformedConfigFails: a malformed BaseURL is fatal in
// the daemon (classifyEmbedderErr treats ErrInvalidConfig as boot-refusing), so
// the summary row must FAIL rather than masquerade as graceful FTS degradation.
func TestEmbedderModeCheck_MalformedConfigFails(t *testing.T) {
	t.Parallel()
	c := newEmbedderModeCheck(&config.ProviderTable{BaseURL: "not-a-url", EmbedModel: "bge-m3"})
	st, _, err := runCheck(t, c)
	if st != StatusFail {
		t.Errorf("status = %q, want %q for a malformed BaseURL", st, StatusFail)
	}
	if err == nil {
		t.Error("want a non-nil error for the fatal config class")
	}
}

// TestOllamaPingCheck_Unreachable: an unreachable chat backend yields WARN
// (not FAIL) — under #236 the chat provider degrades to extraction-disabled, so
// the daemon still boots and recall + remember keep working.
func TestOllamaPingCheck_Unreachable(t *testing.T) {
	t.Parallel()
	c := newOllamaPingCheck(&config.ProviderTable{BaseURL: "http://127.0.0.1:1", ChatModel: "llama3.2:latest"})
	st, detail, _ := runCheck(t, c)
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "recall + remember still work") {
		t.Errorf("detail %q should reassure that recall + remember still work", detail)
	}
}

// TestOllamaPingCheck_ModelMissing: a reachable daemon missing the configured
// chat model yields WARN — extraction is paused but recall still works — with
// the exact `ollama pull` recovery command.
func TestOllamaPingCheck_ModelMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"models":[{"name":"some-other-model:latest"}]}`)
	}))
	defer srv.Close()
	c := newOllamaPingCheck(&config.ProviderTable{BaseURL: srv.URL, ChatModel: "llama3.2:latest"})
	st, detail, _ := runCheck(t, c)
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "ollama pull llama3.2:latest") {
		t.Errorf("detail %q missing pull hint", detail)
	}
}

// TestLoggingCheck_OK verifies the daemon log file can be created
// under a temp MAZ_LOG_FILE override.
func TestLoggingCheck_OK(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")
	t.Setenv("MAZ_LOG_FILE", logPath)
	st, detail, err := runCheck(t, newLoggingCheck())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if detail != logPath {
		t.Errorf("detail = %q, want %q", detail, logPath)
	}
}

// TestDefaultChecks_NoConfig: in a fresh data dir (no config.toml) the
// catalog includes a config.load WARN row and skips provider checks.
func TestDefaultChecks_NoConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	checks := DefaultChecks(dir)
	if len(checks) == 0 {
		t.Fatalf("DefaultChecks empty")
	}
	hasConfigLoad := false
	hasDupEntities := false
	hasDupEdges := false
	for _, c := range checks {
		switch c.Name() {
		case "config.load":
			hasConfigLoad = true
		case "dataquality.duplicate_entities":
			hasDupEntities = true
		case "dataquality.duplicate_active_edges":
			hasDupEdges = true
		case "provider.chat.ping":
			t.Errorf("chat.ping registered without a config")
		}
	}
	if !hasConfigLoad {
		t.Errorf("config.load missing in no-config catalog")
	}
	// The advisory data-quality checks read only the SQLite projection, so they
	// must run even without a config.toml — a freshly-imported store (no config
	// yet) is exactly where duplicates accumulate.
	if !hasDupEntities {
		t.Errorf("dataquality.duplicate_entities missing in no-config catalog")
	}
	if !hasDupEdges {
		t.Errorf("dataquality.duplicate_active_edges missing in no-config catalog")
	}
}

// TestDefaultChecks_AnthropicOllama: with Anthropic chat + Ollama embed
// the catalog includes anthropic.secret, chat.ping, ollama.daemon,
// ollama.embed-model.
func TestDefaultChecks_AnthropicOllama(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
			"ollama":    {EmbedModel: "bge-m3"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	for _, want := range []string{
		"provider.anthropic.secret",
		"provider.chat.ping",
		"provider.ollama.daemon",
		"provider.ollama.embed-model",
	} {
		if !contains(names, want) {
			t.Errorf("catalog missing %q; got %v", want, names)
		}
	}
}

// TestDefaultChecks_EmptyEmbedFallsBackToOllamaProbes pins the daemon
// parity: an empty `defaults.embed` is treated as the M1 Ollama default
// by the daemon resolver (internal/daemon/wiring.go:buildEmbedder), so
// doctor must register Ollama probes too. Without the fallback, a
// legacy / hand-written config would let doctor report "all good"
// while the daemon refuses to start. Greptile finding #5 on PR #117.
func TestDefaultChecks_EmptyEmbedFallsBackToOllamaProbes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: ""},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
			"ollama":    {EmbedModel: "bge-m3"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	if !contains(names, "provider.ollama.daemon") {
		t.Errorf("catalog missing provider.ollama.daemon for empty defaults.embed; got %v", names)
	}
	if contains(names, "embed.disabled") {
		t.Errorf("catalog contains embed.disabled for empty (not 'none') defaults.embed; got %v", names)
	}
}

// TestDefaultChecks_CaseSensitiveEmbedMatchesDaemon pins Greptile
// finding #4 on PR #117: doctor must use case-sensitive matching for
// defaults.embed to match the daemon resolver. A hand-edited
// `defaults.embed = "None"` is rejected at daemon startup; doctor
// must reflect that as a FAIL row naming the unsupported value
// rather than a clean board the user can't reconcile with the
// daemon crash. Greptile follow-up: pre-fix the disabled WARN
// dropped silently and no Ollama probes ran either, so doctor
// reported "all green" while the daemon refused to start.
func TestDefaultChecks_CaseSensitiveEmbedMatchesDaemon(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "None"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	if contains(names, "embed.disabled") {
		t.Errorf("catalog contains embed.disabled for mixed-case \"None\"; doctor must match daemon's case-sensitive resolver. names=%v", names)
	}
	if !contains(names, "embed.unsupported") {
		t.Errorf("catalog missing embed.unsupported FAIL for mixed-case \"None\"; daemon rejects this value at startup. names=%v", names)
	}
}

// TestEmbedUnsupportedCheck_ReportsFAIL pins the standalone check
// constructor: FAIL status, the offending value quoted in detail,
// and a recovery hint naming the accepted values. Mirror of the
// embed-disabled WARN test above.
func TestEmbedUnsupportedCheck_ReportsFAIL(t *testing.T) {
	t.Parallel()
	c := newEmbedUnsupportedCheck("None")
	if c.Name() != "embed.unsupported" {
		t.Errorf("Name() = %q, want embed.unsupported", c.Name())
	}
	if c.Subsystem() != subsystemProvider {
		t.Errorf("Subsystem() = %q, want %q", c.Subsystem(), subsystemProvider)
	}
	status, detail, err := c.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	if status != StatusFail {
		t.Errorf("status = %v, want StatusFail", status)
	}
	if !strings.Contains(detail, `"None"`) {
		t.Errorf("detail = %q, want the offending value quoted", detail)
	}
	if !strings.Contains(detail, "ollama") || !strings.Contains(detail, "none") {
		t.Errorf("detail = %q, want recovery hint naming accepted values", detail)
	}
}

// TestDefaultChecks_UnsupportedEmbedSurfacesFAIL exercises the
// integration path: a garbage `defaults.embed` value (typo or
// unsupported provider) registers the embed-unsupported FAIL and
// suppresses both the embed-disabled WARN and the Ollama probes.
func TestDefaultChecks_UnsupportedEmbedSurfacesFAIL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "voyage"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	if !contains(names, "embed.unsupported") {
		t.Errorf("catalog missing embed.unsupported FAIL; got %v", names)
	}
	if contains(names, "embed.disabled") {
		t.Errorf("catalog contains embed.disabled for unsupported value; got %v", names)
	}
	for _, banned := range []string{"provider.ollama.daemon", "provider.ollama.embed-model"} {
		if contains(names, banned) {
			t.Errorf("catalog should not contain %q when embed is unsupported; got %v", banned, names)
		}
	}
}

// TestDefaultChecks_AnthropicEmbedNone: with Anthropic chat and
// defaults.embed=none, the catalog includes the embed-disabled WARN row
// and skips every Ollama check (an Anthropic-only Skip install must not
// surface FAILs for a daemon the user never asked for).
func TestDefaultChecks_AnthropicEmbedNone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "none"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	if !contains(names, "embed.disabled") {
		t.Errorf("catalog missing embed.disabled WARN; got %v", names)
	}
	for _, banned := range []string{
		"provider.ollama.daemon",
		"provider.ollama.embed-model",
		"provider.chat.ping", // chat.ping is anthropic-or-ollama; assert ollama variant absent
	} {
		// chat.ping is also Anthropic's; the right banned set is just the
		// ollama-prefixed rows.
		if strings.HasPrefix(banned, "provider.ollama") && contains(names, banned) {
			t.Errorf("catalog should not contain %q when embed=none and chat=anthropic; got %v", banned, names)
		}
	}
}

// TestEmbedDisabledCheck_ReportsWARN asserts the new check renders as a
// WARN row with the user-actionable recovery hint.
func TestEmbedDisabledCheck_ReportsWARN(t *testing.T) {
	t.Parallel()
	c := newEmbedDisabledCheck()
	if c.Name() != "embed.disabled" {
		t.Errorf("Name() = %q, want embed.disabled", c.Name())
	}
	if c.Subsystem() != subsystemProvider {
		t.Errorf("Subsystem() = %q, want %q", c.Subsystem(), subsystemProvider)
	}
	status, detail, err := c.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want StatusWarn", status)
	}
	if !strings.Contains(detail, "embeddings disabled") {
		t.Errorf("detail = %q, want a phrase containing 'embeddings disabled'", detail)
	}
	if !strings.Contains(detail, "maz init") {
		t.Errorf("detail = %q, want a recovery hint mentioning `maz init`", detail)
	}
}

// TestDefaultChecks_OllamaOnly: Ollama for both chat and embed; no
// Anthropic-side checks are registered.
func TestDefaultChecks_OllamaOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "ollama", Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {ChatModel: "llama3:8b", EmbedModel: "bge-m3"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	for _, banned := range []string{"provider.anthropic.secret"} {
		if contains(names, banned) {
			t.Errorf("catalog should not contain %q for Ollama-only config; got %v", banned, names)
		}
	}
	for _, want := range []string{"provider.ollama.daemon", "provider.chat.ping"} {
		if !contains(names, want) {
			t.Errorf("catalog missing %q; got %v", want, names)
		}
	}
}

// TestOllamaPingCheck_EmptyChatModel: when defaults.chat=ollama is
// configured but providers.ollama.chat_model is empty, the chat ping
// row must still be registered and surface a FAIL with an actionable
// detail rather than silently disappear from the report.
func TestOllamaPingCheck_EmptyChatModel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "ollama", Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {EmbedModel: "bge-m3"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if !contains(checkNames(DefaultChecks(dir)), "provider.chat.ping") {
		t.Fatalf("provider.chat.ping should be registered even when chat_model is empty")
	}

	c := newOllamaPingCheck(&config.ProviderTable{})
	status, detail, err := c.Run(t.Context())
	if status != StatusFail {
		t.Errorf("status = %v, want %v", status, StatusFail)
	}
	if err == nil || !strings.Contains(err.Error(), "chat_model") {
		t.Errorf("err = %v, want error mentioning chat_model", err)
	}
	if detail != "" {
		t.Errorf("detail = %q, want empty (error carries the message)", detail)
	}
}

// checkNames extracts the Name() of each registered Check. Used by
// the catalog tests above.
func checkNames(checks []Check) []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name()
	}
	return out
}

// contains reports whether s is in haystack. Local copy to avoid
// importing slices for one test fixture.
func contains(haystack []string, s string) bool {
	for _, x := range haystack {
		if x == s {
			return true
		}
	}
	return false
}

// --- Event-log drift check helpers ---

func newDriftSharedForTest(t *testing.T, dataDir string) *sharedSQLite {
	t.Helper()
	return &sharedSQLite{path: sqliteIndexPath(dataDir)}
}

// writeJSONLEvents appends n lines of the given kind to a single JSONL
// file under <dataDir>/events/2026/01-01.jsonl. The exact path layout
// is recursive-walked, so any *.jsonl under events/ counts. The minimal
// JSON shape ({"uid":...,"kind":...}) is enough for the doctor's drift
// scan, which substring-matches on `"kind":"<value>"`.
func writeJSONLEvents(t *testing.T, dataDir, kind string, n int) {
	t.Helper()
	if n == 0 {
		return
	}
	dir := filepath.Join(dataDir, "events", "2026")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	path := filepath.Join(dir, "01-01.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()
	for i := range n {
		line := fmt.Sprintf(`{"uid":"%s%010d","kind":"%s","payload":{}}`+"\n",
			strings.ToUpper(kind)[:1], i, kind)
		if _, werr := f.WriteString(line); werr != nil {
			t.Fatalf("write jsonl line: %v", werr)
		}
	}
}

// writeBoltEntries plants n synthetic bolt keys in dataDir/events/index.bolt.
// Keys are deterministic 16-byte values derived from the index so the bucket
// sorts them in a stable order. The last key's hex representation is returned
// so callers can build matching SQLite rows when testing in-sync scenarios.
func writeBoltEntries(t *testing.T, dataDir string, n int) string {
	t.Helper()
	boltPath := boltIndexPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(boltPath), 0o750); err != nil {
		t.Fatalf("mkdir bolt parent: %v", err)
	}
	db, err := bolt.Open(boltPath, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	defer func() { _ = db.Close() }()

	var lastKeyHex string
	if err := db.Update(func(tx *bolt.Tx) error {
		b, berr := tx.CreateBucketIfNotExists([]byte("events"))
		if berr != nil {
			return berr
		}
		for i := range n {
			var key [16]byte
			// Spread keys across the 16-byte space so they sort predictably.
			key[15] = byte(i + 1)
			key[14] = byte((i + 1) >> 8)
			val := [12]byte{} // minimal valid value (year=0, offset=0)
			if err := b.Put(key[:], val[:]); err != nil {
				return err
			}
			lastKeyHex = hex.EncodeToString(key[:])
		}
		return nil
	}); err != nil {
		t.Fatalf("write bolt entries: %v", err)
	}
	return lastKeyHex
}

// writeEpisodeRows inserts n episode rows into the SQLite store opened via s.
// The create_time values are spaced 1 second apart so ORDER BY create_time
// returns a stable last row. Returns the uid of the last inserted row.
func writeEpisodeRows(t *testing.T, s *sharedSQLite, n int) string {
	t.Helper()
	ctx := t.Context()
	store, err := s.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire sqlite: %v", err)
	}
	var lastUID string
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		uid := fmt.Sprintf("EPISODE%019d", i+1) // fake but unique
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		_, err := store.Writer().ExecContext(ctx,
			"INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)",
			uid, ts, "episode", `{}`,
		)
		if err != nil {
			t.Fatalf("insert episode row %d: %v", i, err)
		}
		lastUID = uid
	}
	return lastUID
}

// --- Event-log drift check tests ---

// TestEventLogDriftCheck_EmptyStore: fresh tmpdir with no events dir and no
// sidecar → the check must report OK with "0 events" in the detail.
func TestEventLogDriftCheck_EmptyStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)
	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if !strings.Contains(detail, "0 events") {
		t.Errorf("detail %q should contain '0 events'", detail)
	}
}

// TestEventLogDriftCheck_SidecarAbsent: events dir exists but the sidecar
// file has been deleted → WARN with "index absent".
func TestEventLogDriftCheck_SidecarAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create the events directory but leave the sidecar absent.
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o750); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	// Insert one episode row so the check does not short-circuit to the
	// all-empty OK path.
	s := newDriftSharedForTest(t, dir)
	writeEpisodeRows(t, s, 1)

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "index absent") {
		t.Errorf("detail %q should contain 'index absent'", detail)
	}
}

// TestEventLogDriftCheck_InSync: 1 bolt entry keyed by a real ULID binary
// plus 1 matching episode row → OK with "in sync".
func TestEventLogDriftCheck_InSync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)

	// Use a well-known ULID string so we can plant the matching binary key
	// in bolt and the matching uid string in SQLite, exercising the full
	// delta==0 + lastUIDs-equal → StatusOK path.
	const episodeUID = "01JVBQAAAAAAAAAAAAAAAAAAAA"

	// Plant the binary ULID key in a fresh bolt sidecar.
	boltPath := boltIndexPath(dir)
	if err := os.MkdirAll(filepath.Dir(boltPath), 0o750); err != nil {
		t.Fatalf("mkdir bolt parent: %v", err)
	}
	db, oerr := bolt.Open(boltPath, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if oerr != nil {
		t.Fatalf("open bolt: %v", oerr)
	}
	if uerr := db.Update(func(tx *bolt.Tx) error {
		b, berr := tx.CreateBucketIfNotExists([]byte("events"))
		if berr != nil {
			return berr
		}
		id, perr := ulid.ParseStrict(episodeUID)
		if perr != nil {
			return fmt.Errorf("parse ulid: %w", perr)
		}
		return b.Put(id[:], make([]byte, 12))
	}); uerr != nil {
		_ = db.Close()
		t.Fatalf("update bolt: %v", uerr)
	}
	_ = db.Close()

	// Plant a JSONL episode event so the new kind-aware drift scan sees
	// one episode-kind event matching the one sqlite row below.
	writeJSONLEvents(t, dir, "episode", 1)

	// Plant the matching episode row in SQLite.
	ctx := t.Context()
	store, err := s.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire sqlite: %v", err)
	}
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, serr := store.Writer().ExecContext(ctx,
		"INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)",
		episodeUID, ts, "episode", `{}`,
	); serr != nil {
		t.Fatalf("insert episode: %v", serr)
	}

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if !strings.Contains(detail, "in sync") {
		t.Errorf("detail %q should contain 'in sync'", detail)
	}
}

// TestEventLogDriftCheck_MinorDrift: 3 episode events + 1 episode row →
// delta 2 → WARN with "run `maz rebuild`".
func TestEventLogDriftCheck_MinorDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)
	writeBoltEntries(t, dir, 3)
	writeJSONLEvents(t, dir, "episode", 3)
	writeEpisodeRows(t, s, 1)

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "maz rebuild") {
		t.Errorf("detail %q missing 'maz rebuild' hint", detail)
	}
}

// TestEventLogDriftCheck_MajorDrift: 20 bolt entries + 5 episodes → delta 15
// → FAIL with "derived store is stale".
func TestEventLogDriftCheck_MajorDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)
	writeBoltEntries(t, dir, 20)
	writeJSONLEvents(t, dir, "episode", 20)
	writeEpisodeRows(t, s, 5)

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusFail {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusFail, detail)
	}
	if !strings.Contains(detail, "maz rebuild") {
		t.Errorf("detail %q missing 'maz rebuild' hint", detail)
	}
	if !strings.Contains(detail, "stale") {
		t.Errorf("detail %q missing 'stale' keyword", detail)
	}
}

// TestEventLogDriftCheck_EpisodeExcess: 2 bolt entries + 5 episodes → delta
// negative → FAIL with "exceeds".
func TestEventLogDriftCheck_EpisodeExcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)
	writeBoltEntries(t, dir, 2)
	writeJSONLEvents(t, dir, "episode", 2)
	writeEpisodeRows(t, s, 5)

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusFail {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusFail, detail)
	}
	if !strings.Contains(detail, "exceeds") {
		t.Errorf("detail %q missing 'exceeds' keyword", detail)
	}
}

// TestEventLogDriftCheck_LowercaseUID verifies that an in-sync store where the
// writer stored a lowercase Crockford base32 UID (accepted by ulid.Parse) still
// reports OK. The ORDER BY UPPER(uid) query must pick the canonical-sort-maximum
// row even when SQLite byte collation would rank a lowercase UID differently
// from the uppercase canonical form that boltUIDHex produces.
func TestEventLogDriftCheck_LowercaseUID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)

	// The same ULID stored as lowercase in SQLite. ulid.Parse accepts it;
	// ulid.ULID.String() returns the uppercase canonical form.
	const upperUID = "01JVBQAAAAAAAAAAAAAAAAAAAA"
	const lowerUID = "01jvbqaaaaaaaaaaaaaaaaaaaa"

	// Plant the binary ULID key in bolt (using the canonical binary encoding).
	boltPath := boltIndexPath(dir)
	if err := os.MkdirAll(filepath.Dir(boltPath), 0o750); err != nil {
		t.Fatalf("mkdir bolt parent: %v", err)
	}
	db, oerr := bolt.Open(boltPath, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if oerr != nil {
		t.Fatalf("open bolt: %v", oerr)
	}
	if uerr := db.Update(func(tx *bolt.Tx) error {
		b, berr := tx.CreateBucketIfNotExists([]byte("events"))
		if berr != nil {
			return berr
		}
		id, perr := ulid.ParseStrict(upperUID)
		if perr != nil {
			return fmt.Errorf("parse ulid: %w", perr)
		}
		return b.Put(id[:], make([]byte, 12))
	}); uerr != nil {
		_ = db.Close()
		t.Fatalf("update bolt: %v", uerr)
	}
	_ = db.Close()

	// Plant a lowercase uid in SQLite (simulating a caller-supplied lowercase UID
	// that the writer accepted via ulid.Parse).
	ctx := t.Context()
	store, err := s.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire sqlite: %v", err)
	}
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, serr := store.Writer().ExecContext(ctx,
		"INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?,?,?,?)",
		lowerUID, ts, "episode", `{}`,
	); serr != nil {
		t.Fatalf("insert episode: %v", serr)
	}

	// One JSONL episode event so the kind-aware scan finds the match.
	writeJSONLEvents(t, dir, "episode", 1)

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q): lowercase UID must not cause false drift", st, StatusOK, detail)
	}
	if !strings.Contains(detail, "in sync") {
		t.Errorf("detail %q should contain 'in sync'", detail)
	}
}

// TestEventLogDriftCheck_IgnoresNonEpisodeKinds is the regression gate
// for the false-positive that prompted this fix: a healthy store with
// many non-episode events (entity, mention, relation, ...) used to
// report "derived store is stale -- run `maz rebuild`" because the
// check compared ALL bolt-indexed events to the episodes table.
//
// With the kind-aware scan, only episode-kind events count toward the
// invariant. 10 entity events + 1 episode event + 1 episode row is
// in sync; the heterogeneous events are inert to this check.
func TestEventLogDriftCheck_IgnoresNonEpisodeKinds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)

	// 11 bolt entries (10 entity + 1 episode) but only 1 episode row.
	writeBoltEntries(t, dir, 11)
	writeJSONLEvents(t, dir, "entity", 10)
	writeJSONLEvents(t, dir, "episode", 1)
	writeEpisodeRows(t, s, 1)

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q); entity events must not count as drift",
			st, StatusOK, detail)
	}
}

// TestEventLogDriftCheck_RejectsKindLiteralInPayload is the Greptile P2
// regression gate. The drift counter previously matched the substring
// `"kind":"episode"` anywhere on the JSONL line, so a non-episode event
// whose payload quoted that literal (e.g. a prompt.rendered record that
// embedded the schema, or any LLM tool-use trace containing JSON text)
// inflated the episode count and the check reported drift on an in-sync
// store. The counter must only consider the TOP-LEVEL `kind` field; an
// inner-payload occurrence must not contribute.
func TestEventLogDriftCheck_RejectsKindLiteralInPayload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newDriftSharedForTest(t, dir)

	// Three non-episode events whose payloads contain a NESTED JSON
	// object with kind:"episode" — e.g. a prompt.rendered event that
	// embeds an example record, or an entity record whose attributes
	// include a sample schema. The naive substring counter sees the
	// literal `"kind":"episode"` and counts each line as an episode,
	// reporting a false +3 drift against the empty episodes table.
	// The top-level kind decode must count zero.
	jsonlDir := filepath.Join(dir, "events", "2026")
	if err := os.MkdirAll(jsonlDir, 0o750); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	path := filepath.Join(jsonlDir, "01-01.jsonl")
	body := `{"uid":"A1","kind":"prompt.rendered","payload":{"sample":{"kind":"episode"}}}` + "\n" +
		`{"uid":"A2","kind":"entity","payload":{"example":{"kind":"episode"}}}` + "\n" +
		`{"uid":"A3","kind":"relation","payload":{"nested":{"kind":"episode"}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	writeBoltEntries(t, dir, 3)

	c := newEventLogDriftCheck(dir, s)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q); payload-embedded `kind:episode` must not count",
			st, StatusOK, detail)
	}
}

// TestDefaultChecks_IncludesDrift asserts that "storage.drift" is registered
// in the DefaultChecks catalog so it always appears in `maz doctor` output.
func TestDefaultChecks_IncludesDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	names := checkNames(DefaultChecks(dir))
	if !contains(names, "storage.drift") {
		t.Errorf("DefaultChecks catalog missing 'storage.drift'; got %v", names)
	}
}

// --- OllamaSupervisor check tests ---

// stubState returns a stateFunc that always returns s. Used by the four
// supervisor-check unit tests to cover each OllamaSupervisorState branch
// without spawning real subprocesses.
func stubState(s daemon.OllamaSupervisorState) func() daemon.OllamaSupervisorState {
	return func() daemon.OllamaSupervisorState { return s }
}

// TestOllamaSupervisorCheck_Healthy: stateFunc returns StateHealthy -> StatusOK.
func TestOllamaSupervisorCheck_Healthy(t *testing.T) {
	t.Parallel()
	c := newOllamaSupervisorCheck(t.TempDir(), stubState(daemon.StateHealthy))
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusOK, detail)
	}
	if !strings.Contains(detail, "healthy") {
		t.Errorf("detail %q missing 'healthy'", detail)
	}
}

// TestOllamaSupervisorCheck_RestartPending: stateFunc returns StateRestartPending -> StatusWarn.
func TestOllamaSupervisorCheck_RestartPending(t *testing.T) {
	t.Parallel()
	c := newOllamaSupervisorCheck(t.TempDir(), stubState(daemon.StateRestartPending))
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "restart pending") {
		t.Errorf("detail %q missing 'restart pending'", detail)
	}
}

// TestOllamaSupervisorCheck_Failing: stateFunc returns StateFailing -> StatusFail.
func TestOllamaSupervisorCheck_Failing(t *testing.T) {
	t.Parallel()
	c := newOllamaSupervisorCheck(t.TempDir(), stubState(daemon.StateFailing))
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusFail {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusFail, detail)
	}
	if !strings.Contains(detail, "failing") {
		t.Errorf("detail %q missing 'failing'", detail)
	}
}

// TestOllamaSupervisorCheck_NotSupervised: stateFunc returns StateNotSupervised -> StatusWarn.
func TestOllamaSupervisorCheck_NotSupervised(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newOllamaSupervisorCheck(dir, stubState(daemon.StateNotSupervised))
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusWarn {
		t.Errorf("status = %q, want %q (detail=%q)", st, StatusWarn, detail)
	}
	if !strings.Contains(detail, "not supervised") {
		t.Errorf("detail %q missing 'not supervised'", detail)
	}
}

// TestDefaultChecks_ManagedOllamaRegisteresSupervisorCheck: when
// providers.ollama.managed == true the catalog includes
// provider.ollama.supervisor.
func TestDefaultChecks_ManagedOllamaRegistersSupervisorCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "ollama", Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {ChatModel: "llama3:8b", EmbedModel: "bge-m3", Managed: true},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	if !contains(names, "provider.ollama.supervisor") {
		t.Errorf("catalog missing provider.ollama.supervisor for managed=true config; got %v", names)
	}
}

// TestDefaultChecks_UnmanagedOllamaNoSupervisorCheck: when
// providers.ollama.managed == false (or absent) the supervisor check is
// not registered, keeping the catalog lean for users who manage Ollama
// themselves.
func TestDefaultChecks_UnmanagedOllamaNoSupervisorCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "ollama", Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {ChatModel: "llama3:8b", EmbedModel: "bge-m3"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	if contains(names, "provider.ollama.supervisor") {
		t.Errorf("catalog should not contain provider.ollama.supervisor when managed=false; got %v", names)
	}
}

// --- Memory-layer check tests ---

// TestExtractorProviderCheck_OK: valid chat provider with a model set returns
// StatusOK and reports the chat/model pair plus confirmation of extract-v1.
func TestExtractorProviderCheck_OK(t *testing.T) {
	t.Parallel()
	cfg := &config.UserConfig{
		Defaults: config.DefaultsTable{Chat: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {ChatModel: "llama3.2"},
		},
	}
	c := newExtractorProviderCheck(cfg)
	if c.Name() != "extractor.provider" {
		t.Errorf("Name() = %q, want extractor.provider", c.Name())
	}
	if c.Subsystem() != subsystemMemory {
		t.Errorf("Subsystem() = %q, want %q", c.Subsystem(), subsystemMemory)
	}
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %v, want StatusOK (detail=%q)", st, detail)
	}
	if !strings.Contains(detail, "ollama") || !strings.Contains(detail, "llama3.2") {
		t.Errorf("detail %q missing provider/model info", detail)
	}
	if !strings.Contains(detail, "extract-v1") {
		t.Errorf("detail %q missing extract-v1 confirmation", detail)
	}
}

// TestExtractorProviderCheck_NoChatModel: chat=ollama but no chat_model set
// returns StatusWarn with actionable detail.
func TestExtractorProviderCheck_NoChatModel(t *testing.T) {
	t.Parallel()
	cfg := &config.UserConfig{
		Defaults: config.DefaultsTable{Chat: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {},
		},
	}
	c := newExtractorProviderCheck(cfg)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusWarn {
		t.Errorf("status = %v, want StatusWarn (detail=%q)", st, detail)
	}
	if !strings.Contains(detail, "chat_model") || !strings.Contains(detail, "maz init") {
		t.Errorf("detail %q missing actionable hint", detail)
	}
}

// TestExtractorProviderCheck_ChatDisabled: an explicit chat=none is the
// intended agent-side-reasoning setup and reports OK; an unset defaults.chat
// is likely misconfiguration and still warns.
func TestExtractorProviderCheck_ChatDisabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		chat       string
		wantStatus Status
		wantDetail string
	}{
		{name: "explicit none is intended", chat: "none", wantStatus: StatusOK, wantDetail: "your agent does the reasoning"},
		{name: "unset warns", chat: "", wantStatus: StatusWarn, wantDetail: "extractor disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.UserConfig{
				Defaults:  config.DefaultsTable{Chat: tc.chat},
				Providers: map[string]config.ProviderTable{},
			}
			st, detail, err := runCheck(t, newExtractorProviderCheck(cfg))
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if st != tc.wantStatus {
				t.Errorf("status = %v, want %v (detail=%q)", st, tc.wantStatus, detail)
			}
			if !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail %q missing %q", detail, tc.wantDetail)
			}
		})
	}
}

// TestResolverPredicatesCheck_OK: a fresh SQLite store with migrations applied
// has >= 10 predicate rows and returns StatusOK.
func TestResolverPredicatesCheck_OK(t *testing.T) {
	t.Parallel()
	s := newSharedForTest(t)
	// Acquiring the store runs the migration runner which applies 0006 and seeds
	// the predicates table.
	ctx := t.Context()
	if _, err := s.acquire(ctx); err != nil {
		t.Fatalf("acquire sqlite: %v", err)
	}
	c := newResolverPredicatesCheck(s)
	if c.Name() != "resolver.predicates_loaded" {
		t.Errorf("Name() = %q, want resolver.predicates_loaded", c.Name())
	}
	if c.Subsystem() != subsystemMemory {
		t.Errorf("Subsystem() = %q, want %q", c.Subsystem(), subsystemMemory)
	}
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %v, want StatusOK (detail=%q)", st, detail)
	}
	if !strings.Contains(detail, "predicates loaded") {
		t.Errorf("detail %q missing 'predicates loaded'", detail)
	}
	if !strings.Contains(detail, "functional") {
		t.Errorf("detail %q missing functional count", detail)
	}
}

// TestResolverPredicatesCheck_MissingTable: pointing at a DB that has no
// predicates table (e.g. only migrations 0001-0005 applied) returns StatusFail
// with a hint to run maz rebuild.
func TestResolverPredicatesCheck_MissingTable(t *testing.T) {
	t.Parallel()
	// Build a sharedSQLite whose underlying DB we seed manually without the
	// full migration runner, so the predicates table is absent.
	dir := t.TempDir()
	shared := &sharedSQLite{path: sqliteIndexPath(dir)}
	ctx := t.Context()

	store, err := shared.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire sqlite: %v", err)
	}
	// Drop the predicates table to simulate a pre-0006 migration state.
	if _, err := store.Writer().ExecContext(ctx, `DROP TABLE IF EXISTS predicates`); err != nil {
		t.Fatalf("drop predicates: %v", err)
	}

	c := newResolverPredicatesCheck(shared)
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusFail {
		t.Errorf("status = %v, want StatusFail (detail=%q)", st, detail)
	}
	if !strings.Contains(detail, "0006") {
		t.Errorf("detail %q missing migration hint (0006)", detail)
	}
	if !strings.Contains(detail, "maz rebuild") {
		t.Errorf("detail %q missing 'maz rebuild' hint", detail)
	}
}

// TestComposerPromptCheck_OK: all eight templates register cleanly against a
// fresh registry and the check reports StatusOK.
func TestComposerPromptCheck_OK(t *testing.T) {
	t.Parallel()
	c := newComposerPromptCheck()
	if c.Name() != "composer.prompt_registered" {
		t.Errorf("Name() = %q, want composer.prompt_registered", c.Name())
	}
	if c.Subsystem() != subsystemMemory {
		t.Errorf("Subsystem() = %q, want %q", c.Subsystem(), subsystemMemory)
	}
	st, detail, err := runCheck(t, c)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if st != StatusOK {
		t.Errorf("status = %v, want StatusOK (detail=%q)", st, detail)
	}
	if !strings.Contains(detail, "8") {
		t.Errorf("detail %q missing template count (8)", detail)
	}
}

// TestDefaultChecks_IncludesMemoryLayerChecks: the three memory-layer checks
// are registered in the DefaultChecks catalog when a valid config is present.
func TestDefaultChecks_IncludesMemoryLayerChecks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "ollama", Embed: "ollama"},
		Providers: map[string]config.ProviderTable{
			"ollama": {ChatModel: "llama3.2", EmbedModel: "bge-m3"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	names := checkNames(DefaultChecks(dir))
	for _, want := range []string{
		"extractor.provider",
		"resolver.predicates_loaded",
		"composer.prompt_registered",
	} {
		if !contains(names, want) {
			t.Errorf("catalog missing %q; got %v", want, names)
		}
	}
	// The old single check must be gone.
	if contains(names, "memory.layer") {
		t.Errorf("catalog still contains deprecated memory.layer check; got %v", names)
	}
}

// TestLadybugCheck_ReadOnlyDataDir verifies that newLadybugCheck returns
// StatusFail with an actionable message when the data dir is not writable
// (mode 0555), rather than letting the LadybugDB C layer SIGABRT the
// process. The test wraps the check in a recover() to guard against any
// regression that lets a C-level panic escape.
func TestLadybugCheck_ReadOnlyDataDir(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks; skip on root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() {
		// Restore write permission so t.TempDir cleanup can remove the dir.
		_ = os.Chmod(dir, 0o755)
	}()

	c := newLadybugCheck(dir)

	var st Status
	var detail string
	var runErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("newLadybugCheck panicked on read-only dir: %v", r)
			}
		}()
		st, detail, runErr = c.Run(context.Background())
	}()

	if st != StatusFail {
		t.Errorf("status = %q, want %q (detail=%q, err=%v); read-only dir must yield FAIL not crash",
			st, StatusFail, detail, runErr)
	}
	if !strings.Contains(detail, "writable") && !strings.Contains(detail, "permission") && !strings.Contains(detail, "read-only") {
		t.Errorf("detail %q should mention the writability failure", detail)
	}
}

// TestAnthropicPingCheck_401_HintSecret verifies that a 401 response from
// the Anthropic API causes the check to emit StatusFail with an actionable
// hint to run `maz secret set anthropic.api_key`, so operators know
// immediately how to remedy the bad key rather than hunting through docs.
// Not parallel: uses t.Setenv.
func TestAnthropicPingCheck_401_HintSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// Anthropic error body shape; the client decodes this into *anthropic.APIError.
		fmt.Fprintln(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
	}))
	defer srv.Close()
	// Provide a dummy key so the env-chain resolves and the client proceeds to ping.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-dummy")

	// Pass the mock server URL via the ProviderTable so newAnthropicPingCheck
	// dials the local server rather than the real Anthropic API.
	c := newAnthropicPingCheck(&config.ProviderTable{
		Model:   "claude-opus-4-7",
		BaseURL: srv.URL,
	})
	st, detail, err := runCheck(t, c)
	if st != StatusFail {
		t.Errorf("status = %q, want %q (detail=%q, err=%v)", st, StatusFail, detail, err)
	}
	// The detail must contain the recovery hint; err must be nil when we carry
	// the hint in detail (the 401 branch returns nil error by design).
	if !strings.Contains(detail, "maz secret set") {
		t.Errorf("FAIL on 401 must include 'maz secret set' hint; got detail=%q err=%v", detail, err)
	}
}

// TestMCPHealthzCheck_ProbesSocket verifies DefaultChecks wires the
// mcp.healthz check to the data dir's Unix socket — the default transport
// now that TCP is opt-in — so `maz doctor --data-dir /custom` probes the
// daemon actually serving /custom rather than a TCP port.
func TestMCPHealthzCheck_ProbesSocket(t *testing.T) {
	t.Parallel()
	dataDir := startUnixHealthzServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != mcp.HealthPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(mcp.HealthResponse{Status: "ok", StartTime: time.Now().UTC()})
	}))

	// Find the mcp.healthz check that DefaultChecks registers and run it.
	var found bool
	for _, c := range DefaultChecks(dataDir) {
		if c.Name() == "mcp.healthz" {
			found = true
			st, detail, err := runCheck(t, c)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if st != StatusOK {
				t.Errorf("mcp.healthz status = %q, want %q (detail=%q); socket probe not wired", st, StatusOK, detail)
			}
		}
	}
	if !found {
		t.Fatalf("mcp.healthz check not registered in DefaultChecks")
	}
}
