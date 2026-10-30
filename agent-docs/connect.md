# maz connect — MCP client registration

`maz connect` registers Mazarbul's MCP server with the AI clients installed on the user's machine. It edits each client's native config file in-place, surgically, preserving all unrelated keys.

> **Not to be confused with `maz source connect`.** `maz connect` wires an AI *client*
> (Claude Code, Cursor, …) to talk to Mazarbul. `maz source connect` wires a *data source*
> (a local git repo, …) so its content is ingested *into* Mazarbul. Different direction,
> different surface — see [ingestion.md](ingestion.md) for the data-source side (#211).

## Quick start

```bash
# Interactive picker — shows all detected clients, lets you multi-select
maz connect

# Headless — register with one specific client
maz connect claude-code

# Register with every installed client at once
maz connect all

# Preview what would change, without writing anything
maz connect all --dry-run

# See current detection status
maz connect --list

# Remove the Mazarbul entry from a client's config
maz connect claude-code --uninstall
```

---

## What it does

`connect` calls `ResolveEntry` to discover how to invoke Mazarbul (`maz mcp serve`), then merges a single `"mazarbul"` key into the MCP servers map of each target client's config file. The resulting entry looks like:

```json
{
  "command": "maz",
  "args": ["mcp", "serve"]
}
```

If the data directory is non-default, an env override is included:

```json
{
  "command": "maz",
  "args": ["mcp", "serve"],
  "env": { "MAZ_DATA_DIR": "/custom/path" }
}
```

`command` is `"maz"` when the running binary resolves to the same inode as `maz` on `$PATH` (homebrew / asdf / mise installs). Otherwise it is the absolute path of the current binary.

---

## Three entry surfaces, one pipeline

| Entry surface | How it launches | Interactive? |
|---|---|---|
| `maz connect [client]` on a TTY with no args | TUI picker (`internal/tui/connect/picker.go`) | Yes |
| `maz connect <client>` or `maz connect all` | Headless apply loop | No |
| Init wizard Connect step | Embedded picker inside `maz init` wizard | Yes |
| Hub Settings · Integrations tab | Embedded picker inside `/settings` panel (tab 3) | Yes |

All four paths call the same `connect.Service` from `internal/connect/connect.go` and produce **byte-identical** config-file output. The TUI picker uses `NewEmbedded` when it is mounted inside the Settings panel, which suppresses the outer title chrome — the panel renders its own borders.

---

## Supported clients

Clients are **table-driven descriptor rows** (`internal/connect/clients/descriptors.go`),
interpreted by one generic adapter — not per-client structs. `maz connect --list`
prints this matrix (paths are macOS; VS Code-family user paths resolve per-OS):

| ID | Display name | Config file (user) | Format | Scopes |
|---|---|---|---|---|
| `claude-code` | Claude Code | `~/.claude.json` | json | user, project |
| `claude-desktop` | Claude Desktop | `~/Library/Application Support/Claude/claude_desktop_config.json` | json | user |
| `cursor` | Cursor | `~/.cursor/mcp.json` | json | user, project |
| `windsurf` | Windsurf | `~/.codeium/windsurf/mcp_config.json` | json | user |
| `vscode` | VS Code | `~/Library/Application Support/Code/User/mcp.json` | json | user, project |
| `gemini` | Gemini CLI | `~/.gemini/settings.json` | json | user, project |
| `codex` | Codex CLI | `~/.codex/config.toml` | toml | user |
| `cline` | Cline | `…/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json` | json | user |
| `roo-code` | Roo Code | `…/Code/User/globalStorage/rooveterinaryinc.roo-cline/settings/mcp_settings.json` | json | user |
| `kiro` | Kiro | `~/.kiro/settings/mcp.json` | json | user, project |
| `qwen` | Qwen Code | `~/.qwen/settings.json` | json | user, project |
| `amazon-q` | Amazon Q CLI | `~/.aws/amazonq/mcp.json` | json | user, project |
| `droid` | Factory Droid | `~/.factory/mcp.json` | json | user |
| `lm-studio` | LM Studio | `~/.lmstudio/mcp.json` | json | user |
| `json-mcp` | Generic bridge | `~/.config/mazarbul/mcp.json` | json | user |

The VS Code row uses the key `"servers"` (not `"mcpServers"`) and refuses to
write a JSONC file (one with comments), asking the user to edit by hand. Codex
reads/writes TOML (`mcp_servers`, underscored) via `tomlfile.go`; every other
row is JSON via `jsonfile.go`. Claude Desktop sets a `RestartHint` surfaced
after a successful apply.

### Deliberately not auto-wired

Surfaced in the `maz connect --list` footnote, because their config does not fit
the standard `{command, args, env}`-under-a-key shape:

- **copilot-cli** — requires a per-server `type: "local"` + `tools` field.
- **opencode** — uses an `mcp` key with command-as-array + `type: "local"`.
- **zed** — uses `context_servers` with a nested `command.{path,args}`.
- **continue** — current config is YAML (`config.yaml`), not JSON.
- **kilo-code** — config format in flux (moved off the globalStorage path).
- **warp** — MCP servers are GUI-managed; no writable config file.

For any of these (or any unlisted MCP client), the generic **`json-mcp`** row
writes a standalone `~/.config/mazarbul/mcp.json` you can point the client at.

---

## Scopes

Three scope values exist:

| Scope | Meaning |
|---|---|
| `user` | Written to the per-user global config file (default) |
| `project` | Written to a project-local config file, scoped to `--project` (defaults to CWD) |
| `workspace` | VS Code workspace `.code-workspace` override |

Requesting a scope a client does not support returns a validation error before any file is touched. Scope is cycle-able in the TUI picker with `s`.

---

## Atomic-write contract

Every config write follows a strict sequence that keeps the file consistent even if the process dies mid-write.

**Backup before first touch.** `SnapshotIfNew` (in `internal/connect/backup.go`) copies the file to `<path>.maz-backup-<unix-ts>` before the first write per process-minute. A second call within the same minute is a no-op. The backup path is returned in `WriteReport.BackupPath` and shown by the CLI.

**Parse-merge-write.** The adapter:
1. Reads the existing file into a `map[string]any` (JSON) or `map[string]any` (TOML).
2. Locates or creates the `mcpServers` / `servers` / `mcp_servers` map.
3. Writes only the `"mazarbul"` key. All other keys and servers are preserved.
4. Marshals the full document back to a `.maz-tmp-*` file in the same directory (`os.CreateTemp`).
5. `fsync`s and `chmod 0600`s the temp file.
6. `os.Rename`s the temp file over the original (atomic on POSIX; near-atomic on Windows).

**Idempotent.** If the existing entry is byte-identical to what would be written, `Apply` returns `action: "noop"` without touching the file. No backup is created for a noop run.

**Conflict detection.** If an existing `"mazarbul"` key is present but differs from what would be written today (e.g. the binary moved, or a previous version wrote different args), `Apply` returns `action: "conflict"` and the message `"existing mazarbul entry differs; pass --force to overwrite"`. No file is touched. Pass `--force` to overwrite.

**Dry-run.** With `ApplyOptions.DryRun: true`, the adapter returns the intended `WriteReport` with `action: "updated"` and `message: "dry run"` without writing anything.

---

## CLI flags

```
maz connect [client] [flags]

Flags:
  --scope string    registration scope: user | project (default "user")
  --project string  project root for --scope=project (defaults to CWD)
  --dry-run         print the intended change without writing
  --force           overwrite a conflicting existing mazarbul entry
  --uninstall       remove the mazarbul entry (and any hooks) instead of applying
  --list            print the static supported-agents matrix and exit
  --capture         also install a passive-capture lifecycle hook (Claude Code Stop)
  --warm-start      also install a warm-start lifecycle hook (Claude Code SessionStart)
```

`--list` prints the **static capability matrix** (the table above) — no disk
I/O, no detection — so it is stable and pasteable into the README. Add
`--output=json` for a machine-readable array. The footnote lists the
deliberately-unsupported clients.

**Apply and uninstall honor `--output=json`** too: an array of per-client write
reports (`clientId`, `scope`, `action`, `configPath`, optional `backupPath` /
`message` / `error`), so scripted callers branch on the document instead of
parsing the human rows. Per the global output convention a bare `maz connect
<client>` emits text on a TTY and JSON when piped; pass `--output=text` to force
the human rows. The exit code is the primary signal regardless of format:
non-zero if any target failed.

A bare `maz connect` (no client, non-TTY) instead prints the **live detection
table** of what is installed/registered on this machine:

```
ID                  Name                Status
──────────────────  ──────────────────  ─────────────────
claude-code         Claude Code         registered
claude-desktop      Claude Desktop      not-installed
cursor              Cursor              not-registered
```

Status values: `registered`, `stale` (registered but args differ from current), `not-registered` (installed but not configured), `not-installed`, `unknown`.

---

## Ambient capture & warm-start (Claude Code)

`--capture` and `--warm-start` additionally install Claude Code **lifecycle hooks** so a session passively builds memory and warms its context — no explicit `remember`/`getContext` call required.

```bash
# Register the MCP server AND install both hooks
maz connect claude-code --capture --warm-start

# One flag at a time is fine
maz connect claude-code --capture            # capture only
maz connect claude-code --warm-start         # injection only

# Remove everything Mazarbul wrote (server entry + hooks)
maz connect claude-code --uninstall
```

**Two files, deliberately.** Claude Code reads MCP servers from `~/.claude.json` but **hooks from a different file** — `~/.claude/settings.json` (user scope) or `<project>/.claude/settings.json` (project scope). The hook writer (`ApplyHooks` in `claude_code.go`) targets settings.json with its own backup + atomic write, mirroring the MCP-entry path. The hook command line is derived from the same `ResolveEntry` result as the MCP entry, so both stay in lockstep (same binary, same `MAZ_DATA_DIR` decision; Claude Code command hooks carry no env map, so a non-default data dir is inlined as a `MAZ_DATA_DIR=… maz hook …` prefix).

The blocks written:

```json
{
  "hooks": {
    "Stop":         [ { "hooks": [ { "type": "command", "command": "maz hook stop" } ] } ],
    "SessionStart": [ { "hooks": [ { "type": "command", "command": "maz hook session-start --warm-start" } ] } ]
  }
}
```

The merge is **non-destructive**: a Mazarbul-owned entry is found-or-replaced (keyed by the `hook ` substring in its command) and sibling hooks the user added are preserved. `--uninstall` strips only the Mazarbul entries.

**What the hooks do** (see `maz hook`, `internal/cli/hook.go`):

- **Stop** fires at the end of every assistant turn. `maz hook stop` reads the Stop payload on stdin and captures the transcript turns added *since the last fire* (a per-session byte cursor under `<dataDir>/hooks/sessions/`) as a signed `remember` episode — `actor=assistant`, `source=hook:claude-code/stop`, confidence 0.6. One episode per delta. The episode is stored durably and is searchable via `recall`/FTS/vector, but passive captures (any `hook:`-prefixed source) are deliberately **excluded from automatic entity/relation extraction** — the Stop hook fires every turn, so auto-extracting each one would flood the graph with mis-typed, low-signal entities. Deliberate `remember` calls still get full extraction.
- **SessionStart** fires when a session opens. `maz hook session-start --warm-start` emits a scope-filtered context brief (reusing `getContext` / `contextbrief` — no second assembly path) as `hookSpecificOutput.additionalContext`.

Both commands are strictly **fail-open**: they always exit 0 and never block the session, even when the daemon is down, stdin is malformed, or the transcript is missing.

**Privacy posture.** Capture defaults *on* once `--capture` is set (it is cheap and the point of the feature); warm-start injection is *opt-in* via `--warm-start` (silent per-turn injection burns token budgets). `maz connect` prints the posture after a successful apply and points at `--uninstall` to disable. Captured events carry a signed ed25519 `EventAuthor` (#209/#220), so passively-captured memory is cryptographically attributable.

**Scope.** This is Claude Code only — the one client (besides Codex) with a real lifecycle-hook system. Adapters opt in by implementing the `connect.HookConfigurer` interface; `Service.Apply` rejects `--capture`/`--warm-start` for clients that do not. Codex hooks and a commit↔session graph-edge hook are tracked as follow-ups.

---

## TUI picker keys

| Key | Action |
|---|---|
| `j` / `↓` | move cursor down |
| `k` / `↑` | move cursor up |
| `space` | toggle selection (disabled for not-installed rows) |
| `s` | cycle scope (user → project → workspace) |
| `Enter` | apply selected clients |
| `q` / `Esc` / `Ctrl+C` | quit (standalone) |

---

## Where to look in code

| Concern | Path |
|---|---|
| Orchestrator + interfaces | `internal/connect/connect.go`, `internal/connect/types.go` |
| Descriptor model + path helpers | `internal/connect/descriptor.go` |
| Generic adapter (interprets a descriptor) | `internal/connect/generic.go` |
| Client descriptor table + `All()` / `Matrix()` | `internal/connect/clients/descriptors.go` |
| JSON read/write helpers | `internal/connect/jsonfile.go` |
| TOML read/write helpers | `internal/connect/tomlfile.go` |
| Backup logic | `internal/connect/backup.go` |
| Entry resolution (`maz mcp serve`) | `internal/connect/resolve.go` |
| Hook-writer interface + merge helpers | `internal/connect/hooks.go` |
| `maz hook` capture / warm-start commands | `internal/cli/hook.go`, `internal/cli/hook_capture.go` |
| TUI picker (standalone + embedded) | `internal/tui/connect/picker.go` |
| CLI command | `internal/cli/connect.go` |
| Hub Settings · Integrations tab | `internal/tui/hub/panels/settings/settings.go` (tab 3 = `tabIntegrations`) |

---

## How to add a new client adapter

Adding a client is a **descriptor row + a test fixture — no new Go struct**.

1. Append a `connect.Descriptor` row to the `descriptors` table in
   `internal/connect/clients/descriptors.go`:

   ```go
   {
       ID:          "my-client",
       DisplayName: "My Client",
       Format:      connect.FormatJSON,   // or FormatTOML
       ServersKey:  "mcpServers",         // the top-level container key
       Scopes:      []connect.Scope{connect.ScopeUser, connect.ScopeProject},
       ConfigPath:  connect.ScopedFile([]string{".myclient", "mcp.json"}, []string{".myclient", "mcp.json"}),
   },
   ```

   Path helpers: `HomeFile` (one file under `~`), `ScopedFile` (user under `~`,
   project under the project root), `OSHomeFile` (per-GOOS segments under `~`,
   for OS-specific application-support paths). Every client must support the
   **user** scope — `Detect` probes it.

2. Outlier knobs are descriptor *fields*, not new types:
   - `OSHomeFile(map[string][]string{...})` for OS-specific paths (Claude
     Desktop, VS Code user, Cline/Roo globalStorage).
   - `ServerMap` for a nested server map (Claude Code nests project entries
     under `projects[absRoot].mcpServers` in the single user file).
   - `RejectJSONC: true` to refuse comment-bearing files (VS Code).

3. Add a fixture/round-trip test if needed; `clients/descriptors_test.go` already
   round-trips a representative JSON, TOML, and the generic bridge, and asserts
   every row is well-formed.

4. Update the client table in this doc.

Clients whose server stanza diverges from `{command, args, env}` (opencode, zed,
copilot-cli, …) are **not** added as rows — they go in the `knownUnsupported`
list with a reason, surfaced in the `--list` footnote.

## Teaching the agent when to use memory (preset on-ramp)

Connecting exposes the tools; it does not tell the agent *when* to call them.
The **`agent-memory`** builtin preset (#210) is that on-ramp — an
instructions-only steering pack (no ontology, `internal/plugin/schemapack/packs/agent-memory/`)
whose body teaches the agent to warm up with `getContext`, `recall` before
answering, and `remember` after learning. It surfaces through the `getContext`
"Operating instructions" section.

```bash
maz preset install agent-memory
```

`maz init` offers it: the interactive form asks "Teach your agent when to use
Mazarbul?" (default yes), and non-interactively it is opt-in via
`maz init --preset=agent-memory` (a bare `--yes` does not install it, to avoid
silently injecting steering instructions). The post-`connect` and post-`init`
output both point at the command. `schemapack.DefaultSteeringPreset` names the
preset so the wiring and docs share one constant.
