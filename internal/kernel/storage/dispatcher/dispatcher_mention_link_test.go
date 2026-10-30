package dispatcher_test

// dispatcher_mention_link_test.go pins the contract added in migration 0008:
// applyEntity stamps entity.UID onto every mention in entity.MentionUIDs, so
// recall can JOIN mentions to entities on entity_uid instead of display_name.
//
// The regression this guards is homonym collapse: two distinct people both
// named "Frodo" would previously share a single join key and recall could
// not tell them apart. The test writes two such entities and asserts the
// query that hybrid recall uses returns each entity through its own mention
// chain, never crossing the wires.

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func TestApplyEntity_BackfillsMentionEntityUID(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	episodeUID := "01HMLINK000000000000EPISODE"
	if err := d.Apply(ctx, []kernel.Event{makeEpisodeEvent(t, episodeUID, "Frodo works at Erebor.", "test")}); err != nil {
		t.Fatalf("Apply episode: %v", err)
	}

	mentionUID := "01HMLINK00000000000MENTION1"
	mention := ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Frodo",
		SourceEventUID: ontology.ULID(episodeUID),
		ExtractionText: "Frodo",
		SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 7},
		GroundingScore: 1.0,
	}
	if err := d.Apply(ctx, []kernel.Event{makeMentionEvent(t, mentionUID, mention)}); err != nil {
		t.Fatalf("Apply mention: %v", err)
	}

	// Before the Entity event lands, entity_uid is NULL.
	if got := readMentionEntityUID(t, ctx, d, mentionUID); got != "" {
		t.Errorf("entity_uid before EntityCreated = %q, want empty", got)
	}

	entityUID := "01HMLINK000000000000ENTITY1"
	entity := ontology.Entity{
		UID:              entityUID,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		MentionUIDs:      []ontology.ULID{ontology.ULID(mentionUID)},
	}
	if err := d.Apply(ctx, []kernel.Event{makeEntityEvent(t, entityUID, entity)}); err != nil {
		t.Fatalf("Apply entity: %v", err)
	}

	if got := readMentionEntityUID(t, ctx, d, mentionUID); got != entityUID {
		t.Errorf("entity_uid after EntityCreated = %q, want %q", got, entityUID)
	}
}

// TestApplyEntity_HomonymsDoNotCollapse is the regression test for the
// display_name-join bug. Two entities share the surface form "Frodo" but
// each has its own mention chain. The hybrid-recall lookup must return each
// entity through its own mentions only.
func TestApplyEntity_HomonymsDoNotCollapse(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	episode1 := "01HMLINK000000000000EPISO1A"
	episode2 := "01HMLINK000000000000EPISO2A"
	if err := d.Apply(ctx, []kernel.Event{
		makeEpisodeEvent(t, episode1, "Frodo Baggins works at Erebor.", "test"),
		makeEpisodeEvent(t, episode2, "Frodo Gardner joined Rivendell.", "test"),
	}); err != nil {
		t.Fatalf("Apply episodes: %v", err)
	}

	mention1 := ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Frodo",
		SourceEventUID: ontology.ULID(episode1),
		ExtractionText: "Frodo",
		SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 7},
		GroundingScore: 1.0,
	}
	mention2 := ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Frodo",
		SourceEventUID: ontology.ULID(episode2),
		ExtractionText: "Frodo",
		SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 7},
		GroundingScore: 1.0,
	}
	mUID1 := "01HMLINK00000000000MENTI1A"
	mUID2 := "01HMLINK00000000000MENTI2A"
	if err := d.Apply(ctx, []kernel.Event{
		makeMentionEvent(t, mUID1, mention1),
		makeMentionEvent(t, mUID2, mention2),
	}); err != nil {
		t.Fatalf("Apply mentions: %v", err)
	}

	eUID1 := "01HMLINK000000000000ENT1AA"
	eUID2 := "01HMLINK000000000000ENT2AA"
	entity1 := ontology.Entity{
		UID:              eUID1,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo Baggins",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		MentionUIDs:      []ontology.ULID{ontology.ULID(mUID1)},
	}
	entity2 := ontology.Entity{
		UID:              eUID2,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo Gardner",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		MentionUIDs:      []ontology.ULID{ontology.ULID(mUID2)},
	}
	if err := d.Apply(ctx, []kernel.Event{
		makeEntityEvent(t, eUID1, entity1),
		makeEntityEvent(t, eUID2, entity2),
	}); err != nil {
		t.Fatalf("Apply entities: %v", err)
	}

	if got := readMentionEntityUID(t, ctx, d, mUID1); got != eUID1 {
		t.Errorf("mention1 entity_uid = %q, want %q", got, eUID1)
	}
	if got := readMentionEntityUID(t, ctx, d, mUID2); got != eUID2 {
		t.Errorf("mention2 entity_uid = %q, want %q", got, eUID2)
	}

	// The hybrid-recall query: given episode1, only entity1 should come back.
	got := entityUIDsForEpisode(t, ctx, d, episode1)
	if len(got) != 1 || got[0] != eUID1 {
		t.Errorf("entities for episode1 = %v, want [%s]", got, eUID1)
	}
	got = entityUIDsForEpisode(t, ctx, d, episode2)
	if len(got) != 1 || got[0] != eUID2 {
		t.Errorf("entities for episode2 = %v, want [%s]", got, eUID2)
	}
}

func readMentionEntityUID(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, mentionUID string) string {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var got *string
	if err := db.QueryRowContext(ctx,
		`SELECT entity_uid FROM mentions WHERE uid = ?`, mentionUID,
	).Scan(&got); err != nil {
		t.Fatalf("scan entity_uid: %v", err)
	}
	if got == nil {
		return ""
	}
	return *got
}

func entityUIDsForEpisode(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, episodeUID string) []string {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT e.uid
		 FROM entities e
		 JOIN mentions m ON m.entity_uid = e.uid
		 WHERE m.source_event_uid = ?`, episodeUID,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}
