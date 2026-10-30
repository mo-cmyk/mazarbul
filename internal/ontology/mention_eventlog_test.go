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

// TestMention_FileEventLog_AcceptsRealisticCallerPayload proves the
// alignment between the Mention schema and FileEventLog's payload-only
// validation contract for the realistic caller: an extractor that does
// NOT pre-fabricate writer-owned fields (name/uid/createTime) in the
// payload. The writer assigns those on the outer Event; the payload
// schema must not require them, otherwise normal Append calls would
// always fail.
//
// Covers Greptile #93 P1 "Writer-owned fields reject appends" — the
// schema is now payload-shape only, so the persisted row cannot suffer
// event.uid vs payload.uid drift. Mirrors the Episode-side proof in
// TestEpisode_FileEventLog_AcceptsRealisticCallerPayload.
func TestMention_FileEventLog_AcceptsRealisticCallerPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("register Mention: %v", err)
	}

	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, fake,
		kernel.WithClock(func() time.Time { return clock }))

	// Realistic payload: substantive fields only. No name/uid/createTime
	// because the caller does not know the UID until Append assigns one.
	// v0.2.0 requires extractionText, sourceSpan, and groundingScore.
	sourceUID := ulid.Make().String()
	payload := []byte(`{"kind":"person","displayName":"Frodo","sourceEventUid":"` + sourceUID + `","disambiguationCtx":"fundraising chat, Erebor engineer","extractionText":"Frodo","sourceSpan":{"startChar":0,"endChar":7},"groundingScore":1.0}`)

	receipts, err := w.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindMention,
		Source:     "extractor:m1",
		Confidence: 0.9,
		Payload:    payload,
	}})
	if err != nil {
		t.Fatalf("Append realistic Mention payload: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(receipts))
	}

	wantPath := filepath.Join(dataDir, "events", "2026", "05", "24.jsonl")
	if _, err := os.ReadFile(wantPath); err != nil {
		t.Errorf("expected JSONL file at %s: %v", wantPath, err)
	}
}

// TestMention_FileEventLog_RejectsMalformedPayload proves the writer
// surfaces schema-validation failures as ErrInvalidEvent when a Mention
// payload misses a substantive-required field (here, kind). Mirrors the
// Episode-side rejection test.
func TestMention_FileEventLog_RejectsMalformedPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("register Mention: %v", err)
	}
	dataDir := t.TempDir()
	clock := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	w := kernel.NewFileEventLog(dataDir, fake,
		kernel.WithClock(func() time.Time { return clock }))

	// Missing the substantive-required "kind" field.
	bad := map[string]any{
		"displayName":    "Frodo",
		"sourceEventUid": ulid.Make().String(),
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal bad payload: %v", err)
	}

	_, err = w.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindMention,
		Source:     "extractor:m1",
		Confidence: 0.9,
		Payload:    payload,
	}})
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Fatalf("want ErrInvalidEvent for missing kind, got %v", err)
	}

	// Disk must be clean: a single rejected event must never touch the log.
	entries, _ := os.ReadDir(dataDir)
	for _, e := range entries {
		if e.Name() == "events" {
			t.Errorf("rejected Mention payload touched the events dir")
		}
	}
}
