package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// docHistoryNavigator is the read surface documentHistory consumes to resolve a
// path/UID argument to a stable document UID. It tries the live documents
// projection first and falls back to the tombstone list so the history of a
// deleted document is still retrievable. Satisfied by *navigate.Navigator.
type docHistoryNavigator interface {
	GetDocumentByPath(ctx context.Context, path string) (navigate.Document, error)
	GetDocumentByUID(ctx context.Context, uid string) (navigate.Document, error)
	DeletedDocuments(ctx context.Context) ([]navigate.Document, error)
}

// docEventReader is the event-log surface documentHistory consumes. The full
// revision chain lives in the append-only log (the documents projection keeps
// only head state), so history reads the log directly. Satisfied by
// *kernel.FileEventLog.
type docEventReader interface {
	Read(ctx context.Context, opts kernel.ReadOpts) iter.Seq2[kernel.Event, error]
}

// docHistoryBodyCap bounds the per-revision body returned when includeBodies is
// set, so a `maz doc diff` over a pathological multi-megabyte revision chain
// cannot blow up the JSON-RPC frame. Diffs of larger bodies are truncated with
// the marker below; the full body is always available via the vault file.
const docHistoryBodyCap = 1 << 20 // 1 MiB

const documentHistoryInputSchema = `{
  "type": "object",
  "properties": {
    "path":          {"type": "string", "description": "Vault-relative path of the document, e.g. notes/project-alpha.md."},
    "documentUid":   {"type": "string", "description": "Stable document UID. Alternative to path."},
    "includeBodies": {"type": "boolean", "description": "When true, include each revision's verbatim body so a caller can diff two revisions. Off by default to keep the listing light."}
  },
  "additionalProperties": false
}`

const documentHistoryOutputSchema = `{
  "type": "object",
  "properties": {
    "documentUid": {"type": "string"},
    "path":        {"type": "string", "description": "The document's current (or last-known) vault path."},
    "revisions":   {"type": "array", "description": "The revision chain in chronological order (oldest first). Each entry is a revision or a deletion tombstone; bodies are present only when includeBodies was set.", "items": {"type": "object"}}
  },
  "required": ["documentUid", "revisions"],
  "additionalProperties": false
}`

// docRevisionWire is one entry in a document's history: a revision or a deletion
// tombstone, identified by Kind.
type docRevisionWire struct {
	UID                 string `json:"uid"`
	CreateTime          string `json:"createTime"`
	Kind                string `json:"kind"` // "revision" | "deleted"
	ContentHash         string `json:"contentHash,omitempty"`
	Title               string `json:"title,omitempty"`
	Bytes               int    `json:"bytes"`
	PreviousRevisionUID string `json:"previousRevisionUid,omitempty"`
	Reason              string `json:"reason,omitempty"` // deletion reason (Kind=deleted)
	Body                string `json:"body,omitempty"`   // only when includeBodies
	// BodyTruncated reports that Body was capped at docHistoryBodyCap and is NOT
	// the verbatim revision. A caller diffing two revisions MUST refuse rather
	// than diff partial content (a difference past the cap would be invisible).
	BodyTruncated bool `json:"bodyTruncated,omitempty"`
}

const (
	docHistoryKindRevision = "revision"
	docHistoryKindDeleted  = "deleted"
)

type documentHistoryInput struct {
	Path          string `json:"path"`
	DocumentUID   string `json:"documentUid"`
	IncludeBodies bool   `json:"includeBodies"`
}

type documentHistoryOutput struct {
	DocumentUID string            `json:"documentUid"`
	Path        string            `json:"path"`
	Revisions   []docRevisionWire `json:"revisions"`
}

// DocumentHistory returns the full revision chain of one document straight from
// the event log — the free version history behind `maz doc log` and the body
// source for `maz doc diff`.
type DocumentHistory struct {
	log docEventReader
	nav docHistoryNavigator
}

// NewDocumentHistory constructs a DocumentHistory tool over the event log and
// navigator.
func NewDocumentHistory(log docEventReader, nav docHistoryNavigator) *DocumentHistory {
	return &DocumentHistory{log: log, nav: nav}
}

// Name implements mcp.Tool.
func (*DocumentHistory) Name() string { return "documentHistory" }

// Description implements mcp.Tool.
func (*DocumentHistory) Description() string {
	return "List the full revision chain of one document — every saved revision and any deletion tombstone — straight from the event log, oldest first. Identify the document by `path` or `documentUid`. Set `includeBodies` to also return each revision's verbatim body so two revisions can be diffed. Powers `maz doc log` and `maz doc diff`."
}

// Unlisted hides documentHistory from tools/list: it is a CLI-facing history
// verb (maz doc log|diff), not part of the agent navigation surface.
func (*DocumentHistory) Unlisted() bool { return true }

// InputSchema implements mcp.Tool.
func (*DocumentHistory) InputSchema() json.RawMessage {
	return json.RawMessage(documentHistoryInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DocumentHistory) OutputSchema() json.RawMessage {
	return json.RawMessage(documentHistoryOutputSchema)
}

// Invoke implements mcp.Tool.
func (h *DocumentHistory) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in documentHistoryInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	uid, path, err := h.resolveDocumentUID(ctx, in)
	if err != nil {
		return nil, err
	}
	revisions, err := h.collectRevisions(ctx, uid, in.IncludeBodies)
	if err != nil {
		return nil, err
	}
	return marshalTool(documentHistoryOutput{DocumentUID: uid, Path: path, Revisions: revisions})
}

// resolveDocumentUID maps the path/UID argument to a stable document UID + its
// current path. Tombstoned documents resolve via the deleted-document list so
// their history stays reachable.
func (h *DocumentHistory) resolveDocumentUID(ctx context.Context, in documentHistoryInput) (uid, path string, err error) {
	switch {
	case in.Path != "" && in.DocumentUID != "":
		return "", "", mcp.NewValidationError("set exactly one of path or documentUid, not both", nil)
	case in.DocumentUID != "":
		return h.resolveByUID(ctx, in.DocumentUID)
	case in.Path != "":
		return h.resolveByPath(ctx, in.Path)
	default:
		return "", "", mcp.NewValidationError("path or documentUid is required", nil)
	}
}

func (h *DocumentHistory) resolveByUID(ctx context.Context, ref string) (uid, path string, err error) {
	d, e := h.nav.GetDocumentByUID(ctx, ref)
	switch {
	case e == nil:
		return d.DocumentUID, d.Path, nil
	case !errors.Is(e, navigate.ErrNotFound):
		return "", "", mcp.NewInternalError(fmt.Errorf("read document %q: %w", ref, e))
	}
	dead, derr := h.deletedByPredicate(ctx, func(d navigate.Document) bool { return d.DocumentUID == ref })
	if derr != nil {
		return "", "", derr
	}
	if dead != nil {
		return dead.DocumentUID, dead.Path, nil
	}
	// The current tombstone projection only carries documents whose HEAD state
	// is deleted. A UID that was deleted and later revived/moved no longer
	// appears there, but its documentDeleted event still names it in the log.
	if du, dp, ok, lerr := h.deletedEventMatch(ctx, func(p ontology.DocumentDeleted) bool { return p.DocumentUID == ref }); lerr != nil {
		return "", "", lerr
	} else if ok {
		return du, dp, nil
	}
	return "", "", mcp.NewNotFoundError(fmt.Sprintf("no document %q", ref))
}

func (h *DocumentHistory) resolveByPath(ctx context.Context, ref string) (uid, path string, err error) {
	// Normalize the path so a bare name like "notes/foo" resolves identically to
	// "notes/foo.md" — mirrors the CleanVaultPath convention in resolveDocArg.
	if clean, cleanErr := document.CleanVaultPath(ref); cleanErr == nil {
		ref = clean
	}
	d, e := h.nav.GetDocumentByPath(ctx, ref)
	switch {
	case e == nil:
		return d.DocumentUID, d.Path, nil
	case !errors.Is(e, navigate.ErrNotFound):
		return "", "", mcp.NewInternalError(fmt.Errorf("read document %q: %w", ref, e))
	}
	dead, derr := h.deletedByPredicate(ctx, func(d navigate.Document) bool { return d.Path == ref })
	if derr != nil {
		return "", "", derr
	}
	if dead != nil {
		return dead.DocumentUID, dead.Path, nil
	}
	// A document deleted at this path and later revived/moved no longer carries
	// this path in either the live projection or the current tombstone list, but
	// its old documentDeleted event still records it. Fall back to the log so
	// historical deleted paths stay reachable.
	if du, dp, ok, lerr := h.deletedEventMatch(ctx, func(p ontology.DocumentDeleted) bool { return p.Path == ref }); lerr != nil {
		return "", "", lerr
	} else if ok {
		return du, dp, nil
	}
	return "", "", mcp.NewNotFoundError(fmt.Sprintf("no document %q", ref))
}

// deletedByPredicate returns the first tombstoned document matching pred, or nil
// when none match.
func (h *DocumentHistory) deletedByPredicate(ctx context.Context, pred func(navigate.Document) bool) (*navigate.Document, error) {
	dead, err := h.nav.DeletedDocuments(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("list deleted documents: %w", err))
	}
	for i := range dead {
		if pred(dead[i]) {
			return &dead[i], nil
		}
	}
	return nil, nil //nolint:nilnil // (nil, nil) is the documented "no match" result, distinct from an error
}

// deletedEventMatch scans documentDeleted events in the append-only log for the
// MOST RECENT tombstone satisfying pred, returning its document UID and the path
// recorded at deletion time. The log is the only place a superseded deletion
// path survives once the document is revived or moved, so this keeps `maz doc
// log <old-path>` answerable. A false ok means no tombstone matched.
func (h *DocumentHistory) deletedEventMatch(ctx context.Context, pred func(ontology.DocumentDeleted) bool) (uid, path string, ok bool, err error) {
	kinds := []kernel.EventKind{kernel.EventKind(ontology.KindDocumentDeleted)}
	for ev, rerr := range h.log.Read(ctx, kernel.ReadOpts{Kinds: kinds}) {
		if rerr != nil {
			return "", "", false, mcp.NewInternalError(fmt.Errorf("read event log: %w", rerr))
		}
		var p ontology.DocumentDeleted
		if json.Unmarshal(ev.Payload, &p) != nil || !pred(p) {
			continue
		}
		// Keep scanning: Read yields oldest-first, so the last match wins and
		// we resolve to the document's most recent deletion under this ref.
		uid, path, ok = p.DocumentUID, p.Path, true
	}
	return uid, path, ok, nil
}

// collectRevisions scans the event log for every documentRevision /
// documentDeleted event belonging to uid, in chronological order.
func (h *DocumentHistory) collectRevisions(ctx context.Context, uid string, includeBodies bool) ([]docRevisionWire, error) {
	kinds := []kernel.EventKind{
		kernel.EventKind(ontology.KindDocumentRevision),
		kernel.EventKind(ontology.KindDocumentDeleted),
	}
	out := make([]docRevisionWire, 0, 8)
	for ev, err := range h.log.Read(ctx, kernel.ReadOpts{Kinds: kinds}) {
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("read event log: %w", err))
		}
		wire, ok := docRevisionFromEvent(ev, uid, includeBodies)
		if ok {
			out = append(out, wire)
		}
	}
	return out, nil
}

// docRevisionFromEvent projects one event into a docRevisionWire when it belongs
// to uid, returning ok=false otherwise.
func docRevisionFromEvent(ev kernel.Event, uid string, includeBodies bool) (docRevisionWire, bool) {
	switch string(ev.Kind) {
	case ontology.KindDocumentRevision:
		var p ontology.DocumentRevision
		if json.Unmarshal(ev.Payload, &p) != nil || p.DocumentUID != uid {
			return docRevisionWire{}, false
		}
		w := docRevisionWire{
			UID:         ev.UID,
			CreateTime:  ev.CreateTime.UTC().Format(time.RFC3339Nano),
			Kind:        docHistoryKindRevision,
			ContentHash: p.ContentHash,
			Title:       p.Title,
			Bytes:       len(p.Body),
		}
		if p.PreviousRevisionUID != nil {
			w.PreviousRevisionUID = *p.PreviousRevisionUID
		}
		if includeBodies {
			w.Body, w.BodyTruncated = capBody(p.Body)
		}
		return w, true
	case ontology.KindDocumentDeleted:
		var p ontology.DocumentDeleted
		if json.Unmarshal(ev.Payload, &p) != nil || p.DocumentUID != uid {
			return docRevisionWire{}, false
		}
		return docRevisionWire{
			UID:        ev.UID,
			CreateTime: ev.CreateTime.UTC().Format(time.RFC3339Nano),
			Kind:       docHistoryKindDeleted,
			Reason:     p.Reason,
		}, true
	default:
		return docRevisionWire{}, false
	}
}

// capBody truncates an over-cap body on a UTF-8 rune boundary, appending a
// marker so a reader knows the body is partial. The truncated return reports
// whether the cap fired, so a diff caller can refuse partial content instead of
// silently diffing it.
func capBody(body string) (capped string, truncated bool) {
	if len(body) <= docHistoryBodyCap {
		return body, false
	}
	cut := docHistoryBodyCap
	for cut > 0 && !utf8RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + "\n…[body truncated]…\n", true
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// 0b10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
