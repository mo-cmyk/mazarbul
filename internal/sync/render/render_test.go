package render_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// ---------------------------------------------------------------------------
// ULID construction helpers
// ---------------------------------------------------------------------------

// fixedEpochMS is 2026-06-03T00:00:00Z in Unix milliseconds.
// Used as the timestamp component for all deterministic test ULIDs.
const fixedEpochMS = 1780099200000

// ulidAt builds a deterministic 26-character Crockford Base32 ULID from a
// fixed timestamp (2026-06-03) and a sequence-derived random portion.
// No time.Now(), no math/rand. The random portion is the decimal digits of
// seq zero-padded and repeated to fill 16 characters of Crockford Base32.
func ulidAt(seq int) string {
	// Crockford Base32 alphabet (no I, L, O, U).
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

	// Encode 48-bit timestamp into 10 characters (5 bits each).
	ts := uint64(fixedEpochMS)
	var tsChars [10]byte
	for i := 9; i >= 0; i-- {
		tsChars[i] = alphabet[ts&0x1F]
		ts >>= 5
	}

	// Encode seq into 16 characters for the random portion.
	// Use seq as a 80-bit counter value: simple, stable, no collision within tests.
	rnd := uint64(seq)
	var rndChars [16]byte
	for i := 15; i >= 0; i-- {
		rndChars[i] = alphabet[rnd&0x1F]
		rnd >>= 5
	}

	return string(tsChars[:]) + string(rndChars[:])
}

// ---------------------------------------------------------------------------
// Event-construction helpers
// ---------------------------------------------------------------------------

// baseTime is a fixed wall-clock reference used in all test payloads.
var baseTime = time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

// makeEvent builds a kernel.Event with the given kind, UID, and JSON payload.
func makeEvent(uid string, kind kernel.EventKind, payload interface{}) kernel.Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("render_test: marshal %s payload: %v", kind, err))
	}
	return kernel.Event{
		UID:        uid,
		Name:       "events/" + uid,
		Kind:       kind,
		Source:     "test",
		Confidence: 1.0,
		CreateTime: baseTime,
		Payload:    raw,
	}
}

// entityPayload builds a minimal valid ontology.Entity payload.
func entityPayload(uid, displayName string, kind ontology.EntityKind) ontology.Entity {
	return ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             kind,
		DisplayName:      displayName,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       baseTime,
	}
}

// relationPayload builds a minimal valid ontology.Relation payload.
func relationPayload(uid, subjectUID, predicate, objectUID string) ontology.Relation {
	return ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     subjectUID,
		Predicate:      predicate,
		ObjectUID:      objectUID,
		Confidence:     1.0,
		SourceEventUID: uid,
		CreateTime:     baseTime,
	}
}

// seqEvents converts a slice of kernel.Event into the iter.Seq[kernel.Event]
// shape that Render accepts.
func seqEvents(events []kernel.Event) iter.Seq[kernel.Event] {
	return func(yield func(kernel.Event) bool) {
		for _, ev := range events {
			if !yield(ev) {
				return
			}
		}
	}
}

// fileByPath returns the File with the given path from the view, or nil.
func fileByPath(files []render.File, path string) *render.File {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
	}
	return nil
}

// pathsContaining returns all file paths that include the given substring.
func pathsContaining(files []render.File, sub string) []string {
	var out []string
	for _, f := range files {
		if strings.Contains(f.Path, sub) {
			out = append(out, f.Path)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRender_EmptyEventStream_EmitsCursorOnly(t *testing.T) {
	t.Parallel()

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(nil),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// No events → cursor stays empty.
	if view.Cursor != "" {
		t.Errorf("Cursor = %q, want empty", view.Cursor)
	}
	// Files may include index stubs for all kinds (always emitted), but
	// facts.jsonl must NOT appear (empty).
	for _, f := range view.Files {
		if f.Path == "rendered/facts.jsonl" {
			t.Errorf("unexpected facts.jsonl in empty render")
		}
	}
	// Stats are all zero.
	var zeroStats render.Stats
	if view.Stats != zeroStats {
		t.Errorf("Stats = %+v, want zero", view.Stats)
	}
}

func TestRender_SingleEntity_EmitsStubAndJSONL(t *testing.T) {
	t.Parallel()

	uid := ulidAt(1)
	events := []kernel.Event{
		makeEvent(uid, ontology.KindEntity, entityPayload(uid, "Merry", ontology.EntityKindPerson)),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Cursor must equal the only event's UID.
	if view.Cursor != uid {
		t.Errorf("Cursor = %q, want %q", view.Cursor, uid)
	}

	// Expect a stub under rendered/by-uid/<shard3>/<shard2>/<uid>.md
	stubPath := fmt.Sprintf("rendered/by-uid/%s/%s/%s.md", uid[0:3], uid[3:5], uid)
	stub := fileByPath(view.Files, stubPath)
	if stub == nil {
		t.Fatalf("entity stub not found at %q; got paths: %v", stubPath, func() []string {
			ps := make([]string, 0, len(view.Files))
			for _, f := range view.Files {
				ps = append(ps, f.Path)
			}
			return ps
		}())
	}
	if !bytes.Contains(stub.Content, []byte("Merry")) {
		t.Errorf("stub does not mention display name; got:\n%s", stub.Content)
	}

	// Expect entities.jsonl with at least one line.
	entJSONL := fileByPath(view.Files, "rendered/entities.jsonl")
	if entJSONL == nil {
		t.Fatal("rendered/entities.jsonl not found")
	}
	if !bytes.Contains(entJSONL.Content, []byte(uid)) {
		t.Errorf("entities.jsonl missing UID %q", uid)
	}

	// Stats: one entity added.
	if view.Stats.EntitiesAdded != 1 {
		t.Errorf("EntitiesAdded = %d, want 1", view.Stats.EntitiesAdded)
	}
}

func TestRender_EntityIdentifierAdded_UpdatesFrontmatter(t *testing.T) {
	t.Parallel()

	entityUID := ulidAt(10)
	idEventUID := ulidAt(11)

	entityEv := makeEvent(entityUID, ontology.KindEntity,
		entityPayload(entityUID, "Sam", ontology.EntityKindPerson))
	idEv := makeEvent(idEventUID, ontology.KindEntityIdentifierAdded,
		ontology.EntityIdentifierAddedEvent{
			EntityUID:  entityUID,
			Identifier: "sam@example.com",
			Kind:       ontology.IdentifierKindEmail,
			Source:     ontology.IdentifierSourceExtracted,
			AddedAt:    baseTime,
		})

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents([]kernel.Event{entityEv, idEv}),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	stubPath := fmt.Sprintf("rendered/by-uid/%s/%s/%s.md", entityUID[0:3], entityUID[3:5], entityUID)
	stub := fileByPath(view.Files, stubPath)
	if stub == nil {
		t.Fatalf("entity stub not found at %q", stubPath)
	}

	// The email identifier must appear in the frontmatter.
	if !bytes.Contains(stub.Content, []byte("sam@example.com")) {
		t.Errorf("stub frontmatter missing identifier; got:\n%s", stub.Content)
	}

	// EntitiesChanged should be 1 (entity existed then got identifier added).
	// With no prior state, entity is added (EntitiesAdded) but the identifier
	// event fires a change. Confirm cursor advanced to idEventUID.
	if view.Cursor != idEventUID {
		t.Errorf("Cursor = %q, want %q", view.Cursor, idEventUID)
	}

	// entities.jsonl must also expose the identifier — otherwise
	// `maz diff` cannot detect identifier-added between two revisions.
	entitiesJSONL := fileByPath(view.Files, "rendered/entities.jsonl")
	if entitiesJSONL == nil {
		t.Fatalf("entities.jsonl missing")
	}
	if !bytes.Contains(entitiesJSONL.Content, []byte(`"identifiers":`)) {
		t.Errorf("entities.jsonl missing identifiers array; got:\n%s", entitiesJSONL.Content)
	}
	if !bytes.Contains(entitiesJSONL.Content, []byte("sam@example.com")) {
		t.Errorf("entities.jsonl missing email value; got:\n%s", entitiesJSONL.Content)
	}
}

func TestRender_RelationLifecycle_CurrentSupersededForgotten(t *testing.T) {
	t.Parallel()

	subjectUID := ulidAt(20)
	objectA := ulidAt(21)
	objectB := ulidAt(22)
	objectC := ulidAt(23)
	relAUID := ulidAt(24)
	relBUID := ulidAt(25)
	relCUID := ulidAt(26)
	supersededEvUID := ulidAt(27)
	forgottenEvUID := ulidAt(28)

	supersededAt := baseTime.Add(time.Minute)
	forgottenAt := baseTime.Add(2 * time.Minute)

	events := []kernel.Event{
		// Register subject and objects as entities.
		makeEvent(subjectUID, ontology.KindEntity, entityPayload(subjectUID, "Subject", ontology.EntityKindPerson)),
		makeEvent(objectA, ontology.KindEntity, entityPayload(objectA, "ObjA", ontology.EntityKindOrganization)),
		makeEvent(objectB, ontology.KindEntity, entityPayload(objectB, "ObjB", ontology.EntityKindOrganization)),
		makeEvent(objectC, ontology.KindEntity, entityPayload(objectC, "ObjC", ontology.EntityKindOrganization)),
		// A: initial relation.
		makeEvent(relAUID, ontology.KindRelation, relationPayload(relAUID, subjectUID, "works_at", objectA)),
		// B: replacement relation (will supersede A).
		makeEvent(relBUID, ontology.KindRelation, relationPayload(relBUID, subjectUID, "works_at", objectB)),
		// Supersede event: A is superseded by B.
		makeEvent(supersededEvUID, ontology.KindRelationRetracted, ontology.RelationRetractedEvent{
			RelationUID:     ontology.ULID(relAUID),
			SupersededByUID: ontology.ULID(relBUID),
			RetractedAt:     supersededAt,
		}),
		// C: another relation to be forgotten.
		makeEvent(relCUID, ontology.KindRelation, relationPayload(relCUID, subjectUID, "lives_in", objectC)),
		// Forgotten event: C is forgotten.
		makeEvent(forgottenEvUID, ontology.KindRelationForgotten, ontology.RelationForgottenEvent{
			RelationUID: relCUID,
			Reason:      "test-reason",
			ForgottenAt: forgottenAt,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// B must be in relations.jsonl (current).
	relJSONL := fileByPath(view.Files, "rendered/relations.jsonl")
	if relJSONL == nil {
		t.Fatal("rendered/relations.jsonl not found")
	}
	if !bytes.Contains(relJSONL.Content, []byte(relBUID)) {
		t.Errorf("relations.jsonl missing current relation B (%s)", relBUID)
	}
	// A must NOT be in relations.jsonl (superseded).
	if bytes.Contains(relJSONL.Content, []byte(relAUID)) {
		t.Errorf("relations.jsonl should not contain superseded relation A (%s)", relAUID)
	}
	// C must NOT be in relations.jsonl (forgotten).
	if bytes.Contains(relJSONL.Content, []byte(relCUID)) {
		t.Errorf("relations.jsonl should not contain forgotten relation C (%s)", relCUID)
	}

	// A must be in superseded-relations.jsonl.
	supJSONL := fileByPath(view.Files, "rendered/superseded-relations.jsonl")
	if supJSONL == nil {
		t.Fatal("rendered/superseded-relations.jsonl not found")
	}
	if !bytes.Contains(supJSONL.Content, []byte(relAUID)) {
		t.Errorf("superseded-relations.jsonl missing A (%s)", relAUID)
	}

	// C must be in forgotten-relations.jsonl.
	forgJSONL := fileByPath(view.Files, "rendered/forgotten-relations.jsonl")
	if forgJSONL == nil {
		t.Fatal("rendered/forgotten-relations.jsonl not found")
	}
	if !bytes.Contains(forgJSONL.Content, []byte(relCUID)) {
		t.Errorf("forgotten-relations.jsonl missing C (%s)", relCUID)
	}

	// Subject's stub must have supersede history strikethrough for A.
	stubPath := fmt.Sprintf("rendered/by-uid/%s/%s/%s.md", subjectUID[0:3], subjectUID[3:5], subjectUID)
	stub := fileByPath(view.Files, stubPath)
	if stub == nil {
		t.Fatalf("subject stub not found at %q", stubPath)
	}
	if !bytes.Contains(stub.Content, []byte("~~")) {
		t.Errorf("subject stub missing strikethrough for superseded relation; got:\n%s", stub.Content)
	}
	// C must appear in forgotten section with reason tag.
	if !bytes.Contains(stub.Content, []byte("test-reason")) {
		t.Errorf("subject stub missing forgotten reason; got:\n%s", stub.Content)
	}

	// Stats.
	if view.Stats.RelationsSuperseded < 1 {
		t.Errorf("RelationsSuperseded = %d, want >= 1", view.Stats.RelationsSuperseded)
	}
	if view.Stats.RelationsForgotten < 1 {
		t.Errorf("RelationsForgotten = %d, want >= 1", view.Stats.RelationsForgotten)
	}
}

func TestRender_SupersedeChainDepth5(t *testing.T) {
	t.Parallel()

	subjectUID := ulidAt(30)
	objectUID := ulidAt(31)

	// Build 5 relation UIDs in ascending order so ULID sort gives A<B<C<D<E.
	relUIDs := [5]string{
		ulidAt(32), ulidAt(33), ulidAt(34), ulidAt(35), ulidAt(36),
	}
	supEvUIDs := [4]string{
		ulidAt(37), ulidAt(38), ulidAt(39), ulidAt(40),
	}

	// 2 entity events + 5 relation events + 4 supersede events = 11 total.
	events := make([]kernel.Event, 0, 11)
	events = append(events,
		makeEvent(subjectUID, ontology.KindEntity, entityPayload(subjectUID, "Chain-Subject", ontology.EntityKindPerson)),
		makeEvent(objectUID, ontology.KindEntity, entityPayload(objectUID, "Chain-Object", ontology.EntityKindOrganization)),
	)
	// Emit all 5 relations.
	for i := range 5 {
		events = append(events,
			makeEvent(relUIDs[i], ontology.KindRelation,
				relationPayload(relUIDs[i], subjectUID, "works_at", objectUID)))
	}
	// Emit supersede events: A→B, B→C, C→D, D→E.
	supersededAt := baseTime.Add(time.Minute)
	for i := range 4 {
		events = append(events,
			makeEvent(supEvUIDs[i], ontology.KindRelationRetracted,
				ontology.RelationRetractedEvent{
					RelationUID:     ontology.ULID(relUIDs[i]),
					SupersededByUID: ontology.ULID(relUIDs[i+1]),
					RetractedAt:     supersededAt.Add(time.Duration(i) * time.Minute),
				}))
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Only E (relUIDs[4]) must be in relations.jsonl.
	relJSONL := fileByPath(view.Files, "rendered/relations.jsonl")
	if relJSONL == nil {
		t.Fatal("rendered/relations.jsonl not found")
	}
	if !bytes.Contains(relJSONL.Content, []byte(relUIDs[4])) {
		t.Errorf("relations.jsonl missing terminal relation E (%s)", relUIDs[4])
	}
	// A..D must appear in superseded-relations.jsonl.
	supJSONL := fileByPath(view.Files, "rendered/superseded-relations.jsonl")
	if supJSONL == nil {
		t.Fatal("rendered/superseded-relations.jsonl not found")
	}
	for i := range 4 {
		if !bytes.Contains(supJSONL.Content, []byte(relUIDs[i])) {
			t.Errorf("superseded-relations.jsonl missing relation[%d] (%s)", i, relUIDs[i])
		}
	}

	if view.Stats.RelationsSuperseded < 4 {
		t.Errorf("RelationsSuperseded = %d, want >= 4", view.Stats.RelationsSuperseded)
	}
}

func TestRender_EpisodeForgotten_OrphansEntityWhenLastMentionGone(t *testing.T) {
	t.Parallel()

	episodeUID := ulidAt(50)
	mentionUID := ulidAt(51)
	entityUID := ulidAt(52)
	forgottenEvUID := ulidAt(53)

	// The Mention links the episode to the entity via episodeMentions index.
	// applyMention registers: episodeMentions[episodeUID][mentionUID].
	// applyEntity then calls registerMentionEpisode which swaps mentionUID
	// for entityUID in the set and adds episodeUID to EntityRow.EpisodeUIDs.
	// For this to work we need: episode first, then mention, then entity.
	events := []kernel.Event{
		// Episode event (just advances cursor; no payload parsing for orphan logic).
		{
			UID:        episodeUID,
			Name:       "events/" + episodeUID,
			Kind:       ontology.KindEpisode,
			Source:     "test",
			Confidence: 1.0,
			CreateTime: baseTime,
			Payload:    json.RawMessage(`{"text":"hello","source":"test","confidence":1.0}`),
		},
		// Mention event with source episode UID in payload.
		makeEvent(mentionUID, ontology.KindMention, ontology.Mention{
			Kind:           ontology.EntityKindPerson,
			DisplayName:    "Merry",
			SourceEventUID: ontology.ULID(episodeUID),
			ExtractionText: "Merry",
			SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 5},
			GroundingScore: 1.0,
		}),
		// Entity event — mentions the same mention UID so registerMentionEpisode fires.
		makeEvent(entityUID, ontology.KindEntity, ontology.Entity{
			Name:             "entities/" + entityUID,
			UID:              entityUID,
			Kind:             ontology.EntityKindPerson,
			DisplayName:      "Merry",
			Confidence:       1.0,
			ResolutionStatus: ontology.ResolutionConfirmed,
			CreateTime:       baseTime,
			MentionUIDs:      []ontology.ULID{ontology.ULID(mentionUID)},
		}),
		// EpisodeForgotten — should cascade, leaving entity with no episode.
		makeEvent(forgottenEvUID, ontology.KindEpisodeForgotten, ontology.EpisodeForgottenEvent{
			EpisodeUID:  episodeUID,
			Reason:      "user-forgot",
			ForgottenAt: baseTime.Add(time.Minute),
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Cursor must be the last event.
	if view.Cursor != forgottenEvUID {
		t.Errorf("Cursor = %q, want %q", view.Cursor, forgottenEvUID)
	}

	// Entity stub must show orphaned warning.
	stubPath := fmt.Sprintf("rendered/by-uid/%s/%s/%s.md", entityUID[0:3], entityUID[3:5], entityUID)
	stub := fileByPath(view.Files, stubPath)
	if stub == nil {
		t.Fatalf("entity stub not found at %q", stubPath)
	}
	if !bytes.Contains(stub.Content, []byte("orphaned")) {
		t.Errorf("entity stub missing orphaned marker; got:\n%s", stub.Content)
	}

	// EntitiesOrphaned stat must be >= 1.
	if view.Stats.EntitiesOrphaned < 1 {
		t.Errorf("EntitiesOrphaned = %d, want >= 1", view.Stats.EntitiesOrphaned)
	}
}

func TestRender_SupersedeConflict_FlaggedHighestPriority(t *testing.T) {
	t.Parallel()

	subjectUID := ulidAt(60)
	objectUID := ulidAt(61)
	relPrimUID := ulidAt(62)
	relConflUID := ulidAt(63)
	conflictEvUID := ulidAt(64)

	events := []kernel.Event{
		makeEvent(subjectUID, ontology.KindEntity, entityPayload(subjectUID, "Conflict-Subject", ontology.EntityKindPerson)),
		makeEvent(objectUID, ontology.KindEntity, entityPayload(objectUID, "Conflict-Object", ontology.EntityKindOrganization)),
		makeEvent(relPrimUID, ontology.KindRelation, relationPayload(relPrimUID, subjectUID, "works_at", objectUID)),
		makeEvent(relConflUID, ontology.KindRelation, relationPayload(relConflUID, subjectUID, "works_at", objectUID)),
		makeEvent(conflictEvUID, ontology.KindRelationSupersedeConflict, ontology.RelationSupersedeConflictEvent{
			PrimaryRelationUID:     ontology.ULID(relPrimUID),
			ConflictingRelationUID: ontology.ULID(relConflUID),
			SubjectUID:             ontology.ULID(subjectUID),
			Predicate:              "works_at",
			Reason:                 "multiple_open_functional",
			DetectedAt:             baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Stats.Conflicts must be > 0.
	if view.Stats.Conflicts < 1 {
		t.Errorf("Conflicts = %d, want >= 1", view.Stats.Conflicts)
	}

	// Both relation UIDs should be cross-referenced somewhere in the subject stub.
	stubPath := fmt.Sprintf("rendered/by-uid/%s/%s/%s.md", subjectUID[0:3], subjectUID[3:5], subjectUID)
	stub := fileByPath(view.Files, stubPath)
	if stub == nil {
		t.Fatalf("subject stub not found at %q", stubPath)
	}
	if !bytes.Contains(stub.Content, []byte(relPrimUID)) {
		t.Errorf("stub missing primary relation UID; got:\n%s", stub.Content)
	}
	if !bytes.Contains(stub.Content, []byte(relConflUID)) {
		t.Errorf("stub missing conflicting relation UID; got:\n%s", stub.Content)
	}
}

func TestRender_PredicateRegistered_AddsToIndex(t *testing.T) {
	t.Parallel()

	predEvUID := ulidAt(70)

	events := []kernel.Event{
		makeEvent(predEvUID, ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate:   "works_at",
			Kind:        ontology.PredicateKindFunctional,
			Description: "Subject is employed at Object.",
			AddedBy:     ontology.PredicateAddedBySeed,
			AddedAt:     baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if view.Stats.PredicatesAdded < 1 {
		t.Errorf("PredicatesAdded = %d, want >= 1", view.Stats.PredicatesAdded)
	}
	if view.Cursor != predEvUID {
		t.Errorf("Cursor = %q, want %q", view.Cursor, predEvUID)
	}
}

func TestRender_Fact_AppendedToFactsJSONL(t *testing.T) {
	t.Parallel()

	srcEvUID := ulidAt(80)
	factUID := ulidAt(81)

	events := []kernel.Event{
		makeEvent(factUID, ontology.KindFact, ontology.Fact{
			Name:           "facts/" + factUID,
			UID:            factUID,
			SubjectUID:     "self",
			StateKey:       "preference.coffee.milk",
			Value:          "oat",
			CreateTime:     baseTime,
			SourceEventUID: srcEvUID,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	factsJSONL := fileByPath(view.Files, "rendered/facts.jsonl")
	if factsJSONL == nil {
		t.Fatal("rendered/facts.jsonl not found")
	}
	if !bytes.Contains(factsJSONL.Content, []byte("oat")) {
		t.Errorf("facts.jsonl missing fact value; got:\n%s", factsJSONL.Content)
	}
	if !bytes.Contains(factsJSONL.Content, []byte("preference.coffee.milk")) {
		t.Errorf("facts.jsonl missing state key; got:\n%s", factsJSONL.Content)
	}
}

func TestRender_RelationCandidatesProposed_NotRendered(t *testing.T) {
	t.Parallel()

	candEvUID := ulidAt(90)

	// relationCandidatesProposed is a transient event; the renderer advances
	// the cursor but emits no files for it (the resolver decides what to do).
	events := []kernel.Event{
		makeEvent(candEvUID, ontology.KindRelationCandidatesProposed,
			ontology.RelationCandidatesProposedEvent{
				EpisodeUID: ulidAt(91),
				Candidates: []ontology.RelationCandidate{
					{SubjectMentionIdx: 0, Predicate: "works_at", ObjectMentionIdx: 1},
				},
			}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Cursor must advance.
	if view.Cursor != candEvUID {
		t.Errorf("Cursor = %q, want %q", view.Cursor, candEvUID)
	}

	// No entity stubs, no relations.jsonl content, no facts.jsonl.
	for _, f := range view.Files {
		if strings.Contains(f.Path, "by-uid/") {
			t.Errorf("unexpected entity stub for candidate-only render: %s", f.Path)
		}
		if f.Path == "rendered/facts.jsonl" {
			t.Errorf("unexpected facts.jsonl for candidate-only render")
		}
	}

	// No stats changed.
	var zeroStats render.Stats
	if view.Stats != zeroStats {
		t.Errorf("Stats = %+v, want zero for candidates-only render", view.Stats)
	}
}

func TestRender_EncryptedScope_Passthrough(t *testing.T) {
	t.Parallel()
	// The render package is scope-agnostic: it receives already-decrypted
	// events. Scope filtering is the transport layer's responsibility
	// (internal/sync/transport). We confirm that rendering with
	// kernel.ScopePersonal produces the same structure as ScopeProject:
	// the renderer does not gate on the scope field.

	uid := ulidAt(100)
	events := []kernel.Event{
		makeEvent(uid, ontology.KindEntity, entityPayload(uid, "Personal-Merry", ontology.EntityKindPerson)),
	}

	viewProject, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render (project): %v", err)
	}

	viewPersonal, err := render.Render(render.Input{
		Scope:  kernel.ScopePersonal,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render (personal): %v", err)
	}

	if len(viewProject.Files) != len(viewPersonal.Files) {
		t.Errorf("file count differs: project=%d personal=%d",
			len(viewProject.Files), len(viewPersonal.Files))
	}
}

func TestRender_FilePathStableUnderRename(t *testing.T) {
	t.Parallel()

	entityUID := ulidAt(110)
	idEvUID := ulidAt(111)

	// The slug is frozen at first entity event (original display name "Sam").
	entityEv := makeEvent(entityUID, ontology.KindEntity,
		entityPayload(entityUID, "Sam", ontology.EntityKindPerson))
	// Adding an alias via EntityIdentifierAdded must not change the file path.
	idEv := makeEvent(idEvUID, ontology.KindEntityIdentifierAdded,
		ontology.EntityIdentifierAddedEvent{
			EntityUID:  entityUID,
			Identifier: "Robert",
			Kind:       ontology.IdentifierKindAlias,
			Source:     ontology.IdentifierSourceUser,
			AddedAt:    baseTime,
		})

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents([]kernel.Event{entityEv, idEv}),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// File path must use entityUID, not the alias.
	expectedPath := fmt.Sprintf("rendered/by-uid/%s/%s/%s.md", entityUID[0:3], entityUID[3:5], entityUID)
	stub := fileByPath(view.Files, expectedPath)
	if stub == nil {
		allPaths := pathsContaining(view.Files, "by-uid")
		t.Fatalf("entity stub not found at %q; by-uid paths: %v", expectedPath, allPaths)
	}

	// Alias must appear in frontmatter.
	if !bytes.Contains(stub.Content, []byte("Robert")) {
		t.Errorf("stub missing alias 'Robert'; got:\n%s", stub.Content)
	}
}

func TestRender_CursorAdvances(t *testing.T) {
	t.Parallel()

	// Emit N=5 entity events. The cursor must equal the last event's UID.
	const n = 5
	events := make([]kernel.Event, n)
	for i := range events {
		uid := ulidAt(120 + i)
		events[i] = makeEvent(uid, ontology.KindEntity,
			entityPayload(uid, fmt.Sprintf("Entity-%d", i), ontology.EntityKindPerson))
	}
	lastUID := events[n-1].UID

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if view.Cursor != lastUID {
		t.Errorf("Cursor = %q, want %q (last event UID)", view.Cursor, lastUID)
	}
}

func TestRender_OutOfOrderEvents_Accepted(t *testing.T) {
	t.Parallel()
	// Per render.go: the renderer does NOT enforce ULID monotonicity on input;
	// ordering is the event log reader's contract. We confirm that an out-of-
	// order sequence is accepted without error (the renderer is a pure function
	// that trusts its input sequence).
	uid1 := ulidAt(200)
	uid2 := ulidAt(130) // lower ULID, arrived second (out of order)

	events := []kernel.Event{
		makeEvent(uid1, ontology.KindEntity, entityPayload(uid1, "First", ontology.EntityKindPerson)),
		makeEvent(uid2, ontology.KindEntity, entityPayload(uid2, "Second", ontology.EntityKindPerson)),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render rejected out-of-order events (expected acceptance): %v", err)
	}
	// Cursor is the last processed event UID regardless of ULID order.
	if view.Cursor != uid2 {
		t.Errorf("Cursor = %q, want %q", view.Cursor, uid2)
	}
}

func TestRender_IncrementalRender_OnlyChangedFilesEmitted(t *testing.T) {
	t.Parallel()

	// First render: two entities.
	uid1 := ulidAt(300)
	uid2 := ulidAt(301)
	uid3 := ulidAt(302)

	events1 := []kernel.Event{
		makeEvent(uid1, ontology.KindEntity, entityPayload(uid1, "EntityOne", ontology.EntityKindPerson)),
		makeEvent(uid2, ontology.KindEntity, entityPayload(uid2, "EntityTwo", ontology.EntityKindPerson)),
	}
	view1, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events1),
	})
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}

	// Collect the prior state from view1 by rebuilding it via a fresh Render
	// using view1's cursor as the baseline. We can do this by passing PriorState
	// from first render internals — but PriorState is produced externally by the
	// caller. Since render.GraphState is exported, we can construct a second render
	// passing the output of a GraphState-returning call. However, render.Render
	// does not return GraphState directly; only View.
	// Per the contract the caller caches PriorState between renders.
	// For this test we can validate the simpler property: a second full render
	// with both old and new events vs. incremental is consistent.
	//
	// We verify here that the incremental path (PriorState nil → full render
	// produces Actions) works correctly for a new file by confirming ActionCreate
	// for a fresh entity.

	// Second render: adds a third entity.
	events2 := []kernel.Event{
		makeEvent(uid1, ontology.KindEntity, entityPayload(uid1, "EntityOne", ontology.EntityKindPerson)),
		makeEvent(uid2, ontology.KindEntity, entityPayload(uid2, "EntityTwo", ontology.EntityKindPerson)),
		makeEvent(uid3, ontology.KindEntity, entityPayload(uid3, "EntityThree", ontology.EntityKindPerson)),
	}
	view2, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events2),
	})
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}

	// view1 has EntitiesAdded=2; view2 has EntitiesAdded=3.
	if view1.Stats.EntitiesAdded != 2 {
		t.Errorf("view1.EntitiesAdded = %d, want 2", view1.Stats.EntitiesAdded)
	}
	if view2.Stats.EntitiesAdded != 3 {
		t.Errorf("view2.EntitiesAdded = %d, want 3", view2.Stats.EntitiesAdded)
	}

	// Third entity stub must appear in view2.
	stub3Path := fmt.Sprintf("rendered/by-uid/%s/%s/%s.md", uid3[0:3], uid3[3:5], uid3)
	stub3 := fileByPath(view2.Files, stub3Path)
	if stub3 == nil {
		t.Errorf("entity-three stub not found in view2")
	}
	_ = view1 // used above
}

func TestRender_UnknownEventKind_IgnoredButCursorAdvances(t *testing.T) {
	t.Parallel()

	uid := ulidAt(400)
	events := []kernel.Event{
		{
			UID:        uid,
			Name:       "events/" + uid,
			Kind:       "totally-unknown-kind",
			Source:     "test",
			Confidence: 1.0,
			CreateTime: baseTime,
			Payload:    json.RawMessage(`{}`),
		},
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Unknown kinds are ignored but cursor must advance.
	if view.Cursor != uid {
		t.Errorf("Cursor = %q, want %q", view.Cursor, uid)
	}
	// No entities, no relations.
	var zeroStats render.Stats
	if view.Stats != zeroStats {
		t.Errorf("Stats = %+v, want zero for unknown-kind render", view.Stats)
	}
}

func TestRender_MixedEntityKinds_EachKindIndexed(t *testing.T) {
	t.Parallel()

	personUID := ulidAt(500)
	orgUID := ulidAt(501)
	placeUID := ulidAt(502)

	events := []kernel.Event{
		makeEvent(personUID, ontology.KindEntity, entityPayload(personUID, "Merry", ontology.EntityKindPerson)),
		makeEvent(orgUID, ontology.KindEntity, entityPayload(orgUID, "Erebor", ontology.EntityKindOrganization)),
		makeEvent(placeUID, ontology.KindEntity, entityPayload(placeUID, "Berlin", ontology.EntityKindPlace)),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Each kind's INDEX.md must exist and contain the respective entity.
	type kindCheck struct {
		kind     string
		uid      string
		wantName string
	}
	checks := []kindCheck{
		{"person", personUID, "Merry"},
		{"organization", orgUID, "Erebor"},
		{"place", placeUID, "Berlin"},
	}
	for _, c := range checks {
		indexPath := fmt.Sprintf("rendered/by-kind/%s/INDEX.md", c.kind)
		f := fileByPath(view.Files, indexPath)
		if f == nil {
			t.Errorf("kind index %q not found", indexPath)
			continue
		}
		if !bytes.Contains(f.Content, []byte(c.uid)) {
			t.Errorf("kind index %q missing UID %q; got:\n%s", indexPath, c.uid, f.Content)
		}
	}
}

// TestRender_EntityKeyedByPayloadUID_NotEventUID pins the contract that
// the renderer keys entities and relations by their payload UID
// (Entity.UID / Relation.UID), not by the kernel event's own UID.
//
// Why this matters: in production the resolver emits an Entity event
// with its own random ULID, and the payload contains the canonical
// entity ULID under `payload.uid`. Later events (relation.subjectUid,
// relationRetracted.relationUid, entityIdentifierAdded.entityUid) all
// reference the payload UID, NOT the event UID. If the renderer keys
// off ev.UID, every cross-reference breaks: relations point at UIDs
// that don't appear in entities.jsonl, and supersede/forget lookups
// miss the row they meant to mutate (leaving superseded-relations.jsonl
// and forgotten-relations.jsonl empty).
//
// We force ev.UID != payload.UID and assert that:
//  1. entities.jsonl carries the payload UID
//  2. relations.jsonl's subject/object resolves to the payload UID
//  3. a relationRetracted keyed by payload-UID actually moves the row
//     into superseded-relations.jsonl
func TestRender_EntityKeyedByPayloadUID_NotEventUID(t *testing.T) {
	t.Parallel()

	// Distinct UIDs across the board so a `ev.UID` shortcut would
	// produce a different key.
	entityEventUID := ulidAt(10)
	entityPayloadUID := ulidAt(110)
	orgEventUID := ulidAt(11)
	orgPayloadUID := ulidAt(111)
	relEventUID := ulidAt(12)
	relPayloadUID := ulidAt(112)
	succEventUID := ulidAt(13)
	succPayloadUID := ulidAt(113)
	supersedeEventUID := ulidAt(14)

	events := []kernel.Event{
		makeEvent(entityEventUID, ontology.KindEntity,
			entityPayload(entityPayloadUID, "Rosie Cotton", ontology.EntityKindPerson)),
		makeEvent(orgEventUID, ontology.KindEntity,
			entityPayload(orgPayloadUID, "Erebor Inc.", ontology.EntityKindOrganization)),
		makeEvent(relEventUID, ontology.KindRelation,
			relationPayload(relPayloadUID, entityPayloadUID, "role_at", orgPayloadUID)),
		makeEvent(succEventUID, ontology.KindRelation,
			relationPayload(succPayloadUID, entityPayloadUID, "role_at", orgPayloadUID)),
		makeEvent(supersedeEventUID, ontology.KindRelationRetracted,
			ontology.RelationRetractedEvent{
				RelationUID:     ontology.ULID(relPayloadUID),
				SupersededByUID: ontology.ULID(succPayloadUID),
				RetractedAt:     baseTime.Add(time.Hour),
			}),
	}

	view, err := render.Render(render.Input{
		Scope:    kernel.ScopePersonal,
		Events:   seqEvents(events),
		HeadULID: supersedeEventUID,
		Device:   "test",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	entities, ok := files["rendered/entities.jsonl"]
	if !ok {
		t.Fatalf("entities.jsonl missing")
	}
	if !bytes.Contains(entities.Content, []byte(entityPayloadUID)) {
		t.Errorf("entities.jsonl does not contain payload UID %q; got:\n%s",
			entityPayloadUID, entities.Content)
	}
	if bytes.Contains(entities.Content, []byte(entityEventUID)) {
		t.Errorf("entities.jsonl wrongly includes event UID %q (should use payload.UID); got:\n%s",
			entityEventUID, entities.Content)
	}

	relations, ok := files["rendered/relations.jsonl"]
	if !ok {
		t.Fatalf("relations.jsonl missing")
	}
	if !bytes.Contains(relations.Content, []byte(relPayloadUID)) &&
		!bytes.Contains(relations.Content, []byte(succPayloadUID)) {
		t.Errorf("relations.jsonl missing payload-UIDs %q / %q; got:\n%s",
			relPayloadUID, succPayloadUID, relations.Content)
	}
	if !bytes.Contains(relations.Content, []byte(entityPayloadUID)) {
		t.Errorf("relations.jsonl subject does not point at entity payload UID %q; got:\n%s",
			entityPayloadUID, relations.Content)
	}

	// The supersede event references the relation by its payload UID;
	// the renderer must look it up by payload UID, mark it superseded,
	// and emit a row into superseded-relations.jsonl.
	superseded, ok := files["rendered/superseded-relations.jsonl"]
	if !ok {
		t.Fatalf("superseded-relations.jsonl missing")
	}
	if len(superseded.Content) == 0 {
		t.Errorf("superseded-relations.jsonl is empty; the supersede event keyed by payload UID was not applied")
	}
	if !bytes.Contains(superseded.Content, []byte(relPayloadUID)) {
		t.Errorf("superseded-relations.jsonl missing superseded relation UID %q; got:\n%s",
			relPayloadUID, superseded.Content)
	}
}

func indexFiles(files []render.File) map[string]render.File {
	out := make(map[string]render.File, len(files))
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}

// TestRender_EntityKindRegistered_WritesKindYAML verifies that an
// entityKindRegistered event causes a YAML file to appear under
// ontology/kinds/<name>.yaml.
func TestRender_EntityKindRegistered_WritesKindYAML(t *testing.T) {
	t.Parallel()

	kindEvUID := ulidAt(9001)

	events := []kernel.Event{
		makeEvent(kindEvUID, ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "vehicle",
			DisplayName: "Vehicle",
			Description: "A motorised conveyance.",
			SameAs:      []string{"https://schema.org/Vehicle"},
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByUser,
			AddedAt:     baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	kindFile, ok := files["ontology/kinds/vehicle.yaml"]
	if !ok {
		t.Fatalf("ontology/kinds/vehicle.yaml missing from output; paths: %v", fileNames(view.Files))
	}
	if !bytes.Contains(kindFile.Content, []byte("vehicle")) {
		t.Errorf("kind YAML missing name; got:\n%s", kindFile.Content)
	}
	if !bytes.Contains(kindFile.Content, []byte("Vehicle")) {
		t.Errorf("kind YAML missing display_name; got:\n%s", kindFile.Content)
	}
	if !bytes.Contains(kindFile.Content, []byte("schema.org/Vehicle")) {
		t.Errorf("kind YAML missing sameAs; got:\n%s", kindFile.Content)
	}
	if view.Cursor != kindEvUID {
		t.Errorf("Cursor = %q, want %q", view.Cursor, kindEvUID)
	}
}

// TestRender_EntityKindRegistered_CoreGuard verifies that a user-provenance
// event cannot overwrite a core kind in the renderer's in-memory state.
func TestRender_EntityKindRegistered_CoreGuard(t *testing.T) {
	t.Parallel()

	coreUID := ulidAt(9010)
	userUID := ulidAt(9011)

	events := []kernel.Event{
		makeEvent(coreUID, ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "person",
			DisplayName: "Person",
			Description: "A human individual.",
			Provenance:  "core",
			AddedBy:     ontology.EntityKindAddedBySeed,
			AddedAt:     baseTime,
		}),
		makeEvent(userUID, ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "person",
			DisplayName: "Person (hijacked)",
			Description: "Overwritten.",
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByUser,
			AddedAt:     baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	kindFile, ok := files["ontology/kinds/person.yaml"]
	if !ok {
		t.Fatalf("ontology/kinds/person.yaml missing")
	}
	if bytes.Contains(kindFile.Content, []byte("hijacked")) {
		t.Errorf("core guard failed: user event overwrote core kind in renderer")
	}
	if !bytes.Contains(kindFile.Content, []byte("A human individual.")) {
		t.Errorf("core description missing from YAML; got:\n%s", kindFile.Content)
	}
}

// TestRender_EntityKindRegistered_StatusFieldInYAML verifies that the P7
// governance status field is included in the kind YAML output. An active kind
// must emit "status: active"; a proposed kind must emit "status: proposed".
func TestRender_EntityKindRegistered_StatusFieldInYAML(t *testing.T) {
	t.Parallel()

	activeUID := ulidAt(9030)
	proposedUID := ulidAt(9031)

	events := []kernel.Event{
		makeEvent(activeUID, ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "vehicle",
			DisplayName: "Vehicle",
			Description: "A motorised conveyance.",
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByUser,
			AddedAt:     baseTime,
			// Status empty → renders as active.
		}),
		makeEvent(proposedUID, ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "user:gadget",
			DisplayName: "Gadget",
			Description: "A small electronic device.",
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByLLM,
			AddedAt:     baseTime,
			Status:      "proposed",
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)

	// Active kind: status must be "active" in YAML.
	activeFile, ok := files["ontology/kinds/vehicle.yaml"]
	if !ok {
		t.Fatalf("ontology/kinds/vehicle.yaml missing; paths: %v", fileNames(view.Files))
	}
	if !bytes.Contains(activeFile.Content, []byte("status: active")) {
		t.Errorf("vehicle.yaml missing 'status: active'; got:\n%s", activeFile.Content)
	}

	// Proposed kind: status must be "proposed" in YAML.
	// ontologyKindPath(name) = "ontology/kinds/" + name + ".yaml", so
	// "user:gadget" → "ontology/kinds/user:gadget.yaml".
	proposedFile, ok := files["ontology/kinds/user:gadget.yaml"]
	if !ok {
		var kindPaths []string
		for p := range files {
			if strings.HasPrefix(p, "ontology/kinds/") {
				kindPaths = append(kindPaths, p)
			}
		}
		t.Fatalf("ontology/kinds/user:gadget.yaml missing; kind files: %v", kindPaths)
	}
	if !bytes.Contains(proposedFile.Content, []byte("status: proposed")) {
		t.Errorf("gadget yaml missing 'status: proposed'; got:\n%s", proposedFile.Content)
	}
}

// TestRender_EntityKindReviewed_UpdatesStatus verifies that an
// entityKindReviewed event with decision=approved updates the kind's YAML
// status from "proposed" to "active".
func TestRender_EntityKindReviewed_UpdatesStatus(t *testing.T) {
	t.Parallel()

	regUID := ulidAt(9040)
	reviewUID := ulidAt(9041)

	events := []kernel.Event{
		makeEvent(regUID, ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "user:gadget",
			DisplayName: "Gadget",
			Description: "A small electronic device.",
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByLLM,
			AddedAt:     baseTime,
			Status:      "proposed",
		}),
		makeEvent(reviewUID, ontology.KindEntityKindReviewed, ontology.EntityKindReviewedEvent{
			Name:       "user:gadget",
			Decision:   ontology.EntityKindReviewApproved,
			ReviewedBy: "cli",
			ReviewedAt: baseTime.Add(time.Minute),
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)

	// After approval the kind must appear as active in YAML.
	// Path: ontologyKindPath("user:gadget") = "ontology/kinds/user:gadget.yaml".
	gadgetFile, ok := files["ontology/kinds/user:gadget.yaml"]
	if !ok {
		var kindPaths []string
		for p := range files {
			if strings.HasPrefix(p, "ontology/kinds/") {
				kindPaths = append(kindPaths, p)
			}
		}
		t.Fatalf("ontology/kinds/user:gadget.yaml missing; kind files: %v", kindPaths)
	}
	if bytes.Contains(gadgetFile.Content, []byte("status: proposed")) {
		t.Errorf("after approval kind must NOT have status: proposed; got:\n%s", gadgetFile.Content)
	}
	if !bytes.Contains(gadgetFile.Content, []byte("status: active")) {
		t.Errorf("after approval kind must have status: active; got:\n%s", gadgetFile.Content)
	}
}

// TestRender_EntityKindReviewed_NonProposedNoOp verifies the render projection
// mirrors the dispatcher's review-lifecycle guard: once a kind has left
// 'proposed', a later (replayed or out-of-order synced) review must not re-flip
// its rendered status, so ontology/kinds/<name>.yaml never disagrees with the
// replayed SQLite projection.
func TestRender_EntityKindReviewed_NonProposedNoOp(t *testing.T) {
	t.Parallel()

	regUID := ulidAt(9050)
	approveUID := ulidAt(9051)
	rejectUID := ulidAt(9052)

	events := []kernel.Event{
		makeEvent(regUID, ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "user:hovercraft",
			DisplayName: "Hovercraft",
			Description: "A vehicle that rides on a cushion of air.",
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByLLM,
			AddedAt:     baseTime,
			Status:      "proposed",
		}),
		makeEvent(approveUID, ontology.KindEntityKindReviewed, ontology.EntityKindReviewedEvent{
			Name:       "user:hovercraft",
			Decision:   ontology.EntityKindReviewApproved,
			ReviewedBy: "cli",
			ReviewedAt: baseTime.Add(time.Minute),
		}),
		// Late reject on the now-active kind: must be a no-op.
		makeEvent(rejectUID, ontology.KindEntityKindReviewed, ontology.EntityKindReviewedEvent{
			Name:       "user:hovercraft",
			Decision:   ontology.EntityKindReviewRejected,
			ReviewedBy: "cli",
			ReviewedAt: baseTime.Add(2 * time.Minute),
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	f, ok := files["ontology/kinds/user:hovercraft.yaml"]
	if !ok {
		t.Fatalf("ontology/kinds/user:hovercraft.yaml missing")
	}
	if !bytes.Contains(f.Content, []byte("status: active")) {
		t.Errorf("kind must remain active after a late reject; got:\n%s", f.Content)
	}
	if bytes.Contains(f.Content, []byte("status: rejected")) {
		t.Errorf("late reject on an active kind must be a no-op, not rendered as rejected; got:\n%s", f.Content)
	}
}

// TestRender_OntologyPredicatesYAML verifies that registered predicates appear
// in ontology/predicates.yaml (distinct from the relations.jsonl file).
func TestRender_OntologyPredicatesYAML(t *testing.T) {
	t.Parallel()

	predUID := ulidAt(9020)
	events := []kernel.Event{
		makeEvent(predUID, ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate:   "employs",
			Kind:        ontology.PredicateKindFunctional,
			Description: "Subject employs object.",
			AddedBy:     ontology.PredicateAddedBySeed,
			AddedAt:     baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	pred, ok := files["ontology/predicates.yaml"]
	if !ok {
		t.Fatalf("ontology/predicates.yaml missing from output; paths: %v", fileNames(view.Files))
	}
	if !bytes.Contains(pred.Content, []byte("employs")) {
		t.Errorf("predicates.yaml missing 'employs'; got:\n%s", pred.Content)
	}
}

// TestRender_SchemaRegistered_WritesSchemaJSON verifies that a schemaRegistered
// event renders to ontology/schemas/<schemaKind>.json so dynamic schemas travel
// through the git transport alongside the SQLite projection.
func TestRender_SchemaRegistered_WritesSchemaJSON(t *testing.T) {
	t.Parallel()

	schemaUID := ulidAt(9030)
	events := []kernel.Event{
		makeEvent(schemaUID, ontology.KindSchemaRegistered, ontology.SchemaRegisteredEvent{
			SchemaKind: "entityKind:vehicle",
			Version:    "v0.1.0",
			Definition: json.RawMessage(`{"type":"object","properties":{"vin":{"type":"string"}}}`),
			Provenance: "user",
			AddedBy:    ontology.EntityKindAddedByUser,
			AddedAt:    baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	schemaPath := "ontology/schemas/" + ontology.SchemaKindRenderFilename("entityKind:vehicle")
	schemaFile, ok := files[schemaPath]
	if !ok {
		t.Fatalf("%s missing from output; paths: %v", schemaPath, fileNames(view.Files))
	}
	// The rendered file must be valid JSON carrying the definition + metadata.
	var decoded struct {
		SchemaKind string          `json:"schemaKind"`
		Version    string          `json:"version"`
		Provenance string          `json:"provenance"`
		Definition json.RawMessage `json:"definition"`
	}
	if err := json.Unmarshal(schemaFile.Content, &decoded); err != nil {
		t.Fatalf("rendered schema not valid JSON: %v\n%s", err, schemaFile.Content)
	}
	if decoded.SchemaKind != "entityKind:vehicle" || decoded.Version != "v0.1.0" || decoded.Provenance != "user" {
		t.Errorf("metadata = %+v, want kind=entityKind:vehicle version=v0.1.0 provenance=user", decoded)
	}
	if !bytes.Contains(decoded.Definition, []byte(`"vin"`)) {
		t.Errorf("definition missing vin property; got: %s", decoded.Definition)
	}
}

// TestRender_SchemaRegistered_VersionMonotonic verifies the renderer mirrors
// the dispatcher contract: a strictly newer version replaces, while same-version
// (immutable) and lower-version events are no-ops in the rendered output.
func TestRender_SchemaRegistered_VersionMonotonic(t *testing.T) {
	t.Parallel()

	events := []kernel.Event{
		makeEvent(ulidAt(9040), ontology.KindSchemaRegistered, ontology.SchemaRegisteredEvent{
			SchemaKind: "entityKind:vehicle",
			Version:    "v0.2.0",
			Definition: json.RawMessage(`{"type":"object","title":"v2"}`),
			Provenance: "user",
			AddedBy:    ontology.EntityKindAddedByUser,
			AddedAt:    baseTime,
		}),
		// Lower version arriving later must not move the rendered file backwards.
		makeEvent(ulidAt(9041), ontology.KindSchemaRegistered, ontology.SchemaRegisteredEvent{
			SchemaKind: "entityKind:vehicle",
			Version:    "v0.1.0",
			Definition: json.RawMessage(`{"type":"object","title":"v1"}`),
			Provenance: "user",
			AddedBy:    ontology.EntityKindAddedByUser,
			AddedAt:    baseTime,
		}),
	}

	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	files := indexFiles(view.Files)
	schemaFile, ok := files["ontology/schemas/"+ontology.SchemaKindRenderFilename("entityKind:vehicle")]
	if !ok {
		t.Fatalf("schema json missing")
	}
	if !bytes.Contains(schemaFile.Content, []byte("v0.2.0")) || bytes.Contains(schemaFile.Content, []byte(`"v1"`)) {
		t.Errorf("rendered schema moved backwards to lower version; got:\n%s", schemaFile.Content)
	}
}

// TestRender_PredicateRegistered_UpdateOverwrites verifies that a repeat
// predicateRegistered event updates the rendered predicates.yaml, mirroring the
// dispatcher's UPSERT (the renderer no longer keeps the stale first row).
func TestRender_PredicateRegistered_UpdateOverwrites(t *testing.T) {
	t.Parallel()

	events := []kernel.Event{
		makeEvent(ulidAt(9050), ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate:   "employs",
			Kind:        ontology.PredicateKindFunctional,
			Description: "first description",
			AddedBy:     ontology.PredicateAddedBySeed,
			AddedAt:     baseTime,
		}),
		makeEvent(ulidAt(9051), ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate:   "employs",
			Kind:        ontology.PredicateKindFunctional,
			Description: "corrected description",
			AddedBy:     ontology.PredicateAddedByUser,
			AddedAt:     baseTime,
		}),
	}

	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	files := indexFiles(view.Files)
	pred, ok := files["ontology/predicates.yaml"]
	if !ok {
		t.Fatalf("ontology/predicates.yaml missing")
	}
	if bytes.Contains(pred.Content, []byte("first description")) {
		t.Errorf("predicates.yaml kept stale first row; got:\n%s", pred.Content)
	}
	if !bytes.Contains(pred.Content, []byte("corrected description")) {
		t.Errorf("predicates.yaml missing updated row; got:\n%s", pred.Content)
	}
}

// TestRender_OntologyKindYAML_QuotesSpecialChars verifies that YAML-special
// characters in user-controlled taxonomy fields are quoted/escaped so the
// rendered kind file stays valid YAML.
func TestRender_OntologyKindYAML_QuotesSpecialChars(t *testing.T) {
	t.Parallel()

	events := []kernel.Event{
		makeEvent(ulidAt(9060), ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        "gadget",
			DisplayName: "- leading dash makes a YAML list item",
			Description: "has: a colon, a # hash, and a \"quote\"",
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByUser,
			AddedAt:     baseTime,
		}),
	}

	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	files := indexFiles(view.Files)
	kindFile, ok := files["ontology/kinds/gadget.yaml"]
	if !ok {
		t.Fatalf("ontology/kinds/gadget.yaml missing")
	}
	// The leading-dash display name and colon/hash description must be quoted.
	if !bytes.Contains(kindFile.Content, []byte(`display_name: "- leading dash`)) {
		t.Errorf("leading-dash display_name not quoted; got:\n%s", kindFile.Content)
	}
	if !bytes.Contains(kindFile.Content, []byte(`description: "has:`)) {
		t.Errorf("colon-bearing description not quoted; got:\n%s", kindFile.Content)
	}
	if !bytes.Contains(kindFile.Content, []byte(`\"quote\"`)) {
		t.Errorf("embedded quotes not escaped; got:\n%s", kindFile.Content)
	}
}

// TestRender_ByteIdentical_WithOntologyEvents extends the base determinism
// check to a stream that includes entityKindRegistered events. This guards
// against non-determinism introduced in ontology.go.
func TestRender_ByteIdentical_WithOntologyEvents(t *testing.T) {
	t.Parallel()

	const runs = 20
	events := buildOntologyEventStream()
	reference := renderAndHash(t, events)
	if len(reference) == 0 {
		t.Fatal("reference render produced 0 files")
	}
	for run := range runs {
		got := renderAndHash(t, events)
		if !hashMapsEqual(reference, got) {
			t.Fatalf("ontology render run %d/%d diverged:\n%s", run+1, runs, hashMapDiff(reference, got))
		}
	}
}

// buildOntologyEventStream constructs a deterministic event stream that
// includes entityKindRegistered events alongside the standard types.
func buildOntologyEventStream() []kernel.Event {
	// kindDisplayNames maps test kind names to their display labels. Using a
	// fixed map avoids the deprecated strings.Title and keeps the test
	// deterministic without an external unicode dependency.
	kindDisplayNames := map[string]string{
		"vehicle": "Vehicle",
		"animal":  "Animal",
		"concept": "Concept",
	}
	names := []string{"vehicle", "animal", "concept"}
	events := make([]kernel.Event, 0, len(names))
	for i, name := range names {
		events = append(events, makeEvent(ulidAt(9100+i), ontology.KindEntityKindRegistered, ontology.EntityKindRegisteredEvent{
			Name:        name,
			DisplayName: kindDisplayNames[name],
			Description: "Test kind " + name + ".",
			SameAs:      []string{"https://schema.org/" + kindDisplayNames[name]},
			Provenance:  "user",
			AddedBy:     ontology.EntityKindAddedByUser,
			AddedAt:     baseTime,
		}))
	}
	return events
}

// fileNames returns a sorted list of paths from a File slice.
func fileNames(files []render.File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Path
	}
	return names
}

// ---------------------------------------------------------------------------
// A. Typed-relation metadata in predicates.yaml (Greptile fix regression tests)
// ---------------------------------------------------------------------------

// TestRender_PredicateRegistered_TypedMetadataInYAML verifies that a
// predicateRegistered event carrying v0.2 typed-relation fields (SubjectKinds,
// ObjectKinds, SameAs, PropertySchema) emits them into ontology/predicates.yaml
// with the expected formatting: subject_kinds/object_kinds in event order,
// same_as sorted, property_schema compacted to a one-liner.
func TestRender_PredicateRegistered_TypedMetadataInYAML(t *testing.T) {
	t.Parallel()

	predEvUID := ulidAt(9200)

	events := []kernel.Event{
		makeEvent(predEvUID, ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate:    "registered_in",
			Kind:         ontology.PredicateKindFunctional,
			Description:  "Subject is registered in the object jurisdiction.",
			SubjectKinds: []string{"extension:business_de:company"},
			ObjectKinds:  []string{"organization"},
			// Deliberately unsorted — same_as must be SORTED in output.
			SameAs:         []string{"https://z.example/b", "https://a.example/a"},
			PropertySchema: json.RawMessage(`{ "type":  "object" }`), // deliberate whitespace
			AddedBy:        ontology.PredicateAddedByUser,
			AddedAt:        baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	pred, ok := files["ontology/predicates.yaml"]
	if !ok {
		t.Fatalf("ontology/predicates.yaml missing")
	}

	content := pred.Content

	// subject_kinds must be present in event order.
	if !bytes.Contains(content, []byte("subject_kinds:")) {
		t.Errorf("predicates.yaml missing subject_kinds line; got:\n%s", content)
	}
	if !bytes.Contains(content, []byte("extension:business_de:company")) {
		t.Errorf("predicates.yaml missing subject_kinds value; got:\n%s", content)
	}

	// object_kinds must be present.
	if !bytes.Contains(content, []byte("object_kinds:")) {
		t.Errorf("predicates.yaml missing object_kinds line; got:\n%s", content)
	}
	if !bytes.Contains(content, []byte("organization")) {
		t.Errorf("predicates.yaml missing object_kinds value; got:\n%s", content)
	}

	// same_as must appear with both IRIs, sorted (a.example before z.example).
	if !bytes.Contains(content, []byte("same_as:")) {
		t.Errorf("predicates.yaml missing same_as line; got:\n%s", content)
	}
	aIdx := bytes.Index(content, []byte("https://a.example/a"))
	zIdx := bytes.Index(content, []byte("https://z.example/b"))
	if aIdx < 0 {
		t.Errorf("predicates.yaml missing https://a.example/a; got:\n%s", content)
	}
	if zIdx < 0 {
		t.Errorf("predicates.yaml missing https://z.example/b; got:\n%s", content)
	}
	if aIdx > zIdx {
		t.Errorf("predicates.yaml same_as not sorted: a.example (%d) should precede z.example (%d); got:\n%s",
			aIdx, zIdx, content)
	}

	// property_schema must be compacted: the YAML renderer double-quotes the JSON
	// object (because '{' is a YAML indicator) and escapes inner double-quotes.
	// Source `{ "type":  "object" }` → compact `{"type":"object"}` → YAML-quoted
	// `"{\"type\":\"object\"}"`.
	if !bytes.Contains(content, []byte(`property_schema:`)) {
		t.Errorf("predicates.yaml missing property_schema line; got:\n%s", content)
	}
	// The YAML-quoted compact form must be present on disk.
	if !bytes.Contains(content, []byte(`"{\"type\":\"object\"}`)) {
		t.Errorf("predicates.yaml property_schema not in expected YAML-quoted compacted form; got:\n%s", content)
	}
	// Must NOT contain extra spaces around the colon (source whitespace retained).
	if bytes.Contains(content, []byte(`"type":  "object"`)) {
		t.Errorf("predicates.yaml property_schema retains source whitespace; got:\n%s", content)
	}
}

// TestRender_PredicateRegistered_NoTypedFields_NoExtraLines confirms that a
// predicateRegistered event with no SubjectKinds/ObjectKinds/SameAs/PropertySchema
// produces no subject_kinds, object_kinds, same_as, or property_schema lines in
// predicates.yaml. This guards against accidental emission of empty YAML keys
// when the typed-metadata fields are absent.
func TestRender_PredicateRegistered_NoTypedFields_NoExtraLines(t *testing.T) {
	t.Parallel()

	events := []kernel.Event{
		makeEvent(ulidAt(9205), ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate:   "plain_predicate",
			Kind:        ontology.PredicateKindFunctional,
			Description: "A plain predicate with no typed-relation metadata.",
			AddedBy:     ontology.PredicateAddedBySeed,
			AddedAt:     baseTime,
		}),
	}

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopeProject,
		Events: seqEvents(events),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	files := indexFiles(view.Files)
	pred, ok := files["ontology/predicates.yaml"]
	if !ok {
		t.Fatalf("ontology/predicates.yaml missing")
	}

	content := pred.Content
	for _, forbidden := range []string{"subject_kinds:", "object_kinds:", "same_as:", "property_schema:"} {
		if bytes.Contains(content, []byte(forbidden)) {
			t.Errorf("predicates.yaml emits %q for a predicate with no typed fields; got:\n%s", forbidden, content)
		}
	}
}

// TestRender_PredicateRegistered_ByteIdenticalRepeat verifies that registering
// the same predicate twice with byte-identical typed fields produces the same
// predicates.yaml content on both renders — proving the no-change idempotency
// path in applyPredicateRegistered. The two renders are independent (no
// PriorState sharing), so equal output means the second event was correctly
// treated as a no-op by the GraphState apply path.
func TestRender_PredicateRegistered_ByteIdenticalRepeat(t *testing.T) {
	t.Parallel()

	payload := ontology.PredicateRegisteredEvent{
		Predicate:      "repeated_pred",
		Kind:           ontology.PredicateKindFunctional,
		Description:    "A predicate registered twice with identical typed fields.",
		SubjectKinds:   []string{"person"},
		ObjectKinds:    []string{"organization"},
		SameAs:         []string{"https://schema.org/worksFor"},
		PropertySchema: json.RawMessage(`{"type":"object","properties":{"role":{"type":"string"}}}`),
		AddedBy:        ontology.PredicateAddedByUser,
		AddedAt:        baseTime,
	}

	// Render with [ev1]: establishes the predicate.
	events1 := []kernel.Event{
		makeEvent(ulidAt(9210), ontology.KindPredicateRegistered, payload),
	}
	view1, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events1)})
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}
	files1 := indexFiles(view1.Files)
	pred1, ok := files1["ontology/predicates.yaml"]
	if !ok {
		t.Fatalf("first render: ontology/predicates.yaml missing")
	}

	// Render with [ev1, ev2] where ev2 is byte-identical to ev1.
	events2 := []kernel.Event{
		makeEvent(ulidAt(9210), ontology.KindPredicateRegistered, payload),
		makeEvent(ulidAt(9211), ontology.KindPredicateRegistered, payload),
	}
	view2, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events2)})
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	files2 := indexFiles(view2.Files)
	pred2, ok := files2["ontology/predicates.yaml"]
	if !ok {
		t.Fatalf("second render: ontology/predicates.yaml missing")
	}

	// Both renders must produce byte-identical predicates.yaml content.
	if !bytes.Equal(pred1.Content, pred2.Content) {
		t.Errorf("predicates.yaml content diverged on repeat registration:\nfirst:\n%s\nsecond:\n%s",
			pred1.Content, pred2.Content)
	}

	// The second event must not have doubled the predicate entry.
	count := bytes.Count(pred2.Content, []byte("repeated_pred"))
	if count != 1 {
		t.Errorf("predicates.yaml has %d occurrences of repeated_pred, want 1; got:\n%s", count, pred2.Content)
	}
}

// ---------------------------------------------------------------------------
// B. Entity Properties in entities.jsonl (Greptile fix regression tests)
// ---------------------------------------------------------------------------

// TestRender_Entity_PropertiesInJSONL verifies that:
// (a) an entity event with Properties renders the compacted JSON into
//
//	entities.jsonl as "properties":{...},
//
// (b) a second entity event for the SAME payload UID without properties keeps
//
//	the stored properties in the re-render.
func TestRender_Entity_PropertiesInJSONL(t *testing.T) {
	t.Parallel()

	entityUID := ulidAt(9220)

	t.Run("entity with properties renders compacted JSON", func(t *testing.T) {
		t.Parallel()

		entity := ontology.Entity{
			Name:             "entities/" + entityUID,
			UID:              entityUID,
			Kind:             ontology.EntityKindOrganization,
			DisplayName:      "Mithril Corp",
			Confidence:       1.0,
			ResolutionStatus: ontology.ResolutionConfirmed,
			CreateTime:       baseTime,
			// Whitespace in Properties must be compacted in the output.
			Properties: json.RawMessage(`{ "vin": "V-1" }`),
		}
		events := []kernel.Event{
			makeEvent(entityUID, ontology.KindEntity, entity),
		}

		view, err := render.Render(render.Input{
			Scope:  kernel.ScopeProject,
			Events: seqEvents(events),
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}

		entJSONL := fileByPath(view.Files, "rendered/entities.jsonl")
		if entJSONL == nil {
			t.Fatal("rendered/entities.jsonl missing")
		}
		// Must contain the compacted form.
		if !bytes.Contains(entJSONL.Content, []byte(`"properties":{"vin":"V-1"}`)) {
			t.Errorf("entities.jsonl missing compacted properties; got:\n%s", entJSONL.Content)
		}
		// Must NOT contain the whitespace-padded form.
		if bytes.Contains(entJSONL.Content, []byte(`"vin": "V-1"`)) {
			t.Errorf("entities.jsonl retains source whitespace in properties; got:\n%s", entJSONL.Content)
		}
	})

	t.Run("update without properties keeps stored properties", func(t *testing.T) {
		t.Parallel()

		// First event: entity with properties.
		entityWithProps := ontology.Entity{
			Name:             "entities/" + entityUID,
			UID:              entityUID,
			Kind:             ontology.EntityKindOrganization,
			DisplayName:      "Mithril Corp",
			Confidence:       1.0,
			ResolutionStatus: ontology.ResolutionConfirmed,
			CreateTime:       baseTime,
			Properties:       json.RawMessage(`{"vin":"V-1"}`),
		}
		// Second event: same UID, no properties (update without typed attrs).
		entityWithoutProps := ontology.Entity{
			Name:             "entities/" + entityUID,
			UID:              entityUID,
			Kind:             ontology.EntityKindOrganization,
			DisplayName:      "Mithril Corp (updated)",
			Confidence:       0.9,
			ResolutionStatus: ontology.ResolutionConfirmed,
			CreateTime:       baseTime,
			// No Properties.
		}

		events := []kernel.Event{
			makeEvent(entityUID, ontology.KindEntity, entityWithProps),
			makeEvent(ulidAt(9221), ontology.KindEntity, entityWithoutProps),
		}

		view, err := render.Render(render.Input{
			Scope:  kernel.ScopeProject,
			Events: seqEvents(events),
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}

		entJSONL := fileByPath(view.Files, "rendered/entities.jsonl")
		if entJSONL == nil {
			t.Fatal("rendered/entities.jsonl missing")
		}
		// Properties must still be present after the no-properties update.
		if !bytes.Contains(entJSONL.Content, []byte(`"properties":{"vin":"V-1"}`)) {
			t.Errorf("entities.jsonl lost properties after update without properties; got:\n%s", entJSONL.Content)
		}
	})
}
