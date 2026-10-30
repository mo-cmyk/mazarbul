package render

import (
	"bytes"
	"encoding/json"
	"time"
)

// forgottenRelationJSONLRow is the wire shape for forgotten-relations.jsonl.
type forgottenRelationJSONLRow struct {
	UID         string `json:"uid"`
	Subject     string `json:"subject"`
	Predicate   string `json:"predicate"`
	Object      string `json:"object"`
	Reason      string `json:"reason,omitempty"`
	ForgottenAt string `json:"forgottenAt,omitempty"`
}

// writeForgottenRelationsJSONL returns the full forgotten-relations.jsonl
// content: one line per relation that received a RelationForgotten event,
// ULID-sorted.
func writeForgottenRelationsJSONL(state *GraphState) []byte {
	uids := sortedKeys(state.Relations)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	for _, uid := range uids {
		r := state.Relations[uid]
		if !r.Forgotten {
			continue
		}

		jr := forgottenRelationJSONLRow{
			UID:       uid,
			Subject:   r.SubjectUID,
			Predicate: r.Predicate,
			Object:    r.ObjectUID,
			Reason:    r.ForgottenReason,
		}
		if r.ForgottenAt != nil {
			jr.ForgottenAt = r.ForgottenAt.UTC().Format(time.RFC3339Nano)
		}
		encodeJSONL(enc, jr)
	}
	return buf.Bytes()
}
