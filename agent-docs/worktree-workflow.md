# Worktree workflow

One feature, one worktree, one Claude session, one branch, one PR. Each worktree owns its own data dir so concurrent Claude sessions never collide.

## Principle

A worktree is a cheap way to spin up an isolated workspace from the same repo. For Mazarbul, the isolation extends to runtime data: `MAZ_DATA_DIR` resolves per-worktree via `{{ config_root }}` in `.mise.toml`, so each worktree has its own SQLite, LadybugDB, vector index, and event log. Two Claude sessions on the same machine can run `maz` against fully separate state.

## Spawn

```bash
git worktree add ../mazarbul-<feature> -b feat/<feature>
cd ../mazarbul-<feature>
mise install
mise run setup
mise run devstack:up
claude   # then: "Read AGENTS.md and start work on <task>"
```

Tools install per worktree (mise resolves from `.mise.toml`). Devstack creates the worktree-local data dir under `MAZ_DATA_DIR`. Claude starts fresh in the new directory; `.claude/settings.local.json` is gitignored and per-worktree.

## Isolation guarantees

- `.mise.toml` sets `MAZ_DATA_DIR = "{{ config_root }}/.mazarbul-data"`. The `{{ config_root }}` template resolves to the worktree root, so the path is unique per worktree.
- Each worktree gets its own SQLite/LadybugDB/vector index under `.mazarbul-data/`. `.mazarbul-data/` is gitignored.
- Each worktree owns `.claude/settings.local.json`. Project-shared settings live in `.claude/settings.json` (committed); local overrides land in `.local.json` (gitignored).
- `lefthook install` runs once per worktree on `mise run setup`. Hooks fire only inside the worktree they were installed for.

## Coordination

No direct agent-to-agent talk between worktrees. Coordination happens **async** via:

- The issue tracker, once issues exist.
- PR comments on overlapping work.
- Convention: each feature PR declares paths it owns; cross-cutting changes need explicit review.

Conflicts on shared paths are resolved by reverting one side and replanning. Lots of small PRs beats few big ones.

## Cleanup

After the PR merges:

```bash
git worktree remove ../mazarbul-<feature>
git branch -D feat/<feature>
```

`git worktree remove` refuses if there are uncommitted changes. Branch deletion is local-only; the merged branch on the remote is GitHub's responsibility.

## Verified working

Smoke-tested 2026-05-23 against the initial scaffold (commit `623ab6e`). Every step ran on a fresh worktree spawned from `main`, on macOS / arm64, with mise managing all tools.

```text
$ git worktree add ~/code/mazarbul-smoketest -b smoke/setup
Preparing worktree (new branch 'smoke/setup')
HEAD is now at 623ab6e chore: initial scaffold with full agent infrastructure

$ cd ~/code/mazarbul-smoketest
$ mise trust
mise trusted ~/code/mazarbul-smoketest

$ mise env --shell bash | grep MAZ_DATA_DIR
export MAZ_DATA_DIR=~/code/mazarbul-smoketest/.mazarbul-data
  # confirmed: the worktree resolves to its own data dir, not the main repo's

$ mise install
mise all tools are installed

$ mise run setup
[setup] $ go mod download
[setup] $ lefthook install
sync hooks: ok (pre-commit, pre-push, commit-msg)

$ mise run ci
[fmt] ... [lint] 0 issues. [test] ok ... [build] bin/maz
Finished in ~7s

$ ./bin/maz init --no-tui
Mazarbul initialized at ~/code/mazarbul-smoketest/.mazarbul-data
  # confirmed: init writes to the worktree's data dir, not the main repo's

$ cd ~/code/mazarbul
$ git worktree remove ~/code/mazarbul-smoketest --force
$ git branch -D smoke/setup
```

Two invariants confirmed by the smoke test:

1. **Isolation:** `MAZ_DATA_DIR` from `.mise.toml` (`{{ config_root }}/.mazarbul-data`) resolves to the worktree's own path. Two worktrees do not share a data dir.
2. **Toolchain reproducibility:** a fresh worktree needs only `mise trust && mise install && mise run setup && mise run ci` to reach a green build.

Re-run this smoke test after any change to `.mise.toml`, `lefthook.yml`, `.golangci.yml`, or `go.mod`. If the invariants stop holding, fix before merging.
