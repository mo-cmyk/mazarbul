package clients

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/connect"
)

// minSupportedClients is the acceptance-criterion floor for issue #230: at
// least 15 clients connectable.
const minSupportedClients = 15

func TestAll_RosterIsWellFormed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	all := All()
	if len(all) < minSupportedClients {
		t.Fatalf("roster has %d clients, want >= %d", len(all), minSupportedClients)
	}

	seen := map[string]bool{}
	for _, c := range all {
		id := c.ID()
		if id == "" {
			t.Fatalf("client has empty ID")
		}
		if seen[id] {
			t.Fatalf("duplicate client ID %q", id)
		}
		seen[id] = true
		if c.DisplayName() == "" {
			t.Fatalf("client %q has empty DisplayName", id)
		}
		scopes := c.SupportedScopes()
		if len(scopes) == 0 {
			t.Fatalf("client %q supports no scopes", id)
		}
		hasUser := false
		for _, s := range scopes {
			// Detect probes user scope, so every client must support it.
			if s == connect.ScopeUser {
				hasUser = true
			}
			if _, err := c.ConfigPath(s, home); err != nil {
				t.Fatalf("client %q ConfigPath(%s) failed: %v", id, s, err)
			}
		}
		if !hasUser {
			t.Fatalf("client %q does not support the user scope (required for Detect)", id)
		}
	}
}

func TestMatrix_MirrorsRoster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	caps := Matrix()
	if len(caps) != len(descriptors) {
		t.Fatalf("matrix has %d rows, want %d", len(caps), len(descriptors))
	}
	for i, c := range caps {
		if c.ID != descriptors[i].ID {
			t.Fatalf("row %d id = %q, want %q", i, c.ID, descriptors[i].ID)
		}
		if c.Format != "json" && c.Format != "toml" {
			t.Fatalf("row %q format = %q, want json|toml", c.ID, c.Format)
		}
		if len(c.Scopes) == 0 {
			t.Fatalf("row %q has no scopes", c.ID)
		}
		if c.UserConfigPath == "" {
			t.Fatalf("row %q has empty user config path", c.ID)
		}
	}
}

// TestAddRow_NoNewStruct is the acceptance-criterion test: a brand-new client
// is added as a Descriptor row alone (no new Go type) and works end-to-end
// through the generic adapter. The local descriptor here stands in for "a new
// table row".
func TestAddRow_NoNewStruct(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	entry, err := connect.ResolveEntry("")
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	c := connect.NewFromDescriptor(&connect.Descriptor{
		ID:          "brand-new",
		DisplayName: "Brand New",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		ConfigPath:  connect.HomeFile(".brandnew", "mcp.json"),
	})

	if _, aerr := c.Apply(ctx, connect.ScopeUser, "", entry, connect.ApplyOptions{}); aerr != nil {
		t.Fatalf("Apply: %v", aerr)
	}
	st, derr := c.Detect(ctx, "")
	if derr != nil {
		t.Fatalf("Detect: %v", derr)
	}
	if st != connect.StatusRegistered {
		t.Fatalf("status = %v, want Registered", st)
	}
	if _, uerr := c.Uninstall(ctx, connect.ScopeUser, ""); uerr != nil {
		t.Fatalf("Uninstall: %v", uerr)
	}
}

// TestRepresentativeClients_RoundTrip covers the issue's verification: a JSON
// client (cursor), a TOML client (codex), and the generic bridge (json-mcp) —
// dry-run, then real apply, then uninstall — against a temp HOME.
func TestRepresentativeClients_RoundTrip(t *testing.T) {
	for _, id := range []string{"cursor", "codex", "json-mcp"} {
		t.Run(id, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ctx := context.Background()
			entry, err := connect.ResolveEntry("")
			if err != nil {
				t.Fatalf("ResolveEntry: %v", err)
			}
			c := find(t, id)
			path, perr := c.ConfigPath(connect.ScopeUser, "")
			if perr != nil {
				t.Fatalf("ConfigPath: %v", perr)
			}

			// Dry-run writes nothing.
			if _, aerr := c.Apply(ctx, connect.ScopeUser, "", entry, connect.ApplyOptions{DryRun: true}); aerr != nil {
				t.Fatalf("dry-run Apply: %v", aerr)
			}
			if _, serr := os.Stat(path); !os.IsNotExist(serr) {
				t.Fatalf("dry-run wrote a file (stat err %v)", serr)
			}

			// Real apply, then detect registered.
			if _, aerr := c.Apply(ctx, connect.ScopeUser, "", entry, connect.ApplyOptions{}); aerr != nil {
				t.Fatalf("Apply: %v", aerr)
			}
			st, derr := c.Detect(ctx, "")
			if derr != nil {
				t.Fatalf("Detect: %v", derr)
			}
			if st != connect.StatusRegistered {
				t.Fatalf("status = %v, want Registered", st)
			}

			// Uninstall returns the file to its pre-install state.
			if _, uerr := c.Uninstall(ctx, connect.ScopeUser, ""); uerr != nil {
				t.Fatalf("Uninstall: %v", uerr)
			}
			st, _ = c.Detect(ctx, "")
			if st != connect.StatusInstalledNotRegistered {
				t.Fatalf("status after uninstall = %v, want InstalledNotRegistered", st)
			}
		})
	}
}

// TestClaudeCode_ProjectNesting pins the real claude-code table row's bespoke
// ServerMap: project-scope entries must nest under
// projects[absRoot].mcpServers within the single ~/.claude.json file, leaving
// the top-level mcpServers map untouched. Exercising the table entry (not an
// inline descriptor) means a regression that drops the override fails here.
func TestClaudeCode_ProjectNesting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	entry, err := connect.ResolveEntry("")
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	c := find(t, "claude-code")
	root := t.TempDir()
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	if _, aerr := c.Apply(ctx, connect.ScopeProject, root, entry, connect.ApplyOptions{}); aerr != nil {
		t.Fatalf("Apply project: %v", aerr)
	}
	path := filepath.Join(home, ".claude.json")
	doc, rerr := connect.ReadJSONFile(path)
	if rerr != nil {
		t.Fatalf("ReadJSONFile: %v", rerr)
	}
	projects, _ := doc["projects"].(map[string]any)
	project, _ := projects[absRoot].(map[string]any)
	servers, _ := project["mcpServers"].(map[string]any)
	if _, ok := servers[connect.MazServerKey]; !ok {
		t.Fatalf("project entry not nested under projects[%q].mcpServers: %v", absRoot, doc)
	}
	if _, ok := doc["mcpServers"]; ok {
		t.Fatalf("project apply leaked a top-level mcpServers map: %v", doc)
	}
}

func find(t *testing.T, id string) connect.Client { //nolint:ireturn // test helper returns the interface.
	t.Helper()
	for _, c := range All() {
		if c.ID() == id {
			return c
		}
	}
	t.Fatalf("client %q not in roster", id)
	return nil
}
