package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
)

// v1DefaultPageSize is the number of rows returned when pageSize is not
// specified by the caller.
const v1DefaultPageSize = 50

// v1MaxPageSize caps the maximum page size a caller may request to bound
// memory usage per request.
const v1MaxPageSize = 500

// NewV1Handler builds the http.Handler that serves the /v1/ REST surface.
// It is registered on the MCP server's mux at "/v1/" so both the HTTP
// and Unix-socket transports expose it alongside the JSON-RPC MCP surface.
//
// The handler is thin: it parses query parameters, delegates to the
// provided reader, and encodes responses as JSON. It is read-only: all
// mutations flow through the MCP tool surface or the socket-only /admin/
// write surface (see adminhandler.go). The /admin/ surface is not an MCP
// tool and is not exposed on TCP.
func NewV1Handler(wired *Wired, logger *zap.Logger) (http.Handler, error) {
	if wired == nil {
		return nil, errors.New("daemon: NewV1Handler: wired required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	// Obtain the SQLite reader from the dispatcher. If the orchestrator's
	// storage is not a *dispatcher.Dispatcher (e.g. in tests) we fall back
	// to a nil reader and return 503 from the data endpoints.
	var reader *sql.DB
	var eventLog kernel.EventLog
	var vec *dispatcher.Dispatcher
	if wired.Orchestrator != nil {
		if disp, ok := wired.Orchestrator.Storage.(*dispatcher.Dispatcher); ok {
			r, err := disp.SQLiteReader()
			if err != nil {
				return nil, fmt.Errorf("daemon: v1handler: sqlite reader: %w", err)
			}
			reader = r
			vec = disp
		}
		eventLog = wired.Orchestrator.Log
	}

	h := &v1Handler{
		reader:   reader,
		eventLog: eventLog,
		jobs:     wired.Jobs,
		vec:      vec,
		logger:   logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/entities", h.listEntities)
	mux.HandleFunc("GET /v1/entities/{uid}", h.getEntity)
	mux.HandleFunc("GET /v1/relations", h.listRelations)
	mux.HandleFunc("GET /v1/relations/{uid}", h.getRelation)
	mux.HandleFunc("GET /v1/events", h.listEvents)
	mux.HandleFunc("GET /v1/events:stream", h.streamEvents)
	mux.HandleFunc("GET /v1/episodes", h.listEpisodes)
	mux.HandleFunc("GET /v1/episodes/{uid}", h.getEpisode)
	mux.HandleFunc("GET /v1/jobs", h.listJobs)
	mux.HandleFunc("GET /v1/sources", h.listSources)
	mux.HandleFunc("GET /v1/scopes", h.listScopes)
	mux.HandleFunc("POST /v1/scopes/{name}/push", h.pushScope)
	mux.HandleFunc("POST /v1/scopes/{name}/pull", h.pullScope)
	mux.HandleFunc("GET /v1/schema", h.listSchema)
	return mux, nil
}

// v1Handler holds the shared read dependencies for the /v1 endpoints.
type v1Handler struct {
	reader   *sql.DB
	eventLog kernel.EventLog
	// jobs is the in-process job registry backing GET /v1/jobs. It tracks
	// ingest connector + rebuild runs; nil in tests that omit it.
	jobs *jobs.Registry
	// vec is the dispatcher, used only for its BackgroundVecDepth gauge on
	// GET /v1/jobs. Nil when storage is not the production dispatcher.
	vec    *dispatcher.Dispatcher
	logger *zap.Logger
}

// writeJSON encodes v as JSON to w and sets Content-Type. On encode
// failure it writes a 500 error.
func (h *v1Handler) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Warn("v1: encode response", zap.Error(err))
	}
}

// writeError writes an AIP-193 error envelope.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // best-effort error response
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

// parsePageSize parses and clamps the pageSize query parameter.
func parsePageSize(r *http.Request) int {
	if s := r.URL.Query().Get("pageSize"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			if n > v1MaxPageSize {
				return v1MaxPageSize
			}
			return n
		}
	}
	return v1DefaultPageSize
}

// requireReader returns false and writes 503 when the SQLite reader is nil.
func (h *v1Handler) requireReader(w http.ResponseWriter) bool {
	if h.reader == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"storage reader not available; daemon may be starting")
		return false
	}
	return true
}

// ---------- entity endpoints ----------

// entityRow is the wire shape for entities.
type entityRow struct {
	Name             string  `json:"name"`
	UID              string  `json:"uid"`
	Kind             string  `json:"kind"`
	DisplayName      string  `json:"displayName"`
	Confidence       float64 `json:"confidence"`
	ResolutionStatus string  `json:"resolutionStatus"`
	CreateTime       string  `json:"createTime"`
}

func (h *v1Handler) listEntities(w http.ResponseWriter, r *http.Request) {
	if !h.requireReader(w) {
		return
	}
	ctx := r.Context()
	pageSize := parsePageSize(r)
	kind := r.URL.Query().Get("kind")
	pageToken := r.URL.Query().Get("pageToken")

	offset := 0
	if pageToken != "" {
		if n, err := strconv.Atoi(pageToken); err == nil {
			offset = n
		}
	}

	q := `SELECT uid, kind, display_name, confidence, resolution_status, create_time
	      FROM entities`
	var args []any
	if kind != "" {
		q += " WHERE kind = ?"
		args = append(args, kind)
	}
	q += " ORDER BY create_time DESC LIMIT ? OFFSET ?"
	args = append(args, pageSize+1, offset)

	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		h.logger.Warn("v1: list entities query", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "query failed")
		return
	}
	defer func() { _ = rows.Close() }()

	var items []entityRow
	for rows.Next() {
		var row entityRow
		if err := rows.Scan(&row.UID, &row.Kind, &row.DisplayName,
			&row.Confidence, &row.ResolutionStatus, &row.CreateTime); err != nil {
			h.logger.Warn("v1: scan entity", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "INTERNAL", "scan failed")
			return
		}
		row.Name = "entities/" + row.UID
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		h.logger.Warn("v1: iterate entities", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "iterate failed")
		return
	}

	var nextPageToken string
	if len(items) > pageSize {
		items = items[:pageSize]
		nextPageToken = strconv.Itoa(offset + pageSize)
	}
	if items == nil {
		items = []entityRow{}
	}
	h.writeJSON(w, map[string]any{
		"items":         items,
		"nextPageToken": nextPageToken,
	})
}

func (h *v1Handler) getEntity(w http.ResponseWriter, r *http.Request) {
	if !h.requireReader(w) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "uid required")
		return
	}
	var row entityRow
	err := h.reader.QueryRowContext(r.Context(),
		`SELECT uid, kind, display_name, confidence, resolution_status, create_time
		 FROM entities WHERE uid = ?`, uid,
	).Scan(&row.UID, &row.Kind, &row.DisplayName,
		&row.Confidence, &row.ResolutionStatus, &row.CreateTime)
	switch {
	case err == sql.ErrNoRows:
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("entity %q not found", uid))
	case err != nil:
		h.logger.Warn("v1: get entity", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "query failed")
	default:
		row.Name = "entities/" + row.UID
		h.writeJSON(w, row)
	}
}

// ---------- relation endpoints ----------

// relationRow is the wire shape for relations.
type relationRow struct {
	Name       string  `json:"name"`
	UID        string  `json:"uid"`
	SubjectUID string  `json:"subjectUid"`
	Predicate  string  `json:"predicate"`
	ObjectUID  string  `json:"objectUid"`
	Confidence float64 `json:"confidence"`
	ValidFrom  string  `json:"validFrom,omitempty"`
	ValidUntil string  `json:"validUntil,omitempty"`
	CreateTime string  `json:"createTime"`
}

func (h *v1Handler) listRelations(w http.ResponseWriter, r *http.Request) {
	if !h.requireReader(w) {
		return
	}
	ctx := r.Context()
	pageSize := parsePageSize(r)
	q := r.URL.Query()
	subject := q.Get("subject")
	predicate := q.Get("predicate")
	object := q.Get("object")
	state := q.Get("state")
	pageToken := q.Get("pageToken")

	offset := 0
	if pageToken != "" {
		if n, err := strconv.Atoi(pageToken); err == nil {
			offset = n
		}
	}

	sqlQ := `SELECT uid, subject_uid, predicate, object_uid, confidence,
	              COALESCE(valid_from,''), COALESCE(valid_until,''), create_time
	         FROM relations WHERE 1=1`
	var args []any
	if subject != "" {
		sqlQ += " AND subject_uid = ?"
		args = append(args, subject)
	}
	if predicate != "" {
		sqlQ += " AND predicate = ?"
		args = append(args, predicate)
	}
	if object != "" {
		sqlQ += " AND object_uid = ?"
		args = append(args, object)
	}
	switch state {
	case "open":
		sqlQ += " AND valid_until IS NULL"
	case "closed":
		sqlQ += " AND valid_until IS NOT NULL"
	}
	sqlQ += " ORDER BY create_time DESC LIMIT ? OFFSET ?"
	args = append(args, pageSize+1, offset)

	rows, err := h.reader.QueryContext(ctx, sqlQ, args...)
	if err != nil {
		h.logger.Warn("v1: list relations query", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "query failed")
		return
	}
	defer func() { _ = rows.Close() }()

	var items []relationRow
	for rows.Next() {
		var row relationRow
		if err := rows.Scan(&row.UID, &row.SubjectUID, &row.Predicate, &row.ObjectUID,
			&row.Confidence, &row.ValidFrom, &row.ValidUntil, &row.CreateTime); err != nil {
			h.logger.Warn("v1: scan relation", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "INTERNAL", "scan failed")
			return
		}
		row.Name = "relations/" + row.UID
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		h.logger.Warn("v1: iterate relations", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "iterate failed")
		return
	}

	var nextPageToken string
	if len(items) > pageSize {
		items = items[:pageSize]
		nextPageToken = strconv.Itoa(offset + pageSize)
	}
	if items == nil {
		items = []relationRow{}
	}
	h.writeJSON(w, map[string]any{
		"items":         items,
		"nextPageToken": nextPageToken,
	})
}

func (h *v1Handler) getRelation(w http.ResponseWriter, r *http.Request) {
	if !h.requireReader(w) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "uid required")
		return
	}
	var row relationRow
	err := h.reader.QueryRowContext(r.Context(),
		`SELECT uid, subject_uid, predicate, object_uid, confidence,
		        COALESCE(valid_from,''), COALESCE(valid_until,''), create_time
		 FROM relations WHERE uid = ?`, uid,
	).Scan(&row.UID, &row.SubjectUID, &row.Predicate, &row.ObjectUID,
		&row.Confidence, &row.ValidFrom, &row.ValidUntil, &row.CreateTime)
	switch {
	case err == sql.ErrNoRows:
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("relation %q not found", uid))
	case err != nil:
		h.logger.Warn("v1: get relation", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "query failed")
	default:
		row.Name = "relations/" + row.UID
		h.writeJSON(w, row)
	}
}

// ---------- event endpoints ----------

// eventRow is the wire shape for events.
type eventRow struct {
	Name       string          `json:"name"`
	UID        string          `json:"uid"`
	CreateTime time.Time       `json:"createTime"`
	Kind       string          `json:"kind"`
	Source     string          `json:"source"`
	Confidence float64         `json:"confidence"`
	Scope      string          `json:"scope,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

//nolint:gocognit,gocyclo // sequence of independent query-param parsers + cursor decode + paginated iteration; splitting the params from the loop would obscure the page-token contract
func (h *v1Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	if h.eventLog == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "event log not available")
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	pageSize := parsePageSize(r)

	// Limit is left unset (0 = unbounded) because the cursor-skip
	// below may discard the first N events the iterator yields, and a
	// tight `pageSize+1` limit would then return short pages. We break
	// out of the iterator manually once we have pageSize+1 *kept*
	// rows.
	opts := kernel.ReadOpts{}
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "since: invalid RFC 3339 time")
			return
		}
		opts.Since = t.UTC()
	}
	if s := q.Get("until"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "until: invalid RFC 3339 time")
			return
		}
		opts.Until = t.UTC()
	}
	if s := q.Get("type"); s != "" {
		opts.Kinds = []kernel.EventKind{kernel.EventKind(s)}
	}
	// pageToken encodes the (CreateTime, UID) of the last item from
	// the previous page. eventLog.Read treats Since as INCLUSIVE, so
	// encoding only CreateTime would re-emit the cursor event on the
	// next request — and any events sharing that CreateTime would be
	// ordered non-deterministically across pages. We carry the UID
	// tiebreaker through the cursor and skip events whose
	// (CreateTime, UID) tuple is <= the cursor.
	var cursorTime time.Time
	var cursorUID string
	if token := q.Get("pageToken"); token != "" {
		t, uid, err := decodeEventPageToken(token)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "pageToken: invalid")
			return
		}
		cursorTime = t
		cursorUID = uid
		if cursorTime.After(opts.Since) {
			opts.Since = cursorTime
		}
	}

	items := make([]eventRow, 0, pageSize+1)
	for ev, err := range h.eventLog.Read(ctx, opts) {
		if err != nil {
			h.logger.Warn("v1: read event log", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "INTERNAL", "read failed")
			return
		}
		if !cursorTime.IsZero() && !eventAfterCursor(ev.CreateTime, ev.UID, cursorTime, cursorUID) {
			continue
		}
		items = append(items, eventRow{
			Name:       "events/" + ev.UID,
			UID:        ev.UID,
			CreateTime: ev.CreateTime,
			Kind:       string(ev.Kind),
			Source:     ev.Source,
			Confidence: ev.Confidence,
			Scope:      string(ev.Scope),
			Payload:    ev.Payload,
		})
		if len(items) > pageSize {
			break
		}
	}
	nextPageToken := ""
	if len(items) > pageSize {
		last := items[pageSize-1]
		nextPageToken = encodeEventPageToken(last.CreateTime, last.UID)
		items = items[:pageSize]
	}
	if items == nil {
		items = []eventRow{}
	}
	resp := map[string]any{"items": items}
	if nextPageToken != "" {
		resp["nextPageToken"] = nextPageToken
	}
	h.writeJSON(w, resp)
}

// encodeEventPageToken serialises a (CreateTime, UID) cursor into the
// `nextPageToken` we hand back to the client. RFC3339Nano + literal
// `|` separator keeps the token grep-readable for support without
// requiring a base64 round-trip — the UID is a ULID so it's already
// URL-safe alphanumerics, and the timestamp can't contain `|`.
func encodeEventPageToken(t time.Time, uid string) string {
	return t.UTC().Format(time.RFC3339Nano) + "|" + uid
}

// decodeEventPageToken accepts both the new `<timestamp>|<uid>` format
// and the bare `<timestamp>` format from earlier builds of this PR so
// in-flight clients with a stale token don't see an INVALID_ARGUMENT
// after deploy. A bare timestamp yields an empty UID, which makes the
// "strictly greater than" comparison degenerate into the original
// inclusive-since behaviour for one extra round of duplicates — still
// less wrong than refusing the token outright.
func decodeEventPageToken(token string) (time.Time, string, error) {
	tsPart, uidPart, hasUID := strings.Cut(token, "|")
	t, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("pageToken timestamp: %w", err)
	}
	if !hasUID {
		return t.UTC(), "", nil
	}
	return t.UTC(), uidPart, nil
}

// eventAfterCursor reports whether (evTime, evUID) sorts strictly after
// the cursor under the (CreateTime, UID) lexicographic order the event
// log emits. Used to skip events the previous page already returned.
func eventAfterCursor(evTime time.Time, evUID string, cursorTime time.Time, cursorUID string) bool {
	if evTime.After(cursorTime) {
		return true
	}
	if evTime.Before(cursorTime) {
		return false
	}
	return evUID > cursorUID
}

// streamEvents implements GET /v1/events:stream as a Server-Sent Events
// endpoint. It fans out newly-appended events to connected clients using a
// per-connection buffered channel. When a consumer is too slow, oldest
// events are dropped and a synthetic "dropped: N" line is emitted so the
// consumer can detect loss without breaking the SSE wire protocol.
//
// This implementation polls the event log using a tail-style read loop
// (bounded by ctx). A future revision can replace the poll with a
// notification channel from the FileEventLog's append path.
//
//nolint:gocognit,gocyclo // streaming poll loop with drop/marshal/flush sub-branches; complexity is inherent
func (h *v1Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	if h.eventLog == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "event log not available")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "streaming not supported")
		return
	}

	kindFilter := r.URL.Query().Get("type")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// Tail: read events as they arrive by polling the log with a
	// (Since, UID) cursor that advances after each batch. The cursor
	// tracks the LAST delivered event's UID so a tie on createTime
	// (kernel ULIDs collide at sub-ms granularity under bursty
	// writes) does not re-deliver the same event nor skip a sibling.
	//
	// Bootstrap: read the current log tail before entering the poll
	// loop. Without this, events appended in the millisecond between
	// the daemon's `time.Now()` and the first poll were never
	// delivered — a real silent-loss bug Greptile caught.
	const pollInterval = 500 * time.Millisecond
	var (
		cursor    = time.Time{} // zero ⇒ "read everything since boot" the first time
		cursorUID string
		dropped   int
		hasCursor bool
	)

	// Initial pass: drain the log to "now" with no Since filter and
	// emit everything as a backfill. This bounds the catchup volume
	// to the size of the live JSONL log, which is fine for human-
	// interactive use; agents that don't want backfill can pass
	// ?since=<rfc3339> on the URL.
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			cursor = t.UTC()
			hasCursor = true
		}
	}

	emit := func(ev kernel.Event) {
		data, merr := json.Marshal(eventRow{
			Name:       "events/" + ev.UID,
			UID:        ev.UID,
			CreateTime: ev.CreateTime,
			Kind:       string(ev.Kind),
			Source:     ev.Source,
			Confidence: ev.Confidence,
			Scope:      string(ev.Scope),
			Payload:    ev.Payload,
		})
		if merr != nil {
			h.logger.Warn("v1: stream events marshal", zap.Error(merr))
			return
		}
		//nolint:errcheck // best-effort SSE write; client disconnect handled by flush
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		// Advance cursor strictly forward: same createTime is allowed
		// only when the new UID is lexicographically greater.
		if ev.CreateTime.After(cursor) || (ev.CreateTime.Equal(cursor) && ev.UID > cursorUID) {
			cursor = ev.CreateTime
			cursorUID = ev.UID
			hasCursor = true
		}
	}

	for {
		opts := kernel.ReadOpts{Since: cursor}
		if kindFilter != "" {
			opts.Kinds = []kernel.EventKind{kernel.EventKind(kindFilter)}
		}
		for ev, err := range h.eventLog.Read(ctx, opts) {
			if err != nil {
				h.logger.Warn("v1: stream events read", zap.Error(err))
				break
			}
			// Skip duplicates: if cursor advanced TO this exact
			// (time, uid) on the previous iteration, Since is
			// inclusive and would re-deliver it.
			if hasCursor && ev.CreateTime.Equal(cursor) && ev.UID <= cursorUID {
				continue
			}
			if dropped > 0 {
				//nolint:errcheck // best-effort SSE write; client disconnect handled by flush
				fmt.Fprintf(w, "data: {\"kind\":\"stream.dropped\",\"dropped\":%d}\n\n", dropped)
				dropped = 0
				flusher.Flush()
			}
			emit(ev)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// ---------- episode endpoints ----------

// episodeRow is the wire shape for episodes.
type episodeRow struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	CreateTime string `json:"createTime"`
	Source     string `json:"source"`
	Text       string `json:"text"`
}

func (h *v1Handler) listEpisodes(w http.ResponseWriter, r *http.Request) {
	if !h.requireReader(w) {
		return
	}
	ctx := r.Context()
	pageSize := parsePageSize(r)
	pageToken := r.URL.Query().Get("pageToken")

	offset := 0
	if pageToken != "" {
		if n, err := strconv.Atoi(pageToken); err == nil {
			offset = n
		}
	}

	rows, err := h.reader.QueryContext(ctx,
		`SELECT uid, create_time, source, substr(text, 1, 160)
		 FROM episodes ORDER BY create_time DESC LIMIT ? OFFSET ?`,
		pageSize+1, offset,
	)
	if err != nil {
		h.logger.Warn("v1: list episodes query", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "query failed")
		return
	}
	defer func() { _ = rows.Close() }()

	var items []episodeRow
	for rows.Next() {
		var row episodeRow
		if err := rows.Scan(&row.UID, &row.CreateTime, &row.Source, &row.Text); err != nil {
			h.logger.Warn("v1: scan episode", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "INTERNAL", "scan failed")
			return
		}
		row.Name = "episodes/" + row.UID
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		h.logger.Warn("v1: iterate episodes", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "iterate failed")
		return
	}

	var nextPageToken string
	if len(items) > pageSize {
		items = items[:pageSize]
		nextPageToken = strconv.Itoa(offset + pageSize)
	}
	if items == nil {
		items = []episodeRow{}
	}
	h.writeJSON(w, map[string]any{
		"items":         items,
		"nextPageToken": nextPageToken,
	})
}

func (h *v1Handler) getEpisode(w http.ResponseWriter, r *http.Request) {
	if !h.requireReader(w) {
		return
	}
	uid := r.PathValue("uid")
	if uid == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "uid required")
		return
	}
	var row episodeRow
	err := h.reader.QueryRowContext(r.Context(),
		`SELECT uid, create_time, source, text FROM episodes WHERE uid = ?`, uid,
	).Scan(&row.UID, &row.CreateTime, &row.Source, &row.Text)
	switch {
	case err == sql.ErrNoRows:
		writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("episode %q not found", uid))
	case err != nil:
		h.logger.Warn("v1: get episode", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL", "query failed")
	default:
		row.Name = "episodes/" + row.UID
		h.writeJSON(w, row)
	}
}

// ---------- scope endpoints ----------

func (h *v1Handler) listScopes(w http.ResponseWriter, _ *http.Request) {
	// Scope status requires the sync layer which is outside the kernel
	// reader. Return a 501 placeholder; CLI users still have `maz sync status`.
	// TODO: wire to the sync supervisor's scope state.
	writeError(w, http.StatusNotImplemented, "UNIMPLEMENTED",
		"scope listing not yet wired; use `maz sync status` for now")
}

func (h *v1Handler) pushScope(w http.ResponseWriter, _ *http.Request) {
	// TODO: wire to the sync supervisor push path.
	writeError(w, http.StatusNotImplemented, "UNIMPLEMENTED",
		"scope push not yet wired; use `maz sync push` for now")
}

func (h *v1Handler) pullScope(w http.ResponseWriter, _ *http.Request) {
	// TODO: wire to the sync supervisor pull path.
	writeError(w, http.StatusNotImplemented, "UNIMPLEMENTED",
		"scope pull not yet wired; use `maz sync pull` for now")
}

// ---------- schema endpoint ----------

func (h *v1Handler) listSchema(w http.ResponseWriter, _ *http.Request) {
	// Schema listing is not yet wired; return an empty (well-formed)
	// array so clients still get a valid response.
	h.writeJSON(w, []any{})
}
