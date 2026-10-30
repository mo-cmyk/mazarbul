package document_test

// rehome_integration_test.go verifies Part A (new-import subject assignment)
// and Part C (retroactive SweepObservationFacts) from the factRehomed feature.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// ---- Part A: new-import subject assignment ----

// TestObservationFacts_RehomeToSubject verifies that a document with a single
// about: subject produces observation facts homed on the subject, not the doc.
func TestObservationFacts_RehomeToSubject(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: uses full dispatcher stack")
	}
	t.Parallel()
	s, dis := newStackWithDis(t)
	subjectUID := s.seedEntity(t, ontology.EntityKindPerson, "Project Alpha")

	vault := writeVault(t, map[string]string{
		"note.md": "---\ntitle: Alpha Notes\nabout: [[Project Alpha]]\n---\n\n" +
			"- [decision] Ship Friday\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// Fact must be on the about: subject, not the document.
	subjectFacts := s.queryFacts(t, dis, subjectUID)
	if len(subjectFacts) != 1 {
		t.Fatalf("subject facts = %d, want 1; doc facts = %d",
			len(subjectFacts), len(s.queryFacts(t, dis, doc.DocumentUID)))
	}
	if !contains(subjectFacts[0].Value, "Ship Friday") {
		t.Errorf("subject fact value = %q, want to contain 'Ship Friday'", subjectFacts[0].Value)
	}

	// Document must have zero observation facts.
	if got := len(s.queryFacts(t, dis, doc.DocumentUID)); got != 0 {
		t.Errorf("doc facts = %d, want 0 (observation homed on subject)", got)
	}
}

// TestObservationFacts_MultiSubject verifies that a document with two about:
// subjects produces two distinct facts (one per subject) with non-colliding
// content hashes, so neither supersedes the other.
func TestObservationFacts_MultiSubject(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: uses full dispatcher stack")
	}
	t.Parallel()
	s, dis := newStackWithDis(t)
	alphaUID := s.seedEntity(t, ontology.EntityKindPerson, "Project Alpha")
	rosieUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"note.md": "---\ntitle: Multi\nabout: [Project Alpha, Rosie Cotton]\n---\n\n" +
			"- [fact] Key decision\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	alphaFacts := s.queryFacts(t, dis, alphaUID)
	rosieFacts := s.queryFacts(t, dis, rosieUID)
	docFacts := s.queryFacts(t, dis, doc.DocumentUID)

	if len(alphaFacts) != 1 {
		t.Fatalf("alpha facts = %d, want 1", len(alphaFacts))
	}
	if len(rosieFacts) != 1 {
		t.Fatalf("rosie facts = %d, want 1", len(rosieFacts))
	}
	if len(docFacts) != 0 {
		t.Errorf("doc facts = %d, want 0 (all homed on subjects)", len(docFacts))
	}

	// Content hashes must differ: same text on two subjects yields two distinct keys.
	if alphaFacts[0].Hash == rosieFacts[0].Hash {
		t.Errorf("alpha and rosie facts share content_hash %q; multi-subject hashes must differ",
			alphaFacts[0].Hash)
	}
}

// TestObservationFacts_FallbackToDoc verifies that a document with no about:
// frontmatter homes its observation facts on the document itself (original
// behavior preserved as the fallback).
func TestObservationFacts_FallbackToDoc(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: uses full dispatcher stack")
	}
	t.Parallel()
	s, dis := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"note.md": "# Note\n\n- [todo] Write more tests\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	facts := s.queryFacts(t, dis, doc.DocumentUID)
	if len(facts) != 1 {
		t.Fatalf("doc facts = %d, want 1 (fallback to document)", len(facts))
	}
}

// TestObservationFacts_InlineOverride verifies that an inline [[X]] wikilink
// in the observation text overrides the about: frontmatter and homes the fact
// on X rather than the about: subject.
func TestObservationFacts_InlineOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: uses full dispatcher stack")
	}
	t.Parallel()
	s, dis := newStackWithDis(t)
	alphaUID := s.seedEntity(t, ontology.EntityKindPerson, "Project Alpha")
	rosieUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// about: points to Project Alpha, but the observation line mentions [[Rosie Cotton]].
	vault := writeVault(t, map[string]string{
		"note.md": "---\ntitle: Override Test\nabout: [[Project Alpha]]\n---\n\n" +
			"- [note] Check [[Rosie Cotton]] for timeline\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// Fact must land on Rosie Cotton (inline override), not Project Alpha or doc.
	rosieFacts := s.queryFacts(t, dis, rosieUID)
	alphaFacts := s.queryFacts(t, dis, alphaUID)
	docFacts := s.queryFacts(t, dis, doc.DocumentUID)

	if len(rosieFacts) != 1 {
		t.Fatalf("rosie facts = %d, want 1 (inline override)", len(rosieFacts))
	}
	if len(alphaFacts) != 0 {
		t.Errorf("alpha facts = %d, want 0 (inline overrides about:)", len(alphaFacts))
	}
	if len(docFacts) != 0 {
		t.Errorf("doc facts = %d, want 0 (inline overrides fallback)", len(docFacts))
	}
}

// TestObservationFacts_MultiInlineFallsThroughToAbout verifies that when an
// observation line contains MORE THAN ONE resolving [[wikilink]], the inline
// override does NOT fire — it is ambiguous which link is the subject, so the
// fact homes on the about: frontmatter subject instead. Guards the rule-1
// contract ("exactly one target resolves") against silently picking the first
// resolving link.
func TestObservationFacts_MultiInlineFallsThroughToAbout(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: uses full dispatcher stack")
	}
	t.Parallel()
	s, dis := newStackWithDis(t)
	aboutUID := s.seedEntity(t, ontology.EntityKindPerson, "Gandalf")
	alphaUID := s.seedEntity(t, ontology.EntityKindPerson, "Project Alpha")
	rosieUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// about: Gandalf, but the observation line resolves TWO inline wikilinks.
	vault := writeVault(t, map[string]string{
		"note.md": "---\ntitle: Multi Inline\nabout: [[Gandalf]]\n---\n\n" +
			"- [note] [[Project Alpha]] reviewed by [[Rosie Cotton]]\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}

	// Two inline links resolve → ambiguous → fall through to about: (Gandalf),
	// NOT the first resolving link (Project Alpha).
	if got := len(s.queryFacts(t, dis, aboutUID)); got != 1 {
		t.Fatalf("about-subject (Gandalf) facts = %d, want 1 (ambiguous inline must fall through to about:)", got)
	}
	if got := len(s.queryFacts(t, dis, alphaUID)); got != 0 {
		t.Errorf("Project Alpha facts = %d, want 0 (must NOT pick the first of multiple resolving inline links)", got)
	}
	if got := len(s.queryFacts(t, dis, rosieUID)); got != 0 {
		t.Errorf("Rosie Cotton facts = %d, want 0", got)
	}
}

// ---- Part C: retroactive SweepObservationFacts ----

// TestSweepObservationFacts_Convergence seeds a document-homed observation fact
// and a describes relation (simulating a doc imported before Part A was
// introduced), runs SweepObservationFacts, asserts the factRehomed event moves
// the fact to the about: subject, then runs the sweep again and asserts no new
// events are emitted (idempotent convergence).
func TestSweepObservationFacts_Convergence(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: uses full dispatcher stack")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	dis, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	log := kernel.NewFileEventLog(dataDir, noopOntology{}, kernel.WithStorage(dis))
	t.Cleanup(func() { _ = dis.Close() })
	t.Cleanup(func() { _ = log.Close() })

	reader, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	// Seed the about: subject entity.
	subjectUID := ulid.Make().String()
	applyEntity(t, ctx, log, subjectUID, "Project Alpha", ontology.EntityKindPerson)

	// Seed the document entity.
	docUID := ulid.Make().String()
	applyEntity(t, ctx, log, docUID, "Notes", ontology.EntityKindDocument)

	// Seed a describes relation: doc -describes-> subject.
	applyDescribesRelation(t, ctx, log, docUID, subjectUID)

	// Seed a Fact event with subject_uid = docUID and source = "document:import"
	// (simulates a pre-Part-A import that homed the fact on the document).
	factUID := ulid.Make().String()
	applyDocFact(t, ctx, log, factUID, docUID)

	// Verify the fact is currently on the document.
	docFacts, err := nav.FactsForSubject(ctx, docUID, navigate.FactFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("FactsForSubject(doc, before sweep): %v", err)
	}
	if len(docFacts) != 1 {
		t.Fatalf("doc facts before sweep = %d, want 1", len(docFacts))
	}

	// QuerySweepStats backs `maz doc rehome --dry-run`: it must project exactly
	// the pending move (1 fact on 1 document) without mutating anything. This is
	// the read path that the dry-run summary's projected count comes from.
	stats, err := document.QuerySweepStats(ctx, reader)
	if err != nil {
		t.Fatalf("QuerySweepStats: %v", err)
	}
	if stats.FactsToRehome != 1 {
		t.Errorf("QuerySweepStats.FactsToRehome = %d, want 1", stats.FactsToRehome)
	}
	if stats.DocumentsAffected != 1 {
		t.Errorf("QuerySweepStats.DocumentsAffected = %d, want 1", stats.DocumentsAffected)
	}

	// Run the sweep.
	nowFn := func() time.Time { return time.Now().UTC() }
	newUID := func() (string, error) { return ulid.Make().String(), nil }
	if err := document.SweepObservationFacts(ctx, reader, log, nowFn, newUID, zap.NewNop()); err != nil {
		t.Fatalf("SweepObservationFacts (first run): %v", err)
	}

	// Fact must now be on the subject, not the document.
	subjectFacts, err := nav.FactsForSubject(ctx, subjectUID, navigate.FactFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("FactsForSubject(subject, after sweep): %v", err)
	}
	if len(subjectFacts) != 1 {
		t.Fatalf("subject facts after sweep = %d, want 1", len(subjectFacts))
	}
	docFactsAfter, err := nav.FactsForSubject(ctx, docUID, navigate.FactFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("FactsForSubject(doc, after sweep): %v", err)
	}
	if len(docFactsAfter) != 0 {
		t.Errorf("doc facts after sweep = %d, want 0", len(docFactsAfter))
	}

	// Second run: the sweep must be idempotent and emit no new events.
	// Count events before the second run.
	readerDB, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader (second): %v", err)
	}
	var countBefore int
	if err := readerDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM episodes`).Scan(&countBefore); err != nil {
		t.Fatalf("count episodes before second sweep: %v", err)
	}

	if err := document.SweepObservationFacts(ctx, reader, log, nowFn, newUID, zap.NewNop()); err != nil {
		t.Fatalf("SweepObservationFacts (second run): %v", err)
	}

	var countAfter int
	if err := readerDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM episodes`).Scan(&countAfter); err != nil {
		t.Fatalf("count episodes after second sweep: %v", err)
	}
	if countAfter != countBefore {
		t.Errorf("second sweep appended %d new events (want 0); not idempotent",
			countAfter-countBefore)
	}
}

// ---- helpers ----

func applyEntity(t *testing.T, ctx context.Context, log *kernel.FileEventLog, uid, name string, kind ontology.EntityKind) {
	t.Helper()
	payload, err := json.Marshal(ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             kind,
		DisplayName:      name,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal entity %s: %v", name, err)
	}
	if _, err := log.Append(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "test",
		Confidence: 1.0,
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("append entity %s: %v", name, err)
	}
}

func applyDescribesRelation(t *testing.T, ctx context.Context, log *kernel.FileEventLog, docUID, subjectUID string) {
	t.Helper()
	relUID := ulid.Make().String()
	payload, err := json.Marshal(ontology.Relation{
		Name:       "relations/" + relUID,
		UID:        relUID,
		SubjectUID: docUID,
		Predicate:  "describes",
		ObjectUID:  subjectUID,
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal describes relation: %v", err)
	}
	if _, err := log.Append(ctx, []kernel.Event{{
		UID:        relUID,
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "test",
		Confidence: 1.0,
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("append describes relation: %v", err)
	}
}

func applyDocFact(t *testing.T, ctx context.Context, log *kernel.FileEventLog, factUID, docUID string) {
	t.Helper()
	stateKey := "timeless.decision.ha1b2c3d"
	value := "Ship Friday"
	payload, err := json.Marshal(ontology.Fact{
		Name:           "facts/" + factUID,
		UID:            factUID,
		SubjectUID:     docUID,
		StateKey:       stateKey,
		Value:          value,
		CreateTime:     time.Now().UTC(),
		SourceEventUID: ulid.Make().String(),
		ContentHash:    "deadbeef00000000deadbeef00000000",
	})
	if err != nil {
		t.Fatalf("marshal doc fact: %v", err)
	}
	if _, err := log.Append(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindFact),
		Source:     "document:import",
		Confidence: 1.0,
		Payload:    payload,
	}}); err != nil {
		t.Fatalf("append doc fact: %v", err)
	}
}
