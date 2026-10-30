package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// writeKindIndex returns the INDEX.md content for one entity kind.
// Table format: | uid | displayName | aliases | mentions | status |
// ULID-sorted.
func writeKindIndex(state *GraphState, kind ontology.EntityKind) []byte {
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("# %s index\n\n", string(kind)))
	buf.WriteString("| uid | displayName | aliases | mentions | status |\n")
	buf.WriteString("|-----|-------------|---------|----------|--------|\n")

	uids := sortedKeys(state.Entities)
	for _, uid := range uids {
		row := state.Entities[uid]
		if row.Kind != kind {
			continue
		}

		aliases := make([]string, len(row.Aliases))
		copy(aliases, row.Aliases)
		sort.Strings(aliases)

		status := string(row.ResolutionStatus)
		if row.Orphaned {
			status = statusOrphaned
		}

		buf.WriteString("| ")
		buf.WriteString(uid)
		buf.WriteString(" | ")
		buf.WriteString(renderYAMLString(row.DisplayName))
		buf.WriteString(" | ")
		buf.WriteString(strings.Join(aliases, ", "))
		buf.WriteString(" | ")
		buf.WriteString(strconv.Itoa(len(row.MentionUIDs)))
		buf.WriteString(" | ")
		buf.WriteString(status)
		buf.WriteString(" |\n")
	}
	return []byte(buf.String())
}

// allKinds returns every EntityKind constant in a stable order.
func allKinds() []ontology.EntityKind {
	return []ontology.EntityKind{
		ontology.EntityKindPerson,
		ontology.EntityKindOrganization,
		ontology.EntityKindPlace,
		ontology.EntityKindProject,
		ontology.EntityKindEvent,
		ontology.EntityKindDocument,
	}
}

// kindIndexPath returns the rendered path for a kind index file.
func kindIndexPath(kind ontology.EntityKind) string {
	return renderedPath("by-kind", string(kind), "INDEX.md")
}
