package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureGitattributes_MarksDerivedTrees pins the editability-marking
// contract: the regenerated/machine-managed trees are flagged
// linguist-generated (collapsed in GitHub diffs, dropped from language
// stats), while chunks/ is deliberately left visible so plaintext-mode
// diff/blame/search keeps working.
func TestEnsureGitattributes_MarksDerivedTrees(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	created, err := ensureGitattributes(dir)
	if err != nil {
		t.Fatalf("ensureGitattributes: %v", err)
	}
	if !created {
		t.Fatalf("first call must report created=true")
	}
	raw, err := os.ReadFile(filepath.Join(dir, gitattributesFilename))
	if err != nil {
		t.Fatalf("read .gitattributes: %v", err)
	}
	body := string(raw)

	for _, glob := range []string{"rendered/**", "ontology/**", "CHANGELOG/**", "MANIFEST.json", "DASHBOARD.md"} {
		line := glob
		if !strings.Contains(body, line) || !strings.Contains(body, "linguist-generated=true") {
			t.Errorf("expected %q marked linguist-generated in:\n%s", glob, body)
		}
	}
	if strings.Contains(body, "chunks/**") {
		t.Errorf("chunks/ must NOT be marked generated (plaintext diff/blame is the point):\n%s", body)
	}
	if !strings.Contains(body, "*.maz binary") {
		t.Errorf("encrypted chunks must stay marked binary:\n%s", body)
	}
}

// TestEnsureGitattributes_PreservesOperatorEdits confirms the first-push
// idempotency: a second call never overwrites a hand-customized file.
func TestEnsureGitattributes_PreservesOperatorEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	custom := "# my own rules\n*.bin binary\n"
	if err := os.WriteFile(filepath.Join(dir, gitattributesFilename), []byte(custom), 0o644); err != nil {
		t.Fatalf("seed custom: %v", err)
	}
	created, err := ensureGitattributes(dir)
	if err != nil {
		t.Fatalf("ensureGitattributes: %v", err)
	}
	if created {
		t.Errorf("a populated .gitattributes must be preserved (created=false)")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, gitattributesFilename))
	if string(raw) != custom {
		t.Errorf("operator edits clobbered; got:\n%s", raw)
	}
}
