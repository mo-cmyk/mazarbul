package kernelboot_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

func TestOpenLog_AppendEpisodeRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	payload, err := json.Marshal(map[string]any{
		"text":       "kernelboot accepted this episode end-to-end",
		"source":     "cli:init",
		"confidence": 1.0,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	receipts, err := log.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindEpisode,
		Source:     "cli:init",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(receipts))
	}
	if receipts[0].UID == "" {
		t.Error("receipt UID empty; writer must assign a ULID")
	}
	if receipts[0].CreateTime.IsZero() {
		t.Error("receipt CreateTime zero; writer must assign one")
	}

	// The JSONL file must have landed under events/YYYY/MM/DD.jsonl.
	matches, err := filepath.Glob(filepath.Join(dataDir, "events", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob jsonl: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("want 1 jsonl file, got %d (%v)", len(matches), matches)
	}
}

func TestOpenLog_CloserIsIdempotent(t *testing.T) {
	t.Parallel()

	log, closeLog, err := kernelboot.OpenLog(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	_ = log

	if err := closeLog(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := closeLog(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestOpenLog_RejectsMalformedEpisode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	// Missing the required "text" field; schema must reject.
	bad, err := json.Marshal(map[string]any{
		"source":     "cli:init",
		"confidence": 1.0,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = log.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindEpisode,
		Source:     "cli:init",
		Confidence: 1.0,
		Payload:    bad,
	}})
	if err == nil {
		t.Fatal("Append accepted payload missing required field; want validation error")
	}

	// Disk must be clean: a rejected event must not create the events dir.
	if _, statErr := os.Stat(filepath.Join(dataDir, "events", "index.bolt")); statErr == nil {
		// bbolt may have created the sidecar on open; that's fine. The
		// invariant is that no JSONL file exists.
		matches, _ := filepath.Glob(filepath.Join(dataDir, "events", "*", "*", "*.jsonl"))
		if len(matches) != 0 {
			t.Errorf("rejected payload created JSONL files: %v", matches)
		}
	}
}

// TestOpenLog_RegistersPromptRenderedKind verifies that OpenLog wires the
// prompt.rendered schema so that a MemoryRegistry's audit events can be
// validated by the ontology without an ErrSchemaNotRegistered error.
func TestOpenLog_RegistersPromptRenderedKind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	log, closeLog, err := kernelboot.OpenLog(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	// Append a well-formed prompt.rendered event; the schema validator inside
	// the event log must accept it (no ErrSchemaNotRegistered / validation error).
	payload, err := json.Marshal(prompts.RenderedPayload{
		TemplateName:    "recall.query",
		TemplateVersion: 1,
		InputHash:       "a3f1b2c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2",
		OutputLen:       42,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if _, appendErr := log.Append(ctx, []kernel.Event{{
		Kind:       prompts.KindRendered,
		Source:     "prompts/registry",
		Confidence: 1.0,
		Payload:    payload,
	}}); appendErr != nil {
		t.Fatalf("Append prompt.rendered: %v", appendErr)
	}
}
