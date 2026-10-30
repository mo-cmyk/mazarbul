package document_test

// recall_integration_test.go proves P3's unified-recall contract on the real
// stack (FileEventLog → dispatcher → SQLite + FTS5 + sqlite-vec → HybridQuery):
// vault documents are first-class recall candidates in the SAME ranked result
// set as episodes — found through documents_fts, anchored to their section
// (Source.Path), carrying graph anchors from their wikilink edges, and gated
// by RecallRequest.Kinds. Assertions are membership + metadata (never strict
// rank order) so background chunk embeddings cannot race the test.

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

// recallStack wires a stack plus a HybridQuery over the same dispatcher.
type recallStack struct {
	*stack
	query *kernel.HybridQuery
}

func newRecallStack(t *testing.T) *recallStack {
	t.Helper()
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
	vec, err := dis.OpenVector(ctx)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	fts, err := dis.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	hq, err := kernel.NewHybridQuery(fakeEmbedder{dim: storage.VectorDim}, vec, fts, reader)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	return &recallStack{
		stack: &stack{ctx: ctx, log: log, nav: nav, im: document.NewImporter(log, nav, zap.NewNop())},
		query: hq,
	}
}

// deleteDocument appends a documentDeleted tombstone for the given document.
func (s *recallStack) deleteDocument(t *testing.T, docUID, path string) error {
	t.Helper()
	payload, err := json.Marshal(ontology.DocumentDeleted{DocumentUID: docUID, Path: path})
	if err != nil {
		return err
	}
	_, err = s.log.Append(s.ctx, []kernel.Event{{
		Kind: kernel.EventKind(ontology.KindDocumentDeleted), Source: "test", Confidence: 1.0, Payload: payload,
	}})
	return err
}

// recallAll drains the recall iterator.
func (s *recallStack) recallAll(t *testing.T, req *kernel.RecallRequest) []kernel.Hit {
	t.Helper()
	hits := make([]kernel.Hit, 0, 8)
	for hit, err := range s.query.Recall(context.Background(), *req) {
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		hits = append(hits, hit)
	}
	return hits
}

func findDocHit(hits []kernel.Hit, uid string) *kernel.Hit {
	for i := range hits {
		if hits[i].Kind == kernel.EventKind(ontology.KindDocumentRevision) && hits[i].UID == uid {
			return &hits[i]
		}
	}
	return nil
}

func TestRecall_DocumentSurfacesWithSourceAndAnchors(t *testing.T) {
	s := newRecallStack(t)
	personUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/zanzibar.md",
		[]byte("# Zanzibar plan\n\nThe zanzibar rollout is owned by [[Rosie Cotton]].\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	hits := s.recallAll(t, &kernel.RecallRequest{Query: "zanzibar"})
	hit := findDocHit(hits, docUID)
	if hit == nil {
		t.Fatalf("document not in recall results; got %d hits", len(hits))
	}
	if hit.Source == nil || hit.Source.Path != "notes/zanzibar.md" {
		t.Fatalf("hit.Source = %+v, want notes/zanzibar.md", hit.Source)
	}
	// A document self-cites its get-able document-entity handle (entities/{uid}),
	// not the revision event: a bare events/{revisionUID} is not fetchable by any
	// read tool. The revision/section location lives on hit.Source instead.
	if len(hit.Citations) != 1 || hit.Citations[0] != "entities/"+docUID {
		t.Fatalf("citations = %v, want entities/%s", hit.Citations, docUID)
	}
	if hit.Confidence != 1.0 {
		t.Fatalf("confidence = %v", hit.Confidence)
	}
	// The wikilink edge becomes a graph anchor: seed-then-traverse pivots to
	// the person without another search.
	var anchored bool
	for _, ref := range hit.Entities {
		if ref.UID == personUID {
			anchored = true
		}
	}
	if !anchored {
		t.Fatalf("entities = %+v, want anchor to %s (Rosie Cotton)", hit.Entities, personUID)
	}
}

func TestRecall_KindsFilterGatesDocuments(t *testing.T) {
	s := newRecallStack(t)
	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "gated.md", []byte("xylophone practice notes\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	// Episode-only filter: the document must not appear.
	hits := s.recallAll(t, &kernel.RecallRequest{
		Query: "xylophone",
		Kinds: []kernel.EventKind{kernel.EventKind(ontology.KindEpisode)},
	})
	if h := findDocHit(hits, docUID); h != nil {
		t.Fatalf("document leaked through an episode-only Kinds filter: %+v", h)
	}

	// Document-only filter: the document is found even with zero episodes in
	// the store (the empty episode union must not short-circuit recall).
	hits = s.recallAll(t, &kernel.RecallRequest{
		Query: "xylophone",
		Kinds: []kernel.EventKind{kernel.EventKind(ontology.KindDocumentRevision)},
	})
	if h := findDocHit(hits, docUID); h == nil {
		t.Fatalf("document missing under a documentRevision Kinds filter; got %d hits", len(hits))
	}
}

// seedPlace appends a place entity with WGS84 coordinates and returns its UID.
func (s *recallStack) seedPlace(t *testing.T, displayName string, lat, lon float64) string {
	t.Helper()
	uid := ulid.Make().String()
	payload, err := json.Marshal(ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindPlace,
		DisplayName:      displayName,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		Coordinates:      &ontology.Coordinates{Latitude: lat, Longitude: lon},
		CreateTime:       time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal seed place: %v", err)
	}
	if _, err := s.log.Append(s.ctx, []kernel.Event{{
		Kind: kernel.EventKind(ontology.KindEntity), Source: "test", Confidence: 1.0, Payload: payload,
	}}); err != nil {
		t.Fatalf("append seed place: %v", err)
	}
	return uid
}

// TestRecall_NearFiltersDocuments proves the fix for the "Near skips documents"
// review finding: the spatial geofence applies to vault documents exactly as it
// does to episodes. A document whose wikilink cites a place inside the radius
// survives; the same document is dropped when the recall is anchored to a far
// place, so an out-of-region note cannot leak into a "near X" recall.
func TestRecall_NearFiltersDocuments(t *testing.T) {
	s := newRecallStack(t)
	// Berlin and Tokyo are ~8900 km apart — a 100 km radius cannot overlap.
	s.seedPlace(t, "Berlin", 52.52, 13.40)
	s.seedPlace(t, "Tokyo", 35.68, 139.69)

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/trip.md",
		[]byte("# Trip\n\nThe summit happens in [[Berlin]] next spring.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	// Anchored near Berlin (the cited place): the document must survive.
	nearBerlin := s.recallAll(t, &kernel.RecallRequest{
		Query: "summit",
		Near:  &kernel.NearFilter{PlaceQuery: "Berlin", RadiusKm: 100},
	})
	if findDocHit(nearBerlin, docUID) == nil {
		t.Fatalf("document citing [[Berlin]] dropped from a near-Berlin recall; got %d hits", len(nearBerlin))
	}

	// Anchored near Tokyo (8900 km from the only cited place): must be gone.
	nearTokyo := s.recallAll(t, &kernel.RecallRequest{
		Query: "summit",
		Near:  &kernel.NearFilter{PlaceQuery: "Tokyo", RadiusKm: 100},
	})
	if h := findDocHit(nearTokyo, docUID); h != nil {
		t.Fatalf("out-of-region document leaked into a near-Tokyo recall: %+v", h)
	}
}

// TestRecall_TypedWikilinkSurfacesThroughGraphLane proves the fix for the
// "typed predicates missed by document graph recall" review finding: a typed
// wikilink such as `[supports] [[Rosie Cotton]]` is stored under its typed
// predicate, not as a default mentions/links_to edge. The document graph lane
// and the seed-and-pivot anchor attach must therefore include the full document
// predicate set, or a Rosie-anchored note would only surface when lexical or
// vector search happened to find it. Asserts the typed-link doc both surfaces
// and carries the Rosie anchor for seed-then-traverse.
func TestRecall_TypedWikilinkSurfacesThroughGraphLane(t *testing.T) {
	s := newRecallStack(t)
	personUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/proposal.md",
		[]byte("# Proposal\n\nThe migration plan [supports] [[Rosie Cotton]].\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	hits := s.recallAll(t, &kernel.RecallRequest{Query: "migration"})
	hit := findDocHit(hits, docUID)
	if hit == nil {
		t.Fatalf("typed-wikilink document not in recall results; got %d hits", len(hits))
	}
	// The typed `supports` edge must become a graph anchor, exactly like a
	// default mentions edge does — proving attachDocumentEntities gates on the
	// full document predicate set, not just mentions/links_to.
	var anchored bool
	for _, ref := range hit.Entities {
		if ref.UID == personUID {
			anchored = true
		}
	}
	if !anchored {
		t.Fatalf("typed-wikilink hit lost its [supports] anchor; entities = %+v, want %s (Rosie Cotton)", hit.Entities, personUID)
	}
}

// TestRecall_NearFiltersTypedWikilinkDocuments proves the fix for the "geofence
// drops typed links" review finding: the spatial geofence must follow the full
// document predicate set, not just mentions/links_to. A document that cites a
// place only through a typed wikilink (`[depends_on] [[Berlin]]`) is collected
// as a document candidate; the geofence must keep it for a near-Berlin recall
// and drop it for a far near-Tokyo recall, just like a default-edge citation.
func TestRecall_NearFiltersTypedWikilinkDocuments(t *testing.T) {
	s := newRecallStack(t)
	s.seedPlace(t, "Berlin", 52.52, 13.40)
	s.seedPlace(t, "Tokyo", 35.68, 139.69)

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/roadmap.md",
		[]byte("# Roadmap\n\nThe launch [depends_on] [[Berlin]] readiness.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	// Near Berlin (the typed-linked place): the document must survive the geofence.
	nearBerlin := s.recallAll(t, &kernel.RecallRequest{
		Query: "launch",
		Near:  &kernel.NearFilter{PlaceQuery: "Berlin", RadiusKm: 100},
	})
	if findDocHit(nearBerlin, docUID) == nil {
		t.Fatalf("document with a [depends_on] [[Berlin]] link dropped from a near-Berlin recall; got %d hits", len(nearBerlin))
	}

	// Near Tokyo (8900 km away): the typed-linked place is out of region, so the
	// document must be dropped — the geofence must not silently pass it through.
	nearTokyo := s.recallAll(t, &kernel.RecallRequest{
		Query: "launch",
		Near:  &kernel.NearFilter{PlaceQuery: "Tokyo", RadiusKm: 100},
	})
	if h := findDocHit(nearTokyo, docUID); h != nil {
		t.Fatalf("out-of-region typed-wikilink document leaked into a near-Tokyo recall: %+v", h)
	}
}

// seedRelation appends a Relation event with an explicit bi-temporal validity
// window (valid_from/valid_until), bypassing the importer so a test can stage a
// superseded wikilink edge.
func (s *recallStack) seedRelation(t *testing.T, subjectUID, predicate, objectUID string, validFrom, validUntil time.Time) {
	t.Helper()
	uid := ulid.Make().String()
	vf, vu := validFrom.UTC(), validUntil.UTC()
	payload, err := json.Marshal(ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     subjectUID,
		Predicate:      predicate,
		ObjectUID:      objectUID,
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     validFrom.UTC(),
		ValidFrom:      &vf,
		ValidUntil:     &vu,
	})
	if err != nil {
		t.Fatalf("marshal seed relation: %v", err)
	}
	if _, err := s.log.Append(s.ctx, []kernel.Event{{
		Kind: kernel.EventKind(ontology.KindRelation), Source: "test", Confidence: 1.0, Payload: payload,
	}}); err != nil {
		t.Fatalf("append seed relation: %v", err)
	}
}

// TestRecall_NearHonorsValidTimeForDocuments proves the "Near ignores history"
// fix: the document geofence applies the SAME valid-time window as the graph
// lane. A wikilink edge that was valid at the requested historical instant — but
// has since been superseded (valid_until in the past) — must keep its document
// in-region for an `At`-scoped near recall, while a present-tense near recall
// (the edge no longer live) drops it.
func TestRecall_NearHonorsValidTimeForDocuments(t *testing.T) {
	s := newRecallStack(t)
	berlinUID := s.seedPlace(t, "Berlin", 52.52, 13.40)

	// Import WITHOUT a wikilink so the importer creates no live auto-edge; the
	// only doc→Berlin edge is the historical one we stage below.
	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/history.md",
		[]byte("# History\n\nThe summit was planned long ago.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	// A doc→Berlin mention valid only across 2020 (long superseded).
	past := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	pastEnd := time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC)
	s.seedRelation(t, docUID, "mentions", berlinUID, past, pastEnd)

	// Present-tense near-Berlin recall: the edge is no longer valid, so the
	// document must NOT pass the geofence.
	now := s.recallAll(t, &kernel.RecallRequest{
		Query: "summit",
		Near:  &kernel.NearFilter{PlaceQuery: "Berlin", RadiusKm: 100},
	})
	if h := findDocHit(now, docUID); h != nil {
		t.Fatalf("document with a superseded Berlin edge leaked into a present near-Berlin recall: %+v", h)
	}

	// Historical near-Berlin recall (At inside the 2020 window): the edge was
	// valid then, so the document must survive the geofence.
	at := time.Date(2020, 8, 1, 0, 0, 0, 0, time.UTC)
	hist := s.recallAll(t, &kernel.RecallRequest{
		Query: "summit",
		At:    &at,
		Near:  &kernel.NearFilter{PlaceQuery: "Berlin", RadiusKm: 100},
	})
	histHit := findDocHit(hist, docUID)
	if histHit == nil {
		t.Fatalf("document valid-at-2020 dropped from a historical near-Berlin recall; got %d hits", len(hist))
	}
	// The pivot anchor must reflect the graph AS IT WAS VALID THEN:
	// attachDocumentEntities must use the same At window, so the 2020-valid
	// Berlin edge surfaces as an anchor for seed-and-pivot on this very hit.
	var anchored bool
	for _, ref := range histHit.Entities {
		if ref.UID == berlinUID {
			anchored = true
		}
	}
	if !anchored {
		t.Fatalf("historical document hit lost its valid-at-2020 Berlin anchor; entities = %+v", histHit.Entities)
	}
}

func TestRecall_DeletedDocumentDoesNotSurface(t *testing.T) {
	s := newRecallStack(t)
	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "gone.md", []byte("quokka habitat survey\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	if hits := s.recallAll(t, &kernel.RecallRequest{Query: "quokka"}); findDocHit(hits, docUID) == nil {
		t.Fatal("precondition: live document should surface")
	}

	if err := s.deleteDocument(t, docUID, "gone.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if hits := s.recallAll(t, &kernel.RecallRequest{Query: "quokka"}); findDocHit(hits, docUID) != nil {
		t.Fatal("tombstoned document still surfaces in recall")
	}
}
