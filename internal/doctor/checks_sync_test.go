package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/secret"
)

func TestStateTOMLCheck_AbsentIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, detail, err := newStateTOMLCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (absent state.toml is fine)", status)
	}
	if !strings.Contains(detail, "absent") {
		t.Errorf("detail = %q, want 'absent' hint", detail)
	}
}

func TestStateTOMLCheck_CorruptIsFail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.toml"),
		[]byte("not = valid = toml ="), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	status, _, err := newStateTOMLCheck(dir).Fn(context.Background())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if status != StatusFail {
		t.Errorf("status = %v, want FAIL", status)
	}
}

func TestSyncRemotesCheck_NoConfigIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, detail, err := newSyncRemotesCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (no config = single-device)", status)
	}
	if !strings.Contains(detail, "absent") && !strings.Contains(detail, "single-device") {
		t.Errorf("detail = %q, want absent/single-device", detail)
	}
}

func TestWatchwordPostureCheck_PlaintextOnlyIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// No config, no state — plaintext-default world. Watchword absent
	// is the expected new-user posture; check should be OK.
	status, detail, err := newWatchwordPostureCheck(dir, stubChain{}).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (plaintext default needs no Watchword)", status)
	}
	if !strings.Contains(detail, "plaintext") && !strings.Contains(detail, "not required") {
		t.Errorf("detail = %q, want plaintext/not-required hint", detail)
	}
}

func TestEncryptionPostureCheck_NoRemotesIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, detail, err := newEncryptionPostureCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (no remotes — n/a)", status)
	}
	if !strings.Contains(detail, "n/a") && !strings.Contains(detail, "no remotes") {
		t.Errorf("detail = %q, want n/a hint", detail)
	}
}

func TestSealedChunksReadableCheck_NoChunksIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, detail, err := newSealedChunksReadableCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (no chunks yet)", status)
	}
	if !strings.Contains(detail, "no sealed chunks") {
		t.Errorf("detail = %q, want 'no sealed chunks' hint", detail)
	}
}

func TestGitBinaryCheck_RunsWithoutPanic(t *testing.T) {
	t.Parallel()
	// We don't assert on the result because CI environments differ; we
	// only verify the check function returns either OK (git present)
	// or WARN (git absent) without panicking or erroring.
	status, _, err := newGitBinaryCheck().Fn(context.Background())
	if err != nil {
		t.Errorf("unexpected err: %v", err)
	}
	if status != StatusOK && status != StatusWarn {
		t.Errorf("status = %v, want OK or WARN", status)
	}
}

// stubChain returns ErrNotFound for every Get so the Watchword check
// exercises its absent-but-not-broken branch (status=Warn, no error).
type stubChain struct{}

func (stubChain) Get(_ context.Context, _ string) (string, error) {
	return "", secret.ErrNotFound
}

func (stubChain) Set(_ context.Context, _ string, _ string) error { return nil }
func (stubChain) Delete(_ context.Context, _ string) error        { return nil }
