package tools

// document_delete_move.go: UNLISTED MCP tools for document deletion and
// UID-preserving rename. Both are gated as UnlistedTool (hidden from
// tools/list, always dispatchable via tools/call) because they are
// destructive or structurally mutating operations.
//
// Edge semantics:
//   - deleteDocument: all outgoing relation edges (all 8 document predicates)
//     are forgotten in the SAME atomic batch as the tombstone. Backlinks
//     pointing AT the deleted doc are left intact — they become dead links
//     that `maz doc lint` surfaces; the human authored them and the system
//     must not silently rewrite references.
//   - moveDocument: UID-preserving (no edges change); only the path updates,
//     so all wikilink/mention edges survive the rename. The slug in the
//     rendered git tree is re-derived from newPath at the dispatcher UPSERT,
//     so the rendered file moves paths on the next `maz render` pass.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

// documentDeleter is the write surface deleteDocument / moveDocument consume.
// Satisfied by *document.Importer.
type documentDeleter interface {
	Delete(ctx context.Context, path, documentUID, reason, scope string) (document.DeleteResult, error)
	Move(ctx context.Context, path, documentUID, newPath, scope string) (document.MoveResult, error)
}

// moveProjector extends vaultProjector with the SyncMove operation used by
// moveDocument to atomically write the new-path file and remove the old one.
// Satisfied by *vault.Projector.
type moveProjector interface {
	vaultProjector
	SyncMove(ctx context.Context, oldPath, newPath, scope string) (vault.Stats, error)
}

// ============================================================================
// deleteDocument
// ============================================================================

const deleteDocumentInputSchema = `{
  "type": "object",
  "properties": {
    "path":        {"type": "string", "description": "Vault-relative path of the document to delete, e.g. notes/old-idea.md."},
    "documentUid": {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "Stable document UID. Alternative to path."},
    "reason":      {"type": "string", "description": "Optional free-text reason for the deletion; stored in the event log for audit."},
    "scope":       {"type": "string", "enum": ["project", "personal", "team", "draft"], "description": "Disambiguates when the same path exists in more than one scope (composite (path, scope) identity). Omit when the path is unambiguous, or when addressing by documentUid. Deleting a team-scoped document requires membership, else E_PERMISSION."}
  },
  "oneOf": [
    {"required": ["path"]},
    {"required": ["documentUid"]}
  ],
  "additionalProperties": false
}`

const deleteDocumentOutputSchema = `{
  "type": "object",
  "properties": {
    "documentUid":  {"type": "string", "description": "Stable ULID of the deleted document."},
    "path":         {"type": "string", "description": "Vault-relative path the document occupied."},
    "displayName":  {"type": "string", "description": "Document title or humanised filename."},
    "status":       {"type": "string", "enum": ["deleted"], "description": "Always \"deleted\" on success."}
  },
  "required": ["documentUid", "path", "status"],
  "additionalProperties": false
}`

// DeleteDocument appends a documentDeleted tombstone (and edge-forget events
// for all outgoing wikilink relations) through the importer's single-writer
// discipline. Unlisted: destructive operation; agents must call explicitly.
type DeleteDocument struct {
	deleter documentDeleter
	proj    vaultProjector
}

// NewDeleteDocument constructs a DeleteDocument tool.
func NewDeleteDocument(deleter documentDeleter, proj vaultProjector) *DeleteDocument {
	return &DeleteDocument{deleter: deleter, proj: proj}
}

// Name implements mcp.Tool.
func (*DeleteDocument) Name() string { return "deleteDocument" }

// Description implements mcp.Tool.
func (*DeleteDocument) Description() string {
	return "Soft-delete a vault document by `path` or `documentUid`: appends a tombstone event that removes it from FTS5 / vector recall and forgets its outgoing wikilink edges. Backlinks pointing AT this document are left intact as dead links (surfaced by `maz doc lint`). The JSONL event log preserves full history. Unlisted; call explicitly when you need to delete a document."
}

// InputSchema implements mcp.Tool.
func (*DeleteDocument) InputSchema() json.RawMessage {
	return json.RawMessage(deleteDocumentInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DeleteDocument) OutputSchema() json.RawMessage {
	return json.RawMessage(deleteDocumentOutputSchema)
}

// Unlisted hides deleteDocument from tools/list — it is a destructive
// operation that agents should reach for deliberately, not discover
// automatically during navigation.
func (*DeleteDocument) Unlisted() bool { return true }

type deleteDocumentInput struct {
	Path        string `json:"path"`
	DocumentUID string `json:"documentUid"`
	Reason      string `json:"reason"`
	Scope       string `json:"scope"`
}

// Invoke implements mcp.Tool.
func (d *DeleteDocument) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in deleteDocumentInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	switch {
	case in.Path == "" && in.DocumentUID == "":
		return nil, mcp.NewValidationError("path or documentUid is required", nil)
	case in.Path != "" && in.DocumentUID != "":
		return nil, mcp.NewValidationError("set exactly one of path or documentUid, not both", nil)
	}
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}

	res, err := d.deleter.Delete(ctx, in.Path, in.DocumentUID, in.Reason, string(scope))
	if err != nil {
		return nil, mapDeleteMoveErr(err)
	}

	// Trigger tombstone cleanup in the vault projector so the file disappears
	// promptly. This runs SyncAll which processes the tombstone via
	// removeTombstoneLocked — conservative (I2): it only deletes the file when
	// on-disk bytes still match the last projection. A materialization error
	// must not fail the delete (the event is already durable in the log).
	if d.proj != nil {
		if _, projErr := d.proj.SyncAll(ctx); projErr != nil {
			// Non-fatal: the deletion is durable; the file disappears on the
			// next SyncAll / watcher pass.
			_ = projErr
		}
	}

	return marshalTool(res)
}

// ============================================================================
// moveDocument
// ============================================================================

const moveDocumentInputSchema = `{
  "type": "object",
  "properties": {
    "path":        {"type": "string", "description": "Current vault-relative path of the document to move."},
    "documentUid": {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "Stable document UID. Alternative to path."},
    "newPath":     {"type": "string", "minLength": 1, "description": "Target vault-relative path, e.g. archive/old-idea.md (.md appended when no extension)."},
    "scope":       {"type": "string", "enum": ["project", "personal", "team", "draft"], "description": "Disambiguates when the same path exists in more than one scope (composite (path, scope) identity). Omit when the path is unambiguous, or when addressing by documentUid. A move stays within one scope; moving a team-scoped document requires membership, else E_PERMISSION."}
  },
  "required": ["newPath"],
  "oneOf": [
    {"required": ["path", "newPath"]},
    {"required": ["documentUid", "newPath"]}
  ],
  "additionalProperties": false
}`

const moveDocumentOutputSchema = `{
  "type": "object",
  "properties": {
    "documentUid": {"type": "string", "description": "Stable ULID of the document — unchanged by the move."},
    "oldPath":     {"type": "string", "description": "Previous vault-relative path."},
    "newPath":     {"type": "string", "description": "New vault-relative path."},
    "revisionUid": {"type": "string", "description": "UID of the new documentRevision event at the new path."},
    "scope":       {"type": "string", "enum": ["project", "personal", "team", "draft"], "description": "Storage scope of the moved document — unchanged by the move (a move stays within one scope)."},
    "status":      {"type": "string", "enum": ["moved"], "description": "Always \"moved\" on success."}
  },
  "required": ["documentUid", "oldPath", "newPath", "revisionUid", "scope", "status"],
  "additionalProperties": false
}`

// MoveDocument appends a new documentRevision at newPath with the same
// documentUID so wikilink/mention edges survive the rename. Vault
// materialization is triggered after the move via SyncMove which writes the
// new-path file and removes the old one (conservatively, I2) in one
// serialised operation. Unlisted.
type MoveDocument struct {
	mover documentDeleter
	proj  moveProjector
	nav   docNavigator
}

// NewMoveDocument constructs a MoveDocument tool.
func NewMoveDocument(nav docNavigator, mover documentDeleter, proj moveProjector) *MoveDocument {
	return &MoveDocument{nav: nav, mover: mover, proj: proj}
}

// Name implements mcp.Tool.
func (*MoveDocument) Name() string { return "moveDocument" }

// Description implements mcp.Tool.
func (*MoveDocument) Description() string {
	return "Move (rename) a vault document to `newPath` while preserving its document UID, so all wikilink and mention edges survive the rename. Appends a new documentRevision at `newPath` with the same body and chains `previousRevisionUid`. Rejects if `newPath` is occupied by a different document. Unlisted; call explicitly when you need to rename a document."
}

// InputSchema implements mcp.Tool.
func (*MoveDocument) InputSchema() json.RawMessage {
	return json.RawMessage(moveDocumentInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*MoveDocument) OutputSchema() json.RawMessage {
	return json.RawMessage(moveDocumentOutputSchema)
}

// Unlisted hides moveDocument from tools/list.
func (*MoveDocument) Unlisted() bool { return true }

type moveDocumentInput struct {
	Path        string `json:"path"`
	DocumentUID string `json:"documentUid"`
	NewPath     string `json:"newPath"`
	Scope       string `json:"scope"`
}

// Invoke implements mcp.Tool.
func (m *MoveDocument) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in moveDocumentInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.NewPath == "" {
		return nil, mcp.NewValidationError("newPath is required", nil)
	}
	switch {
	case in.Path == "" && in.DocumentUID == "":
		return nil, mcp.NewValidationError("path or documentUid is required", nil)
	case in.Path != "" && in.DocumentUID != "":
		return nil, mcp.NewValidationError("set exactly one of path or documentUid, not both", nil)
	}
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}

	res, err := m.mover.Move(ctx, in.Path, in.DocumentUID, in.NewPath, string(scope))
	if err != nil {
		return nil, mapDeleteMoveErr(err)
	}

	// Materialize: SyncMove writes the new-path file and removes the old-path
	// file in one serialised, I2-safe operation. We always materialize, even on
	// an idempotent no-op retry (OldPath == NewPath): a prior move could have
	// appended the revision and then failed during SyncMove (projection errors
	// are non-fatal here), leaving the new file un-written. SyncMove handles
	// equal paths safely (write/adopt, never remove — see Projector.SyncMove),
	// so retrying repairs the vault instead of silently skipping the repair.
	if m.proj != nil {
		if _, syncErr := m.proj.SyncMove(ctx, res.OldPath, res.NewPath, res.Scope); syncErr != nil {
			_ = syncErr // non-fatal; event is durable
		}
	}

	return marshalTool(res)
}

// mapDeleteMoveErr converts importer-layer errors to MCP tool errors. The
// error hierarchy from document.Delete / document.Move:
//   - contains navigate.ErrNotFound → E_NOT_FOUND
//   - any CleanVaultPath validation message → E_VALIDATION
//   - path collision / argument-shape messages → E_VALIDATION
//   - everything else → E_INTERNAL
//
// The validation substrings mirror the exact messages CleanVaultPath and the
// Move/resolveDoc guards produce (path.go: "must be vault-relative, not
// absolute", "escapes the vault root", "only markdown documents", "path is
// empty"; delete_move.go: collision, same-path, and argument-shape errors), so
// bad caller input is never misreported as an internal failure.
func mapDeleteMoveErr(err error) error {
	if errors.Is(err, navigate.ErrNotFound) {
		return mcp.NewNotFoundError(err.Error())
	}
	msg := err.Error()
	validationSubstrings := []string{
		// CleanVaultPath (internal/document/path.go).
		"path is empty",
		"must be vault-relative, not absolute",
		"escapes the vault root",
		"only markdown documents",
		// Move / resolveDoc argument-shape and collision guards (delete_move.go).
		"already exists at",
		"same as the current path",
		"path or documentUid",
		"exactly one of",
	}
	for _, sub := range validationSubstrings {
		if strings.Contains(msg, sub) {
			return mcp.NewValidationError(msg, nil)
		}
	}
	return mcp.NewInternalError(err)
}

// Compile-time check: *vault.Projector satisfies both vaultProjector and
// moveProjector, so the production wiring's w.Projector can be passed to both
// NewDeleteDocument and NewMoveDocument without an explicit cast.
var _ moveProjector = (*vault.Projector)(nil)
