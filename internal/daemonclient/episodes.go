package daemonclient

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// Episode is the wire shape returned by the daemon's /v1/episodes endpoints.
type Episode struct {
	// Name is the AIP resource name, e.g. "episodes/{uid}".
	Name string `json:"name"`
	// UID is the stable ULID identifier.
	UID string `json:"uid"`
	// CreateTime is the time the episode was appended, in UTC RFC 3339.
	CreateTime string `json:"createTime"`
	// Source identifies the episode origin.
	Source string `json:"source"`
	// Text is the episode content, truncated for list responses.
	Text string `json:"text"`
}

// ListEpisodesRequest parameterises the list call.
type ListEpisodesRequest struct {
	// PageSize is the maximum number of items to return. Zero uses the server default.
	PageSize int
	// PageToken is the cursor returned by a previous list call.
	PageToken string
}

// ListEpisodesResponse is the AIP list envelope for episodes.
type ListEpisodesResponse struct {
	// Items is the page of episodes.
	Items []Episode `json:"items"`
	// NextPageToken is set when additional pages are available.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// ListEpisodes calls GET /v1/episodes and returns the current page.
func (c *Client) ListEpisodes(ctx context.Context, req ListEpisodesRequest) (ListEpisodesResponse, error) {
	q := url.Values{}
	if req.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(req.PageSize))
	}
	if req.PageToken != "" {
		q.Set("pageToken", req.PageToken)
	}
	path := "/v1/episodes"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.Get(ctx, path)
	if err != nil {
		return ListEpisodesResponse{}, err
	}
	var resp ListEpisodesResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return ListEpisodesResponse{}, err
	}
	return resp, nil
}

// GetEpisode calls GET /v1/episodes/{uid} and returns the episode.
func (c *Client) GetEpisode(ctx context.Context, uid string) (Episode, error) {
	if uid == "" {
		return Episode{}, errors.New("daemonclient: GetEpisode: uid required")
	}
	raw, err := c.Get(ctx, "/v1/episodes/"+url.PathEscape(uid))
	if err != nil {
		return Episode{}, err
	}
	var ep Episode
	if err := decodeJSON(raw, &ep); err != nil {
		return Episode{}, err
	}
	return ep, nil
}
