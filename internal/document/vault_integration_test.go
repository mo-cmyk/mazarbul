package document_test

// vault_integration_test.go proves the I1 round-trip fixpoint on the REAL
// pipeline (FileEventLog → dispatcher → SQLite → navigate → vault projector):
// import → materialize → re-import → re-materialize is byte-stable and a
// no-op at every step after the first. This is the determinism golden test
// for P2 — bodies are verbatim, so the vault never drifts from the log.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

func TestImportContent_VaultRoundTripIsByteStableFixpoint(t *testing.T) {
	s := newStack(t)
	root := t.TempDir()
	proj := vault.NewProjector(s.nav, root, nil)

	// Awkward-on-purpose bodies: frontmatter, CRLF, trailing whitespace, no
	// trailing newline — all must survive verbatim.
	bodies := map[string]string{
		"notes/alpha.md":   "---\ntitle: Alpha\ntags:\n  - x\n---\n# Alpha\n\nbody with [[Beta]] link\n",
		"notes/beta.md":    "# Beta\r\n\r\ncrlf body with trailing space  \r\n",
		"no-newline-at.md": "tail without newline",
	}
	for rel, body := range bodies {
		if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, rel, []byte(body)); err != nil {
			t.Fatalf("ImportContent %s: %v", rel, err)
		}
	}

	// Materialize: every file must be byte-identical to what was ingested.
	stats, err := proj.SyncAll(s.ctx)
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if stats.Written != len(bodies) || stats.PendingEdits != 0 {
		t.Fatalf("first sync stats = %+v, want Written=%d", stats, len(bodies))
	}
	// The vault is partitioned as <root>/<scope>/<path>, so a default-scoped
	// import materializes under the personal partition.
	scopeDir := filepath.Join(root, string(kernel.DefaultScope))
	for rel, body := range bodies {
		raw, err := os.ReadFile(filepath.Join(scopeDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(raw) != body {
			t.Fatalf("%s round-trip mismatch:\ngot  %q\nwant %q", rel, raw, body)
		}
	}

	// Re-import the materialized vault partition: content hashes match → all
	// skipped. Importing the scope partition dir keeps relative paths free of a
	// scope prefix, so the fixpoint resolves against the same documents.
	res, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, scopeDir)
	if err != nil {
		t.Fatalf("re-import materialized vault: %v", err)
	}
	if res.Imported != 0 || res.Skipped != len(bodies) {
		t.Fatalf("re-import = %+v, want all %d skipped (I1 fixpoint)", res, len(bodies))
	}

	// Re-materialize: nothing to do.
	stats, err = proj.SyncAll(s.ctx)
	if err != nil {
		t.Fatalf("second SyncAll: %v", err)
	}
	if stats.Written != 0 || stats.Unchanged != len(bodies) {
		t.Fatalf("second sync stats = %+v, want Unchanged=%d", stats, len(bodies))
	}
}

func TestImportContent_NewRevisionAdvancesVaultFile(t *testing.T) {
	s := newStack(t)
	root := t.TempDir()
	proj := vault.NewProjector(s.nav, root, nil)

	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "a.md", []byte("v1\n")); err != nil {
		t.Fatalf("ImportContent v1: %v", err)
	}
	if _, err := proj.SyncAll(s.ctx); err != nil {
		t.Fatalf("SyncAll v1: %v", err)
	}

	// A second revision through the write surface (agent writeDocument path).
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "a.md", []byte("v2\n")); err != nil {
		t.Fatalf("ImportContent v2: %v", err)
	}
	stats, err := proj.SyncPath(s.ctx, "a.md", string(kernel.DefaultScope))
	if err != nil {
		t.Fatalf("SyncPath: %v", err)
	}
	if stats.Written != 1 {
		t.Fatalf("stats = %+v, want the stale projection advanced", stats)
	}
	raw, err := os.ReadFile(filepath.Join(root, string(kernel.DefaultScope), "a.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "v2\n" {
		t.Fatalf("body = %q, want v2", raw)
	}

	// A manual edit is never clobbered, even when another revision lands.
	manual := "manual edit\n"
	if err := os.WriteFile(filepath.Join(root, string(kernel.DefaultScope), "a.md"), []byte(manual), 0o600); err != nil {
		t.Fatalf("seed manual edit: %v", err)
	}
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "a.md", []byte("v3\n")); err != nil {
		t.Fatalf("ImportContent v3: %v", err)
	}
	stats, err = proj.SyncPath(s.ctx, "a.md", string(kernel.DefaultScope))
	if err != nil {
		t.Fatalf("SyncPath v3: %v", err)
	}
	if stats.PendingEdits != 1 || stats.Written != 0 {
		t.Fatalf("stats = %+v, want PendingEdits=1 (I2)", stats)
	}
	raw, err = os.ReadFile(filepath.Join(root, string(kernel.DefaultScope), "a.md"))
	if err != nil {
		t.Fatalf("read after v3: %v", err)
	}
	if string(raw) != manual {
		t.Fatalf("manual edit clobbered: %q", raw)
	}

	// Ingesting the manual edit reconciles: file already matches → unchanged.
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "a.md", []byte(manual)); err != nil {
		t.Fatalf("ingest manual edit: %v", err)
	}
	stats, err = proj.SyncPath(s.ctx, "a.md", string(kernel.DefaultScope))
	if err != nil {
		t.Fatalf("SyncPath after ingest: %v", err)
	}
	if stats.Unchanged != 1 || stats.PendingEdits != 0 {
		t.Fatalf("stats = %+v, want Unchanged=1 after the edit is ingested", stats)
	}
}

// Compile-time proof the real Navigator satisfies the projector's Reader.
var _ vault.Reader = (*navigate.Navigator)(nil)
