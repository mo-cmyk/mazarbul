package git

// render_prune_test.go pins the prune contract for the renderer-owned
// subtrees: pruneStaleRendered must walk BOTH rendered/ and the root-level
// ontology/ tree so a removed/renamed kind, schema, or predicate (or a change
// to the schema-filename mapping) never leaks a stale artifact into the synced
// repo. Regression guard for the "Prune old schema paths" review finding.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPruneStaleRendered_PrunesOntologySubtree verifies that a stale file under
// the root-level ontology/ tree is removed when it is absent from the renderer's
// view, while a current ontology file and the append-only CHANGELOG/ tree
// survive.
func TestPruneStaleRendered_PrunesOntologySubtree(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()

	// Seed the repo with: a stale schema artifact (old colon path), a stale
	// kind file, a current schema artifact, a current rendered file, and a
	// CHANGELOG fragment.
	seed := map[string]string{
		"ontology/schemas/entityKind:vehicle.json":          "stale-colon-path",
		"ontology/kinds/removed_kind.yaml":                  "stale-kind",
		"ontology/schemas/entityKind-vehicle-1c33562e.json": "current-schema",
		"ontology/predicates.yaml":                          "current-predicates",
		"rendered/entities.jsonl":                           "current-entities",
		"rendered/stale.jsonl":                              "stale-rendered",
		"CHANGELOG/2026/06/abc__device.md":                  "append-only-history",
	}
	for rel, content := range seed {
		full := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// The renderer's view keeps only the current files.
	want := map[string]struct{}{
		"ontology/schemas/entityKind-vehicle-1c33562e.json": {},
		"ontology/predicates.yaml":                          {},
		"rendered/entities.jsonl":                           {},
	}

	if err := pruneStaleRendered(repoDir, want); err != nil {
		t.Fatalf("pruneStaleRendered: %v", err)
	}

	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(rel)))
		return err == nil
	}

	// Stale files (both ontology/ and rendered/) must be pruned.
	for _, rel := range []string{
		"ontology/schemas/entityKind:vehicle.json",
		"ontology/kinds/removed_kind.yaml",
		"rendered/stale.jsonl",
	} {
		if exists(rel) {
			t.Errorf("stale file not pruned: %s", rel)
		}
	}

	// Current files and the append-only CHANGELOG must survive.
	for _, rel := range []string{
		"ontology/schemas/entityKind-vehicle-1c33562e.json",
		"ontology/predicates.yaml",
		"rendered/entities.jsonl",
		"CHANGELOG/2026/06/abc__device.md",
	} {
		if !exists(rel) {
			t.Errorf("current/history file wrongly pruned: %s", rel)
		}
	}
}

// TestPruneStaleRendered_MissingSubtreeIsNoOp verifies that a missing ontology/
// or rendered/ subtree (first push) is a no-op rather than an error.
func TestPruneStaleRendered_MissingSubtreeIsNoOp(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	if err := pruneStaleRendered(repoDir, map[string]struct{}{}); err != nil {
		t.Fatalf("pruneStaleRendered on empty repo: %v", err)
	}
}

// TestPruneRenderedTree_RemovesOntologySubtree verifies that the encrypted-flip
// prune wipes the root-level ontology/ taxonomy tree (alongside rendered/ and
// CHANGELOG/) so a prior plaintext push cannot leak plaintext taxonomy
// artifacts into an encrypted snapshot. Regression guard for the re-review
// security finding on pruneRenderedTree.
func TestPruneRenderedTree_RemovesOntologySubtree(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()

	seed := map[string]string{
		"rendered/entities.jsonl":                   "plaintext-graph",
		"CHANGELOG/2026/06/abc__device.md":          "plaintext-history",
		"ontology/kinds/vehicle.yaml":               "plaintext-kind",
		"ontology/schemas/entitykind-deadbeef.json": "plaintext-schema",
		"ontology/predicates.yaml":                  "plaintext-predicates",
		"DASHBOARD.md":                              "plaintext-dashboard",
	}
	for rel, content := range seed {
		full := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	if err := pruneRenderedTree(repoDir); err != nil {
		t.Fatalf("pruneRenderedTree: %v", err)
	}

	for _, sub := range []string{"rendered", "CHANGELOG", "ontology", "DASHBOARD.md"} {
		if _, err := os.Stat(filepath.Join(repoDir, sub)); !os.IsNotExist(err) {
			t.Errorf("%s not pruned on encrypted flip (err=%v); plaintext artifact would leak", sub, err)
		}
	}
}
