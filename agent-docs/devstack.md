# Devstack

Per-worktree development environment manager. Mazarbul embeds every backing service (SQLite, LadybugDB, sqlite-vec, FTS5) so there are no separate processes to start in M1. The devstack still owns the data-dir lifecycle and the worktree-spawn flow, and it is the seam where future background services (sync daemon, MCP server, webhook receivers) will land.

## Install mise

```bash
curl https://mise.run | sh
```

Verify with `mise --version`. Add the shim to your shell as the installer instructs (`~/.zshrc`, `~/.bashrc`, or `~/.config/fish/config.fish`). All Go tooling (Go itself, golangci-lint, lefthook, gofumpt, goimports, air, vhs) is installed automatically by `mise install` from `.mise.toml`.

## Per-clone setup

From inside the repo (or any worktree):

```bash
mise trust       # one-time per clone; opts into running .mise.toml tasks
mise install     # downloads and pins every tool in .mise.toml
mise run setup   # go mod download + lefthook install
```

Verify with `./scripts/devstack doctor` (see below).

## Daily workflow

```bash
mise run dev     # hot reload via air; restarts on .go file changes
mise run ci      # full local gate: fmt + lint + test + build
```

For one-off runs:

```bash
mise run build
./bin/maz --help
./bin/maz init             # TUI wizard (TTY) or plain output (--no-tui / --json / piped)
```

## devstack script

The `./scripts/devstack` script is the one-stop wrapper for environment lifecycle and worktree spawning.

| Subcommand            | Purpose                                                                                  |
|-----------------------|------------------------------------------------------------------------------------------|
| `devstack up`         | Create the data dir at `$MAZ_DATA_DIR` (default `./.mazarbul-data`).                     |
| `devstack down`       | No-op today; reserved for future background services started by `devstack up`.            |
| `devstack reset`      | Delete the data dir entirely. Combine with `devstack up` to wipe and restart.            |
| `devstack status`     | Print the resolved data dir, worktree path, branch, and data size.                       |
| `devstack doctor`     | Preflight: check `mise`, `git`, `gh`, `go` are present, mise is trusted, lefthook is wired, data dir is in the expected place. Exit non-zero if anything fails. |
| `devstack worktree <name>` | Spawn an isolated feature worktree (see below).                                     |
| `devstack cw <name>`  | Alias for `worktree`.                                                                    |

Mise aliases are wired for the most common: `mise run devstack:up`, `mise run devstack:down`.

## Spawning an isolated feature worktree

```bash
./scripts/devstack worktree event-log
```

What this does (deterministic order):

1. Creates `../mazarbul-event-log` on a new branch `feat/event-log`.
2. Trusts mise inside the new worktree (`mise trust`).
3. Installs the tool set (`mise install`).
4. Runs `mise run setup` (Go deps + lefthook).
5. Runs `mise run ci` (full gate) so the worktree starts green.
6. Captures `MAZ_DATA_DIR` from `mise env` to confirm per-worktree isolation.
7. Prints the `cd` path and the `claude` next-step, plus the cleanup commands for after the PR merges.

The name must be lowercase alphanumerics and dashes. Pick the same slug the GitHub issue uses so the branch, worktree path, and PR all line up.

Each worktree has its own `.mazarbul-data/` because `.mise.toml` declares `MAZ_DATA_DIR = "{{ config_root }}/.mazarbul-data"` and `config_root` is the worktree root, not the main repo. Two worktrees never share data.

## Reset

To wipe state in the current worktree:

```bash
./scripts/devstack reset
./scripts/devstack up      # recreates an empty data dir
```

To clean up an experimental worktree:

```bash
git worktree remove ../mazarbul-<name>
git branch -D feat/<name>
```

## Daemon logs

When the daemon supervisor (#42) lands, the daemon writes JSON-Lines logs to a rotated file. M1 ships the file sink and the reader CLI ahead of the supervisor.

| Resolution order | Path |
|---|---|
| `$MAZ_LOG_FILE` (explicit override) | the supplied path |
| `$XDG_CACHE_HOME` set | `$XDG_CACHE_HOME/mazarbul/daemon.log` |
| default | `~/.cache/mazarbul/daemon.log` |

Directory mode `0700`, file mode `0600`. Rotation via lumberjack at 10 MB with 5 historical files retained. No time-based rotation.

```bash
maz daemon logs                 # prints the resolved path (TUI panel)
maz daemon logs --no-tui        # plain path on stdout
maz daemon logs --json          # {"logFile":"/Users/.../daemon.log"}
maz daemon logs --follow        # tails the live file; Ctrl-C exits cleanly
```

`--follow` polls every 200 ms, follows rotation automatically, and exits cleanly on `SIGINT`/`SIGTERM` without signalling the daemon. Foreground `maz` commands continue to log to stderr; the file sink only activates when `Config.Log.FilePath` is non-empty (set by the supervisor at fork time).

## Future background services

When M2 lands sync (CRDT over git), `devstack up` will additionally start the sync daemon, and `devstack down` will stop it. Same pattern for webhook receivers added for connectors. The script is intentionally a thin shell wrapper so adding services means appending to the relevant `cmd_*` function, not rewriting the lifecycle.

<!-- TODO(m2): expand once sync daemon lands: PID file location, port allocation per worktree, devstack profile selection. -->
