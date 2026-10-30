package dispatcher_test

// dispatcher_spatial_test.go exercises the coordinate-provenance ranking logic
// in the entity write path and the NearbyEntities geo scan against a real
// dispatcher-backed SQLite store.
//
// Acceptance criteria (P4):
//   - user_provided coordinates are never overwritten by geocoded:nominatim.
//   - geocoded:nominatim can overwrite a NULL provenance.
//   - calendar:google overwrites geocoded:nominatim but not user_provided.
//   - NearbyEntities returns only entities within the given radius.

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/geo"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// allScopesTestPredicate is the scope fragment these geo tests pass to
// NearbyEntities. They exercise the haversine/radius logic, not scope isolation,
// and their fixtures seed personal-scope entities, so an all-scopes predicate
// keeps every seeded entity in range (scope isolation is covered separately by
// TestNearbyEntities_OutOfScopeExcluded). NearbyEntities now requires a non-empty
// scope predicate (#206), so this is mandatory rather than optional.
const allScopesTestPredicate = "scope IN ('personal', 'project', 'team')"

// makeCoordEntity returns an ontology.Entity with coordinates and provenance.
func makeCoordEntity(uid, name string, lat, lon float64, prov string) ontology.Entity {
	return ontology.Entity{
		UID:                   uid,
		Kind:                  ontology.EntityKindPlace,
		DisplayName:           name,
		Confidence:            1.0,
		ResolutionStatus:      ontology.ResolutionConfirmed,
		CreateTime:            time.Unix(1_700_000_000, 0).UTC(),
		Coordinates:           &ontology.Coordinates{Latitude: lat, Longitude: lon},
		CoordinatesProvenance: prov,
	}
}

// applyEntity is a convenience wrapper that marshals an Entity into a
// kernel.Event and calls d.Apply.
func applyEntity(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string, entity ontology.Entity) {
	t.Helper()
	ev := makeEntityEvent(t, uid, entity)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply entity %s: %v", uid, err)
	}
}

// readEntityCoords reads (latitude, longitude, coordinates_provenance) for uid
// directly from the SQLite reader. Returns nil, "" when the entity has no
// coordinates.
func readEntityCoords(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string) (lat, lon *float64, prov string) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var la, lo *float64
	var pr *string
	if err := db.QueryRowContext(ctx,
		`SELECT latitude, longitude, coordinates_provenance FROM entities WHERE uid = ?`, uid,
	).Scan(&la, &lo, &pr); err != nil {
		t.Fatalf("scan coords uid=%s: %v", uid, err)
	}
	if pr != nil {
		prov = *pr
	}
	return la, lo, prov
}

// TestEntityCoords_UserProvidedNeverOverwritten verifies that a
// user_provided coordinate set is preserved when a subsequent EntityUpdated
// event carries geocoded:nominatim or calendar:google coordinates.
func TestEntityCoords_UserProvidedNeverOverwritten(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	uid := "01HSPATIAL000000000000001"

	// First event: user_provided coordinates.
	applyEntity(t, ctx, d, uid, makeCoordEntity(uid, "Berlin HQ", 52.52, 13.40, "user_provided"))

	// Second event: geocoded:nominatim tries to overwrite.
	applyEntity(t, ctx, d, uid, makeCoordEntity(uid, "Berlin HQ", 52.00, 13.00, "geocoded:nominatim"))

	lat, lon, prov := readEntityCoords(t, ctx, d, uid)
	if lat == nil || *lat != 52.52 {
		t.Errorf("latitude = %v, want 52.52 (user_provided must win)", lat)
	}
	if lon == nil || *lon != 13.40 {
		t.Errorf("longitude = %v, want 13.40 (user_provided must win)", lon)
	}
	if prov != "user_provided" {
		t.Errorf("provenance = %q, want %q", prov, "user_provided")
	}

	// Third event: calendar:google also must not overwrite user_provided.
	applyEntity(t, ctx, d, uid, makeCoordEntity(uid, "Berlin HQ", 52.50, 13.39, "calendar:google"))

	lat2, lon2, prov2 := readEntityCoords(t, ctx, d, uid)
	if lat2 == nil || *lat2 != 52.52 {
		t.Errorf("after calendar:google: latitude = %v, want 52.52", lat2)
	}
	_ = lon2
	if prov2 != "user_provided" {
		t.Errorf("after calendar:google: provenance = %q, want %q", prov2, "user_provided")
	}
}

// TestEntityCoords_CalendarOverwritesNominatim verifies that calendar:google
// (rank 2) overwrites geocoded:nominatim (rank 1).
func TestEntityCoords_CalendarOverwritesNominatim(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	uid := "01HSPATIAL000000000000002"

	applyEntity(t, ctx, d, uid, makeCoordEntity(uid, "Munich Office", 48.14, 11.58, "geocoded:nominatim"))
	applyEntity(t, ctx, d, uid, makeCoordEntity(uid, "Munich Office", 48.15, 11.59, "calendar:google"))

	lat, _, prov := readEntityCoords(t, ctx, d, uid)
	if lat == nil || *lat != 48.15 {
		t.Errorf("latitude = %v, want 48.15 (calendar:google must win)", lat)
	}
	if prov != "calendar:google" {
		t.Errorf("provenance = %q, want %q", prov, "calendar:google")
	}
}

// TestEntityCoords_NominatimFillsNullCoords verifies that a geocoded:nominatim
// event fills coordinates when none are stored yet.
func TestEntityCoords_NominatimFillsNullCoords(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	uid := "01HSPATIAL000000000000003"

	// First event without coordinates.
	applyEntity(t, ctx, d, uid, ontology.Entity{
		UID:              uid,
		Kind:             ontology.EntityKindPlace,
		DisplayName:      "Hamburg",
		Confidence:       0.8,
		ResolutionStatus: ontology.ResolutionTentative,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
	})

	lat, _, _ := readEntityCoords(t, ctx, d, uid)
	if lat != nil {
		t.Fatalf("expected nil latitude before coordinates are set, got %v", *lat)
	}

	// Second event with geocoded:nominatim coordinates.
	applyEntity(t, ctx, d, uid, makeCoordEntity(uid, "Hamburg", 53.55, 10.00, "geocoded:nominatim"))

	lat2, _, prov2 := readEntityCoords(t, ctx, d, uid)
	if lat2 == nil || *lat2 != 53.55 {
		t.Errorf("latitude = %v, want 53.55", lat2)
	}
	if prov2 != "geocoded:nominatim" {
		t.Errorf("provenance = %q, want %q", prov2, "geocoded:nominatim")
	}
}

// TestNearbyEntities_ReturnsCorrectSubset applies several entities with
// coordinates and verifies that NearbyEntities returns only those within the
// query radius. Berlin is the query origin (52.52 N, 13.40 E).
//
//   - Berlin HQ (0 km):      always in
//   - Munich (504 km):        out for radius 100, in for radius 600
//   - London (932 km):        out for radius 600
func TestNearbyEntities_ReturnsCorrectSubset(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	entities := []struct {
		uid  string
		name string
		lat  float64
		lon  float64
	}{
		{"01HGEO0000000000000000001", "Berlin HQ", 52.52, 13.40},
		{"01HGEO0000000000000000002", "Munich Office", 48.14, 11.58},
		{"01HGEO0000000000000000003", "London Branch", 51.51, -0.13},
	}

	for _, e := range entities {
		applyEntity(t, ctx, d, e.uid,
			makeCoordEntity(e.uid, e.name, e.lat, e.lon, "user_provided"))
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	// 100 km from Berlin: only Berlin HQ.
	nearby100, err := geo.NearbyEntities(ctx, db, 52.52, 13.40, 100, allScopesTestPredicate, nil)
	if err != nil {
		t.Fatalf("NearbyEntities r=100: %v", err)
	}
	if len(nearby100) != 1 {
		t.Errorf("r=100: got %d entities, want 1 (Berlin only)", len(nearby100))
	}
	if len(nearby100) == 1 && nearby100[0].DisplayName != "Berlin HQ" {
		t.Errorf("r=100: got %q, want Berlin HQ", nearby100[0].DisplayName)
	}

	// 600 km from Berlin: Berlin HQ + Munich.
	nearby600, err := geo.NearbyEntities(ctx, db, 52.52, 13.40, 600, allScopesTestPredicate, nil)
	if err != nil {
		t.Fatalf("NearbyEntities r=600: %v", err)
	}
	if len(nearby600) != 2 {
		t.Errorf("r=600: got %d entities, want 2 (Berlin + Munich)", len(nearby600))
	}

	// 2000 km from Berlin: all three.
	nearby2000, err := geo.NearbyEntities(ctx, db, 52.52, 13.40, 2000, allScopesTestPredicate, nil)
	if err != nil {
		t.Fatalf("NearbyEntities r=2000: %v", err)
	}
	if len(nearby2000) != 3 {
		t.Errorf("r=2000: got %d entities, want 3", len(nearby2000))
	}
}

// TestNearbyEntities_OutOfScopeExcluded reproduces the #206 spatial-lane leak:
// a team-scoped entity sitting inside the query radius must NOT be returned to a
// default-scope ({project, personal}) caller, because its coordinates are
// out-of-scope location data. The all-scopes predicate proves the team entity is
// genuinely in range, so the exclusion is the scope predicate doing its job, not
// the radius.
func TestNearbyEntities_OutOfScopeExcluded(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const personalUID = "01HGEO0000000000000000010"
	const teamUID = "01HGEO0000000000000000011"
	// Both within ~5 km of the Berlin origin (52.52, 13.40).
	applyEntityScoped(t, ctx, d, personalUID,
		makeCoordEntity(personalUID, "Berlin Personal", 52.52, 13.40, "user_provided"), kernel.ScopePersonal)
	applyEntityScoped(t, ctx, d, teamUID,
		makeCoordEntity(teamUID, "Berlin Team", 52.50, 13.42, "user_provided"), kernel.ScopeTeam)

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	// Default read span {project, personal} must hide the team entity.
	defPred, defArgs := kernel.DefaultReadSet().Predicate("")
	def, err := geo.NearbyEntities(ctx, db, 52.52, 13.40, 100, defPred, defArgs)
	if err != nil {
		t.Fatalf("NearbyEntities default scope: %v", err)
	}
	for _, e := range def {
		if e.UID == teamUID {
			t.Errorf("LEAK: default-scope NearbyEntities returned team entity %s", teamUID)
		}
	}
	if !containsCoordUID(def, personalUID) {
		t.Errorf("default-scope NearbyEntities missing personal entity %s: %v", personalUID, coordUIDs(def))
	}

	// All scopes proves the team entity is genuinely in range (it was excluded by
	// scope, not by distance).
	allPred, allArgs := kernel.AllScopes().Predicate("")
	all, err := geo.NearbyEntities(ctx, db, 52.52, 13.40, 100, allPred, allArgs)
	if err != nil {
		t.Fatalf("NearbyEntities all scopes: %v", err)
	}
	if !containsCoordUID(all, teamUID) {
		t.Errorf("all-scopes NearbyEntities missing team entity %s (should be in radius): %v", teamUID, coordUIDs(all))
	}

	// Fail-closed: an empty scope set returns nothing.
	emptyPred, emptyArgs := kernel.NewScopeSet().Predicate("")
	empty, err := geo.NearbyEntities(ctx, db, 52.52, 13.40, 100, emptyPred, emptyArgs)
	if err != nil {
		t.Fatalf("NearbyEntities empty scope: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty ScopeSet returned %d entities, want 0 (fail-closed)", len(empty))
	}
}

// applyEntityScoped applies an entity-created event stamped with scope.
func applyEntityScoped(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string, entity ontology.Entity, scope kernel.Scope) {
	t.Helper()
	ev := makeEntityEvent(t, uid, entity)
	ev.Scope = scope
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply scoped entity %s: %v", uid, err)
	}
}

func containsCoordUID(cs []geo.EntityCoord, uid string) bool {
	for _, c := range cs {
		if c.UID == uid {
			return true
		}
	}
	return false
}

func coordUIDs(cs []geo.EntityCoord) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.UID
	}
	return out
}

// TestNearbyEntities_EntityWithoutCoordsExcluded verifies that an entity
// without coordinates is never returned by NearbyEntities regardless of radius.
func TestNearbyEntities_EntityWithoutCoordsExcluded(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	uid := "01HGEO0000000000000000004"
	applyEntity(t, ctx, d, uid, ontology.Entity{
		UID:              uid,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "No Coords Person",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		// Coordinates intentionally nil.
	})

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	nearby, err := geo.NearbyEntities(ctx, db, 0, 0, 1e9, allScopesTestPredicate, nil) // planet-wide radius
	if err != nil {
		t.Fatalf("NearbyEntities: %v", err)
	}
	for _, ec := range nearby {
		if ec.UID == uid {
			t.Errorf("entity without coordinates should not appear in NearbyEntities results")
		}
	}
}
