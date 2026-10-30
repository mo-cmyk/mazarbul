package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/cognitive"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// fakeTrigger is a CognitiveTrigger double returning canned results/errors.
type fakeTrigger struct {
	reflect    cognitive.Result
	reflectErr error
	consol     cognitive.Result
	consolErr  error
}

func (f *fakeTrigger) RunReflection(context.Context) (cognitive.Result, error) {
	return f.reflect, f.reflectErr
}

func (f *fakeTrigger) RunConsolidation(context.Context) (cognitive.Result, error) {
	return f.consol, f.consolErr
}

type triggerOutput struct {
	AppendedEventUIDs []string `json:"appendedEventUids"`
	AppendedCount     int      `json:"appendedCount"`
	Skipped           string   `json:"skipped"`
}

// TestTriggerReflection_AppendedResult asserts the tool reports the appended
// event UIDs and a matching count.
func TestTriggerReflection_AppendedResult(t *testing.T) {
	t.Parallel()
	sv := &fakeTrigger{reflect: cognitive.Result{AppendedEventUIDs: []string{"ev1", "ev2"}}}
	tool := tools.NewTriggerReflection(sv)

	body, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out triggerOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode output: %v", uerr)
	}
	if out.AppendedCount != 2 || len(out.AppendedEventUIDs) != 2 {
		t.Errorf("count=%d uids=%v, want 2 / [ev1 ev2]", out.AppendedCount, out.AppendedEventUIDs)
	}
	if out.Skipped != "" {
		t.Errorf("skipped = %q, want empty", out.Skipped)
	}
}

// TestTriggerReflection_SkippedSurfaced asserts a no-append pass surfaces its
// skipped reason with a zero count and an empty (non-null) uid array.
func TestTriggerReflection_SkippedSurfaced(t *testing.T) {
	t.Parallel()
	sv := &fakeTrigger{reflect: cognitive.Result{Skipped: "breaker-open"}}
	tool := tools.NewTriggerReflection(sv)

	body, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out triggerOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode output: %v", uerr)
	}
	if out.Skipped != "breaker-open" {
		t.Errorf("skipped = %q, want breaker-open", out.Skipped)
	}
	if out.AppendedCount != 0 {
		t.Errorf("count = %d, want 0", out.AppendedCount)
	}
	if out.AppendedEventUIDs == nil {
		t.Error("appendedEventUids is null; schema requires an array (normalize nil to [])")
	}
}

// TestTriggerConsolidation_ErrorWrapped asserts a job error becomes an MCP
// internal error rather than a panic or silent success.
func TestTriggerConsolidation_ErrorWrapped(t *testing.T) {
	t.Parallel()
	sv := &fakeTrigger{consolErr: errors.New("boom")}
	tool := tools.NewTriggerConsolidation(sv)

	if _, err := tool.Invoke(context.Background(), nil); err == nil {
		t.Fatal("expected an error from a failing consolidation, got nil")
	}
}

// TestTriggerTools_Names pins the advertised tool names.
func TestTriggerTools_Names(t *testing.T) {
	t.Parallel()
	if got := tools.NewTriggerReflection(nil).Name(); got != "triggerReflection" {
		t.Errorf("reflection name = %q", got)
	}
	if got := tools.NewTriggerConsolidation(nil).Name(); got != "triggerConsolidation" {
		t.Errorf("consolidation name = %q", got)
	}
}
