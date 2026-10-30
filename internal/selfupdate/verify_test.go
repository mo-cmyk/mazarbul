package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyChecksum(t *testing.T) {
	t.Parallel()
	const name = "mazarbul_v1.0.0_darwin_arm64.tar.gz"
	data := []byte("archive-bytes")
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	// writeCase lays down the archive and a checksums.txt with the given line.
	writeCase := func(t *testing.T, sumsLine string) (string, string) {
		t.Helper()
		dir := t.TempDir()
		archive := filepath.Join(dir, name)
		if err := os.WriteFile(archive, data, 0o600); err != nil {
			t.Fatalf("write archive: %v", err)
		}
		sums := filepath.Join(dir, "checksums.txt")
		if err := os.WriteFile(sums, []byte(sumsLine), 0o600); err != nil {
			t.Fatalf("write sums: %v", err)
		}
		return archive, sums
	}

	t.Run("accepts a matching checksum", func(t *testing.T) {
		t.Parallel()
		archive, sums := writeCase(t, fmt.Sprintf("%s  %s\n", hexSum, name))
		if err := VerifyChecksum(archive, sums, name); err != nil {
			t.Errorf("VerifyChecksum: %v", err)
		}
	})

	t.Run("rejects a tampered checksum", func(t *testing.T) {
		t.Parallel()
		bad := "0000000000000000000000000000000000000000000000000000000000000000"
		archive, sums := writeCase(t, fmt.Sprintf("%s  %s\n", bad, name))
		if err := VerifyChecksum(archive, sums, name); err == nil {
			t.Error("expected a checksum mismatch error")
		}
	})

	t.Run("rejects a missing entry", func(t *testing.T) {
		t.Parallel()
		archive, sums := writeCase(t, hexSum+"  some-other-file.tar.gz\n")
		if err := VerifyChecksum(archive, sums, name); err == nil {
			t.Error("expected a missing-entry error")
		}
	})

	t.Run("does not treat dots as wildcards", func(t *testing.T) {
		t.Parallel()
		// A look-alike name (dots replaced) carrying the correct hash must not
		// satisfy the exact-name lookup for the real archive.
		lookalike := "mazarbul_v1X0X0_darwin_arm64Xtar.gz"
		archive, sums := writeCase(t, fmt.Sprintf("%s  %s\n", hexSum, lookalike))
		if err := VerifyChecksum(archive, sums, name); err == nil {
			t.Error("expected exact-name matching to reject a look-alike entry")
		}
	})
}
