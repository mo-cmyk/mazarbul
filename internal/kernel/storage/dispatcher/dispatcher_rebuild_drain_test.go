package dispatcher_test

// dispatcher_rebuild_drain_test.go is the regression guard for the
// `maz rebuild` embedding-drop bug (fix c5fbce3). Apply schedules entity
// vector upserts on detached bgVec goroutines and returns before they finish.
// Dispatcher.Close() sets d.closed=true and ONLY THEN waits, so any embed
// worker still queued at close time observes closed, short-circuits, and SKIPS
// its vec0 upsert (logging "dispatcher: closed"). A one-shot rebuild that just
// defers Close() therefore silently drops embeddings for the still-queued tail,
// leaving those entities unsearchable by the vector lane.
//
// runRebuild now drains via Dispatcher.WaitForBackgroundVec BEFORE the deferred
// Close runs. This test pins that contract under load: with a slow embedder
// forcing many embeds to queue behind the background pool, WaitForBackgroundVec
// must leave EVERY applied entity with its name vec row — none dropped.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestRebuildDrain_WaitForBackgroundVec_PreservesAllQueuedEmbeds applies a batch
// of entities through a slow embedder (so their name embeds are still in flight
// when Apply returns), drains exactly as runRebuild does, then asserts every
// entity has its vec row. NOT t.Parallel: the slow embedder + batch keeps the
// scheduler busy and a parallel sibling would only add noise.
func TestRebuildDrain_WaitForBackgroundVec_PreservesAllQueuedEmbeds(t *testing.T) {
	ctx := testutil.TestContext(t)

	// 50ms per embed: long enough that the whole batch is queued/in-flight when
	// Apply returns (so a no-drain Close would short-circuit the tail), short
	// enough that the full drain finishes well inside the deadline.
	emb := &slowEmbedder{dim: storage.VectorDim, block: 50 * time.Millisecond}
	d := newDispatcher(t, ctx, emb)

	const n = 12
	uids := make([]string, n)
	for i := range uids {
		// 26-char, Crockford-safe (digits only after the 01H prefix) ULID-shaped id.
		uid := fmt.Sprintf("01H%023d", i)
		uids[i] = uid
		entity := ontology.Entity{
			UID:              uid,
			Kind:             ontology.EntityKindPerson,
			DisplayName:      fmt.Sprintf("Drain Person %02d", i),
			Confidence:       1.0,
			ResolutionStatus: ontology.ResolutionConfirmed,
			CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		}
		if err := d.Apply(ctx, []kernel.Event{makeEntityEvent(t, uid, entity)}); err != nil {
			t.Fatalf("Apply %s: %v", uid, err)
		}
	}

	// The drain runRebuild performs before its deferred Close. Without it, the
	// still-queued embeds would be aborted by Close and never materialise.
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	// Every entity must have its name embedding; a dropped embed shows as 0 rows.
	for _, uid := range uids {
		if got := countEntityVecRows(t, ctx, d, uid); got == 0 {
			t.Errorf("entity %s has no entities_vec row — its embed was dropped (drain did not preserve it)", uid)
		}
	}

	// Closing after a full drain must succeed and must not abort anything further
	// (bgVec is already idle), matching runRebuild's deferred Close ordering.
	if err := d.Close(); err != nil {
		t.Fatalf("Close after drain: %v", err)
	}
}
