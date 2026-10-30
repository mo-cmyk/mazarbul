package secret

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// EnvSecretsDir names the environment variable that, when set, swaps the
// persistent secret store from the OS keychain to a directory-backed
// FileProvider rooted at its value. It is unset in normal production use, so
// the keychain remains the default. It exists so a Mazarbul instance can keep
// a fully isolated secret namespace — distinct identity, Watchword, and team
// DEK keys — without colliding on the single host keychain. That makes it
// possible to run more than one brain (personal, project, team) or more than
// one device profile on the same machine, and to run hermetic, prompt-free
// secret storage in CI or headless environments.
const EnvSecretsDir = "MAZ_SECRETS_DIR"

// FileProvider stores each secret as its own 0600 file inside a single
// directory. It is the writable persistent store used when EnvSecretsDir is
// set, standing in for KeychainProvider. The directory itself is created with
// 0700 permissions on first write.
//
// Each secret name maps to one file whose base name is the URL-safe base64
// encoding of the name. The encoding is injective and free of path
// separators, so distinct names never collide and a name can never escape the
// root directory via traversal.
//
// FileProvider is safe for concurrent readers and for a single writer; writes
// are atomic (temp file + rename) so a reader never observes a torn value.
// Concurrent writers to the same name race on the final rename, which is the
// same last-writer-wins semantics the keychain offers.
type FileProvider struct {
	dir string
}

// NewFileProvider returns a FileProvider rooted at dir. The directory is
// created lazily on the first Set, so a missing dir is not an error here.
func NewFileProvider(dir string) (*FileProvider, error) {
	if dir == "" {
		return nil, errors.New("secret: file provider dir is empty")
	}
	return &FileProvider{dir: dir}, nil
}

// path returns the absolute file path backing name.
func (f *FileProvider) path(name string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(name))
	return filepath.Join(f.dir, enc+".secret")
}

// Get reads the value stored under name. It returns ErrNotFound when no file
// backs the name.
func (f *FileProvider) Get(_ context.Context, name string) (string, error) {
	b, err := os.ReadFile(f.path(name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("secret: file get %q: %w", name, err)
	}
	return string(b), nil
}

// Set writes value under name, creating the root directory if needed. The
// write is atomic: the value lands in a temp file that is then renamed over
// any existing entry.
func (f *FileProvider) Set(_ context.Context, name string, value string) error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return fmt.Errorf("secret: file mkdir: %w", err)
	}
	dst := f.path(name)
	tmp, err := os.CreateTemp(f.dir, ".tmp-secret-*")
	if err != nil {
		return fmt.Errorf("secret: file set %q: %w", name, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			_ = rmErr // best-effort temp cleanup; the rename below already won on success
		}
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("secret: file chmod %q: %w", name, err)
	}
	if _, err := tmp.WriteString(value); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("secret: file write %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("secret: file close %q: %w", name, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		cleanup()
		return fmt.Errorf("secret: file rename %q: %w", name, err)
	}
	return nil
}

// Delete removes the file backing name. It returns ErrNotFound when no entry
// exists.
func (f *FileProvider) Delete(_ context.Context, name string) error {
	err := os.Remove(f.path(name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("secret: file delete %q: %w", name, err)
	}
	return nil
}
