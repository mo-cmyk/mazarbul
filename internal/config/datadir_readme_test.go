package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureDataDirReadme_WritesOnceAndPreservesEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := EnsureDataDirReadme(dir); err != nil {
		t.Fatalf("EnsureDataDirReadme: %v", err)
	}
	path := filepath.Join(dir, "README.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	body := string(raw)
	for _, want := range []string{"vault/", "events/", "What you may edit", "maz rebuild"} {
		if !strings.Contains(body, want) {
			t.Errorf("data-dir README missing %q", want)
		}
	}

	// Idempotent: a hand-edited README must survive a second call.
	custom := "# my own notes\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatalf("seed custom: %v", err)
	}
	if err := EnsureDataDirReadme(dir); err != nil {
		t.Fatalf("second EnsureDataDirReadme: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != custom {
		t.Errorf("hand-edited README was clobbered; got:\n%s", got)
	}
}
