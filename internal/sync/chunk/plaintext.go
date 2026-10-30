package chunk

// plaintext.go — GitHub-friendly JSONL variant of the chunk format.
//
// The plaintext default inverts the original encrypted default: events ship plaintext unless the
// operator explicitly opts into encryption. Plaintext chunks are one
// event per line so GitHub's web UI renders them with syntax
// highlighting, git diff hunks line up per event, and `git blame`
// works directly on individual events. The MAZ1-framed encrypted
// variant in chunk.go remains the opt-in privacy mode.
//
// Wire framing (file contents, no magic bytes):
//
//	line 1:       {"_meta":true, …Header fields…}
//	lines 2..N:   one kernel.Event per line, JSON-encoded
//
// The leading "_meta":true sentinel lets the decoder distinguish the
// metadata line from event lines without positional bias and makes
// the chunk self-describing when inspected outside Mazarbul.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// ExtensionJSONL is the canonical filename suffix for plaintext chunks.
const ExtensionJSONL = ".jsonl"

// metaEnvelope wraps Header for serialization with the leading
// `"_meta":true` sentinel. The Header fields are flattened via
// struct embedding so the on-the-wire shape is one object, not a
// nested {"_meta":true,"header":{…}}.
type metaEnvelope struct {
	Meta bool `json:"_meta"`
	Header
}

// EncodePlaintext serializes events into a plaintext JSONL chunk.
// Caller-public companion to Encode(opts.ScopeKey=nil); useful for
// callers (sync CLI, tests) that want the plaintext path explicitly
// without constructing an EncodeOpts.
//
// Returns the file bytes plus the populated Header (Encrypted=false,
// Device=device). Mismatched scopes on events are rejected so a
// caller bug cannot leak data across scope boundaries.
func EncodePlaintext(events []kernel.Event, scope kernel.Scope, device string) ([]byte, Header, error) {
	return EncodeWithOpts(events, EncodeOpts{Scope: scope, ScopeKey: nil, Device: device})
}

// DecodePlaintext parses a plaintext JSONL chunk back into Header
// and events. The header's declared scope is enforced against
// expectedScope (defence against substituted chunks). The returned
// Header always has Encrypted=false (the plaintext invariant).
func DecodePlaintext(blob []byte, expectedScope kernel.Scope) (Header, []kernel.Event, error) {
	hdr, payload, err := parsePlaintextFrame(blob)
	if err != nil {
		return Header{}, nil, err
	}
	if expectedScope != "" && hdr.Scope != expectedScope {
		return Header{}, nil, fmt.Errorf("%w: header says %q, expected %q",
			ErrScopeMismatch, hdr.Scope, expectedScope)
	}
	events, err := parseJSONL(payload)
	if err != nil {
		return Header{}, nil, err
	}
	if len(events) != hdr.Count {
		return Header{}, nil, fmt.Errorf("chunk: header says count=%d, payload has %d events",
			hdr.Count, len(events))
	}
	return hdr, events, nil
}

// PeekHeaderJSONL extracts just the metadata line from a plaintext
// chunk. Cheaper than full Decode when only chunk-level metadata is
// needed (listing, MANIFEST regeneration, doctor reporting).
func PeekHeaderJSONL(blob []byte) (Header, error) {
	hdr, _, err := parsePlaintextFrame(blob)
	return hdr, err
}

// PathForJSONL returns the canonical filesystem path of a plaintext
// chunk inside dataDir. Mirrors PathFor for the encrypted variant.
func PathForJSONL(dataDir string, scope kernel.Scope, chunkID string) string {
	return filepath.Join(dataDir, "sync", string(scope), "chunks", chunkID+ExtensionJSONL)
}

// PathForHeader picks the right extension for hdr and returns the
// canonical filesystem path. Used by FSStore.Put and the git
// transport to write to the correct extension without re-sniffing
// the blob.
func PathForHeader(dataDir string, hdr Header) string {
	if hdr.Encrypted {
		return PathFor(dataDir, hdr.Scope, hdr.ChunkID)
	}
	return PathForJSONL(dataDir, hdr.Scope, hdr.ChunkID)
}

// marshalPlaintextChunk produces the JSONL representation: one _meta
// line plus one line per event. Called from EncodeWithOpts when
// ScopeKey is nil. Named to avoid clashing with the public
// EncodePlaintext entry point.
func marshalPlaintextChunk(hdr Header, sorted []kernel.Event) ([]byte, error) {
	hdr.Encrypted = false
	metaLine, err := json.Marshal(metaEnvelope{Meta: true, Header: hdr})
	if err != nil {
		return nil, fmt.Errorf("chunk: marshal _meta line: %w", err)
	}
	var out bytes.Buffer
	out.Write(metaLine)
	out.WriteByte('\n')
	enc := json.NewEncoder(&out)
	for _, ev := range sorted {
		if err := enc.Encode(ev); err != nil {
			return nil, fmt.Errorf("chunk: encode event %s: %w", ev.UID, err)
		}
	}
	return out.Bytes(), nil
}

// parsePlaintextFrame splits a JSONL chunk into Header + event-line
// payload. Validates the _meta sentinel, refuses chunks whose meta
// line is missing critical fields, and forces Encrypted=false on
// the returned header (the plaintext invariant).
func parsePlaintextFrame(blob []byte) (Header, []byte, error) {
	nl := bytes.IndexByte(blob, '\n')
	if nl < 0 {
		return Header{}, nil, fmt.Errorf("%w: missing _meta line terminator", ErrCorruptHeader)
	}
	if nl == 0 {
		return Header{}, nil, fmt.Errorf("%w: empty _meta line", ErrCorruptHeader)
	}
	metaRaw := blob[:nl]
	rest := blob[nl+1:]
	if len(metaRaw) > MaxHeaderBytes {
		return Header{}, nil, fmt.Errorf("%w: _meta line %d bytes exceeds cap %d",
			ErrCorruptHeader, len(metaRaw), MaxHeaderBytes)
	}
	var probe metaEnvelope
	if err := json.Unmarshal(metaRaw, &probe); err != nil {
		return Header{}, nil, fmt.Errorf("%w: %w", ErrCorruptHeader, err)
	}
	if !probe.Meta {
		return Header{}, nil, fmt.Errorf("%w: leading line missing _meta:true sentinel", ErrCorruptHeader)
	}
	if probe.ChunkID == "" || probe.Scope == "" {
		return Header{}, nil, fmt.Errorf("%w: _meta missing chunkId/scope", ErrCorruptHeader)
	}
	hdr := probe.Header
	hdr.Encrypted = false
	return hdr, rest, nil
}

// looksLikeMAZ1 returns true when blob starts with the 4-byte MAZ1
// magic. Router helper used by Decode + PeekHeader to pick between
// the framed encrypted path and the JSONL plaintext path.
func looksLikeMAZ1(blob []byte) bool {
	return len(blob) >= len(Magic) && string(blob[:len(Magic)]) == Magic
}

// looksLikePlaintextChunk returns true when blob starts with a `{`
// byte — the unique prefix of a JSONL chunk's `_meta` line. Used
// alongside looksLikeMAZ1 so the router can distinguish genuinely
// unrecognized input (neither prefix) from a chunk in either of
// the supported formats.
func looksLikePlaintextChunk(blob []byte) bool {
	return len(blob) > 0 && blob[0] == '{'
}
