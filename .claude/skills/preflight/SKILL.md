---
name: preflight
description: Heavy pre-PR check, equivalent to local CI dry-run. USE WHEN ready to open a PR, when user says 'is this ready to ship', when /go-strict completes its loop and asks for final gate, or before merging a critical change. Runs full golangci-lint (not --new-from-rev), go test -race, , mise run dead, /* govulncheck disabled for now */, # (no frontend)+test+typecheck, knip. Output: ✓ all green OR ✗ N issues with punch list. NOT FOR mid-task verification (use /verify , much faster).
allowed-tools: Bash(make *), Bash(golangci-lint *), Bash(buf *), Bash(/* govulncheck disabled for now */ *), Bash(pnpm *), Bash(go test *), Bash(go vet *), Bash(deadcode *), Bash(atlas migrate *), Bash(git diff *), Bash(git status)
effort: high
---

# /preflight , Heavy pre-PR check

The `/verify` skill is fast and surface-scoped (changed files only). `/preflight` is the heavyweight version , full lint, race tests, vulnerability scan, breaking-change detection. This is what CI runs.

## When to invoke

- Right before opening a PR (after `/go-strict` completes)
- When the user asks "is this ready to ship?"
- Before merging a critical change (migrations, audit-log, permission changes)
- When `/go-strict` reaches its iteration cap and wants final validation
- NOT for mid-task work , too slow (multi-minute runtime)

## How it runs

### Step 1 , Detect changed surfaces (broader than /verify)

```
git diff --name-only main..HEAD
git diff --name-only HEAD  # uncommitted
```

Bucket into surfaces:
- **Go** , `*.go` (excluding `gen/`)
- **Proto** , `*.proto` (any change →  check vs main)
- **Web** , `web/**/*.{ts,tsx,js,jsx}` (excluding `web/src/gen/`)
- **Migrations** , `migrations/*.sql`
- **Terraform** , `terraform/**/*.tf`

### Step 2 , Run the full matrix (parallel where independent)

| Surface | Commands |
|---|---|
| **Go** | `gofmt -l .` <br>`go vet ./...` <br>`golangci-lint run --timeout=5m` (full, not --new-from-rev) <br>`go test -race -short ./...` <br>`mise run dead` (deadcode reachability) <br>`/* govulncheck disabled for now */ ./...` (CVE scan, source mode) |
| **Proto** |  <br>` -d` <br>` --against ".git#branch=main"` <br>`` (Google AIP compliance) |
| **Web** | `# (no frontend currently)` <br>`# (no frontend currently) --max-warnings=0` <br>`# (no frontend currently)` <br>`# (no frontend currently)` |
| **Migrations** | `# (no atlas migrations yet)` <br>`atlas migrate lint -d file://migrations -t latest` |
| **Terraform** | `terraform fmt -check -recursive` <br>`terraform validate` (from `terraform/`) |

### Step 3 , Report

Single consolidated punch list:

```
✓ Go: gofmt, go vet, golangci-lint, go test -race, mise run dead, /* govulncheck disabled for now */
✓ Proto: , , , 
✗ Web: # (no frontend) (3 errors), knip (2 unused exports)
✓ Migrations: atlas migrate lint
n/a Terraform: no changes

Failures:
  internal/kernel/recall.go:42  undefined: rankOpts
  internal/kernel/recall_test.go:88  TestRecall_Empty failed
  internal/mcp/tools/get.go:115  errcheck: Error return value is not checked

Cleanup-opportunities:
  web/src/legacy/format.ts  knip: unused export 'formatLegacy'
  web/src/utils/old.ts      knip: unused file

3 blocking issues. Fix before opening PR.
```

Or:

```
✓ All checks green , ready to ship.
```

### Step 4 , Exit cleanly

This is read-only. Do NOT attempt to auto-fix at this stage , the goal is binary signal (ship / don't ship). If issues exist, surface them and let the user / `/go-strict` decide.

## What this skill deliberately does NOT do

- Auto-fix anything (read-only , `/go-strict` is the fix-loop wrapper)
- Run integration tests with testcontainers (too slow for preflight; CI handles that)
- Run e2e tests (separate workflow)
- Modify any code
- Skip checks "to save time" , preflight is the slow path on purpose

## Pairs well with

- `/go-strict` , calls `/preflight` as final iteration gate
- `/ship` , should call `/preflight` before `gh pr create`
- `/review` , call `/preflight` before re-pushing fixes to a PR branch
- `/verify` , fast version for mid-task; preflight is final gate

## Gotchas

- **`golangci-lint run` without `--new-from-rev` is much slower** (~30-60s vs ~5s). Worth it for the comprehensive view but don't use in mid-task loops.
- ** needs an `--against` ref** , `.git#branch=main` works in CI, but locally the agent must be on a branch with `main` upstream tracked.
- **`/* govulncheck disabled for now */` source mode** (no `-mode=binary`) is the right one for dev loop. Runs against the module's call graph; takes ~10-30s.
- **`# (no frontend)` with Vitest** can hang if there are unfinished promises. Use `--reporter=basic --pool=forks --no-watch`.
- **`atlas migrate lint`** requires `migrations/atlas.sum` to be current. If you added a migration without `# (no atlas migrations yet)`, lint will fail with "checksum mismatch" , run hash first.
- **`knip` may flag entry files** if the `web/knip.json` `entry:` glob is wrong. Update knip.json, don't suppress the warning.
