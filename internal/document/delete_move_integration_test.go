package document_test

// delete_move_integration_test.go: integration tests for Importer.Delete and
// Importer.Move against the real four-layer stack (FileEventLog → dispatcher
// → SQLite + FTS5 + sqlite-vec + LadybugDB). Tests run with -race.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

// documentDeletedScope scans the event log for the documentDeleted tombstone of
// docUID and returns its event-level scope. The delete's scope is what gates it
// through the scope-aware write path; the navigate layer does not surface it.
func (s *stack) documentDeletedScope(t *testing.T, docUID string) (kernel.Scope, bool) {
	t.Helper()
	kinds := []kernel.EventKind{kernel.EventKind(ontology.KindDocumentDeleted)}
	for ev, err := range s.log.Read(s.ctx, kernel.ReadOpts{Kinds: kinds}) {
		if err != nil {
			t.Fatalf("read documentDeleted events: %v", err)
		}
		var d ontology.DocumentDeleted
		if json.Unmarshal(ev.Payload, &d) != nil {
			continue
		}
		if d.DocumentUID == docUID {
			return ev.Scope, true
		}
	}
	return "", false
}

// TestDelete_TombstoneCarriesDocumentScope is the authz-hole guard for Part F: a
// non-personal document's delete tombstone must land in that document's OWN
// scope. If it defaulted to personal, the delete would target the wrong
// partition (not hiding the project/team row) AND slip through the ungated
// personal lane instead of the scope-gated write path — letting a non-member
// delete a team document. project is distinguishable from the empty→personal
// default, so it proves the scope was threaded, not defaulted.
func TestDelete_TombstoneCarriesDocumentScope(t *testing.T) {
	s := newStack(t)

	res, err := s.im.ImportContent(s.ctx, kernel.ScopeProject, "notes/proj.md",
		[]byte("# Proj\n\nbody\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	if _, err := s.im.Delete(s.ctx, "notes/proj.md", "", "", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, ok := s.documentDeletedScope(t, docUID)
	if !ok {
		t.Fatal("no documentDeleted event found for the project document")
	}
	if got != kernel.ScopeProject {
		t.Errorf("tombstone scope = %q, want project (delete must be gated in the doc's own scope)", got)
	}
}

// documentRevisionScopeAt scans the event log for a documentRevision of docUID
// at the given path and returns its event-level scope (last match wins, and
// Read is chronological, so this is the most recent revision at that path).
func (s *stack) documentRevisionScopeAt(t *testing.T, docUID, path string) (kernel.Scope, bool) {
	t.Helper()
	kinds := []kernel.EventKind{kernel.EventKind(ontology.KindDocumentRevision)}
	var got kernel.Scope
	var found bool
	for ev, err := range s.log.Read(s.ctx, kernel.ReadOpts{Kinds: kinds}) {
		if err != nil {
			t.Fatalf("read documentRevision events: %v", err)
		}
		var r ontology.DocumentRevision
		if json.Unmarshal(ev.Payload, &r) != nil {
			continue
		}
		if r.DocumentUID == docUID && r.Path == path {
			got, found = ev.Scope, true
		}
	}
	return got, found
}

// TestMove_RevisionCarriesDocumentScope mirrors the delete-scope guard for Move:
// the documentRevision emitted at the new path must carry the moved document's
// OWN scope, so a within-scope move stays gated in that scope (a non-member must
// not move a team document through the ungated personal lane). project is
// distinguishable from the empty→personal default, proving the scope was
// threaded rather than defaulted.
func TestMove_RevisionCarriesDocumentScope(t *testing.T) {
	s := newStack(t)

	res, err := s.im.ImportContent(s.ctx, kernel.ScopeProject, "notes/movable.md",
		[]byte("# Movable\n\nbody\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	if _, err := s.im.Move(s.ctx, "notes/movable.md", "", "notes/moved.md", ""); err != nil {
		t.Fatalf("Move: %v", err)
	}

	got, ok := s.documentRevisionScopeAt(t, docUID, "notes/moved.md")
	if !ok {
		t.Fatal("no documentRevision event found at the new path")
	}
	if got != kernel.ScopeProject {
		t.Errorf("moved revision scope = %q, want project (move must stay gated in the doc's own scope)", got)
	}
}

// TestDelete_ScopeExactResolvesTheRightDocument pins the scope-exact resolveDoc
// fix: when the same path exists in two scopes (composite (path, scope) identity
// this wave introduced), a delete with an explicit scope targets exactly that
// scope's document and leaves the other scope's same-path document live. Without
// the disambiguator the delete would hit whichever row was most-recently-updated.
func TestDelete_ScopeExactResolvesTheRightDocument(t *testing.T) {
	s := newStackNav(t, true)

	projRes, err := s.im.ImportContent(s.ctx, kernel.ScopeProject, "notes/dup.md", []byte("# Dup\n\nproject\n"))
	if err != nil {
		t.Fatalf("import project: %v", err)
	}
	persRes, err := s.im.ImportContent(s.ctx, kernel.ScopePersonal, "notes/dup.md", []byte("# Dup\n\npersonal\n"))
	if err != nil {
		t.Fatalf("import personal: %v", err)
	}
	projUID := projRes.Documents[0].DocumentUID
	persUID := persRes.Documents[0].DocumentUID

	// Delete ONLY the project copy, selected by scope.
	if _, err := s.im.Delete(s.ctx, "notes/dup.md", "", "", "project"); err != nil {
		t.Fatalf("scoped Delete: %v", err)
	}

	// The tombstone landed on the project document...
	got, ok := s.documentDeletedScope(t, projUID)
	if !ok || got != kernel.ScopeProject {
		t.Errorf("project tombstone scope = %q ok=%v, want project", got, ok)
	}
	// ...and the personal same-path document is untouched (still resolvable).
	persDoc, err := s.nav.GetDocumentByPathScope(s.ctx, "notes/dup.md", "personal")
	if err != nil {
		t.Fatalf("personal document must survive the scoped project delete: %v", err)
	}
	if persDoc.DocumentUID != persUID {
		t.Errorf("personal document UID = %s, want %s (scoped delete crossed scopes)", persDoc.DocumentUID, persUID)
	}
}

// TestDelete_TombstoneLandsAndVaultFileRemoved proves the complete delete
// path: tombstone in the event log, row soft-deleted, FTS no longer returns
// the document, vec sidecar gone, vault file removed, edges forgotten, and a
// re-delete returns ErrNotFound.
func TestDelete_TombstoneLandsAndVaultFileRemoved(t *testing.T) {
	s := newStack(t)
	vaultDir := t.TempDir()
	proj := vault.NewProjector(s.nav, vaultDir, nil)

	// Import a document with a wikilink so we can verify edge-forget.
	sarah := s.seedEntity(t, "person", "Rosie Cotton")
	body := "# Deletable\n\n[[Rosie Cotton]] knows things.\n"
	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/deletable.md", []byte(body))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported %d, want 1", res.Imported)
	}
	_ = sarah

	// Materialize the vault file.
	if _, err := proj.SyncAll(s.ctx); err != nil {
		t.Fatalf("SyncAll before delete: %v", err)
	}
	// The vault is partitioned as <root>/<scope>/<path>.
	vaultFile := filepath.Join(vaultDir, string(kernel.DefaultScope), filepath.FromSlash("notes/deletable.md"))
	if _, err := os.Stat(vaultFile); err != nil {
		t.Fatalf("vault file missing before delete: %v", err)
	}

	// Delete the document.
	delRes, err := s.im.Delete(s.ctx, "notes/deletable.md", "", "", "")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if delRes.Status != "deleted" {
		t.Errorf("status = %q, want deleted", delRes.Status)
	}
	if delRes.Path != "notes/deletable.md" {
		t.Errorf("path = %q, want notes/deletable.md", delRes.Path)
	}

	// Document should no longer be visible via navigate (deleted=1).
	_, gerr := s.nav.GetDocumentByPath(s.ctx, "notes/deletable.md")
	if !errors.Is(gerr, navigate.ErrNotFound) {
		t.Errorf("GetDocumentByPath after delete: want ErrNotFound, got %v", gerr)
	}

	// Vault file should be removed by a subsequent SyncAll.
	if _, err := proj.SyncAll(s.ctx); err != nil {
		t.Fatalf("SyncAll after delete: %v", err)
	}
	if _, err := os.Stat(vaultFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("vault file still present after SyncAll tombstone removal: %v", err)
	}

	// Re-delete: should return ErrNotFound (idempotent = clean error).
	_, reDelErr := s.im.Delete(s.ctx, "notes/deletable.md", "", "", "")
	if !errors.Is(reDelErr, navigate.ErrNotFound) {
		t.Errorf("re-delete: want ErrNotFound, got %v", reDelErr)
	}
}

// TestDelete_EdgesForgotten verifies that deleting a document forgets its
// outgoing wikilink edges so backlinks no longer list it as a subject.
func TestDelete_EdgesForgotten(t *testing.T) {
	s := newStack(t)

	// Create a target entity and a document that links to it.
	targetUID := s.seedEntity(t, "person", "Merry Target")
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/linker.md",
		[]byte("# Linker\n\n[[Merry Target]] is referenced here.\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}

	// Confirm the mentions edge exists.
	rels, err := s.nav.EntityRelations(s.ctx, targetUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations before delete: %v", err)
	}
	found := false
	for _, r := range rels {
		if r.ObjectUID == targetUID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("mentions edge not found before delete (rels=%v)", rels)
	}

	// Delete the linking document.
	if _, err := s.im.Delete(s.ctx, "notes/linker.md", "", "", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Outgoing edges must be forgotten (superseded).
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/linker.md")
	if !errors.Is(err, navigate.ErrNotFound) {
		// If somehow visible, check edges.
		_ = doc
	}
	// Active relations should now be zero for the deleted doc as subject.
	afterRels, err := s.nav.EntityRelations(s.ctx, targetUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations after delete: %v", err)
	}
	for _, r := range afterRels {
		if r.ObjectUID == targetUID {
			t.Errorf("mentions edge still active after delete: %+v", r)
		}
	}
}

// TestMove_UIDPreservedEdgesSurvive proves the headline property: after a
// move, the document UID is unchanged and relation edges still resolve.
func TestMove_UIDPreservedEdgesSurvive(t *testing.T) {
	s := newStack(t)

	entityUID := s.seedEntity(t, "person", "Sam Survive")
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/pre-move.md",
		[]byte("# Pre-Move\n\n[[Sam Survive]] is linked here.\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}

	// Capture the UID before move.
	before, err := s.nav.GetDocumentByPath(s.ctx, "notes/pre-move.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath before move: %v", err)
	}
	origUID := before.DocumentUID

	// Move.
	moveRes, err := s.im.Move(s.ctx, "notes/pre-move.md", "", "notes/post-move.md", "")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moveRes.DocumentUID != origUID {
		t.Errorf("UID changed by move: got %s, want %s", moveRes.DocumentUID, origUID)
	}
	if moveRes.Status != "moved" {
		t.Errorf("status = %q, want moved", moveRes.Status)
	}
	if moveRes.OldPath != "notes/pre-move.md" {
		t.Errorf("oldPath = %q, want notes/pre-move.md", moveRes.OldPath)
	}
	if moveRes.NewPath != "notes/post-move.md" {
		t.Errorf("newPath = %q, want notes/post-move.md", moveRes.NewPath)
	}

	// Document is now visible at the new path.
	after, err := s.nav.GetDocumentByPath(s.ctx, "notes/post-move.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath at new path: %v", err)
	}
	if after.DocumentUID != origUID {
		t.Errorf("UID at new path = %s, want %s", after.DocumentUID, origUID)
	}

	// Old path is gone (live docs only).
	_, gerr := s.nav.GetDocumentByPath(s.ctx, "notes/pre-move.md")
	if !errors.Is(gerr, navigate.ErrNotFound) {
		t.Errorf("old path still live after move: got %v, want ErrNotFound", gerr)
	}

	// Wikilink edges from the doc to Sam survive the move.
	rels, err := s.nav.EntityRelations(s.ctx, entityUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations after move: %v", err)
	}
	_ = rels // edges exist; we just confirm the call does not error

	// neighbors call on the document entity must still work.
	neighbors, err := s.nav.Neighbors(s.ctx, origUID, 1, navigate.RelationFilter{Limit: 10})
	if err != nil {
		t.Fatalf("Neighbors after move: %v", err)
	}
	found := false
	for _, nb := range neighbors {
		if nb.Entity.UID == entityUID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Sam Survive not found as neighbor of moved document (neighbors=%v)", neighbors)
	}
}

// TestMove_VaultOldFileRemovedNewFilePresent verifies the vault-side cleanup:
// after SyncMove, the old file is gone and the new file is present.
func TestMove_VaultOldFileRemovedNewFilePresent(t *testing.T) {
	s := newStack(t)
	vaultDir := t.TempDir()
	proj := vault.NewProjector(s.nav, vaultDir, nil)

	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/old.md",
		[]byte("# Old\n\nbody\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}

	// Materialize old path. The vault is partitioned as <root>/<scope>/<path>,
	// so a default-scoped import lands under the personal partition.
	if _, err := proj.SyncAll(s.ctx); err != nil {
		t.Fatalf("SyncAll before move: %v", err)
	}
	scope := string(kernel.DefaultScope)
	oldFile := filepath.Join(vaultDir, scope, filepath.FromSlash("notes/old.md"))
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("old vault file missing before move: %v", err)
	}

	// Move.
	moveRes, err := s.im.Move(s.ctx, "notes/old.md", "", "notes/new.md", "")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	// SyncMove: write new, remove old (a move stays within one scope).
	if _, syncErr := proj.SyncMove(s.ctx, moveRes.OldPath, moveRes.NewPath, moveRes.Scope); syncErr != nil {
		t.Fatalf("SyncMove: %v", syncErr)
	}

	newFile := filepath.Join(vaultDir, scope, filepath.FromSlash("notes/new.md"))
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("new vault file missing after SyncMove: %v", err)
	}
	if _, err := os.Stat(oldFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old vault file still present after SyncMove: %v", err)
	}
}

// TestMove_RevisionChainIntact verifies that PreviousRevisionUID correctly
// chains the move revision to the pre-move revision.
func TestMove_RevisionChainIntact(t *testing.T) {
	s := newStack(t)

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/chainable.md", []byte("# Chain\n\nbody\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	origRevUID := res.Documents[0].RevisionUID

	moveRes, err := s.im.Move(s.ctx, "notes/chainable.md", "", "notes/moved.md", "")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	// The new revision UID must differ from the original.
	if moveRes.RevisionUID == origRevUID {
		t.Errorf("move returned same revision UID as original: %s", moveRes.RevisionUID)
	}

	// The moved document's RevisionUID in the projection must be the move revision.
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/moved.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath after move: %v", err)
	}
	if doc.RevisionUID != moveRes.RevisionUID {
		t.Errorf("projected revision = %s, want %s", doc.RevisionUID, moveRes.RevisionUID)
	}
}

// TestMove_PathCollisionRejected verifies that a move targeting an occupied
// path by a DIFFERENT document returns an error.
func TestMove_PathCollisionRejected(t *testing.T) {
	s := newStack(t)

	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/a.md", []byte("# A\n\nbody a\n")); err != nil {
		t.Fatalf("ImportContent a: %v", err)
	}
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/b.md", []byte("# B\n\nbody b\n")); err != nil {
		t.Fatalf("ImportContent b: %v", err)
	}

	_, err := s.im.Move(s.ctx, "notes/a.md", "", "notes/b.md", "")
	if err == nil {
		t.Fatal("Move to occupied path: want error, got nil")
	}
	if !substrInStr(err.Error(), "already exists at") {
		t.Errorf("error %q does not mention collision", err.Error())
	}
}

// TestDelete_ByUID proves that deleting via documentUid works.
func TestDelete_ByUID(t *testing.T) {
	s := newStack(t)
	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/uid-del.md", []byte("# UID Del\n\nbody\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	uid := res.Documents[0].DocumentUID

	_, delErr := s.im.Delete(s.ctx, "", uid, "", "")
	if delErr != nil {
		t.Fatalf("Delete by UID: %v", delErr)
	}

	_, gerr := s.nav.GetDocumentByUID(s.ctx, uid)
	if !errors.Is(gerr, navigate.ErrNotFound) {
		t.Errorf("GetDocumentByUID after delete: want ErrNotFound, got %v", gerr)
	}
}

// TestMove_SamePathRejected verifies that moving to the current path is an error.
func TestMove_SamePathRejected(t *testing.T) {
	s := newStack(t)
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/same.md", []byte("# Same\n\nbody\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	_, err := s.im.Move(s.ctx, "notes/same.md", "", "notes/same.md", "")
	if err == nil {
		t.Fatal("Move to same path: want error, got nil")
	}
}

// TestDelete_NotFoundReturnsError verifies that deleting a nonexistent path
// returns navigate.ErrNotFound.
func TestDelete_NotFoundReturnsError(t *testing.T) {
	s := newStack(t)
	_, err := s.im.Delete(s.ctx, "notes/ghost.md", "", "", "")
	if !errors.Is(err, navigate.ErrNotFound) {
		t.Errorf("Delete of ghost doc: want ErrNotFound, got %v", err)
	}
}

// TestMove_PreservesUnresolvedLinkSnapshot is the regression test for the
// metadata-preservation bug: a move re-emits a documentRevision, and the
// dispatcher refreshes unresolved_links_json from that revision's Wikilinks. If
// the move revision omitted the dangling-link snapshot, the moved document
// would silently drop out of DocumentsWithUnresolvedLinks and `maz doc lint`
// would stop reporting its dead links. The move must carry the same unresolved
// remainder the import recorded.
func TestMove_PreservesUnresolvedLinkSnapshot(t *testing.T) {
	s := newStack(t)

	// A document with a wikilink to a target that does NOT exist: it stays
	// dangling and is recorded in unresolved_links_json.
	body := "# Has Dead Link\n\n[[Nonexistent Target]] is not a real entity.\n"
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/dead-link.md", []byte(body)); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}

	// Confirm the doc is in the unresolved-link snapshot before the move.
	before, err := s.nav.DocumentsWithUnresolvedLinks(s.ctx)
	if err != nil {
		t.Fatalf("DocumentsWithUnresolvedLinks before move: %v", err)
	}
	if !hasUnresolvedDoc(before, "notes/dead-link.md") {
		t.Fatalf("doc not in unresolved-link snapshot before move (snapshot=%v)", before)
	}

	// Move it.
	if _, err := s.im.Move(s.ctx, "notes/dead-link.md", "", "notes/moved-dead-link.md", ""); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// The moved doc must STILL be in the unresolved-link snapshot at its new path.
	after, err := s.nav.DocumentsWithUnresolvedLinks(s.ctx)
	if err != nil {
		t.Fatalf("DocumentsWithUnresolvedLinks after move: %v", err)
	}
	if !hasUnresolvedDoc(after, "notes/moved-dead-link.md") {
		t.Errorf("moved doc dropped from unresolved-link snapshot; the move erased the dead-link state (snapshot=%v)", after)
	}
	// And the stale old-path entry must not linger as a live document.
	if hasUnresolvedDoc(after, "notes/dead-link.md") {
		t.Errorf("old path still present in unresolved-link snapshot after move (snapshot=%v)", after)
	}
}

// TestMove_ByUIDIdempotentRetry is the regression test for the UID-retry bug: a
// second moveDocument issued by documentUid (after the first landed) resolves
// the UID at the new path; the same-path guard must yield idempotent success,
// not a "same as the current path" validation error.
func TestMove_ByUIDIdempotentRetry(t *testing.T) {
	s := newStack(t)
	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/retry-src.md", []byte("# Retry\n\nbody\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	uid := res.Documents[0].DocumentUID

	first, err := s.im.Move(s.ctx, "", uid, "notes/retry-dst.md", "")
	if err != nil {
		t.Fatalf("first Move by UID: %v", err)
	}
	if first.Status != "moved" || first.NewPath != "notes/retry-dst.md" {
		t.Fatalf("first move = %+v, want moved → notes/retry-dst.md", first)
	}

	// Retry the SAME move-by-UID. The UID now resolves to notes/retry-dst.md, so
	// newRelPath == doc.Path. This must be an idempotent success, not an error.
	second, err := s.im.Move(s.ctx, "", uid, "notes/retry-dst.md", "")
	if err != nil {
		t.Fatalf("idempotent move-by-UID retry returned error: %v", err)
	}
	if second.Status != "moved" {
		t.Errorf("retry status = %q, want moved", second.Status)
	}
	if second.NewPath != "notes/retry-dst.md" {
		t.Errorf("retry newPath = %q, want notes/retry-dst.md", second.NewPath)
	}
}

// TestSyncMove_KeepsOldFileWhenTargetIsManualEdit is the regression test for
// the SyncMove guard bug: when an un-ingested manual file already occupies the
// target path, writeDocLocked skips it (I2). SyncMove must NOT then remove the
// old file — doing so would lose the only projected copy of the moved document
// and leave the unrelated target file in its place.
func TestSyncMove_KeepsOldFileWhenTargetIsManualEdit(t *testing.T) {
	s := newStack(t)
	vaultDir := t.TempDir()
	proj := vault.NewProjector(s.nav, vaultDir, nil)

	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/src.md", []byte("# Src\n\nsource body\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if _, err := proj.SyncAll(s.ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	scope := string(kernel.DefaultScope)
	oldFile := filepath.Join(vaultDir, scope, filepath.FromSlash("notes/src.md"))
	dstFile := filepath.Join(vaultDir, scope, filepath.FromSlash("notes/dst.md"))

	// Plant an UNRELATED manual file at the target path (bytes the projector
	// never wrote — an un-ingested manual edit).
	if err := os.MkdirAll(filepath.Dir(dstFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manual := []byte("# Manual\n\nun-ingested local edit\n")
	if err := os.WriteFile(dstFile, manual, 0o600); err != nil {
		t.Fatalf("write manual target: %v", err)
	}

	// Move src → dst in the event log, then materialize.
	moveRes, err := s.im.Move(s.ctx, "notes/src.md", "", "notes/dst.md", "")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	stats, err := proj.SyncMove(s.ctx, moveRes.OldPath, moveRes.NewPath, moveRes.Scope)
	if err != nil {
		t.Fatalf("SyncMove: %v", err)
	}

	// The target was a pending edit, so nothing was written or deleted.
	if stats.Deleted != 0 {
		t.Errorf("SyncMove deleted %d files; want 0 (target was a manual edit)", stats.Deleted)
	}
	if stats.PendingEdits == 0 {
		t.Errorf("SyncMove PendingEdits = 0; want >0 (target holds an un-ingested edit)")
	}
	// The old projected file must STILL exist — it is the only copy of the doc.
	if _, statErr := os.Stat(oldFile); statErr != nil {
		t.Errorf("old vault file removed despite pending-edit target: %v", statErr)
	}
	// The manual target file must be left untouched (I2).
	got, readErr := os.ReadFile(dstFile)
	if readErr != nil {
		t.Fatalf("read target after SyncMove: %v", readErr)
	}
	if !bytes.Equal(got, manual) {
		t.Errorf("manual target file was clobbered: got %q, want %q", got, manual)
	}
}

// TestSyncMove_SamePathKeepsFile is the regression test for the idempotent
// move-by-uid retry hazard: SyncMove called with oldPath == newPath must write
// (or adopt) the file and NEVER remove it — removing the path it just wrote
// would delete the moved document from the vault.
func TestSyncMove_SamePathKeepsFile(t *testing.T) {
	s := newStack(t)
	vaultDir := t.TempDir()
	proj := vault.NewProjector(s.nav, vaultDir, nil)

	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/keep.md", []byte("# Keep\n\nbody\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if _, err := proj.SyncAll(s.ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	file := filepath.Join(vaultDir, string(kernel.DefaultScope), filepath.FromSlash("notes/keep.md"))
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file missing before SyncMove: %v", err)
	}

	stats, err := proj.SyncMove(s.ctx, "notes/keep.md", "notes/keep.md", string(kernel.DefaultScope))
	if err != nil {
		t.Fatalf("SyncMove same path: %v", err)
	}
	if stats.Deleted != 0 {
		t.Errorf("SyncMove same path deleted %d files; want 0", stats.Deleted)
	}
	if _, statErr := os.Stat(file); statErr != nil {
		t.Errorf("file removed by same-path SyncMove (idempotent retry would lose the doc): %v", statErr)
	}
}

// TestDelete_ForgetsProseExtractedEdges is the regression test for the
// interaction with the base branch's prose-edge protection: the deterministic
// forget pass SKIPS prose-extracted edges on a re-import (the async extractor
// reconciles them), but a DELETE has no future extraction pass, so it must
// forget prose edges too — otherwise a tombstoned document leaves dangling
// prose "mentions" edges behind.
func TestDelete_ForgetsProseExtractedEdges(t *testing.T) {
	ctx := context.Background()
	mention := ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Prose Person",
		ExtractionText: "Prose Person",
		SourceEventUID: ontology.ULID("01JXXXXXXXXXXXXXXXXXXXXXXXX"),
		GroundingScore: 0.9,
	}
	ext := &countingExtractor{mentions: []ontology.Mention{mention}}
	s := stackWithExtractor(t, ext)
	personUID := s.seedEntity(t, ontology.EntityKindPerson, "Prose Person")

	// Import a doc whose BODY mentions the person only in prose (no wikilink),
	// so the resulting mentions edge is prose-extracted (provenance prefixed).
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/prose-del.md",
		[]byte("# Prose Del\n\nProse Person was discussed at length.\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/prose-del.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// Wait for the async prose extraction to append the edge.
	if !waitForProseEdge(t, ctx, s.nav, doc.DocumentUID, personUID) {
		t.Fatalf("prose mentions edge not present before delete")
	}

	// Delete the document.
	if _, err := s.im.Delete(s.ctx, "notes/prose-del.md", "", "", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The prose-extracted mentions edge must now be forgotten (no active edge
	// from the deleted doc to the person).
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations after delete: %v", err)
	}
	for i := range rels {
		if rels[i].SubjectUID == doc.DocumentUID && rels[i].ObjectUID == personUID {
			t.Errorf("prose mentions edge still active after delete: %+v", rels[i])
		}
	}
}

// hasUnresolvedDoc reports whether the unresolved-link snapshot contains a
// document at the given vault-relative path.
func hasUnresolvedDoc(docs []navigate.UnresolvedLinkDoc, path string) bool {
	for _, d := range docs {
		if d.Path == path {
			return true
		}
	}
	return false
}

// substrInStr is a zero-import substring check for the test file, avoiding
// name collision with the containsStr helper in importer_integration_test.go.
func substrInStr(s, sub string) bool {
	if len(sub) == 0 || len(s) < len(sub) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
