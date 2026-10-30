package chunk_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// monoEntropy generates strictly-increasing ULIDs even within the
// same millisecond, sidestepping the non-monotonic default of
// ulid.New(ulid.Now(), nil). Package-level so every helper observes the
// same counter and tests can rely on insertion-order == ULID-sort-order.
var (
	monoEntropy = ulid.Monotonic(rand.NewChaCha8([32]byte{1, 2, 3}), 0)
	monoMu      sync.Mutex
)

func sampleEvent(t *testing.T, scope kernel.Scope, text string) kernel.Event {
	t.Helper()
	monoMu.Lock()
	id, err := ulid.New(ulid.Now(), monoEntropy)
	monoMu.Unlock()
	if err != nil {
		t.Fatalf("mint ulid: %v", err)
	}
	payload, _ := json.Marshal(map[string]string{"text": text})
	return kernel.Event{
		Name:       "events/" + id.String(),
		UID:        id.String(),
		CreateTime: time.Now().UTC(),
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      scope,
		Payload:    payload,
	}
}

func deriveKey(t *testing.T) []byte {
	t.Helper()
	// Construct the all-zeros BIP-39 reference mnemonic programmatically
	// so source-rewriting tools cannot silently collapse the repeated
	// tokens (and so the test is self-documenting about which vector
	// it uses).
	parts := make([]string, watchword.Words)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[len(parts)-1] = "art"
	mnemonic := strings.Join(parts, " ")
	root, err := watchword.DeriveRoot(mnemonic)
	if err != nil {
		t.Fatalf("DeriveRoot: %v", err)
	}
	key, err := watchword.DeriveScopeKey(root, "personal")
	if err != nil {
		t.Fatalf("DeriveScopeKey: %v", err)
	}
	return key
}

func TestEncodeDecode_Roundtrip(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	events := []kernel.Event{
		sampleEvent(t, kernel.ScopePersonal, "first"),
		sampleEvent(t, kernel.ScopePersonal, "second"),
		sampleEvent(t, kernel.ScopePersonal, "third"),
	}
	blob, hdr, err := chunk.Encode(events, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if hdr.Count != len(events) {
		t.Errorf("hdr.Count = %d, want %d", hdr.Count, len(events))
	}
	if hdr.Scope != kernel.ScopePersonal {
		t.Errorf("hdr.Scope = %q, want personal", hdr.Scope)
	}
	if hdr.ChunkID == "" {
		t.Errorf("hdr.ChunkID is empty")
	}

	gotHdr, gotEvents, err := chunk.Decode(blob, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if gotHdr.ChunkID != hdr.ChunkID {
		t.Errorf("ChunkID mismatch on round-trip")
	}
	if len(gotEvents) != len(events) {
		t.Fatalf("event count mismatch: got %d, want %d", len(gotEvents), len(events))
	}
	for i, ev := range gotEvents {
		if ev.UID != events[i].UID {
			t.Errorf("event[%d].UID mismatch: %q != %q", i, ev.UID, events[i].UID)
		}
		if ev.Scope != events[i].Scope {
			t.Errorf("event[%d].Scope mismatch: %q != %q", i, ev.Scope, events[i].Scope)
		}
		if string(ev.Payload) != string(events[i].Payload) {
			t.Errorf("event[%d].Payload mismatch", i)
		}
	}
}

func TestEncode_ChunkIDsAreUniqueAcrossRapidSeals(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	seen := make(map[string]bool, 50)
	for i := range 50 {
		ev := sampleEvent(t, kernel.ScopePersonal, fmt.Sprintf("ev-%d", i))
		_, hdr, err := chunk.Encode([]kernel.Event{ev}, kernel.ScopePersonal, key)
		if err != nil {
			t.Fatalf("Encode %d: %v", i, err)
		}
		if seen[hdr.ChunkID] {
			t.Fatalf("ChunkID collision at iteration %d: %q", i, hdr.ChunkID)
		}
		seen[hdr.ChunkID] = true
	}
}

func TestEncode_RejectsCrossScope(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	events := []kernel.Event{
		sampleEvent(t, kernel.ScopePersonal, "a"),
		sampleEvent(t, kernel.ScopeTeam, "b"), // wrong scope
	}
	_, _, err := chunk.Encode(events, kernel.ScopePersonal, key)
	if err == nil {
		t.Fatalf("Encode must reject mixed-scope batch")
	}
}

func TestEncode_RejectsEmpty(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	_, _, err := chunk.Encode(nil, kernel.ScopePersonal, key)
	if err == nil {
		t.Fatalf("Encode must reject empty batch")
	}
}

func TestEncode_RejectsBadScope(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	_, _, err := chunk.Encode([]kernel.Event{sampleEvent(t, "organization", "x")}, "organization", key)
	if err == nil {
		t.Fatalf("Encode must reject invalid scope")
	}
}

func TestDecode_FailsOnScopeMismatch(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "a")}
	blob, _, err := chunk.Encode(events, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	_, _, err = chunk.Decode(blob, kernel.ScopeTeam, key)
	if err == nil {
		t.Fatalf("Decode must fail when expected scope differs from header")
	}
	if !errors.Is(err, chunk.ErrScopeMismatch) {
		t.Errorf("err = %v, want errors.Is ErrScopeMismatch", err)
	}
}

func TestDecode_FailsOnWrongKey(t *testing.T) {
	t.Parallel()
	k1 := deriveKey(t)
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "a")}
	blob, _, err := chunk.Encode(events, kernel.ScopePersonal, k1)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	k2 := bytes.Repeat([]byte{0xAA}, 32)
	_, _, err = chunk.Decode(blob, kernel.ScopePersonal, k2)
	if err == nil {
		t.Fatalf("Decode must fail with wrong key")
	}
}

func TestDecode_FailsOnTamper(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "a")}
	blob, _, err := chunk.Encode(events, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Flip a byte in the ciphertext region.
	blob[len(blob)-1] ^= 0x01
	_, _, err = chunk.Decode(blob, kernel.ScopePersonal, key)
	if err == nil {
		t.Fatalf("Decode must reject tampered ciphertext")
	}
}

// reframeWithHeader rebuilds a MAZ1 frame, swapping in newHdr's bytes
// while keeping the ORIGINAL sealed payload. It is the test seam for
// "the header claims X but the payload was sealed under Y": Decode must
// reject it because the header bytes are bound as AEAD AAD.
func reframeWithHeader(t *testing.T, origBlob []byte, newHdr chunk.Header) []byte {
	t.Helper()
	const prefix = len("MAZ1") + 1 + 2
	hdrLen := int(binary.BigEndian.Uint16(origBlob[5:prefix]))
	payload := origBlob[prefix+hdrLen:]
	hj, err := json.Marshal(newHdr)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	var out bytes.Buffer
	out.WriteString(chunk.Magic)
	out.WriteByte(chunk.Version)
	if err := binary.Write(&out, binary.BigEndian, uint16(len(hj))); err != nil {
		t.Fatalf("write hdrlen: %v", err)
	}
	out.Write(hj)
	out.Write(payload)
	return out.Bytes()
}

func TestDecode_FailsOnHeaderTamper(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	events := []kernel.Event{sampleEvent(t, kernel.ScopeTeam, "a"), sampleEvent(t, kernel.ScopeTeam, "b")}
	blob, hdr, err := chunk.EncodeWithOpts(events, chunk.EncodeOpts{
		Scope: kernel.ScopeTeam, ScopeKey: key, Device: "device-aaaa", DEKEpoch: 2,
	})
	if err != nil {
		t.Fatalf("EncodeWithOpts: %v", err)
	}
	// Sanity: the untampered chunk decodes.
	if _, _, derr := chunk.Decode(blob, kernel.ScopeTeam, key); derr != nil {
		t.Fatalf("untampered Decode: %v", derr)
	}
	mutators := map[string]func(h chunk.Header) chunk.Header{
		"device":    func(h chunk.Header) chunk.Header { h.Device = "device-bbbb"; return h },
		"count":     func(h chunk.Header) chunk.Header { h.Count = h.Count + 1; return h },
		"encrypted": func(h chunk.Header) chunk.Header { h.Encrypted = false; return h },
		"dekEpoch":  func(h chunk.Header) chunk.Header { h.DEKEpoch = 99; return h },
	}
	for name, mut := range mutators {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tampered := reframeWithHeader(t, blob, mut(hdr))
			if _, _, derr := chunk.Decode(tampered, kernel.ScopeTeam, key); derr == nil {
				t.Fatalf("Decode must reject a chunk whose %s header field was altered post-seal", name)
			}
		})
	}
}

func TestAuthorsMatchDevice(t *testing.T) {
	t.Parallel()
	ev := func(dev string) kernel.Event {
		e := sampleEvent(t, kernel.ScopeTeam, "x")
		e.Author = kernel.EventAuthor{PrincipalUID: "p1", DeviceID: dev, SigKeyID: "kid:1"}
		return e
	}
	cases := []struct {
		name    string
		device  string
		events  []kernel.Event
		wantErr bool
	}{
		{"match", "dev-A", []kernel.Event{ev("dev-A"), ev("dev-A")}, false},
		{"mismatch", "dev-A", []kernel.Event{ev("dev-A"), ev("dev-B")}, true},
		{"blank header device is no-op", "", []kernel.Event{ev("dev-A")}, false},
		{"blank event device is skipped", "dev-A", []kernel.Event{ev("")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := chunk.Header{Device: tc.device}
			err := h.AuthorsMatchDevice(tc.events)
			if tc.wantErr {
				if !errors.Is(err, chunk.ErrDeviceMismatch) {
					t.Errorf("err = %v, want errors.Is ErrDeviceMismatch", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
		})
	}
}

func TestPeekHeader_NoDecrypt(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "a")}
	blob, hdr, err := chunk.Encode(events, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	peeked, err := chunk.PeekHeader(blob)
	if err != nil {
		t.Fatalf("PeekHeader: %v", err)
	}
	if peeked.ChunkID != hdr.ChunkID {
		t.Errorf("PeekHeader.ChunkID = %q, want %q", peeked.ChunkID, hdr.ChunkID)
	}
}

func TestPeekHeader_RejectsBadMagic(t *testing.T) {
	t.Parallel()
	_, err := chunk.PeekHeader([]byte("not-a-chunk-file"))
	if err == nil {
		t.Fatalf("PeekHeader must reject non-magic input")
	}
	if !errors.Is(err, chunk.ErrBadMagic) {
		t.Errorf("err = %v, want errors.Is ErrBadMagic", err)
	}
}

func TestFSStore_PutGetList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := chunk.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	key := deriveKey(t)

	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "one")}
	blob, hdr, err := chunk.Encode(events, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := store.Put(ctx, kernel.ScopePersonal, hdr, blob); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, kernel.ScopePersonal, hdr.ChunkID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("Get returned %d bytes, want %d", len(got), len(blob))
	}
	refs, err := store.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("List len = %d, want 1", len(refs))
	}
	if refs[0].ChunkID != hdr.ChunkID {
		t.Errorf("refs[0].ChunkID = %q, want %q", refs[0].ChunkID, hdr.ChunkID)
	}

	// Path layout invariant.
	wantPath := filepath.Join(dir, "sync", "personal", "chunks", hdr.ChunkID+chunk.Extension)
	if refs[0].Path != wantPath {
		t.Errorf("refs[0].Path = %q, want %q", refs[0].Path, wantPath)
	}
}

// TestFSStore_List_SkipsLegacyAndStrayChunks pins the sync-resilience fix: a
// single unreadable chunk must NOT abort the whole local listing. The user hit
// `maz sync push/pull` bricking entirely ("unsupported format version: got
// 0x01, want 0x02") on one leftover pre-0x02 chunk. Because chunks are derived
// transport artifacts whose events stay durable in the event log, List skips a
// legacy 0x01 MAZ1 chunk (clean-cut, deliberately unreadable) and a stray file
// with a chunk extension (bad magic), returning the readable chunks instead of
// failing the operation.
func TestFSStore_List_SkipsLegacyAndStrayChunks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := chunk.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	// A readable plaintext chunk — the one List must still return.
	good := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "keep me")}
	blob, hdr, err := chunk.EncodeWithOpts(good, chunk.EncodeOpts{Scope: kernel.ScopePersonal})
	if err != nil {
		t.Fatalf("EncodeWithOpts: %v", err)
	}
	if err := store.Put(ctx, kernel.ScopePersonal, hdr, blob); err != nil {
		t.Fatalf("Put: %v", err)
	}

	chunksDir := chunk.DirFor(dir, kernel.ScopePersonal)

	// A legacy 0x01 MAZ1-framed chunk (clean-cut: unreadable by this build →
	// ErrBadVersion). "MAZ1" + version 0x01 + headerLen 5 + 5 filler bytes.
	legacy := append([]byte(chunk.Magic), 0x01, 0x00, 0x05, 'x', 'x', 'x', 'x', 'x')
	if werr := os.WriteFile(filepath.Join(chunksDir, "01LEGACYLEGACYLEGACYLEGAC0"+chunk.Extension), legacy, 0o600); werr != nil {
		t.Fatalf("write legacy chunk: %v", werr)
	}

	// A stray file with a chunk extension that is neither MAZ1 nor JSONL
	// (ErrBadMagic).
	if werr := os.WriteFile(filepath.Join(chunksDir, "stray"+chunk.Extension), []byte("not a chunk"), 0o600); werr != nil {
		t.Fatalf("write stray file: %v", werr)
	}

	refs, err := store.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List must skip unreadable legacy/stray chunks, not fail: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("List len = %d, want 1 (only the readable chunk)", len(refs))
	}
	if refs[0].ChunkID != hdr.ChunkID {
		t.Errorf("refs[0].ChunkID = %q, want %q", refs[0].ChunkID, hdr.ChunkID)
	}
}

func TestFSStore_GetMissingReturnsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := chunk.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	_, err = store.Get(ctx, kernel.ScopePersonal, "01HKJZNB3T0000000000000000")
	if err == nil {
		t.Fatalf("Get must fail on missing chunk")
	}
	if !errors.Is(err, chunk.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is ErrNotFound", err)
	}
}

// memCursor is a simple in-memory CursorStore for sealer tests.
type memCursor struct {
	mu     sync.Mutex
	sealed map[kernel.Scope]map[string]struct{}
}

func newMemCursor() *memCursor {
	return &memCursor{sealed: map[kernel.Scope]map[string]struct{}{}}
}

func (m *memCursor) Sealed(_ context.Context, scope kernel.Scope) (map[string]struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	got, ok := m.sealed[scope]
	if !ok {
		return map[string]struct{}{}, nil
	}
	// Return a copy so the sealer's mutations don't surprise the next
	// test setup that grabs the same memCursor.
	cp := make(map[string]struct{}, len(got))
	for k := range got {
		cp[k] = struct{}{}
	}
	return cp, nil
}

func (m *memCursor) MarkSealed(_ context.Context, scope kernel.Scope, uids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed[scope] == nil {
		m.sealed[scope] = map[string]struct{}{}
	}
	for _, u := range uids {
		m.sealed[scope][u] = struct{}{}
	}
	return nil
}

// listSource is an EventSource backed by an in-memory slice.
type listSource struct {
	events []kernel.Event
}

func (s *listSource) Read(_ context.Context, _ kernel.ReadOpts) iter.Seq2[kernel.Event, error] {
	return func(yield func(kernel.Event, error) bool) {
		for _, ev := range s.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func TestSealOnce_BasicFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := deriveKey(t)
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	src := &listSource{events: []kernel.Event{
		sampleEvent(t, kernel.ScopePersonal, "a"),
		sampleEvent(t, kernel.ScopePersonal, "b"),
		sampleEvent(t, kernel.ScopeProject, "skipme"),
		sampleEvent(t, kernel.ScopePersonal, "c"),
	}}

	n, last, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, key, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("chunksWritten = %d, want 1", n)
	}
	if last == "" {
		t.Errorf("newCursor is empty")
	}

	// Run again: idempotent, no new chunks.
	n2, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, key, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce again: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent re-run wrote %d chunks, want 0", n2)
	}

	// List should show our one chunk.
	refs, err := store.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("List len = %d, want 1", len(refs))
	}
	if refs[0].Count != 3 { // 3 personal events, 1 project skipped
		t.Errorf("chunk event count = %d, want 3", refs[0].Count)
	}
}

func TestSealOnce_RespectsBatchSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := deriveKey(t)
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	events := make([]kernel.Event, 0, 25)
	for range 25 {
		events = append(events, sampleEvent(t, kernel.ScopePersonal, "e"))
		time.Sleep(time.Millisecond) // ensure distinct ULIDs
	}
	src := &listSource{events: events}

	n, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, key, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce: %v", err)
	}
	if n != 3 { // 10 + 10 + 5
		t.Errorf("chunksWritten = %d, want 3", n)
	}
}

func TestSealOnce_AdvancesCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := deriveKey(t)
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	first := []kernel.Event{
		sampleEvent(t, kernel.ScopePersonal, "a"),
		sampleEvent(t, kernel.ScopePersonal, "b"),
	}
	src := &listSource{events: first}
	_, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, key, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce first: %v", err)
	}
	sealed, _ := cursor.Sealed(ctx, kernel.ScopePersonal)
	if len(sealed) == 0 {
		t.Fatalf("cursor not advanced after first seal")
	}

	// Add new events; SealOnce should only seal the new ones.
	src.events = append(src.events, sampleEvent(t, kernel.ScopePersonal, "c"))
	n, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, key, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce second: %v", err)
	}
	if n != 1 {
		t.Errorf("second seal wrote %d chunks, want 1 (only the new event)", n)
	}
	refs, _ := store.List(ctx, kernel.ScopePersonal)
	if len(refs) != 2 {
		t.Errorf("total chunks = %d, want 2", len(refs))
	}
	if refs[1].Count != 1 {
		t.Errorf("second chunk event count = %d, want 1", refs[1].Count)
	}
}

// TestSealOnce_SealsBackfillEventWithOlderUID is the regression test
// for Greptile finding #1: the cursor used to be a single high-water
// UID, so a later-appended event with an OLDER UID (caller-supplied
// CreateTime, replayed import, late connector backfill) was silently
// dropped on every subsequent seal. The set-membership cursor seals
// such events because membership is checked per-UID, not against a
// watermark.
func TestSealOnce_SealsBackfillEventWithOlderUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := deriveKey(t)
	store := chunk.NewMemStore()
	cursor := newMemCursor()

	// Round 1: seal two events with high ULIDs.
	high1 := sampleEvent(t, kernel.ScopePersonal, "high-a")
	high2 := sampleEvent(t, kernel.ScopePersonal, "high-b")
	src := &listSource{events: []kernel.Event{high1, high2}}
	if _, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, key, store, cursor, 10); err != nil {
		t.Fatalf("SealOnce 1: %v", err)
	}

	// Round 2: a backfill event arrives with an OLDER ULID than the
	// previously-sealed pair. Constructed by hand so the UID is
	// guaranteed to sort below high1/high2. With the prior
	// watermark-based filter this event would be dropped forever.
	backfill := high1
	backfill.UID = "01H0000000000000000000BACKF" // lex-smaller than 01K...
	backfill.Name = "events/" + backfill.UID

	src.events = append(src.events, backfill)
	n, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, key, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce 2: %v", err)
	}
	if n != 1 {
		t.Errorf("second seal wrote %d chunks, want 1 (backfill must be sealed, not dropped)", n)
	}
}
