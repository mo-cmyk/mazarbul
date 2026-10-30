package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

type fakeReader struct {
	docs    []navigate.Document
	deleted []navigate.Document
	listErr error
}

func (f *fakeReader) ListDocuments(_ context.Context, opts navigate.DocumentFilter) ([]navigate.Document, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if !opts.IncludeBody {
		return nil, errors.New("projector must request bodies")
	}
	return f.docs, nil
}

func (f *fakeReader) GetDocumentByPathScope(_ context.Context, path, scope string) (navigate.Document, error) {
	for _, d := range f.docs {
		if d.Path == path && d.Scope == scope {
			return d, nil
		}
	}
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeReader) DeletedDocuments(_ context.Context) ([]navigate.Document, error) {
	return f.deleted, nil
}

// doc builds a personal-scoped document — the default partition for the bulk of
// these tests. scopedDoc covers the cross-scope cases.
func doc(path, body string) navigate.Document {
	return scopedDoc(path, "personal", body)
}

func scopedDoc(path, scope, body string) navigate.Document {
	sum := sha256.Sum256([]byte(body))
	return navigate.Document{
		DocumentUID: "uid-" + scope + "-" + path,
		Path:        path,
		Scope:       scope,
		Body:        body,
		ContentHash: hex.EncodeToString(sum[:]),
	}
}

// vaultPath is the on-disk location of a personal-scoped document under the
// partitioned vault (<root>/<scope>/<path>).
func vaultPath(root string, rel ...string) string {
	return filepath.Join(append([]string{root, "personal"}, rel...)...)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestSyncAll_WritesFreshFilesIncludingNestedDirs(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{docs: []navigate.Document{
		doc("notes/foo.md", "# Foo\n\nbody\n"),
		doc("deep/nested/dir/bar.md", "bar\n"),
	}}
	p := NewProjector(r, root, nil)

	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if stats.Written != 2 || stats.Unchanged != 0 {
		t.Fatalf("stats = %+v, want Written=2 Unchanged=0", stats)
	}
	if got := readFile(t, vaultPath(root, "notes", "foo.md")); got != "# Foo\n\nbody\n" {
		t.Fatalf("foo.md body = %q", got)
	}
	if got := readFile(t, vaultPath(root, "deep", "nested", "dir", "bar.md")); got != "bar\n" {
		t.Fatalf("bar.md body = %q", got)
	}
}

func TestSyncAll_PartitionsByScope(t *testing.T) {
	root := t.TempDir()
	// The load-bearing case: same relative path in two scopes must materialize
	// to distinct files and never clobber each other.
	r := &fakeReader{docs: []navigate.Document{
		scopedDoc("notes/foo.md", "personal", "personal body\n"),
		scopedDoc("notes/foo.md", "team", "team body\n"),
		scopedDoc("secret.md", "draft", "draft body\n"),
	}}
	p := NewProjector(r, root, nil)

	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if stats.Written != 3 {
		t.Fatalf("stats = %+v, want Written=3", stats)
	}
	if got := readFile(t, filepath.Join(root, "personal", "notes", "foo.md")); got != "personal body\n" {
		t.Fatalf("personal notes/foo.md = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "team", "notes", "foo.md")); got != "team body\n" {
		t.Fatalf("team notes/foo.md = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "draft", "secret.md")); got != "draft body\n" {
		t.Fatalf("draft secret.md = %q", got)
	}
}

func TestSyncAll_SecondPassIsByteStableNoOp(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{docs: []navigate.Document{doc("a.md", "alpha\n")}}
	p := NewProjector(r, root, nil)

	if _, err := p.SyncAll(context.Background()); err != nil {
		t.Fatalf("first SyncAll: %v", err)
	}
	target := vaultPath(root, "a.md")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("second SyncAll: %v", err)
	}
	if stats.Written != 0 || stats.Unchanged != 1 {
		t.Fatalf("stats = %+v, want Written=0 Unchanged=1 (I1 idempotency)", stats)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("file was rewritten on an idempotent pass (I1 violated)")
	}
}

func TestSyncAll_NeverClobbersUningestedEdit(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{docs: []navigate.Document{doc("a.md", "from the log\n")}}
	p := NewProjector(r, root, nil)

	// A human edit that has not been ingested yet.
	manual := "my manual edit, not yet ingested\n"
	target := vaultPath(root, "a.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir scope dir: %v", err)
	}
	if err := os.WriteFile(target, []byte(manual), 0o600); err != nil {
		t.Fatalf("seed manual edit: %v", err)
	}

	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if stats.PendingEdits != 1 || stats.Written != 0 {
		t.Fatalf("stats = %+v, want PendingEdits=1 Written=0 (I2)", stats)
	}
	if got := readFile(t, target); got != manual {
		t.Fatalf("manual edit was clobbered: %q", got)
	}
}

func TestSyncAll_UpdatesFileWhenBodyAdvances(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{docs: []navigate.Document{doc("a.md", "v1\n")}}
	p := NewProjector(r, root, nil)
	if _, err := p.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll v1: %v", err)
	}

	// A new revision lands in the log; on-disk still holds v1 verbatim.
	r.docs = []navigate.Document{doc("a.md", "v2\n")}
	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll v2: %v", err)
	}
	if stats.Written != 1 || stats.PendingEdits != 0 {
		t.Fatalf("stats = %+v, want Written=1 (on-disk matched the previous body)", stats)
	}
	if got := readFile(t, vaultPath(root, "a.md")); got != "v2\n" {
		t.Fatalf("body = %q, want v2", got)
	}
}

func TestSyncAll_StateSurvivesAcrossProjectorInstances(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{docs: []navigate.Document{doc("a.md", "v1\n")}}
	if _, err := NewProjector(r, root, nil).SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll v1: %v", err)
	}

	// A new process (fresh Projector) sees a new revision; the on-disk file
	// still holds v1 — the state sidecar proves we wrote it, so it advances.
	r.docs = []navigate.Document{doc("a.md", "v2\n")}
	stats, err := NewProjector(r, root, nil).SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll v2: %v", err)
	}
	if stats.Written != 1 || stats.PendingEdits != 0 {
		t.Fatalf("stats = %+v, want Written=1 across process restart", stats)
	}
	if got := readFile(t, vaultPath(root, "a.md")); got != "v2\n" {
		t.Fatalf("body = %q, want v2", got)
	}
}

func TestSyncAll_CorruptStateDegradesToPendingEdit(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{docs: []navigate.Document{doc("a.md", "v2\n")}}
	// On-disk file does not match the latest revision...
	target := vaultPath(root, "a.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir scope dir: %v", err)
	}
	if err := os.WriteFile(target, []byte("v1\n"), 0o600); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	// ...and the sidecar that could prove it was ours is corrupt. The sidecar
	// stays at the vault root (one owner, one mutex — I3), not in a partition.
	stateFile := filepath.Join(root, ".mazarbul", "state.json")
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(stateFile, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt state: %v", err)
	}

	stats, err := NewProjector(r, root, nil).SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	// Failing SAFE: with no proof we wrote the bytes, never overwrite them.
	if stats.PendingEdits != 1 || stats.Written != 0 {
		t.Fatalf("stats = %+v, want PendingEdits=1 (corrupt state must not enable overwrites)", stats)
	}
	if got := readFile(t, target); got != "v1\n" {
		t.Fatalf("body = %q, want untouched v1", got)
	}
}

func TestSyncAll_TombstoneRemovesMatchingFile(t *testing.T) {
	root := t.TempDir()
	d := doc("gone.md", "to be deleted\n")
	r := &fakeReader{docs: []navigate.Document{d}}
	p := NewProjector(r, root, nil)
	if _, err := p.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	r.docs = nil
	r.deleted = []navigate.Document{d}
	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll tombstone: %v", err)
	}
	if stats.Deleted != 1 || stats.Kept != 0 {
		t.Fatalf("stats = %+v, want Deleted=1", stats)
	}
	if _, err := os.Stat(vaultPath(root, "gone.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still exists after tombstone removal: %v", err)
	}
}

func TestSyncAll_TombstoneKeepsModifiedFile(t *testing.T) {
	root := t.TempDir()
	d := doc("gone.md", "deleted revision\n")
	r := &fakeReader{deleted: []navigate.Document{d}}
	p := NewProjector(r, root, nil)

	// On-disk bytes were edited after the tombstone — must survive.
	edited := "edited after delete\n"
	target := vaultPath(root, "gone.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir scope dir: %v", err)
	}
	if err := os.WriteFile(target, []byte(edited), 0o600); err != nil {
		t.Fatalf("seed edited file: %v", err)
	}

	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if stats.Kept != 1 || stats.Deleted != 0 {
		t.Fatalf("stats = %+v, want Kept=1 Deleted=0", stats)
	}
	if got := readFile(t, target); got != edited {
		t.Fatalf("edited file content = %q", got)
	}
}

func TestSyncAll_TombstoneMissingFileIsNoOp(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{deleted: []navigate.Document{doc("never-here.md", "x\n")}}
	p := NewProjector(r, root, nil)

	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if stats.Deleted != 0 || stats.Kept != 0 {
		t.Fatalf("stats = %+v, want all-zero for an already-absent file", stats)
	}
}

func TestSyncAll_SkipsPathEscapingRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "..", "escape.md")
	r := &fakeReader{docs: []navigate.Document{doc("../escape.md", "evil\n")}}
	p := NewProjector(r, root, nil)

	stats, err := p.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if stats.Written != 0 {
		t.Fatalf("stats = %+v, want nothing written for an escaping path", stats)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file materialized outside the vault root: %v", err)
	}
}

func TestSyncPath_WritesSingleDocument(t *testing.T) {
	root := t.TempDir()
	r := &fakeReader{docs: []navigate.Document{
		doc("a.md", "alpha\n"),
		doc("b.md", "beta\n"),
	}}
	p := NewProjector(r, root, nil)

	stats, err := p.SyncPath(context.Background(), "b.md", "personal")
	if err != nil {
		t.Fatalf("SyncPath: %v", err)
	}
	if stats.Written != 1 {
		t.Fatalf("stats = %+v, want Written=1", stats)
	}
	if got := readFile(t, vaultPath(root, "b.md")); got != "beta\n" {
		t.Fatalf("b.md = %q", got)
	}
	if _, err := os.Stat(vaultPath(root, "a.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("SyncPath touched a sibling document")
	}
}

func TestSyncPath_UnknownPathReturnsNotFound(t *testing.T) {
	p := NewProjector(&fakeReader{}, t.TempDir(), nil)
	_, err := p.SyncPath(context.Background(), "missing.md", "personal")
	if !errors.Is(err, navigate.ErrNotFound) {
		t.Fatalf("err = %v, want navigate.ErrNotFound", err)
	}
}

func TestFilePath_RejectsEscapes(t *testing.T) {
	p := NewProjector(&fakeReader{}, t.TempDir(), nil)
	for _, bad := range []string{"../x.md", "a/../../x.md", "/abs.md"} {
		if _, err := p.FilePath(bad, "personal"); err == nil {
			t.Errorf("FilePath(%q) accepted an escaping path", bad)
		}
	}
	// An invalid scope is refused before any join.
	if _, err := p.FilePath("ok.md", "galaxy"); err == nil {
		t.Error("FilePath accepted an invalid scope")
	}
	got, err := p.FilePath("notes/ok.md", "team")
	if err != nil {
		t.Fatalf("FilePath(notes/ok.md): %v", err)
	}
	want := filepath.Join(p.Root(), "team", "notes", "ok.md")
	if got != want {
		t.Fatalf("FilePath = %q, want %q", got, want)
	}
}

func TestSyncAll_ListErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	p := NewProjector(&fakeReader{listErr: sentinel}, t.TempDir(), nil)
	if _, err := p.SyncAll(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
}

func TestWriteFileAtomic_FilePermsAreOwnerOnly(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "perm.md")
	if err := writeFileAtomic(target, []byte("x\n")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
}

// TestVaultRel pins the watcher's scope-segment parsing: a partitioned vault is
// <root>/<scope>/<path>, so vaultRel accepts only a valid-scope leading segment
// followed by a non-empty path, and ignores (skips) a loose file at the root, a
// file under an unknown top-level directory, the .mazarbul state sidecar, and a
// path that escapes the root.
func TestVaultRel(t *testing.T) {
	root := t.TempDir()
	w := &Watcher{root: root, log: zap.NewNop()}

	tests := []struct {
		name      string
		abs       string
		wantScope string
		wantRel   string
		wantOK    bool
	}{
		{"valid nested scope path", filepath.Join(root, "personal", "notes", "a.md"), "personal", "notes/a.md", true},
		{"valid top-level scope path", filepath.Join(root, "team", "b.md"), "team", "b.md", true},
		{"draft scope path", filepath.Join(root, "draft", "wip.md"), "draft", "wip.md", true},
		{"loose file at vault root", filepath.Join(root, "loose.md"), "", "", false},
		{"unknown top-level directory", filepath.Join(root, "galaxy", "c.md"), "", "", false},
		{"state sidecar file", filepath.Join(root, mazarbulStateDir, "state.json"), "", "", false},
		{"escapes the vault root", filepath.Join(root, "..", "outside.md"), "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope, rel, ok := w.vaultRel(tc.abs)
			if scope != tc.wantScope || rel != tc.wantRel || ok != tc.wantOK {
				t.Errorf("vaultRel(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.abs, scope, rel, ok, tc.wantScope, tc.wantRel, tc.wantOK)
			}
		})
	}
}
