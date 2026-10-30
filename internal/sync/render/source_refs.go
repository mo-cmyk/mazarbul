package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// sourceRefJSONLRow is the stable wire shape for one line in source-refs.jsonl.
// It is the PLAINTEXT projection: external_url and dedup_key are dropped and
// external_id is carried only as its SHA-256 (externalIdHash). Field order here
// is the emitted key order. Encoded via json.Marshal (not fmt %q) so control
// characters in the free-text fields produce valid JSON \u00NN escapes.
type sourceRefJSONLRow struct {
	UID            string `json:"uid"`
	SubjectUID     string `json:"subjectUid"`
	SourceType     string `json:"sourceType"`
	Connector      string `json:"connector"`
	Stream         string `json:"stream"`
	ExternalIDHash string `json:"externalIdHash"`
	ContentHash    string `json:"contentHash"`
	FetchTime      string `json:"fetchTime"`
	CreateTime     string `json:"createTime"`
}

// SourceRefRow holds one external-source provenance record in render state. It
// retains the raw external_id and external_url verbatim; the plaintext redaction
// (#207 render rule) happens at emit time in writeSourceRefsJSONL, not here, so
// the transform is explicit and testable.
type SourceRefRow struct {
	UID         string
	SubjectUID  string
	SourceType  string
	Connector   string
	Stream      string
	ExternalID  string
	ExternalURL string
	ContentHash string
	FetchTime   time.Time
	DedupKey    string
	CreateTime  time.Time
}

// applySourceRefRecorded folds a sourceRefRecorded event into render state. The
// row is keyed in the SourceRefs map by the dedupKey-derived uid with
// last-write-wins semantics (NOT appended in event order), mirroring the
// source_refs UNIQUE(dedup_key) upsert in the dispatcher. See the
// GraphState.SourceRefs field comment for the identity model.
func (g *GraphState) applySourceRefRecorded(ev kernel.Event) (bool, error) {
	var payload ontology.SourceRefRecordedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal sourceRefRecorded: %w", err)
	}

	// uid is derived from dedupKey (matching the dispatcher projection), not
	// trusted from payload.UID — so the rendered uid is the authoritative
	// SourceRef identity regardless of what a producer stamped. Keying the map by
	// it means a re-recorded dedup_key overwrites in place (last-write-wins),
	// exactly like the source_refs UNIQUE(dedup_key) upsert.
	uid := string(ontology.SourceRefUID(payload.DedupKey))
	g.SourceRefs[uid] = &SourceRefRow{
		UID:         uid,
		SubjectUID:  string(payload.SubjectUID),
		SourceType:  payload.SourceType,
		Connector:   payload.Connector,
		Stream:      payload.Stream,
		ExternalID:  payload.ExternalID,
		ExternalURL: payload.ExternalURL,
		ContentHash: payload.ContentHash,
		FetchTime:   payload.FetchTime.UTC(),
		DedupKey:    payload.DedupKey,
		CreateTime:  ev.CreateTime.UTC(),
	}
	g.Cursor = ev.UID
	return true, nil
}

// writeSourceRefsJSONL renders per-scope source-refs.jsonl. Returns nil when
// empty.
//
// #207 plaintext render rule (building on #204's plaintext/encrypted boundary):
// external_url is OMITTED entirely and external_id is replaced by its SHA-256
// hash (externalIdHash). The renderer only ever runs on the plaintext push path
// — writeRenderedView prunes the rendered/ tree for encrypted scopes before
// Render is ever called — so this redaction is UNCONDITIONAL: there is no
// plaintext-vs-encrypted branch to get wrong, and a future caller cannot route
// an encrypted scope through here without the redaction.
//
// Dedup_key is ALSO omitted: it is an internal idempotency key for the
// source_refs projection (which is rebuilt from the event log, not from this
// informational git file), and connectors commonly derive it from the external
// id (e.g. "gmail:<messageId>"). Emitting it verbatim would re-leak the very
// identifier external_id was just hashed to protect.
func writeSourceRefsJSONL(state *GraphState) []byte {
	if len(state.SourceRefs) == 0 {
		return nil
	}
	sorted := make([]*SourceRefRow, 0, len(state.SourceRefs))
	for _, s := range state.SourceRefs {
		sorted = append(sorted, s)
	}
	// uids are unique map keys, so this total order is deterministic.
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UID < sorted[j].UID })

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, s := range sorted {
		var externalIDHash string
		if s.ExternalID != "" {
			externalIDHash = hashExternalID(s.ExternalID)
		}
		encodeJSONL(enc, sourceRefJSONLRow{
			UID:            s.UID,
			SubjectUID:     s.SubjectUID,
			SourceType:     s.SourceType,
			Connector:      s.Connector,
			Stream:         s.Stream,
			ExternalIDHash: externalIDHash,
			ContentHash:    s.ContentHash,
			FetchTime:      s.FetchTime.UTC().Format(time.RFC3339Nano),
			CreateTime:     s.CreateTime.UTC().Format(time.RFC3339Nano),
		})
	}
	return buf.Bytes()
}

// hashExternalID returns the lower-hex SHA-256 of an external identifier. SHA-256
// is chosen because the hash is a PRIVACY redaction (irreversible, so the raw
// source-system id never reaches plaintext git) — distinct from the
// non-cryptographic MD5 dedup key in the facts dispatcher.
func hashExternalID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
