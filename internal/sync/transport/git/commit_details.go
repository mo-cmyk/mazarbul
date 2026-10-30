package git

// commit_details.go bridges the raw chunk bytes the transport just
// wrote to disk and the structured commitDetails the message
// builder consumes. The bridge runs ONCE per push, after
// materializeBlobs and before commitAndPush.
//
// Privacy: only plaintext chunks are decoded; encrypted chunks
// contribute to the on-wire tally and nothing else. A scope pushed
// in encrypted mode therefore returns (nil, nil) — buildCommitMessage
// sees the absence and renders the sealed-mode subject.

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// buildPushSummary materialises a pushSummary for the chunks just
// pushed in this commit.
//
// FreshHeads is the subset of blobs that were new on disk (idempotent
// re-pushes are excluded so the commit message matches what `git diff`
// actually shows).
//
// Two passes over the decoded events:
//
//  1. Append-everything into a raw commitDetails so the unstructured
//     event stream becomes typed rows.
//  2. Dedupe entities by UID (a single entity often emits multiple
//     `entity` events as mentions roll in — the renderer cares about
//     the latest state, not the count of mention rollups) and resolve
//     relation subject/object UIDs to display names using the entity
//     map we just built. Display names make `git log` readable; the
//     UIDs are still in the body line for grep.
func buildPushSummary(scope kernel.Scope, deviceID, selfPrincipalUID string, tally pushTally, freshHeads []chunk.Header, chunksDir string) (pushSummary, error) {
	summary := pushSummary{
		Scope:    scope,
		DeviceID: deviceID,
		Tally:    tally,
		Heads:    freshHeads,
	}
	var totalBytes int64
	var details *commitDetails
	sources := map[string]struct{}{}
	peers := map[string]struct{}{}
	for _, h := range freshHeads {
		path := filepath.Join(chunksDir, h.ChunkID+extensionFor(h))
		info, err := os.Stat(path)
		if err == nil {
			totalBytes += info.Size()
		}
		if h.Encrypted {
			continue
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // bounded by chunksDir walk
		if rerr != nil {
			// Best-effort: a missing or corrupt chunk falls through to
			// a degraded "no details" body. Better to commit with a
			// sparser message than to fail the push.
			continue
		}
		_, events, derr := chunk.DecodePlaintext(raw, scope)
		if derr != nil {
			continue
		}
		if details == nil {
			details = &commitDetails{}
		}
		appendEventsToDetails(details, events)
		collectAuthorship(sources, peers, events, selfPrincipalUID)
	}
	if details != nil {
		dedupeAndResolve(details)
	}
	summary.SizeBytes = totalBytes
	summary.Details = details
	summary.Agents = sortedSetKeys(sources)
	summary.peerPrincipalUIDs = sortedSetKeys(peers)
	return summary, nil
}

// collectAuthorship folds one chunk's events into the running sets of distinct
// agent sources and distinct PEER principal UIDs (every authoring principal but
// the committing device's own). Blank values are skipped so a legacy event
// without an Author/Source contributes nothing.
func collectAuthorship(sources, peers map[string]struct{}, events []kernel.Event, selfPrincipalUID string) {
	for i := range events {
		if src := events[i].Source; src != "" {
			sources[src] = struct{}{}
		}
		if p := events[i].Author.PrincipalUID; p != "" && p != selfPrincipalUID {
			peers[p] = struct{}{}
		}
	}
}

// dedupeAndResolve collapses duplicate entity rows and rewrites
// relation rows so subject/object UIDs are followed by their display
// names. Pure transformation; safe to call repeatedly.
func dedupeAndResolve(d *commitDetails) {
	if len(d.Entities) > 1 {
		seen := make(map[string]int, len(d.Entities))
		uniq := d.Entities[:0]
		for _, e := range d.Entities {
			if idx, ok := seen[e.UID]; ok {
				// Last write wins for action/displayName so an
				// "updated"-then-"forgotten" sequence ends as
				// "forgotten" (which actionRank already sorts highest).
				uniq[idx] = e
				continue
			}
			seen[e.UID] = len(uniq)
			uniq = append(uniq, e)
		}
		d.Entities = uniq
	}
	dedupeDocuments(d)
	if len(d.Relations) == 0 || len(d.Entities) == 0 {
		return
	}
	nameByUID := make(map[string]string, len(d.Entities))
	for _, e := range d.Entities {
		if e.DisplayName != "" {
			nameByUID[e.UID] = e.DisplayName
		}
	}
	for i, r := range d.Relations {
		if name, ok := nameByUID[r.SubjectUID]; ok && name != "" {
			d.Relations[i].SubjectUID = name + " (" + shortUID(r.SubjectUID) + ")"
		}
		if name, ok := nameByUID[r.ObjectUID]; ok && name != "" {
			d.Relations[i].ObjectUID = name + " (" + shortUID(r.ObjectUID) + ")"
		}
	}
}

// dedupeDocuments collapses repeated documentChange rows for the same document
// UID (a doc often saves several revisions in one push) to one row, keeping the
// last write so a "+ then -" sequence ends as a deletion (actionRank sorts it
// highest). Pure transformation; safe to call repeatedly.
func dedupeDocuments(d *commitDetails) {
	if len(d.Documents) <= 1 {
		return
	}
	seen := make(map[string]int, len(d.Documents))
	uniq := d.Documents[:0]
	for _, dc := range d.Documents {
		if idx, ok := seen[dc.UID]; ok {
			uniq[idx] = dc
			continue
		}
		seen[dc.UID] = len(uniq)
		uniq = append(uniq, dc)
	}
	d.Documents = uniq
}

// shortUID returns the first 8 characters of a ULID for inline
// labelling. Full UIDs still live in the rendered/by-uid path for
// grep-by-full-UID lookups; the short form is for the human reading
// `git log`.
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// appendEventsToDetails classifies events by ontology kind into the
// four body sections. Kinds outside the user-facing surface (mention,
// entityIdentifierAdded, relationCandidatesProposed, predicateRegistered,
// relationRetracted → tracked as a relation update) are folded into
// the closest semantic bucket; unknown kinds are silently dropped so a
// future ontology addition does not break the commit pipeline.
func appendEventsToDetails(d *commitDetails, events []kernel.Event) {
	for _, ev := range events {
		switch string(ev.Kind) {
		case ontology.KindEpisode:
			d.Episodes = append(d.Episodes, episodeRefFromPayload(ev))
		case ontology.KindEntity:
			d.Entities = append(d.Entities, entityChangeFromPayload(ev, "+"))
		case ontology.KindDocumentRevision:
			d.Documents = append(d.Documents, documentChangeFromPayload(ev))
		case ontology.KindDocumentDeleted:
			d.Documents = append(d.Documents, documentDeletedFromPayload(ev))
		case ontology.KindRelation:
			d.Relations = append(d.Relations, relationChangeFromPayload(ev, "+"))
		case ontology.KindRelationRetracted:
			d.Relations = append(d.Relations, relationRetractionFromPayload(ev))
		case ontology.KindEpisodeForgotten:
			d.Forgets = append(d.Forgets, forgetRef{Target: forgetTargetFromPayload(ev, "episodeUid")})
		case ontology.KindRelationForgotten:
			d.Forgets = append(d.Forgets, forgetRef{Target: forgetTargetFromPayload(ev, "relationUid")})
		}
	}
}

func episodeRefFromPayload(ev kernel.Event) episodeRef {
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(ev.Payload, &p) //nolint:errcheck // best-effort decode; missing fields fall through to zero values
	return episodeRef{UID: ev.UID, Snippet: p.Text}
}

func entityChangeFromPayload(ev kernel.Event, action string) entityChange {
	var p struct {
		UID         string `json:"uid"`
		Kind        string `json:"kind"`
		DisplayName string `json:"displayName"`
	}
	_ = json.Unmarshal(ev.Payload, &p) //nolint:errcheck // best-effort decode; missing fields fall through to zero values
	uid := p.UID
	if uid == "" {
		uid = ev.UID
	}
	return entityChange{UID: uid, Kind: p.Kind, DisplayName: p.DisplayName, Action: action}
}

// documentChangeFromPayload decodes a documentRevision event. The action is
// "+" for a first revision and "~" for a revision that chains off a prior one
// (PreviousRevisionUid set), mirroring how a doc reads in `git log`.
func documentChangeFromPayload(ev kernel.Event) documentChange {
	var p struct {
		DocumentUID         string  `json:"documentUid"`
		Path                string  `json:"path"`
		Title               string  `json:"title"`
		PreviousRevisionUID *string `json:"previousRevisionUid"`
	}
	_ = json.Unmarshal(ev.Payload, &p) //nolint:errcheck // best-effort decode; absent fields stay zero-valued (zero values are the correct fallback)
	uid := p.DocumentUID
	if uid == "" {
		uid = ev.UID
	}
	action := "+"
	if p.PreviousRevisionUID != nil && *p.PreviousRevisionUID != "" {
		action = "~"
	}
	return documentChange{UID: uid, Path: p.Path, Title: p.Title, Action: action}
}

// documentDeletedFromPayload decodes a documentDeleted tombstone event.
func documentDeletedFromPayload(ev kernel.Event) documentChange {
	var p struct {
		DocumentUID string `json:"documentUid"`
		Path        string `json:"path"`
	}
	_ = json.Unmarshal(ev.Payload, &p) //nolint:errcheck // best-effort decode; absent fields stay zero-valued (zero values are the correct fallback)
	uid := p.DocumentUID
	if uid == "" {
		uid = ev.UID
	}
	return documentChange{UID: uid, Path: p.Path, Action: "-"}
}

func relationChangeFromPayload(ev kernel.Event, action string) relationChange {
	var p struct {
		SubjectUID string  `json:"subjectUid"`
		Predicate  string  `json:"predicate"`
		ObjectUID  string  `json:"objectUid"`
		Confidence float64 `json:"confidence"`
	}
	_ = json.Unmarshal(ev.Payload, &p) //nolint:errcheck // best-effort decode; missing fields fall through to zero values
	return relationChange{
		SubjectUID: p.SubjectUID, Predicate: p.Predicate, ObjectUID: p.ObjectUID,
		Confidence: p.Confidence, Action: action,
	}
}

// relationRetractionFromPayload decodes a relationRetracted event for the
// commit audit trail. A retraction payload carries only the closed relation UID
// and its replacement — NOT subject/predicate/object — so relationChangeFromPayload
// would record an empty "~  --[]-->   (conf 0.00)" line. Instead surface the two
// real IDs directly: the rendered line reads
// "~ <relationUid> --[superseded_by]--> <supersededByUid>", identifying which
// arm was closed and what replaced it. Confidence comes from the event envelope
// (resolver emits retractions at 1.0).
func relationRetractionFromPayload(ev kernel.Event) relationChange {
	var p struct {
		RelationUID     string `json:"relationUid"`
		SupersededByUID string `json:"supersededByUid"`
	}
	_ = json.Unmarshal(ev.Payload, &p) //nolint:errcheck // best-effort decode; missing fields fall through to zero values
	return relationChange{
		SubjectUID: p.RelationUID,
		Predicate:  "superseded_by",
		ObjectUID:  p.SupersededByUID,
		Confidence: ev.Confidence,
		Action:     "~",
	}
}

func forgetTargetFromPayload(ev kernel.Event, field string) string {
	m := map[string]any{}
	_ = json.Unmarshal(ev.Payload, &m) //nolint:errcheck // best-effort decode; missing fields fall through to zero values
	if v, ok := m[field].(string); ok && v != "" {
		return v
	}
	return "events/" + ev.UID
}
