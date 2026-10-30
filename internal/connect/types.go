// Package connect is Mazarbul's MCP auto-setup pipeline. The `maz
// connect` CLI, the in-hub Settings · Integrations panel, and the init
// wizard's new Connect step all share the same Client adapters and
// orchestrator (Service) defined here.
//
// Each supported AI client (Claude Code, Claude Desktop, Cursor,
// Windsurf, VS Code, Gemini CLI, Codex CLI) gets one adapter
// satisfying the Client interface. Adapters know exactly one
// per-client config file format; the Service composes them to detect
// installations and apply Mazarbul's MCP entry atomically.
package connect

import (
	"context"
)

// Scope is the breadth of a registration: user-wide, project-local,
// workspace-only. Clients advertise their supported scopes through
// SupportedScopes; trying to apply at an unsupported scope returns a
// validation error rather than silently changing the meaning.
type Scope string

const (
	// ScopeUser is the per-user registration (default).
	ScopeUser Scope = "user"
	// ScopeProject is the per-project registration (e.g. .mcp.json).
	ScopeProject Scope = "project"
	// ScopeWorkspace is the per-workspace registration (VS Code).
	ScopeWorkspace Scope = "workspace"
)

// Status reports a client's installation + registration state at the
// moment of detection. The TUI picker and CLI table both render
// against these values.
type Status int

const (
	// StatusUnknown is the zero value; detection has not run.
	StatusUnknown Status = iota
	// StatusNotInstalled means the client is not present on disk.
	StatusNotInstalled
	// StatusInstalledNotRegistered means the client is installed but
	// Mazarbul is not configured as an MCP server.
	StatusInstalledNotRegistered
	// StatusRegistered means Mazarbul is registered + matches the
	// command/args we'd write today.
	StatusRegistered
	// StatusRegisteredStale means Mazarbul is registered but the
	// command/args differ from what we'd write today (e.g. the user
	// moved the binary, or a previous version's entry persists).
	StatusRegisteredStale
)

// String is a short label for Status used in tables and JSON output.
func (s Status) String() string {
	switch s {
	case StatusNotInstalled:
		return "not-installed"
	case StatusInstalledNotRegistered:
		return "not-registered"
	case StatusRegistered:
		return "registered"
	case StatusRegisteredStale:
		return "stale"
	default:
		return "unknown"
	}
}

// ServerEntry is the per-server payload every adapter writes into its
// client's config file. The wire shape is identical across most JSON
// clients ({command, args, env}); the TOML adapter (Codex) marshals
// it through the same struct.
type ServerEntry struct {
	Command string            `json:"command" toml:"command"`
	Args    []string          `json:"args,omitempty" toml:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty" toml:"env,omitempty"`
}

// WriteReport summarises what Apply or Uninstall did.
type WriteReport struct {
	ClientID   string
	Scope      Scope
	ConfigPath string
	Action     string // "created" | "updated" | "noop" | "conflict" | "skipped"
	BackupPath string
	Message    string
}

// Wrote reports whether this result represents an actual config write (as
// opposed to a noop, conflict, or error), so callers can gate post-write
// output such as the onboarding tip on a real change having occurred.
func (r *WriteReport) Wrote() bool { return r.Action == actionUpdated }

// Client is the per-client adapter contract. Concrete adapters live
// in internal/connect/clients/. Each is intentionally tiny: detect
// where the client's config lives, decide whether Mazarbul is
// already there, and write/remove the entry atomically.
type Client interface {
	// ID is the wire identifier the CLI exposes (kebab-case).
	ID() string

	// DisplayName is the human label shown in the picker + tables.
	DisplayName() string

	// SupportedScopes returns the scopes this client supports.
	SupportedScopes() []Scope

	// Detect returns the current installation + registration status.
	// dataDir is the Service's resolved data directory; adapters thread
	// it into ResolveEntry(dataDir) so the canonical comparison matches
	// what Apply would write (Apply writes MAZ_DATA_DIR env when the
	// data dir is non-default, and detection must compare against the
	// same canonical entry to avoid a false RegisteredStale right after
	// a successful Apply on a custom data dir).
	Detect(ctx context.Context, dataDir string) (Status, error)

	// ConfigPath returns the on-disk path of the config file we'd
	// touch for the given scope (and optional project root). Used by
	// --dry-run and the picker's "config path" column.
	ConfigPath(scope Scope, projectRoot string) (string, error)

	// Apply registers Mazarbul as an MCP server.
	Apply(ctx context.Context, scope Scope, projectRoot string, entry ServerEntry, opts ApplyOptions) (WriteReport, error)

	// Uninstall removes Mazarbul from the client's config; sibling
	// servers and unrelated keys are preserved.
	Uninstall(ctx context.Context, scope Scope, projectRoot string) (WriteReport, error)

	// RestartHint, if non-empty, is shown to the user after Apply.
	// Used by Claude Desktop to surface the "restart the app" rule.
	RestartHint() string
}

// ApplyOptions tunes a single Apply call.
type ApplyOptions struct {
	// DryRun returns the intended WriteReport without touching disk.
	DryRun bool
	// Force overrides a conflicting existing mazarbul entry.
	Force bool
	// Capture additionally installs the passive-capture lifecycle hook
	// (Claude Code Stop). Only adapters implementing HookConfigurer honor
	// it; Service.Apply rejects it for adapters that do not.
	Capture bool
	// WarmStart additionally installs the warm-start lifecycle hook (Claude
	// Code SessionStart) that injects a scope-filtered context brief.
	WarmStart bool
}
