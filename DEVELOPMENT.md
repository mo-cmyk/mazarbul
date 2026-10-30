# Development guide

Practical guide for working on Mazarbul locally. Read this once during onboarding, then keep it as a cheatsheet.

## Prerequisites

- [`mise`](https://mise.jdx.dev) (manages Go, golangci-lint, lefthook, gofumpt, air, vhs).
- [`gh`](https://cli.github.com) (GitHub CLI, for PR and label commands).
- Git 2.40+ (worktrees).
- A POSIX shell (bash/zsh).

You do not need to install Go, golangci-lint, or any other Go tool globally; `mise install` provisions them per-repo.

## One-time setup

```bash
git clone git@github.com:mo-cmyk/mazarbul.git
cd mazarbul
mise install          # provisions all tools from .mise.toml
mise run setup        # go mod download + lefthook install
```

`mise install` reads `.mise.toml` and pins Go, golangci-lint, lefthook, gofumpt, goimports, air, and vhs to the project versions.

## Daily workflow

```bash
# Edit code, then either:
mise run dev          # hot reload via air
# or run a one-shot:
mise run build && ./bin/maz --help
```

Before pushing:

```bash
mise run ci           # fmt + lint + test + build
```

Lefthook runs a fast subset on `git commit` and the full `mise run ci` on `git push`.

## Commands cheatsheet

| Command | Purpose |
| --- | --- |
| `mise run setup` | Install Go deps + git hooks. |
| `mise run fmt` | Run gofumpt + goimports. |
| `mise run lint` | Full golangci-lint pass. |
| `mise run lint-fast` | Fast linters only (used by pre-commit). |
| `mise run test` | All tests with race detector. |
| `mise run test-short` | Short-mode unit tests. |
| `mise run coverage` | HTML coverage report at `coverage.html`. |
| `mise run bench` | Run all benchmarks with allocations. |
| `mise run build` | Build `./bin/maz` with version ldflags. |
| `mise run dev` | Hot-reload dev loop (air). |
| `mise run demo` | Re-record TUI demo GIFs from `docs/demos/*.tape`. |
| `mise run tui:snap` | Update teatest snapshots. |
| `mise run pprof:cpu` | Open a 30s CPU profile in browser. |
| `mise run pprof:mem` | Open a heap profile in browser. |
| `mise run devstack:up` | Create the local data dir. |
| `mise run devstack:down` | Tear down the local devstack. |
| `mise run ci` | Full CI gate. |

## Worktree workflow

One feature equals one worktree equals one branch equals one PR. See [agent-docs/worktree-workflow.md](agent-docs/worktree-workflow.md) for the full procedure. The short version:

```bash
git worktree add ../mazarbul-<feature> -b feat/<feature>
cd ../mazarbul-<feature>
mise install && mise run setup
```

`MAZ_DATA_DIR` in `.mise.toml` resolves per-worktree, so each worktree owns its own SQLite/LadybugDB data dir under `.mazarbul-data/`.

## Local devstack

See [agent-docs/devstack.md](agent-docs/devstack.md). The short version:

```bash
mise run devstack:up      # create .mazarbul-data/
./bin/maz init            # initialize a fresh data dir
mise run devstack:down    # no-op; data persists between sessions
./scripts/devstack reset  # nuke .mazarbul-data/
```

## Troubleshooting

- **`golangci-lint: command not found`**: run `mise install` from the repo root.
- **Lefthook hooks not firing**: run `mise run setup` again; it reinstalls hooks.
- **Tests fail on first run after pull**: run `go mod download && mise run setup`.
- **`MAZ_DATA_DIR` points at the wrong directory**: `mise install` re-resolves `{{ config_root }}`; rerun from the worktree root.
- **`mise install` fails on a Go tool**: ensure your network proxy passes `proxy.golang.org`. Retry once; transient `go install` failures are common.
