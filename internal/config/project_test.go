package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// TestLoadProjectConfig_NotFound: missing file returns ErrProjectConfigNotFound.
func TestLoadProjectConfig_NotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := config.LoadProjectConfig(dir)
	if err == nil {
		t.Fatal("expected ErrProjectConfigNotFound, got nil")
	}
	if !errors.Is(err, config.ErrProjectConfigNotFound) {
		t.Fatalf("got %v, want ErrProjectConfigNotFound", err)
	}
}

// TestLoadProjectConfig_FoundInCWD: a .mazarbul.toml in the given dir is found.
func TestLoadProjectConfig_FoundInCWD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := `schema_version = 1

[defaults]
chat = "ollama"
`
	if err := os.WriteFile(filepath.Join(dir, config.ProjectConfigFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	cfg, err := config.LoadProjectConfig(dir)
	if err != nil {
		t.Fatalf("LoadProjectConfig: %v", err)
	}
	if cfg.Defaults.Chat != "ollama" {
		t.Errorf("defaults.chat = %q, want ollama", cfg.Defaults.Chat)
	}
}

// TestLoadProjectConfig_WalksUp: the loader finds .mazarbul.toml in a parent dir.
func TestLoadProjectConfig_WalksUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `schema_version = 1

[defaults]
chat = "anthropic"
`
	if err := os.WriteFile(filepath.Join(root, config.ProjectConfigFile), []byte(content), 0o600); err != nil {
		t.Fatalf("write root config: %v", err)
	}
	cfg, err := config.LoadProjectConfig(child)
	if err != nil {
		t.Fatalf("LoadProjectConfig from child: %v", err)
	}
	if cfg.Defaults.Chat != "anthropic" {
		t.Errorf("defaults.chat = %q, want anthropic", cfg.Defaults.Chat)
	}
}

// TestAddProjectConfigToGitignore_Noop: no .gitignore → no error, no file created.
func TestAddProjectConfigToGitignore_Noop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// No .gitignore present.
	if err := config.AddProjectConfigToGitignore(dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("no .gitignore should have been created")
	}
}

// TestAddProjectConfigToGitignore_Appends: appends the entry when .gitignore exists.
func TestAddProjectConfigToGitignore_Appends(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitignore := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("*.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := config.AddProjectConfigToGitignore(dir); err != nil {
		t.Fatalf("AddProjectConfigToGitignore: %v", err)
	}
	data, _ := os.ReadFile(gitignore)
	content := string(data)
	if content != "*.log\n.mazarbul.toml\n" {
		t.Errorf("unexpected .gitignore content: %q", content)
	}
}

// TestAddProjectConfigToGitignore_Idempotent: no duplicate added when already present.
func TestAddProjectConfigToGitignore_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitignore := filepath.Join(dir, ".gitignore")
	initial := "*.log\n.mazarbul.toml\n"
	if err := os.WriteFile(gitignore, []byte(initial), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := config.AddProjectConfigToGitignore(dir); err != nil {
		t.Fatalf("AddProjectConfigToGitignore: %v", err)
	}
	data, _ := os.ReadFile(gitignore)
	if string(data) != initial {
		t.Errorf("content changed on second call: %q", string(data))
	}
}

// TestMergeProjectConfig_SyncScalarsApplyWithoutRemotes pins the
// regression Greptile caught on PR #144: a `.mazarbul.toml` carrying
// only sync scalars (auto_push / coalesce / seal limits) with no
// `[sync.remotes.*]` block must still take effect. The earlier code
// gated every sync transfer on len(Remotes)>0.
func TestMergeProjectConfig_SyncScalarsApplyWithoutRemotes(t *testing.T) {
	t.Parallel()
	cs30, cs90 := 30, 90
	base := config.UserConfig{
		SchemaVersion: 1,
		Sync:          config.SyncTable{AutoPush: false, CoalesceSeconds: &cs30},
	}
	overlay := config.UserConfig{
		SchemaVersion: 1,
		Sync: config.SyncTable{
			AutoPush:               true,
			CoalesceSeconds:        &cs90,
			SealMaxEventsPlaintext: 100,
		},
	}
	merged := config.MergeProjectConfig(base, overlay)
	if !merged.Sync.AutoPush {
		t.Errorf("AutoPush not propagated from sync-scalars-only overlay")
	}
	if merged.Sync.CoalesceSeconds == nil || *merged.Sync.CoalesceSeconds != 90 {
		t.Errorf("CoalesceSeconds = %v, want 90", merged.Sync.CoalesceSeconds)
	}
	if merged.Sync.SealMaxEventsPlaintext != 100 {
		t.Errorf("SealMaxEventsPlaintext = %d, want 100", merged.Sync.SealMaxEventsPlaintext)
	}
}

// TestMergeProjectConfig_ManagedFlagPreservesInheritance pins the
// Managed-flag regression: a project overlay that only changes a
// model / base URL must not clobber an inherited managed=true from
// the base user config.
func TestMergeProjectConfig_ManagedFlagPreservesInheritance(t *testing.T) {
	t.Parallel()
	base := config.UserConfig{
		SchemaVersion: 1,
		Providers: map[string]config.ProviderTable{
			"ollama": {ChatModel: "qwen3:4b", Managed: true},
		},
	}
	overlay := config.UserConfig{
		SchemaVersion: 1,
		Providers: map[string]config.ProviderTable{
			"ollama": {ChatModel: "llama3:8b"}, // no managed flag set
		},
	}
	merged := config.MergeProjectConfig(base, overlay)
	got, ok := merged.Providers["ollama"]
	if !ok {
		t.Fatalf("ollama provider missing after merge")
	}
	if got.ChatModel != "llama3:8b" {
		t.Errorf("ChatModel = %q, want llama3:8b", got.ChatModel)
	}
	if !got.Managed {
		t.Errorf("Managed was clobbered by overlay; want true (inherited from base)")
	}
}

// TestMergeProjectConfig_OverridesDefaults: project overlay wins over base defaults.
func TestMergeProjectConfig_OverridesDefaults(t *testing.T) {
	t.Parallel()
	base := config.UserConfig{
		SchemaVersion: 1,
		Defaults:      config.DefaultsTable{Chat: "anthropic", Embed: "none"},
	}
	overlay := config.UserConfig{
		SchemaVersion: 1,
		Defaults:      config.DefaultsTable{Chat: "ollama"},
	}
	merged := config.MergeProjectConfig(base, overlay)
	if merged.Defaults.Chat != "ollama" {
		t.Errorf("chat = %q, want ollama", merged.Defaults.Chat)
	}
	// embed not overridden → inherits from base
	if merged.Defaults.Embed != "none" {
		t.Errorf("embed = %q, want none (inherited)", merged.Defaults.Embed)
	}
}
