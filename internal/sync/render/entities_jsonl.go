package render

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"
)

// entityJSONLRow is the stable wire shape for one line in entities.jsonl.
type entityJSONLRow struct {
	UID              string                 `json:"uid"`
	Kind             string                 `json:"kind"`
	DisplayName      string                 `json:"displayName"`
	Aliases          []string               `json:"aliases"`
	Identifiers      []entityIdentifierJSON `json:"identifiers,omitempty"`
	Confidence       float64                `json:"confidence"`
	ResolutionStatus string                 `json:"resolutionStatus"`
	CreateTime       string                 `json:"createTime"`
	LastUpdated      string                 `json:"lastUpdated"`
	MentionCount     int                    `json:"mentionCount"`
	Orphaned         bool                   `json:"orphaned"`
	// Properties carries the entity's typed attributes (P3) as the compacted
	// JSON object stored on the EntityRow, so the git-rendered view shows the
	// same typed data the SQLite projection materializes. Omitted when the
	// entity has none.
	Properties json.RawMessage `json:"properties,omitempty"`
}

// entityIdentifierJSON mirrors ontology.Identifier as a JSONL-friendly
// shape. Kept private to this file so the JSONL wire format stays
// owned by the renderer regardless of upstream renames.
type entityIdentifierJSON struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// writeEntitiesJSONL returns a byte slice where each line is a JSON object
// for one current entity, ULID-sorted.
func writeEntitiesJSONL(state *GraphState) []byte {
	uids := sortedKeys(state.Entities)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	for _, uid := range uids {
		row := state.Entities[uid]
		aliases := make([]string, len(row.Aliases))
		copy(aliases, row.Aliases)
		sort.Strings(aliases)

		status := string(row.ResolutionStatus)
		if row.Orphaned {
			status = statusOrphaned
		}

		// Redact uri-type identifier values for external_ref entities, and use
		// the redacted display name when it was auto-set to a raw URI.
		// Mirrors the source_refs #207 render rule: unconditional, because the
		// renderer only runs on the plaintext push path.
		var idRows []entityIdentifierJSON
		if len(row.Identifiers) > 0 {
			idRows = make([]entityIdentifierJSON, 0, len(row.Identifiers))
			for _, id := range row.Identifiers {
				idRows = append(idRows, entityIdentifierJSON{
					Type:  id.Type,
					Value: redactedIdentifierValue(row.Kind, id.Type, id.Value),
				})
			}
			sort.SliceStable(idRows, func(i, j int) bool {
				if idRows[i].Type != idRows[j].Type {
					return idRows[i].Type < idRows[j].Type
				}
				return idRows[i].Value < idRows[j].Value
			})
		}

		jr := entityJSONLRow{
			UID:              uid,
			Kind:             string(row.Kind),
			DisplayName:      redactedExternalRefDisplayName(row),
			Aliases:          aliases,
			Identifiers:      idRows,
			Confidence:       row.Confidence,
			ResolutionStatus: status,
			CreateTime:       row.CreateTime.UTC().Format(time.RFC3339Nano),
			LastUpdated:      row.LastUpdated.UTC().Format(time.RFC3339Nano),
			MentionCount:     len(row.MentionUIDs),
			Orphaned:         row.Orphaned,
			Properties:       row.Properties,
		}
		encodeJSONL(enc, jr)
	}
	return buf.Bytes()
}
