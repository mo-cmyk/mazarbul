package render

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"
)

// relationJSONLRow is the stable wire shape for one line in relations.jsonl.
type relationJSONLRow struct {
	UID        string            `json:"uid"`
	Subject    string            `json:"subject"`
	Predicate  string            `json:"predicate"`
	Object     string            `json:"object"`
	Since      string            `json:"since,omitempty"`
	Confidence float64           `json:"confidence"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// writeRelationsJSONL returns bytes where each line is a JSON object for one
// currently-valid (not superseded, not forgotten) relation, ULID-sorted.
func writeRelationsJSONL(state *GraphState) []byte {
	uids := sortedKeys(state.Relations)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	for _, uid := range uids {
		r := state.Relations[uid]
		if r.SupersededByUID != "" || r.Forgotten {
			continue
		}

		jr := relationJSONLRow{
			UID:        uid,
			Subject:    r.SubjectUID,
			Predicate:  r.Predicate,
			Object:     r.ObjectUID,
			Confidence: r.Confidence,
		}
		if r.ValidFrom != nil {
			jr.Since = r.ValidFrom.UTC().Format(time.RFC3339Nano)
		}
		if len(r.Attributes) > 0 {
			jr.Attributes = sortedAttributes(r.Attributes)
		}
		encodeJSONL(enc, jr)
	}
	return buf.Bytes()
}

// sortedAttributes returns a new map with the same key/value pairs, with
// keys iterated in sorted order when marshalled by encoding/json.
func sortedAttributes(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}
