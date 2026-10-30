package daemonclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// EventRecord is the wire shape returned by the daemon's /v1/events endpoints.
// Field names follow kernel.Event.
type EventRecord struct {
	// Name is the AIP resource name, e.g. "events/{uid}".
	Name string `json:"name"`
	// UID is the stable ULID identifier.
	UID string `json:"uid"`
	// CreateTime is the time the event was appended, in UTC RFC 3339.
	CreateTime time.Time `json:"createTime"`
	// Kind classifies the event payload.
	Kind string `json:"kind"`
	// Source identifies the event origin.
	Source string `json:"source"`
	// Confidence is the writer's stated confidence, 0.0 to 1.0.
	Confidence float64 `json:"confidence"`
	// Scope is the sync/encryption boundary (project|personal|team).
	Scope string `json:"scope,omitempty"`
	// Payload is the kind-specific event body as raw JSON.
	Payload json.RawMessage `json:"payload"`
}

// ListEventsRequest parameterises the list call.
type ListEventsRequest struct {
	// Type filters by event kind when non-empty.
	Type string
	// Since is the inclusive RFC 3339 lower bound on createTime.
	Since string
	// Until is the exclusive RFC 3339 upper bound on createTime.
	Until string
	// PageSize is the maximum number of items to return. Zero uses the server default.
	PageSize int
	// PageToken is the cursor returned by a previous list call.
	PageToken string
}

// ListEventsResponse is the AIP list envelope for events.
type ListEventsResponse struct {
	// Items is the page of events.
	Items []EventRecord `json:"items"`
	// NextPageToken is set when additional pages are available.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// ListEvents calls GET /v1/events and returns the current page.
func (c *Client) ListEvents(ctx context.Context, req ListEventsRequest) (ListEventsResponse, error) {
	q := url.Values{}
	if req.Type != "" {
		q.Set("type", req.Type)
	}
	if req.Since != "" {
		q.Set("since", req.Since)
	}
	if req.Until != "" {
		q.Set("until", req.Until)
	}
	if req.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(req.PageSize))
	}
	if req.PageToken != "" {
		q.Set("pageToken", req.PageToken)
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.Get(ctx, path)
	if err != nil {
		return ListEventsResponse{}, err
	}
	var resp ListEventsResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return ListEventsResponse{}, err
	}
	return resp, nil
}

// StreamEventsFilter parameterises the SSE stream.
type StreamEventsFilter struct {
	// Type filters by event kind when non-empty (empty = all kinds).
	Type string
}

// StreamEvents opens a Server-Sent Events connection to /v1/events:stream
// and returns a channel of decoded EventRecords. The channel is closed when
// ctx is cancelled, the connection is broken, or the server terminates the
// stream. Any parsing error is sent as a zero-value EventRecord with only
// the Kind field set to "stream.error". The returned cancel func must always
// be called to release the underlying connection.
//
// The HTTP request is bound to a derived context so calling cancel actually
// closes the in-flight request — a scanner blocked waiting for the next SSE
// line returns when cancel fires, the body is closed in the goroutine's
// defer chain, and the underlying TCP/Unix connection is released. Before
// this binding, cancel only signalled the goroutine; the HTTP request
// stayed open until the parent ctx died or the server hung up.
func (c *Client) StreamEvents(ctx context.Context, filter StreamEventsFilter) (<-chan EventRecord, context.CancelFunc, error) {
	path := "/v1/events:stream"
	if filter.Type != "" {
		q := url.Values{}
		q.Set("type", filter.Type)
		path += "?" + q.Encode()
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	body, err := c.Stream(streamCtx, path)
	if err != nil {
		cancelStream()
		return nil, func() {}, err
	}
	// cancel must close both the goroutine and the body so a scanner
	// blocked on the next SSE line returns immediately.
	cancel := func() {
		cancelStream()
		_ = body.Close() //nolint:errcheck // best-effort; canceller is non-fatal
	}
	ch := make(chan EventRecord, 64)
	go func() {
		defer close(ch)
		defer body.Close() //nolint:errcheck // deferred close in goroutine; error is unobservable
		if scanErr := sseScanner(streamCtx, body, func(data []byte) error {
			var ev EventRecord
			if err := json.Unmarshal(data, &ev); err != nil {
				// Forward a sentinel so the consumer can see the raw error,
				// then continue scanning (do not abort the stream).
				ch <- EventRecord{Kind: "stream.error", Source: string(data)}
				return nil //nolint:nilerr // deliberate: error forwarded as sentinel event; stream continues
			}
			select {
			case ch <- ev:
				return nil
			case <-streamCtx.Done():
				return fmt.Errorf("daemonclient: stream context cancelled: %w", streamCtx.Err())
			}
		}); scanErr != nil && streamCtx.Err() == nil {
			// Only surface real errors; context cancellation is expected.
			ch <- EventRecord{Kind: "stream.error", Source: scanErr.Error()}
		}
	}()
	return ch, cancel, nil
}
