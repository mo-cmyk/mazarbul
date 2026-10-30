// Package geo provides geographic distance and nearby-entity scan helpers.
// M1 ships brute-force haversine; geohash prefix indexing lands when
// n > 5k coord-bearing entities forces it.
//
// All coordinates are WGS84 decimal degrees unless stated otherwise.
// Latitude is in [-90, 90]; longitude is in [-180, 180].
package geo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// EarthRadiusKm is the WGS84 mean radius in kilometres.
const EarthRadiusKm = 6371.0

// HaversineKm returns the great-circle distance in kilometres between two
// WGS84 coordinates (lat1, lon1) and (lat2, lon2) given in decimal degrees.
//
// Pure function: no allocation, no I/O. The haversine formula is numerically
// stable for both small and large inter-point distances including the
// near-antipodal case (lat1=-lat2, lon1=lon2+180). Maximum absolute error
// is roughly 0.3% because WGS84 is an oblate spheroid and haversine treats
// the Earth as a sphere; the error is negligible for the brute-force nearby
// scan at M1 scale (n < 5k entities).
func HaversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const toRad = math.Pi / 180.0

	dLat := (lat2 - lat1) * toRad
	dLon := (lon2 - lon1) * toRad

	lat1r := lat1 * toRad
	lat2r := lat2 * toRad

	sinDLat := math.Sin(dLat / 2)
	sinDLon := math.Sin(dLon / 2)

	a := sinDLat*sinDLat + math.Cos(lat1r)*math.Cos(lat2r)*sinDLon*sinDLon
	c := 2 * math.Asin(math.Sqrt(a))

	return EarthRadiusKm * c
}

// EntityCoord pairs an entity UID with its stored coordinates and the
// haversine distance from the query origin.
type EntityCoord struct {
	// UID is the entity's ULID identifier.
	UID string

	// DisplayName is the human-readable label for the entity.
	DisplayName string

	// Latitude is the entity's WGS84 latitude in decimal degrees.
	Latitude float64

	// Longitude is the entity's WGS84 longitude in decimal degrees.
	Longitude float64

	// CoordinatesProvenance records the origin of the coordinates, e.g.
	// "user_provided", "geocoded:nominatim", or "calendar:google".
	CoordinatesProvenance string

	// DistanceKm is the haversine great-circle distance from the query
	// origin to (Latitude, Longitude) in kilometres.
	DistanceKm float64
}

// NearbyEntities returns all entities within radiusKm of (lat, lon) by
// scanning every entity that has non-NULL coordinates in the entities table
// and computing the haversine distance for each one.
//
// Complexity: O(n) in the number of coord-bearing entities. At n < 5k this
// runs in under 1 ms on M-series silicon; beyond that, add geohash prefix
// indexing as a non-breaking optimisation (the latitude/longitude columns
// are already nullable and can be backfilled). The haversine computation is
// a pure function so there is no caching risk.
//
// The result slice is unsorted; callers that need closest-first ordering
// must sort by DistanceKm.
//
// Db must be a read-capable connection (the dispatcher's reader pool is
// appropriate; no write lock is acquired).
// ScopePredicate is a SQL fragment over the (unqualified) entities.scope column
// plus its bound args, threaded in by the caller — geo is a leaf package and
// cannot import kernel.ScopeSet, so the predicate is passed as text. It must be
// non-empty (ScopeSet.Predicate always yields one, e.g. "scope IN (?, ?)" or the
// fail-closed "scope IN (NULL)"); a coordinate scan that skipped it would let
// out-of-scope entity locations influence in-scope recall (#206).
func NearbyEntities(ctx context.Context, db *sql.DB, lat, lon, radiusKm float64, scopePredicate string, scopeArgs []any) ([]EntityCoord, error) {
	if scopePredicate == "" {
		return nil, errors.New("geo: NearbyEntities requires a scope predicate (fail-closed)")
	}
	//nolint:gosec // G202: scopePredicate is constant fragments + bound ? params from ScopeSet.Predicate
	q := `
		SELECT uid, display_name, latitude, longitude, coordinates_provenance
		FROM   entities
		WHERE  latitude IS NOT NULL
		  AND  longitude IS NOT NULL
		  AND  ` + scopePredicate
	rows, err := db.QueryContext(ctx, q, scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("geo: NearbyEntities query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []EntityCoord
	for rows.Next() {
		var (
			uid  string
			name string
			elat float64
			elon float64
			prov sql.NullString
		)
		if err := rows.Scan(&uid, &name, &elat, &elon, &prov); err != nil {
			return nil, fmt.Errorf("geo: NearbyEntities scan: %w", err)
		}
		d := HaversineKm(lat, lon, elat, elon)
		if d <= radiusKm {
			result = append(result, EntityCoord{
				UID:                   uid,
				DisplayName:           name,
				Latitude:              elat,
				Longitude:             elon,
				CoordinatesProvenance: prov.String,
				DistanceKm:            d,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("geo: NearbyEntities iterate: %w", err)
	}
	return result, nil
}
