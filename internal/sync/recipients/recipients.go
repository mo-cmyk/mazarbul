// Package recipients models the recipients.jsonl bootstrap artifact for
// encrypted team scopes.
//
// For an ENCRYPTED team scope the synced git repo contains only opaque
// .maz chunks plus MANIFEST.json — there is no plaintext rendered graph.
// A fresh device that joins the team must obtain the team's symmetric
// data-encryption key (DEK) BEFORE it can decrypt any chunk, which is a
// chicken-and-egg problem: the key it needs lives behind the encryption
// it cannot yet open. recipients.jsonl breaks that circularity. It lists,
// per admitted member per DEK epoch, the member's wrap public key and the
// DEK wrapped (sealed) to that public key. A wrapped DEK is safe to
// publish: only the holder of the matching wrap private key can open it.
// The joining device finds its row, unwraps its DEK with its local wrap
// private key, then decrypts the chunks.
//
// PRIVACY: a Row carries NO personally-identifying information — no
// display name, no GitHub login, no email. Only an opaque principal UID,
// the wrap public key, the DEK epoch, the wrapped DEK, and a create time.
// The struct deliberately has no field for anything else; a reflection
// test in recipients_test.go enforces the field set.
//
// The package is stdlib-only so any layer (transport, kernel, cli) can
// import it without risking an import cycle.
package recipients

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Filename is the canonical name of the recipients artifact at the
// scope repo root.
const Filename = "recipients.jsonl"

// Row is one admitted (member, DEK epoch) entry in recipients.jsonl. It
// is the publishable bootstrap record a joining device uses to recover
// its DEK. Every field is non-identifying: a Row reveals only that some
// opaque principal holds a wrap key and may open one epoch's DEK.
type Row struct {
	// PrincipalUID is the opaque identifier of the admitted member.
	// It carries no name, login, or email — only a stable handle the
	// joining device matches against its own principal identity.
	PrincipalUID string `json:"principalUid"`
	// WrapPublicKey is the member's base64-encoded x25519 wrap public
	// key. The DEK in WrappedDEK is sealed to this key.
	WrapPublicKey string `json:"wrapPublicKey"`
	// DEKEpoch is the integer epoch of the data-encryption key this row
	// wraps. The DEK is rotated on membership change; each epoch has its
	// own set of recipient rows.
	DEKEpoch int `json:"dekEpoch"`
	// WrappedDEK is the base64-encoded sealed box of the DEK, opened only
	// by the holder of the matching wrap private key. Safe to publish.
	WrappedDEK string `json:"wrappedDek"`
	// CreateTime is when this row was admitted, in UTC.
	CreateTime time.Time `json:"createTime"`
}

// Write serializes rows to path as JSONL (one compact JSON object per
// line, trailing newline). Rows are sorted deterministically by
// (DEKEpoch, PrincipalUID) so the git diff is stable across runs
// regardless of input order. The parent directory is created (0o700) if
// absent. The write is atomic via a temp file in the same directory plus
// rename, so a concurrent reader never sees a half-written file.
func Write(path string, rows []Row) error {
	// Refuse to publish an empty roster: an admitted team always has at least
	// its genesis member, so zero rows signals a transient/buggy read, and
	// overwriting the file with zero bytes would destroy the bootstrap roster a
	// joining device needs. The caller leaves the prior file in place.
	if len(rows) == 0 {
		return fmt.Errorf("recipients: refusing to write an empty roster to %s", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("recipients: mkdir %s: %w", dir, err)
	}

	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].DEKEpoch != sorted[j].DEKEpoch {
			return sorted[i].DEKEpoch < sorted[j].DEKEpoch
		}
		return sorted[i].PrincipalUID < sorted[j].PrincipalUID
	})

	var buf bytes.Buffer
	for i := range sorted {
		line, err := json.Marshal(sorted[i])
		if err != nil {
			return fmt.Errorf("recipients: marshal row %d: %w", i, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	tmp, err := os.CreateTemp(dir, ".tmp-recipients-*")
	if err != nil {
		return fmt.Errorf("recipients: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			_ = rmErr // best-effort: a leftover temp file is harmless
		}
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recipients: write temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recipients: chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("recipients: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("recipients: rename: %w", err)
	}
	return nil
}

// Read parses the JSONL recipients file at path. A missing file returns
// (nil, nil): recipients.jsonl is a best-effort bootstrap, and its
// absence simply means no admitted members have been published yet.
// Blank lines are skipped. A malformed line returns a wrapped error.
func Read(path string) ([]Row, error) {
	f, err := os.Open(path) //nolint:gosec // path is caller-controlled, not user input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("recipients: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var rows []Row
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var row Row
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("recipients: parse %s line %d: %w", path, line, err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("recipients: scan %s: %w", path, err)
	}
	return rows, nil
}
