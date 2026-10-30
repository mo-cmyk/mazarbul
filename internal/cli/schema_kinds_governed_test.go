package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// addKindJSON runs `schema kinds add spaceship ... --json` against dir and
// decodes the result. Shared by the permissive and governed subtests.
func addKindJSON(t *testing.T, dir string) kindsAddResult {
	t.Helper()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{
		"schema", "kinds", "add", "spaceship",
		"--display-name", "Spaceship",
		"--description", "A vehicle designed for travel in outer space.",
		"--data-dir", dir,
		"--json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema kinds add: %v (stderr=%q)", err, errBuf.String())
	}
	var res kindsAddResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal result %q: %v", out.String(), err)
	}
	return res
}

// TestSchemaKindsAdd_GovernedModeProposes pins the governance gate on the human
// CLI mint path: `maz schema kinds add` must honor [ontology] mode="governed"
// and land the kind as proposed (out of the active taxonomy until
// `maz schema review approve`), matching the defineEntityKind MCP tool so both
// explicit-mint entry points gate uniformly. Permissive mode (the default,
// no config) mints active. Regression guard for the silent-bypass bug where the
// CLI minted active regardless of mode.
func TestSchemaKindsAdd_GovernedModeProposes(t *testing.T) {
	t.Parallel()

	t.Run("permissive default", func(t *testing.T) {
		t.Parallel()
		// No config written → LoadUserConfig returns ErrUserConfigNotFound →
		// the add path degrades to permissive.
		res := addKindJSON(t, t.TempDir())
		if res.Status != ontology.EntityKindStatusActive {
			t.Errorf("permissive status = %q, want %q", res.Status, ontology.EntityKindStatusActive)
		}
	})

	t.Run("governed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := config.UserConfig{SchemaVersion: config.CurrentUserConfigSchemaVersion}
		cfg.Ontology.Mode = "governed"
		if err := config.WriteUserConfig(dir, cfg); err != nil {
			t.Fatalf("WriteUserConfig: %v", err)
		}
		res := addKindJSON(t, dir)
		if res.Status != ontology.EntityKindStatusProposed {
			t.Errorf("governed status = %q, want %q", res.Status, ontology.EntityKindStatusProposed)
		}
	})
}
