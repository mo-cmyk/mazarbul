// Package chunk implements Mazarbul's sealed-chunk format for sync
// transport. A Chunk is an immutable, content-addressed bundle of
// events from one scope, gzip-compressed and AEAD-sealed with the
// scope's symmetric key (see internal/watchword).
//
// The format is the byte-level contract between every device pairing
// against the same scope: producers (the sealer) emit chunks; the
// transport layer (internal/sync/transport) shuttles them across a
// shared git remote; consumers (replay during pull) parse them back
// into kernel.Event batches that the dispatcher applies idempotently.
//
// File layout (on disk and in git):
//
//	<datadir>/sync/<scope>/chunks/<chunkID>.maz
//
// chunkID is a ULID minted at seal time so the lexicographic sort of
// chunk filenames coincides with seal order. The .maz extension is
// purely cosmetic — git treats the file as a binary blob.
//
// Wire framing (file contents):
//
//	[4 bytes] magic: "MAZ1"
//	[1 byte]  format version (currently 0x02)
//	[2 bytes] header length (big-endian uint16)
//	[N bytes] header (JSON; chunkID, scope, count, firstULID, lastULID, createTime, …)
//	[remainder] sealed payload: XChaCha20-Poly1305(gzip(JSONL events)) using watchword.SealWithAAD
//
// The header is plaintext on purpose so an operator with the file but
// not the scope key can still see which scope it belongs to and when
// it was minted; the events themselves stay encrypted. The verbatim
// on-wire header bytes are bound as AEAD additional-authenticated-data,
// so any post-seal tamper of the header (Device, Count, Encrypted,
// DEKEpoch, …) fails the auth tag on Open — the header is plaintext but
// not malleable.
package chunk

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// chunkIDEntropy provides strictly-monotonic 80-bit entropy for chunk IDs.
// The default ulid.New(ms, nil) path leaves the entropy bytes zeroed, so
// two chunks sealed within the same millisecond would collide on disk
// (and overwrite each other in the chunk store). Wrap crypto/rand in
// ulid.Monotonic to guarantee distinct IDs even under fast back-to-back
// seals while keeping seal-time-order = lex-order.
var (
	chunkIDEntropy = ulid.Monotonic(rand.Reader, 0) //nolint:gochecknoglobals // shared entropy source
	chunkIDMu      sync.Mutex                       //nolint:gochecknoglobals // serializes monotonic reader access
)

// Magic is the 4-byte file marker every sealed chunk begins with.
// Lets a reader fail fast on stray non-chunk files in the sync dir.
const Magic = "MAZ1"

// Version is the on-disk format version. Bump for incompatible
// header or framing changes; the reader rejects anything else.
//
// 0x02 binds the verbatim plaintext header bytes as AEAD AAD (header
// authentication) and adds the DEKEpoch header field for envelope-mode
// team scopes. This is a clean cut from 0x01 (Mazarbul is pre-prod, no
// back-compat window): 0x01-framed encrypted chunks are not readable by
// this build. Plaintext JSONL chunks are unaffected (they are not
// MAZ1-framed).
const Version byte = 0x02

// MaxHeaderBytes caps the plaintext header size. Defends against a
// hostile chunk that lies about its header length and tries to
// allocate gigabytes during decode.
const MaxHeaderBytes = 64 * 1024

// Extension is the canonical chunk filename suffix.
const Extension = ".maz"

// Header is the plaintext metadata block every chunk begins with.
// AIP naming (createTime; output-only fields) for consistency with
// the kernel event log surface.
//
// Device records which device sealed the chunk; surfaces in
// MANIFEST.json and `git blame`. Encrypted is the on-wire mode:
// true means MAZ1 framing + XChaCha20-Poly1305 sealed payload,
// false means a plaintext JSONL chunk (lines 2..N are events).
// Both fields use omitempty so existing pre-plaintext .maz files (which
// have neither) round-trip cleanly; decoders force the Encrypted
// invariant based on which framing they successfully parsed.
type Header struct {
	ChunkID    string       `json:"chunkId"`
	Scope      kernel.Scope `json:"scope"`
	Count      int          `json:"count"`
	FirstULID  string       `json:"firstUlid"`
	LastULID   string       `json:"lastUlid"`
	CreateTime time.Time    `json:"createTime"`
	Device     string       `json:"device,omitempty"`
	Encrypted  bool         `json:"encrypted,omitempty"`

	// DEKEpoch identifies which envelope DEK epoch sealed this chunk
	// (envelope-mode team scopes only; see internal/watchword). A pulling
	// device reads this from the plaintext header to select the matching
	// DEK from its local epoch ring before Open. omitempty so HKDF-keyed
	// and plaintext chunks (epoch is meaningless there) round-trip
	// unchanged. Bound as AAD, so it cannot be flipped post-seal.
	DEKEpoch int `json:"dekEpoch,omitempty"`
}

// Ref is a chunk-store list entry: the metadata needed to reason
// about chunks without decrypting their payloads.
type Ref struct {
	Header
	Path string // optional; filesystem store fills this in
}

// AuthorsMatchDevice reports an error if any event in the chunk was
// signed by a device other than the one the header claims sealed it. A
// chunk that advertises Device=X in its (AEAD-bound) header but carries
// events authored by device Y is rejected: the header is authenticated
// against tampering, but this binds the header's device claim to the
// per-event signed authorship so a chunk cannot misattribute its seal.
//
// A blank Header.Device is a no-op (legacy/plaintext chunks predate the
// device tag), and a blank ev.Author.DeviceID is skipped (an event
// written before EventAuthor existed and not yet re-stamped). The
// caller runs this at the replay boundary, where the device/principal
// tables and the signature verifier already live; Decode itself stays
// pure crypto with no identity dependency.
func (h Header) AuthorsMatchDevice(events []kernel.Event) error {
	if h.Device == "" {
		return nil
	}
	for i := range events {
		dev := events[i].Author.DeviceID
		if dev != "" && dev != h.Device {
			return fmt.Errorf("%w: header device %q, event %s signed by device %q",
				ErrDeviceMismatch, h.Device, events[i].UID, dev)
		}
	}
	return nil
}

// ErrBadMagic indicates the file does not begin with the expected
// 4-byte marker. Typically a stray file in the chunks dir.
var ErrBadMagic = errors.New("chunk: bad magic")

// ErrBadVersion indicates the file's format version is not what this
// build understands.
var ErrBadVersion = errors.New("chunk: unsupported format version")

// ErrCorruptHeader indicates the plaintext header could not be
// parsed (truncation, JSON syntax error, or length-prefix > MaxHeaderBytes).
var ErrCorruptHeader = errors.New("chunk: corrupt header")

// ErrScopeMismatch indicates the chunk's declared scope does not
// match the scope the caller is decrypting against. A symptom of
// either operator error (wrong key) or attacker substitution.
var ErrScopeMismatch = errors.New("chunk: scope mismatch")

// ErrDeviceMismatch indicates a chunk's authenticated Header.Device
// disagrees with the signed authorship of one of its events: the chunk
// claims one sealing device but carries events authored by another.
// Surfaced by Header.AuthorsMatchDevice at the replay boundary.
var ErrDeviceMismatch = errors.New("chunk: header device disagrees with event signer")

// ErrSensitivePayloadPlaintext indicates the batch contains an event a
// registered privacy marker flagged as sensitive, but the chunk is being
// encoded for a plaintext, non-project scope (no scope key). Fail-closed:
// the chunk is NOT written, so no plaintext chunk, commit message, or
// rendered file can ever leak the payload. Mirrors the
// encrypted-without-Watchword posture: never a silent drop, never a partial
// render.
//
// This is now a defence-in-depth guard for direct/non-sealer callers: the
// sealer skips a sensitive event before batching it for a permanently-plaintext
// scope (sealer.go skipPlaintextSensitive), so it never hands Encode a batch
// that would trip this error and wedge the scope's cursor. A caller that hits
// this directly must either encrypt the scope or drop the sensitive event.
var ErrSensitivePayloadPlaintext = errors.New("chunk: refuse to seal sensitive payload into a plaintext chunk")

// EncodeOpts bundles the parameters Encode needs. ScopeKey == nil
// selects the plaintext JSONL path; a 32-byte key selects MAZ1
// framing + XChaCha20-Poly1305. Device populates Header.Device
// (surfaces in MANIFEST.json + git blame); empty omits the field.
type EncodeOpts struct {
	Scope    kernel.Scope
	ScopeKey []byte
	Device   string

	// DEKEpoch stamps Header.DEKEpoch for envelope-mode team scopes so a
	// reader can select the matching DEK from its epoch ring. Ignored for
	// plaintext and HKDF-keyed chunks. Zero is the genesis epoch.
	DEKEpoch int
}

// Encode serializes events into a single sealed chunk. Caller
// supplies the scope key (derived from the operator's Watchword
// via watchword.DeriveScopeKey) and the scope name. Events with
// mismatched Scope are rejected so a bug in the sealer cannot leak
// data across scope boundaries.
//
// Returns the file bytes (ready to write or push) plus the Header
// for the caller's index. This signature is the original sealed-chunk contract;
// new callers that want plaintext output or a Device tag on the
// Header should use EncodeWithOpts.
func Encode(events []kernel.Event, scope kernel.Scope, scopeKey []byte) ([]byte, Header, error) {
	return EncodeWithOpts(events, EncodeOpts{Scope: scope, ScopeKey: scopeKey})
}

// EncodeWithOpts is the option-rich variant of Encode. With
// opts.ScopeKey nil it produces a plaintext JSONL chunk (the new
// plaintext default for fresh installs); with a 32-byte key it produces
// a MAZ1-framed encrypted chunk (the opt-in privacy mode). Events
// with mismatched Scope are rejected.
//
// Returns the file bytes (ready to write or push) plus the Header
// (with Encrypted set to match the chosen path) for the caller's
// index.
func EncodeWithOpts(events []kernel.Event, opts EncodeOpts) ([]byte, Header, error) {
	if len(events) == 0 {
		return nil, Header{}, errors.New("chunk: refuse to encode empty chunk")
	}
	if !opts.Scope.Valid() {
		return nil, Header{}, fmt.Errorf("chunk: invalid scope %q", opts.Scope)
	}
	sorted := sortByULID(events)
	for _, ev := range sorted {
		if ev.Scope != opts.Scope {
			return nil, Header{}, fmt.Errorf("chunk: event uid=%s scope=%q does not match chunk scope %q",
				ev.UID, ev.Scope, opts.Scope)
		}
	}
	hdr, err := newHeader(sorted, opts.Scope, opts.Device, opts.ScopeKey != nil, opts.DEKEpoch)
	if err != nil {
		return nil, Header{}, err
	}
	if opts.ScopeKey == nil {
		// Fail-closed: a sensitive event must never be written in
		// cleartext. Refusing here — before the chunk bytes exist —
		// means no plaintext chunk, commit message, or rendered file can
		// leak the payload. The sealer skips such an event before it
		// reaches this batch for a permanently-plaintext scope (see
		// sealer.go skipPlaintextSensitive), so this refusal is the
		// defence-in-depth guard for a direct caller; it never wedges the
		// sealer's cursor.
		if uid := firstSensitivePlaintext(sorted, opts.Scope); uid != "" {
			return nil, Header{}, fmt.Errorf("%w: event uid=%s scope=%q",
				ErrSensitivePayloadPlaintext, uid, opts.Scope)
		}
		blob, err := marshalPlaintextChunk(hdr, sorted)
		return blob, hdr, err
	}
	if len(opts.ScopeKey) != 32 {
		return nil, Header{}, fmt.Errorf("chunk: scope key must be 32 bytes, got %d", len(opts.ScopeKey))
	}
	blob, err := encodeSealed(hdr, sorted, opts.ScopeKey)
	return blob, hdr, err
}

// firstSensitivePlaintext returns the UID of the first event a registered
// privacy marker flags as sensitive when the batch is bound for a
// plaintext, non-project scope, or "" when the batch is safe to write in
// cleartext. Project is exempt: it is the intentionally public, in-repo
// scope and cannot be encrypted. Keeping the scan in a helper holds
// EncodeWithOpts under the complexity gate.
func firstSensitivePlaintext(events []kernel.Event, scope kernel.Scope) string {
	if scope == kernel.ScopeProject {
		return ""
	}
	for i := range events {
		if privacy.IsSensitivePayload(events[i]) {
			return events[i].UID
		}
	}
	return ""
}

// newHeader mints a chunk ID and assembles the Header common to
// both encoding modes. Encrypted == true sets Header.Encrypted so
// downstream consumers (FSStore.Put, MANIFEST writer) can pick
// the right extension without re-sniffing the blob.
func newHeader(sorted []kernel.Event, scope kernel.Scope, device string, encrypted bool, dekEpoch int) (Header, error) {
	chunkIDMu.Lock()
	id, err := ulid.New(ulid.Now(), chunkIDEntropy)
	chunkIDMu.Unlock()
	if err != nil {
		return Header{}, fmt.Errorf("chunk: mint chunk id: %w", err)
	}
	return Header{
		ChunkID:    id.String(),
		Scope:      scope,
		Count:      len(sorted),
		FirstULID:  sorted[0].UID,
		LastULID:   sorted[len(sorted)-1].UID,
		CreateTime: ulid.Time(id.Time()).UTC(),
		Device:     device,
		Encrypted:  encrypted,
		DEKEpoch:   dekEpoch,
	}, nil
}

// encodeSealed produces the MAZ1-framed, XChaCha20-Poly1305-sealed
// representation of the events. The plaintext header at the front
// of the file uses Encrypted=true so a reader that parses the
// header without the key still learns which mode the chunk uses.
func encodeSealed(hdr Header, sorted []kernel.Event, scopeKey []byte) ([]byte, error) {
	plaintext, err := jsonlBytes(sorted)
	if err != nil {
		return nil, err
	}
	compressed, err := gzipBytes(plaintext)
	if err != nil {
		return nil, err
	}
	// Marshal the header BEFORE sealing: the same bytes are bound as AEAD
	// AAD and written verbatim to the frame, so Open authenticates the
	// on-wire header and any post-seal tamper (Device/Count/Encrypted/
	// DEKEpoch/…) fails the auth tag.
	headerJSON, err := json.Marshal(hdr)
	if err != nil {
		return nil, fmt.Errorf("chunk: marshal header: %w", err)
	}
	if len(headerJSON) > MaxHeaderBytes {
		return nil, fmt.Errorf("chunk: header %d bytes exceeds cap %d", len(headerJSON), MaxHeaderBytes)
	}
	sealed, err := watchword.SealWithAAD(scopeKey, compressed, headerJSON)
	if err != nil {
		return nil, fmt.Errorf("chunk: seal: %w", err)
	}
	var out bytes.Buffer
	out.WriteString(Magic)
	out.WriteByte(Version)
	if err := binary.Write(&out, binary.BigEndian, uint16(len(headerJSON))); err != nil {
		return nil, fmt.Errorf("chunk: write header length: %w", err)
	}
	out.Write(headerJSON)
	out.Write(sealed)
	return out.Bytes(), nil
}

// Decode parses a chunk and returns the Header plus the event slice.
// Routes on the leading 4 bytes: MAZ1 framing → encrypted path
// (scopeKey required); otherwise → plaintext JSONL path (scopeKey
// ignored). Validates the chunk's declared scope matches
// expectedScope (defence against an attacker swapping in a chunk
// from a different scope) and, for the encrypted path, that the
// AEAD tag verifies.
func Decode(blob []byte, expectedScope kernel.Scope, scopeKey []byte) (Header, []kernel.Event, error) {
	switch {
	case looksLikeMAZ1(blob):
		// fall through to framed-decode path below
	case looksLikePlaintextChunk(blob):
		return DecodePlaintext(blob, expectedScope)
	default:
		return Header{}, nil, fmt.Errorf("%w: blob is neither MAZ1-framed nor JSONL plaintext", ErrBadMagic)
	}
	hdr, headerBytes, payload, err := parseFrame(blob)
	if err != nil {
		return Header{}, nil, err
	}
	if expectedScope != "" && hdr.Scope != expectedScope {
		return Header{}, nil, fmt.Errorf("%w: header says %q, expected %q",
			ErrScopeMismatch, hdr.Scope, expectedScope)
	}
	if len(scopeKey) != 32 {
		return Header{}, nil, fmt.Errorf("chunk: scope key must be 32 bytes, got %d", len(scopeKey))
	}
	// AAD = the verbatim on-wire header bytes; a flipped header field
	// fails the auth tag here rather than silently decoding a lie.
	compressed, err := watchword.OpenWithAAD(scopeKey, payload, headerBytes)
	if err != nil {
		return Header{}, nil, fmt.Errorf("chunk: open: %w", err)
	}
	plaintext, err := gunzipBytes(compressed)
	if err != nil {
		return Header{}, nil, fmt.Errorf("chunk: gunzip: %w", err)
	}
	hdr.Encrypted = true
	events, err := parseJSONL(plaintext)
	if err != nil {
		return Header{}, nil, err
	}
	if len(events) != hdr.Count {
		return Header{}, nil, fmt.Errorf("chunk: header says count=%d, payload has %d events",
			hdr.Count, len(events))
	}
	return hdr, events, nil
}

// PeekHeader returns just the plaintext header without decrypting
// (or fully parsing) the payload. Routes on the leading 4 bytes:
// MAZ1 framing → parse framed header; otherwise → parse JSONL
// _meta line. Useful for indexing a sync directory before scope
// keys are loaded or for surfacing chunk metadata in the TUI.
func PeekHeader(blob []byte) (Header, error) {
	switch {
	case looksLikeMAZ1(blob):
		hdr, _, _, err := parseFrame(blob)
		if err != nil {
			return Header{}, err
		}
		hdr.Encrypted = true
		return hdr, nil
	case looksLikePlaintextChunk(blob):
		return PeekHeaderJSONL(blob)
	default:
		return Header{}, fmt.Errorf("%w: blob is neither MAZ1-framed nor JSONL plaintext", ErrBadMagic)
	}
}

// PathFor returns the canonical filesystem path of a chunk inside
// dataDir. Layout: <dataDir>/sync/<scope>/chunks/<chunkID>.maz.
func PathFor(dataDir string, scope kernel.Scope, chunkID string) string {
	return filepath.Join(dataDir, "sync", string(scope), "chunks", chunkID+Extension)
}

// DirFor returns the canonical chunks directory for a scope.
func DirFor(dataDir string, scope kernel.Scope) string {
	return filepath.Join(dataDir, "sync", string(scope), "chunks")
}

// parseFrame splits a MAZ1 frame into its parsed header, the VERBATIM
// on-wire header bytes (the AEAD AAD — callers must pass these, not a
// re-marshal of hdr, to OpenWithAAD), and the sealed payload.
func parseFrame(blob []byte) (hdr Header, headerBytes, payload []byte, err error) {
	const fixedPrefix = len(Magic) + 1 + 2 // magic + version + headerLen
	if len(blob) < fixedPrefix {
		return Header{}, nil, nil, fmt.Errorf("%w: file too short (%d bytes)", ErrCorruptHeader, len(blob))
	}
	if string(blob[:len(Magic)]) != Magic {
		return Header{}, nil, nil, fmt.Errorf("%w: got %q", ErrBadMagic, blob[:len(Magic)])
	}
	if blob[len(Magic)] != Version {
		return Header{}, nil, nil, fmt.Errorf("%w: got 0x%02x, want 0x%02x", ErrBadVersion, blob[len(Magic)], Version)
	}
	hdrLen := int(binary.BigEndian.Uint16(blob[len(Magic)+1 : fixedPrefix]))
	if hdrLen == 0 || hdrLen > MaxHeaderBytes {
		return Header{}, nil, nil, fmt.Errorf("%w: header length %d out of range", ErrCorruptHeader, hdrLen)
	}
	if len(blob) < fixedPrefix+hdrLen {
		return Header{}, nil, nil, fmt.Errorf("%w: header truncated", ErrCorruptHeader)
	}
	headerBytes = blob[fixedPrefix : fixedPrefix+hdrLen]
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return Header{}, nil, nil, fmt.Errorf("%w: %w", ErrCorruptHeader, err)
	}
	payload = blob[fixedPrefix+hdrLen:]
	return hdr, headerBytes, payload, nil
}

func jsonlBytes(events []kernel.Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return nil, fmt.Errorf("chunk: encode event %s: %w", ev.UID, err)
		}
	}
	return buf.Bytes(), nil
}

func parseJSONL(b []byte) ([]kernel.Event, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	out := make([]kernel.Event, 0, 32)
	for dec.More() {
		var ev kernel.Event
		if err := dec.Decode(&ev); err != nil {
			return nil, fmt.Errorf("chunk: decode event #%d: %w", len(out), err)
		}
		out = append(out, ev)
	}
	return out, nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, fmt.Errorf("chunk: gzip write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("chunk: gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

func gunzipBytes(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("chunk: gzip open: %w", err)
	}
	defer closeGzipReader(r)
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("chunk: gzip read: %w", err)
	}
	return out, nil
}

func closeGzipReader(r *gzip.Reader) {
	// Best-effort: a Close error after ReadAll has already drained
	// the stream signals at most a trailing trailer issue we cannot
	// recover from at the call site. The caller has the bytes.
	if cerr := r.Close(); cerr != nil {
		_ = cerr
	}
}

func sortByULID(events []kernel.Event) []kernel.Event {
	out := make([]kernel.Event, len(events))
	copy(out, events)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}
