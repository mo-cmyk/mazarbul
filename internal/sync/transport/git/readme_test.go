package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

func TestRenderREADME_PlaintextLandingPage(t *testing.T) {
	t.Parallel()
	body := newReadmeWriter("device-a").renderREADME(kernel.ScopePersonal, false)

	for _, want := range []string{
		"# Your Mazarbul memory — `personal` scope",
		"## ⚠ What you may edit",
		"chunks/*.jsonl",
		"refused on the next pull", // integrity guard messaging
		"## At a glance",
		"[DASHBOARD.md](DASHBOARD.md)",
		"```mermaid", // how-it-works graph
		"## Browse your memory",
		"rendered/by-uid",
		"git blame rendered/by-uid",
		"maz sync pull --scope personal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plaintext README missing %q", want)
		}
	}
}

func TestRenderREADME_EncryptedHidesGraph(t *testing.T) {
	t.Parallel()
	body := newReadmeWriter("device-a").renderREADME(kernel.ScopeTeam, true)

	// The editability section and reading-on-new-device still appear...
	for _, want := range []string{
		"## ⚠ What you may edit",
		"chunks/*.maz",
		"maz init --restore",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("encrypted README missing %q", want)
		}
	}
	// ...but nothing that would leak a graph the encrypted scope does not render.
	for _, forbidden := range []string{
		"[DASHBOARD.md](DASHBOARD.md)",
		"```mermaid",
		"## Browse your memory",
		"rendered/by-uid",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("encrypted README must not contain %q (no rendered tree for encrypted scopes)", forbidden)
		}
	}
}

func TestEnsureREADME_WriteOncePreservesEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := newReadmeWriter("device-a")

	created, err := w.ensureREADME(dir, kernel.ScopePersonal, false)
	if err != nil {
		t.Fatalf("ensureREADME: %v", err)
	}
	if !created {
		t.Fatalf("first call must create the README")
	}

	custom := "# my own README\nhand-edited\n"
	path := filepath.Join(dir, readmeFilename)
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatalf("seed custom: %v", err)
	}
	created2, err := w.ensureREADME(dir, kernel.ScopePersonal, false)
	if err != nil {
		t.Fatalf("second ensureREADME: %v", err)
	}
	if created2 {
		t.Errorf("a populated README must be preserved (created=false)")
	}
	got, _ := os.ReadFile(path)
	if string(got) != custom {
		t.Errorf("hand-edited README was clobbered; got:\n%s", got)
	}
}
