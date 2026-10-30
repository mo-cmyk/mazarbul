package ontology_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestEpisode_FileEventLog_AcceptsRealisticCallerPayload proves the
// alignment between the Episode schema and FileEventLog's payload-only
// validation contract for the realistic caller: a caller that does NOT
// pre-fabricate writer-owned fields (name/uid/createTime) in the payload.
// The writer assigns those on the outer Event; the payload schema must
// not require them, otherwise normal Append calls would always fail.
//
// Covers Greptile 3/5 iter-3 finding: "Validate a payload shape that
// excludes writer-owned fields" — the schema is now payload-shape only,
// so the persisted row cannot suffer event.uid vs payload.uid drift.
func TestEpisode_FileEventLog_AcceptsRealisticCallerPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fake := newFakeOntology()
	if err := ontology.RegisterEpisode(ctx, fake); err != nil {
		t.Fatalf("register Episode: %v", err)
	}

	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, fake,
		kernel.WithClock(func() time.Time { return clock }))

	// Realistic payload: substantive fields only. No name/uid/createTime
	// because the caller does not know the UID until Append assigns one.
	payload := []byte(`{"text":"the writer accepted this episode end-to-end","source":"cli:remember","confidence":1.0}`)

	receipts, err := w.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindEpisode,
		Source:     "cli:remember",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		t.Fatalf("Append realistic Episode payload: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(receipts))
	}

	wantPath := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if _, err := os.ReadFile(wantPath); err != nil {
		t.Errorf("expected JSONL file at %s: %v", wantPath, err)
	}
}

// TestEpisode_FileEventLog_RejectsMalformedPayload proves the writer
// surfaces schema-validation failures as ErrInvalidEvent when a payload
// fails the Episode schema. Two cases: (a) missing required field, and
// (b) malformed JSON. Together they cover both the registered-schema-
// rejects-payload path and the wrap-as-ErrInvalidEvent contract.
//
// Covers Greptile 3/5 findings "Malformed Episode payloads can still be
// appended because missing schema lookups are treated as allowed" and
// "The new Episode schema is not reached by the current remember event
// validation path".
func TestEpisode_FileEventLog_RejectsMalformedPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fake := newFakeOntology()
	if err := ontology.RegisterEpisode(ctx, fake); err != nil {
		t.Fatalf("register Episode: %v", err)
	}
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, fake,
		kernel.WithClock(func() time.Time { return clock }))

	uid := ulid.Make().String()
	// Missing the required "text" field.
	bad := map[string]any{
		"name":       "episodes/" + uid,
		"uid":        uid,
		"createTime": "2026-05-24T12:00:00Z",
		"source":     "cli:remember",
		"confidence": 1.0,
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal bad payload: %v", err)
	}

	_, err = w.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindEpisode,
		Source:     "cli:remember",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("want ErrInvalidEvent for missing required field, got %v", err)
	}

	// Disk must be clean: a single rejected event must never touch the log.
	entries, _ := os.ReadDir(dataDir)
	for _, e := range entries {
		if e.Name() == "events" {
			t.Errorf("rejected Episode payload touched the events dir")
		}
	}
}

// TestEpisode_FileEventLog_RejectsUnregisteredKind proves the strict
// contract: if Episode is NOT registered with the writer's ontology, an
// event with Kind=episode is rejected (no permissive bypass). This
// is the "schema must be reached" half of Greptile 3/5 finding 1.
func TestEpisode_FileEventLog_RejectsUnregisteredKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Note: NO RegisterEpisode call. The ontology is empty.
	fake := newFakeOntology()
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, fake,
		kernel.WithClock(func() time.Time { return clock }))

	uid := ulid.Make().String()
	episode := ontology.Episode{
		Name:       "episodes/" + uid,
		UID:        uid,
		CreateTime: clock,
		Text:       "would have been valid against Episode schema",
		Source:     "cli:remember",
		Confidence: 1.0,
	}
	payload, err := json.Marshal(episode)
	if err != nil {
		t.Fatalf("marshal episode: %v", err)
	}

	_, err = w.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindEpisode,
		Source:     "cli:remember",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("want ErrInvalidEvent for unregistered kind, got %v", err)
	}
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("want wrapped ErrNotFound, got %v", err)
	}
}
