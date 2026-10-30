package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// httpTokenFilename is the basename of the bearer-token file the daemon
// writes into its data dir when the (opt-in) TCP transport is enabled.
// Kept alongside the Unix socket and PID file so a deployment has a single
// directory to inspect.
const httpTokenFilename = "mcp-http-token"

// HTTPTokenPath returns the path of the MCP HTTP bearer-token file for a
// daemon whose data directory is dataDir. Local TCP clients read this file
// to authenticate; it is distinct from the argv ownership token in the PID
// file (which is visible via `ps` and is therefore unfit as a credential).
func HTTPTokenPath(dataDir string) string {
	return filepath.Join(dataDir, httpTokenFilename)
}

// MintAndWriteHTTPToken generates a fresh 256-bit bearer token, persists
// it 0600 at HTTPTokenPath(dataDir), and returns it. The write is atomic
// (temp file + chmod + fsync + rename) so a reader never observes a
// partial or world-readable token, even if a stale file from a previous
// run is being replaced.
func MintAndWriteHTTPToken(dataDir string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("daemon: mint HTTP token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := writeFile0600Atomic(HTTPTokenPath(dataDir), []byte(token)); err != nil {
		return "", fmt.Errorf("daemon: write HTTP token: %w", err)
	}
	return token, nil
}

// RemoveHTTPToken deletes the bearer-token file. A missing file is not an
// error, so the supervisor can call it unconditionally on shutdown.
func RemoveHTTPToken(dataDir string) error {
	if err := os.Remove(HTTPTokenPath(dataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon: remove HTTP token: %w", err)
	}
	return nil
}

// writeFile0600Atomic writes data to path with mode 0600 via a temp file
// in the same directory, fsynced and renamed into place. Mirrors the
// recipe in internal/config/toml_atomic.go; kept local to avoid coupling
// the daemon to the TOML-marshalling writer.
func writeFile0600Atomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup of the temp file on the error path
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}
