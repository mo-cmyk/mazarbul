package kernel_test

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/hlc"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestPrepare_StampsHLC verifies that Append stamps a non-empty HLC on every
// event, that the stamp is a valid 16-char hex string, and that successive
// events within the same millisecond have strictly increasing HLC stamps.
func TestPrepare_StampsHLC(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	fixedMs := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	w := newTestWriter(t, kernel.WithClock(fixedClock(fixedMs)))

	// Append three events at the same wall-clock instant.
	batch := []kernel.Event{sampleEvent(), sampleEvent(), sampleEvent()}
	receipts, err := w.Append(ctx, batch)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != 3 {
		t.Fatalf("receipt count = %d, want 3", len(receipts))
	}

	// Read back via the iterator.
	events := make([]kernel.Event, 0, len(batch))
	for ev, rerr := range w.Read(ctx, kernel.ReadOpts{}) {
		if rerr != nil {
			t.Fatalf("Read: %v", rerr)
		}
		events = append(events, ev)
	}
	if len(events) != 3 {
		t.Fatalf("read %d events, want 3", len(events))
	}

	// Every event must have a non-empty, valid HLC.
	for i, ev := range events {
		if ev.HLC == "" {
			t.Errorf("event[%d] HLC is empty; prepare must stamp every local event", i)
		}
		if _, _, err := hlc.Decode(ev.HLC); err != nil {
			t.Errorf("event[%d] HLC %q is invalid: %v", i, ev.HLC, err)
		}
	}

	// Events at the same ms must use the logical counter to maintain strict order.
	for i := 1; i < len(events); i++ {
		if events[i].HLC <= events[i-1].HLC {
			t.Errorf("HLC not strictly increasing: events[%d]=%q <= events[%d]=%q",
				i, events[i].HLC, i-1, events[i-1].HLC)
		}
	}
}

// TestGraft_PreservesRemoteHLC verifies that GraftSealed copies remote HLC
// verbatim and advances the local clock past the observed remote stamp.
func TestGraft_PreservesRemoteHLC(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)

	// Producer: write two events, read them back (they carry HLC stamps).
	producer := newTestWriter(t, kernel.WithSigner(
		kernel.NewEd25519Signer(priv, "01PRINCIPAL000000000000001", "01DEVICE0000000000000000001", "kid:test")))
	batch := []kernel.Event{
		sampleEventWithScope(kernel.ScopeTeam),
		sampleEventWithScope(kernel.ScopeTeam),
	}
	receipts, err := producer.Append(ctx, batch)
	if err != nil {
		t.Fatalf("producer Append: %v", err)
	}
	signed := make([]kernel.Event, len(receipts))
	for i, r := range receipts {
		ev, aerr := producer.At(ctx, r.UID)
		if aerr != nil {
			t.Fatalf("producer At: %v", aerr)
		}
		signed[i] = ev
	}

	// Preserve the original HLC stamps for later comparison.
	origHLCs := make([]string, len(signed))
	for i, ev := range signed {
		origHLCs[i] = ev.HLC
		if origHLCs[i] == "" {
			t.Fatalf("signed[%d] HLC is empty; producer must stamp events", i)
		}
	}

	// Consumer: a separate log that grafts the signed batch.
	consumerDir := t.TempDir()
	consumer := kernel.NewFileEventLog(consumerDir, &fakeOntology{},
		kernel.WithClock(fixedClock(authorClock)),
		kernel.WithVerifier(testVerifier{pub: pub}))
	defer func() { _ = consumer.Close() }()

	res, err := consumer.GraftSealed(ctx, signed)
	if err != nil {
		t.Fatalf("GraftSealed: %v", err)
	}
	if res.Grafted != 2 || res.Skipped != 0 || len(res.Rejections) != 0 {
		t.Fatalf("graft result: grafted=%d skipped=%d rejected=%d, want 2/0/0",
			res.Grafted, res.Skipped, len(res.Rejections))
	}

	// Verify remote HLC is preserved verbatim.
	for _, orig := range signed {
		got, aerr := consumer.At(ctx, orig.UID)
		if aerr != nil {
			t.Fatalf("consumer At %s: %v", orig.UID, aerr)
		}
		if got.HLC != orig.HLC {
			t.Errorf("HLC changed on graft: got %q, want %q (must be preserved verbatim)",
				got.HLC, orig.HLC)
		}
	}

	// After graft, the consumer's local clock must have advanced past the
	// maximum observed remote HLC. A new local event should sort after all
	// grafted events.
	localEv := sampleEventWithScope(kernel.ScopePersonal)
	localEv.Scope = kernel.ScopePersonal
	localReceipts, err := consumer.Append(ctx, []kernel.Event{localEv})
	if err != nil {
		t.Fatalf("consumer Append after graft: %v", err)
	}
	localGot, err := consumer.At(ctx, localReceipts[0].UID)
	if err != nil {
		t.Fatalf("consumer At local event: %v", err)
	}
	maxRemote := origHLCs[len(origHLCs)-1]
	if localGot.HLC <= maxRemote {
		t.Errorf("local HLC after graft (%q) not > max remote HLC (%q); clock must advance",
			localGot.HLC, maxRemote)
	}
}

// TestV1V2SignatureCoexistence verifies that:
//  1. Legacy events (no HLC) continue to verify under v1.
//  2. New events (with HLC) verify under v2.
//  3. Mutating the HLC on a v2 event breaks verification.
//  4. A mixed log (legacy + HLC events) rebuilds cleanly.
func TestV1V2SignatureCoexistence(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)
	signer := kernel.NewEd25519Signer(priv, "P", "D", "kid:test")

	// Build a v1 event manually: sign it WITHOUT an HLC field, exactly as the
	// pre-wave code did. This simulates a peer-signed legacy event from a
	// device that has not yet upgraded.
	v1ev := kernel.Event{
		UID:        "01HZX0000000000000000V1EV1",
		CreateTime: authorClock,
		Kind:       "remember",
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		Payload:    json.RawMessage(`{"text":"v1 legacy event"}`),
		// HLC intentionally empty: v1 event
	}
	if err := kernel.SignEventAuthor(&v1ev, signer); err != nil {
		t.Fatalf("sign v1 event: %v", err)
	}
	if v1ev.HLC != "" {
		t.Fatalf("v1 event must have empty HLC after signing; got %q", v1ev.HLC)
	}

	// Verify the v1 event directly.
	if !ed25519.Verify(pub, v1ev.CanonicalBytes(), v1ev.Author.Sig) {
		t.Error("v1 legacy event does not verify with its own signature")
	}

	// Build a v2 event via Append (HLC gets stamped automatically).
	w := newTestWriter(t, kernel.WithClock(fixedClock(authorClock)), kernel.WithSigner(signer))
	v2batch := []kernel.Event{sampleEvent()}
	receipts, err := w.Append(ctx, v2batch)
	if err != nil {
		t.Fatalf("Append v2 event: %v", err)
	}
	v2ev, err := w.At(ctx, receipts[0].UID)
	if err != nil {
		t.Fatalf("At v2 event: %v", err)
	}
	if v2ev.HLC == "" {
		t.Fatal("v2 event HLC is empty; Append must stamp it")
	}

	// Verify the v2 event.
	if !ed25519.Verify(pub, v2ev.CanonicalBytes(), v2ev.Author.Sig) {
		t.Error("v2 event does not verify with its own signature")
	}

	// Cross-version: v1 sig must NOT verify against v2 canonical bytes and vice versa.
	v2canonOnV1 := v1ev
	v2canonOnV1.HLC = "0197e9660b800000" // arbitrary non-empty HLC
	if ed25519.Verify(pub, v2canonOnV1.CanonicalBytes(), v1ev.Author.Sig) {
		t.Error("v1 signature verified against v2 canonical bytes; versions must be non-interchangeable")
	}

	// Mutating the HLC on a v2 event must break verification.
	tampered := v2ev
	tampered.HLC = "0000000000000001" // different HLC
	if ed25519.Verify(pub, tampered.CanonicalBytes(), v2ev.Author.Sig) {
		t.Error("mutated HLC still verifies on v2 event; HLC must be covered by the signature")
	}

	// A log that contains both pre-wave (v1, no HLC) and post-wave (v2) events
	// rebuilds cleanly when the verifier is set.
	dataDir := t.TempDir()
	dayPath := dayLogPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(dayPath), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write the v1 event (no hlc key in JSON) then the v2 event as JSONL.
	v1Line, _ := json.Marshal(v1ev)
	v2Line, _ := json.Marshal(v2ev)
	content := string(v1Line) + "\n" + string(v2Line) + "\n"
	if err := os.WriteFile(dayPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write mixed log: %v", err)
	}

	mixed := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(authorClock)),
		kernel.WithVerifier(testVerifier{pub: pub}))
	defer func() { _ = mixed.Close() }()

	// Rebuild must accept both events: personal scope skips verification for v1,
	// and v2 verifies correctly.
	store := newRebuildFakeStorage()
	errs, err := mixed.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("Rebuild mixed log: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("Rebuild mixed log: %d apply errors, want 0: %v", len(errs), errs)
	}
}

// TestSortDayEvents_MixedLegacyAndHLC verifies that a day file containing both
// legacy events (no HLC) and post-wave events (with HLC) is sorted correctly:
//   - Legacy events sort by their implicit HLC (CreateTime ms, logical 0).
//   - HLC events sort by their explicit stamp.
//   - A late-stamped HLC event with a higher wall still sorts after legacy events
//     at an earlier wall time.
func TestSortDayEvents_MixedLegacyAndHLC(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	dataDir := t.TempDir()
	dayPath := filepath.Join(dataDir, "events", "2026", "07", "01.jsonl")
	if err := os.MkdirAll(filepath.Dir(dayPath), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Construct events with known HLCs in out-of-order arrangement on disk.
	// We write them in reverse sort order to prove sortDayEvents re-orders them.
	legacyEarly := kernel.Event{
		UID:        "01HZX0000000000000000AA01",
		CreateTime: base,
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Payload:    json.RawMessage(`{"text":"legacy early"}`),
		// No HLC → implicit = Encode(base.UnixMilli(), 0)
	}
	legacyLate := kernel.Event{
		UID:        "01HZX0000000000000000AA02",
		CreateTime: base.Add(1 * time.Millisecond),
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Payload:    json.RawMessage(`{"text":"legacy late"}`),
		// No HLC → implicit = Encode((base+1ms).UnixMilli(), 0)
	}
	hlcEvent := kernel.Event{
		UID:        "01HZX0000000000000000AA03",
		CreateTime: base.Add(2 * time.Millisecond),
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Payload:    json.RawMessage(`{"text":"hlc stamped"}`),
		// HLC with wall = base+2ms, logical = 5
		HLC: hlc.Encode(base.Add(2*time.Millisecond).UnixMilli(), 5),
	}

	// Write in reverse order: hlcEvent, legacyLate, legacyEarly.
	eventsToWrite := []kernel.Event{hlcEvent, legacyLate, legacyEarly}
	lines := make([]string, 0, len(eventsToWrite))
	for _, ev := range eventsToWrite {
		b, _ := json.Marshal(ev)
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(dayPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write day file: %v", err)
	}

	r := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(base)))
	defer func() { _ = r.Close() }()

	got := make([]kernel.Event, 0, len(eventsToWrite))
	for ev, rerr := range r.Read(ctx, kernel.ReadOpts{}) {
		if rerr != nil {
			t.Fatalf("Read: %v", rerr)
		}
		got = append(got, ev)
	}

	if len(got) != 3 {
		t.Fatalf("read %d events, want 3", len(got))
	}

	// Expected order: legacyEarly, legacyLate, hlcEvent.
	wantUIDs := []string{
		legacyEarly.UID,
		legacyLate.UID,
		hlcEvent.UID,
	}
	for i, ev := range got {
		if ev.UID != wantUIDs[i] {
			t.Errorf("position %d: UID=%q, want %q", i, ev.UID, wantUIDs[i])
		}
	}

	// EffectiveHLC must be strictly increasing across the result set.
	for i := 1; i < len(got); i++ {
		if got[i].EffectiveHLC() <= got[i-1].EffectiveHLC() {
			t.Errorf("EffectiveHLC not increasing: [%d]=%q, [%d]=%q",
				i, got[i].EffectiveHLC(), i-1, got[i-1].EffectiveHLC())
		}
	}
}

// TestHLC_PersistsAcrossReopen verifies that the clock state is saved to the
// bbolt sidecar and reloaded on the next open, so a restarted log does not
// generate HLC stamps that collide with or precede the last run.
func TestHLC_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	fixedT := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	w1 := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(fixedT)))
	receipts, err := w1.Append(ctx, []kernel.Event{sampleEvent(), sampleEvent()})
	if err != nil {
		t.Fatalf("first Append: %v", err)
	}
	lastHLC1, err := w1.At(ctx, receipts[len(receipts)-1].UID)
	if err != nil {
		t.Fatalf("first At: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("Close w1: %v", err)
	}

	// Reopen at the SAME wall time. Without HLC persistence the new log
	// would start at logical 0 for the same ms, causing a stamp collision.
	w2 := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(fixedT)))
	defer func() { _ = w2.Close() }()
	receipts2, err := w2.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("second Append: %v", err)
	}
	firstHLC2, err := w2.At(ctx, receipts2[0].UID)
	if err != nil {
		t.Fatalf("second At: %v", err)
	}

	// The new stamp must sort strictly AFTER the previous session's last stamp.
	if firstHLC2.HLC <= lastHLC1.HLC {
		t.Errorf("after reopen, new stamp %q <= previous session stamp %q; HLC must be monotonic across sessions",
			firstHLC2.HLC, lastHLC1.HLC)
	}
}

// TestEffectiveHLC_LegacyEventIsImplicit verifies that EffectiveHLC for a
// legacy event (no HLC field) equals hlc.Implicit(CreateTime), and that two
// machines derive the same value from the same event.
func TestEffectiveHLC_LegacyEventIsImplicit(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	ev := kernel.Event{
		UID:        "01HZX0000000000000000LEG01",
		CreateTime: ts,
		Kind:       "remember",
		// HLC intentionally empty.
	}
	want := hlc.Implicit(ts)
	if got := ev.EffectiveHLC(); got != want {
		t.Errorf("EffectiveHLC() = %q, want implicit %q", got, want)
	}
}
