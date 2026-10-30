// Package plugin defines the contracts for Mazarbul's three plugin
// categories (providers, connectors, extensions) and the slot registry
// that hosts them. Concrete loaders, manifest parsing, and the install
// UX land in M3; this file holds the interfaces and a Noop registry.
//
// See agent-docs/architecture.md (or the external spec section 17 for
// the long form) for the category definitions:
//
//	Provider   - implementation of a kernel-required capability. One
//	             active per slot. Examples: anthropic chat, postgres
//	             storage, sqlite-vec vector.
//	Connector  - bridge to an external service. Many active. Examples:
//	             gmail, slack, notion, linear, github, drive. Usually
//	             exposed as MCP servers.
//	Extension  - everything else. Schema packs, enrichment sources,
//	             custom parsers, custom commands.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// ErrNotImplemented is returned by the Noop placeholders in this package.
// Real implementations replace these placeholders in M3.
var ErrNotImplemented = errors.New("plugin: not yet implemented")

// HostAPIVersion is the plugin-API version the running kernel implements.
// Plugin manifests declare the apiVersion they were built against; the install
// gate refuses a plugin whose declared version is incompatible with this.
//
// Versioning contract (a public API; treat it as one):
//
//   - MAJOR is bumped on a breaking change to any plugin interface in this
//     package (a removed method, a changed signature, a changed semantic).
//   - MINOR is bumped on a backward-compatible addition (a new optional method
//     on Host, a new slot, a new manifest field a plugin may ignore).
//
// See CheckAPICompatibility for the compatibility rule.
const HostAPIVersion = "v1.0.0"

// ErrInvalidAPIVersion is returned by CheckAPICompatibility when a manifest's
// apiVersion is non-empty but not valid semver. The install gate maps it to
// E_VALIDATION.
var ErrInvalidAPIVersion = errors.New("plugin: manifest apiVersion is not valid semver")

// ErrIncompatibleAPIVersion is returned by CheckAPICompatibility when a
// manifest's apiVersion is well-formed but outside the range this host
// supports. The install gate surfaces the wrapped host-vs-required message.
var ErrIncompatibleAPIVersion = errors.New("plugin: manifest apiVersion is incompatible with this host")

// CheckAPICompatibility reports whether a plugin declaring apiVersion can run on
// this host. The rule (documented for plugin authors):
//
//   - An EMPTY apiVersion opts out of the gate and is always compatible, so a
//     manifest written before this field existed installs exactly as before.
//   - A non-empty value must be valid semver (a leading "v" is optional; "1.2.0"
//     and "v1.2.0" are both accepted). A malformed value returns
//     ErrInvalidAPIVersion.
//   - The MAJOR must equal HostAPIVersion's major: a major bump is breaking, so
//     a plugin built against a different major is refused.
//   - The host must be at least as new as the plugin within that major
//     (HostAPIVersion >= declared): the host must implement the API the plugin
//     was built against. A plugin from the future is refused.
//
// An incompatible (but well-formed) version returns ErrIncompatibleAPIVersion
// wrapped with a clear "plugin requires X but this host implements Y" message
// the install flow prints verbatim.
func CheckAPICompatibility(apiVersion string) error {
	v := strings.TrimSpace(apiVersion)
	if v == "" {
		return nil
	}
	want := v
	if !strings.HasPrefix(want, "v") {
		want = "v" + want
	}
	if !semver.IsValid(want) {
		return fmt.Errorf("%w: %q (want MAJOR.MINOR.PATCH, e.g. %q)", ErrInvalidAPIVersion, v, HostAPIVersion)
	}
	if semver.Major(want) != semver.Major(HostAPIVersion) {
		return fmt.Errorf("%w: plugin requires API %s but this host implements %s (incompatible major version)",
			ErrIncompatibleAPIVersion, want, HostAPIVersion)
	}
	if semver.Compare(HostAPIVersion, want) < 0 {
		return fmt.Errorf("%w: plugin requires API %s but this host implements %s (host is too old; upgrade maz)",
			ErrIncompatibleAPIVersion, want, HostAPIVersion)
	}
	return nil
}

// Category distinguishes the three plugin shapes. Manifests declare their
// category so the loader knows which interface to satisfy.
type Category string

// Plugin categories. Each value is the literal string used in manifest files.
const (
	CategoryProvider  Category = "provider"
	CategoryConnector Category = "connector"
	CategoryExtension Category = "extension"
)

// Slot names a kernel capability that exactly one provider fills at a time.
// Providers declare which slots they fill via the manifest's `fills` field.
type Slot string

// Kernel slot identifiers. Each value matches the string in manifest files.
const (
	SlotChat           Slot = "chat"
	SlotEmbed          Slot = "embed"
	SlotStorage        Slot = "storage"
	SlotVector         Slot = "vector"
	SlotGraph          Slot = "graph"
	SlotFullText       Slot = "full_text"
	SlotSync           Slot = "sync"
	SlotSecret         Slot = "secret"
	SlotDocumentParser Slot = "document_parser"
)

// Manifest is the parsed form of a plugin's manifest file. M1 reads
// manifests from disk; this is the in-memory shape downstream code touches.
type Manifest struct {
	// Name is the install identifier (bare, no prefix). Example: "anthropic".
	Name string `json:"name"        toml:"name"`

	// Version is semver-shaped. Example: "0.4.2".
	Version string `json:"version"  toml:"version"`

	// Description is one short sentence shown in `maz list` and `maz search`.
	Description string `json:"description" toml:"description"`

	// Category is one of CategoryProvider, CategoryConnector, CategoryExtension.
	Category Category `json:"category" toml:"category"`

	// APIVersion is the host plugin-API version this plugin was built against,
	// semver-shaped (e.g. "v1.0.0"; a leading "v" is optional). OPTIONAL: an
	// empty value opts out of the compatibility gate and installs on any host,
	// so manifests written before this field existed stay valid. See
	// CheckAPICompatibility for the gate.
	APIVersion string `json:"apiVersion,omitempty" toml:"api_version,omitempty"`

	// Scopes lists the read scopes this plugin requests at install time. Each
	// token must be a recognised kernel scope (project|personal|team); the
	// install gate rejects an unknown token (E_VALIDATION) and prompts the user
	// to grant the declared set. OPTIONAL: an empty list requests no scopes and
	// installs without a consent prompt.
	Scopes []string `json:"scopes,omitempty" toml:"scopes,omitempty"`

	// Fills is the slot or slots this provider fills. Empty for connectors
	// and extensions; non-empty for providers.
	Fills []Slot `json:"fills,omitempty" toml:"fills,omitempty"`

	// AlsoEnables names companion plugins this one unlocks. Used by the
	// loader to suggest postgres + pgvector together, etc.
	AlsoEnables []string `json:"alsoEnables,omitempty" toml:"also_enables,omitempty"`

	// Config is the plugin-specific config shape, validated against the
	// plugin's JSON Schema at install time.
	Config json.RawMessage `json:"config,omitempty" toml:"config,omitempty"`
}

// Host is the surface the kernel offers to plugins. Plugins call into
// Host to read from the event log, register MCP tools, schedule jobs,
// emit logs, or fetch secrets. Real implementations land in M3.
type Host interface {
	// EventLog returns a read-only view of the event log. Plugins should
	// never write to the log directly; they propose via MCP tools or
	// enrichment proposals instead. The placeholder return type is `any`
	// to keep this package free of a kernel import until M3 wires the
	// concrete kernel.EventLog through.
	EventLog(ctx context.Context) (any, error)

	// Secret returns the value of a named secret from the OS keychain
	// (or the configured secret provider). Returns ErrNotFound when the
	// secret is not set.
	Secret(ctx context.Context, name string) (string, error)
}

// Plugin is the minimal shape every plugin implements. The category-
// specific interfaces (Provider, Connector, Extension) extend this.
type Plugin interface {
	// Manifest returns the plugin's parsed manifest.
	Manifest() Manifest

	// Init is called once at process startup before any other method.
	// Plugins use it to open connections, parse their config, etc.
	Init(ctx context.Context, host Host) error

	// Shutdown is called at process exit. Plugins flush state and close
	// connections here.
	Shutdown(ctx context.Context) error
}

// Provider implements one or more kernel slots. M1 ships with one
// provider implementation per active slot (Anthropic for chat, Ollama
// for embed, etc.); M3 makes the slot swappable via `maz use <slot> <name>`.
type Provider interface {
	Plugin

	// Slots returns the slots this provider fills. Must match Manifest.Fills.
	Slots() []Slot
}

// Connector bridges to one external service. Connectors typically run as
// MCP servers and expose their capabilities via MCP tools.
type Connector interface {
	Plugin

	// MCPTools returns the names of MCP tools this connector exposes.
	// The MCP server registers them at startup; the connector itself
	// handles tool invocations.
	MCPTools() []string
}

// Extension adds new functionality (schema packs, enrichment sources,
// parsers, custom commands). The Kind field tells the loader what shape
// to expect.
type Extension interface {
	Plugin

	// Kind identifies the extension flavor: "schema-pack", "enrichment-
	// source", "parser", "command", etc. Open enum.
	Kind() string
}

// NoopHost is a placeholder Host that returns ErrNotImplemented from every
// method. Used during testing and during the interim before the M3 loader
// lands.
type NoopHost struct{}

// EventLog implements Host. It returns ErrNotImplemented so an unwired host
// cannot be mistaken for a real "no event log available" outcome.
func (NoopHost) EventLog(_ context.Context) (any, error) {
	return nil, ErrNotImplemented
}

// Secret implements Host. It returns ErrNotImplemented.
func (NoopHost) Secret(_ context.Context, _ string) (string, error) {
	return "", ErrNotImplemented
}
