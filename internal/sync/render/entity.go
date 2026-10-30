package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// identifierTypeURI is the identifier type for external_ref pointer entities.
// Defined locally so the render package does not import internal/mcp/tools,
// which is the other consumer of this literal.
const identifierTypeURI = "uri"

// redactedExternalRefDisplayName returns the display name to render for row.
// For external_ref entities whose DisplayName equals one of the raw uri-type
// identifier values (the reference tool auto-sets the display name to the URL
// when no label is supplied), the matching URI is replaced with its SHA-256
// hash. All other entities and display names are returned verbatim.
//
// Redaction is UNCONDITIONAL: the renderer only ever runs on the plaintext
// push path (writeRenderedView prunes the rendered/ tree for encrypted scopes
// before Render is called), so there is no plaintext-vs-encrypted branch to
// get wrong. This mirrors the source_refs #207 render rule in
// writeSourceRefsJSONL.
func redactedExternalRefDisplayName(row *EntityRow) string {
	if row.Kind != ontology.EntityKindExternalRef {
		return row.DisplayName
	}
	for _, id := range row.Identifiers {
		if id.Type == identifierTypeURI && id.Value == row.DisplayName {
			return hashExternalID(id.Value)
		}
	}
	return row.DisplayName
}

// redactedIdentifierValue returns the value to render for one identifier on
// an entity. For uri-type identifiers on external_ref entities the raw value
// is replaced with its SHA-256 hash. All other identifiers on all other entity
// kinds return their value verbatim.
//
// Redaction is UNCONDITIONAL (same rationale as redactedExternalRefDisplayName).
func redactedIdentifierValue(kind ontology.EntityKind, idType, idValue string) string {
	if kind == ontology.EntityKindExternalRef && idType == identifierTypeURI {
		return hashExternalID(idValue)
	}
	return idValue
}

// writeEntityStubFull produces the full markdown file for one entity.
// Sections in fixed order: frontmatter, H1 title, Current relations,
// Supersede history, Forgotten, Conflicts, Provenance trail.
// Empty sections are omitted entirely.
func writeEntityStubFull(state *GraphState, uid string) []byte {
	row, ok := state.Entities[uid]
	if !ok {
		return nil
	}

	var buf strings.Builder

	writeFullFrontmatter(&buf, row)
	fmt.Fprintf(&buf, "\n# %s\n", redactedExternalRefDisplayName(row))

	if row.Orphaned {
		buf.WriteString("\n> Warning: No current mentions (orphaned).\n")
	}

	writeCurrentRelations(&buf, state, uid)
	writeSupersededRelations(&buf, state, uid)
	writeForgottenRelationsSection(&buf, state, uid)
	writeConflictsSection(&buf, state, uid)
	writeProvenanceTrail(&buf, row)

	return []byte(buf.String())
}

func writeFullFrontmatter(buf *strings.Builder, row *EntityRow) {
	sortedAliases := make([]string, len(row.Aliases))
	copy(sortedAliases, row.Aliases)
	sort.Strings(sortedAliases)

	status := string(row.ResolutionStatus)
	if row.Orphaned {
		status = statusOrphaned
	}

	// Write scalar fields via writeFrontmatter for deterministic key order.
	// display_name uses the redacted form so a raw URI never reaches the
	// plaintext git tree for external_ref entities (#207-style render rule).
	fields := []frontmatterField{
		{key: "uid", value: row.UID},
		{key: "kind", value: string(row.Kind)},
		{key: "display_name", value: redactedExternalRefDisplayName(row)},
	}
	if len(sortedAliases) > 0 {
		fields = append(fields, frontmatterField{key: "aliases", value: sortedAliases})
	}
	fields = append(fields,
		frontmatterField{key: "confidence", value: row.Confidence},
		frontmatterField{key: "resolution_status", value: status},
		frontmatterField{key: "create_time", value: row.CreateTime.UTC()},
		frontmatterField{key: "last_updated", value: row.LastUpdated.UTC()},
		frontmatterField{key: "mention_count", value: len(row.MentionUIDs)},
	)

	// writeFrontmatter handles the "---\n...\n---\n" envelope for scalars.
	// We then append block sequences for identifiers and provenance by
	// reopening the envelope manually.
	writeEntityFrontmatterBlock(buf, fields, row)
}

// writeEntityFrontmatterBlock writes the complete YAML frontmatter block
// including the scalar fields via writeFrontmatter and the block sequences
// for identifiers and provenance.
func writeEntityFrontmatterBlock(buf *strings.Builder, fields []frontmatterField, row *EntityRow) {
	// Write opening "---\n" + scalar fields (no closing "---\n" yet).
	buf.WriteString("---\n")
	for _, f := range fields {
		if f.value == nil {
			continue
		}
		buf.WriteString(f.key)
		buf.WriteString(": ")
		buf.WriteString(renderFrontmatterValue(f.value))
		buf.WriteByte('\n')
	}

	if len(row.Identifiers) > 0 {
		buf.WriteString("identifiers:\n")
		for _, id := range row.Identifiers {
			// Hash uri-type identifier values for external_ref entities: the raw
			// URI never reaches the plaintext git tree (#207-style render rule).
			idValue := redactedIdentifierValue(row.Kind, id.Type, id.Value)
			buf.WriteString("  - type: ")
			buf.WriteString(renderYAMLString(id.Type))
			buf.WriteByte('\n')
			buf.WriteString("    value: ")
			buf.WriteString(renderYAMLString(idValue))
			buf.WriteByte('\n')
		}
	}

	if len(row.EpisodeUIDs) > 0 {
		sorted := make([]string, len(row.EpisodeUIDs))
		copy(sorted, row.EpisodeUIDs)
		sort.Strings(sorted)
		buf.WriteString("provenance:\n")
		for _, ep := range sorted {
			buf.WriteString("  - event: ")
			buf.WriteString(ep)
			buf.WriteByte('\n')
		}
	}

	buf.WriteString("---\n")
}

func writeCurrentRelations(buf *strings.Builder, state *GraphState, uid string) {
	rows := collectCurrentRelations(state, uid)
	if len(rows) == 0 {
		return
	}
	buf.WriteString("\n## Current relations\n")
	for _, r := range rows {
		buf.WriteString("- ")
		buf.WriteString(r.Predicate)
		buf.WriteString(" → ")
		buf.WriteString(r.ObjectUID)
		if r.ValidFrom != nil {
			buf.WriteString(" (since ")
			buf.WriteString(r.ValidFrom.UTC().Format("2006-01-02"))
			buf.WriteByte(')')
		}
		if r.ConflictWith != "" {
			buf.WriteString(" ⚠ conflict with ")
			buf.WriteString(r.ConflictWith)
		}
		buf.WriteByte('\n')
	}
}

func writeSupersededRelations(buf *strings.Builder, state *GraphState, uid string) {
	rows := collectSupersededRelations(state, uid)
	if len(rows) == 0 {
		return
	}
	buf.WriteString("\n## Supersede history\n")
	for _, r := range rows {
		buf.WriteString("- ~~")
		buf.WriteString(r.Predicate)
		buf.WriteString(" → ")
		buf.WriteString(r.ObjectUID)
		buf.WriteString("~~")
		if r.ValidUntil != nil {
			buf.WriteString(" (until ")
			buf.WriteString(r.ValidUntil.UTC().Format("2006-01-02"))
			buf.WriteString(", ↳ superseded by ")
			buf.WriteString(r.SupersededByUID)
			buf.WriteByte(')')
		}
		buf.WriteByte('\n')
	}
}

func writeForgottenRelationsSection(buf *strings.Builder, state *GraphState, uid string) {
	rows := collectForgottenRelations(state, uid)
	if len(rows) == 0 {
		return
	}
	buf.WriteString("\n## Forgotten\n")
	for _, r := range rows {
		buf.WriteString("- ⊗ ")
		buf.WriteString(r.Predicate)
		buf.WriteString(" → ")
		buf.WriteString(r.ObjectUID)
		if r.ForgottenReason != "" {
			buf.WriteString(" (")
			buf.WriteString(renderYAMLString(r.ForgottenReason))
			buf.WriteByte(')')
		}
		buf.WriteByte('\n')
	}
}

func writeConflictsSection(buf *strings.Builder, state *GraphState, uid string) {
	rows := collectConflictRelations(state, uid)
	if len(rows) == 0 {
		return
	}
	buf.WriteString("\n## Unresolved conflicts\n")
	for _, r := range rows {
		buf.WriteString("- ⚠ ")
		buf.WriteString(r.Predicate)
		buf.WriteString(": conflict between ")
		buf.WriteString(r.UID)
		buf.WriteString(" and ")
		buf.WriteString(r.ConflictWith)
		buf.WriteByte('\n')
	}
}

func writeProvenanceTrail(buf *strings.Builder, row *EntityRow) {
	if len(row.EpisodeUIDs) == 0 && len(row.MentionUIDs) == 0 {
		return
	}
	buf.WriteString("\n## Provenance trail\n")
	sorted := make([]string, len(row.EpisodeUIDs))
	copy(sorted, row.EpisodeUIDs)
	sort.Strings(sorted)
	for _, ep := range sorted {
		buf.WriteString("- ")
		buf.WriteString(ep)
		buf.WriteByte('\n')
	}
}

// collectCurrentRelations returns relations where this entity is the subject,
// not superseded, not forgotten, sorted by UID.
func collectCurrentRelations(state *GraphState, subjectUID string) []*RelationRow {
	rows := state.relationsForSubject(subjectUID)
	out := make([]*RelationRow, 0, len(rows))
	for _, r := range rows {
		if r.SupersededByUID != "" || r.Forgotten {
			continue
		}
		out = append(out, r)
	}
	return out
}

// collectSupersededRelations returns superseded relations for a subject UID,
// sorted by UID.
func collectSupersededRelations(state *GraphState, subjectUID string) []*RelationRow {
	rows := state.relationsForSubject(subjectUID)
	out := make([]*RelationRow, 0, len(rows))
	for _, r := range rows {
		if r.SupersededByUID == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// collectForgottenRelations returns forgotten relations for a subject UID,
// sorted by UID.
func collectForgottenRelations(state *GraphState, subjectUID string) []*RelationRow {
	rows := state.relationsForSubject(subjectUID)
	out := make([]*RelationRow, 0, len(rows))
	for _, r := range rows {
		if !r.Forgotten {
			continue
		}
		out = append(out, r)
	}
	return out
}

// collectConflictRelations returns relations with an unresolved conflict for
// a subject UID, sorted by UID.
func collectConflictRelations(state *GraphState, subjectUID string) []*RelationRow {
	rows := state.relationsForSubject(subjectUID)
	out := make([]*RelationRow, 0, len(rows))
	for _, r := range rows {
		if r.ConflictWith == "" {
			continue
		}
		if r.SupersededByUID != "" || r.Forgotten {
			continue
		}
		out = append(out, r)
	}
	return out
}

// entityFilePath returns the canonical rendered path for an entity stub.
func entityFilePath(uid string) string {
	return renderedPath(shardPath(uid))
}
