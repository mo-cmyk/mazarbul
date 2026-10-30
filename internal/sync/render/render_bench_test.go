//go:build bench

package render_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// scaleEventBuilder constructs an event stream sized to (entities,
// relations). One Entity per slot, plus enough Relation events to
// hit the relation count. All ULIDs are produced via ulidAt(seq) so
// the stream is deterministic and the benchmark numbers are
// comparable across runs.
//
// Kept separate from buildDeterministicEventStream because the
// determinism test runs a small fixed-size workload (it cares about
// correctness, not throughput) while the scale benches need a
// parametric builder.
func scaleEventBuilder(entities, relations int) []kernel.Event {
	events := make([]kernel.Event, 0, entities+relations+1)

	predUID := ulidAt(0)
	events = append(events, makeEvent(predUID, ontology.KindPredicateRegistered,
		ontology.PredicateRegisteredEvent{
			Predicate:   "works_at",
			Kind:        ontology.PredicateKindFunctional,
			Description: "Person is employed by Organization.",
		}))

	// Entities go at seq 10, 20, ... so relation seqs slot in between.
	entitySeq := func(i int) int { return (i + 1) * 10 }
	for i := 0; i < entities; i++ {
		uid := ulidAt(entitySeq(i))
		events = append(events, makeEvent(uid, ontology.KindEntity,
			entityPayload(uid, "Person "+itoa(i), ontology.EntityKindPerson)))
	}

	// Relations connect entity[i] → entity[(i+1) % entities].
	// Relation UIDs use a high-seq offset so they sort AFTER the
	// Entity events that defined their endpoints — required by the
	// renderer's canonical-order contract.
	for i := 0; i < relations; i++ {
		subj := ulidAt(entitySeq(i % entities))
		obj := ulidAt(entitySeq((i + 1) % entities))
		relUID := ulidAt(1_000_000 + i)
		events = append(events, makeEvent(relUID, ontology.KindRelation,
			relationPayload(relUID, subj, "works_at", obj)))
	}
	return events
}

// BenchmarkRender_10k50k is the lower-scale gate. Plan budget: < 500 ms.
// b.N is honored even though Render is internally O(events): we want
// the bench framework's amortisation noise floor, not a hard wall.
func BenchmarkRender_10k50k(b *testing.B) {
	events := scaleEventBuilder(10_000, 50_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := render.Render(render.Input{
			Scope:  kernel.ScopePersonal,
			Events: seqEvents(events),
		}); err != nil {
			b.Fatalf("Render: %v", err)
		}
	}
}

// BenchmarkRender_50k250k. Plan budget: < 5 s.
func BenchmarkRender_50k250k(b *testing.B) {
	events := scaleEventBuilder(50_000, 250_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := render.Render(render.Input{
			Scope:  kernel.ScopePersonal,
			Events: seqEvents(events),
		}); err != nil {
			b.Fatalf("Render: %v", err)
		}
	}
}

// BenchmarkRender_100k500k is the headline scale gate. Plan budget:
// < 30 s wall-clock, < 200 MB output. The wall-clock side is
// captured by `go test -bench`; the disk side is logged via
// b.Logf for the operator to spot regressions, but not asserted
// because output size depends on the seed event payload sizes,
// which are simplified in this synthetic builder.
func BenchmarkRender_100k500k(b *testing.B) {
	events := scaleEventBuilder(100_000, 500_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := render.Render(render.Input{
			Scope:  kernel.ScopePersonal,
			Events: seqEvents(events),
		})
		if err != nil {
			b.Fatalf("Render: %v", err)
		}
		if i == 0 {
			var totalBytes int64
			for _, f := range view.Files {
				totalBytes += int64(len(f.Content))
			}
			b.Logf("100k entities + 500k relations: %d files, %d total bytes",
				len(view.Files), totalBytes)
		}
	}
}

// BenchmarkRender_IncrementalPush_1kChanged_100kBase mirrors the
// common-case push: a 100k-entity scope receives 1k of new events.
// Today the renderer does not expose a true incremental path
// through the public API (PriorState is unwired in transport); this
// benchmark therefore measures the FULL re-render with 1k more
// events appended on top of the 100k base — the worst case the
// transport actually runs. Plan budget: < 200 ms (the budget assumes
// PriorState plumbing; without it, the number is the cold-render
// number, which is what we report).
func BenchmarkRender_IncrementalPush_1kChanged_100kBase(b *testing.B) {
	base := scaleEventBuilder(100_000, 0)
	delta := scaleEventBuilder(1_000, 0)
	for i := range delta {
		delta[i].UID = ulidAt(10_000_000 + i)
		delta[i].Name = "events/" + delta[i].UID
	}
	combined := make([]kernel.Event, 0, len(base)+len(delta))
	combined = append(combined, base...)
	combined = append(combined, delta...)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := render.Render(render.Input{
			Scope:  kernel.ScopePersonal,
			Events: seqEvents(combined),
		}); err != nil {
			b.Fatalf("Render: %v", err)
		}
	}
}
