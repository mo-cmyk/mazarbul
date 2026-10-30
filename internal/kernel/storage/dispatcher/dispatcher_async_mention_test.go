package dispatcher_test

// dispatcher_async_mention_test.go pins the latency contract for the Mention
// write path. Mirrors dispatcher_async_episode_test.go: dispatcher.Apply MUST
// return as soon as the mentions row is durable, with the embedder call
// deferred to a background goroutine. The resolver emits one Mention event
// per extracted span; a LongMemEval haystack of 500 episodes with ~5 mentions
// each is 2,500 synchronous Ollama round-trips on the writer goroutine before
// the fix. After the fix, the row write commits immediately and the vector
// materialises asynchronously alongside the episode vector.

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestApply_Mention_AsyncEmbedding_ReturnsBeforeEmbedderCompletes is the
// canary for the async-mention-embed contract. NOT t.Parallel for the same
// reason as the episode canary: wall-clock measurement plus LadybugDB
// cold-start needs a quiet machine.
func TestApply_Mention_AsyncEmbedding_ReturnsBeforeEmbedderCompletes(t *testing.T) {
	ctx := testutil.TestContext(t)

	emb := &slowEmbedder{dim: storage.VectorDim, block: blockDuration}
	d := newDispatcher(t, ctx, emb)

	episodeUID := "01HEPISODE0000ASYNCMENTION1"
	if err := d.Apply(ctx, []kernel.Event{
		makeEpisodeEvent(t, episodeUID, "Frodo works at Erebor.", "test"),
	}); err != nil {
		t.Fatalf("Apply episode: %v", err)
	}

	mentionUID := "01HMENTION0000000000000001"
	mention := ontology.Mention{
		UID:            mentionUID,
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Frodo",
		SourceEventUID: ontology.ULID(episodeUID),
		ExtractionText: "Frodo works at Erebor.",
		SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 23},
		GroundingScore: 1.0,
	}
	ev := makeMentionEvent(t, mentionUID, mention)

	start := time.Now()
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply mention: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > hotPathBudget {
		t.Errorf("Apply hot path took %v, want <= %v (mention embedder is async, not inline)", elapsed, hotPathBudget)
	}

	waitCtx, cancel := context.WithTimeout(ctx, bgVecDeadline)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	if !hasMentionVecRow(t, ctx, d, mentionUID) {
		t.Errorf("mentions_vec_ids row missing for %s after WaitForBackgroundVec", mentionUID)
	}
}
