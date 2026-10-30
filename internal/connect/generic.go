package connect

import (
	"context"
	"fmt"
	"os"
)

// Action labels written into WriteReport.Action. Centralised so callers (CLI
// tables, TUI picker) can switch on a stable set and the goconst linter does
// not flag the repeated literals.
const (
	actionUpdated  = "updated"
	actionNoop     = "noop"
	actionConflict = "conflict"
)

// Common message strings shared across the adapter's report paths.
const (
	msgConflict    = "existing mazarbul entry differs; pass --force to overwrite"
	msgDryRun      = "dry run"
	msgJSONCApply  = "config contains JSONC comments; add the mazarbul entry manually"
	msgJSONCRemove = "config contains JSONC comments; remove the mazarbul entry manually"
)

// genericClient is the single Client implementation that interprets a
// Descriptor. It replaces the per-client adapter structs: the read → locate
// server map → compare/conflict/noop/force → atomic write + backup loop that
// every client shares lives here once, and the descriptor supplies the
// per-client specifics (format, container key, paths, scopes, quirks).
type genericClient struct {
	d *Descriptor
}

// ID implements Client.
func (c *genericClient) ID() string { return c.d.ID }

// DisplayName implements Client.
func (c *genericClient) DisplayName() string { return c.d.DisplayName }

// SupportedScopes implements Client.
func (c *genericClient) SupportedScopes() []Scope { return c.d.Scopes }

// RestartHint implements Client.
func (c *genericClient) RestartHint() string { return c.d.RestartHint }

// ConfigPath implements Client.
func (c *genericClient) ConfigPath(scope Scope, projectRoot string) (string, error) {
	return c.d.ConfigPath(scope, projectRoot)
}

// readDoc reads the config file in the descriptor's format, returning an empty
// map when absent so callers can write into it directly.
func (c *genericClient) readDoc(path string) (map[string]any, error) {
	if c.d.Format == FormatTOML {
		return ReadTOMLFile(path)
	}
	return ReadJSONFile(path)
}

// writeDoc writes doc atomically in the descriptor's format, returning the
// backup path (empty when no backup was taken).
func (c *genericClient) writeDoc(path string, doc map[string]any) (string, error) {
	if c.d.Format == FormatTOML {
		return WriteTOMLFileAtomic(path, doc)
	}
	return WriteJSONFileAtomic(path, doc)
}

// entryEquals reports whether an existing config value matches the entry this
// build would write, using the format-appropriate comparison (TOML decodes
// through a type-preserving path distinct from the JSON tree).
func (c *genericClient) entryEquals(existing any, want ServerEntry) bool {
	if c.d.Format == FormatTOML {
		return TOMLEntryEquals(existing, want)
	}
	return EntryEquals(existing, want)
}

// entryValue serialises a ServerEntry into the map shape the descriptor's
// encoder expects.
func (c *genericClient) entryValue(entry ServerEntry) map[string]any {
	if c.d.Format == FormatTOML {
		return TOMLEntry(entry)
	}
	return JSONEntry(entry)
}

// serverMap locates (creating when absent) the map the mazarbul entry lives in.
// The descriptor's ServerMap override handles Claude Code's project nesting;
// otherwise it is the top-level ServersKey map.
func (c *genericClient) serverMap(doc map[string]any, scope Scope, projectRoot string) (map[string]any, error) {
	if c.d.ServerMap != nil {
		return c.d.ServerMap(doc, scope, projectRoot)
	}
	return EnsureServersMap(doc, c.d.ServersKey), nil
}

// jsoncConflict returns true (with the report populated) when the descriptor
// rejects JSONC and the on-disk file contains comments. VS Code tolerates
// JSONC; we refuse to round-trip such a file through the strict JSON parser and
// ask the user to edit it by hand.
func (c *genericClient) jsoncConflict(path, message string, rep *WriteReport) bool {
	if !c.d.RejectJSONC {
		return false
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the resolved ConfigPath value.
	if err == nil && hasJSONCComments(raw) {
		rep.Action = actionConflict
		rep.Message = message
		return true
	}
	return false
}

// Detect implements Client. It probes the user-scope config path and compares
// any existing mazarbul entry against the canonical entry this build would
// write (resolved with dataDir) so a drifted command/args/env surfaces as
// RegisteredStale rather than a false Registered.
func (c *genericClient) Detect(_ context.Context, dataDir string) (Status, error) {
	path, err := c.d.ConfigPath(ScopeUser, "")
	if err != nil {
		return StatusUnknown, err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return StatusNotInstalled, nil
		}
		return StatusUnknown, fmt.Errorf("stat %s: %w", path, statErr)
	}
	var doc map[string]any
	if c.d.RejectJSONC {
		// RejectJSONC clients are JSON: read once, reject on comments, then
		// parse the same bytes — no second read of the file.
		raw, readErr := os.ReadFile(path) //nolint:gosec // G304: path resolved by ConfigPath.
		if readErr != nil {
			return StatusUnknown, fmt.Errorf("read %s: %w", path, readErr)
		}
		if hasJSONCComments(raw) {
			return StatusRegisteredStale, nil
		}
		doc, err = ParseJSONBytes(raw, path)
	} else {
		doc, err = c.readDoc(path)
	}
	if err != nil {
		return StatusUnknown, err
	}
	// Locate the map via the same resolver Apply uses (user scope), so a
	// descriptor whose ServerMap override differs for the user scope can't make
	// Detect probe the wrong key and false-report InstalledNotRegistered.
	servers, err := c.serverMap(doc, ScopeUser, "")
	if err != nil {
		return StatusUnknown, err
	}
	existing, ok := servers[MazServerKey]
	if !ok {
		return StatusInstalledNotRegistered, nil
	}
	canon, resolveErr := ResolveEntry(dataDir)
	if resolveErr == nil && !c.entryEquals(existing, canon) {
		return StatusRegisteredStale, nil
	}
	return StatusRegistered, nil
}

// Apply implements Client. It writes the MCP server entry and, when
// --capture/--warm-start is requested for a hook-capable client (descriptor
// declares a HookSpec), the matching lifecycle-hook blocks into the client's
// separate settings file (see ApplyHooks). A server-entry conflict aborts
// before any hook is written so the user resolves it (or passes --force) first.
func (c *genericClient) Apply(ctx context.Context, scope Scope, projectRoot string, entry ServerEntry, opts ApplyOptions) (WriteReport, error) {
	rep, err := c.applyServer(scope, projectRoot, entry, opts)
	if err != nil {
		return rep, err
	}
	if c.d.Hooks == nil || rep.Action == actionConflict || (!opts.Capture && !opts.WarmStart) {
		return rep, nil
	}
	hookRep, hookErr := c.ApplyHooks(ctx, scope, projectRoot, HookCommandFromEntry(entry), HookOptions{
		Capture:   opts.Capture,
		WarmStart: opts.WarmStart,
		DryRun:    opts.DryRun,
		Force:     opts.Force,
	})
	if hookErr != nil {
		// The MCP server entry already landed; surface that so a hook failure
		// does not read as a total failure (the user can retry just the hooks).
		return rep, fmt.Errorf("MCP server entry written; hook configuration failed: %w", hookErr)
	}
	rep.Message = mergeHookMessage(rep.Message, hookRep.Action, hookRep.ConfigPath)
	return rep, nil
}

// applyServer writes the MCP server entry, returning its WriteReport. The
// hook-aware Apply orchestrates this plus ApplyHooks.
func (c *genericClient) applyServer(scope Scope, projectRoot string, entry ServerEntry, opts ApplyOptions) (WriteReport, error) {
	path, err := c.d.ConfigPath(scope, projectRoot)
	if err != nil {
		return WriteReport{}, err
	}
	rep := WriteReport{ClientID: c.d.ID, Scope: scope, ConfigPath: path}
	if c.jsoncConflict(path, msgJSONCApply, &rep) {
		return rep, nil
	}
	doc, err := c.readDoc(path)
	if err != nil {
		return rep, err
	}
	servers, err := c.serverMap(doc, scope, projectRoot)
	if err != nil {
		return rep, err
	}
	if existing, ok := servers[MazServerKey]; ok {
		if c.entryEquals(existing, entry) {
			rep.Action = actionNoop
			return rep, nil
		}
		if !opts.Force {
			rep.Action = actionConflict
			rep.Message = msgConflict
			return rep, nil
		}
	}
	servers[MazServerKey] = c.entryValue(entry)
	if opts.DryRun {
		rep.Action = actionUpdated
		rep.Message = msgDryRun
		return rep, nil
	}
	backupPath, err := c.writeDoc(path, doc)
	if err != nil {
		return rep, err
	}
	rep.BackupPath = backupPath
	rep.Action = actionUpdated
	rep.Message = c.d.RestartHint
	return rep, nil
}

// Uninstall implements Client. It removes the MCP server entry and,
// symmetrically, any Mazarbul-owned lifecycle-hook blocks for a hook-capable
// client — `--uninstall` clears everything Mazarbul wrote, with no flags
// needed. Sibling servers and sibling hooks are preserved.
func (c *genericClient) Uninstall(ctx context.Context, scope Scope, projectRoot string) (WriteReport, error) {
	rep, err := c.uninstallServer(scope, projectRoot)
	if err != nil {
		return rep, err
	}
	if c.d.Hooks == nil {
		return rep, nil
	}
	hookRep, hookErr := c.UninstallHooks(ctx, scope, projectRoot)
	if hookErr != nil {
		// The MCP server entry was already removed; surface that so a hook
		// removal failure does not read as a total failure (mirrors Apply).
		return rep, fmt.Errorf("MCP server entry removed; hook removal failed: %w", hookErr)
	}
	if hookRep.Action == actionUpdated {
		// A hooks-only removal still counts as work done, so a server-noop
		// surfaces as updated rather than misreporting "nothing to do".
		rep.Action = actionUpdated
		rep.Message = mergeHookMessage(rep.Message, hookRep.Action, hookRep.ConfigPath)
	}
	return rep, nil
}

// uninstallServer removes the MCP server entry, returning its WriteReport. The
// hook-aware Uninstall orchestrates this plus UninstallHooks.
func (c *genericClient) uninstallServer(scope Scope, projectRoot string) (WriteReport, error) {
	path, err := c.d.ConfigPath(scope, projectRoot)
	if err != nil {
		return WriteReport{}, err
	}
	rep := WriteReport{ClientID: c.d.ID, Scope: scope, ConfigPath: path}
	if c.jsoncConflict(path, msgJSONCRemove, &rep) {
		return rep, nil
	}
	doc, err := c.readDoc(path)
	if err != nil {
		return rep, err
	}
	servers, err := c.serverMap(doc, scope, projectRoot)
	if err != nil {
		return rep, err
	}
	if _, ok := servers[MazServerKey]; !ok {
		rep.Action = actionNoop
		return rep, nil
	}
	delete(servers, MazServerKey)
	backupPath, err := c.writeDoc(path, doc)
	if err != nil {
		return rep, err
	}
	rep.BackupPath = backupPath
	rep.Action = actionUpdated
	return rep, nil
}

// SupportsHooks implements HookConfigurer. True only when the descriptor
// declares a HookSpec (Claude Code today). The generic adapter satisfies
// HookConfigurer for every client, so this — not the type assertion — is the
// real capability gate Service.Apply consults.
func (c *genericClient) SupportsHooks() bool { return c.d.Hooks != nil }

// ApplyHooks implements HookConfigurer. It merges the requested hook blocks into
// the client's settings file (see HookSpec.ConfigPath) non-destructively
// (find-or-replace the Mazarbul-owned entry, preserve siblings), backs up +
// writes atomically, then re-reads to verify the entries landed. Hook files are
// always JSON, so this uses the JSON helpers directly rather than readDoc.
func (c *genericClient) ApplyHooks(_ context.Context, scope Scope, projectRoot string, hookCmd HookCommand, opts HookOptions) (WriteReport, error) {
	spec := c.d.Hooks
	if spec == nil {
		return WriteReport{ClientID: c.d.ID, Scope: scope},
			fmt.Errorf("client %s does not support lifecycle hooks", c.d.ID)
	}
	path, err := spec.ConfigPath(scope, projectRoot)
	if err != nil {
		return WriteReport{}, err
	}
	rep := WriteReport{ClientID: c.d.ID, Scope: scope, ConfigPath: path}
	if !opts.Capture && !opts.WarmStart {
		// Nothing requested: a no-op that never reads or creates the settings
		// file, so a plain apply leaves the user's hook file untouched even if a
		// future caller reaches ApplyHooks directly.
		rep.Action = actionNoop
		return rep, nil
	}
	doc, err := ReadJSONFile(path)
	if err != nil {
		return rep, err
	}
	if opts.Capture {
		UpsertHookEntry(doc, spec.CaptureEvent.Event, hookCmd.CommandLine(spec.CaptureEvent.Args...))
	}
	if opts.WarmStart {
		UpsertHookEntry(doc, spec.WarmStartEvent.Event, hookCmd.CommandLine(spec.WarmStartEvent.Args...))
	}
	if opts.DryRun {
		rep.Action = actionUpdated
		rep.Message = msgDryRun
		return rep, nil
	}
	backupPath, err := WriteJSONFileAtomic(path, doc)
	if err != nil {
		return rep, err
	}
	rep.BackupPath = backupPath
	rep.Action = actionUpdated
	if verr := c.verifyHooks(path, opts); verr != nil {
		return rep, verr
	}
	return rep, nil
}

// verifyHooks re-reads the settings file and confirms each requested Mazarbul
// hook entry is present, converting a silently-failed merge into an actionable
// error.
func (c *genericClient) verifyHooks(path string, opts HookOptions) error {
	spec := c.d.Hooks
	doc, err := ReadJSONFile(path)
	if err != nil {
		return fmt.Errorf("verify hooks: %w", err)
	}
	if opts.Capture && !HasMazHookEntry(doc, spec.CaptureEvent.Event) {
		return fmt.Errorf("verify hooks: missing %q entry after write to %s", spec.CaptureEvent.Event, path)
	}
	if opts.WarmStart && !HasMazHookEntry(doc, spec.WarmStartEvent.Event) {
		return fmt.Errorf("verify hooks: missing %q entry after write to %s", spec.WarmStartEvent.Event, path)
	}
	return nil
}

// UninstallHooks implements HookConfigurer. It strips every Mazarbul-owned hook
// entry from the settings file, leaving sibling hooks intact.
func (c *genericClient) UninstallHooks(_ context.Context, scope Scope, projectRoot string) (WriteReport, error) {
	spec := c.d.Hooks
	if spec == nil {
		return WriteReport{ClientID: c.d.ID, Scope: scope},
			fmt.Errorf("client %s does not support lifecycle hooks", c.d.ID)
	}
	path, err := spec.ConfigPath(scope, projectRoot)
	if err != nil {
		return WriteReport{}, err
	}
	rep := WriteReport{ClientID: c.d.ID, Scope: scope, ConfigPath: path}
	doc, err := ReadJSONFile(path)
	if err != nil {
		return rep, err
	}
	removed := RemoveHookEntries(doc, spec.CaptureEvent.Event)
	if RemoveHookEntries(doc, spec.WarmStartEvent.Event) {
		removed = true
	}
	if !removed {
		rep.Action = actionNoop
		return rep, nil
	}
	backupPath, err := WriteJSONFileAtomic(path, doc)
	if err != nil {
		return rep, err
	}
	rep.BackupPath = backupPath
	rep.Action = actionUpdated
	return rep, nil
}

// hasJSONCComments returns true iff raw contains a `//` or `/*` outside of a
// JSON string literal. A small state machine tracks the in-string + escape
// state so slashes inside string values ("https://…") don't trigger a false
// positive. Moved here from the VS Code adapter when the descriptor model
// absorbed the per-client structs.
func hasJSONCComments(raw []byte) bool {
	inString := false
	escaped := false
	for i := range raw {
		ch := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == '/' && i+1 < len(raw) && (raw[i+1] == '/' || raw[i+1] == '*') {
			return true
		}
	}
	return false
}
