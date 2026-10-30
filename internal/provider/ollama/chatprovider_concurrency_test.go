package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestChatProvider_ToolUse_BoundsConcurrency pins the burst-timeout fix: the
// ChatProvider must never have more than maxConcurrent /api/chat requests in
// flight at once, regardless of how many callers fire concurrently. Before
// the fix, extraction dispatched unbounded concurrent ToolUse calls; against a
// single-slot Ollama they queued past the per-request HTTP timeout and the
// tail requests were lost (6/8 in a real burst).
//
// The test is deterministic, not timing-based: the chat handler blocks until
// the test releases it, so we can observe exactly how many requests the
// provider lets through the gate.
func TestChatProvider_ToolUse_BoundsConcurrency(t *testing.T) {
	const maxConcurrent = 3
	const callers = 9

	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	arrived := make(chan struct{}, callers)
	release := make(chan struct{})

	chatHandler := func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			cur := maxObserved.Load()
			if n <= cur || maxObserved.CompareAndSwap(cur, n) {
				break
			}
		}
		arrived <- struct{}{}
		<-release // hold the inference slot until the test releases it
		inFlight.Add(-1)
		_, _ = w.Write(toolCallChatJSON(t, "extract_entity", json.RawMessage(`{"name":"Merry"}`)))
	}

	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel, "llama3.1:8b"))
		case "/api/version":
			_, _ = w.Write([]byte(versionJSON("0.5.0")))
		case "/api/chat":
			chatHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	c, err := New(context.Background(), Options{
		BaseURL:    srv.srv.URL,
		ChatModel:  "llama3.1:8b",
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, err := NewChatProvider(c, maxConcurrent)
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, callErr := p.ToolUse(context.Background(), kernel.ToolUseRequest{
				Tool:     sampleToolSchema(),
				Messages: sampleKernelMessages(),
			})
			errs[idx] = callErr
		}(i)
	}

	// Exactly maxConcurrent requests should reach the handler; the rest must
	// block in acquireChatSlot, Go-side, never touching the wire.
	for i := range maxConcurrent {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/%d requests reached the handler; slot acquire is stuck", i, maxConcurrent)
		}
	}
	if got := inFlight.Load(); got != maxConcurrent {
		t.Fatalf("in-flight = %d, want exactly %d", got, maxConcurrent)
	}
	// No (maxConcurrent+1)th request may slip through while the first batch
	// still holds every slot.
	select {
	case <-arrived:
		t.Fatalf("a request exceeded the concurrency cap of %d", maxConcurrent)
	case <-time.After(100 * time.Millisecond):
	}

	close(release) // let the held requests finish; the rest drain in waves
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("caller %d: ToolUse error: %v", i, e)
		}
	}
	if got := maxObserved.Load(); got > maxConcurrent {
		t.Fatalf("observed %d concurrent /api/chat requests, cap is %d", got, maxConcurrent)
	}
	if got := maxObserved.Load(); got != maxConcurrent {
		t.Fatalf("observed peak concurrency %d, want exactly %d (limiter must use the configured value, not collapse to serial)", got, maxConcurrent)
	}
}

// TestNewChatProvider_DefaultsToSingleSlot verifies that a non-positive
// maxConcurrent falls back to the safe single-slot default rather than an
// unbounded channel (which would reintroduce the burst-timeout bug).
func TestNewChatProvider_DefaultsToSingleSlot(t *testing.T) {
	c := &Client{} // construction-only; no calls are made
	for _, maxConcurrent := range []int{0, -1} {
		p, err := NewChatProvider(c, maxConcurrent)
		if err != nil {
			t.Fatalf("NewChatProvider(c, %d): %v", maxConcurrent, err)
		}
		if got := p.MaxConcurrent(); got != defaultChatConcurrency {
			t.Fatalf("maxConcurrent=%d: MaxConcurrent() = %d, want %d", maxConcurrent, got, defaultChatConcurrency)
		}
	}
}

// TestChatProvider_ToolUse_RespectsContextCancellation pins the second branch
// of acquireChatSlot: when every slot is held and the caller's ctx is
// cancelled, ToolUse must return a wrapped context.Canceled WITHOUT ever
// reaching the wire. A regression that dropped the ctx.Done() case from the
// acquire select would compile and silently block forever instead.
func TestChatProvider_ToolUse_RespectsContextCancellation(t *testing.T) {
	var handlerHits atomic.Int32
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})

	chatHandler := func(w http.ResponseWriter, _ *http.Request) {
		handlerHits.Add(1)
		arrived <- struct{}{}
		<-release // hold the only slot until the test releases it
		_, _ = w.Write(toolCallChatJSON(t, "extract_entity", json.RawMessage(`{"name":"Merry"}`)))
	}
	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel, "llama3.1:8b"))
		case "/api/version":
			_, _ = w.Write([]byte(versionJSON("0.5.0")))
		case "/api/chat":
			chatHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	c, err := New(context.Background(), Options{
		BaseURL:    srv.srv.URL,
		ChatModel:  "llama3.1:8b",
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, err := NewChatProvider(c, 1) // single slot
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}

	// Holder occupies the one slot (and completes the lazy version probe, so
	// the cancelled caller below goes straight to acquireChatSlot).
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		_, callErr := p.ToolUse(context.Background(), kernel.ToolUseRequest{
			Tool: sampleToolSchema(), Messages: sampleKernelMessages(),
		})
		if callErr != nil {
			t.Errorf("holder ToolUse: %v", callErr)
		}
	}()
	<-arrived // slot is now held

	// Cancelled caller: must fail in acquire, never hit the handler.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.ToolUse(ctx, kernel.ToolUseRequest{
		Tool: sampleToolSchema(), Messages: sampleKernelMessages(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ToolUse: err = %v, want context.Canceled", err)
	}
	if got := handlerHits.Load(); got != 1 {
		t.Fatalf("handler hit %d times; cancelled caller must not reach the wire (want 1)", got)
	}

	close(release)
	<-holderDone
}
