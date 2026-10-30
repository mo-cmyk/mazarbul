package dispatcher_test

// dispatcher_async_entity_test.go pins the latency contract for the Entity
// write path. Entities sync-embedded BOTH the name and (when present) the
// summary before the writer tx even began, so each Entity event cost up to
// 2x the slowEmbedder round-trip. A LongMemEval haystack resolves on the
// order of one Entity per Mention, so this is the third major Apply stall
// after episodes and mentions. After the fix, the entities row, the
// mention-link backfill, and the graph node land synchronously; the two
// vec rows materialise on a detached goroutine tracked by d.bgVec.

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestApply_Entity_AsyncEmbedding_ReturnsBeforeEmbedderCompletes is the
// canary for the async-entity-embed contract. NOT t.Parallel: wall-clock
// measurement plus LadybugDB cold-start needs a quiet machine.
func TestApply_Entity_AsyncEmbedding_ReturnsBeforeEmbedderCompletes(t *testing.T) {
	ctx := testutil.TestContext(t)

	emb := &slowEmbedder{dim: storage.VectorDim, block: blockDuration}
	d := newDispatcher(t, ctx, emb)

	entityUID := "01HENTITY00000000ASYNC0001"
	entity := ontology.Entity{
		UID:              entityUID,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo Baggins",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		Summary:          "Frodo Baggins is an engineer at Erebor Inc. based in Berlin.",
	}
	ev := makeEntityEvent(t, entityUID, entity)

	start := time.Now()
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply entity: %v", err)
	}
	elapsed := time.Since(start)

	// Both the name and summary embeds together would push the inline path
	// over 2 * blockDuration. The hot path must fit well under one embed.
	if elapsed > hotPathBudget {
		t.Errorf("Apply hot path took %v, want <= %v (entity embedder is async, not inline)", elapsed, hotPathBudget)
	}

	waitCtx, cancel := context.WithTimeout(ctx, bgVecDeadline)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	// Two vec rows must exist after the drain: name + summary.
	if got := countEntityVecRows(t, ctx, d, entityUID); got != 2 {
		t.Errorf("entities_vec_ids rows = %d, want 2 (name + summary) after WaitForBackgroundVec", got)
	}
}
