package daemonclient

import (
	"context"
	"errors"
	"net/url"
)

// ScopeStatus is the wire shape for a sync scope's status, returned by
// GET /v1/scopes.
type ScopeStatus struct {
	// Name is the AIP resource name, e.g. "scopes/personal".
	Name string `json:"name"`
	// Scope is the scope identifier (project|personal|team).
	Scope string `json:"scope"`
	// Mode is "plaintext" or "encrypted".
	Mode string `json:"mode"`
	// RemoteURL is the configured remote URL for this scope.
	RemoteURL string `json:"remoteUrl,omitempty"`
	// LocalCount is the number of chunks in the local FSStore.
	LocalCount int `json:"localCount"`
	// Ahead is the number of local chunks not yet pushed to the remote.
	Ahead int `json:"ahead"`
	// Behind is the number of remote chunks not yet pulled locally.
	Behind int `json:"behind"`
}

// ListScopesResponse is the AIP list envelope for scopes.
type ListScopesResponse struct {
	// Items is the list of configured scopes and their sync status.
	Items []ScopeStatus `json:"items"`
}

// PushResponse is returned by POST /v1/scopes/{name}/push.
type PushResponse struct {
	// Scope is the scope that was pushed.
	Scope string `json:"scope"`
	// Pushed is the number of chunks accepted by the remote.
	Pushed int `json:"pushed"`
}

// PullResponse is returned by POST /v1/scopes/{name}/pull.
type PullResponse struct {
	// Scope is the scope that was pulled.
	Scope string `json:"scope"`
	// Fetched is the number of chunks downloaded from the remote.
	Fetched int `json:"fetched"`
}

// ListScopes calls GET /v1/scopes and returns the sync status of all
// configured scopes.
func (c *Client) ListScopes(ctx context.Context) (ListScopesResponse, error) {
	raw, err := c.Get(ctx, "/v1/scopes")
	if err != nil {
		return ListScopesResponse{}, err
	}
	var resp ListScopesResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return ListScopesResponse{}, err
	}
	return resp, nil
}

// PushScope calls POST /v1/scopes/{name}/push to trigger an immediate push
// for the named scope. The name parameter is the scope identifier (e.g. "personal").
func (c *Client) PushScope(ctx context.Context, name string) (PushResponse, error) {
	if name == "" {
		return PushResponse{}, errors.New("daemonclient: PushScope: name required")
	}
	raw, err := c.Post(ctx, "/v1/scopes/"+url.PathEscape(name)+"/push", nil)
	if err != nil {
		return PushResponse{}, err
	}
	var resp PushResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return PushResponse{}, err
	}
	return resp, nil
}

// PullScope calls POST /v1/scopes/{name}/pull to fetch chunks from the
// remote for the named scope. The name parameter is the scope identifier (e.g. "personal").
func (c *Client) PullScope(ctx context.Context, name string) (PullResponse, error) {
	if name == "" {
		return PullResponse{}, errors.New("daemonclient: PullScope: name required")
	}
	raw, err := c.Post(ctx, "/v1/scopes/"+url.PathEscape(name)+"/pull", nil)
	if err != nil {
		return PullResponse{}, err
	}
	var resp PullResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return PullResponse{}, err
	}
	return resp, nil
}
