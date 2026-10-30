package chunk_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// connectorEvent reuses sampleEvent's ULID minting but tags the event
// with a connector:* source so the privacy seed marker flags it.
func connectorEvent(t *testing.T, scope kernel.Scope) kernel.Event {
	t.Helper()
	e := sampleEvent(t, scope, "external-transcript")
	e.Source = "connector:granola"
	return e
}

// TestEncodeWithOpts_PlaintextSensitive_Blocked pins acceptance #1 at the
// encode boundary: a sensitive event bound for a plaintext scope produces
// no chunk bytes and the sentinel error — blocked, not silently dropped.
func TestEncodeWithOpts_PlaintextSensitive_Blocked(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{connectorEvent(t, kernel.ScopePersonal)}
	blob, _, err := chunk.EncodeWithOpts(events, chunk.EncodeOpts{Scope: kernel.ScopePersonal, ScopeKey: nil})
	if !errors.Is(err, chunk.ErrSensitivePayloadPlaintext) {
		t.Fatalf("err = %v, want ErrSensitivePayloadPlaintext", err)
	}
	if blob != nil {
		t.Errorf("blob = %d bytes, want nil (nothing encoded)", len(blob))
	}
}

// TestEncodeWithOpts_PlaintextMixedBatch_RefusesWholeBatch pins the
// fail-the-whole-pass decision: one sensitive event refuses the entire
// batch, including the innocent events sharing it.
func TestEncodeWithOpts_PlaintextMixedBatch_RefusesWholeBatch(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{
		sampleEvent(t, kernel.ScopePersonal, "innocent-1"),
		connectorEvent(t, kernel.ScopePersonal),
		sampleEvent(t, kernel.ScopePersonal, "innocent-2"),
	}
	blob, _, err := chunk.EncodeWithOpts(events, chunk.EncodeOpts{Scope: kernel.ScopePersonal})
	if !errors.Is(err, chunk.ErrSensitivePayloadPlaintext) {
		t.Fatalf("err = %v, want ErrSensitivePayloadPlaintext (whole batch refused)", err)
	}
	if blob != nil {
		t.Errorf("blob = %d bytes, want nil", len(blob))
	}
}

// TestEncodeWithOpts_ExplicitFlag_Blocked proves the opt-in payload flag
// also trips the plaintext gate.
func TestEncodeWithOpts_ExplicitFlag_Blocked(t *testing.T) {
	t.Parallel()
	e := sampleEvent(t, kernel.ScopePersonal, "x")
	e.Payload = []byte(`{"sensitive":true,"text":"x"}`)
	_, _, err := chunk.EncodeWithOpts([]kernel.Event{e}, chunk.EncodeOpts{Scope: kernel.ScopePersonal})
	if !errors.Is(err, chunk.ErrSensitivePayloadPlaintext) {
		t.Fatalf("err = %v, want ErrSensitivePayloadPlaintext (explicit flag)", err)
	}
}

// TestEncodeWithOpts_PlaintextNonSensitive_Seals is the positive control:
// normal cli:/mcp:-sourced events must still seal plaintext untouched, so
// the seed markers cannot regress the default plaintext-sync flow.
func TestEncodeWithOpts_PlaintextNonSensitive_Seals(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"cli:remember", "mcp:fact", "extractor:m1", "document:import"} {
		e := sampleEvent(t, kernel.ScopePersonal, "ordinary note")
		e.Source = src
		blob, hdr, err := chunk.EncodeWithOpts([]kernel.Event{e}, chunk.EncodeOpts{Scope: kernel.ScopePersonal})
		if err != nil {
			t.Fatalf("source %q: EncodeWithOpts err = %v, want nil (normal events must seal)", src, err)
		}
		if hdr.Encrypted {
			t.Errorf("source %q: hdr.Encrypted = true, want false", src)
		}
		if len(blob) == 0 {
			t.Errorf("source %q: blob empty, want plaintext bytes", src)
		}
	}
}

// TestEncodeWithOpts_PlaintextProjectScope_Exempt pins the project
// exemption: the intentionally-public scope encodes sensitive events in
// cleartext without blocking.
func TestEncodeWithOpts_PlaintextProjectScope_Exempt(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{connectorEvent(t, kernel.ScopeProject)}
	blob, hdr, err := chunk.EncodeWithOpts(events, chunk.EncodeOpts{Scope: kernel.ScopeProject})
	if err != nil {
		t.Fatalf("project scope must be exempt, got err = %v", err)
	}
	if hdr.Encrypted {
		t.Errorf("hdr.Encrypted = true, want false (plaintext project)")
	}
	if len(blob) == 0 {
		t.Errorf("blob empty, want plaintext chunk bytes")
	}
}

// TestEncodeWithOpts_EncryptedSensitive_Seals pins acceptance #3:
// encrypted scopes seal sensitive payloads normally.
func TestEncodeWithOpts_EncryptedSensitive_Seals(t *testing.T) {
	t.Parallel()
	key := deriveKey(t)
	events := []kernel.Event{connectorEvent(t, kernel.ScopePersonal)}
	blob, hdr, err := chunk.EncodeWithOpts(events, chunk.EncodeOpts{Scope: kernel.ScopePersonal, ScopeKey: key})
	if err != nil {
		t.Fatalf("encrypted scope must seal sensitive payloads, got err = %v", err)
	}
	if !hdr.Encrypted {
		t.Errorf("hdr.Encrypted = false, want true (encrypted path)")
	}
	if len(blob) == 0 {
		t.Errorf("blob empty, want sealed bytes")
	}
}

// TestSealOnce_PlaintextSensitive_SkipsSensitiveSealsRest is the end-to-end
// acceptance for the skip-not-wedge fix: a sensitive event in a
// permanently-plaintext scope is skipped (never sealed to a plaintext chunk),
// while the innocent events in the same pass DO seal and the cursor advances
// past them. Previously this wedged the whole scope — the sensitive event
// failed the encode, the cursor never advanced, and every following event
// stalled behind it indefinitely (personal never acquires a scope key).
func TestSealOnce_PlaintextSensitive_SkipsSensitiveSealsRest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	ok1 := sampleEvent(t, kernel.ScopePersonal, "ok-1")
	sensitive := connectorEvent(t, kernel.ScopePersonal)
	ok2 := sampleEvent(t, kernel.ScopePersonal, "ok-2")
	src := &listSource{events: []kernel.Event{ok1, sensitive, ok2}}

	// nil scopeKey selects the plaintext path.
	n, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, nil, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce err = %v, want nil (sensitive event skipped, not wedging the pass)", err)
	}
	if n != 1 {
		t.Errorf("chunksWritten = %d, want 1 (the two innocent events seal into one chunk)", n)
	}

	refs, lerr := store.List(ctx, kernel.ScopePersonal)
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(refs) != 1 {
		t.Errorf("store has %d chunk(s), want 1 (innocent events sealed)", len(refs))
	}

	sealed, serr := cursor.Sealed(ctx, kernel.ScopePersonal)
	if serr != nil {
		t.Fatalf("Sealed: %v", serr)
	}
	if _, ok := sealed[sensitive.UID]; ok {
		t.Errorf("sensitive uid=%s is sealed, want skipped (never written to a plaintext chunk)", sensitive.UID)
	}
	if _, ok := sealed[ok1.UID]; !ok {
		t.Errorf("innocent uid=%s not sealed, want sealed", ok1.UID)
	}
	if _, ok := sealed[ok2.UID]; !ok {
		t.Errorf("innocent uid=%s not sealed, want sealed", ok2.UID)
	}
}

// TestSealOnce_PlaintextAllSensitive_NoErrorNoChunk proves an all-sensitive
// pass is a clean no-op, not an error: every event is skipped, nothing seals,
// and the cursor stays put — so a scope holding only sensitive events never
// wedges and never spins a stuck-push loop.
func TestSealOnce_PlaintextAllSensitive_NoErrorNoChunk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	src := &listSource{events: []kernel.Event{
		connectorEvent(t, kernel.ScopePersonal),
		connectorEvent(t, kernel.ScopePersonal),
	}}

	n, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, nil, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce err = %v, want nil (all-sensitive pass is a no-op, not a wedge)", err)
	}
	if n != 0 {
		t.Errorf("chunksWritten = %d, want 0 (nothing to seal)", n)
	}
	refs, lerr := store.List(ctx, kernel.ScopePersonal)
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(refs) != 0 {
		t.Errorf("store has %d chunk(s), want 0 (all events skipped)", len(refs))
	}
}

// TestSealOnce_EncryptedSensitive_Seals is the negative control for the skip:
// with a scope key the plaintext gate does not apply, so a sensitive event
// seals normally inside the encrypted envelope. The sealer's skip must not
// over-reach into encrypted scopes.
func TestSealOnce_EncryptedSensitive_Seals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	sensitive := connectorEvent(t, kernel.ScopePersonal)
	src := &listSource{events: []kernel.Event{sensitive}}

	n, _, err := chunk.SealOnce(ctx, src, kernel.ScopePersonal, deriveKey(t), store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce (encrypted) err = %v, want nil (encrypted scopes seal sensitive payloads)", err)
	}
	if n != 1 {
		t.Errorf("chunksWritten = %d, want 1 (sensitive event seals inside the envelope)", n)
	}
	sealed, serr := cursor.Sealed(ctx, kernel.ScopePersonal)
	if serr != nil {
		t.Fatalf("Sealed: %v", serr)
	}
	if _, ok := sealed[sensitive.UID]; !ok {
		t.Errorf("sensitive uid=%s not sealed, want sealed (encrypted path)", sensitive.UID)
	}
}

// TestSealOnce_PlaintextProjectSensitive_Seals confirms the project
// exemption end-to-end: a sensitive event in project scope seals to a
// plaintext chunk without blocking.
func TestSealOnce_PlaintextProjectSensitive_Seals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := chunk.NewMemStore()
	cursor := newMemCursor()
	src := &listSource{events: []kernel.Event{connectorEvent(t, kernel.ScopeProject)}}

	n, _, err := chunk.SealOnce(ctx, src, kernel.ScopeProject, nil, store, cursor, 10)
	if err != nil {
		t.Fatalf("SealOnce (project exempt): %v", err)
	}
	if n != 1 {
		t.Errorf("chunksWritten = %d, want 1 (project is exempt)", n)
	}
}
