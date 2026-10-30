package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestDocumentRevision_Register_AgainstRealMemoryOntology exercises
// RegisterDocumentRevision against the production kernel.MemoryOntology, which
// applies semver.IsValid to schema.Version. Catches regressions where
// VersionDocumentRevision stops being a full MAJOR.MINOR.PATCH triplet
// (mirrors the Episode/Entity regression nets).
func TestDocumentRevision_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterDocumentRevision(ctx, o); err != nil {
		t.Fatalf("RegisterDocumentRevision against real MemoryOntology: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindDocumentRevision)
	if err != nil {
		t.Fatalf("Schema lookup after register: %v", err)
	}
	if got.Version != ontology.VersionDocumentRevision {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionDocumentRevision)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}
	if got.Kind != ontology.KindDocumentRevision {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindDocumentRevision)
	}
}

// TestDocumentDeleted_Register_AgainstRealMemoryOntology exercises
// RegisterDocumentDeleted against the production kernel.MemoryOntology.
func TestDocumentDeleted_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterDocumentDeleted(ctx, o); err != nil {
		t.Fatalf("RegisterDocumentDeleted against real MemoryOntology: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindDocumentDeleted)
	if err != nil {
		t.Fatalf("Schema lookup after register: %v", err)
	}
	if got.Version != ontology.VersionDocumentDeleted {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionDocumentDeleted)
	}
	if got.Kind != ontology.KindDocumentDeleted {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindDocumentDeleted)
	}
}

// TestDocumentRevision_SchemaReflects asserts that DocumentRevisionSchema
// returns a non-nil Definition and that the result is consistent across two
// calls (sync.OnceValue contract).
func TestDocumentRevision_SchemaReflects(t *testing.T) {
	t.Parallel()
	first := ontology.DocumentRevisionSchema()
	if len(first.Definition) == 0 {
		t.Fatal("DocumentRevisionSchema returned empty Definition")
	}
	second := ontology.DocumentRevisionSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Error("DocumentRevisionSchema returned different Definitions across calls")
	}
}

// TestDocumentDeleted_SchemaReflects asserts that DocumentDeletedSchema
// returns a non-nil Definition and is stable across calls.
func TestDocumentDeleted_SchemaReflects(t *testing.T) {
	t.Parallel()
	first := ontology.DocumentDeletedSchema()
	if len(first.Definition) == 0 {
		t.Fatal("DocumentDeletedSchema returned empty Definition")
	}
	second := ontology.DocumentDeletedSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Error("DocumentDeletedSchema returned different Definitions across calls")
	}
}

// TestDocumentRevision_RoundTrip builds a DocumentRevision with every field
// populated, marshals it to JSON, unmarshals it back, and asserts byte-stable
// equality. This is the critical verbatim-body invariant: the body field must
// survive the round-trip without normalisation. Also covers Wikilinks and
// Observations slice fields.
func TestDocumentRevision_RoundTrip(t *testing.T) {
	t.Parallel()

	docUID := ulid.Make().String()
	revUID := ulid.Make().String()
	prevUID := ulid.Make().String()

	original := ontology.DocumentRevision{
		Name:           "documentRevisions/" + revUID,
		UID:            revUID,
		CreateTime:     time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
		DocumentUID:    docUID,
		Path:           "notes/project-alpha.md",
		Title:          "Project Alpha",
		Body:           "# Project Alpha\n\nThis is a **verbatim** body.\nLine two.\n",
		Frontmatter:    json.RawMessage(`{"tags":["alpha","project"],"pinned":false}`),
		FrontmatterRaw: "---\ntags:\n  - alpha\n  - project\npinned: false\n---",
		ContentHash:    "a" + repeatChar('b', 63),
		Wikilinks: []ontology.WikiLink{
			{Raw: "[[Rosie Cotton]]", Target: "Rosie Cotton"},
			{Raw: "[[relates_to::Project Beta]]", Target: "Project Beta", RelType: "relates_to", Confidence: 0.9},
		},
		Observations: []ontology.Observation{
			{Category: "note", Text: "Alpha ships in Q3", Tags: []string{"timeline"}},
		},
		PreviousRevisionUID: &prevUID,
		Pinned:              true,
		Tags:                []string{"alpha", "project"},
	}

	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var roundTripped ontology.DocumentRevision
	if err := json.Unmarshal(payload, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rePayload, err := json.Marshal(roundTripped)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip JSON mismatch\n original: %s\n     got: %s", payload, rePayload)
	}

	// Verbatim body must survive byte-for-byte.
	if roundTripped.Body != original.Body {
		t.Errorf("body mismatch\n original: %q\n     got: %q", original.Body, roundTripped.Body)
	}
	if !original.CreateTime.Equal(roundTripped.CreateTime) {
		t.Errorf("createTime drift: original=%v got=%v", original.CreateTime, roundTripped.CreateTime)
	}
	if roundTripped.PreviousRevisionUID == nil || *roundTripped.PreviousRevisionUID != prevUID {
		t.Errorf("previousRevisionUid drift: want %q got %v", prevUID, roundTripped.PreviousRevisionUID)
	}
	if len(roundTripped.Wikilinks) != 2 {
		t.Errorf("wikilinks len = %d, want 2", len(roundTripped.Wikilinks))
	}
	if len(roundTripped.Observations) != 1 {
		t.Errorf("observations len = %d, want 1", len(roundTripped.Observations))
	}
}

// TestDocumentRevision_RoundTrip_MinimalPayload verifies that a payload with
// only the required fields (documentUid, path, body, contentHash) is accepted
// by json.Unmarshal and marshals back without extra fields.
func TestDocumentRevision_RoundTrip_MinimalPayload(t *testing.T) {
	t.Parallel()
	docUID := ulid.Make().String()
	minimal := map[string]any{
		"documentUid": docUID,
		"path":        "notes/simple.md",
		"body":        "Hello world\n",
		"contentHash": "c" + repeatChar('d', 63),
	}
	payload, err := json.Marshal(minimal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc ontology.DocumentRevision
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.DocumentUID != docUID {
		t.Errorf("documentUid = %q, want %q", doc.DocumentUID, docUID)
	}
	if doc.Body != "Hello world\n" {
		t.Errorf("body = %q, want %q", doc.Body, "Hello world\n")
	}
}

// TestDocumentRevision_Register_RejectsDoubleRegistration verifies that a
// second call to RegisterDocumentRevision without a higher Version surfaces
// kernel.ErrSchemaConflict per the "first registration wins" rule.
func TestDocumentRevision_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeOntology()
	if err := ontology.RegisterDocumentRevision(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := ontology.RegisterDocumentRevision(ctx, fake)
	if !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestDocumentDeleted_Register_RejectsDoubleRegistration verifies the same
// "first registration wins" contract for DocumentDeleted.
func TestDocumentDeleted_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newFakeOntology()
	if err := ontology.RegisterDocumentDeleted(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := ontology.RegisterDocumentDeleted(ctx, fake)
	if !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: err = %v, want %v", err, kernel.ErrSchemaConflict)
	}
}

// TestDocumentDeleted_RoundTrip verifies that DocumentDeleted marshals and
// unmarshals with byte-stable fidelity.
func TestDocumentDeleted_RoundTrip(t *testing.T) {
	t.Parallel()
	docUID := ulid.Make().String()
	original := ontology.DocumentDeleted{
		DocumentUID: docUID,
		Path:        "notes/old.md",
		Reason:      "superseded by structured facts",
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTripped ontology.DocumentDeleted
	if err := json.Unmarshal(payload, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rePayload, err := json.Marshal(roundTripped)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip mismatch\n original: %s\n     got: %s", payload, rePayload)
	}
	if roundTripped.DocumentUID != docUID {
		t.Errorf("documentUid = %q, want %q", roundTripped.DocumentUID, docUID)
	}
}

// TestDocumentRevision_Schema_RequiredFields decodes the reflected schema and
// asserts that the fields marked required in the struct tags are present in the
// schema's required array. Guards against silent invopop changes that drop the
// RequiredFromJSONSchemaTags flag.
func TestDocumentRevision_Schema_RequiredFields(t *testing.T) {
	t.Parallel()
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(ontology.DocumentRevisionSchema().Definition, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	required := make(map[string]bool, len(doc.Required))
	for _, f := range doc.Required {
		required[f] = true
	}
	for _, field := range []string{"documentUid", "path", "body", "contentHash"} {
		if !required[field] {
			t.Errorf("schema missing required field %q; required = %v", field, doc.Required)
		}
	}
}

// repeatChar returns a string of n copies of ch. Used to build valid SHA-256
// hex digests (64 chars) in test data without string literals that reviewers
// would have to count.
func repeatChar(ch byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ch
	}
	return string(b)
}
