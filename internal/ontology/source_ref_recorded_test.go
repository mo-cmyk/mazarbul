package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestSourceRefRecorded_Register_SchemaRoundtrips registers the schema against
// the real MemoryOntology, asserts its identity, and round-trips a fully
// populated event through marshal/validate/unmarshal.
func TestSourceRefRecorded_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterSourceRefRecorded(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := o.Schema(ctx, ontology.KindSourceRefRecorded)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindSourceRefRecorded {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindSourceRefRecorded)
	}
	if got.Version != ontology.VersionSourceRefRecorded {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionSourceRefRecorded)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	original := ontology.SourceRefRecordedEvent{
		UID:         ontology.ULID(ulid.Make().String()),
		SubjectUID:  ontology.ULID(ulid.Make().String()),
		SourceType:  "email",
		Connector:   "gmail",
		Stream:      "INBOX",
		ExternalID:  "msg-42",
		ExternalURL: "https://mail.example.com/msg-42",
		ContentHash: strings.Repeat("a", 64),
		FetchTime:   time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
		DedupKey:    "gmail:msg-42",
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindSourceRefRecorded, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.SourceRefRecordedEvent
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

// TestSourceRefRecorded_Validate_MissingRequiredField drops each required field
// and asserts validation fails. The optional fields (connector/stream/
// externalId/externalUrl/contentHash) are deliberately absent from `complete`
// so the required set is exercised in isolation.
func TestSourceRefRecorded_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	complete := map[string]any{
		"uid":        ulid.Make().String(),
		"subjectUid": ulid.Make().String(),
		"sourceType": "email",
		"fetchTime":  "2026-06-15T10:00:00Z",
		"dedupKey":   "gmail:msg-42",
	}
	for _, field := range []string{"uid", "subjectUid", "sourceType", "fetchTime", "dedupKey"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterSourceRefRecorded(ctx, o); err != nil {
				t.Fatalf("register: %v", err)
			}
			bad := make(map[string]any, len(complete))
			for k, v := range complete {
				if k == field {
					continue
				}
				bad[k] = v
			}
			payload, err := json.Marshal(bad)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := o.Validate(ctx, ontology.KindSourceRefRecorded, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}

// TestSourceRefUID_Deterministic asserts the dedupKey→uid derivation is a pure
// function (stable across calls), produces a valid ULID, and separates distinct
// keys. This is the property that keeps source_refs PRIMARY KEY(uid) and
// UNIQUE(dedup_key) 1:1, so re-recording the same item resolves to one row.
func TestSourceRefUID_Deterministic(t *testing.T) {
	t.Parallel()

	const key = "gmail:msg-42"
	first := ontology.SourceRefUID(key)
	second := ontology.SourceRefUID(key)
	if first != second {
		t.Errorf("SourceRefUID(%q) not stable: %q != %q", key, first, second)
	}

	// The derived value parses as a real ULID (so it satisfies the
	// ^[0-9A-HJKMNP-TV-Z]{26}$ resource-name pattern).
	if _, err := ulid.Parse(string(first)); err != nil {
		t.Errorf("SourceRefUID(%q) = %q is not a valid ULID: %v", key, first, err)
	}

	if other := ontology.SourceRefUID("slack:c1:ts2"); other == first {
		t.Errorf("distinct dedupKeys collided to the same uid %q", other)
	}
	if empty := ontology.SourceRefUID(""); empty == first {
		t.Errorf("empty dedupKey collided with %q", first)
	}
}

// TestSourceRefRecorded_Schema_Cached verifies the schema cache is stable across
// calls (the sync.OnceValue contract).
func TestSourceRefRecorded_Schema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.SourceRefRecordedSchema()
	second := ontology.SourceRefRecordedSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("SourceRefRecordedSchema returned different Definitions across calls")
	}
}
