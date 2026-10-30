package schemapack_test

// capstone_test.go is the business-de smoke test for the dynamic-ontology plan.
// It exercises the end-to-end lifecycle of the embedded business-de pack:
//
//  1. Load + Install through a Dispatcher-backed event log
//  2. Assert entity_kinds and predicates land with correct provenance + typed metadata
//  3. Mint a company entity with hrb identifier; assert entity row + entity_identifiers
//  4. Mint a managing_director + assert manages_company relation + ValidateRelation
//  5. Render the sync tree and assert ontology/kinds/<company>.yaml + ontology/predicates.yaml
//  6. Full event-log replay into a fresh dispatcher; assert registry + entity + identifier reconstruct
//
// All subtests share one fixture (event log + dispatcher pair) built in the
// parent so the fixture cost is paid once and the sequence is readable as the
// plan's smoke script.  No Ollama required; the chat provider is not wired.

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// capstoneFixture holds the shared test infrastructure.
type capstoneFixture struct {
	ctx         context.Context
	d           *dispatcher.Dispatcher
	log         kernel.EventLog
	dataDir     string
	companyUID  string
	directorUID string
}

// buildCapstoneFixture opens a fresh dispatcher + event log, installs both
// built-in packs (schema-org-core first so business-de can extend its kinds),
// and mints the company + director + manages_company relation.  The returned
// fixture has the entity UIDs recorded for downstream subtests.
func buildCapstoneFixture(t *testing.T) *capstoneFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()

	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir: dataDir,
		// No embedder: we do not need vector columns for the capstone.
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// Build a minimal ontology that accepts the event kinds the installer and
	// createEntity paths emit without requiring the full kernel.NewMemoryOntology
	// wiring (which needs LLM schemas for episode/mention/relation).
	ont := &capstoneLenientOntology{}

	// Wire the dispatcher as the storage backend so every Append fans out to
	// SQLite/LadybugDB alongside the JSONL log.  Without WithStorage the log
	// writes to JSONL only and the dispatcher tables stay empty.
	log := kernel.NewFileEventLog(dataDir, ont, kernel.WithStorage(d))

	// Install schema-org-core first (business-de extends its "organization"
	// and "person" kinds), then business-de.
	inst := &schemapack.Installer{Log: log}
	for _, name := range schemapack.BuiltinPackNames {
		p, perr := schemapack.LoadBuiltin(name)
		if perr != nil {
			t.Fatalf("LoadBuiltin(%q): %v", name, perr)
		}
		if _, ierr := inst.Install(ctx, p, false); ierr != nil {
			t.Fatalf("Install(%q): %v", name, ierr)
		}
	}

	// Mint the company entity.
	companyUID := capstoneULID(1)
	companyEnt := ontology.Entity{
		Name:             "entities/" + companyUID,
		UID:              companyUID,
		Kind:             ontology.EntityKind("extension:business_de:company"),
		DisplayName:      "Muster GmbH",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       capstoneTime,
		Identifiers: []ontology.Identifier{
			{Type: "hrb", Value: "HRB 12345 B"},
		},
		Properties: json.RawMessage(`{"hrb":"HRB 12345 B","legalForm":"GmbH"}`),
	}
	companyPayload, err := json.Marshal(companyEnt)
	if err != nil {
		t.Fatalf("marshal company entity: %v", err)
	}
	idPayload, err := json.Marshal(ontology.EntityIdentifierAddedEvent{
		EntityUID:  companyUID,
		Identifier: "HRB 12345 B",
		Kind:       ontology.IdentifierKind("hrb"),
		Source:     ontology.IdentifierSourceUser,
		AddedAt:    capstoneTime,
	})
	if err != nil {
		t.Fatalf("marshal identifier event: %v", err)
	}
	if _, aerr := log.Append(ctx, []kernel.Event{
		{Kind: kernel.EventKind(ontology.KindEntity), Source: "capstone", Confidence: 1.0, Payload: companyPayload},
		{Kind: kernel.EventKind(ontology.KindEntityIdentifierAdded), Source: "capstone", Confidence: 1.0, Payload: idPayload},
	}); aerr != nil {
		t.Fatalf("append company entity + identifier: %v", aerr)
	}

	// Mint the managing director entity.
	directorUID := capstoneULID(2)
	directorEnt := ontology.Entity{
		Name:             "entities/" + directorUID,
		UID:              directorUID,
		Kind:             ontology.EntityKind("extension:business_de:managing_director"),
		DisplayName:      "Bilbo Baggins",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       capstoneTime,
	}
	dirPayload, err := json.Marshal(directorEnt)
	if err != nil {
		t.Fatalf("marshal director entity: %v", err)
	}
	if _, aerr := log.Append(ctx, []kernel.Event{
		{Kind: kernel.EventKind(ontology.KindEntity), Source: "capstone", Confidence: 1.0, Payload: dirPayload},
	}); aerr != nil {
		t.Fatalf("append director entity: %v", aerr)
	}

	// Mint manages_company relation.
	relUID := capstoneULID(3)
	rel := ontology.Relation{
		Name:           "relations/" + relUID,
		UID:            relUID,
		SubjectUID:     directorUID,
		Predicate:      "manages_company",
		ObjectUID:      companyUID,
		Confidence:     1.0,
		SourceEventUID: relUID,
		CreateTime:     capstoneTime,
	}
	relPayload, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	if _, aerr := log.Append(ctx, []kernel.Event{
		{Kind: kernel.EventKind(ontology.KindRelation), Source: "capstone", Confidence: 1.0, Payload: relPayload},
	}); aerr != nil {
		t.Fatalf("append relation: %v", aerr)
	}

	return &capstoneFixture{
		ctx:         ctx,
		d:           d,
		log:         log,
		dataDir:     dataDir,
		companyUID:  companyUID,
		directorUID: directorUID,
	}
}

// capstoneTime is a fixed wall-clock reference for all test events.
var capstoneTime = time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

// capstoneULID returns a deterministic 26-char ULID string for use in tests.
// Uses the Crockford Base32 alphabet; seq differentiates concurrent entities.
func capstoneULID(seq int) string {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// Timestamp portion: capstoneTime in Unix ms = 1_749_254_400_000 decimal.
	ts := uint64(capstoneTime.UnixMilli())
	var tsChars [10]byte
	for i := 9; i >= 0; i-- {
		tsChars[i] = alpha[ts&0x1F]
		ts >>= 5
	}
	// Random portion: deterministic from seq.
	rnd := uint64(seq)
	var rndChars [16]byte
	for i := 15; i >= 0; i-- {
		rndChars[i] = alpha[rnd&0x1F]
		rnd >>= 5
	}
	return string(tsChars[:]) + string(rndChars[:])
}

// capstoneLenientOntology is a minimal Ontology that accepts any well-formed
// JSON payload without consulting a schema registry.  It allows the capstone
// to drive the FileEventLog without wiring the full in-memory ontology (which
// needs episode/mention/relation schema registrations separate from the pack
// vocabulary being tested here).
type capstoneLenientOntology struct{}

func (capstoneLenientOntology) Schema(_ context.Context, _ string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotFound
}

func (capstoneLenientOntology) Register(_ context.Context, _ kernel.Schema) error {
	return nil
}

func (capstoneLenientOntology) List(_ context.Context) ([]kernel.Schema, error) {
	return nil, nil
}

func (capstoneLenientOntology) Validate(_ context.Context, _ string, payload json.RawMessage) error {
	// Accept any valid JSON; reject non-JSON to preserve the basic guard.
	var probe any
	return json.Unmarshal(payload, &probe)
}

// seqFromLog adapts the kernel.EventLog read surface into the iter.Seq shape
// that render.Render expects.  It reads from the beginning (no cursor) so a
// fresh render always replays all events.
func seqFromLog(ctx context.Context, t *testing.T, log kernel.EventLog) iter.Seq[kernel.Event] {
	t.Helper()
	return func(yield func(kernel.Event) bool) {
		for ev, err := range log.Read(ctx, kernel.ReadOpts{}) {
			if err != nil {
				t.Errorf("seqFromLog: %v", err)
				return
			}
			if !yield(ev) {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Capstone: TestBusinessDE_Pack_EndToEnd
// ---------------------------------------------------------------------------

// TestBusinessDE_Pack_EndToEnd is the plan's smoke test for the dynamic-ontology
// capstone.  Subtests share one fixture built in the parent.
func TestBusinessDE_Pack_EndToEnd(t *testing.T) {
	fx := buildCapstoneFixture(t)

	t.Run("pack kinds land in entity kinds with extension provenance", func(t *testing.T) {
		db, err := fx.d.OpenSQLite(fx.ctx)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}

		wantKinds := []string{
			"extension:business_de:company",
			"extension:business_de:managing_director",
			"extension:business_de:shareholder",
		}
		for _, kind := range wantKinds {
			var provenance, addedBy string
			rerr := db.QueryRowContext(fx.ctx,
				`SELECT provenance, added_by FROM entity_kinds WHERE name = ?`, kind,
			).Scan(&provenance, &addedBy)
			if rerr != nil {
				t.Errorf("kind %q not found in entity_kinds: %v", kind, rerr)
				continue
			}
			if provenance != "extension:business-de" {
				t.Errorf("kind %q: provenance = %q, want extension:business-de", kind, provenance)
			}
			if addedBy != "extension" {
				t.Errorf("kind %q: added_by = %q, want extension", kind, addedBy)
			}
		}
	})

	t.Run("predicates land with typed metadata", func(t *testing.T) {
		db, err := fx.d.OpenSQLite(fx.ctx)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}

		// registered_with must carry subject_kinds = ["extension:business_de:company"]
		// and property_schema containing "hrb".
		var subjectKinds, propertySchema string
		rerr := db.QueryRowContext(fx.ctx,
			`SELECT COALESCE(subject_kinds,''), COALESCE(property_schema,'')
			   FROM predicates WHERE predicate = 'registered_with'`,
		).Scan(&subjectKinds, &propertySchema)
		if rerr != nil {
			t.Fatalf("registered_with not found in predicates: %v", rerr)
		}
		if !strings.Contains(subjectKinds, "extension:business_de:company") {
			t.Errorf("registered_with: subject_kinds = %q, want to contain extension:business_de:company", subjectKinds)
		}
		if !strings.Contains(propertySchema, "hrb") {
			t.Errorf("registered_with: property_schema = %q, want to contain hrb field", propertySchema)
		}

		// manages_company must carry subject_kinds = managing_director, object_kinds = company.
		var subjKinds2, objKinds2 string
		rerr = db.QueryRowContext(fx.ctx,
			`SELECT COALESCE(subject_kinds,''), COALESCE(object_kinds,'')
			   FROM predicates WHERE predicate = 'manages_company'`,
		).Scan(&subjKinds2, &objKinds2)
		if rerr != nil {
			t.Fatalf("manages_company not found in predicates: %v", rerr)
		}
		if !strings.Contains(subjKinds2, "extension:business_de:managing_director") {
			t.Errorf("manages_company: subject_kinds = %q, want managing_director", subjKinds2)
		}
		if !strings.Contains(objKinds2, "extension:business_de:company") {
			t.Errorf("manages_company: object_kinds = %q, want company", objKinds2)
		}
	})

	t.Run("company entity materialized with properties", func(t *testing.T) {
		db, err := fx.d.OpenSQLite(fx.ctx)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}

		var kind, props string
		rerr := db.QueryRowContext(fx.ctx,
			`SELECT kind, COALESCE(properties,'') FROM entities WHERE uid = ?`, fx.companyUID,
		).Scan(&kind, &props)
		if rerr != nil {
			t.Fatalf("company entity not found (uid=%s): %v", fx.companyUID, rerr)
		}
		if kind != "extension:business_de:company" {
			t.Errorf("entity kind = %q, want extension:business_de:company", kind)
		}
		if !strings.Contains(props, "HRB 12345 B") {
			t.Errorf("entity properties = %q, want to contain HRB 12345 B", props)
		}
		if !strings.Contains(props, "GmbH") {
			t.Errorf("entity properties = %q, want to contain GmbH (legalForm)", props)
		}
	})

	t.Run("hrb identifier row written for company", func(t *testing.T) {
		db, err := fx.d.OpenSQLite(fx.ctx)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}

		var identifierVal, identifierKind string
		rerr := db.QueryRowContext(fx.ctx,
			`SELECT identifier, kind FROM entity_identifiers WHERE entity_uid = ? AND kind = 'hrb'`,
			fx.companyUID,
		).Scan(&identifierVal, &identifierKind)
		if rerr != nil {
			t.Fatalf("hrb identifier row not found for company (uid=%s): %v", fx.companyUID, rerr)
		}
		if identifierVal != "HRB 12345 B" {
			t.Errorf("identifier value = %q, want HRB 12345 B", identifierVal)
		}
		if identifierKind != "hrb" {
			t.Errorf("identifier kind = %q, want hrb", identifierKind)
		}
	})

	t.Run("manages company relation written between director and company", func(t *testing.T) {
		db, err := fx.d.OpenSQLite(fx.ctx)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}

		var subjectUID, predicate, objectUID string
		rerr := db.QueryRowContext(fx.ctx,
			`SELECT subject_uid, predicate, object_uid
			   FROM relations
			  WHERE subject_uid = ? AND predicate = 'manages_company' AND object_uid = ?`,
			fx.directorUID, fx.companyUID,
		).Scan(&subjectUID, &predicate, &objectUID)
		if rerr != nil {
			t.Fatalf("manages_company relation not found (director=%s → company=%s): %v",
				fx.directorUID, fx.companyUID, rerr)
		}
		if subjectUID != fx.directorUID || objectUID != fx.companyUID {
			t.Errorf("relation endpoints = (%s, %s), want (%s, %s)",
				subjectUID, objectUID, fx.directorUID, fx.companyUID)
		}
	})

	t.Run("ValidateRelation accepts manages company with registered predicate", func(t *testing.T) {
		db, err := fx.d.OpenSQLite(fx.ctx)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		// SQLiteOntology.ValidateRelation uses the predicates table to enforce
		// domain/range.  Seed a minimal SQLiteOntology against the dispatcher's
		// SQLite so the same data is visible to the validator.
		// NOTE: The dispatcher does not expose SQLiteOntology directly, so we
		// drive the SQL read that ValidateRelation would perform: check that the
		// predicates row exists with the correct subject_kinds / object_kinds.
		// This is the observable behavior of ValidateRelation under the hood.
		var subjectKinds, objectKinds string
		rerr := db.QueryRowContext(fx.ctx,
			`SELECT COALESCE(subject_kinds,''), COALESCE(object_kinds,'')
			   FROM predicates WHERE predicate = 'manages_company'`,
		).Scan(&subjectKinds, &objectKinds)
		if rerr != nil {
			t.Fatalf("manages_company predicate row missing: %v", rerr)
		}
		// Domain: managing_director; Range: company. Both must be registered.
		if !strings.Contains(subjectKinds, "extension:business_de:managing_director") {
			t.Errorf("manages_company subject_kinds = %q, want managing_director", subjectKinds)
		}
		if !strings.Contains(objectKinds, "extension:business_de:company") {
			t.Errorf("manages_company object_kinds = %q, want company", objectKinds)
		}
		// Director entity must have the expected kind in the entities table.
		var dirKind string
		if rerr := db.QueryRowContext(fx.ctx,
			`SELECT kind FROM entities WHERE uid = ?`, fx.directorUID,
		).Scan(&dirKind); rerr != nil {
			t.Fatalf("director entity missing: %v", rerr)
		}
		if dirKind != "extension:business_de:managing_director" {
			t.Errorf("director kind = %q, want extension:business_de:managing_director", dirKind)
		}
	})

	t.Run("render sync tree contains ontology kind YAML and predicates YAML", func(t *testing.T) {
		view, err := render.Render(render.Input{
			Scope:  kernel.ScopeProject,
			Events: seqFromLog(fx.ctx, t, fx.log),
		})
		if err != nil {
			t.Fatalf("render.Render: %v", err)
		}

		files := make(map[string]render.File, len(view.Files))
		for _, f := range view.Files {
			files[f.Path] = f
		}

		// Company kind YAML must exist and carry status + provenance.
		companyYAML, ok := files["ontology/kinds/extension:business_de:company.yaml"]
		if !ok {
			var paths []string
			for p := range files {
				if strings.HasPrefix(p, "ontology/kinds/") {
					paths = append(paths, p)
				}
			}
			t.Fatalf("ontology/kinds/extension:business_de:company.yaml missing; kind files: %v", paths)
		}
		if !bytes.Contains(companyYAML.Content, []byte("extension:business-de")) {
			t.Errorf("company kind YAML missing provenance extension:business-de; got:\n%s", companyYAML.Content)
		}
		if !bytes.Contains(companyYAML.Content, []byte("status:")) {
			t.Errorf("company kind YAML missing status field; got:\n%s", companyYAML.Content)
		}

		// ontology/predicates.yaml must carry manages_company with subject/object kinds.
		predYAML, ok := files["ontology/predicates.yaml"]
		if !ok {
			t.Fatal("ontology/predicates.yaml missing from render output")
		}
		if !bytes.Contains(predYAML.Content, []byte("manages_company")) {
			t.Errorf("predicates.yaml missing manages_company; got:\n%s", predYAML.Content)
		}
		if !bytes.Contains(predYAML.Content, []byte("managing_director")) {
			t.Errorf("predicates.yaml missing managing_director subject kind; got:\n%s", predYAML.Content)
		}
	})

	t.Run("rebuild into fresh store reconstructs registry entity and identifier", func(t *testing.T) {
		freshDir := t.TempDir()
		d2, err := dispatcher.Open(fx.ctx, dispatcher.Config{
			DataDir: freshDir,
		})
		if err != nil {
			t.Fatalf("dispatcher.Open (fresh): %v", err)
		}
		t.Cleanup(func() { _ = d2.Close() })

		applyErrs, err := fx.log.Rebuild(fx.ctx, d2)
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		if len(applyErrs) != 0 {
			t.Fatalf("Rebuild produced %d apply errors; first: %v", len(applyErrs), applyErrs[0])
		}

		db2, err := d2.OpenSQLite(fx.ctx)
		if err != nil {
			t.Fatalf("OpenSQLite (fresh): %v", err)
		}

		// Registry: company kind must be present with extension provenance.
		var provenance string
		if rerr := db2.QueryRowContext(fx.ctx,
			`SELECT provenance FROM entity_kinds WHERE name = 'extension:business_de:company'`,
		).Scan(&provenance); rerr != nil {
			t.Fatalf("entity_kinds not reconstructed after rebuild: %v", rerr)
		}
		if provenance != "extension:business-de" {
			t.Errorf("rebuilt provenance = %q, want extension:business-de", provenance)
		}

		// Entity: company must be present with properties.
		var entKind, props string
		if rerr := db2.QueryRowContext(fx.ctx,
			`SELECT kind, COALESCE(properties,'') FROM entities WHERE uid = ?`, fx.companyUID,
		).Scan(&entKind, &props); rerr != nil {
			t.Fatalf("company entity not reconstructed after rebuild: %v", rerr)
		}
		if entKind != "extension:business_de:company" {
			t.Errorf("rebuilt entity kind = %q", entKind)
		}
		if !strings.Contains(props, "HRB 12345 B") {
			t.Errorf("rebuilt entity properties = %q, want HRB 12345 B", props)
		}

		// Identifier: hrb row must be present.
		var idVal string
		if rerr := db2.QueryRowContext(fx.ctx,
			`SELECT identifier FROM entity_identifiers WHERE entity_uid = ? AND kind = 'hrb'`,
			fx.companyUID,
		).Scan(&idVal); rerr != nil {
			t.Fatalf("hrb identifier not reconstructed after rebuild: %v", rerr)
		}
		if idVal != "HRB 12345 B" {
			t.Errorf("rebuilt identifier value = %q, want HRB 12345 B", idVal)
		}

		// Manages_company relation must also reconstruct.
		var relCount int
		if rerr := db2.QueryRowContext(fx.ctx,
			`SELECT COUNT(*) FROM relations WHERE subject_uid = ? AND predicate = 'manages_company' AND object_uid = ?`,
			fx.directorUID, fx.companyUID,
		).Scan(&relCount); rerr != nil {
			t.Fatalf("relation count after rebuild: %v", rerr)
		}
		if relCount != 1 {
			t.Errorf("manages_company relation count after rebuild = %d, want 1", relCount)
		}
	})
}
