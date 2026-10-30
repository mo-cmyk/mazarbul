package ontology

import "strings"

// The connector-source convention: an event pulled by an external ingestion
// connector tags kernel.Event.Source with one of these prefixes (e.g.
// "connector:local-git"). The prefix is the single shared signal that an
// event's content is UNTRUSTED external input. Two boundaries key off it and
// must never drift, so both call IsConnectorSource:
//
//   - the privacy gate (internal/sync/privacy.sourceConnectorMarker) treats a
//     connector-sourced event as sensitive and fail-closed-refuses it on a
//     plaintext sync surface;
//   - the resolver treats facts extracted from a connector-sourced episode as
//     untrusted: status=proposed, excluded from supersession, and barred from
//     minting new entity kinds / predicates (issue #211). Trust is carried by
//     the status ATTRIBUTE, not a depressed confidence (#212); promoteRelation
//     lifts the quarantine by flipping a proposed edge to asserted.
const (
	// ConnectorSourceColonPrefix is the canonical external-source form, e.g.
	// "connector:gmail" (documented on the MCP source field).
	ConnectorSourceColonPrefix = "connector:"
	// ConnectorSourceSlashPrefix mirrors the "extractor/v1" slash style some
	// writers use, e.g. "connector/local-git".
	ConnectorSourceSlashPrefix = "connector/"
)

// IsConnectorSource reports whether an event Source string names an external
// ingestion connector (and is therefore untrusted external input). It is the
// single definition shared by the privacy leak gate and the resolver trust
// gate, so the two boundaries can never disagree about what counts as a
// connector source.
func IsConnectorSource(source string) bool {
	return strings.HasPrefix(source, ConnectorSourceColonPrefix) ||
		strings.HasPrefix(source, ConnectorSourceSlashPrefix)
}

// Relation status values for the trust posture of an edge (see Relation.Status).
const (
	// RelationStatusAsserted is the authoritative default: the edge can
	// supersede and be superseded.
	RelationStatusAsserted = "asserted"
	// RelationStatusProposed marks an edge from an untrusted external source
	// (connector ingest): excluded from supersession and predicate minting until
	// promoted. It is NOT confidence-penalised — the status attribute alone
	// carries the trust posture (#212), so a proposed edge ranks on relevance and
	// promoteRelation can flip it to asserted.
	RelationStatusProposed = "proposed"
)
