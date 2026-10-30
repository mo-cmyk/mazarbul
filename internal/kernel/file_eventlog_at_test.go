package kernel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// dataDirOf returns the dataDir the test writer was constructed with by
// walking up from the events/ root the writer touches. We cannot read
// the unexported field from kernel_test, so the convention is: every
// FileEventLog under test owns a fresh TempDir, and the writer's
// dataDir is also the test's tempDir. The caller of newTestWriter knows
// the dir; the helpers below take it explicitly.

func sampleEventForAt(uid string) kernel.Event {
	return kernel.Event{
		UID:        uid,
		Kind:       "remember",
		Source:     "test/at",
		Confidence: 1,
		Payload:    json.RawMessage(fmt.Sprintf(`{"text":%q}`, "hello-"+uid)),
	}
}

// newTestWriterAt is a variant of newTestWriter that returns both the
// writer and its dataDir so tests can poke at the on-disk layout.
func newTestWriterAt(t *testing.T, opts ...kernel.Option) (*kernel.FileEventLog, string) {
	t.Helper()
	dir := t.TempDir()
	defaultClock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	base := []kernel.Option{kernel.WithClock(fixedClock(defaultClock))}
	w := kernel.NewFileEventLog(dir, &fakeOntology{}, append(base, opts...)...)
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return w, dir
}

func TestFileEventLog_At_RoundTripsSingleEvent(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, _ := newTestWriterAt(t)

	in := sampleEventForAt("")
	receipts, err := w.Append(ctx, []kernel.Event{in})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := w.At(ctx, receipts[0].UID)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if got.UID != receipts[0].UID {
		t.Errorf("UID drift: got %q, want %q", got.UID, receipts[0].UID)
	}
	if got.Kind != in.Kind || got.Source != in.Source || got.Confidence != in.Confidence {
		t.Errorf("scalar field drift: got %+v", got)
	}
	if !bytes.Equal(got.Payload, in.Payload) {
		t.Errorf("payload drift: got %s, want %s", got.Payload, in.Payload)
	}
}

func TestFileEventLog_At_RoundTripsManyEvents(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, _ := newTestWriterAt(t)

	const n = 200
	batch := make([]kernel.Event, n)
	for i := range batch {
		batch[i] = sampleEventForAt("")
		batch[i].Payload = json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
	}
	receipts, err := w.Append(ctx, batch)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	for i, r := range receipts {
		got, lerr := w.At(ctx, r.UID)
		if lerr != nil {
			t.Fatalf("At[%d] (%s): %v", i, r.UID, lerr)
		}
		if got.UID != r.UID {
			t.Errorf("UID drift at %d: got %q, want %q", i, got.UID, r.UID)
		}
		want := fmt.Sprintf(`{"i":%d}`, i)
		if string(got.Payload) != want {
			t.Errorf("payload drift at %d: got %s, want %s", i, got.Payload, want)
		}
	}
}

func TestFileEventLog_At_UnknownUIDReturnsErrNotFound(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, _ := newTestWriterAt(t)

	// Land one event so the index file exists.
	if _, err := w.Append(ctx, []kernel.Event{sampleEventForAt("")}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Any other valid ULID that was never appended.
	other := ulid.MustNew(ulid.Now(), nil).String()
	if _, err := w.At(ctx, other); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("want ErrNotFound for unknown UID, got %v", err)
	}
}

func TestFileEventLog_At_MalformedUIDReturnsErrInvalidEvent(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, _ := newTestWriterAt(t)

	if _, err := w.At(ctx, "not-a-ulid"); !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Errorf("want ErrInvalidEvent for malformed UID, got %v", err)
	}
}

func TestFileEventLog_At_RebuildsWhenIndexMissing(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, dir := newTestWriterAt(t)

	in := sampleEventForAt("")
	receipts, err := w.Append(ctx, []kernel.Event{in})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Close to release the bolt file lock, then delete the sidecar.
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	indexPath := filepath.Join(dir, "events", "index.bolt")
	if rerr := os.Remove(indexPath); rerr != nil {
		t.Fatalf("remove index: %v", rerr)
	}
	if _, sterr := os.Stat(indexPath); !errors.Is(sterr, os.ErrNotExist) {
		t.Fatalf("expected index removed, stat: %v", sterr)
	}

	// At against the same writer should fall back to the JSONL scan,
	// return the event, and rebuild the index sidecar.
	got, lerr := w.At(ctx, receipts[0].UID)
	if lerr != nil {
		t.Fatalf("At after rebuild: %v", lerr)
	}
	if got.UID != receipts[0].UID {
		t.Errorf("UID drift: got %q, want %q", got.UID, receipts[0].UID)
	}
	if _, sterr := os.Stat(indexPath); sterr != nil {
		t.Errorf("expected index rebuilt, stat: %v", sterr)
	}
}

func TestFileEventLog_At_CrossDayBatch(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, _ := newTestWriterAt(t)

	// Two events crossing midnight UTC. Caller-supplied CreateTime, so
	// the writer's fixed clock is irrelevant beyond the maxSkew check
	// (which only rejects FUTURE times). 2026-05-23 is the day before
	// the writer's clock and 2026-05-24 is the same day.
	before := sampleEventForAt("")
	before.CreateTime = time.Date(2026, 5, 23, 23, 59, 0, 0, time.UTC)
	after := sampleEventForAt("")
	after.CreateTime = time.Date(2026, 5, 24, 0, 1, 0, 0, time.UTC)

	receipts, err := w.Append(ctx, []kernel.Event{before, after})
	if err != nil {
		t.Fatalf("Append cross-day: %v", err)
	}

	for _, r := range receipts {
		got, lerr := w.At(ctx, r.UID)
		if lerr != nil {
			t.Fatalf("At %s: %v", r.UID, lerr)
		}
		if got.UID != r.UID {
			t.Errorf("UID drift: got %q, want %q", got.UID, r.UID)
		}
	}
}

// TestFileEventLog_At_RebuildsAfterRunningDeletion mirrors the
// operator-removed-sidecar scenario described in the design doc: the
// writer is still open (its bolt handle holds the inode) when the
// sidecar file is deleted from disk. At must detect the missing path,
// reopen, and rebuild — leaving a fresh events/index.bolt behind.
func TestFileEventLog_At_RebuildsAfterRunningDeletion(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, dir := newTestWriterAt(t)

	receipts, err := w.Append(ctx, []kernel.Event{sampleEventForAt("")})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Force the writer to open the bolt handle, then delete the file
	// while the handle is still live.
	if _, err := w.At(ctx, receipts[0].UID); err != nil {
		t.Fatalf("prime At: %v", err)
	}
	indexPath := filepath.Join(dir, "events", "index.bolt")
	if rerr := os.Remove(indexPath); rerr != nil {
		t.Fatalf("remove index: %v", rerr)
	}
	if _, sterr := os.Stat(indexPath); !errors.Is(sterr, os.ErrNotExist) {
		t.Fatalf("expected index removed, stat: %v", sterr)
	}

	got, lerr := w.At(ctx, receipts[0].UID)
	if lerr != nil {
		t.Fatalf("At after running deletion: %v", lerr)
	}
	if got.UID != receipts[0].UID {
		t.Errorf("UID drift: got %q, want %q", got.UID, receipts[0].UID)
	}
	if _, sterr := os.Stat(indexPath); sterr != nil {
		t.Errorf("expected index rebuilt at path, stat: %v", sterr)
	}
}

// TestFileEventLog_Append_RejectsDuplicateUIDInBatch covers a caller
// who supplies the same ULID twice within a single Append. Without
// pre-commit deduplication the index would point only to the latest
// offset, so a caller holding the first receipt could materialize
// the wrong event.
func TestFileEventLog_Append_RejectsDuplicateUIDInBatch(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, _ := newTestWriterAt(t)

	id := ulid.MustNew(ulid.Now(), nil).String()
	a := sampleEventForAt(id)
	b := sampleEventForAt(id)

	_, err := w.Append(ctx, []kernel.Event{a, b})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Errorf("want ErrInvalidEvent for duplicate UID in batch, got %v", err)
	}
}

// TestFileEventLog_Append_RejectsDuplicateUIDAcrossBatches covers a
// caller who reuses a previously appended ULID. The earlier event must
// remain reachable via At, and the duplicate Append must fail before
// any JSONL bytes are written.
func TestFileEventLog_Append_RejectsDuplicateUIDAcrossBatches(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, _ := newTestWriterAt(t)

	id := ulid.MustNew(ulid.Now(), nil).String()
	first := sampleEventForAt(id)
	first.Payload = []byte(`{"text":"first"}`)
	if _, err := w.Append(ctx, []kernel.Event{first}); err != nil {
		t.Fatalf("first Append: %v", err)
	}

	second := sampleEventForAt(id)
	second.Payload = []byte(`{"text":"second"}`)
	if _, err := w.Append(ctx, []kernel.Event{second}); !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Errorf("want ErrInvalidEvent for duplicate UID across batches, got %v", err)
	}

	// The first event must still resolve to the first payload.
	got, err := w.At(ctx, id)
	if err != nil {
		t.Fatalf("At first: %v", err)
	}
	if string(got.Payload) != `{"text":"first"}` {
		t.Errorf("first payload drift: got %s", got.Payload)
	}
}

// TestFileEventLog_Append_RejectsDuplicateUIDWhenSidecarMissing covers
// the operator-deleted-sidecar window flagged by Greptile on PR #92:
// the JSONL files still hold the durable events, so the duplicate-UID
// guard must rebuild the index from the JSONL source of truth before
// it can declare a caller-supplied UID unique.
func TestFileEventLog_Append_RejectsDuplicateUIDWhenSidecarMissing(t *testing.T) {
	ctx := testutil.TestContext(t)
	w, dir := newTestWriterAt(t)

	id := ulid.MustNew(ulid.Now(), nil).String()
	first := sampleEventForAt(id)
	first.Payload = []byte(`{"text":"first"}`)
	if _, err := w.Append(ctx, []kernel.Event{first}); err != nil {
		t.Fatalf("first Append: %v", err)
	}

	// Operator removes the sidecar; JSONL stays put. The writer still
	// holds the bolt handle from the first Append, mirroring the
	// running-daemon recovery path documented in the design doc.
	indexPath := filepath.Join(dir, "events", "index.bolt")
	if rerr := os.Remove(indexPath); rerr != nil {
		t.Fatalf("remove index: %v", rerr)
	}

	second := sampleEventForAt(id)
	second.Payload = []byte(`{"text":"second"}`)
	if _, err := w.Append(ctx, []kernel.Event{second}); !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Errorf("want ErrInvalidEvent for duplicate UID with missing sidecar, got %v", err)
	}

	got, err := w.At(ctx, id)
	if err != nil {
		t.Fatalf("At first event: %v", err)
	}
	if string(got.Payload) != `{"text":"first"}` {
		t.Errorf("first payload should survive: got %s", got.Payload)
	}
}

func TestFileEventLog_At_RejectsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(testutil.TestContext(t))
	cancel()
	w, _ := newTestWriterAt(t)

	if _, err := w.At(ctx, ulid.MustNew(ulid.Now(), nil).String()); err == nil {
		t.Errorf("want error on cancelled context, got nil")
	}
}
