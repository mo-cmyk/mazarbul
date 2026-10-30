package connect

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// HookConfigurer is the optional adapter capability for clients that can
// additionally write lifecycle-hook blocks (passive capture + warm-start)
// alongside the MCP server entry. The single generic adapter implements this
// interface for every client, so SupportsHooks reports whether THIS client's
// descriptor actually declares a hook spec; Service.Apply rejects a
// --capture/--warm-start request when SupportsHooks is false rather than
// silently dropping it.
//
// The hook config file is the client's own (for Claude Code it is
// ~/.claude/settings.json — a DIFFERENT file from the ~/.claude.json that
// carries the MCP server entry), so implementations own their own path
// resolution, backup, and atomic write.
type HookConfigurer interface {
	// SupportsHooks reports whether this client can write lifecycle hooks
	// (i.e. its descriptor declares a HookSpec). The generic adapter satisfies
	// HookConfigurer for all clients, so callers must consult this rather than
	// rely on the type assertion alone.
	SupportsHooks() bool
	// ApplyHooks writes the requested hook blocks. hookCmd carries the
	// resolved `maz` invocation (binary + MAZ_DATA_DIR env) so the written
	// command line self-locates its data dir in a fresh hook process.
	ApplyHooks(ctx context.Context, scope Scope, projectRoot string, hookCmd HookCommand, opts HookOptions) (WriteReport, error)
	// UninstallHooks removes every Mazarbul-owned hook entry, preserving
	// sibling hooks the user added.
	UninstallHooks(ctx context.Context, scope Scope, projectRoot string) (WriteReport, error)
}

// HookSpec is a client's declarative lifecycle-hook capability, carried on its
// Descriptor. A non-nil HookSpec is what makes genericClient.SupportsHooks
// return true; only Claude Code sets one today. The hook config file is the
// client's own settings file (distinct from the MCP ConfigPath), and the two
// events bind a client hook-event name to the `maz` subcommand the hook runs.
type HookSpec struct {
	// ConfigPath resolves the settings file hooks are written to (for Claude
	// Code ~/.claude/settings.json or <projectRoot>/.claude/settings.json — a
	// different file from the descriptor's MCP ConfigPath).
	ConfigPath func(scope Scope, projectRoot string) (string, error)
	// CaptureEvent is the hook event + maz subcommand for passive capture
	// (Claude Code: event "Stop", args {"hook","stop"}).
	CaptureEvent HookEvent
	// WarmStartEvent is the hook event + maz subcommand for warm-start
	// injection (Claude Code: "SessionStart", {"hook","session-start","--warm-start"}).
	WarmStartEvent HookEvent
}

// HookEvent binds a client hook-event name to the `maz` subcommand args the
// hook command line invokes for that event.
type HookEvent struct {
	// Event is the client's hook-event key (e.g. "Stop", "SessionStart").
	Event string
	// Args are the `maz` subcommand args the hook runs (e.g. {"hook","stop"}).
	Args []string
}

// mergeHookMessage appends a one-line note about the hook write to an existing
// report message (e.g. the server-entry dry-run/conflict message).
func mergeHookMessage(existing, hookAction, hookPath string) string {
	note := fmt.Sprintf("hooks %s → %s", hookAction, hookPath)
	if existing == "" {
		return note
	}
	return existing + "; " + note
}

// HookOptions selects which hook blocks ApplyHooks writes.
type HookOptions struct {
	// Capture installs the passive-capture hook (Stop).
	Capture bool
	// WarmStart installs the warm-start injection hook (SessionStart).
	WarmStart bool
	// DryRun shapes the in-memory config without touching disk.
	DryRun bool
	// Force is reserved for parity with ApplyOptions; hook merges are
	// non-destructive (find-or-replace the Mazarbul entry), so it is unused
	// today.
	Force bool
}

// HookCommand carries the resolved `maz` binary plus the env that must precede
// it on a hook command line. It is derived from the same ServerEntry the MCP
// registration writes (HookCommandFromEntry) so the hook command and the MCP
// entry stay in lockstep: same binary, same MAZ_DATA_DIR decision.
type HookCommand struct {
	// Command is the resolved maz binary (bare "maz" or an absolute path).
	Command string
	// Env holds vars inlined ahead of the command (currently only
	// MAZ_DATA_DIR, and only when the data dir is non-default). Claude Code
	// command hooks carry no env map, so the values are inlined as
	// `KEY=VALUE` shell prefixes by CommandLine.
	Env map[string]string
}

// HookCommandFromEntry builds a HookCommand from the MCP ServerEntry resolved
// for this Apply, reusing its binary path and MAZ_DATA_DIR env verbatim.
func HookCommandFromEntry(entry ServerEntry) HookCommand {
	return HookCommand{Command: entry.Command, Env: entry.Env}
}

// CommandLine renders the shell command string for a `maz` subcommand,
// inlining Env as sorted `KEY=VALUE` prefixes. Values and the binary path are
// shell-quoted when they contain anything outside a conservative safe set, so
// a data dir or install path with spaces does not break the hook.
func (h HookCommand) CommandLine(args ...string) string {
	var b strings.Builder
	keys := make([]string, 0, len(h.Env))
	for k := range h.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(h.Env[k]))
		b.WriteByte(' ')
	}
	b.WriteString(shellQuote(h.Command))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

// shellQuote wraps s in single quotes (escaping embedded single quotes) when it
// contains a character outside the safe set [A-Za-z0-9_/.:-]. The empty string
// renders as ”.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isShellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '/' || r == '.' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}

// commandIsMazHook reports whether a hook command line is one Mazarbul wrote.
// A Mazarbul hook invokes the `maz` (or `mazarbul`) binary with `hook stop` /
// `hook session-start`, e.g. `MAZ_DATA_DIR=/d /usr/local/bin/maz hook stop`.
//
// Ownership is decided structurally, not by substring: we tokenize the command
// (honoring the single-quote shell-quoting CommandLine emits), skip any leading
// `KEY=VALUE` env-assignment tokens, and require that the FIRST remaining token
// — the invoked program — has basename `maz`/`mazarbul`, followed by the `hook`
// subcommand. So we own `maz hook stop` and `MAZ_DATA_DIR=/d /opt/maz hook stop`
// but NOT `logger maz hook stop`, `echo maz hook stop`, or `my-tool hook stop`
// (where `maz` is an argument or a different binary) — those are other people's
// hooks and must be preserved on apply and left alone on uninstall. A
// `matcher`-based sentinel is avoided because Claude Code may filter a
// Stop/SessionStart entry out entirely when an unmatched matcher is present.
func commandIsMazHook(cmd string) bool {
	toks := shellTokens(cmd)
	i := 0
	for i < len(toks) && isEnvAssignment(toks[i]) {
		i++
	}
	if i >= len(toks) {
		return false
	}
	if base := commandBase(toks[i]); base != "maz" && base != "mazarbul" {
		return false
	}
	rest := toks[i+1:]
	return len(rest) >= 2 && rest[0] == "hook" &&
		(rest[1] == "stop" || rest[1] == "session-start")
}

// shellTokens splits a command line into tokens on unquoted whitespace,
// honoring single-quote regions and backslash escapes — the only quoting
// CommandLine/shellQuote emit. A single-quoted value with embedded spaces (e.g.
// a MAZ_DATA_DIR with a space) stays one token.
func shellTokens(s string) []string {
	var toks []string
	var cur strings.Builder
	inSingle, started := false, false
	flush := func() {
		if started {
			toks = append(toks, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(c)
				started = true
			}
		case c == '\'':
			inSingle, started = true, true
		case c == '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				started = true
			}
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return toks
}

// isEnvAssignment reports whether tok is a leading `KEY=VALUE` env assignment
// (KEY is a shell identifier), i.e. a prefix that precedes the invoked binary.
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := range eq {
		c := tok[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// commandBase returns the basename of a (possibly path-qualified) binary token.
func commandBase(tok string) string {
	if idx := strings.LastIndexByte(tok, '/'); idx >= 0 {
		return tok[idx+1:]
	}
	return tok
}

// UpsertHookEntry inserts or replaces Mazarbul's command-hook entry for the
// given event name within doc["hooks"][event], preserving sibling entries the
// user added. The command argument is the full shell command line to run.
func UpsertHookEntry(doc map[string]any, event, command string) {
	hooks := EnsureServersMap(doc, "hooks")
	arr, _ := hooks[event].([]any) //nolint:errcheck // a non-array value is treated as no entries and replaced below.
	entry := newMazHookEntry(command)
	for i, e := range arr {
		if hookEntryIsMaz(e) {
			arr[i] = entry
			hooks[event] = arr
			return
		}
	}
	hooks[event] = append(arr, entry)
}

// RemoveHookEntries deletes every Mazarbul-owned command-hook entry from
// doc["hooks"][event], dropping the event key when it empties (and the hooks
// map when it empties). Returns true if anything was removed.
func RemoveHookEntries(doc map[string]any, event string) bool {
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return false
	}
	arr, ok := hooks[event].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(arr))
	removed := false
	for _, e := range arr {
		if hookEntryIsMaz(e) {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	if len(hooks) == 0 {
		delete(doc, "hooks")
	}
	return true
}

// HasMazHookEntry reports whether doc["hooks"][event] contains a Mazarbul-owned
// command-hook entry. Used for post-write verification.
func HasMazHookEntry(doc map[string]any, event string) bool {
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return false
	}
	arr, ok := hooks[event].([]any)
	if !ok {
		return false
	}
	for _, e := range arr {
		if hookEntryIsMaz(e) {
			return true
		}
	}
	return false
}

// newMazHookEntry builds the Claude Code hook-event entry that runs command.
// Stop/SessionStart entries carry no matcher (matchers filter by tool name and
// apply only to PreToolUse/PostToolUse).
func newMazHookEntry(command string) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	}
}

// hookEntryIsMaz reports whether a hook-event array element is one Mazarbul
// wrote, detected when any inner command satisfies commandIsMazHook.
func hookEntryIsMaz(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok && commandIsMazHook(cmd) {
			return true
		}
	}
	return false
}
