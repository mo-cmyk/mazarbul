---
name: inspect-deploy
description: Show what is actually running locally or in a hosted environment for Mazarbul. USE WHEN user asks "what version am I running?", "is the daemon running?", "is the MCP server up?", or there's a suspected version mismatch. NOT FOR runtime errors (use /inspect-runtime) or storage state (use /inspect-db).
allowed-tools:
  - Bash(ps *)
  - Bash(lsof *)
  - Bash(curl *)
  - Bash(./bin/maz *)
---

# Deploy inspection

Mazarbul is local-first. The "deploy" surface is whatever processes are running on the user's machine plus any hosted Imladris instance the user opted into.

## Triage tree

### 1. Is the daemon running?

```bash
ps aux | grep -i 'maz mcp serve' | grep -v grep
```

Or:

```bash
./bin/maz status
```

Watch for:
- Multiple `maz mcp serve` processes (port collision risk).
- Daemon running from an old binary path (after `mise run build`).

### 2. What version is running?

```bash
./bin/maz --version
ps -o command= -p <PID>
```

Compare to:

```bash
git log -1 --format='%h %s' HEAD
git describe --tags --always --dirty
```

A drift means the daemon was started from a stale build. Rebuild and restart.

### 3. Which ports are bound?

```bash
lsof -iTCP -sTCP:LISTEN -n -P | grep maz
```

The HTTP API (if running) binds the configured port; the pprof endpoint (if `--debug-addr` set) binds another.

### 4. Hosted Imladris (if opted in)

When implemented, surface via `maz sync status`. Stub for now.

## Common gotchas

- **Stale daemon after pulling a branch**, always stop the daemon, rebuild, restart.
- **Port collision between worktrees**, each worktree should pick a different port via env or flag.
- **`maz status` reports stale info**, the daemon caches state; restart it or call `maz refresh`.

## What this skill is NOT

- Cloud-DB ops, see `/inspect-db` for the four-layer storage.
- Runtime trace triage, see `/inspect-runtime`.
