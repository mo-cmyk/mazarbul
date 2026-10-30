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

// fakeOntology is a map-backed test double for kernel.Ontology. It
// compiles schemas via santhosh-tekuri/jsonschema/v6 on first Validate
// and caches the compiled validator for subsequent calls. The real
// in-process registry (issue #37) replaces this fake in production.
type fakeOntology struct {
	schemas  map[string]kernel.Schema
	compiled map[string]*jsonschema.Schema
}

func newFakeOntology() *fakeOntology {
	return &fakeOntology{
		schemas:  make(map[string]kernel.Schema),
		compiled: make(map[string]*jsonschema.Schema),
	}
}

func (f *fakeOntology) Schema(_ context.Context, kind string) (kernel.Schema, error) {
	s, ok := f.schemas[kind]
	if !ok {
		return kernel.Schema{}, kernel.ErrNotFound
	}
	return s, nil
}

// Register implements the strict kernel.Ontology contract: "first
// registration wins". Any second registration for an existing Kind
// without a higher Version returns kernel.ErrSchemaConflict. The fake
// does not implement the version-upgrade path because no test exercises
// it.
func (f *fakeOntology) Register(_ context.Context, s kernel.Schema) error {
	if _, ok := f.schemas[s.Kind]; ok {
		return kernel.ErrSchemaConflict
	}
	f.schemas[s.Kind] = s
	return nil
}

func (f *fakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	out := make([]kernel.Schema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
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

// TestEpisode_Register_SchemaRoundtrips builds an Episode in Go,
// registers the schema with a fake Ontology, marshals to JSON, validates
// against the registered schema, unmarshals back, and asserts that the
// round-trip is byte-stable plus that the timestamp survives unchanged.
// Covers AC4 (schema generated + registered) and AC5 (round-trip test).
func TestEpisode_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeOntology()
	if err := ontology.RegisterEpisode(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := fake.Schema(ctx, ontology.KindEpisode)
	if err != nil {
		t.Fatalf("lookup registered schema: %v", err)
	}
	if got.Kind != ontology.KindEpisode {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindEpisode)
	}
	if got.Version != ontology.VersionEpisode {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionEpisode)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	uid := ulid.Make().String()
	parentUID := ulid.Make().String()
	original := ontology.Episode{
		Name:             "episodes/" + uid,
		UID:              uid,
		CreateTime:       time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Text:             "shipped the episode schema",
		Source:           "cli:remember",
		Confidence:       1.0,
		MentionUIDs:      []ontology.ULID{ontology.ULID(ulid.Make().String())},
		RelationUIDs:     []ontology.ULID{ontology.ULID(ulid.Make().String())},
		Tags:             []string{"m1", "ontology"},
		ParentEpisodeUID: &parentUID,
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEpisode, payload); err != nil {
		t.Fatalf("validate original payload: %v", err)
	}

	var roundTripped ontology.Episode
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
	if roundTripped.ParentEpisodeUID == nil || *roundTripped.ParentEpisodeUID != parentUID {
		t.Errorf("parentEpisodeUid drift: original=%q got=%v", parentUID, roundTripped.ParentEpisodeUID)
	}
}

// TestEpisode_Register_AgainstRealMemoryOntology exercises
// RegisterEpisode against the production kernel.MemoryOntology (not the
// fake), which applies semver.IsValid to schema.Version. Catches
// regressions where VersionEpisode stops being a full MAJOR.MINOR.PATCH
// triplet (mirrors the Entity-side regression net added in #82).
func TestEpisode_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(ctx, o); err != nil {
		t.Fatalf("RegisterEpisode against real MemoryOntology: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindEpisode)
	if err != nil {
		t.Fatalf("Schema lookup after register: %v", err)
	}
	if got.Version != ontology.VersionEpisode {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionEpisode)
	}
}

// TestEpisode_Register_RejectsDoubleRegistration verifies that a second
// call to Register without a higher Version surfaces
// kernel.ErrSchemaConflict per the "first registration wins" rule.
func TestEpisode_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeOntology()
	if err := ontology.RegisterEpisode(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := ontology.RegisterEpisode(ctx, fake)
	if !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestEpisode_Register_ConflictBumpsKind asserts that an attempt to
// re-register a different Definition under the same Kind surfaces
// kernel.ErrSchemaConflict. This guards the registration contract more
// than the Episode struct itself.
func TestEpisode_Register_ConflictBumpsKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeOntology()
	if err := ontology.RegisterEpisode(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	conflicting := kernel.Schema{
		Kind:       ontology.KindEpisode,
		Version:    ontology.VersionEpisode,
		Definition: json.RawMessage(`{"type":"object"}`),
		Provenance: "core",
	}
	if err := fake.Register(ctx, conflicting); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("conflicting Register err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestEpisode_Validate_MissingRequiredField table-drives the negative
// case across every required field: drop the field, assert validation
// fails with a typed *jsonschema.ValidationError, and assert the error
// mentions the dropped field name. Covers AC6 (negative test) plus a
// regression net against invopop dropping `required` tags silently.
//
// Note: name, uid, and createTime are output-only fields (writer-owned)
// and intentionally NOT required on the writer-facing schema. A realistic
// caller cannot supply them before Append assigns the UID. This loop
// only covers the truly-required substantive fields. See
// TestEpisode_Validate_AcceptsPayloadWithoutOutputOnlyFields below.
func TestEpisode_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	uid := ulid.Make().String()
	complete := map[string]any{
		"name":       "episodes/" + uid,
		"uid":        uid,
		"createTime": "2026-05-24T12:00:00Z",
		"text":       "hello",
		"source":     "cli:remember",
		"confidence": 1.0,
	}

	for _, field := range []string{"text", "source", "confidence"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			// Each subtest owns its fake so the lazy-compile cache is
			// not shared across parallel goroutines.
			ctx := context.Background()
			fake := newFakeOntology()
			if err := ontology.RegisterEpisode(ctx, fake); err != nil {
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
			err = fake.Validate(ctx, ontology.KindEpisode, payload)
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

// TestEpisode_Validate_AcceptsPayloadWithoutOutputOnlyFields documents
// the writer-facing contract: name/uid/createTime are output-only, so
// the schema must accept payloads that omit them. This is the realistic
// caller shape (a remember MCP tool that does not know the UID until
// Append assigns one). Without this property, every legitimate Episode
// append would be rejected by the schema.
//
// Covers Greptile #69 iter-3 finding: "Validate a payload shape that
// excludes writer-owned fields".
func TestEpisode_Validate_AcceptsPayloadWithoutOutputOnlyFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeOntology()
	if err := ontology.RegisterEpisode(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Substantive fields only. No name, no uid, no createTime.
	payload := []byte(`{"text":"hello","source":"cli:remember","confidence":1.0}`)
	if err := fake.Validate(ctx, ontology.KindEpisode, payload); err != nil {
		t.Errorf("payload without output-only fields must validate, got: %v", err)
	}
}

// TestEpisode_Validate_RejectsBogusULIDArrayItem asserts that the
// schema enforces the ULID pattern on each element of mentionUids and
// relationUids, not just the array type. Regression coverage for
// Greptile PR #70 P1 (slice items previously typed as plain strings).
func TestEpisode_Validate_RejectsBogusULIDArrayItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	uid := ulid.Make().String()
	base := map[string]any{
		"name":       "episodes/" + uid,
		"uid":        uid,
		"createTime": "2026-05-24T12:00:00Z",
		"text":       "hello",
		"source":     "cli:remember",
		"confidence": 1.0,
	}
	cases := []struct {
		field string
	}{
		{"mentionUids"},
		{"relationUids"},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			t.Parallel()
			fake := newFakeOntology()
			if err := ontology.RegisterEpisode(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			bad := make(map[string]any, len(base)+1)
			for k, v := range base {
				bad[k] = v
			}
			bad[c.field] = []string{"not-a-ulid"}
			payload, err := json.Marshal(bad)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindEpisode, payload); err == nil {
				t.Errorf("expected validation error for bogus %s entry, got nil", c.field)
			}
		})
	}
}

// TestEpisode_Validate_ConfidenceOutOfRange asserts the schema enforces
// the 0..1 closed interval on confidence. Belt-and-suspenders coverage
// for the jsonschema minimum/maximum tag.
func TestEpisode_Validate_ConfidenceOutOfRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeOntology()
	if err := ontology.RegisterEpisode(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	uid := ulid.Make().String()
	bad := map[string]any{
		"name":       "episodes/" + uid,
		"uid":        uid,
		"createTime": "2026-05-24T12:00:00Z",
		"text":       "hello",
		"source":     "cli:remember",
		"confidence": 1.5,
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal bad payload: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEpisode, payload); err == nil {
		t.Fatal("expected validation error for confidence=1.5, got nil")
	}
}

// TestEpisodeSchema_Cached verifies that EpisodeSchema returns the same
// cached document across calls (sync.OnceValue contract).
func TestEpisodeSchema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.EpisodeSchema()
	second := ontology.EpisodeSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("EpisodeSchema returned different Definitions across calls")
	}
}

// TestEpisodeSchema_PatternsConsistent decodes the reflected schema and
// asserts that the three identifier-bearing fields share the canonical
// ULID and resource-name regexes. Guards against future typos in the
// struct-tag literals (Go struct tags cannot reference constants, so
// the patterns are copy-pasted across three fields).
func TestEpisodeSchema_PatternsConsistent(t *testing.T) {
	t.Parallel()
	const (
		wantULID         = "^[0-9A-HJKMNP-TV-Z]{26}$"
		wantResourceName = "^episodes/[0-9A-HJKMNP-TV-Z]{26}$"
	)
	var doc struct {
		Properties map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ontology.EpisodeSchema().Definition, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	cases := []struct {
		field string
		want  string
	}{
		{"name", wantResourceName},
		{"uid", wantULID},
		{"parentEpisodeUid", wantULID},
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
