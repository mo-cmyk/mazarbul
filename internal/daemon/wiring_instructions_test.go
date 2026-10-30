package daemon

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// TestOpenWiring_InstructionComposerComposesBaseInstructions proves Channel A
// (#214) is wired end-to-end through the REAL daemon wiring, not a fake:
// OpenWiring builds a non-nil InstructionComposer, and invoking it against a
// real (empty) SQLite projection exercises the whole
// presetBriefReader → nav.ActivePresets → OperatingInstructions →
// ComposeServerInstructions chain. With no preset active it must degrade cleanly
// to exactly base + bootstrap (AC #3) — never error, never empty, never an
// "Operating instructions" section. embed=none keeps the test offline (no Ollama).
func TestOpenWiring_InstructionComposerComposesBaseInstructions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "none"},
		Providers: map[string]config.ProviderTable{
			"anthropic": {Model: "claude-opus-4-7"},
		},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	w, err := OpenWiring(t.Context(), WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}
	defer func() {
		if cerr := w.Close(t.Context()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	if w.InstructionComposer == nil {
		t.Fatal("w.InstructionComposer is nil — Channel A (#214) not wired in OpenWiring")
	}

	got, err := w.InstructionComposer(t.Context())
	if err != nil {
		t.Fatalf("InstructionComposer: %v", err)
	}
	// Fresh dir, no preset active → exactly base + bootstrap, via the real chain.
	if want := mcp.ComposeServerInstructions("", ""); got != want {
		t.Errorf("composer output with no preset != base+bootstrap:\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "## Operating instructions") {
		t.Errorf("no preset active but an Operating instructions section appeared:\n%s", got)
	}
	if !strings.Contains(got, "getContext") {
		t.Errorf("composed instructions missing the bootstrap getContext pointer:\n%s", got)
	}
}
