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

// relationFakeOntology is a map-backed test double for kernel.Ontology,
// duplicating the shape used in episode_test.go and entity_test.go so
// the kinds remain independently testable. Lifting this helper into a
// shared package is the right follow-up once the third copy proves the
// abstraction, but until then keeping it local avoids cross-test
// coupling.
type relationFakeOntology struct {
	schemas  map[string]kernel.Schema
	compiled map[string]*jsonschema.Schema
}

func newRelationFakeOntology() *relationFakeOntology {
	return &relationFakeOntology{
		schemas:  make(map[string]kernel.Schema),
		compiled: make(map[string]*jsonschema.Schema),
	}
}

func (f *relationFakeOntology) Schema(_ context.Context, kind string) (kernel.Schema, error) {
	s, ok := f.schemas[kind]
	if !ok {
		return kernel.Schema{}, kernel.ErrNotFound
	}
	return s, nil
}

func (f *relationFakeOntology) Register(_ context.Context, s kernel.Schema) error {
	if _, ok := f.schemas[s.Kind]; ok {
		return kernel.ErrSchemaConflict
	}
	f.schemas[s.Kind] = s
	return nil
}

func (f *relationFakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	out := make([]kernel.Schema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, s)
	}
	return out, nil
}

func (f *relationFakeOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
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

// completeRelationPayload builds a fully-populated map representation of
// a Relation suitable as the base for negative tests. Required fields
// only; optional fields are added by individual tests as needed.
func completeRelationPayload(t *testing.T) map[string]any {
	t.Helper()
	uid := ulid.Make().String()
	return map[string]any{
		"name":           "relations/" + uid,
		"uid":            uid,
		"subjectUid":     ulid.Make().String(),
		"predicate":      "works_at",
		"objectUid":      ulid.Make().String(),
		"confidence":     0.85,
		"sourceEventUid": ulid.Make().String(),
		"createTime":     "2026-05-24T12:00:00Z",
	}
}

// TestRelation_Register_SchemaRoundtrips builds a Relation with every
// field populated (including both optional timestamps), registers the
// schema with a fake Ontology, marshals to JSON, validates against the
// registered schema, unmarshals back, and asserts the round-trip is
// byte-stable plus that the timestamps survive unchanged. Covers AC1
// (required + optional fields) and AC4 (round-trip test).
func TestRelation_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := fake.Schema(ctx, ontology.KindRelation)
	if err != nil {
		t.Fatalf("lookup registered schema: %v", err)
	}
	if got.Kind != ontology.KindRelation {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindRelation)
	}
	if got.Version != ontology.VersionRelation {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionRelation)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	uid := ulid.Make().String()
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	validUntil := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	original := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     ulid.Make().String(),
		Predicate:      "works_at",
		ObjectUID:      ulid.Make().String(),
		Confidence:     0.85,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Provenance:     "inferred from co-mention in 3 emails",
		ValidFrom:      &validFrom,
		ValidUntil:     &validUntil,
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
		t.Fatalf("validate original payload: %v", err)
	}

	var roundTripped ontology.Relation
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
	if roundTripped.ValidFrom == nil || !original.ValidFrom.Equal(*roundTripped.ValidFrom) {
		t.Errorf("validFrom drift: original=%v got=%v", original.ValidFrom, roundTripped.ValidFrom)
	}
	if roundTripped.ValidUntil == nil || !original.ValidUntil.Equal(*roundTripped.ValidUntil) {
		t.Errorf("validUntil drift: original=%v got=%v", original.ValidUntil, roundTripped.ValidUntil)
	}
	if roundTripped.Predicate != original.Predicate {
		t.Errorf("predicate drift: original=%q got=%q", original.Predicate, roundTripped.Predicate)
	}
}

// TestRelation_Register_AgainstRealMemoryOntology exercises
// RegisterRelation against the production kernel.MemoryOntology (not
// the fake), which applies semver.IsValid to schema.Version. Catches
// regressions where VersionRelation stops being a full
// MAJOR.MINOR.PATCH triplet (Greptile PR #82 P1 found that "v0.1" is
// rejected; "v0.1.0" is required).
func TestRelation_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelation(ctx, o); err != nil {
		t.Fatalf("RegisterRelation against real MemoryOntology: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindRelation)
	if err != nil {
		t.Fatalf("Schema lookup after register: %v", err)
	}
	if got.Version != ontology.VersionRelation {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionRelation)
	}
}

// TestRelation_Register_RejectsDoubleRegistration verifies that a
// second call to Register without a higher Version surfaces
// kernel.ErrSchemaConflict per the "first registration wins" rule.
func TestRelation_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := ontology.RegisterRelation(ctx, fake)
	if !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestRelation_Register_ConflictBumpsKind asserts that an attempt to
// re-register a different Definition under the same Kind surfaces
// kernel.ErrSchemaConflict.
func TestRelation_Register_ConflictBumpsKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	conflicting := kernel.Schema{
		Kind:       ontology.KindRelation,
		Version:    ontology.VersionRelation,
		Definition: json.RawMessage(`{"type":"object"}`),
		Provenance: "core",
	}
	if err := fake.Register(ctx, conflicting); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("conflicting Register err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestRelation_Validate_MissingRequiredField table-drives the negative
// case across every required field: drop the field, assert validation
// fails with a typed *jsonschema.ValidationError, and assert the error
// mentions the dropped field name. Covers AC1 (required fields) and
// AC4 (negative test).
func TestRelation_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	required := []string{
		"name",
		"uid",
		"subjectUid",
		"predicate",
		"objectUid",
		"confidence",
		"sourceEventUid",
		"createTime",
	}
	for _, field := range required {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationFakeOntology()
			if err := ontology.RegisterRelation(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			complete := completeRelationPayload(t)
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
			err = fake.Validate(ctx, ontology.KindRelation, payload)
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

// TestRelation_Validate_RejectsBogusULID asserts the schema enforces
// the ULID pattern on every required ULID-bearing scalar field.
func TestRelation_Validate_RejectsBogusULID(t *testing.T) {
	t.Parallel()
	const bogus = "not-a-ulid"
	for _, field := range []string{"uid", "subjectUid", "objectUid", "sourceEventUid"} {
		t.Run("bogus "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationFakeOntology()
			if err := ontology.RegisterRelation(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			bad := completeRelationPayload(t)
			bad[field] = bogus
			payload, err := json.Marshal(bad)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindRelation, payload); err == nil {
				t.Errorf("expected validation error for %s=%q, got nil", field, bogus)
			}
		})
	}
}

// TestRelation_Validate_RejectsBogusResourceName asserts the schema
// enforces the relations/{ulid} pattern on the AIP resource name field.
func TestRelation_Validate_RejectsBogusResourceName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	bad := completeRelationPayload(t)
	bad["name"] = "entities/01ARZ3NDEKTSV4RRFFQ69G5FAV"
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err == nil {
		t.Error("expected validation error for non-relations/ name, got nil")
	}
}

// TestRelation_Validate_RejectsEmptyPredicate asserts the schema
// enforces minLength=1 on predicate, the M1 floor for the "open enum"
// contract (AC3: open enum, no closed list, but still non-empty).
func TestRelation_Validate_RejectsEmptyPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	bad := completeRelationPayload(t)
	bad["predicate"] = ""
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err == nil {
		t.Error("expected validation error for empty predicate, got nil")
	}
}

// TestRelation_Validate_AcceptsArbitraryPredicate asserts the schema
// does not constrain the predicate to a closed enum at M1. Any
// non-empty string is accepted; closed-list enforcement is M3+ via
// plugin extensions.
func TestRelation_Validate_AcceptsArbitraryPredicate(t *testing.T) {
	t.Parallel()
	for _, predicate := range []string{"works_at", "member_of", "mentions", "custom_xyz_42"} {
		t.Run(predicate, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationFakeOntology()
			if err := ontology.RegisterRelation(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := completeRelationPayload(t)
			payloadMap["predicate"] = predicate
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
				t.Errorf("expected validation success for predicate=%q, got: %v", predicate, err)
			}
		})
	}
}

// TestRelation_Validate_ConfidenceOutOfRange asserts the schema
// enforces the 0..1 closed interval on confidence, accepting the
// boundary values 0.0 and 1.0 and rejecting anything outside.
func TestRelation_Validate_ConfidenceOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		confidence float64
		wantErr    bool
	}{
		{"lower boundary 0.0", 0.0, false},
		{"upper boundary 1.0", 1.0, false},
		{"below range", -0.1, true},
		{"above range", 1.1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationFakeOntology()
			if err := ontology.RegisterRelation(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := completeRelationPayload(t)
			payloadMap["confidence"] = c.confidence
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = fake.Validate(ctx, ontology.KindRelation, payload)
			if c.wantErr && err == nil {
				t.Errorf("expected validation error for confidence=%v, got nil", c.confidence)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected validation success for confidence=%v, got: %v", c.confidence, err)
			}
		})
	}
}

// TestRelation_Validate_AcceptsOptionalFieldsAbsent asserts that a
// payload built from a struct with all optional fields unset (no
// provenance, nil validFrom, nil validUntil) validates and that the
// emitted JSON omits those fields entirely. Regression guard for the
// *time.Time omitempty pattern: a bare time.Time would emit
// "0001-01-01T00:00:00Z" instead of omitting.
func TestRelation_Validate_AcceptsOptionalFieldsAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	uid := ulid.Make().String()
	r := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     ulid.Make().String(),
		Predicate:      "works_at",
		ObjectUID:      ulid.Make().String(),
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"provenance", "validFrom", "validUntil"} {
		if bytes.Contains(payload, []byte(`"`+field+`"`)) {
			t.Errorf("expected omitempty to drop %q, got payload: %s", field, payload)
		}
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
		t.Errorf("expected validation success with optional fields absent, got: %v", err)
	}
}

// TestRelation_Validate_AcceptsValidityWindow asserts that valid
// RFC 3339 UTC timestamps in validFrom and validUntil pass validation,
// including the equality case where validFrom == validUntil
// (instantaneous supersession per architecture spec section 06).
func TestRelation_Validate_AcceptsValidityWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		validFrom  string
		validUntil string
	}{
		{"strict interval", "2026-01-01T00:00:00Z", "2026-12-31T23:59:59Z"},
		{"equal bounds", "2026-05-24T12:00:00Z", "2026-05-24T12:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationFakeOntology()
			if err := ontology.RegisterRelation(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := completeRelationPayload(t)
			payloadMap["validFrom"] = c.validFrom
			payloadMap["validUntil"] = c.validUntil
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
				t.Errorf("expected validation success, got: %v", err)
			}
		})
	}
}

// TestRelation_Validate_RejectsBogusTimestamp asserts that the
// ontology validator itself rejects malformed timestamps in every
// format=date-time field. This is the wire-layer contract: downstream
// consumers (event-log readers, graph writer) validate raw payload
// JSON through Ontology.Validate without unmarshalling into Relation,
// so the schema is the only line of defence.
//
// JSON Schema Draft 2020-12 treats `format` as annotation-only by
// default. kernel.MemoryOntology.Validate calls Compiler.AssertFormat
// to upgrade it to an assertion; this test pins that behavior so a
// regression in the compiler config would surface immediately.
func TestRelation_Validate_RejectsBogusTimestamp(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"createTime", "validFrom", "validUntil"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationFakeOntology()
			if err := ontology.RegisterRelation(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := completeRelationPayload(t)
			payloadMap[field] = "not-a-timestamp"
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindRelation, payload); err == nil {
				t.Errorf("expected validation error for malformed %s, got nil", field)
			}
		})
	}
}

// TestRelation_Validate_RejectsBogusTimestamp_RealMemoryOntology
// covers the same contract against the production
// kernel.MemoryOntology (not the fake), to prove the assertion is
// enabled in real wiring, not just test-side configuration.
func TestRelation_Validate_RejectsBogusTimestamp_RealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelation(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, field := range []string{"createTime", "validFrom", "validUntil"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			payloadMap := completeRelationPayload(t)
			payloadMap[field] = "not-a-timestamp"
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := o.Validate(ctx, ontology.KindRelation, payload); err == nil {
				t.Errorf("expected validation error for malformed %s, got nil", field)
			}
		})
	}
}

// TestRelation_GoDecoder_RejectsBogusValidityTimestamp asserts that
// malformed timestamps in the optional validity-window fields fail to
// decode into the Go Relation struct.
//
// The schema also rejects these via Compiler.AssertFormat (see
// TestRelation_Validate_RejectsBogusTimestamp), but this test pins the
// second line of defence: Go's time.Time decoder rejects non-RFC-3339
// input on unmarshal. Two independent guards on the same contract.
func TestRelation_GoDecoder_RejectsBogusValidityTimestamp(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"createTime", "validFrom", "validUntil"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			payloadMap := completeRelationPayload(t)
			payloadMap[field] = "not-a-timestamp"
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var r ontology.Relation
			if err := json.Unmarshal(payload, &r); err == nil {
				t.Errorf("expected Go decoder to reject malformed %s, got nil", field)
			}
		})
	}
}

// TestRelationSchema_Cached verifies that RelationSchema returns the
// same cached document across calls (sync.OnceValue contract).
func TestRelationSchema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.RelationSchema()
	second := ontology.RelationSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("RelationSchema returned different Definitions across calls")
	}
}

// TestRelationSchema_PatternsConsistent decodes the reflected schema
// and asserts that every identifier-bearing field shares the canonical
// ULID and resource-name regexes. Guards against future typos in the
// struct-tag literals (Go struct tags cannot reference constants, so
// the patterns are copy-pasted across fields).
func TestRelationSchema_PatternsConsistent(t *testing.T) {
	t.Parallel()
	const (
		wantULID         = "^[0-9A-HJKMNP-TV-Z]{26}$"
		wantULIDOrSelf   = "^([0-9A-HJKMNP-TV-Z]{26}|self)$"
		wantResourceName = "^relations/[0-9A-HJKMNP-TV-Z]{26}$"
	)
	var doc struct {
		Properties map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ontology.RelationSchema().Definition, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	cases := []struct {
		field string
		want  string
	}{
		{"name", wantResourceName},
		{"uid", wantULID},
		// subjectUid accepts the "self" sentinel for first-person
		// self-statements emitted by the lexical detector.
		{"subjectUid", wantULIDOrSelf},
		// objectUid also accepts the "self" sentinel: a note with
		// `entity: self` frontmatter emits a doc --mentions--> self edge
		// (the user is a legitimate edge target).
		{"objectUid", wantULIDOrSelf},
		{"sourceEventUid", wantULID},
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

// TestRelation_v020_RoundTrip builds a Relation with all v0.2.0 optional
// fields (validityScore, supersededByUid) and asserts byte-stable
// round-trip and schema validity.
func TestRelation_v020_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	supersededByUID := ontology.ULID(ulid.Make().String())
	original := ontology.Relation{
		Name:            "relations/" + uid,
		UID:             uid,
		SubjectUID:      ulid.Make().String(),
		Predicate:       "works_at",
		ObjectUID:       ulid.Make().String(),
		Confidence:      0.9,
		SourceEventUID:  ulid.Make().String(),
		CreateTime:      time.Date(2026, 5, 25, 7, 0, 0, 0, time.UTC),
		ValidityScore:   0.1,
		SupersededByUID: &supersededByUID,
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.Relation
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
	if rt.ValidityScore != original.ValidityScore {
		t.Errorf("validityScore drift: %v vs %v", rt.ValidityScore, original.ValidityScore)
	}
	if rt.SupersededByUID == nil || *rt.SupersededByUID != supersededByUID {
		t.Errorf("supersededByUid drift: %v vs %v", rt.SupersededByUID, supersededByUID)
	}
}

// TestRelation_v020_OptionalFieldsOmitted asserts that a Relation with
// no optional v0.2.0 fields set still validates and that marshaled JSON
// omits those fields entirely.
func TestRelation_v020_OptionalFieldsOmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	uid := ulid.Make().String()
	r := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     ulid.Make().String(),
		Predicate:      "works_at",
		ObjectUID:      ulid.Make().String(),
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     time.Date(2026, 5, 25, 7, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"validityScore", "supersededByUid"} {
		if bytes.Contains(payload, []byte(`"`+field+`"`)) {
			t.Errorf("expected omitempty to drop %q, got: %s", field, payload)
		}
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
		t.Errorf("relation without optional v0.2.0 fields must validate, got: %v", err)
	}
}

// TestRelation_ValidityScore_Bounds asserts the schema enforces the 0..1
// closed interval on validityScore.
func TestRelation_ValidityScore_Bounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		score   float64
		wantErr bool
	}{
		{"lower boundary 0.0", 0.0, false},
		{"upper boundary 1.0", 1.0, false},
		{"below range", -0.1, true},
		{"above range", 1.1, true},
		{"mid range", 0.5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationFakeOntology()
			if err := ontology.RegisterRelation(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := completeRelationPayload(t)
			payloadMap["validityScore"] = c.score
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = fake.Validate(ctx, ontology.KindRelation, payload)
			if c.wantErr && err == nil {
				t.Errorf("expected error for validityScore=%v, got nil", c.score)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected success for validityScore=%v, got: %v", c.score, err)
			}
		})
	}
}

// TestRelation_SupersededByUID_RejectsBogusULID asserts the schema
// enforces the ULID pattern on supersededByUid when present.
func TestRelation_SupersededByUID_RejectsBogusULID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	payloadMap := completeRelationPayload(t)
	payloadMap["supersededByUid"] = "not-a-ulid"
	payload, err := json.Marshal(payloadMap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err == nil {
		t.Error("expected validation error for bogus supersededByUid, got nil")
	}
}

// TestRelation_v010_to_v020_UpgradeViaMemoryOntology asserts the semver
// upgrade path for Relation: v0.1.0 replaced by v0.2.0 via
// MemoryOntology.Register.
// TestRelation_v030_AttributesRoundTrip pins the v0.3.0 schema
// addition: Relation.Attributes carries qualifying key/value pairs
// (role, title, employment_type) extracted from the source text. The
// JSON marshaller must preserve every key/value and the schema must
// accept the resulting payload.
func TestRelation_v030_AttributesRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	uid := ulid.Make().String()
	original := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     ulid.Make().String(),
		Predicate:      "works_at",
		ObjectUID:      ulid.Make().String(),
		Confidence:     0.95,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     time.Date(2026, 5, 27, 7, 0, 0, 0, time.UTC),
		Attributes: map[string]string{
			"role":            "CEO",
			"employment_type": "full-time",
		},
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.Relation
	if err := json.Unmarshal(payload, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rt.Attributes["role"] != "CEO" {
		t.Errorf("attributes[role] drift: got %q, want %q", rt.Attributes["role"], "CEO")
	}
	if rt.Attributes["employment_type"] != "full-time" {
		t.Errorf("attributes[employment_type] drift: got %q, want %q", rt.Attributes["employment_type"], "full-time")
	}
}

// TestRelation_v030_AttributesOmittedWhenNil pins the additive-schema
// contract: a Relation with nil Attributes must still validate, and
// the marshaller must drop the field from the output via omitempty
// (so v0.2.0 readers continue to parse v0.3.0 payloads without churn).
func TestRelation_v030_AttributesOmittedWhenNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationFakeOntology()
	if err := ontology.RegisterRelation(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	uid := ulid.Make().String()
	r := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     ulid.Make().String(),
		Predicate:      "works_at",
		ObjectUID:      ulid.Make().String(),
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     time.Date(2026, 5, 27, 7, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(payload, []byte(`"attributes"`)) {
		t.Errorf("attributes must be omitted when nil, got: %s", payload)
	}
	if err := fake.Validate(ctx, ontology.KindRelation, payload); err != nil {
		t.Errorf("relation without attributes must validate, got: %v", err)
	}
}

func TestRelation_v010_to_v020_UpgradeViaMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()

	v010 := kernel.Schema{
		Kind:       ontology.KindRelation,
		Version:    "v0.1.0",
		Definition: json.RawMessage(`{"type":"object","properties":{"predicate":{"type":"string"}},"required":["predicate"]}`),
		Provenance: "core",
	}
	if err := o.Register(ctx, v010); err != nil {
		t.Fatalf("register v0.1.0: %v", err)
	}
	if err := ontology.RegisterRelation(ctx, o); err != nil {
		t.Fatalf("RegisterRelation v0.2.0 upgrade: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindRelation)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionRelation {
		t.Errorf("schema.Version after upgrade = %q, want %q", got.Version, ontology.VersionRelation)
	}
}
