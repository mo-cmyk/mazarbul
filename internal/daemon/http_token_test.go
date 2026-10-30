package daemon

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// TestMintAndWriteHTTPToken asserts the bearer-token file is written with
// owner-only 0600 permissions and holds a fresh 256-bit hex token. The mode
// matters: the token is the sole credential for the opt-in TCP transport, so
// a world- or group-readable file would hand it to every local user.
func TestMintAndWriteHTTPToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	tok, err := MintAndWriteHTTPToken(dir)
	if err != nil {
		t.Fatalf("MintAndWriteHTTPToken: %v", err)
	}

	raw, err := hex.DecodeString(tok)
	if err != nil {
		t.Fatalf("token %q is not hex: %v", tok, err)
	}
	if len(raw) != 32 {
		t.Fatalf("token decodes to %d bytes, want 32 (256-bit)", len(raw))
	}

	path := HTTPTokenPath(dir)
	if got := path; got != filepath.Join(dir, httpTokenFilename) {
		t.Fatalf("HTTPTokenPath = %q, want %q", got, filepath.Join(dir, httpTokenFilename))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %#o, want 0600", perm)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(onDisk) != tok {
		t.Fatalf("on-disk token %q != returned token %q", onDisk, tok)
	}
}

// TestMintAndWriteHTTPToken_Freshness pins that each mint replaces the prior
// token, so a restarted daemon never reuses a credential that may have leaked.
func TestMintAndWriteHTTPToken_Freshness(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := MintAndWriteHTTPToken(dir)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	second, err := MintAndWriteHTTPToken(dir)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if first == second {
		t.Fatal("two mints produced the same token; want a fresh credential each time")
	}
	// The atomic rewrite leaves exactly the latest token on disk.
	onDisk, err := os.ReadFile(HTTPTokenPath(dir))
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(onDisk) != second {
		t.Fatalf("on-disk token %q != latest mint %q", onDisk, second)
	}
}

// TestRemoveHTTPToken covers the supervisor's shutdown cleanup: the file is
// gone afterward, and a second remove (or a remove with no file) is not an
// error, so the deferred cleanup can run unconditionally.
func TestRemoveHTTPToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := MintAndWriteHTTPToken(dir); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := RemoveHTTPToken(dir); err != nil {
		t.Fatalf("RemoveHTTPToken: %v", err)
	}
	if _, err := os.Stat(HTTPTokenPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("token file still present after remove (stat err = %v)", err)
	}
	// Idempotent: removing an absent token is a no-op, not an error.
	if err := RemoveHTTPToken(dir); err != nil {
		t.Fatalf("RemoveHTTPToken on missing file = %v, want nil", err)
	}
}

// TestPrepareHTTPToken_TCPOffClearsStaleToken pins the file-presence
// invariant daemonHTTPEnabled relies on: when the daemon starts with TCP off
// (httpAddr empty), any token left behind by a previously SIGKILL-ed
// TCP-enabled daemon — whose shutdown defer never ran — must be cleared, so
// `maz daemon status` does not report httpEnabled:true for a listener that is
// off. Regression test for the stale-token-file edge case (PR #250).
func TestPrepareHTTPToken_TCPOffClearsStaleToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Simulate the crashed-daemon residue: a token file on disk while the
	// next launch has TCP off.
	if _, err := MintAndWriteHTTPToken(dir); err != nil {
		t.Fatalf("seed stale token: %v", err)
	}

	tok, cleanup, err := prepareHTTPToken(dir, "", zap.NewNop())
	if err != nil {
		t.Fatalf("prepareHTTPToken: %v", err)
	}
	t.Cleanup(cleanup)

	if tok != "" {
		t.Errorf("TCP off → token = %q, want empty", tok)
	}
	if _, err := os.Stat(HTTPTokenPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("stale token still present after TCP-off startup (stat err = %v)", err)
	}
}

// TestPrepareHTTPToken_TCPOnMintsAndCleansUp asserts the TCP-on path mints a
// fresh token file and that the returned cleanup removes it on shutdown.
func TestPrepareHTTPToken_TCPOnMintsAndCleansUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	tok, cleanup, err := prepareHTTPToken(dir, "127.0.0.1:7842", zap.NewNop())
	if err != nil {
		t.Fatalf("prepareHTTPToken: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("token len = %d, want 64 hex chars", len(tok))
	}
	if _, err := os.Stat(HTTPTokenPath(dir)); err != nil {
		t.Fatalf("token file absent after TCP-on startup: %v", err)
	}

	cleanup()
	if _, err := os.Stat(HTTPTokenPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("token file present after cleanup (stat err = %v)", err)
	}
}
