package daemon

// adminhandler.go implements the privileged, unix-socket-only write surface
// for the Mazarbul daemon. It is mounted at "/admin/" ONLY on the Unix
// transport (see internal/mcp/server.go buildHandler); the route is
// structurally absent from the TCP transport so no runtime check is needed.
//
// Part B (rebuild / reindex) will add routes here:
//
//	POST /admin/v1/rebuild  → adminHandler.triggerRebuild
//	POST /admin/v1/reindex  → adminHandler.triggerReindex
//
// Both routes share the same mux created in NewAdminHandler. The handler
// struct carries the deps both A and B need: eventLog, reader (for
// materialization checks), and logger. B will add a jobRegistry field.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
)

// adminBodyLimit caps the request body at 512 KiB so a misbehaving client
// cannot allocate unbounded memory server-side.
const adminBodyLimit = 512 * 1024

// AppendEventsRequest is the wire shape for POST /admin/v1/events:append.
// The CLI builds events with empty UID/CreateTime/Author; FileEventLog.Append
// → prepare() stamps the ULID, time, default scope, and the daemon's ed25519
// signature server-side — callers cannot forge a principal.
type AppendEventsRequest struct {
	// Events is the batch to append. Must be non-empty.
	Events []kernel.Event `json:"events"`
	// Precheck holds materialization checks evaluated BEFORE the append.
	// If all present-checks are already satisfied and no absent-checks fail,
	// the handler returns alreadyPresent:true and does NOT append (atomic
	// idempotency, no TOCTOU). Evaluated against the dispatcher's read-only
	// SQLite connection.
	Precheck []MatCheckWire `json:"precheck,omitempty"`
	// Checks holds materialization checks evaluated AFTER the append to
	// confirm the batch landed in the projection. A failing check returns
	// 422 with an actionable rebuild hint.
	Checks []MatCheckWire `json:"checks,omitempty"`
}

// MatCheckWire is one read-back probe. The daemon runs the same SQL that the
// CLI's confirmMaterialised / confirmAbsent run in-process, so the two paths
// cannot diverge.
type MatCheckWire struct {
	// Query is a COUNT(*) SQL query returning exactly one integer.
	Query string `json:"query"`
	// Args are the positional query arguments.
	Args []any `json:"args"`
	// Label is a human-readable name for the check used in error messages.
	Label string `json:"label"`
	// Confirm controls what the daemon asserts: "present" requires count>0;
	// "absent" requires count==0.
	Confirm string `json:"confirm"`
}

// AppendEventsResponse is the wire shape returned by POST /admin/v1/events:append.
type AppendEventsResponse struct {
	// Receipts is one entry per appended event with its stamped UID and
	// CreateTime.
	Receipts []ReceiptWire `json:"receipts"`
	// AlreadyPresent is true when all Precheck "present" conditions were met
	// before the append; the batch was NOT appended.
	AlreadyPresent bool `json:"alreadyPresent,omitempty"`
}

// ReceiptWire is the JSON-serialisable form of kernel.Receipt.
type ReceiptWire struct {
	// UID is the ULID assigned to this event by Append → prepare.
	UID string `json:"uid"`
}

// adminHandler holds the dependencies for the /admin/ surface.
type adminHandler struct {
	eventLog kernel.EventLog
	reader   *sql.DB
	logger   *zap.Logger

	// coord drives online rebuild/reindex. When nil the rebuild/reindex/status
	// routes are not registered (the append-only test constructor leaves it nil).
	coord *rebuildCoordinator
	// wired is the live wiring a triggered rebuild reads the event log + embedder
	// from. Nil alongside coord.
	wired *Wired
	// dataDir is where the shadow build is staged and the swap lands.
	dataDir string
}

// registerRoutes builds the admin mux. The append route is always present; the
// rebuild/reindex/status routes register only when a coordinator is wired.
func (h *adminHandler) registerRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/v1/events:append", h.appendEvents)
	if h.coord != nil {
		mux.HandleFunc("POST /admin/v1/rebuild", h.triggerRebuild)
		mux.HandleFunc("POST /admin/v1/reindex", h.triggerReindex)
		mux.HandleFunc("GET /admin/v1/rebuild", h.rebuildStatus)
	}
	return mux
}

// NewAdminHandlerDirect constructs the admin handler by injecting the event
// log and SQLite reader directly. This is the test-friendly constructor for the
// append path: tests that don't have a full *Wired supply fakes for the two
// deps. It leaves the coordinator nil, so the rebuild routes are absent.
// Production code uses NewAdminHandler.
func NewAdminHandlerDirect(eventLog kernel.EventLog, reader *sql.DB, logger *zap.Logger) (http.Handler, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	h := &adminHandler{
		eventLog: eventLog,
		reader:   reader,
		logger:   logger,
	}
	return h.registerRoutes(), nil
}

// NewAdminHandler constructs the http.Handler for the admin write surface.
// It mirrors NewV1Handler's wired-dependency acquisition pattern.
//
// This handler must be mounted ONLY on the Unix-socket transport (see
// mcp.Options.AdminRoutes and buildHandler). It is structurally absent from
// the TCP transport; no runtime transport check is performed.
//
// Coord drives online rebuild/reindex; it outlives serving-stack reloads and is
// created once in RunSupervisor. DataDir is where the shadow build is staged.
func NewAdminHandler(wired *Wired, coord *rebuildCoordinator, dataDir string, logger *zap.Logger) (http.Handler, error) {
	if wired == nil {
		return nil, errors.New("daemon: NewAdminHandler: wired required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	var reader *sql.DB
	var eventLog kernel.EventLog
	if wired.Orchestrator != nil {
		if disp, ok := wired.Orchestrator.Storage.(*dispatcher.Dispatcher); ok {
			r, err := disp.SQLiteReader()
			if err != nil {
				return nil, fmt.Errorf("daemon: adminhandler: sqlite reader: %w", err)
			}
			reader = r
		}
		eventLog = wired.Orchestrator.Log
	}

	h := &adminHandler{
		eventLog: eventLog,
		reader:   reader,
		logger:   logger,
		coord:    coord,
		wired:    wired,
		dataDir:  dataDir,
	}
	return h.registerRoutes(), nil
}

// rebuildBodyLimit caps the tiny rebuild/reindex request body ({"strict":bool}).
const rebuildBodyLimit = 4 * 1024

// triggerRebuild handles POST /admin/v1/rebuild — a full online rebuild.
func (h *adminHandler) triggerRebuild(w http.ResponseWriter, r *http.Request) {
	h.trigger(w, r, rebuildKindRebuild)
}

// triggerReindex handles POST /admin/v1/reindex — re-embed everything by
// replaying the log into a fresh projection (the rebuild path regenerates all
// vectors with the current embedder).
func (h *adminHandler) triggerReindex(w http.ResponseWriter, r *http.Request) {
	h.trigger(w, r, rebuildKindReindex)
}

// trigger starts an online rebuild/reindex and returns 202 {jobUid}. A second
// trigger while one runs returns 409. Progress is polled via GET on the same
// path; the coordinator's status survives the swap's reload so the CLI can
// re-dial and read the terminal state.
func (h *adminHandler) trigger(w http.ResponseWriter, r *http.Request, kind rebuildKind) {
	if h.coord == nil || h.wired == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"rebuild not available; daemon may be starting")
		return
	}
	// Optional body {"strict":bool}; default strict=false (collect + continue).
	r.Body = http.MaxBytesReader(w, r.Body, rebuildBodyLimit)
	var body struct {
		Strict bool `json:"strict"`
	}
	if raw, rerr := io.ReadAll(r.Body); rerr == nil && len(raw) > 0 {
		//nolint:errcheck // malformed body falls back to strict=false
		_ = json.Unmarshal(raw, &body)
	}

	jobUID, started := h.coord.start(h.wired, h.dataDir, kind, body.Strict)
	if !started {
		writeError(w, http.StatusConflict, "ABORTED",
			"a rebuild or reindex is already running")
		return
	}
	h.logger.Info("Admin rebuild triggered",
		zap.String("kind", string(kind)),
		zap.String("jobUid", jobUID),
		zap.Bool("strict", body.Strict),
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	//nolint:errcheck // best-effort response encode
	_ = json.NewEncoder(w).Encode(map[string]any{"jobUid": jobUID, "kind": string(kind)})
}

// rebuildStatus handles GET /admin/v1/rebuild — the authoritative online-rebuild
// status (survives the swap's serving-stack reload).
func (h *adminHandler) rebuildStatus(w http.ResponseWriter, _ *http.Request) {
	if h.coord == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"rebuild not available; daemon may be starting")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	//nolint:errcheck // best-effort response encode
	_ = json.NewEncoder(w).Encode(h.coord.statusView())
}

// appendEvents handles POST /admin/v1/events:append.
//
// Security guards applied here (defense-in-depth on top of the structural
// Unix-socket-only mount):
//  1. Body size capped at adminBodyLimit (512 KiB).
//  2. Empty batch → 400.
//  3. Any event with scope != personal → 400.
//  4. Audit-log at Info: batch size + event kinds (NOT payloads).
//
// Source is deliberately NOT clamped or allowlisted: the in-scope operations
// carry legitimate caller-set sources (preset install → "schemapack:<pack>";
// load → the original sources of a re-imported backup, which are arbitrary),
// so an allowlist would both break those flows and diverge from the in-process
// Append path. The trustworthy attribution is EventAuthor, which the kernel's
// prepare() re-signs server-side (callers cannot forge a principal); Source is
// informational metadata only. Forging it grants nothing a same-UID local peer
// could not already do by running `maz` directly (see the threat model).
func (h *adminHandler) appendEvents(w http.ResponseWriter, r *http.Request) {
	if h.eventLog == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"event log not available; daemon may be starting")
		return
	}

	req, ok := h.decodeRequest(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	if done := h.runPrecheck(ctx, w, req.Precheck); done {
		return
	}

	receipts, err := h.eventLog.Append(ctx, req.Events)
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				fmt.Sprintf("invalid event: %v", err))
			return
		}
		h.logger.Error("Admin append: event log append failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "INTERNAL",
			fmt.Sprintf("append failed: %v", err))
		return
	}

	if !h.runPostChecks(ctx, w, req.Checks) {
		return
	}

	resp := AppendEventsResponse{Receipts: make([]ReceiptWire, len(receipts))}
	for i, rc := range receipts {
		resp.Receipts[i] = ReceiptWire{UID: rc.UID}
	}
	w.Header().Set("Content-Type", "application/json")
	//nolint:errcheck // best-effort response encode
	_ = json.NewEncoder(w).Encode(resp)
}

// decodeRequest reads and validates the request body, returning the parsed
// AppendEventsRequest and true on success, or writing an error response and
// returning false on any validation failure.
func (h *adminHandler) decodeRequest(w http.ResponseWriter, r *http.Request) (AppendEventsRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, adminBodyLimit)
	// Read the entire body first so that MaxBytesReader fires on oversize
	// payloads before we hand off to the JSON decoder. If the decoder saw bytes
	// before the limit was reached (e.g. a JSON syntax error in the first few
	// bytes of a 600 KiB body), it would return a *json.SyntaxError instead of
	// *http.MaxBytesError, making the size guard invisible.
	rawBody, rerr := io.ReadAll(r.Body)
	if rerr != nil {
		var mbe *http.MaxBytesError
		if errors.As(rerr, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "RESOURCE_EXHAUSTED",
				"request body exceeds 512 KiB limit")
			return AppendEventsRequest{}, false
		}
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("read request body: %v", rerr))
		return AppendEventsRequest{}, false
	}
	var req AppendEventsRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("decode request: %v", err))
		return AppendEventsRequest{}, false
	}
	if len(req.Events) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"events batch must not be empty")
		return AppendEventsRequest{}, false
	}
	if msg := validateEvents(req.Events); msg != "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", msg)
		return AppendEventsRequest{}, false
	}
	// Audit log: batch size and kinds only, never payloads.
	kinds := make([]string, len(req.Events))
	for i := range req.Events {
		kinds[i] = string(req.Events[i].Kind)
	}
	h.logger.Info("Admin append request",
		zap.Int("batchSize", len(req.Events)),
		zap.Strings("kinds", kinds),
	)
	return req, true
}

// runPrecheck evaluates the pre-append checks. It returns true (done=true) if
// the caller should stop (either because the batch is already present and
// idempotent, or because a check query failed).
func (h *adminHandler) runPrecheck(ctx context.Context, w http.ResponseWriter, precheck []MatCheckWire) (done bool) {
	if len(precheck) == 0 {
		return false
	}
	// runChecks returns (allPass=true, nil) when every check passes, meaning
	// it is SAFE to proceed with the append. If allPass is false the
	// pre-conditions are NOT met — the row is already in the undesired state —
	// so the batch is idempotent: return 200 with alreadyPresent:true.
	allPass, err := h.runChecks(ctx, precheck)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL",
			fmt.Sprintf("precheck failed: %v", err))
		return true
	}
	if !allPass {
		w.Header().Set("Content-Type", "application/json")
		//nolint:errcheck // best-effort response encode
		_ = json.NewEncoder(w).Encode(AppendEventsResponse{AlreadyPresent: true})
		return true
	}
	return false
}

// runPostChecks evaluates post-append materialization checks. It returns true
// when all checks pass (caller should continue), false when any check fails
// (error response already written).
func (h *adminHandler) runPostChecks(ctx context.Context, w http.ResponseWriter, checks []MatCheckWire) bool {
	if len(checks) == 0 {
		return true
	}
	ok, cerr := h.runChecks(ctx, checks)
	if cerr != nil || !ok {
		if cerr != nil {
			h.logger.Warn("Admin append: post-append check error", zap.Error(cerr))
		}
		writeError(w, http.StatusUnprocessableEntity, "FAILED_PRECONDITION",
			"committed but did not materialise; run `maz rebuild --yes`")
		return false
	}
	return true
}

// validateEvents checks each event in the batch for the personal-scope
// constraint. It returns a non-empty error message on the first violation,
// or an empty string when all events are valid.
//
// Source is intentionally not validated here — see appendEvents for why
// (caller-set sources are legitimate and must reach the kernel unchanged for
// parity with the in-process Append path).
//
// Extracted from appendEvents to keep that function's cyclomatic and
// cognitive complexity within lint thresholds.
func validateEvents(events []kernel.Event) string {
	for i := range events {
		// Security guard: reject non-personal scope.
		if events[i].Scope != "" && events[i].Scope != kernel.ScopePersonal {
			return fmt.Sprintf("events[%d]: scope %q not permitted; admin endpoint accepts only personal-scoped events", i, events[i].Scope)
		}
	}
	return ""
}

// runChecks evaluates a slice of MatCheckWire against the dispatcher's
// SQLite reader. It returns (true, nil) when ALL checks pass (present checks
// find rows, absent checks find no rows), (false, nil) when any check fails,
// and (false, err) on a query execution error.
//
// This is the shared evaluator used by both Precheck and Checks, replicating
// the semantics of cli.confirmMaterialised / cli.confirmAbsent so the daemon
// and in-process paths run identical SQL logic.
func (h *adminHandler) runChecks(ctx context.Context, checks []MatCheckWire) (allPass bool, err error) {
	if h.reader == nil {
		return false, errors.New("sqlite reader not available")
	}
	for _, c := range checks {
		var n int
		if qerr := h.reader.QueryRowContext(ctx, c.Query, c.Args...).Scan(&n); qerr != nil {
			return false, fmt.Errorf("check %q: %w", c.Label, qerr)
		}
		switch c.Confirm {
		case "present":
			if n == 0 {
				return false, nil
			}
		case "absent":
			if n != 0 {
				return false, nil
			}
		default:
			return false, fmt.Errorf("check %q: unknown confirm value %q; must be present or absent", c.Label, c.Confirm)
		}
	}
	return true, nil
}
