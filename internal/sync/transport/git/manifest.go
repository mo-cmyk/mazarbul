package git

// manifest.go — per-push MANIFEST.json regeneration.
//
// The MANIFEST is a plaintext index of every chunk currently in the
// scope repo. Each entry carries the same plaintext metadata that
// already lives in the chunk's _meta line (or the MAZ1 plaintext
// header): chunkId, count, firstUlid, lastUlid, createTime, device,
// encrypted. Re-rendered on every push so `git log -p MANIFEST.json`
// becomes a complete history of what was added when by which
// device — exactly the "in each commit how information tracking
// worked" experience the product brief asked for.
//
// Plaintext metadata only. Never event content. Mode-agnostic: the
// same MANIFEST shape works for plaintext and encrypted repos.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// manifestFilename is the canonical filename at scope repo root.
const manifestFilename = "MANIFEST.json"

// chunkRecord pairs a chunk's plaintext header with the hex SHA-256 of
// its canonical on-disk bytes (chunk.ContentHash). The hash lets a
// consumer detect a chunk hand-edited outside Mazarbul (the MANIFEST
// integrity contract); it is not part of chunk.Header because it
// describes the file framing, not the sealed payload.
type chunkRecord struct {
	Header      chunk.Header
	ContentHash string
}

// manifestEntry is the wire shape of one row in MANIFEST.json.
// Field order intentionally matches chunk.Header so the JSON diff
// in the GitHub web UI reads naturally per-chunk.
type manifestEntry struct {
	ChunkID    string `json:"chunkId"`
	Scope      string `json:"scope"`
	Count      int    `json:"count"`
	FirstULID  string `json:"firstUlid"`
	LastULID   string `json:"lastUlid"`
	CreateTime string `json:"createTime"`
	Device     string `json:"device,omitempty"`
	Encrypted  bool   `json:"encrypted"`
	// ContentHash is the lowercase hex SHA-256 of the chunk file's
	// canonical bytes. A consumer recomputes it over the bytes it pulls
	// and refuses to graft on mismatch — the integrity guard that brings
	// plaintext chunks up to the tamper-detection the AEAD tag already
	// gives encrypted ones. Present only in schemaVersion >= 2 manifests;
	// omitempty so a legacy reader and the diff stay clean.
	ContentHash string `json:"contentHash,omitempty"`
	// DEKEpoch is the data-encryption-key epoch that sealed this chunk.
	// Present only for encrypted (team-scope envelope-mode) chunks; absent
	// (omitted) for plaintext chunks. The doctor sync.dek_coverage check reads
	// this field to determine which epochs the remote references and whether the
	// local keychain holds the matching DEK(s). Genesis epoch=0 is omitted here
	// when it is the only value (omitempty), so "absent" means epoch 0 for
	// encrypted chunks — the caller treats absence the same as 0.
	DEKEpoch int `json:"dekEpoch,omitempty"`
}

// manifest is the JSON document the file contains. Wrapping the
// slice in a top-level object (rather than a bare array) leaves
// room to evolve: schema version, repo-level summary fields, etc.
type manifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	Chunks        []manifestEntry `json:"chunks"`
}

// manifestSchemaVersion is bumped when the MANIFEST shape changes
// incompatibly. Older readers ignore unknown fields per Go's JSON
// default, so additive changes do not require a bump. Bumped to 2 when
// per-chunk ContentHash landed: a consumer gates integrity verification
// on schemaVersion >= 2 so a legacy (v1) manifest is treated as
// "unverified", never "tampered".
const manifestSchemaVersion = 2

// writeManifest regenerates MANIFEST.json in repoDir to reflect the
// chunk records in recs. ULID-sorted (which is also seal-time-sorted
// because chunkID is a ULID). Atomic via temp+rename so a crash
// mid-write leaves the previous MANIFEST intact.
func writeManifest(repoDir string, recs []chunkRecord) error {
	sorted := make([]chunkRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Header.ChunkID < sorted[j].Header.ChunkID })
	entries := make([]manifestEntry, 0, len(sorted))
	for _, r := range sorted {
		h := r.Header
		e := manifestEntry{
			ChunkID:     h.ChunkID,
			Scope:       string(h.Scope),
			Count:       h.Count,
			FirstULID:   h.FirstULID,
			LastULID:    h.LastULID,
			CreateTime:  h.CreateTime.UTC().Format("2006-01-02T15:04:05.000Z"),
			Device:      h.Device,
			Encrypted:   h.Encrypted,
			ContentHash: r.ContentHash,
		}
		// Record the DEK epoch only for encrypted (team-scope) chunks so the
		// sync.dek_coverage doctor check can compare remote epochs against the
		// local keychain without having to open the sealed payload. Plaintext
		// chunks keep DEKEpoch=0 (omitted by omitempty) since they have no DEK.
		if h.Encrypted {
			e.DEKEpoch = h.DEKEpoch
		}
		entries = append(entries, e)
	}
	doc := manifest{SchemaVersion: manifestSchemaVersion, Chunks: entries}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("git transport: marshal MANIFEST: %w", err)
	}
	raw = append(raw, '\n') // trailing newline is the universally polite default
	finalPath := filepath.Join(repoDir, manifestFilename)
	tmp, err := os.CreateTemp(repoDir, ".tmp-manifest-*")
	if err != nil {
		return fmt.Errorf("git transport: create temp MANIFEST: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			_ = rmErr
		}
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("git transport: write temp MANIFEST: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("git transport: chmod MANIFEST: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("git transport: close MANIFEST: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		cleanup()
		return fmt.Errorf("git transport: rename MANIFEST: %w", err)
	}
	return nil
}

// readManifestContentHashes reads MANIFEST.json from repoDir and returns
// the recorded chunkID -> content-hash map. Returns an empty (non-nil)
// map when the manifest is absent, unreadable, or pre-v2 (no hashes
// recorded) — the consumer treats a missing hash as "unverified", never
// "tampered", so a legacy remote degrades gracefully rather than
// rejecting every chunk.
func readManifestContentHashes(repoDir string) map[string]string {
	out := make(map[string]string)
	raw, err := os.ReadFile(filepath.Join(repoDir, manifestFilename)) //nolint:gosec // fixed filename under the scope mirror
	if err != nil {
		return out
	}
	var doc manifest
	if err := json.Unmarshal(raw, &doc); err != nil || doc.SchemaVersion < 2 {
		return out
	}
	for _, e := range doc.Chunks {
		if e.ContentHash != "" {
			out[e.ChunkID] = e.ContentHash
		}
	}
	return out
}

// listChunkHeaders walks chunksDir and returns the plaintext header plus
// the canonical content hash of every chunk file. Used to regenerate
// MANIFEST.json from the current working tree (rather than tracking
// incremental state).
func listChunkHeaders(chunksDir string) ([]chunkRecord, error) {
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("git transport: readdir for manifest: %w", err)
	}
	out := make([]chunkRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isChunkFilename(e.Name()) {
			continue
		}
		path := filepath.Join(chunksDir, e.Name())
		blob, rerr := os.ReadFile(path) //nolint:gosec // bounded by ReadDir of internal dir
		if rerr != nil {
			return nil, fmt.Errorf("git transport: read chunk for manifest %s: %w", path, rerr)
		}
		hdr, perr := chunk.PeekHeader(blob)
		if perr != nil {
			// Mirror the listing path: a stale pre-0x02 chunk (ErrBadVersion)
			// or a stray non-chunk file (ErrBadMagic) is omitted from the
			// MANIFEST rather than failing the whole push. Real corruption of
			// a valid chunk still surfaces.
			if errors.Is(perr, chunk.ErrBadVersion) || errors.Is(perr, chunk.ErrBadMagic) {
				continue
			}
			return nil, fmt.Errorf("git transport: peek chunk for manifest %s: %w", path, perr)
		}
		out = append(out, chunkRecord{Header: hdr, ContentHash: chunk.ContentHash(blob)})
	}
	return out, nil
}
