package connect

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Format is the on-disk encoding of a client's config file. Every supported
// client today is either a JSON document or a TOML document; the generic
// adapter switches on this to pick the matching read/write/compare helpers.
type Format int

const (
	// FormatJSON is a JSON config file (the common case: mcpServers / servers).
	FormatJSON Format = iota
	// FormatTOML is a TOML config file (Codex's mcp_servers table).
	FormatTOML
)

// String is a short label for Format used in the supported-agents matrix.
func (f Format) String() string {
	switch f {
	case FormatTOML:
		return "toml"
	case FormatJSON:
		return "json"
	default:
		return fmt.Sprintf("unknown(%d)", int(f))
	}
}

// Descriptor is the declarative definition of one AI client's MCP config.
// The generic adapter (see NewFromDescriptor) interprets a Descriptor to
// satisfy Client, so adding a client is adding a Descriptor row + a test
// fixture — no new Go struct. A descriptor declares where the config lives,
// its format, the container key Mazarbul writes itself under, the scopes the
// client supports, and the two per-client quirks (JSONC tolerance, restart
// hint). The dominant `{command, args, env}` server-stanza shape is handled
// uniformly; clients with a divergent stanza are intentionally out of scope.
type Descriptor struct {
	// ID is the wire identifier the CLI exposes (kebab-case).
	ID string
	// DisplayName is the human label shown in the picker + tables.
	DisplayName string
	// Format is the config file's on-disk encoding.
	Format Format
	// ServersKey is the top-level map key the MCP server entry lives under:
	// "mcpServers" (most), "servers" (VS Code), or "mcp_servers" (Codex TOML).
	ServersKey string
	// Scopes are the registration scopes this client supports.
	Scopes []Scope
	// RestartHint, when non-empty, is shown after a successful Apply (and
	// returned by Client.RestartHint). Used by Claude Desktop.
	RestartHint string
	// RejectJSONC, when true, refuses to parse/write a config that contains
	// // or /* comments and surfaces a manual-edit conflict instead (VS Code).
	RejectJSONC bool

	// ConfigPath resolves the on-disk config path for a scope (and optional
	// project root). Build it from HomeFile / ScopedFile / OSHomeFile; only
	// Claude Code needs a bespoke value (single user file, project entries
	// nested under projects[absRoot]).
	ConfigPath func(scope Scope, projectRoot string) (string, error)

	// ServerMap, when non-nil, overrides the default "ensure the top-level
	// ServersKey map" lookup that Apply/Uninstall use to locate where the
	// mazarbul entry is written. Non-nil only for Claude Code, whose project
	// scope nests under projects[absRoot].mcpServers within the user file.
	ServerMap func(doc map[string]any, scope Scope, projectRoot string) (map[string]any, error)

	// Hooks, when non-nil, declares this client's lifecycle-hook capability
	// (passive capture + warm-start). It is what makes the generic adapter
	// answer SupportsHooks=true and honor --capture/--warm-start; nil for every
	// client except Claude Code. The hook file is the client's own settings
	// file, distinct from the MCP ConfigPath above.
	Hooks *HookSpec
}

// HomeFile returns a ConfigPath resolver that joins segments under the user's
// home directory for every scope. Used by clients with a single config file
// regardless of scope (e.g. Codex ~/.codex/config.toml, Windsurf).
func HomeFile(segments ...string) func(Scope, string) (string, error) {
	return func(_ Scope, _ string) (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		return filepath.Join(append([]string{home}, segments...)...), nil
	}
}

// ScopedFile returns a ConfigPath resolver that places the user-scope config
// under the home directory and the project-scope config under the project root
// (defaulting to CWD when the root is empty). Used by clients that store a
// per-project copy alongside the user-wide one (Cursor, Gemini, VS Code
// project scope).
func ScopedFile(userSegments, projectSegments []string) func(Scope, string) (string, error) {
	return func(scope Scope, projectRoot string) (string, error) {
		if scope == ScopeProject {
			root := projectRoot
			if root == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return "", fmt.Errorf("getwd: %w", err)
				}
				root = cwd
			}
			return filepath.Join(append([]string{root}, projectSegments...)...), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		return filepath.Join(append([]string{home}, userSegments...)...), nil
	}
}

// OSHomeFile returns a ConfigPath resolver that joins OS-specific segments
// under the home directory, keyed by runtime.GOOS with a "default" fallback
// (used for the non-darwin/windows case). On windows the APPDATA env var, when
// set, replaces the home prefix for the windows entry. Used by clients whose
// user-scope config lives in an OS-specific application-support directory
// (Claude Desktop, VS Code user scope). The resolver ignores scope/projectRoot
// — these clients are user-scope only or handle project scope via ScopedFile.
func OSHomeFile(perOS map[string][]string) func(Scope, string) (string, error) {
	return func(_ Scope, _ string) (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home: %w", err)
		}
		segments, ok := perOS[runtime.GOOS]
		if !ok {
			segments = perOS["default"]
		}
		if runtime.GOOS == "windows" {
			if appData := os.Getenv("APPDATA"); appData != "" {
				return filepath.Join(append([]string{appData}, segments...)...), nil
			}
			return filepath.Join(append([]string{home, "AppData", "Roaming"}, segments...)...), nil
		}
		return filepath.Join(append([]string{home}, segments...)...), nil
	}
}

// NewFromDescriptor wraps a Descriptor as a Client. The returned adapter is the
// single generic implementation that interprets the descriptor; the (Client,
// concrete) split keeps the registry table pure data. The pointer is retained
// (not copied): descriptors are immutable package-level table entries.
func NewFromDescriptor(d *Descriptor) Client { //nolint:ireturn // factory returns the interface by design.
	return &genericClient{d: d}
}
