package navigate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SourceRef is one external-source provenance record read off the source_refs
// table: the connector, stream, and external identity a subject record was
// derived from (W3C PROV wasDerivedFrom), plus a content hash and fetch time.
//
// Provenance carried here is SELF-ASSERTED and UNVERIFIED — there is no
// principal/signature trust for a SourceRef yet (#207). The getSourceRef tool
// stamps that label onto the response; this read struct is the raw projection.
type SourceRef struct {
	UID            string `json:"uid"`
	Name           string `json:"name"` // sourceRefs/{uid}
	SubjectUID     string `json:"subjectUid"`
	SourceType     string `json:"sourceType"`
	Connector      string `json:"connector,omitempty"`
	Stream         string `json:"stream,omitempty"`
	ExternalID     string `json:"externalId,omitempty"`
	ExternalURL    string `json:"externalUrl,omitempty"`
	ContentHash    string `json:"contentHash,omitempty"`
	FetchTime      string `json:"fetchTime"`
	DedupKey       string `json:"dedupKey"`
	SourceEventUID string `json:"sourceEventUid"`
	CreateTime     string `json:"createTime,omitempty"`
	Scope          string `json:"scope,omitempty"`
	// FreshnessToken is the captureExternal HMAC binding (dedupKey + contentHash +
	// summary length). Empty for pull-connector rows. Used by captureExternal to
	// detect a replayed or forged hash on re-capture; not surfaced by getSourceRef.
	FreshnessToken string `json:"-"`
	// Tombstone marks a captured external item the agent reported gone
	// ("deleted") or inaccessible ("access-revoked"); empty for a live snapshot.
	// Read by captureExternal to learn the prior snapshot state on re-capture;
	// recall classifies a tombstoned SourceRef as access-unverified.
	Tombstone string `json:"-"`
}

// sourceRefColumns is the shared SELECT list for the source_refs table, in
// scanSourceRef order. The nullable text columns (connector/stream/external_id/
// external_url/content_hash/freshness_token/tombstone) coalesce to the empty
// string for a uniform wire shape; fetch_time, dedup_key, source_event_uid,
// create_time, and scope are NOT NULL.
const sourceRefColumns = `uid, subject_uid, source_type,
	COALESCE(connector, ''), COALESCE(stream, ''), COALESCE(external_id, ''),
	COALESCE(external_url, ''), COALESCE(content_hash, ''),
	fetch_time, dedup_key, source_event_uid, create_time, scope,
	COALESCE(freshness_token, ''), COALESCE(tombstone, '')`

// scanSourceRef reads one source_refs row in sourceRefColumns order and stamps
// the AIP resource name.
func scanSourceRef(s interface{ Scan(dest ...any) error }) (SourceRef, error) {
	var r SourceRef
	if err := s.Scan(&r.UID, &r.SubjectUID, &r.SourceType,
		&r.Connector, &r.Stream, &r.ExternalID,
		&r.ExternalURL, &r.ContentHash,
		&r.FetchTime, &r.DedupKey, &r.SourceEventUID, &r.CreateTime, &r.Scope,
		&r.FreshnessToken, &r.Tombstone); err != nil {
		return SourceRef{}, err
	}
	r.Name = "sourceRefs/" + r.UID
	return r, nil
}

// GetSourceRef returns the SourceRef with uid, or ErrNotFound. The read is
// scope-isolated: the navigator's scope predicate is ANDed into the query so an
// empty scope set fails closed (#206), exactly like GetFact.
func (n *Navigator) GetSourceRef(ctx context.Context, uid string) (SourceRef, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT ` + sourceRefColumns + ` FROM source_refs WHERE uid = ? AND ` + scope
	r, err := scanSourceRef(n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return SourceRef{}, fmt.Errorf("source ref %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return SourceRef{}, fmt.Errorf("get source ref %q: %w", uid, err)
	}
	return r, nil
}
