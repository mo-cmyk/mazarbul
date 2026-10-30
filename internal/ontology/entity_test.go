package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// entityFakeOntology is a map-backed test double for kernel.Ontology,
// duplicating the shape used in episode_test.go so the two kinds remain
// independently testable. Lifting the helper into a shared package is a
// follow-up cleanup once Mention (#35) and Relation (#36) land and the
// third copy proves the abstraction.
type entityFakeOntology struct {
	schemas  map[string]kernel.Schema
	compiled map[string]*jsonschema.Schema
}

func newEntityFakeOntology() *entityFakeOntology {
	return &entityFakeOntology{
		schemas:  make(map[string]kernel.Schema),
		compiled: make(map[string]*jsonschema.Schema),
	}
}

func (f *entityFakeOntology) Schema(_ context.Context, kind string) (kernel.Schema, error) {
	s, ok := f.schemas[kind]
	if !ok {
		return kernel.Schema{}, kernel.ErrNotFound
	}
	return s, nil
}

func (f *entityFakeOntology) Register(_ context.Context, s kernel.Schema) error {
	if _, ok := f.schemas[s.Kind]; ok {
		return kernel.ErrSchemaConflict
	}
	f.schemas[s.Kind] = s
	return nil
}

func (f *entityFakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	out := make([]kernel.Schema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, s)
	}
	return out, nil
}

func (f *entityFakeOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
	compiled, ok := f.compiled[kind]
	if !ok {
		s, err := f.Schema(ctx, kind)
		if err != nil {
			return err
		}
		var doc any
		if err := json.Unmarshal(s.Definition, &doc); err != nil {
			return fmt.Errorf("unmarshal schema: %w", err)
		}
		c := jsonschema.NewCompiler()
		// AssertFormat mirrors kernel.MemoryOntology.Validate so the fake
		// rejects malformed timestamps the same way production does.
		c.AssertFormat()
		const resourceURL = "inline://schema.json"
		if err := c.AddResource(resourceURL, doc); err != nil {
			return fmt.Errorf("add resource: %w", err)
		}
		built, err := c.Compile(resourceURL)
		if err != nil {
			return fmt.Errorf("compile schema: %w", err)
		}
		f.compiled[kind] = built
		compiled = built
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	return compiled.Validate(decoded)
}

// TestEntity_Register_SchemaRoundtrips builds an Entity with every
// field populated, registers the schema with a fake Ontology, marshals
// to JSON, validates against the registered schema, unmarshals back,
// and asserts the round-trip is byte-stable plus that the timestamp
// survives unchanged. Covers AC4 (schema generated + registered) and
// AC5 (round-trip test).
func TestEntity_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := fake.Schema(ctx, ontology.KindEntity)
	if err != nil {
		t.Fatalf("lookup registered schema: %v", err)
	}
	if got.Kind != ontology.KindEntity {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindEntity)
	}
	if got.Version != ontology.VersionEntity {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionEntity)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	uid := ulid.Make().String()
	original := ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo Baggins",
		Confidence:       0.95,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		AlternateNames:   []string{"F. Baggins", "frodo"},
		Identifiers: []ontology.Identifier{
			{Type: "email", Value: "frodo@erebor.example"},
			{Type: "github_user_id", Value: "12345678"},
		},
		MentionUIDs: []ontology.ULID{ontology.ULID(ulid.Make().String())},
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err != nil {
		t.Fatalf("validate original payload: %v", err)
	}

	var roundTripped ontology.Entity
	if err := json.Unmarshal(payload, &roundTripped); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	rePayload, err := json.Marshal(roundTripped)
	if err != nil {
		t.Fatalf("marshal round-trip: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip JSON mismatch\n original: %s\n got:      %s", payload, rePayload)
	}
	if !original.CreateTime.Equal(roundTripped.CreateTime) {
		t.Errorf("createTime drift: original=%v got=%v", original.CreateTime, roundTripped.CreateTime)
	}
	if roundTripped.Kind != original.Kind {
		t.Errorf("kind drift: original=%q got=%q", original.Kind, roundTripped.Kind)
	}
	if roundTripped.ResolutionStatus != original.ResolutionStatus {
		t.Errorf("resolutionStatus drift: original=%q got=%q", original.ResolutionStatus, roundTripped.ResolutionStatus)
	}
}

// TestEntity_Register_AgainstRealMemoryOntology exercises RegisterEntity
// against the production kernel.MemoryOntology (not the fake), which
// applies semver.IsValid to schema.Version. Catches regressions where
// VersionEntity stops being a full MAJOR.MINOR.PATCH triplet (Greptile
// PR #82 P1 found that "v0.1" is rejected; "v0.1.0" is required).
func TestEntity_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntity(ctx, o); err != nil {
		t.Fatalf("RegisterEntity against real MemoryOntology: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindEntity)
	if err != nil {
		t.Fatalf("Schema lookup after register: %v", err)
	}
	if got.Version != ontology.VersionEntity {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionEntity)
	}
}

// TestEntity_Register_RejectsDoubleRegistration verifies that a second
// call to Register without a higher Version surfaces
// kernel.ErrSchemaConflict per the "first registration wins" rule.
func TestEntity_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := ontology.RegisterEntity(ctx, fake)
	if !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestEntity_Register_ConflictBumpsKind asserts that an attempt to
// re-register a different Definition under the same Kind surfaces
// kernel.ErrSchemaConflict.
func TestEntity_Register_ConflictBumpsKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	conflicting := kernel.Schema{
		Kind:       ontology.KindEntity,
		Version:    ontology.VersionEntity,
		Definition: json.RawMessage(`{"type":"object"}`),
		Provenance: "core",
	}
	if err := fake.Register(ctx, conflicting); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("conflicting Register err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestEntity_Validate_MissingRequiredField table-drives the negative
// case across every required field: drop the field, assert validation
// fails with a typed *jsonschema.ValidationError, and assert the error
// mentions the dropped field name. Covers AC2 (required fields) and
// AC5 (negative test).
func TestEntity_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	complete := map[string]any{
		"name":             "entities/" + uid,
		"uid":              uid,
		"kind":             string(ontology.EntityKindPerson),
		"displayName":      "Frodo Baggins",
		"confidence":       0.95,
		"resolutionStatus": string(ontology.ResolutionConfirmed),
		"createTime":       "2026-05-24T12:00:00Z",
	}

	for _, field := range []string{"name", "uid", "kind", "displayName", "confidence", "resolutionStatus", "createTime"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newEntityFakeOntology()
			if err := ontology.RegisterEntity(ctx, fake); err != nil {
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
			err = fake.Validate(ctx, ontology.KindEntity, payload)
			if err == nil {
				t.Fatalf("expected validation error for missing %q, got nil", field)
			}
			var ve *jsonschema.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected *jsonschema.ValidationError, got %T: %v", err, err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(field)) {
				t.Errorf("expected error to mention %q, got: %v", field, err)
			}
		})
	}
}

// TestEntity_Validate_RejectsBogusULIDArrayItem asserts the schema
// enforces the ULID pattern on each element of mentionUids, not just
// the array type.
func TestEntity_Validate_RejectsBogusULIDArrayItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	bad := map[string]any{
		"name":             "entities/" + uid,
		"uid":              uid,
		"kind":             string(ontology.EntityKindPerson),
		"displayName":      "Frodo Baggins",
		"confidence":       0.95,
		"resolutionStatus": string(ontology.ResolutionConfirmed),
		"createTime":       "2026-05-24T12:00:00Z",
		"mentionUids":      []string{"not-a-ulid"},
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err == nil {
		t.Error("expected validation error for bogus mentionUids entry, got nil")
	}
}

// TestEntity_Validate_KindPattern asserts that the EntityKind open-enum
// pattern accepts the six core kinds and valid namespaced extension kinds,
// and rejects strings that violate the pattern. The kind field is no
// longer a closed enum (P2 dynamic-ontology open-enum change): any
// correctly-patterned string is accepted at the schema level; registry
// membership is enforced separately via a warning-accept policy.
func TestEntity_Validate_KindPattern(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	base := map[string]any{
		"name":             "entities/" + uid,
		"uid":              uid,
		"displayName":      "Frodo Baggins",
		"confidence":       0.95,
		"resolutionStatus": string(ontology.ResolutionConfirmed),
		"createTime":       "2026-05-24T12:00:00Z",
	}

	accepting := []string{
		// Six core kinds.
		string(ontology.EntityKindPerson),
		string(ontology.EntityKindOrganization),
		string(ontology.EntityKindPlace),
		string(ontology.EntityKindProject),
		string(ontology.EntityKindEvent),
		string(ontology.EntityKindDocument),
		// Valid extension kinds.
		"user:deal",
		"extension:schema_org:Person",
		"ns:sub_kind",
	}
	rejecting := []string{
		"",          // empty
		"Fish",      // uppercase start
		"123abc",    // digit start
		"user:",     // trailing colon
		":deal",     // leading colon
		"has space", // space inside
		"PERSON",    // all-caps
	}

	for _, kind := range accepting {
		t.Run("accepts "+kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newEntityFakeOntology()
			if err := ontology.RegisterEntity(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			m := make(map[string]any, len(base)+1)
			for k, v := range base {
				m[k] = v
			}
			m["kind"] = kind
			payload, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindEntity, payload); err != nil {
				t.Errorf("expected acceptance for kind=%q, got: %v", kind, err)
			}
		})
	}

	for _, kind := range rejecting {
		t.Run("rejects "+kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newEntityFakeOntology()
			if err := ontology.RegisterEntity(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			m := make(map[string]any, len(base)+1)
			for k, v := range base {
				m[k] = v
			}
			m["kind"] = kind
			payload, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindEntity, payload); err == nil {
				t.Errorf("expected validation error for kind=%q, got nil", kind)
			}
		})
	}
}

// TestEntity_Validate_ResolutionStatusRejectsInvalidEnum asserts that
// resolutionStatus (a closed enum) still rejects values outside the
// three valid constants, even after the EntityKind open-enum change.
func TestEntity_Validate_ResolutionStatusRejectsInvalidEnum(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	base := map[string]any{
		"name":        "entities/" + uid,
		"uid":         uid,
		"kind":        string(ontology.EntityKindPerson),
		"displayName": "Frodo Baggins",
		"confidence":  0.95,
		"createTime":  "2026-05-24T12:00:00Z",
	}
	t.Run("invalid resolutionStatus", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		fake := newEntityFakeOntology()
		if err := ontology.RegisterEntity(ctx, fake); err != nil {
			t.Fatalf("register: %v", err)
		}
		bad := make(map[string]any, len(base)+1)
		for k, v := range base {
			bad[k] = v
		}
		bad["resolutionStatus"] = "maybe"
		payload, err := json.Marshal(bad)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := fake.Validate(ctx, ontology.KindEntity, payload); err == nil {
			t.Error("expected validation error for resolutionStatus=maybe, got nil")
		}
	})
}

// TestEntity_Validate_ConfidenceOutOfRange asserts the schema enforces
// the 0..1 closed interval on confidence.
func TestEntity_Validate_ConfidenceOutOfRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	bad := map[string]any{
		"name":             "entities/" + uid,
		"uid":              uid,
		"kind":             string(ontology.EntityKindPerson),
		"displayName":      "Frodo Baggins",
		"confidence":       1.5,
		"resolutionStatus": string(ontology.ResolutionConfirmed),
		"createTime":       "2026-05-24T12:00:00Z",
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal bad payload: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err == nil {
		t.Fatal("expected validation error for confidence=1.5, got nil")
	}
}

// TestEntitySchema_Cached verifies that EntitySchema returns the same
// cached document across calls (sync.OnceValue contract).
func TestEntitySchema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.EntitySchema()
	second := ontology.EntitySchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("EntitySchema returned different Definitions across calls")
	}
}

// TestEntitySchema_PatternsConsistent decodes the reflected schema and
// asserts that the identifier-bearing fields share the canonical ULID
// and resource-name regexes. Guards against future typos in the
// struct-tag literals (Go struct tags cannot reference constants, so
// the patterns are copy-pasted across fields).
func TestEntitySchema_PatternsConsistent(t *testing.T) {
	t.Parallel()
	const (
		wantULID         = "^[0-9A-HJKMNP-TV-Z]{26}$"
		wantResourceName = "^entities/[0-9A-HJKMNP-TV-Z]{26}$"
	)
	var doc struct {
		Properties map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ontology.EntitySchema().Definition, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	cases := []struct {
		field string
		want  string
	}{
		{"name", wantResourceName},
		{"uid", wantULID},
	}
	for _, c := range cases {
		got, ok := doc.Properties[c.field]
		if !ok {
			t.Errorf("schema is missing property %q", c.field)
			continue
		}
		if got.Pattern != c.want {
			t.Errorf("%s pattern = %q, want %q", c.field, got.Pattern, c.want)
		}
	}
}

// TestEntity_v020_RoundTrip builds an Entity with all v0.2.0 optional
// fields populated (summary, summaryUpdateTime, coordinates,
// coordinatesProvenance) and asserts byte-stable round-trip plus schema
// validity.
func TestEntity_v020_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	summaryTime := time.Date(2026, 5, 25, 8, 0, 0, 0, time.UTC)
	original := ontology.Entity{
		Name:                  "entities/" + uid,
		UID:                   uid,
		Kind:                  ontology.EntityKindPlace,
		DisplayName:           "Berlin",
		Confidence:            1.0,
		ResolutionStatus:      ontology.ResolutionConfirmed,
		CreateTime:            time.Date(2026, 5, 25, 7, 0, 0, 0, time.UTC),
		Summary:               "Berlin is the capital of Germany.",
		SummaryUpdateTime:     &summaryTime,
		Coordinates:           &ontology.Coordinates{Latitude: 52.52, Longitude: 13.40},
		CoordinatesProvenance: "user_provided",
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.Entity
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
	if rt.Summary != original.Summary {
		t.Errorf("summary drift: %q vs %q", rt.Summary, original.Summary)
	}
	if rt.SummaryUpdateTime == nil || !original.SummaryUpdateTime.Equal(*rt.SummaryUpdateTime) {
		t.Errorf("summaryUpdateTime drift: %v vs %v", original.SummaryUpdateTime, rt.SummaryUpdateTime)
	}
	if rt.Coordinates == nil || rt.Coordinates.Latitude != original.Coordinates.Latitude || rt.Coordinates.Longitude != original.Coordinates.Longitude {
		t.Errorf("coordinates drift: %+v vs %+v", original.Coordinates, rt.Coordinates)
	}
	if rt.CoordinatesProvenance != original.CoordinatesProvenance {
		t.Errorf("coordinatesProvenance drift: %q vs %q", rt.CoordinatesProvenance, original.CoordinatesProvenance)
	}
}

// TestEntity_v020_OptionalFieldsOmitted asserts that an Entity with no
// optional v0.2.0 fields set still validates and that the marshaled JSON
// omits those fields entirely.
func TestEntity_v020_OptionalFieldsOmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	e := ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo Baggins",
		Confidence:       0.9,
		ResolutionStatus: ontology.ResolutionTentative,
		CreateTime:       time.Date(2026, 5, 25, 7, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"summary", "summaryUpdateTime", "coordinates", "coordinatesProvenance"} {
		if bytes.Contains(payload, []byte(`"`+field+`"`)) {
			t.Errorf("expected omitempty to drop %q, got: %s", field, payload)
		}
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err != nil {
		t.Errorf("entity without optional v0.2.0 fields must validate, got: %v", err)
	}
}

// TestEntity_Coordinates_LatitudeBounds asserts the schema enforces the
// [-90, 90] closed interval on Coordinates.Latitude.
func TestEntity_Coordinates_LatitudeBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		lat     float64
		wantErr bool
	}{
		{"lower boundary -90", -90.0, false},
		{"upper boundary +90", 90.0, false},
		{"below range", -90.1, true},
		{"above range", 90.1, true},
		{"mid range", 52.52, false},
	}
	uid := ulid.Make().String()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newEntityFakeOntology()
			if err := ontology.RegisterEntity(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := map[string]any{
				"name":             "entities/" + uid,
				"uid":              uid,
				"kind":             string(ontology.EntityKindPlace),
				"displayName":      "Berlin",
				"confidence":       1.0,
				"resolutionStatus": string(ontology.ResolutionConfirmed),
				"createTime":       "2026-05-25T07:00:00Z",
				"coordinates":      map[string]any{"latitude": c.lat, "longitude": 13.40},
			}
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = fake.Validate(ctx, ontology.KindEntity, payload)
			if c.wantErr && err == nil {
				t.Errorf("expected error for latitude=%v, got nil", c.lat)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected success for latitude=%v, got: %v", c.lat, err)
			}
		})
	}
}

// TestEntity_Coordinates_LongitudeBounds asserts the schema enforces the
// [-180, 180] closed interval on Coordinates.Longitude.
func TestEntity_Coordinates_LongitudeBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		lng     float64
		wantErr bool
	}{
		{"lower boundary -180", -180.0, false},
		{"upper boundary +180", 180.0, false},
		{"below range", -180.1, true},
		{"above range", 180.1, true},
		{"mid range", 13.40, false},
	}
	uid := ulid.Make().String()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newEntityFakeOntology()
			if err := ontology.RegisterEntity(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := map[string]any{
				"name":             "entities/" + uid,
				"uid":              uid,
				"kind":             string(ontology.EntityKindPlace),
				"displayName":      "Berlin",
				"confidence":       1.0,
				"resolutionStatus": string(ontology.ResolutionConfirmed),
				"createTime":       "2026-05-25T07:00:00Z",
				"coordinates":      map[string]any{"latitude": 52.52, "longitude": c.lng},
			}
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = fake.Validate(ctx, ontology.KindEntity, payload)
			if c.wantErr && err == nil {
				t.Errorf("expected error for longitude=%v, got nil", c.lng)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected success for longitude=%v, got: %v", c.lng, err)
			}
		})
	}
}

// TestEntity_Coordinates_BothRequiredWhenPresent asserts that a
// Coordinates object with only one of latitude or longitude is rejected.
func TestEntity_Coordinates_BothRequiredWhenPresent(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	cases := []struct {
		name  string
		coord map[string]any
	}{
		{"latitude only", map[string]any{"latitude": 52.52}},
		{"longitude only", map[string]any{"longitude": 13.40}},
		{"empty object", map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newEntityFakeOntology()
			if err := ontology.RegisterEntity(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := map[string]any{
				"name":             "entities/" + uid,
				"uid":              uid,
				"kind":             string(ontology.EntityKindPlace),
				"displayName":      "Berlin",
				"confidence":       1.0,
				"resolutionStatus": string(ontology.ResolutionConfirmed),
				"createTime":       "2026-05-25T07:00:00Z",
				"coordinates":      c.coord,
			}
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindEntity, payload); err == nil {
				t.Errorf("expected error for partial coordinates %+v, got nil", c.coord)
			}
		})
	}
}

// TestEntity_v010_to_v020_UpgradeViaMemoryOntology asserts the semver
// upgrade path: a v0.1.0 entity schema can be replaced by the real
// v0.2.0 schema via MemoryOntology.Register.
func TestEntity_v010_to_v020_UpgradeViaMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()

	v010 := kernel.Schema{
		Kind:       ontology.KindEntity,
		Version:    "v0.1.0",
		Definition: json.RawMessage(`{"type":"object","properties":{"displayName":{"type":"string"}},"required":["displayName"]}`),
		Provenance: "core",
	}
	if err := o.Register(ctx, v010); err != nil {
		t.Fatalf("register v0.1.0: %v", err)
	}
	if err := ontology.RegisterEntity(ctx, o); err != nil {
		t.Fatalf("RegisterEntity v0.3.0 upgrade: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindEntity)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionEntity {
		t.Errorf("schema.Version after upgrade = %q, want %q", got.Version, ontology.VersionEntity)
	}
}

// TestEntity_v040_Properties_RoundTrip builds an Entity with the new
// v0.4.0 optional Properties field set, validates it against the
// schema, and asserts byte-stable round-trip.
func TestEntity_v040_Properties_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	original := ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Merry",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
		Properties:       json.RawMessage(`{"email":"merry@example.com","age":30}`),
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}

	var rt ontology.Entity
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
	if string(rt.Properties) != string(original.Properties) {
		t.Errorf("properties drift: %s vs %s", rt.Properties, original.Properties)
	}
}

// TestEntity_v040_Properties_OmittedWhenAbsent asserts that an Entity without
// Properties serialises without the "properties" key (omitempty).
func TestEntity_v040_Properties_OmittedWhenAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	e := ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Sam",
		Confidence:       0.9,
		ResolutionStatus: ontology.ResolutionTentative,
		CreateTime:       time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(payload, []byte(`"properties"`)) {
		t.Errorf("expected omitempty to drop properties key, got: %s", payload)
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err != nil {
		t.Errorf("entity without properties must validate, got: %v", err)
	}
}

// TestEntity_v040_Properties_MustBeObject asserts that the schema rejects
// a non-object value for the properties field.
func TestEntity_v040_Properties_MustBeObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityFakeOntology()
	if err := ontology.RegisterEntity(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	bad := map[string]any{
		"name":             "entities/" + uid,
		"uid":              uid,
		"kind":             string(ontology.EntityKindPerson),
		"displayName":      "Charlie",
		"confidence":       0.9,
		"resolutionStatus": string(ontology.ResolutionTentative),
		"createTime":       "2026-06-05T00:00:00Z",
		"properties":       "not-an-object",
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntity, payload); err == nil {
		t.Error("expected validation error for properties=string, got nil")
	}
}

// TestEntity_VersionEntity_IsV040 pins the current schema version so a
// future version bump is a deliberate, visible change.
func TestEntity_VersionEntity_IsV040(t *testing.T) {
	t.Parallel()
	if ontology.VersionEntity != "v0.4.0" {
		t.Errorf("VersionEntity = %q, want v0.4.0 (bump intentionally and update this test)", ontology.VersionEntity)
	}
}
