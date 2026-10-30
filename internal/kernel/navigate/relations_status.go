package navigate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ProposedRelation is one relation in a given trust state (typically
// "proposed") enriched with the connector source tag and the scope it lives in,
// so the reviewer queue can group untrusted edges by the connector that
// proposed them and promote each in its own scope. It embeds Relation, so all
// the standard edge fields (uid, predicate, hydrated endpoint names, confidence,
// createTime, status) are present unchanged.
type ProposedRelation struct {
	Relation
	// Source is the origin event's source tag, e.g. "connector:granola". It is
	// read from the provenance row of the relation's source_event_uid (the
	// lineage index), so the queue can group untrusted edges by connector. Empty
	// when the origin event recorded no source.
	Source string `json:"source"`
	// Scope is the scope the edge physically lives in (project|personal|team).
	// The reviewer promotes each edge in its own scope, so a team-scope edge is
	// never promoted under personal authority.
	Scope string `json:"scope"`
}

// connectorSourceExpr reads the connector source tag for a relation from the
// origin event's provenance row (the lineage index keyed on event_uid), NOT the
// relation.provenance free-text column. A correlated subquery avoids the row
// multiplication a LEFT JOIN would risk if an event ever carried >1 provenance
// row. It is deliberately unscoped, matching EventProvenance: the entry point is
// a relation already gated by the scope predicate below, so the lineage lookup
// cannot widen visibility.
const connectorSourceExpr = `COALESCE((SELECT pv.source FROM provenance pv WHERE pv.event_uid = r.source_event_uid LIMIT 1), '')`

// ListRelationsByStatus returns every relation whose trust status equals status
// (e.g. "proposed"), newest first, scoped to the navigator's span. It is the
// global reviewer-queue read: unlike EntityRelations it is not anchored to one
// entity, so a human can disposition every connector-proposed edge at once.
//
// Subject and object display names are hydrated under the same scope gate as
// GetRelation: an out-of-span endpoint surfaces only its UID, never its name.
// Each row carries the connector source tag (from the origin event's
// provenance) and the edge's own scope so a reviewer can group by connector and
// promote in the right scope. Scope isolation is structural: a team-scope edge
// never appears in a {project,personal} read, so it can never leak into a
// personal review (#206).
//
// A non-empty source narrows the result to edges whose origin event source
// equals it (the connector filter). A non-positive limit applies defaultLimit.
func (n *Navigator) ListRelationsByStatus(ctx context.Context, status, source string, limit int) ([]ProposedRelation, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	rScope, rArgs := n.scopeCond("r")
	sScope, sArgs := n.scopeCond("s")
	oScope, oArgs := n.scopeCond("o")

	where := "r.status = ? AND " + rScope
	whereArgs := make([]any, 0, len(rArgs)+2)
	whereArgs = append(whereArgs, status)
	whereArgs = append(whereArgs, rArgs...)
	if source != "" {
		where += " AND " + connectorSourceExpr + " = ?"
		whereArgs = append(whereArgs, source)
	}

	// The clause fragments are compile-time constants; every runtime value is a
	// bound ? parameter, so this concatenation is not an injection vector.
	//nolint:gosec // G202: only constant clause fragments concatenated; values are parameterized
	q := `SELECT r.uid, r.subject_uid, s.display_name, r.predicate,
	             r.object_uid, o.display_name, r.confidence, r.create_time,
	             r.valid_from, r.valid_until, r.superseded_by_uid, r.attributes,
	             r.source_event_uid, r.provenance, r.status,
	             r.scope, ` + connectorSourceExpr + ` AS connector_source
	      FROM relations r
	      LEFT JOIN entities s ON s.uid = r.subject_uid AND ` + sScope + `
	      LEFT JOIN entities o ON o.uid = r.object_uid AND ` + oScope + `
	      WHERE ` + where + `
	      ORDER BY connector_source ASC, r.create_time DESC
	      LIMIT ?`

	// Args follow query-text order: the JOIN scope predicates precede the WHERE
	// clause, then the WHERE args, then the LIMIT.
	args := make([]any, 0, len(sArgs)+len(oArgs)+len(whereArgs)+1)
	args = append(args, sArgs...)
	args = append(args, oArgs...)
	args = append(args, whereArgs...)
	args = append(args, limit)

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list relations by status %q: %w", status, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ProposedRelation, 0, limit)
	for rows.Next() {
		pr, serr := scanProposedRelation(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan proposed relation: %w", serr)
		}
		out = append(out, pr)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate proposed relations: %w", rerr)
	}
	return out, nil
}

// scanProposedRelation reads one row in the column order ListRelationsByStatus
// selects: the 15 canonical relation columns (matching scanRelation) followed by
// the edge's scope and the connector source. It mirrors scanRelation's
// null-handling so an embedded Relation round-trips identically.
func scanProposedRelation(s rowScanner) (ProposedRelation, error) {
	var (
		pr                                           ProposedRelation
		subjName, objName                            sql.NullString
		validFrom, validUntil, supersededBy, attrRaw sql.NullString
		sourceEventUID, provenance, status           sql.NullString
		scope, connectorSource                       sql.NullString
	)
	if err := s.Scan(
		&pr.UID, &pr.SubjectUID, &subjName, &pr.Predicate,
		&pr.ObjectUID, &objName, &pr.Confidence, &pr.CreateTime,
		&validFrom, &validUntil, &supersededBy, &attrRaw,
		&sourceEventUID, &provenance, &status,
		&scope, &connectorSource,
	); err != nil {
		return ProposedRelation{}, err
	}
	pr.SubjectName = subjName.String
	pr.ObjectName = objName.String
	pr.SourceEventUID = sourceEventUID.String
	pr.ValidFrom = validFrom.String
	pr.ValidUntil = validUntil.String
	pr.SupersededByUID = supersededBy.String
	pr.Provenance = provenance.String
	pr.Status = status.String
	if pr.Status == "" {
		pr.Status = relationStatusAssertedDefault
	}
	if attrRaw.Valid && attrRaw.String != "" && attrRaw.String != "{}" {
		attrs := map[string]string{}
		if err := json.Unmarshal([]byte(attrRaw.String), &attrs); err == nil && len(attrs) > 0 {
			pr.Attributes = attrs
		}
	}
	pr.Scope = scope.String
	pr.Source = connectorSource.String
	return pr, nil
}
