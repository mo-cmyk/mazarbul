package selfupdate

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// VerifyChecksum confirms the archive at archivePath matches its entry in the
// checksums.txt at sumsPath. It must run before anything is placed: a tampered
// or corrupt archive is rejected here, before extraction or swap.
func VerifyChecksum(archivePath, sumsPath, name string) error {
	expected, err := expectedChecksum(sumsPath, name)
	if err != nil {
		return err
	}
	actual, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	// Constant-time compare is defense-in-depth: the hashes are not secret, but
	// it costs nothing and avoids a timing side channel on the comparison.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) != 1 {
		return fmt.Errorf("selfupdate: checksum mismatch for %s (expected %s, got %s)", name, expected, actual)
	}
	return nil
}

// expectedChecksum reads the sha256 hex for name from a checksums.txt in
// sha256sum format ("<hex>  <name>"). The filename column is matched exactly
// (not as a pattern): the archive name contains dots that a regex would treat
// as wildcards, so a tampered checksums.txt could otherwise map a look-alike
// name to the wrong hash. A binary-mode "*" name prefix is tolerated.
func expectedChecksum(sumsPath, name string) (string, error) {
	f, err := os.Open(sumsPath) // #nosec G304 -- sumsPath is under a caller-created temp dir
	if err != nil {
		return "", fmt.Errorf("selfupdate: open checksums: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("selfupdate: read checksums: %w", err)
	}
	return "", fmt.Errorf("selfupdate: no checksum entry for %s in checksums.txt", name)
}

// fileSHA256 returns the lower-case hex sha256 of the file, streamed so a large
// archive is never fully buffered in memory.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is under a caller-created temp dir
	if err != nil {
		return "", fmt.Errorf("selfupdate: open archive: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("selfupdate: hash archive: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
