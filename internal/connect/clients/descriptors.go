// Package clients holds the table of supported AI-client connect descriptors.
// Each client is a connect.Descriptor row interpreted by connect's single
// generic adapter — adding a client is adding a row here plus a test fixture,
// never a new Go struct.
//
// Scope of the descriptor model: the dominant `{command, args, env}`
// server-stanza shape, under a configurable container key, in a JSON or TOML
// file. Clients with a divergent stanza are intentionally NOT listed and are
// documented as such (see knownUnsupported) rather than shipped as broken rows:
//
//   - copilot-cli  — requires a per-server "type":"local" + "tools" field.
//   - opencode     — uses an `mcp` key with command-as-array + "type":"local".
//   - zed          — uses `context_servers` with a nested command.{path,args}.
//   - continue     — current config is YAML (config.yaml), not JSON.
//   - kilo-code     — config format in flux (v7.0.33 moved off globalStorage).
//   - warp          — MCP servers are GUI-managed; no writable config file.
package clients

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mo-cmyk/mazarbul/internal/connect"
)

// All returns every supported client adapter in registration order. The CLI
// `maz connect` command, the init wizard's Connect step, and detection-driven
// pickers all build their roster from this single source of truth.
func All() []connect.Client {
	out := make([]connect.Client, 0, len(descriptors))
	for i := range descriptors {
		out = append(out, connect.NewFromDescriptor(&descriptors[i]))
	}
	return out
}

// UnsupportedClient is a client we deliberately do not auto-wire, with the
// reason why (a divergent config shape or a GUI-only client).
type UnsupportedClient struct {
	ID     string
	Reason string
}

// knownUnsupported lists clients we deliberately do not auto-wire, with the
// reason. Surfaced by `maz connect --list` so the omission is explicit (per the
// no-silent-caps convention) instead of reading as "we cover everything".
//
//nolint:gochecknoglobals // read-only reference table, never mutated.
var knownUnsupported = []UnsupportedClient{
	{"copilot-cli", "requires a per-server type:local + tools field"},
	{"opencode", "uses an mcp key with command-as-array + type:local"},
	{"zed", "uses context_servers with a nested command.{path,args}"},
	{"continue", "current config is YAML (config.yaml), not JSON"},
	{"kilo-code", "config format in flux (moved off globalStorage)"},
	{"warp", "MCP servers are GUI-managed; no writable config file"},
}

// descriptors is the supported-client table. Order is the display + apply-all
// order. The 7 carried-over clients come first, then the breadth additions,
// then the generic bridge.
//
//nolint:gochecknoglobals // read-only registry table, never mutated.
var descriptors = []connect.Descriptor{
	{
		ID:          "claude-code",
		DisplayName: "Claude Code",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
		// Claude Code keeps both user and project MCP entries in one file
		// (~/.claude.json); project scope nests under projects[absRoot].mcpServers.
		ConfigPath: connect.HomeFile(".claude.json"),
		ServerMap:  claudeCodeServerMap,
		// Claude Code is the one client with a lifecycle-hook system: passive
		// capture (Stop) + warm-start (SessionStart) live in a SEPARATE file,
		// ~/.claude/settings.json, not the ~/.claude.json that carries the MCP
		// entry. Declaring this spec is what makes --capture/--warm-start apply.
		Hooks: &connect.HookSpec{
			ConfigPath:     claudeCodeHookConfigPath,
			CaptureEvent:   connect.HookEvent{Event: "Stop", Args: []string{"hook", "stop"}},
			WarmStartEvent: connect.HookEvent{Event: "SessionStart", Args: []string{"hook", "session-start", "--warm-start"}},
		},
	},
	{
		ID:          "claude-desktop",
		DisplayName: "Claude Desktop",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		RestartHint: "Restart Claude Desktop to apply the change.",
		ConfigPath: connect.OSHomeFile(map[string][]string{
			"darwin":  {"Library", "Application Support", "Claude", "claude_desktop_config.json"},
			"windows": {"Claude", "claude_desktop_config.json"},
			"default": {".config", "Claude", "claude_desktop_config.json"},
		}),
	},
	{
		ID:          "cursor",
		DisplayName: "Cursor",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
		ConfigPath:  connect.ScopedFile([]string{".cursor", "mcp.json"}, []string{".cursor", "mcp.json"}),
	},
	{
		ID:          "windsurf",
		DisplayName: "Windsurf",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		ConfigPath:  connect.HomeFile(".codeium", "windsurf", "mcp_config.json"),
	},
	{
		ID:          "vscode",
		DisplayName: "VS Code",
		Format:      connect.FormatJSON,
		ServersKey:  "servers", // VS Code uses `servers`, not `mcpServers`.
		Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
		RejectJSONC: true, // mcp.json tolerates JSONC comments; we refuse to round-trip those.
		ConfigPath:  vscodeConfigPath,
	},
	{
		ID:          "gemini",
		DisplayName: "Gemini CLI",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
		ConfigPath:  connect.ScopedFile([]string{".gemini", "settings.json"}, []string{".gemini", "settings.json"}),
	},
	{
		ID:          "codex",
		DisplayName: "Codex CLI",
		Format:      connect.FormatTOML,
		ServersKey:  "mcp_servers", // underscored TOML table, not camel-cased.
		Scopes:      []connect.Scope{connect.ScopeUser},
		ConfigPath:  connect.HomeFile(".codex", "config.toml"),
	},

	// --- breadth additions (standard {command, args, env} shape) ---
	{
		ID:          "cline",
		DisplayName: "Cline",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		ConfigPath:  vscodeGlobalStorage("saoudrizwan.claude-dev", "cline_mcp_settings.json"),
	},
	{
		ID:          "roo-code",
		DisplayName: "Roo Code",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		ConfigPath:  vscodeGlobalStorage("rooveterinaryinc.roo-cline", "mcp_settings.json"),
	},
	{
		ID:          "kiro",
		DisplayName: "Kiro",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
		ConfigPath:  connect.ScopedFile([]string{".kiro", "settings", "mcp.json"}, []string{".kiro", "settings", "mcp.json"}),
	},
	{
		ID:          "qwen",
		DisplayName: "Qwen Code",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
		ConfigPath:  connect.ScopedFile([]string{".qwen", "settings.json"}, []string{".qwen", "settings.json"}),
	},
	{
		ID:          "amazon-q",
		DisplayName: "Amazon Q CLI",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
		ConfigPath:  connect.ScopedFile([]string{".aws", "amazonq", "mcp.json"}, []string{".amazonq", "mcp.json"}),
	},
	{
		ID:          "droid",
		DisplayName: "Factory Droid",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		ConfigPath:  connect.HomeFile(".factory", "mcp.json"),
	},
	{
		ID:          "lm-studio",
		DisplayName: "LM Studio",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		ConfigPath:  connect.HomeFile(".lmstudio", "mcp.json"),
	},
	{
		ID:          "json-mcp",
		DisplayName: "Generic (mcpServers JSON)",
		Format:      connect.FormatJSON,
		ServersKey:  "mcpServers",
		Scopes:      []connect.Scope{connect.ScopeUser},
		// Generic bridge: a standalone mcpServers JSON file for any client that
		// reads one but isn't auto-detected. Lives under the XDG-ish default.
		ConfigPath: connect.HomeFile(".config", "mazarbul", "mcp.json"),
	},
}

// claudeCodeServerMap locates the mcpServers map for Claude Code. User scope
// uses doc["mcpServers"]; project scope nests under
// doc["projects"][absProjectRoot]["mcpServers"] per Claude Code's documented
// layout (both live in the single ~/.claude.json file).
func claudeCodeServerMap(doc map[string]any, scope connect.Scope, projectRoot string) (map[string]any, error) {
	if scope != connect.ScopeProject {
		return connect.EnsureServersMap(doc, "mcpServers"), nil
	}
	root := projectRoot
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("project root: %w", err)
		}
		root = cwd
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("abs project root: %w", err)
	}
	projects := connect.EnsureServersMap(doc, "projects")
	project := connect.EnsureServersMap(projects, absRoot)
	return connect.EnsureServersMap(project, "mcpServers"), nil
}

// claudeCodeHookConfigPath returns the settings.json file Claude Code reads
// lifecycle hooks from. This is DELIBERATELY different from the descriptor's
// MCP ConfigPath (~/.claude.json): hooks live in ~/.claude/settings.json (user)
// or <projectRoot>/.claude/settings.json (project).
func claudeCodeHookConfigPath(scope connect.Scope, projectRoot string) (string, error) {
	if scope == connect.ScopeProject {
		root := projectRoot
		if root == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("project root: %w", err)
			}
			root = cwd
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return "", fmt.Errorf("abs project root: %w", err)
		}
		return filepath.Join(absRoot, ".claude", "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// vscodeConfigPath resolves VS Code's config: an OS-specific user-data file for
// user scope and <projectRoot>/.vscode/mcp.json for project scope.
func vscodeConfigPath(scope connect.Scope, projectRoot string) (string, error) {
	if scope == connect.ScopeProject {
		root := projectRoot
		if root == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("getwd: %w", err)
			}
			root = cwd
		}
		return filepath.Join(root, ".vscode", "mcp.json"), nil
	}
	return connect.OSHomeFile(map[string][]string{
		"darwin":  {"Library", "Application Support", "Code", "User", "mcp.json"},
		"windows": {"Code", "User", "mcp.json"},
		"default": {".config", "Code", "User", "mcp.json"},
	})(scope, projectRoot)
}

// vscodeGlobalStorage builds an OS-aware user-scope ConfigPath for a file under
// the VS Code User globalStorage directory, where VS Code extensions (Cline,
// Roo Code) keep their MCP settings.
func vscodeGlobalStorage(extension, file string) func(connect.Scope, string) (string, error) {
	return connect.OSHomeFile(map[string][]string{
		"darwin":  {"Library", "Application Support", "Code", "User", "globalStorage", extension, "settings", file},
		"windows": {"Code", "User", "globalStorage", extension, "settings", file},
		"default": {".config", "Code", "User", "globalStorage", extension, "settings", file},
	})
}

// KnownUnsupported returns the clients deliberately not auto-wired, for the
// `maz connect --list` footnote.
func KnownUnsupported() []UnsupportedClient {
	out := make([]UnsupportedClient, len(knownUnsupported))
	copy(out, knownUnsupported)
	return out
}

// Capability is a static, detection-free description of one supported client —
// the row shape `maz connect --list` renders into the supported-agents matrix.
type Capability struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"displayName"`
	Format         string   `json:"format"`
	Scopes         []string `json:"scopes"`
	UserConfigPath string   `json:"userConfigPath"`
}

// Matrix returns the capability row for every supported client, in display
// order. UserConfigPath is the resolved user-scope path on this machine (empty
// when resolution fails, e.g. an unreadable home directory).
func Matrix() []Capability {
	out := make([]Capability, 0, len(descriptors))
	for i := range descriptors {
		d := descriptors[i]
		scopes := make([]string, 0, len(d.Scopes))
		for _, s := range d.Scopes {
			scopes = append(scopes, string(s))
		}
		path := ""
		if p, err := d.ConfigPath(connect.ScopeUser, ""); err == nil {
			path = p
		}
		out = append(out, Capability{
			ID:             d.ID,
			DisplayName:    d.DisplayName,
			Format:         d.Format.String(),
			Scopes:         scopes,
			UserConfigPath: path,
		})
	}
	return out
}
