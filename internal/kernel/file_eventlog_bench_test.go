package kernel_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// seedReadBench fills a temp data dir with `total` events split across
// three day buckets. Half are kind "remember", half are "recall_hit".
// Returns the writer pointing at the seeded dir so the benchmark can
// Read against it.
func seedReadBench(b *testing.B, total int) *kernel.FileEventLog {
	b.Helper()
	dataDir := b.TempDir()
	// Clock past every event so the writer accepts caller-supplied
	// CreateTime values without tripping skew.
	clock := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, &fakeOntology{}, kernel.WithClock(fixedClock(clock)))

	// Spread events across three days; mix two kinds 50/50.
	days := []time.Time{
		time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
	}
	perDay := total / len(days)
	batch := make([]kernel.Event, 0, perDay)
	for di, day := range days {
		batch = batch[:0]
		for i := range perDay {
			ev := kernel.Event{
				Source:     "bench",
				Confidence: 1,
				Payload:    json.RawMessage(`{"text":"hello"}`),
				CreateTime: day.Add(time.Duration(i) * time.Second),
			}
			if (di+i)%2 == 0 {
				ev.Kind = "remember"
			} else {
				ev.Kind = "recall_hit"
			}
			batch = append(batch, ev)
		}
		if _, err := w.Append(context.Background(), batch); err != nil {
			b.Fatalf("seed Append day %d: %v", di, err)
		}
	}
	return w
}

func BenchmarkAppend_Single(b *testing.B) {
	ctx := context.Background()
	w := kernel.NewFileEventLog(b.TempDir(), &fakeOntology{})

	ev := kernel.Event{
		Kind:       "remember",
		Source:     "bench",
		Confidence: 1,
		Payload:    json.RawMessage(`{"text":"hello"}`),
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := w.Append(ctx, []kernel.Event{ev}); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
}

func BenchmarkAppend_BatchOf100(b *testing.B) {
	ctx := context.Background()
	w := kernel.NewFileEventLog(b.TempDir(), &fakeOntology{})

	batch := make([]kernel.Event, 100)
	for i := range batch {
		batch[i] = kernel.Event{
			Kind:       "remember",
			Source:     "bench",
			Confidence: 1,
			Payload:    json.RawMessage(`{"text":"hello"}`),
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := w.Append(ctx, batch); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
}

// BenchmarkAt_HotIndex measures random-access lookup against an index
// already populated with 10k events. The cost is one bbolt page read
// plus one JSONL line read; the goal in the design doc is sub-100us
// per lookup. Tracked in the PR description so regressions are visible.
func BenchmarkAt_HotIndex(b *testing.B) {
	ctx := context.Background()
	w := kernel.NewFileEventLog(b.TempDir(), &fakeOntology{})
	b.Cleanup(func() { _ = w.Close() })

	const seedEvents = 10_000
	batch := make([]kernel.Event, seedEvents)
	for i := range batch {
		batch[i] = kernel.Event{
			Kind:       "remember",
			Source:     "bench",
			Confidence: 1,
			Payload:    json.RawMessage(`{"text":"hello"}`),
		}
	}
	receipts, err := w.Append(ctx, batch)
	if err != nil {
		b.Fatalf("seed Append: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		uid := receipts[i%len(receipts)].UID
		if _, err := w.At(ctx, uid); err != nil {
			b.Fatalf("At: %v", err)
		}
	}
}

func BenchmarkRead_Sequential(b *testing.B) {
	const total = 9000 // 3 day buckets x 3000 events each
	w := seedReadBench(b, total)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var n int
		for _, err := range w.Read(ctx, kernel.ReadOpts{}) {
			if err != nil {
				b.Fatalf("Read: %v", err)
			}
			n++
		}
		if n != total {
			b.Fatalf("Read drained %d events, want %d", n, total)
		}
	}
}

func BenchmarkRead_Filtered(b *testing.B) {
	const total = 9000
	w := seedReadBench(b, total)
	ctx := context.Background()
	opts := kernel.ReadOpts{Kinds: []kernel.EventKind{"remember"}}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for _, err := range w.Read(ctx, opts) {
			if err != nil {
				b.Fatalf("Read: %v", err)
			}
		}
	}
}
