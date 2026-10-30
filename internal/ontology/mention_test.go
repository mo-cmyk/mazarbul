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

// mentionFakeOntology is a map-backed test double for kernel.Ontology,
// duplicating the shape used in episode_test.go and entity_test.go so
// each kind stays independently testable. Promoting the helper to a
// shared package is a follow-up cleanup once a fourth kind copy proves
// the abstraction is worth lifting.
type mentionFakeOntology struct {
	schemas  map[string]kernel.Schema
	compiled map[string]*jsonschema.Schema
}

func newMentionFakeOntology() *mentionFakeOntology {
	return &mentionFakeOntology{
		schemas:  make(map[string]kernel.Schema),
		compiled: make(map[string]*jsonschema.Schema),
	}
}

func (f *mentionFakeOntology) Schema(_ context.Context, kind string) (kernel.Schema, error) {
	s, ok := f.schemas[kind]
	if !ok {
		return kernel.Schema{}, kernel.ErrNotFound
	}
	return s, nil
}

func (f *mentionFakeOntology) Register(_ context.Context, s kernel.Schema) error {
	if _, ok := f.schemas[s.Kind]; ok {
		return kernel.ErrSchemaConflict
	}
	f.schemas[s.Kind] = s
	return nil
}

func (f *mentionFakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	out := make([]kernel.Schema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, s)
	}
	return out, nil
}

func (f *mentionFakeOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
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

// TestMention_Register_SchemaRoundtrips builds a Mention with every
// field populated, registers the schema with a fake Ontology, marshals
// to JSON, validates against the registered schema, unmarshals back,
// and asserts the round-trip is byte-stable plus that the timestamp
// survives unchanged. Covers AC1 (required fields), AC2 (optional fields),
// and the round-trip leg of AC4.
func TestMention_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := fake.Schema(ctx, ontology.KindMention)
	if err != nil {
		t.Fatalf("lookup registered schema: %v", err)
	}
	if got.Kind != ontology.KindMention {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindMention)
	}
	if got.Version != ontology.VersionMention {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionMention)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	uid := ulid.Make().String()
	sourceUID := ulid.Make().String()
	createTime := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	original := ontology.Mention{
		Name:           "mentions/" + uid,
		UID:            uid,
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Frodo",
		SourceEventUID: ontology.ULID(sourceUID),
		CreateTime:     &createTime,
		Identifiers: []ontology.Identifier{
			{Type: "email", Value: "frodo@erebor.example"},
			{Type: "github_username", Value: "FrodoB"},
		},
		DisambiguationCtx: "fundraising chat, Erebor engineer",
		ExtractionText:    "Frodo",
		SourceSpan:        ontology.SourceSpan{StartChar: 0, EndChar: 7},
		GroundingScore:    1.0,
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindMention, payload); err != nil {
		t.Fatalf("validate original payload: %v", err)
	}

	var roundTripped ontology.Mention
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
	if roundTripped.CreateTime == nil || !original.CreateTime.Equal(*roundTripped.CreateTime) {
		t.Errorf("createTime drift: original=%v got=%v", original.CreateTime, roundTripped.CreateTime)
	}
	if roundTripped.Kind != original.Kind {
		t.Errorf("kind drift: original=%q got=%q", original.Kind, roundTripped.Kind)
	}
	if roundTripped.SourceEventUID != original.SourceEventUID {
		t.Errorf("sourceEventUid drift: original=%q got=%q", original.SourceEventUID, roundTripped.SourceEventUID)
	}
	if roundTripped.DisambiguationCtx != original.DisambiguationCtx {
		t.Errorf("disambiguationCtx drift: original=%q got=%q", original.DisambiguationCtx, roundTripped.DisambiguationCtx)
	}
}

// TestMention_Register_AgainstRealMemoryOntology exercises RegisterMention
// against the production kernel.MemoryOntology (not the fake), which
// applies semver.IsValid to schema.Version. Catches regressions where
// VersionMention stops being a full MAJOR.MINOR.PATCH triplet (Greptile
// PR #82 P1 baseline: "v0.1" is rejected; "v0.1.0" is required).
func TestMention_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterMention(ctx, o); err != nil {
		t.Fatalf("RegisterMention against real MemoryOntology: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindMention)
	if err != nil {
		t.Fatalf("Schema lookup after register: %v", err)
	}
	if got.Version != ontology.VersionMention {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionMention)
	}
}

// TestMention_Register_RejectsDoubleRegistration verifies that a second
// call to Register without a higher Version surfaces
// kernel.ErrSchemaConflict per the "first registration wins" rule.
func TestMention_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := ontology.RegisterMention(ctx, fake)
	if !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestMention_Register_ConflictBumpsKind asserts that an attempt to
// re-register a different Definition under the same Kind surfaces
// kernel.ErrSchemaConflict.
func TestMention_Register_ConflictBumpsKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	conflicting := kernel.Schema{
		Kind:       ontology.KindMention,
		Version:    ontology.VersionMention,
		Definition: json.RawMessage(`{"type":"object"}`),
		Provenance: "core",
	}
	if err := fake.Register(ctx, conflicting); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("conflicting Register err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestMention_Validate_MissingRequiredField table-drives the negative
// case across every required field: drop the field, assert validation
// fails with a typed *jsonschema.ValidationError, and assert the error
// mentions the dropped field name. Covers the substantive-required leg
// of the issue AC.
//
// Note: name, uid, and createTime are output-only fields (writer-owned)
// and intentionally NOT required on the writer-facing schema. A realistic
// caller cannot supply them before Append assigns the UID. This loop
// only covers the truly-required substantive fields. See
// TestMention_Validate_AcceptsPayloadWithoutOutputOnlyFields below.
func TestMention_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	sourceUID := ulid.Make().String()
	complete := map[string]any{
		"name":           "mentions/" + uid,
		"uid":            uid,
		"kind":           string(ontology.EntityKindPerson),
		"displayName":    "Frodo",
		"sourceEventUid": sourceUID,
		"createTime":     "2026-05-24T12:00:00Z",
		"extractionText": "Frodo",
		"sourceSpan":     map[string]any{"startChar": 0, "endChar": 7},
		"groundingScore": 1.0,
	}

	for _, field := range []string{"kind", "displayName", "sourceEventUid", "extractionText", "sourceSpan", "groundingScore"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newMentionFakeOntology()
			if err := ontology.RegisterMention(ctx, fake); err != nil {
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
			err = fake.Validate(ctx, ontology.KindMention, payload)
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

// TestMention_Validate_AcceptsPayloadWithoutOptionalFields documents
// that identifiers and disambiguationCtx are optional: a minimal-shape
// payload that omits them must validate. The substantive-required-only
// shape mirrors what a realistic extractor emits (writer-owned
// name/uid/createTime are filled by the event-log writer, not the
// caller).
func TestMention_Validate_AcceptsPayloadWithoutOptionalFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	sourceUID := ulid.Make().String()
	minimal := map[string]any{
		"kind":           string(ontology.EntityKindPerson),
		"displayName":    "Frodo",
		"sourceEventUid": sourceUID,
		"extractionText": "Frodo",
		"sourceSpan":     map[string]any{"startChar": 0, "endChar": 7},
		"groundingScore": 1.0,
	}
	payload, err := json.Marshal(minimal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindMention, payload); err != nil {
		t.Errorf("minimal payload must validate, got: %v", err)
	}
}

// TestMention_Validate_AcceptsPayloadWithoutOutputOnlyFields documents
// the writer-facing contract: name/uid/createTime are output-only, so
// the schema must accept payloads that omit them. This is the realistic
// caller shape (an extractor emitting a Mention event before the writer
// assigns an outer Event.UID). Without this property, every legitimate
// Mention append would be rejected by the schema.
//
// Covers Greptile #93 P1 finding: "Writer-owned fields reject appends".
// Mirrors TestEpisode_Validate_AcceptsPayloadWithoutOutputOnlyFields.
func TestMention_Validate_AcceptsPayloadWithoutOutputOnlyFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	sourceUID := ulid.Make().String()
	payload := []byte(`{"kind":"person","displayName":"Frodo","sourceEventUid":"` + sourceUID + `","extractionText":"Frodo","sourceSpan":{"startChar":0,"endChar":7},"groundingScore":1.0}`)
	if err := fake.Validate(ctx, ontology.KindMention, payload); err != nil {
		t.Errorf("payload without output-only fields must validate, got: %v", err)
	}
}

// TestMention_Validate_RejectsBogusSourceEventUID covers AC3: the
// sourceEventUid pattern must reject non-ULID strings.
func TestMention_Validate_RejectsBogusSourceEventUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	bad := map[string]any{
		"name":           "mentions/" + uid,
		"uid":            uid,
		"kind":           string(ontology.EntityKindPerson),
		"displayName":    "Frodo",
		"sourceEventUid": "not-a-ulid",
		"createTime":     "2026-05-24T12:00:00Z",
		"extractionText": "Frodo",
		"sourceSpan":     map[string]any{"startChar": 0, "endChar": 7},
		"groundingScore": 1.0,
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindMention, payload); err == nil {
		t.Error("expected validation error for bogus sourceEventUid, got nil")
	}
}

// TestMention_Validate_KindPattern asserts the kind field (shared with
// Entity) uses the open-enum pattern introduced in P2: valid bare
// snake_case kinds and namespaced extension kinds are accepted; strings
// that violate the pattern are rejected. "penguin" is now valid (matches
// bare snake_case); truly invalid examples use uppercase, digits at
// start, empty string, or trailing colons.
func TestMention_Validate_KindPattern(t *testing.T) {
	t.Parallel()
	sourceUID := ulid.Make().String()
	base := map[string]any{
		"displayName":    "Frodo",
		"sourceEventUid": sourceUID,
		"extractionText": "Frodo",
		"sourceSpan":     map[string]any{"startChar": 0, "endChar": 7},
		"groundingScore": 1.0,
	}

	accepting := []string{
		string(ontology.EntityKindPerson),
		string(ontology.EntityKindDocument),
		"penguin", // bare snake_case — valid pattern, unregistered kind (membership check is warn-accept)
		"user:deal",
		"extension:schema_org:Person",
	}
	rejecting := []string{
		"",
		"Fish",
		"123abc",
		"user:",
		":deal",
		"has space",
	}

	for _, kind := range accepting {
		t.Run("accepts "+kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newMentionFakeOntology()
			if err := ontology.RegisterMention(ctx, fake); err != nil {
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
			if err := fake.Validate(ctx, ontology.KindMention, payload); err != nil {
				t.Errorf("expected acceptance for kind=%q, got: %v", kind, err)
			}
		})
	}

	for _, kind := range rejecting {
		t.Run("rejects "+kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newMentionFakeOntology()
			if err := ontology.RegisterMention(ctx, fake); err != nil {
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
			if err := fake.Validate(ctx, ontology.KindMention, payload); err == nil {
				t.Errorf("expected validation error for kind=%q, got nil", kind)
			}
		})
	}
}

// TestMention_Validate_RejectsBogusNameOrUID asserts that the name and
// uid patterns are enforced as well (defense in depth against typos in
// the tag literal; the consistency test below verifies the literal
// itself).
func TestMention_Validate_RejectsBogusNameOrUID(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	sourceUID := ulid.Make().String()
	base := map[string]any{
		"name":           "mentions/" + uid,
		"uid":            uid,
		"kind":           string(ontology.EntityKindPerson),
		"displayName":    "Frodo",
		"sourceEventUid": sourceUID,
		"createTime":     "2026-05-24T12:00:00Z",
		"extractionText": "Frodo",
		"sourceSpan":     map[string]any{"startChar": 0, "endChar": 7},
		"groundingScore": 1.0,
	}
	cases := []struct {
		field string
		value string
	}{
		{"name", "episodes/" + uid},
		{"uid", "not-a-ulid"},
	}
	for _, c := range cases {
		t.Run("bogus "+c.field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newMentionFakeOntology()
			if err := ontology.RegisterMention(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			bad := make(map[string]any, len(base))
			for k, v := range base {
				bad[k] = v
			}
			bad[c.field] = c.value
			payload, err := json.Marshal(bad)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindMention, payload); err == nil {
				t.Errorf("expected validation error for %s=%q, got nil", c.field, c.value)
			}
		})
	}
}

// TestMention_Marshal_OmitsZeroCreateTime documents the writer-owned
// timestamp invariant: a caller that constructs an ontology.Mention via
// Go struct literal and json.Marshals it without setting CreateTime
// must produce JSON that omits the createTime field entirely. The
// alternative (time.Time as a non-pointer with omitempty) silently
// emits "0001-01-01T00:00:00Z" because omitempty cannot detect a zero
// struct, which the schema accepts as a valid RFC 3339 string and
// FileEventLog then persists, surfacing year 0001 to downstream
// resolver code instead of the writer-assigned observation time.
//
// Covers Greptile #93 P1 "Zero time still persists".
func TestMention_Marshal_OmitsZeroCreateTime(t *testing.T) {
	t.Parallel()
	m := ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Frodo",
		SourceEventUID: ontology.ULID(ulid.Make().String()),
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte(`"createTime"`)) {
		t.Errorf("marshal of zero-CreateTime Mention emitted createTime: %s", raw)
	}
}

// TestMentionSchema_Cached verifies that MentionSchema returns the same
// cached document across calls (sync.OnceValue contract).
func TestMentionSchema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.MentionSchema()
	second := ontology.MentionSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("MentionSchema returned different Definitions across calls")
	}
}

// TestMentionSchema_PatternsConsistent decodes the reflected schema and
// asserts that the identifier-bearing fields share the canonical ULID
// and resource-name regexes. Guards against future typos in the
// struct-tag literals (Go struct tags cannot reference constants, so
// the patterns are copy-pasted across fields).
func TestMentionSchema_PatternsConsistent(t *testing.T) {
	t.Parallel()
	const (
		wantULID         = "^[0-9A-HJKMNP-TV-Z]{26}$"
		wantResourceName = "^mentions/[0-9A-HJKMNP-TV-Z]{26}$"
	)
	var doc struct {
		Properties map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ontology.MentionSchema().Definition, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	cases := []struct {
		field string
		want  string
	}{
		{"name", wantResourceName},
		{"uid", wantULID},
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

// TestMention_v020_RoundTrip builds a Mention with all v0.2.0 fields
// populated, marshals it, validates against the registered v0.2.0 schema,
// unmarshals back, and asserts byte-stability. Covers the v0.2.0
// addition of extractionText, sourceSpan, and groundingScore.
func TestMention_v020_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newMentionFakeOntology()
	if err := ontology.RegisterMention(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	sourceUID := ulid.Make().String()
	createTime := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	m := ontology.Mention{
		Name:              "mentions/" + uid,
		UID:               uid,
		Kind:              ontology.EntityKindOrganization,
		DisplayName:       "Erebor Inc.",
		SourceEventUID:    ontology.ULID(sourceUID),
		CreateTime:        &createTime,
		ExtractionText:    "Erebor Inc.",
		SourceSpan:        ontology.SourceSpan{StartChar: 16, EndChar: 28},
		GroundingScore:    0.97,
		DisambiguationCtx: "Berlin fundraising",
	}

	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindMention, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.Mention
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
	if rt.ExtractionText != m.ExtractionText {
		t.Errorf("extractionText drift: %q vs %q", rt.ExtractionText, m.ExtractionText)
	}
	if rt.SourceSpan != m.SourceSpan {
		t.Errorf("sourceSpan drift: %+v vs %+v", rt.SourceSpan, m.SourceSpan)
	}
	if rt.GroundingScore != m.GroundingScore {
		t.Errorf("groundingScore drift: %v vs %v", rt.GroundingScore, m.GroundingScore)
	}
}

// TestMention_GroundingScore_Bounds asserts the schema enforces the 0..1
// closed interval on groundingScore.
func TestMention_GroundingScore_Bounds(t *testing.T) {
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
		{"mid range", 0.75, false},
	}
	uid := ulid.Make().String()
	sourceUID := ulid.Make().String()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newMentionFakeOntology()
			if err := ontology.RegisterMention(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			payloadMap := map[string]any{
				"name":           "mentions/" + uid,
				"uid":            uid,
				"kind":           string(ontology.EntityKindPerson),
				"displayName":    "Frodo",
				"sourceEventUid": sourceUID,
				"extractionText": "Frodo",
				"sourceSpan":     map[string]any{"startChar": 0, "endChar": 7},
				"groundingScore": c.score,
			}
			payload, err := json.Marshal(payloadMap)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = fake.Validate(ctx, ontology.KindMention, payload)
			if c.wantErr && err == nil {
				t.Errorf("expected validation error for groundingScore=%v, got nil", c.score)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected success for groundingScore=%v, got: %v", c.score, err)
			}
		})
	}
}

// TestMention_SourceSpan_RequiredFields asserts that both startChar and
// endChar are required inside sourceSpan.
func TestMention_SourceSpan_RequiredFields(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	sourceUID := ulid.Make().String()
	base := map[string]any{
		"kind":           string(ontology.EntityKindPerson),
		"displayName":    "Frodo",
		"sourceEventUid": sourceUID,
		"extractionText": "Frodo",
		"groundingScore": 1.0,
	}
	cases := []struct {
		name       string
		sourceSpan map[string]any
	}{
		{"missing endChar", map[string]any{"startChar": 0}},
		{"missing startChar", map[string]any{"endChar": 7}},
		{"empty object", map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newMentionFakeOntology()
			if err := ontology.RegisterMention(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			m := make(map[string]any, len(base)+1)
			for k, v := range base {
				m[k] = v
			}
			m["sourceSpan"] = c.sourceSpan
			_ = uid // used only to satisfy lint; UID not required on caller-facing payloads
			payload, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindMention, payload); err == nil {
				t.Errorf("expected validation error for incomplete sourceSpan %+v, got nil", c.sourceSpan)
			}
		})
	}
}

// TestMention_v010_to_v020_UpgradeViaMemoryOntology asserts that the
// MemoryOntology.Register semver-upgrade path accepts a v0.2.0 schema
// after a v0.1.0 schema was already registered (the production upgrade
// path for a running daemon that loaded v0.1.0 at boot and then hot-
// reloads v0.2.0). Covers the kernel.MemoryOntology "higher version
// replaces" rule.
func TestMention_v010_to_v020_UpgradeViaMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()

	// Simulate a previous v0.1.0 registration by seeding a minimal schema
	// document at the old version. The real v0.1.0 definition is no longer
	// in the codebase; this synthetic stand-in is sufficient to prove the
	// upgrade path.
	v010 := kernel.Schema{
		Kind:       ontology.KindMention,
		Version:    "v0.1.0",
		Definition: json.RawMessage(`{"type":"object","properties":{"displayName":{"type":"string"}},"required":["displayName"]}`),
		Provenance: "core",
	}
	if err := o.Register(ctx, v010); err != nil {
		t.Fatalf("register v0.1.0: %v", err)
	}

	// Now register the real v0.3.0 schema; the higher version must be
	// accepted and must replace the v0.1.0 entry.
	if err := ontology.RegisterMention(ctx, o); err != nil {
		t.Fatalf("RegisterMention v0.3.0 upgrade: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindMention)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionMention {
		t.Errorf("schema.Version after upgrade = %q, want %q", got.Version, ontology.VersionMention)
	}
}
