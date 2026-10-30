package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEnrich is a minimal EnrichmentReporter for tests. queueDepth and
// inflight are set directly; waitFn lets each test fix the drain
// behavior (instant, blocked, or context-cancel).
type fakeEnrich struct {
	queueDepth int
	inflight   atomic.Int64
	waitFn     func(ctx context.Context) error
}

func (f *fakeEnrich) BackgroundWorkDepth() int64 { return f.inflight.Load() }
func (f *fakeEnrich) ExtractionQueueDepth() int  { return f.queueDepth }
func (f *fakeEnrich) WaitForBackgroundWork(ctx context.Context) error {
	if f.waitFn == nil {
		return nil
	}
	return f.waitFn(ctx)
}

type fakeVec struct {
	inflight atomic.Int64
	waitFn   func(ctx context.Context) error
}

func (f *fakeVec) BackgroundVecDepth() int64 { return f.inflight.Load() }
func (f *fakeVec) WaitForBackgroundVec(ctx context.Context) error {
	if f.waitFn == nil {
		return nil
	}
	return f.waitFn(ctx)
}

func TestIdleStatus_ReportsCurrentCounters(t *testing.T) {
	t.Parallel()
	enrich := &fakeEnrich{queueDepth: 3}
	enrich.inflight.Store(5)
	vec := &fakeVec{}
	vec.inflight.Store(2)

	tool := NewIdleStatus(enrich, vec)
	body, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out idleStatusOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.EnrichmentQueueDepth != 3 || out.EnrichmentInflight != 5 || out.VecInflight != 2 {
		t.Errorf("counters wrong: %+v", out)
	}
	if out.IsIdle {
		t.Errorf("isIdle should be false when work is in flight: %+v", out)
	}
}

func TestIdleStatus_IsIdleWhenAllZero(t *testing.T) {
	t.Parallel()
	tool := NewIdleStatus(&fakeEnrich{}, &fakeVec{})
	body, _ := tool.Invoke(context.Background(), nil)
	var out idleStatusOutput
	_ = json.Unmarshal(body, &out)
	if !out.IsIdle {
		t.Errorf("isIdle should be true when all lanes are 0: %+v", out)
	}
}

func TestIdleStatus_NilReportersAreInert(t *testing.T) {
	t.Parallel()
	tool := NewIdleStatus(nil, nil)
	body, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke with nil reporters: %v", err)
	}
	var out idleStatusOutput
	_ = json.Unmarshal(body, &out)
	if !out.IsIdle {
		t.Errorf("nil reporters should report idle: %+v", out)
	}
}

func TestWaitForIdle_ReturnsImmediatelyWhenAlreadyIdle(t *testing.T) {
	t.Parallel()
	enrich := &fakeEnrich{} // waitFn nil → returns immediately
	vec := &fakeVec{}
	tool := NewWaitForIdle(enrich, vec)

	start := time.Now()
	body, err := tool.Invoke(context.Background(), nil)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if dur > 100*time.Millisecond {
		t.Errorf("idle wait took too long: %s", dur)
	}
	var out waitForIdleOutput
	_ = json.Unmarshal(body, &out)
	if !out.IsIdle || out.TimedOut {
		t.Errorf("expected idle + not timed out: %+v", out)
	}
}

func TestWaitForIdle_HonorsTimeout(t *testing.T) {
	t.Parallel()
	enrich := &fakeEnrich{
		waitFn: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	enrich.inflight.Store(1)
	tool := NewWaitForIdle(enrich, nil)

	req := json.RawMessage(`{"timeoutSeconds":1}`)
	start := time.Now()
	body, err := tool.Invoke(context.Background(), req)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if dur < 900*time.Millisecond || dur > 2*time.Second {
		t.Errorf("timeout duration off: %s", dur)
	}
	var out waitForIdleOutput
	_ = json.Unmarshal(body, &out)
	if !out.TimedOut || out.IsIdle {
		t.Errorf("expected timedOut=true, isIdle=false: %+v", out)
	}
	if out.EnrichmentInflight != 1 {
		t.Errorf("expected enrichmentInflight=1 at timeout: %+v", out)
	}
}

func TestWaitForIdle_DrainsEnrichmentBeforeVec(t *testing.T) {
	t.Parallel()
	// Pin the documented invariant: enrichment is drained first.
	// If vec's WaitForBackgroundVec is invoked before enrichment's
	// returns, the order pin would fire and the test would fail.
	var enrichDone atomic.Bool
	enrich := &fakeEnrich{
		waitFn: func(_ context.Context) error {
			time.Sleep(50 * time.Millisecond)
			enrichDone.Store(true)
			return nil
		},
	}
	vec := &fakeVec{
		waitFn: func(_ context.Context) error {
			if !enrichDone.Load() {
				return errors.New("vec drained before enrichment")
			}
			return nil
		},
	}
	tool := NewWaitForIdle(enrich, vec)
	body, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out waitForIdleOutput
	_ = json.Unmarshal(body, &out)
	if !out.IsIdle {
		t.Errorf("expected idle after staged drain: %+v", out)
	}
}
