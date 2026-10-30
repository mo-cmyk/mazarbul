package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// --- fakes ----------------------------------------------------------------

type noopIngestAppender struct{}

func (noopIngestAppender) Append(context.Context, []kernel.Event) ([]kernel.Receipt, error) {
	return []kernel.Receipt{}, nil
}

type noopIngestReader struct{}

func (noopIngestReader) SourceRefHash(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (noopIngestReader) Watermark(_ context.Context, connector, stream string) (ingest.Watermark, error) {
	return ingest.Watermark{Connector: connector, Stream: stream}, nil
}

type noopSecrets struct{}

func (noopSecrets) Get(context.Context, string) (string, error) { return "", secret.ErrNotFound }
func (noopSecrets) Set(context.Context, string, string) error   { return secret.ErrReadOnly }
func (noopSecrets) Delete(context.Context, string) error        { return secret.ErrReadOnly }

func newTestConnectSource() *ConnectSource {
	orch := ingest.NewOrchestrator(noopIngestAppender{}, noopIngestReader{}, jobs.NewRegistry(), zap.NewNop())
	return NewConnectSource(orch, noopSecrets{}, nil, zap.NewNop())
}

func wantValidation(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want E_VALIDATION error, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("err.Code = %q, want %q (%v)", te.Code, mcp.CodeValidation, err)
	}
}

// --- tests ----------------------------------------------------------------

func TestConnectSource_NamesAndSchemas(t *testing.T) {
	t.Parallel()
	c := newTestConnectSource()
	if c.Name() != "connectSource" {
		t.Errorf("Name = %q", c.Name())
	}
	if !json.Valid(c.InputSchema()) || !json.Valid(c.OutputSchema()) {
		t.Error("schemas must be valid JSON")
	}
}

func TestConnectSource_RejectsMissingConnector(t *testing.T) {
	t.Parallel()
	c := newTestConnectSource()
	_, err := c.Invoke(context.Background(), json.RawMessage(`{"repoPath":"/tmp"}`))
	wantValidation(t, err)
}

func TestConnectSource_RejectsMissingRepoPath(t *testing.T) {
	t.Parallel()
	c := newTestConnectSource()
	_, err := c.Invoke(context.Background(), json.RawMessage(`{"connector":"local-git"}`))
	wantValidation(t, err)
}

func TestConnectSource_RefusesProjectScope(t *testing.T) {
	t.Parallel()
	c := newTestConnectSource()
	_, err := c.Invoke(context.Background(),
		json.RawMessage(`{"connector":"local-git","repoPath":"/tmp","scope":"project"}`))
	wantValidation(t, err)
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("error should explain project refusal: %v", err)
	}
}

// TestConnectSource_RefusesPlaintextScope pins the #212 up-front guard: when the
// target scope syncs as plaintext, connect refuses before fetching anything
// (rather than letting the seal-time leak gate silently stall the events).
func TestConnectSource_RefusesPlaintextScope(t *testing.T) {
	t.Parallel()
	orch := ingest.NewOrchestrator(noopIngestAppender{}, noopIngestReader{}, jobs.NewRegistry(), zap.NewNop())
	guard := func(scope kernel.Scope) bool { return scope == kernel.ScopePersonal }
	c := NewConnectSource(orch, noopSecrets{}, guard, zap.NewNop())
	_, err := c.Invoke(context.Background(),
		json.RawMessage(`{"connector":"local-git","repoPath":"/tmp","scope":"personal"}`))
	wantValidation(t, err)
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("error should explain plaintext refusal: %v", err)
	}
}

func TestConnectSource_RejectsUnknownConnector(t *testing.T) {
	t.Parallel()
	c := newTestConnectSource()
	_, err := c.Invoke(context.Background(),
		json.RawMessage(`{"connector":"slack","repoPath":"/tmp"}`))
	wantValidation(t, err)
}

func TestConnectSource_RejectsBadRepoPath(t *testing.T) {
	t.Parallel()
	c := newTestConnectSource()
	_, err := c.Invoke(context.Background(),
		json.RawMessage(`{"connector":"local-git","repoPath":"/nonexistent/not/a/repo","dryRun":true}`))
	wantValidation(t, err) // Validate() fails: not a git repo
}

func TestIngestSource_NameAndRefusesProjectScope(t *testing.T) {
	t.Parallel()
	orch := ingest.NewOrchestrator(noopIngestAppender{}, noopIngestReader{}, jobs.NewRegistry(), zap.NewNop())
	i := NewIngestSource(orch, noopSecrets{}, nil, zap.NewNop())
	if i.Name() != "ingestSource" {
		t.Errorf("Name = %q", i.Name())
	}
	_, err := i.Invoke(context.Background(),
		json.RawMessage(`{"connector":"local-git","repoPath":"/tmp","scope":"project"}`))
	wantValidation(t, err)
}

func TestConnectSource_DryRunReportsItems(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitInit(t, dir)
	gitCommit(t, dir, "first commit message")
	gitCommit(t, dir, "second commit message")

	c := newTestConnectSource()
	out, err := c.Invoke(context.Background(),
		json.RawMessage(`{"connector":"local-git","repoPath":"`+dir+`","dryRun":true}`))
	if err != nil {
		t.Fatalf("Invoke dryRun: %v", err)
	}
	var report ingest.Report
	if uerr := json.Unmarshal(out, &report); uerr != nil {
		t.Fatalf("unmarshal report: %v", uerr)
	}
	if !report.DryRun {
		t.Error("report.DryRun = false, want true")
	}
	if report.ItemsApplied != 2 {
		t.Errorf("itemsApplied = %d, want 2 (preview count)", report.ItemsApplied)
	}
	if report.Connector != "local-git" {
		t.Errorf("connector = %q", report.Connector)
	}
}

// --- git helpers (scrubbed env so a leaked GIT_DIR cannot touch the real repo) -

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGitTest(t, dir, "init", "-b", "main")
}

func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	runGitTest(t, dir,
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
		"commit", "--allow-empty", "-m", msg)
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(context.Background(), "git", full...)
	cmd.Env = scrubbedGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func scrubbedGitEnv() []string {
	drop := map[string]struct{}{
		"GIT_DIR": {}, "GIT_WORK_TREE": {}, "GIT_INDEX_FILE": {},
		"GIT_OBJECT_DIRECTORY": {}, "GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
		"GIT_COMMON_DIR": {}, "GIT_NAMESPACE": {}, "GIT_PREFIX": {},
	}
	var out []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if _, bad := drop[key]; !bad {
			out = append(out, kv)
		}
	}
	return out
}
