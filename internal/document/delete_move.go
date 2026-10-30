package document

// delete_move.go: Importer methods for document deletion and UID-preserving
// rename. Both operations go through the importer's single-writer discipline
// (the event log's serialised Append), so they are safe to call concurrently
// with ImportContent / ImportDir.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// DeleteResult summarises a document deletion.
type DeleteResult struct {
	// DocumentUID is the stable ULID of the deleted document.
	DocumentUID string `json:"documentUid"`
	// Path is the vault-relative path the document occupied.
	Path string `json:"path"`
	// DisplayName is the document's title or humanised filename.
	DisplayName string `json:"displayName,omitempty"`
	// Status is always "deleted" on success.
	Status string `json:"status"`
}

// MoveResult summarises a UID-preserving document rename.
type MoveResult struct {
	// DocumentUID is the stable ULID — unchanged by the move.
	DocumentUID string `json:"documentUid"`
	// OldPath is the previous vault-relative path.
	OldPath string `json:"oldPath"`
	// NewPath is the new vault-relative path.
	NewPath string `json:"newPath"`
	// RevisionUID is the new documentRevision event UID.
	RevisionUID string `json:"revisionUid"`
	// Scope is the document's storage scope — unchanged by the move (a move is
	// within one scope). The vault projector needs it to materialize under the
	// right <root>/<scope>/ partition.
	Scope string `json:"scope"`
	// Status is always "moved" on success.
	Status string `json:"status"`
}

// Delete appends a documentDeleted tombstone for the document at path (or by
// documentUID). The tombstone is applied by the dispatcher's
// applyDocumentDeleted handler which:
//   - sets documents.deleted = 1 (soft-delete, row kept for rebuild idempotency)
//   - removes FTS5 coverage via the AFTER UPDATE trigger
//   - deletes vec sidecar rows
//
// Outgoing relation edges (wikilink predicates) are forgotten in the same
// Append batch using the same forgottenEdgeEvents machinery the importer uses
// on re-import. Backlinks pointing AT the deleted document are LEFT INTACT —
// they become dead links surfaced by `maz doc lint`, which is deliberate: the
// human authored those references and the system does not silently rewrite
// them on delete.
//
// Idempotency: deleting an already-deleted document returns ErrNotFound (same
// as Get* returns for tombstoned docs), which the MCP layer maps to a clean
// E_NOT_FOUND response.
func (im *Importer) Delete(ctx context.Context, path, documentUID, reason, scope string) (DeleteResult, error) {
	doc, err := im.resolveDoc(ctx, path, documentUID, scope)
	if err != nil {
		return DeleteResult{}, err
	}

	// Serialize the edge read-then-append per document, sharing the same
	// per-document lock the async prose extractor uses. Delete's forget pass
	// reads the active edge set and then appends relationForgotten events; a
	// concurrent prose extraction on the same document UID could otherwise
	// append a fresh prose edge AFTER Delete reads the edge set but before its
	// forgets land, leaving a dangling edge on a tombstoned document. The lock
	// closes that window (the daemon shares ONE Importer between MCP tools and
	// the vault watcher, so this is the same lock the extractor takes).
	pl := im.acquireProseLock(doc.DocumentUID)
	pl.mu.Lock()
	defer func() {
		pl.mu.Unlock()
		im.releaseProseLock(doc.DocumentUID)
	}()

	// Build all events in one slice: edge-forget first, then the tombstone.
	// Appending them atomically means the derived store either transitions
	// from live-with-edges to deleted-no-edges or stays unchanged.
	var events []kernel.Event

	// Forget all outgoing document edges (all 8 document predicates), INCLUDING
	// prose-extracted edges (includeProse=true): a tombstoned document has no
	// future extraction pass to reconcile them, so every outgoing edge must go
	// or it would linger as a dead edge from a non-existent document. The
	// deleted document's entity node stays in the graph (so the UID history is
	// preserved) but its active edges are pruned to keep relations consistent.
	// The forget Reason records the deletion (not a revision edit) so the audit
	// trail stays honest per forgottenEdgeEvents' contract.
	forgetCause := "document deleted"
	if reason != "" {
		forgetCause = "document deleted: " + reason
	}
	// Every event this delete emits (forgets + tombstone) carries the resolved
	// document's OWN scope, so a team/project document's tombstone lands in that
	// scope — which both hides the correct row and routes the write through
	// AuthorizeTeamWrite, closing the hole where a non-member could delete a team
	// document through the ungated personal lane.
	docScope := kernel.Scope(doc.Scope)
	forgets, err := im.forgottenEdgeEvents(ctx, docScope, doc.DocumentUID, nil, forgetCause, true)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("delete %q: build edge-forget events: %w", doc.Path, err)
	}
	events = append(events, forgets...)

	// Tombstone event. A caller-supplied reason is stored verbatim for audit;
	// fall back to a generic reason when none was given.
	if reason == "" {
		reason = "deleteDocument via MCP"
	}
	tombstonePayload, err := json.Marshal(ontology.DocumentDeleted{
		DocumentUID: doc.DocumentUID,
		Path:        doc.Path,
		Reason:      reason,
	})
	if err != nil {
		return DeleteResult{}, fmt.Errorf("delete %q: marshal tombstone: %w", doc.Path, err)
	}
	events = append(events, kernel.Event{
		Kind:       kernel.EventKind(ontology.KindDocumentDeleted),
		Scope:      docScope,
		Source:     "mcp:deleteDocument",
		Confidence: 1.0,
		Payload:    tombstonePayload,
	})

	if _, err := im.appender.Append(ctx, events); err != nil {
		return DeleteResult{}, fmt.Errorf("delete %q: append events: %w", doc.Path, err)
	}

	return DeleteResult{
		DocumentUID: doc.DocumentUID,
		Path:        doc.Path,
		DisplayName: doc.Title,
		Status:      "deleted",
	}, nil
}

// Move appends a new documentRevision for documentUID (or the document at
// path) at newPath with the SAME documentUID and verbatim body, so all
// wikilink/mention edges survive the rename. The previous revision's UID is
// chained via PreviousRevisionUID. No tombstone is emitted for the old path:
// the vault projector detects the path change for an existing UID and removes
// the stale file at the old path via the sidecar-state cleanup in SyncPath
// (see vault_projector.go MoveCleanup).
//
// Validation:
//   - newPath must be vault-local, have a .md extension, and not collide with
//     an existing live document (E_CONFLICT).
//   - newPath must differ from oldPath (E_VALIDATION).
func (im *Importer) Move(ctx context.Context, path, documentUID, newPath, scope string) (MoveResult, error) {
	// Resolve source (scope-exact when a scope disambiguates same-path documents).
	doc, err := im.resolveDoc(ctx, path, documentUID, scope)
	if err != nil {
		return MoveResult{}, err
	}

	newRelPath, err := CleanVaultPath(newPath)
	if err != nil {
		return MoveResult{}, err
	}

	if newRelPath == doc.Path {
		// The resolved document already lives at newRelPath. How to treat this
		// depends on how the caller addressed the document:
		//   - by documentUid: this is an idempotent retry of a move that already
		//     landed (the UID now resolves to newPath). Return success so a
		//     re-issued moveDocument call is a clean no-op, as documented.
		//   - by path: the caller literally asked to move oldPath onto itself,
		//     a degenerate request that signals confusion; reject it as a
		//     validation error.
		if documentUID != "" {
			return MoveResult{
				DocumentUID: doc.DocumentUID,
				OldPath:     doc.Path,
				NewPath:     newRelPath,
				RevisionUID: doc.RevisionUID,
				Scope:       doc.Scope,
				Status:      "moved",
			}, nil
		}
		return MoveResult{}, fmt.Errorf("newPath is the same as the current path %q", doc.Path)
	}

	// Collision check: reject if a DIFFERENT live document already occupies
	// newPath IN THIS DOCUMENT'S SCOPE. A move stays within one scope, and
	// uniqueness is the composite (path, scope), so the check must be scope-exact
	// — an all-scope lookup would falsely reject moving a team doc onto a path
	// that only a personal doc occupies (the two coexist legitimately).
	existing, gerr := im.reader.GetDocumentByPathScope(ctx, newRelPath, doc.Scope)
	switch {
	case gerr == nil && existing.DocumentUID != doc.DocumentUID:
		return MoveResult{}, fmt.Errorf("a document already exists at %q (uid %s); choose a different path",
			newRelPath, existing.DocumentUID)
	case gerr == nil && existing.DocumentUID == doc.DocumentUID:
		// Same UID at newPath means a previous move already landed; idempotent.
		return MoveResult{
			DocumentUID: doc.DocumentUID,
			OldPath:     doc.Path,
			NewPath:     newRelPath,
			RevisionUID: existing.RevisionUID,
			Scope:       doc.Scope,
			Status:      "moved",
		}, nil
	case errors.Is(gerr, navigate.ErrNotFound):
		// Happy path: newPath is free.
	default:
		return MoveResult{}, fmt.Errorf("move %q: check target path: %w", doc.Path, gerr)
	}

	// Mint a new revision UID. The event log honours caller-supplied ULIDs.
	revUID, err := im.newUID()
	if err != nil {
		return MoveResult{}, fmt.Errorf("move %q: mint revision uid: %w", doc.Path, err)
	}

	now := im.now()

	// Re-parse the verbatim body to recover the full revision metadata that
	// navigate.Document does not expose (FrontmatterRaw, Wikilinks, Observations,
	// Tags). The dispatcher's applyDocumentRevision UPSERT refreshes
	// frontmatter_raw, tags, and unresolved_links_json from the new revision, so
	// omitting these fields would blank them out — a move would silently erase a
	// document's dead-link snapshot (dropping it from `maz doc lint`) and its
	// byte-stable frontmatter. Body is verbatim, so the re-parse reproduces the
	// same metadata the latest import recorded.
	parsed, perr := Parse([]byte(doc.Body))
	if perr != nil {
		// Parse returns partial results with a non-nil error (e.g. an undecodable
		// frontmatter block); the body and the fields that did parse are still
		// usable, so log-and-continue rather than fail the move.
		im.log.Warn("move: re-parse body for metadata; continuing with partial parse",
			zap.String("documentUid", doc.DocumentUID), zap.Error(perr))
	}

	// Recompute the unresolved-wikilink snapshot exactly as the importer does, so
	// the moved revision carries the same dangling-link remainder (resolved links
	// stay edges; only the still-unresolvable ones are snapshotted). Resolution
	// runs through the draft-blind resolveReader — identical to the import path —
	// so a non-draft document's link to a draft target is not silently dropped
	// from the dangling snapshot by resolving against a partition it must not see.
	_, unresolved := resolveWikilinks(ctx, im.resolveReader, im.snr, doc.DocumentUID, parsed.Wikilinks)
	_, fmUnresolved := resolveFrontmatterLinks(ctx, im.resolveReader, im.binder, doc.DocumentUID, parsed.FrontmatterLinks)
	unresolved = append(unresolved, fmUnresolved...)

	// Build the revision at the new path. Body, title, content hash, and all
	// metadata are preserved from the source document so the move is a pure
	// path change. PreviousRevisionUID chains correctly.
	prevUID := doc.RevisionUID
	rev := ontology.DocumentRevision{
		DocumentUID:         doc.DocumentUID,
		Path:                newRelPath,
		Title:               doc.Title,
		Body:                doc.Body,
		Frontmatter:         parsed.Frontmatter,
		FrontmatterRaw:      parsed.FrontmatterRaw,
		ContentHash:         doc.ContentHash,
		Wikilinks:           unresolved,
		Observations:        parsed.Observations,
		PreviousRevisionUID: &prevUID,
		Pinned:              doc.Pinned,
		Tags:                parsed.Tags,
		CreateTime:          now,
	}

	payload, err := json.Marshal(rev)
	if err != nil {
		return MoveResult{}, fmt.Errorf("move %q: marshal revision: %w", doc.Path, err)
	}

	// A move is within one scope (a re-home to a different scope is a
	// delete+create, not modeled here): the new revision carries the source
	// document's scope so the moved row stays in its partition and a team move is
	// gated by AuthorizeTeamWrite.
	events := []kernel.Event{{
		UID:        revUID,
		Kind:       kernel.EventKind(ontology.KindDocumentRevision),
		Scope:      kernel.Scope(doc.Scope),
		Source:     "mcp:moveDocument",
		Confidence: 1.0,
		Payload:    payload,
	}}

	if _, err := im.appender.Append(ctx, events); err != nil {
		return MoveResult{}, fmt.Errorf("move %q: append events: %w", doc.Path, err)
	}

	return MoveResult{
		DocumentUID: doc.DocumentUID,
		OldPath:     doc.Path,
		NewPath:     newRelPath,
		RevisionUID: revUID,
		Scope:       doc.Scope,
		Status:      "moved",
	}, nil
}

// resolveDoc is the shared resolution helper: fetches the document by path
// (when non-empty) or by documentUID. When a scope is given alongside a path the
// lookup is scope-EXACT (GetDocumentByPathScope) so a caller can target one of
// several same-path documents that composite (path, scope) uniqueness now allows
// to coexist; with an empty scope it falls back to the deterministic by-path
// getter (most-recently-updated across the read set). Returns
// navigate.ErrNotFound-wrapped as a plain error the tool layer maps to
// E_NOT_FOUND.
func (im *Importer) resolveDoc(ctx context.Context, path, documentUID, scope string) (navigate.Document, error) {
	switch {
	case path != "" && documentUID != "":
		return navigate.Document{}, errors.New("set exactly one of path or documentUid, not both")
	case path != "":
		rel, cleanErr := CleanVaultPath(path)
		if cleanErr != nil {
			return navigate.Document{}, cleanErr
		}
		var doc navigate.Document
		var err error
		if scope != "" {
			doc, err = im.reader.GetDocumentByPathScope(ctx, rel, scope)
		} else {
			doc, err = im.reader.GetDocumentByPath(ctx, rel)
		}
		if errors.Is(err, navigate.ErrNotFound) {
			return navigate.Document{}, fmt.Errorf("no document %q: %w", rel, navigate.ErrNotFound)
		}
		return doc, err
	case documentUID != "":
		doc, err := im.reader.GetDocumentByUID(ctx, documentUID)
		if errors.Is(err, navigate.ErrNotFound) {
			return navigate.Document{}, fmt.Errorf("no document uid %q: %w", documentUID, navigate.ErrNotFound)
		}
		return doc, err
	default:
		return navigate.Document{}, errors.New("path or documentUid is required")
	}
}

// ErrConflict is returned by Move when the target path is occupied by a
// different document.
var ErrConflict = errors.New("document: path conflict")
