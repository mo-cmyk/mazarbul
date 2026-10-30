package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// authorClock pins Append's timestamp so signed events land in a known daily
// bucket (events/2026/05/24.jsonl) for read-back.
var authorClock = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// fixedTestKey returns a deterministic ed25519 keypair so signatures are
// reproducible across runs (golden output, stable assertions).
func fixedTestKey(tb testing.TB) (ed25519.PrivateKey, ed25519.PublicKey) {
	tb.Helper()
	seed := sha256.Sum256([]byte("mazarbul:test:event-author:v1"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv, priv.Public().(ed25519.PublicKey)
}

// testVerifier implements kernel.Verifier by recomputing the canonical bytes
// and checking the ed25519 signature against a single known public key.
type testVerifier struct{ pub ed25519.PublicKey }

func (v testVerifier) Verify(_ context.Context, ev *kernel.Event) error {
	if ed25519.Verify(v.pub, ev.CanonicalBytes(), ev.Author.Sig) {
		return nil
	}
	return kernel.ErrSignatureInvalid
}

// dayLogPath is the JSONL bucket the fixed test clock (2026-05-24) writes to.
func dayLogPath(dataDir string) string {
	return filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
}

func TestCanonicalBytes_DeterministicAndExcludesSigSigKeyID(t *testing.T) {
	t.Parallel()
	ev := kernel.Event{
		UID:        "01HZX0000000000000000TEST1",
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      kernel.ScopePersonal,
		Payload:    json.RawMessage(`{"text":"hello"}`),
		Author: kernel.EventAuthor{
			PrincipalUID: "P1",
			DeviceID:     "D1",
			Sig:          []byte("original-sig"),
			SigKeyID:     "kid:original",
		},
	}
	want := ev.CanonicalBytes()

	// Sig and SigKeyID are excluded: mutating them must not change the bytes.
	got := ev
	got.Author.Sig = []byte("totally-different-and-longer-signature")
	got.Author.SigKeyID = "kid:rotated"
	got.Name = "events/derived-from-uid"
	if !bytesEqual(want, got.CanonicalBytes()) {
		t.Error("CanonicalBytes changed when only Sig/SigKeyID/Name changed; they must be excluded")
	}

	// PrincipalUID, DeviceID, and Payload ARE covered.
	for _, mut := range []func(*kernel.Event){
		func(e *kernel.Event) { e.Author.PrincipalUID = "P2" },
		func(e *kernel.Event) { e.Author.DeviceID = "D2" },
		func(e *kernel.Event) { e.Payload = json.RawMessage(`{"text":"world"}`) },
		func(e *kernel.Event) { e.Kind = "relate" },
	} {
		m := ev
		mut(&m)
		if bytesEqual(want, m.CanonicalBytes()) {
			t.Error("CanonicalBytes did not change when a covered field changed")
		}
	}
}

// TestCanonicalBytes_GoldenVector pins the exact v1 wire layout with a
// hard-coded hex string. Unlike the round-trip tests, this fails loudly if
// ANY layout detail drifts (field order, the NUL after the tag, the uint32-BE
// length prefixes, or the 'g'/-1/64 float format) — forcing a conscious
// canonicalEventVersion bump rather than a silent signature break.
func TestCanonicalBytes_GoldenVector(t *testing.T) {
	t.Parallel()
	ev := kernel.Event{
		UID:        "01HZX0000000000000000TEST1",
		CreateTime: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Kind:       "remember",
		Source:     "test",
		Confidence: 0.5,
		Scope:      kernel.ScopePersonal,
		Payload:    json.RawMessage(`{"text":"hello"}`),
		Author:     kernel.EventAuthor{PrincipalUID: "P1", DeviceID: "D1"},
	}
	const wantHex = "6d617a617262756c2d6576656e742d7631000000001a3031485a5830303030303030303030303030303030544553543100000014323032362d30352d32345431323a30303a30305a0000000872656d656d626572000000047465737400000003302e3500000008706572736f6e616c000000025031000000024431000000107b2274657874223a2268656c6c6f227d"
	got := hex.EncodeToString(ev.CanonicalBytes())
	if got != wantHex {
		t.Fatalf("canonical bytes golden mismatch:\n got  %s\n want %s", got, wantHex)
	}
}

func TestAppend_SignsEvent_RoundTripVerifies(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)
	dataDir := t.TempDir()
	clock := fixedClock(authorClock)
	signer := kernel.NewEd25519Signer(priv, "01PRINCIPAL000000000000001", "01DEVICE0000000000000000001", "kid:test")
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(clock), kernel.WithSigner(signer))

	if _, err := w.Append(ctx, []kernel.Event{authorSampleEvent()}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ev := readSingleEvent(t, dayLogPath(dataDir))
	if ev.Author.PrincipalUID != "01PRINCIPAL000000000000001" {
		t.Errorf("Author.PrincipalUID = %q, want signer principal", ev.Author.PrincipalUID)
	}
	if ev.Author.SigKeyID != "kid:test" {
		t.Errorf("Author.SigKeyID = %q, want kid:test", ev.Author.SigKeyID)
	}
	if len(ev.Author.Sig) != ed25519.SignatureSize {
		t.Fatalf("Author.Sig length = %d, want %d", len(ev.Author.Sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, ev.CanonicalBytes(), ev.Author.Sig) {
		t.Error("round-tripped event signature did not verify")
	}
}

func TestVerify_TamperedPayloadFails(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)
	dataDir := t.TempDir()
	signer := kernel.NewEd25519Signer(priv, "P", "D", "kid:test")
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(authorClock)), kernel.WithSigner(signer))
	if _, err := w.Append(ctx, []kernel.Event{authorSampleEvent()}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Flip one byte of the payload on disk, keeping the stale signature.
	tamperPayload(t, dayLogPath(dataDir), "hello", "world")

	ev := readSingleEvent(t, dayLogPath(dataDir))
	if ed25519.Verify(pub, ev.CanonicalBytes(), ev.Author.Sig) {
		t.Error("verification passed on a tampered payload; want failure")
	}
}

func TestRebuild_TeamScopeTamperedFailsVerification(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)
	dataDir := t.TempDir()
	signer := kernel.NewEd25519Signer(priv, "P", "D", "kid:test")
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(authorClock)), kernel.WithSigner(signer),
		kernel.WithVerifier(testVerifier{pub: pub}))

	ev := authorSampleEvent()
	ev.Scope = kernel.ScopeTeam
	if _, err := w.Append(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	tamperPayload(t, dayLogPath(dataDir), "hello", "world")

	// Non-strict: the tampered team event is recorded as an ApplyError.
	store := newRebuildFakeStorage()
	errs, err := w.Rebuild(ctx, store)
	if err != nil {
		t.Fatalf("Rebuild (non-strict): %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("ApplyError count = %d, want 1", len(errs))
	}
	if !errors.Is(errs[0], kernel.ErrSignatureInvalid) {
		t.Errorf("ApplyError = %v, want ErrSignatureInvalid", errs[0])
	}

	// Strict: the first failed verification aborts the rebuild.
	_, serr := w.Rebuild(ctx, store, kernel.WithStrict())
	if !errors.Is(serr, kernel.ErrSignatureInvalid) {
		t.Errorf("strict Rebuild error = %v, want ErrSignatureInvalid", serr)
	}
}

func TestAppend_TeamScopeRejectsLocalDefaultSigner(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	// No WithSigner: the log installs the deterministic local-default signer
	// (public seed). A personal-scope event signs fine under it...
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(authorClock)))
	if _, err := w.Append(ctx, []kernel.Event{authorSampleEvent()}); err != nil {
		t.Fatalf("personal-scope Append under local default: %v", err)
	}

	// ...but a team-scope event must be refused: a forgeable local-default
	// signature can never cross the writer boundary.
	team := authorSampleEvent()
	team.Scope = kernel.ScopeTeam
	_, err := w.Append(ctx, []kernel.Event{team})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("team-scope Append under local default = %v, want ErrInvalidEvent", err)
	}
}

func TestRebuild_PersonalScopeSkipsVerification(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)
	dataDir := t.TempDir()
	signer := kernel.NewEd25519Signer(priv, "P", "D", "kid:test")
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{},
		kernel.WithClock(fixedClock(authorClock)), kernel.WithSigner(signer),
		kernel.WithVerifier(testVerifier{pub: pub}))

	// Personal scope (the default) with a tampered payload must still rebuild
	// cleanly: the scope guard means verification never runs.
	if _, err := w.Append(ctx, []kernel.Event{authorSampleEvent()}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	tamperPayload(t, dayLogPath(dataDir), "hello", "world")

	errs, err := w.Rebuild(ctx, newRebuildFakeStorage())
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("ApplyError count = %d, want 0 (personal scope skips verification)", len(errs))
	}
}

// TestLogNeedsSigning_MixedBucketDetected guards the boot check against a
// mixed bucket: a signed first line must NOT mask a later unsigned legacy
// event. A first-line-only peek would report the bucket signed and let the
// daemon boot on a log that still violates the mandatory-Author invariant.
func TestLogNeedsSigning_MixedBucketDetected(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	bucket := dayLogPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(bucket), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// First line carries an author signature; the second is legacy/unsigned.
	writeLegacyLog(t, bucket, []string{
		`{"uid":"01HZX0000000000000000SIG01","kind":"remember","source":"x","confidence":1,"scope":"personal","payload":{"text":"signed"},"author":{"principalUid":"P","deviceId":"D","sig":"AAAA","sigKeyId":"kid:test"}}`,
		`{"uid":"01HZX0000000000000000LEG02","kind":"remember","source":"legacy","confidence":1,"scope":"personal","payload":{"text":"unsigned"}}`,
	})

	needs, err := kernel.LogNeedsSigning(dataDir)
	if err != nil {
		t.Fatalf("LogNeedsSigning: %v", err)
	}
	if !needs {
		t.Error("LogNeedsSigning = false for a mixed bucket; a later unsigned line was missed")
	}
}

func TestReStampAuthors_SignsLegacyAndIsIdempotent(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	bucket := dayLogPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(bucket), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Two legacy (unsigned) events: marshal without an Author block by using a
	// minimal struct, mirroring a pre-signing on-disk line.
	writeLegacyLog(t, bucket, []string{
		`{"uid":"01HZX0000000000000000LEG01","kind":"remember","source":"legacy","confidence":1,"scope":"personal","payload":{"text":"one"}}`,
		`{"uid":"01HZX0000000000000000LEG02","kind":"remember","source":"legacy","confidence":1,"scope":"personal","payload":{"text":"two"}}`,
	})

	if needs, err := kernel.LogNeedsSigning(dataDir); err != nil || !needs {
		t.Fatalf("LogNeedsSigning before = (%v, %v), want (true, nil)", needs, err)
	}

	priv, pub := fixedTestKey(t)
	signer := kernel.NewEd25519Signer(priv, "P", "D", "kid:test")
	n, err := kernel.ReStampAuthors(dataDir, signer)
	if err != nil {
		t.Fatalf("ReStampAuthors: %v", err)
	}
	if n != 2 {
		t.Fatalf("re-stamped %d events, want 2", n)
	}

	// Every line now verifies, and UID + payload are preserved verbatim.
	lines := readAllLines(t, bucket)
	if len(lines) != 2 {
		t.Fatalf("line count after re-stamp = %d, want 2", len(lines))
	}
	wantUIDs := []string{"01HZX0000000000000000LEG01", "01HZX0000000000000000LEG02"}
	for i, line := range lines {
		var ev kernel.Event
		if uerr := json.Unmarshal([]byte(line), &ev); uerr != nil {
			t.Fatalf("decode line %d: %v", i, uerr)
		}
		if ev.UID != wantUIDs[i] {
			t.Errorf("line %d UID = %q, want %q (UID must be preserved)", i, ev.UID, wantUIDs[i])
		}
		if !ed25519.Verify(pub, ev.CanonicalBytes(), ev.Author.Sig) {
			t.Errorf("line %d did not verify after re-stamp", i)
		}
	}

	if needs, lerr := kernel.LogNeedsSigning(dataDir); lerr != nil || needs {
		t.Fatalf("LogNeedsSigning after = (%v, %v), want (false, nil)", needs, lerr)
	}

	// Idempotent: a second run signs nothing.
	n2, err := kernel.ReStampAuthors(dataDir, signer)
	if err != nil {
		t.Fatalf("ReStampAuthors (2nd): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second ReStampAuthors signed %d events, want 0 (idempotent)", n2)
	}
}

func BenchmarkAppendSigned(b *testing.B) {
	ctx := context.Background()
	priv, _ := fixedTestKey(b)
	signer := kernel.NewEd25519Signer(priv, "P", "D", "kid:test")
	w := kernel.NewFileEventLog(b.TempDir(), &fakeOntology{}, kernel.WithSigner(signer))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := w.Append(ctx, []kernel.Event{authorSampleEvent()}); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
}

// --- helpers (kept local to avoid coupling to other test files) ---

func authorSampleEvent() kernel.Event {
	return kernel.Event{
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Payload:    json.RawMessage(`{"text":"hello"}`),
	}
}

func readSingleEvent(t *testing.T, path string) kernel.Event {
	t.Helper()
	lines := readAllLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("want 1 event line, got %d", len(lines))
	}
	var ev kernel.Event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return ev
}

func readAllLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func tamperPayload(t *testing.T, path, old, replacement string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read for tamper: %v", err)
	}
	mutated := strings.Replace(string(raw), old, replacement, 1)
	if mutated == string(raw) {
		t.Fatalf("tamper target %q not found in log", old)
	}
	if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}
}

func writeLegacyLog(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
