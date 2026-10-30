package daemonclient

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// Relation is the wire shape returned by the daemon's /v1/relations endpoints.
type Relation struct {
	// Name is the AIP resource name, e.g. "relations/{uid}".
	Name string `json:"name"`
	// UID is the stable ULID identifier.
	UID string `json:"uid"`
	// SubjectUID is the UID of the subject entity.
	SubjectUID string `json:"subjectUid"`
	// Predicate is the relationship type (e.g. "WORKS_AT", "KNOWS").
	Predicate string `json:"predicate"`
	// ObjectUID is the UID of the object entity.
	ObjectUID string `json:"objectUid"`
	// Confidence is the writer's stated confidence, 0.0 to 1.0.
	Confidence float64 `json:"confidence"`
	// ValidFrom is the RFC 3339 start of the relation's validity window (may be empty).
	ValidFrom string `json:"validFrom,omitempty"`
	// ValidUntil is the RFC 3339 end of the validity window (empty = still open).
	ValidUntil string `json:"validUntil,omitempty"`
	// CreateTime is the time the relation was first observed.
	CreateTime string `json:"createTime"`
}

// ListRelationsRequest parameterises the list call.
type ListRelationsRequest struct {
	// Subject filters by subject entity UID when non-empty.
	Subject string
	// Predicate filters by predicate string when non-empty.
	Predicate string
	// Object filters by object entity UID when non-empty.
	Object string
	// State filters by "open" or "closed" (valid_until NULL vs non-NULL) when non-empty.
	State string
	// PageSize is the maximum number of items to return. Zero uses the server default.
	PageSize int
	// PageToken is the cursor returned by a previous list call.
	PageToken string
}

// ListRelationsResponse is the AIP list envelope for relations.
type ListRelationsResponse struct {
	// Items is the page of relations.
	Items []Relation `json:"items"`
	// NextPageToken is set when additional pages are available.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// ListRelations calls GET /v1/relations and returns the current page.
//
//nolint:gocritic // hugeParam: ListRelationsRequest is a value type; pointer would complicate the call sites
func (c *Client) ListRelations(ctx context.Context, req ListRelationsRequest) (ListRelationsResponse, error) {
	q := url.Values{}
	if req.Subject != "" {
		q.Set("subject", req.Subject)
	}
	if req.Predicate != "" {
		q.Set("predicate", req.Predicate)
	}
	if req.Object != "" {
		q.Set("object", req.Object)
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	if req.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(req.PageSize))
	}
	if req.PageToken != "" {
		q.Set("pageToken", req.PageToken)
	}
	path := "/v1/relations"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.Get(ctx, path)
	if err != nil {
		return ListRelationsResponse{}, err
	}
	var resp ListRelationsResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return ListRelationsResponse{}, err
	}
	return resp, nil
}

// GetRelation calls GET /v1/relations/{uid} and returns the relation.
func (c *Client) GetRelation(ctx context.Context, uid string) (Relation, error) {
	if uid == "" {
		return Relation{}, errors.New("daemonclient: GetRelation: uid required")
	}
	raw, err := c.Get(ctx, "/v1/relations/"+url.PathEscape(uid))
	if err != nil {
		return Relation{}, err
	}
	var r Relation
	if err := decodeJSON(raw, &r); err != nil {
		return Relation{}, err
	}
	return r, nil
}
