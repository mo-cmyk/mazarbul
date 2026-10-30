package render

import (
	"bytes"
	"encoding/json"
	"time"
)

// supersededRelationJSONLRow is the wire shape for superseded-relations.jsonl.
type supersededRelationJSONLRow struct {
	UID          string `json:"uid"`
	Subject      string `json:"subject"`
	Predicate    string `json:"predicate"`
	Object       string `json:"object"`
	SupersededBy string `json:"superseded_by"`
	ValidUntil   string `json:"validUntil,omitempty"`
}

// writeSupersededRelationsJSONL returns the full superseded-relations.jsonl
// content: one line per relation whose SupersededByUID is non-empty,
// ULID-sorted.
func writeSupersededRelationsJSONL(state *GraphState) []byte {
	uids := sortedKeys(state.Relations)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	for _, uid := range uids {
		r := state.Relations[uid]
		if r.SupersededByUID == "" {
			continue
		}

		jr := supersededRelationJSONLRow{
			UID:          uid,
			Subject:      r.SubjectUID,
			Predicate:    r.Predicate,
			Object:       r.ObjectUID,
			SupersededBy: r.SupersededByUID,
		}
		if r.ValidUntil != nil {
			jr.ValidUntil = r.ValidUntil.UTC().Format(time.RFC3339Nano)
		}
		encodeJSONL(enc, jr)
	}
	return buf.Bytes()
}
