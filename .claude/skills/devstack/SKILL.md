---
name: devstack
description: Per-worktree dev environment lifecycle for Mazarbul. USE WHEN user says "start the stack", "spin up devstack", "bring it up", "start the dev environment", "is the stack running", "shut it down", "tear it down", "what's running", "create a new worktree for X", "clean up old worktrees", or anything about bringing up/down local services for a worktree.
---

# devstack, local dev-environment lifecycle

`./scripts/devstack` is the canonical tool for spinning up, observing, and tearing down per-worktree dev environments. Each worktree gets its own data dir (`$MAZ_DATA_DIR`) and its own running daemon (with a unique port window).

Full reference: [`agent-docs/devstack.md`](../../../agent-docs/devstack.md).

## Hard rules

1. **Always run `./scripts/devstack up`**, never start `maz` daemons by hand from random directories; devstack wires the data dir and port correctly per worktree.

2. **After `devstack up`, verify health before declaring done.**
   ```bash
   ./scripts/devstack status
   ```
   Expected: daemon running, data dir initialized, MCP server bound. Anything else means a service crashed during startup, check logs.

3. **After `devstack down`, verify no leftover listeners** on the worktree's ports.
   ```bash
   lsof -iTCP -sTCP:LISTEN | grep maz
   ```

## Commands

```
./scripts/devstack init        # bootstrap this worktree (create data dir, seed minimal fixtures)
./scripts/devstack up          # start the daemon and any long-running services
./scripts/devstack down        # stop them
./scripts/devstack status      # services + data-dir + counts
./scripts/devstack reset       # blow away the data dir and re-init (destructive)
./scripts/devstack logs        # tail the daemon log
./scripts/devstack ports       # KEY=VAL port mapping for this worktree
./scripts/devstack doctor      # health check
```

## Worktree lifecycle

```bash
# Spawn
git worktree add ../mazarbul-<feature> -b feat/<feature>
cd ../mazarbul-<feature>
mise install && mise run setup
./scripts/devstack up

# Tear down
./scripts/devstack down
cd ..
git worktree remove mazarbul-<feature>
git branch -D feat/<feature>   # only after PR merged
```

## Common gotchas

- **`$MAZ_DATA_DIR` not set** in a fresh shell, run `mise install` first; `.mise.toml` sets it per-worktree.
- **Port collision between worktrees**, devstack picks a port window per worktree; if collision still happens, check `.env.local`.
- **Daemon won't stop**, `./scripts/devstack down --force` (uses SIGKILL after a grace period).

## What this skill is NOT

- A production deploy tool, Mazarbul is local-first; production-style deploys are out of scope.
- A migration runner, schema changes go through normal Go migrations under `migrations/`.
