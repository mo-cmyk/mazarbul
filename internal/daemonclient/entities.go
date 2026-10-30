package daemonclient

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// Entity is the wire shape returned by the daemon's /v1/entities endpoints.
type Entity struct {
	// Name is the AIP resource name, e.g. "entities/{uid}".
	Name string `json:"name"`
	// UID is the stable ULID identifier.
	UID string `json:"uid"`
	// Kind classifies the entity (person, organization, place, ...).
	Kind string `json:"kind"`
	// DisplayName is the human-readable canonical name.
	DisplayName string `json:"displayName"`
	// Confidence is the writer's stated confidence, 0.0 to 1.0.
	Confidence float64 `json:"confidence"`
	// ResolutionStatus is the current resolver state.
	ResolutionStatus string `json:"resolutionStatus"`
	// CreateTime is the time the entity was first observed.
	CreateTime string `json:"createTime"`
}

// ListEntitiesRequest parameterises the list call.
type ListEntitiesRequest struct {
	// Kind filters by entity kind when non-empty.
	Kind string
	// Scope filters by kernel scope (project|personal|team) when non-empty.
	Scope string
	// PageSize is the maximum number of items to return. Zero uses the server default.
	PageSize int
	// PageToken is the cursor returned by a previous list call.
	PageToken string
}

// ListEntitiesResponse is the AIP list envelope for entities.
type ListEntitiesResponse struct {
	// Items is the page of entities.
	Items []Entity `json:"items"`
	// NextPageToken is set when additional pages are available.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// ListEntities calls GET /v1/entities and returns the current page.
func (c *Client) ListEntities(ctx context.Context, req ListEntitiesRequest) (ListEntitiesResponse, error) {
	q := url.Values{}
	if req.Kind != "" {
		q.Set("kind", req.Kind)
	}
	if req.Scope != "" {
		q.Set("scope", req.Scope)
	}
	if req.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(req.PageSize))
	}
	if req.PageToken != "" {
		q.Set("pageToken", req.PageToken)
	}
	path := "/v1/entities"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.Get(ctx, path)
	if err != nil {
		return ListEntitiesResponse{}, err
	}
	var resp ListEntitiesResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return ListEntitiesResponse{}, err
	}
	return resp, nil
}

// GetEntity calls GET /v1/entities/{uid} and returns the entity.
// The uid parameter is the ULID portion of the resource name, e.g. "01HNXYZ...".
func (c *Client) GetEntity(ctx context.Context, uid string) (Entity, error) {
	if uid == "" {
		return Entity{}, errors.New("daemonclient: GetEntity: uid required")
	}
	raw, err := c.Get(ctx, "/v1/entities/"+url.PathEscape(uid))
	if err != nil {
		return Entity{}, err
	}
	var e Entity
	if err := decodeJSON(raw, &e); err != nil {
		return Entity{}, err
	}
	return e, nil
}
