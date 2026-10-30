package reranker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

type fakeChat struct {
	in    kernel.ToolUseRequest
	out   kernel.ToolUseResponse
	err   error
	calls int
}

func (f *fakeChat) ToolUse(_ context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	f.in = req
	f.calls++
	return f.out, f.err
}

func (*fakeChat) Complete(_ context.Context, _ kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	return kernel.CompleteResponse{}, errors.New("unexpected Complete call")
}

func hits(uids ...string) []kernel.Hit {
	out := make([]kernel.Hit, len(uids))
	for i, uid := range uids {
		out[i] = kernel.Hit{UID: uid, Snippet: "snippet " + uid}
	}
	return out
}

func TestRerank_EmptyOrSingleIsPassThrough(t *testing.T) {
	t.Parallel()
	r, err := New(Config{}, &fakeChat{}, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := hits("A")
	for _, n := range []int{0, 1} {
		in := base[:n]
		got, err := r.Rerank(context.Background(), "q", in)
		if err != nil {
			t.Fatalf("n=%d Rerank: %v", n, err)
		}
		if len(got) != n {
			t.Errorf("n=%d got %d hits, want %d", n, len(got), n)
		}
	}
}

func TestRerank_ReordersByLLM(t *testing.T) {
	t.Parallel()
	chat := &fakeChat{
		out: kernel.ToolUseResponse{
			ToolUse: &kernel.ToolInvocation{
				Name:  "rerank",
				Input: json.RawMessage(`{"order":[2,0,1]}`),
			},
			StopReason: "tool_use",
		},
	}
	r, err := New(Config{}, chat, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := r.Rerank(context.Background(), "who works at Erebor", hits("A", "B", "C"))
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	wantOrder := []string{"C", "A", "B"}
	for i, h := range got {
		if h.UID != wantOrder[i] {
			t.Errorf("hits[%d].UID = %q, want %q", i, h.UID, wantOrder[i])
		}
	}
}

func TestRerank_MalformedOrderReturnsOriginal(t *testing.T) {
	t.Parallel()
	chat := &fakeChat{
		out: kernel.ToolUseResponse{
			ToolUse: &kernel.ToolInvocation{
				Name:  "rerank",
				Input: json.RawMessage(`{"order":[0,0,1]}`),
			},
			StopReason: "tool_use",
		},
	}
	r, err := New(Config{}, chat, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := hits("A", "B", "C")
	got, err := r.Rerank(context.Background(), "q", in)
	if err == nil {
		t.Fatal("expected error on malformed order, got nil")
	}
	for i, h := range got {
		if h.UID != in[i].UID {
			t.Errorf("hits[%d].UID = %q, want %q (original ordering on error)", i, h.UID, in[i].UID)
		}
	}
}

func TestRerank_LLMErrorReturnsOriginal(t *testing.T) {
	t.Parallel()
	boom := errors.New("ollama unreachable")
	chat := &fakeChat{err: boom}
	r, err := New(Config{}, chat, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := hits("A", "B", "C")
	got, err := r.Rerank(context.Background(), "q", in)
	if err == nil {
		t.Fatal("expected error to bubble up, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want wrap of %v", err, boom)
	}
	if len(got) != len(in) {
		t.Fatalf("got %d hits, want %d", len(got), len(in))
	}
}

// TestRerank_ShortPermutationIsPaddedNotDropped pins the realistic-degrade
// behavior. qwen2.5:7b routinely returns a permutation that is missing one
// or two indices when ranking 10+ similar candidates (observed in the
// LongMemEval corpus: "order length = 8, want 10" with snippets dominated
// by Arwen duplicates). Before the fix the reranker returned the
// original RRF ordering on any short permutation, throwing away the LLM's
// partial reordering work. After: we honor the LLM's order for the indices
// it DID return and pad the rest in their original positions, treating
// "didn't mention" as "less relevant than the ones it did mention".
func TestRerank_ShortPermutationIsPaddedNotDropped(t *testing.T) {
	t.Parallel()
	// LLM returned [2, 0] for an input of length 5 — only indices 2 and 0.
	// Expected output:
	//   - hits[2] first (LLM said it's most relevant)
	//   - hits[0] second (LLM said it's second-most)
	//   - hits[1], hits[3], hits[4] in their original order (LLM omitted them)
	chat := &fakeChat{
		out: kernel.ToolUseResponse{
			ToolUse: &kernel.ToolInvocation{
				Name:  "rerank",
				Input: json.RawMessage(`{"order":[2,0]}`),
			},
			StopReason: "tool_use",
		},
	}
	r, err := New(Config{}, chat, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := hits("A", "B", "C", "D", "E")
	got, err := r.Rerank(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("Rerank: %v (expected partial-permutation to succeed)", err)
	}
	want := []string{"C", "A", "B", "D", "E"}
	if len(got) != len(want) {
		t.Fatalf("got %d hits, want %d", len(got), len(want))
	}
	for i, h := range got {
		if h.UID != want[i] {
			t.Errorf("hits[%d].UID = %q, want %q", i, h.UID, want[i])
		}
	}
}

// TestRerank_DuplicateIndexStillRejected is the boundary case: a
// permutation that contains duplicates is malformed (the LLM hallucinated)
// and must NOT be silently coerced. Returning original ordering with err
// is the right behavior; padding makes no sense here.
func TestRerank_DuplicateIndexStillRejected(t *testing.T) {
	t.Parallel()
	chat := &fakeChat{
		out: kernel.ToolUseResponse{
			ToolUse: &kernel.ToolInvocation{
				Name:  "rerank",
				Input: json.RawMessage(`{"order":[0,0,1]}`),
			},
			StopReason: "tool_use",
		},
	}
	r, err := New(Config{}, chat, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := hits("A", "B", "C")
	got, err := r.Rerank(context.Background(), "q", in)
	if err == nil {
		t.Fatal("expected error on duplicate index, got nil")
	}
	for i, h := range got {
		if h.UID != in[i].UID {
			t.Errorf("hits[%d].UID = %q, want %q (original ordering on error)", i, h.UID, in[i].UID)
		}
	}
}

// TestRerank_OutOfRangeIndexStillRejected: an index outside [0, n) is
// hallucinated and must not be padded into the output.
func TestRerank_OutOfRangeIndexStillRejected(t *testing.T) {
	t.Parallel()
	chat := &fakeChat{
		out: kernel.ToolUseResponse{
			ToolUse: &kernel.ToolInvocation{
				Name:  "rerank",
				Input: json.RawMessage(`{"order":[0,1,7]}`),
			},
			StopReason: "tool_use",
		},
	}
	r, err := New(Config{}, chat, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := hits("A", "B", "C")
	got, err := r.Rerank(context.Background(), "q", in)
	if err == nil {
		t.Fatal("expected error on out-of-range index, got nil")
	}
	if len(got) != len(in) {
		t.Fatalf("got %d hits, want %d (original on error)", len(got), len(in))
	}
}

func TestRerank_NilToolUseReturnsOriginal(t *testing.T) {
	t.Parallel()
	chat := &fakeChat{
		out: kernel.ToolUseResponse{StopReason: "end_turn"},
	}
	r, err := New(Config{}, chat, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := hits("A", "B")
	got, err := r.Rerank(context.Background(), "q", in)
	if err == nil {
		t.Fatal("expected error on nil ToolUse, got nil")
	}
	if len(got) != len(in) {
		t.Errorf("got %d hits, want %d", len(got), len(in))
	}
}
