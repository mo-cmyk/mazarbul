package resolver

import "github.com/mo-cmyk/mazarbul/internal/ontology"

// ForwardCoordinates copies coordinates from a place Mention to its Entity,
// respecting the provenance rank order so lower-rank sources never overwrite
// higher-rank coordinates.
//
// Rank order (highest wins): user_provided > calendar:google >
// geocoded:nominatim > unknown (rank 0).
//
// Note: Mentions do not currently carry a Coordinates field in the v0.2.0
// schema. This helper accepts them as separate arguments so the orchestrator
// can plumb them through from a future MCP surface (e.g. `maz remember
// --coords`) or from a connector that enriches place Mentions with geocoded
// coordinates before calling Resolve.
func ForwardCoordinates(entity ontology.Entity, mentionCoords *ontology.Coordinates, mentionProv string) ontology.Entity {
	if mentionCoords == nil {
		return entity
	}

	// If the entity already has coordinates, only overwrite when the new
	// source outranks the existing one.
	if entity.Coordinates != nil {
		existingRank := ProvenanceRank(entity.CoordinatesProvenance)
		incomingRank := ProvenanceRank(mentionProv)
		if incomingRank <= existingRank {
			return entity
		}
	}

	entity.Coordinates = mentionCoords
	entity.CoordinatesProvenance = mentionProv
	return entity
}
