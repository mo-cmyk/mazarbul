package kernel_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// BenchmarkValidateEntityKind measures the ValidateEntityKind hot path for
// both the cached hit (second-and-subsequent calls for the same kind) and the
// uncached miss (first call, requires a DB query + cache population).
//
// The bench populates a fresh SQLite store with the six core kinds, then
// drives the hot path for "person" (always cached after the warmup) and
// deliberately evicts the cache between sub-benchmarks by constructing a new
// SQLiteOntology for the cold path.
func BenchmarkValidateEntityKind(b *testing.B) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open sqlite: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for _, kind := range []string{"person", "organization", "place", "project", "event", "document"} {
		if _, err := store.Writer().ExecContext(ctx,
			`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			kind, kind, "Bench kind.", "core", "seed", now,
		); err != nil {
			b.Fatalf("seed %q: %v", kind, err)
		}
	}

	b.Run("cached hit", func(b *testing.B) {
		ont, err := kernel.NewSQLiteOntology(store.Reader())
		if err != nil {
			b.Fatalf("NewSQLiteOntology: %v", err)
		}
		// Warm the cache for "person".
		if err := ont.ValidateEntityKind(ctx, "person"); err != nil {
			b.Fatalf("warmup: %v", err)
		}
		b.ResetTimer()
		b.ReportAllocs()
		for range b.N {
			if err := ont.ValidateEntityKind(ctx, "person"); err != nil {
				b.Fatalf("ValidateEntityKind: %v", err)
			}
		}
	})

	b.Run("uncached miss (ErrEntityKindNotRegistered)", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			// Fresh ontology each iteration so the cache is always cold for
			// the unknown kind. The DB query is the measured cost.
			ont, err := kernel.NewSQLiteOntology(store.Reader())
			if err != nil {
				b.Fatalf("NewSQLiteOntology: %v", err)
			}
			err = ont.ValidateEntityKind(ctx, "alien")
			if err == nil {
				b.Fatal("expected ErrEntityKindNotRegistered, got nil")
			}
		}
	})

	b.Run("uncached hit (DB query then cache)", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			// Fresh ontology per iteration so every call hits the DB.
			ont, err := kernel.NewSQLiteOntology(store.Reader())
			if err != nil {
				b.Fatalf("NewSQLiteOntology: %v", err)
			}
			if err := ont.ValidateEntityKind(ctx, "person"); err != nil {
				b.Fatalf("ValidateEntityKind: %v", err)
			}
		}
	})
}
