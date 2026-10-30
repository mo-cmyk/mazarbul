package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestConnectorWatermarkAdvanced_Register_SchemaRoundtrips registers the schema
// against the real MemoryOntology, asserts its identity, and round-trips a
// populated event through marshal/validate/unmarshal.
func TestConnectorWatermarkAdvanced_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterConnectorWatermarkAdvanced(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := o.Schema(ctx, ontology.KindConnectorWatermarkAdvanced)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindConnectorWatermarkAdvanced {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindConnectorWatermarkAdvanced)
	}
	if got.Version != ontology.VersionConnectorWatermarkAdvanced {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionConnectorWatermarkAdvanced)
	}

	original := ontology.ConnectorWatermarkAdvancedEvent{
		Connector:   "local-git",
		Stream:      "commits",
		Cursor:      json.RawMessage(`{"lastSHA":"deadbeef"}`),
		Sequence:    7,
		AdvanceTime: time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindConnectorWatermarkAdvanced, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.ConnectorWatermarkAdvancedEvent
	if err := json.Unmarshal(payload, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rePayload, err := json.Marshal(rt)
	if err != nil {
		t.Fatalf("marshal rt: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip mismatch\n  original: %s\n  got:      %s", payload, rePayload)
	}
}

// TestValidateCursorShape covers the two structural guards independent of
// sequencing: the size cap and the well-formed-JSON-object requirement.
func TestValidateCursorShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cursor  json.RawMessage
		wantErr bool
	}{
		{"valid object", json.RawMessage(`{"lastSHA":"abc"}`), false},
		{"empty object", json.RawMessage(`{}`), false},
		{"array not object", json.RawMessage(`["a","b"]`), true},
		{"number not object", json.RawMessage(`42`), true},
		{"string not object", json.RawMessage(`"abc"`), true},
		{"garbage", json.RawMessage(`{not json`), true},
		{"empty bytes", json.RawMessage(``), true},
		{"oversized", json.RawMessage(`{"k":"` + strings.Repeat("x", ontology.MaxCursorBytes) + `"}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ontology.ValidateCursorShape(tc.cursor)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateCursorShape(%s) = nil, want error", tc.cursor)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateCursorShape(%s) = %v, want nil", tc.cursor, err)
			}
		})
	}
}

// TestValidateCursor covers the monotonicity arm layered on top of the shape
// guards: a strictly-greater sequence advances, a regression or non-advancing
// sequence FAILs (unless done allows a terminal re-advance at the same seq).
func TestValidateCursor(t *testing.T) {
	t.Parallel()
	good := json.RawMessage(`{"lastSHA":"abc"}`)
	cases := []struct {
		name    string
		prev    uint64
		next    uint64
		cursor  json.RawMessage
		done    bool
		wantErr bool
	}{
		{"first connect", 0, 1, good, false, false},
		{"advance", 5, 6, good, false, false},
		{"big jump", 5, 100, good, false, false},
		{"regression fails", 6, 5, good, false, true},
		{"equal not done fails", 6, 6, good, false, true},
		{"equal done ok", 6, 6, good, true, false},
		{"regression even when done fails", 6, 5, good, true, true},
		{"bad shape fails before monotonicity", 0, 1, json.RawMessage(`[]`), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ontology.ValidateCursor(tc.prev, tc.next, tc.cursor, tc.done)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateCursor(prev=%d next=%d done=%v) = nil, want error", tc.prev, tc.next, tc.done)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateCursor(prev=%d next=%d done=%v) = %v, want nil", tc.prev, tc.next, tc.done, err)
			}
		})
	}
}
