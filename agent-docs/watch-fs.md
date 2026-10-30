# maz watch-fs — agent-agnostic passive capture

`maz watch-fs` runs a long-lived filesystem watcher that turns every text-file change under the watched directories into a **signed** `remember()` episode. It is the only passive-capture path for hookless setups: agents without a lifecycle to hook (cursor, cline, zed, warp), raw Claude/OpenAI API usage, and even a plain editor save with no agent in the loop all land memory.

Each change becomes an Episode with `source = "fs-watch:<project>:<changeKind>"`, discoverable via `maz recall` and attributed via the event's signed `EventAuthor` — unlike the competitor's unsigned blob.

## Quick start

```bash
# Watch a repo in the foreground (Ctrl-C to stop)
maz watch-fs ~/code/myrepo

# Watch the roots configured in config.toml under [watch_fs]
maz watch-fs

# Persist under launchd so it survives logout/reboot (macOS)
maz watch-fs install ~/code/myrepo
maz watch-fs status
maz watch-fs uninstall
```

## What it captures

On a text-file create/write the watcher captures the first 4 KB of the file (configurable) into the episode body, with a `[truncated: …]` marker when the file is larger. A delete of a tracked text file becomes a short "File X was deleted" episode. Binary files are skipped — both their writes and their deletes (a known-binary extension is filtered by path, so no delete event is emitted for it either).

`changeKind` (`file_change` / `file_delete`) rides in the `source` string, so no new event type is introduced — the watcher reuses the existing signed `remember` write path, which also signs (`EventAuthor`), de-duplicates (semantic dedup on the dispatcher), and extracts entities/relations automatically.

## Ignore rules

Three layers, applied in order:

1. **Default denylist** (always on): `.git`, `node_modules`, build/output dirs (`dist`, `build`, `target`, `vendor`, …), language caches (`__pycache__`, `.venv`, …), and editor/tool dirs.
2. **Binary filter**: known binary extensions are skipped up front; everything else is content-checked (NUL byte / non-UTF-8 ⇒ skipped).
3. **`.gitignore`**: each root's `.gitignore` is honoured, as are nested `.gitignore` files in subdirectories (each applies to paths beneath it, like git), plus any extra `--ignore` patterns (`.gitignore` syntax).

## Debounce & dedup

Rapid edits to one file are coalesced within a per-path debounce window (default 500 ms), so a burst of saves yields one capture. Identical content is further collapsed by the write path's semantic dedup, so re-saving an unchanged file does not spam the log.

## Scope

Captured episodes default to **project** scope (file changes are repo-bound and sync in-repo). Override with `--scope personal|project|team` or the config `scope` key. Scope is enforced on the read spine per the scope-isolation model.

## Configuration

Flags override `config.toml`. The `[watch_fs]` table:

```toml
[watch_fs]
roots = ["/Users/me/code/myrepo", "/Users/me/notes"]
scope = "project"          # personal | project | team
size_cap_bytes = 4096      # bytes of content captured per change
debounce_ms = 500          # per-path coalescing window
ignore = ["*.tmp", "scratch/"]  # extra .gitignore-syntax patterns
confidence = 0.6           # writer confidence on captured episodes (0..1)
```

| Flag | Config key | Default |
| --- | --- | --- |
| `--scope` | `scope` | `project` |
| `--size-cap` | `size_cap_bytes` | `4096` |
| `--debounce` (ms) | `debounce_ms` | `500` |
| `--ignore` (repeatable) | `ignore` | — |
| `--confidence` | `confidence` | `0.6` |

## Running under launchd (macOS)

`maz watch-fs install [dirs...]` writes `~/Library/LaunchAgents/io.mazarbul.watch-fs.plist` and loads it: the watcher starts now (`RunAtLoad`) and relaunches on every login and after a crash (`KeepAlive`). Roots default to the `[watch_fs]` config when omitted. `maz watch-fs uninstall` stops it and removes the agent.

The launchd-managed watcher is the persistent, single instance; `maz doctor` reports its status (`watch_fs.status` check), preferring the launchd job on macOS and falling back to the foreground pidfile (`<dataDir>/watch-fs.pid`) elsewhere.

## How it routes writes

Each debounced change is sent through the same daemon-or-in-process path as `maz remember`: when a Mazarbul daemon is running it routes over the daemon's MCP transport (cheap, no lock contention); otherwise it opens the kernel store in-process. Running the watcher alongside a daemon is the recommended setup.
