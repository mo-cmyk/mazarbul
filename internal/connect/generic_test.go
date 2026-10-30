package connect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// canonicalEntry is what ResolveEntry produces on the default data dir; Apply
// writes it and Detect compares against it, so tests register and detect the
// same value.
func canonicalEntry(t *testing.T) ServerEntry {
	t.Helper()
	entry, err := ResolveEntry("")
	if err != nil {
		t.Fatalf("ResolveEntry: %v", err)
	}
	return entry
}

// setHome points os.UserHomeDir at a fresh temp directory for the subtest. The
// home-relative path helpers resolve under it, so each test gets an isolated
// config tree. t.Setenv forbids t.Parallel, so these subtests run serially.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// jsonDescriptor is a minimal standard JSON client rooted at ~/<file>.
func jsonDescriptor(file string) *Descriptor {
	return &Descriptor{
		ID:          "test-json",
		DisplayName: "Test JSON",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []Scope{ScopeUser},
		ConfigPath:  HomeFile(file),
	}
}

func TestGenericClient_JSONLifecycle(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	entry := canonicalEntry(t)
	c := NewFromDescriptor(jsonDescriptor("client.json"))
	path, err := c.ConfigPath(ScopeUser, "")
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}

	t.Run("detects not installed when absent", func(t *testing.T) {
		st, derr := c.Detect(ctx, "")
		if derr != nil {
			t.Fatalf("Detect: %v", derr)
		}
		if st != StatusNotInstalled {
			t.Fatalf("status = %v, want NotInstalled", st)
		}
	})

	t.Run("apply creates the entry", func(t *testing.T) {
		rep, aerr := c.Apply(ctx, ScopeUser, "", entry, ApplyOptions{})
		if aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}
		if rep.Action != actionUpdated {
			t.Fatalf("action = %q, want updated", rep.Action)
		}
		doc, rerr := ReadJSONFile(path)
		if rerr != nil {
			t.Fatalf("ReadJSONFile: %v", rerr)
		}
		servers, _ := doc["mcpServers"].(map[string]any)
		if _, ok := servers[MazServerKey]; !ok {
			t.Fatalf("mazarbul entry not written: %v", doc)
		}
	})

	t.Run("detects registered after apply", func(t *testing.T) {
		st, derr := c.Detect(ctx, "")
		if derr != nil {
			t.Fatalf("Detect: %v", derr)
		}
		if st != StatusRegistered {
			t.Fatalf("status = %v, want Registered", st)
		}
	})

	t.Run("re-apply is a noop", func(t *testing.T) {
		rep, aerr := c.Apply(ctx, ScopeUser, "", entry, ApplyOptions{})
		if aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}
		if rep.Action != actionNoop {
			t.Fatalf("action = %q, want noop", rep.Action)
		}
	})

	t.Run("uninstall removes the entry", func(t *testing.T) {
		rep, uerr := c.Uninstall(ctx, ScopeUser, "")
		if uerr != nil {
			t.Fatalf("Uninstall: %v", uerr)
		}
		if rep.Action != actionUpdated {
			t.Fatalf("action = %q, want updated", rep.Action)
		}
		doc, _ := ReadJSONFile(path)
		servers, _ := doc["mcpServers"].(map[string]any)
		if _, ok := servers[MazServerKey]; ok {
			t.Fatalf("mazarbul entry still present after uninstall")
		}
	})
}

func TestGenericClient_PreservesSiblingsAndBacksUp(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	entry := canonicalEntry(t)
	c := NewFromDescriptor(jsonDescriptor("client.json"))
	path, _ := c.ConfigPath(ScopeUser, "")

	// Pre-seed a sibling server + an unrelated top-level key.
	seed := map[string]any{
		"mcpServers":   map[string]any{"other": map[string]any{"command": "x"}},
		"unrelatedKey": "keep-me",
	}
	if _, werr := WriteJSONFileAtomic(path, seed); werr != nil {
		t.Fatalf("seed: %v", werr)
	}

	rep, aerr := c.Apply(ctx, ScopeUser, "", entry, ApplyOptions{})
	if aerr != nil {
		t.Fatalf("Apply: %v", aerr)
	}
	if rep.BackupPath == "" {
		t.Fatalf("expected a backup when overwriting an existing file")
	}
	if _, serr := os.Stat(rep.BackupPath); serr != nil {
		t.Fatalf("backup file missing: %v", serr)
	}
	doc, _ := ReadJSONFile(path)
	servers, _ := doc["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("sibling server dropped: %v", servers)
	}
	if _, ok := servers[MazServerKey]; !ok {
		t.Fatalf("mazarbul entry not added: %v", servers)
	}
	if doc["unrelatedKey"] != "keep-me" {
		t.Fatalf("unrelated key dropped: %v", doc)
	}

	// Uninstall must keep the sibling and the unrelated key.
	if _, uerr := c.Uninstall(ctx, ScopeUser, ""); uerr != nil {
		t.Fatalf("Uninstall: %v", uerr)
	}
	doc, _ = ReadJSONFile(path)
	servers, _ = doc["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("uninstall dropped the sibling server: %v", servers)
	}
	if doc["unrelatedKey"] != "keep-me" {
		t.Fatalf("uninstall dropped the unrelated key: %v", doc)
	}
}

func TestGenericClient_ConflictAndForce(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	c := NewFromDescriptor(jsonDescriptor("client.json"))
	path, _ := c.ConfigPath(ScopeUser, "")

	// Existing mazarbul entry that differs from what we'd write.
	seed := map[string]any{"mcpServers": map[string]any{
		MazServerKey: map[string]any{"command": "stale-binary"},
	}}
	if _, werr := WriteJSONFileAtomic(path, seed); werr != nil {
		t.Fatalf("seed: %v", werr)
	}
	entry := canonicalEntry(t)

	t.Run("conflict without force", func(t *testing.T) {
		rep, aerr := c.Apply(ctx, ScopeUser, "", entry, ApplyOptions{})
		if aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}
		if rep.Action != actionConflict {
			t.Fatalf("action = %q, want conflict", rep.Action)
		}
	})

	t.Run("detects stale before repair", func(t *testing.T) {
		st, derr := c.Detect(ctx, "")
		if derr != nil {
			t.Fatalf("Detect: %v", derr)
		}
		if st != StatusRegisteredStale {
			t.Fatalf("status = %v, want RegisteredStale", st)
		}
	})

	t.Run("force overwrites", func(t *testing.T) {
		rep, aerr := c.Apply(ctx, ScopeUser, "", entry, ApplyOptions{Force: true})
		if aerr != nil {
			t.Fatalf("Apply: %v", aerr)
		}
		if rep.Action != actionUpdated {
			t.Fatalf("action = %q, want updated", rep.Action)
		}
		st, _ := c.Detect(ctx, "")
		if st != StatusRegistered {
			t.Fatalf("status after force = %v, want Registered", st)
		}
	})
}

func TestGenericClient_DryRun(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	c := NewFromDescriptor(jsonDescriptor("client.json"))
	path, _ := c.ConfigPath(ScopeUser, "")
	entry := canonicalEntry(t)

	rep, aerr := c.Apply(ctx, ScopeUser, "", entry, ApplyOptions{DryRun: true})
	if aerr != nil {
		t.Fatalf("Apply: %v", aerr)
	}
	if rep.Action != actionUpdated || rep.Message != msgDryRun {
		t.Fatalf("dry-run report = %+v, want updated/%q", rep, msgDryRun)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("dry-run wrote a file; want none (stat err: %v)", serr)
	}
}

func TestGenericClient_TOML(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	entry := canonicalEntry(t)
	c := NewFromDescriptor(&Descriptor{
		ID:          "test-toml",
		DisplayName: "Test TOML",
		Format:      FormatTOML,
		ServersKey:  "mcp_servers",
		Scopes:      []Scope{ScopeUser},
		ConfigPath:  HomeFile("config.toml"),
	})
	path, _ := c.ConfigPath(ScopeUser, "")

	if _, aerr := c.Apply(ctx, ScopeUser, "", entry, ApplyOptions{}); aerr != nil {
		t.Fatalf("Apply: %v", aerr)
	}
	doc, rerr := ReadTOMLFile(path)
	if rerr != nil {
		t.Fatalf("ReadTOMLFile: %v", rerr)
	}
	servers, _ := doc["mcp_servers"].(map[string]any)
	if _, ok := servers[MazServerKey]; !ok {
		t.Fatalf("mazarbul TOML entry not written: %v", doc)
	}
	st, _ := c.Detect(ctx, "")
	if st != StatusRegistered {
		t.Fatalf("TOML detect = %v, want Registered", st)
	}
}

func TestGenericClient_RejectJSONC(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	c := NewFromDescriptor(&Descriptor{
		ID:          "test-jsonc",
		DisplayName: "Test JSONC",
		Format:      FormatJSON,
		ServersKey:  "servers",
		Scopes:      []Scope{ScopeUser},
		RejectJSONC: true,
		ConfigPath:  HomeFile("mcp.json"),
	})
	path, _ := c.ConfigPath(ScopeUser, "")
	if err := os.WriteFile(path, []byte("{\n  // a comment\n  \"servers\": {}\n}\n"), 0o600); err != nil {
		t.Fatalf("seed jsonc: %v", err)
	}

	rep, aerr := c.Apply(ctx, ScopeUser, "", canonicalEntry(t), ApplyOptions{})
	if aerr != nil {
		t.Fatalf("Apply: %v", aerr)
	}
	if rep.Action != actionConflict || rep.Message != msgJSONCApply {
		t.Fatalf("jsonc apply = %+v, want conflict/%q", rep, msgJSONCApply)
	}
	st, _ := c.Detect(ctx, "")
	if st != StatusRegisteredStale {
		t.Fatalf("jsonc detect = %v, want RegisteredStale", st)
	}

	// Uninstall must refuse a JSONC file too rather than strip its comments.
	urep, uerr := c.Uninstall(ctx, ScopeUser, "")
	if uerr != nil {
		t.Fatalf("Uninstall: %v", uerr)
	}
	if urep.Action != actionConflict || urep.Message != msgJSONCRemove {
		t.Fatalf("jsonc uninstall = %+v, want conflict/%q", urep, msgJSONCRemove)
	}
}

func TestGenericClient_ServerMapNesting(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	entry := canonicalEntry(t)
	// A Claude-Code-like descriptor: one user file, project entries nested
	// under projects[absRoot].mcpServers.
	c := NewFromDescriptor(&Descriptor{
		ID:          "test-nested",
		DisplayName: "Test Nested",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []Scope{ScopeUser, ScopeProject},
		ConfigPath:  HomeFile("nested.json"),
		ServerMap: func(doc map[string]any, scope Scope, projectRoot string) (map[string]any, error) {
			if scope != ScopeProject {
				return EnsureServersMap(doc, "mcpServers"), nil
			}
			abs, _ := filepath.Abs(projectRoot)
			projects := EnsureServersMap(doc, "projects")
			project := EnsureServersMap(projects, abs)
			return EnsureServersMap(project, "mcpServers"), nil
		},
	})
	root := t.TempDir()
	path, _ := c.ConfigPath(ScopeProject, root)

	if _, aerr := c.Apply(ctx, ScopeProject, root, entry, ApplyOptions{}); aerr != nil {
		t.Fatalf("Apply project: %v", aerr)
	}
	doc, _ := ReadJSONFile(path)
	projects, _ := doc["projects"].(map[string]any)
	abs, _ := filepath.Abs(root)
	project, _ := projects[abs].(map[string]any)
	servers, _ := project["mcpServers"].(map[string]any)
	if _, ok := servers[MazServerKey]; !ok {
		t.Fatalf("project-scoped entry not nested correctly: %v", doc)
	}
	// Top-level mcpServers must remain untouched for project scope.
	if _, ok := doc["mcpServers"]; ok {
		t.Fatalf("project apply leaked a top-level mcpServers map: %v", doc)
	}
}

func TestPathHelpers(t *testing.T) {
	home := setHome(t)

	t.Run("HomeFile joins under home", func(t *testing.T) {
		got, err := HomeFile(".cfg", "x.json")(ScopeUser, "")
		if err != nil {
			t.Fatalf("HomeFile: %v", err)
		}
		if want := filepath.Join(home, ".cfg", "x.json"); got != want {
			t.Fatalf("HomeFile = %q, want %q", got, want)
		}
	})

	t.Run("ScopedFile splits user and project", func(t *testing.T) {
		resolve := ScopedFile([]string{".u", "x.json"}, []string{".p", "x.json"})
		gotUser, _ := resolve(ScopeUser, "")
		if want := filepath.Join(home, ".u", "x.json"); gotUser != want {
			t.Fatalf("user = %q, want %q", gotUser, want)
		}
		root := t.TempDir()
		gotProj, _ := resolve(ScopeProject, root)
		if want := filepath.Join(root, ".p", "x.json"); gotProj != want {
			t.Fatalf("project = %q, want %q", gotProj, want)
		}
	})

	t.Run("OSHomeFile resolves the running GOOS", func(t *testing.T) {
		resolve := OSHomeFile(map[string][]string{
			"darwin":  {"Library", "x.json"},
			"windows": {"X", "x.json"},
			"default": {".config", "x.json"},
		})
		got, err := resolve(ScopeUser, "")
		if err != nil {
			t.Fatalf("OSHomeFile: %v", err)
		}
		if got == "" {
			t.Fatalf("OSHomeFile returned empty path")
		}
	})
}

func TestService_Errors(t *testing.T) {
	setHome(t)
	ctx := context.Background()
	svc := NewService("", NewFromDescriptor(jsonDescriptor("client.json")))

	t.Run("unknown client", func(t *testing.T) {
		if _, err := svc.Apply(ctx, "nope", ScopeUser, "", ApplyOptions{}); err == nil {
			t.Fatalf("expected error for unknown client")
		}
	})

	t.Run("unsupported scope", func(t *testing.T) {
		// The descriptor supports only ScopeUser.
		if _, err := svc.Apply(ctx, "test-json", ScopeProject, "", ApplyOptions{}); err == nil {
			t.Fatalf("expected error for unsupported scope")
		}
	})
}
