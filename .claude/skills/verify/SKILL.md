---
name: verify
description: Static verification of in-progress work. USE WHEN you want to confirm work before claiming done, before opening a PR, when Stop hook requests verification, or when user says 'does this work?'. Detects changed surfaces (Go/proto/web), runs only relevant fast checks (go vet, golangci-lint --new-from-rev, , # (no frontend), go test -short on changed packages). NOT FOR full pre-PR check (use /preflight) or end-to-end behavior verification (use bundled /verify which launches the app).
---

# /verify , Mid-task verification loop

Boris Cherny's **#1 tip from his canonical Jan 2 2026 thread**:

> "Probably the most important thing , give Claude a verification loop = 2-3× quality."

This skill is the on-demand version of the `Stop` hook (`.claude/hooks/stop-verify.sh`). Use it explicitly whenever you want to confirm work before progressing, without waiting for the agent to claim done.

## When to invoke

- Before opening a PR
- After a large refactor (more than ~3 files touched)
- Before saying "this is done" to the user
- When the user asks "does this work?" or "prove this works"
- Inside a `/loop` cycle that wants explicit gating

## How it runs

### Step 1 , Detect changed surfaces

```
git diff --name-only HEAD
```

Bucket files into surfaces:
- **Go** , `*.go` (excluding `gen/`)
- **Proto** , `*.proto`
- **Web** , `web/**/*.{ts,tsx}` (excluding `web/src/gen/`)
- **Terraform** , `terraform/**/*.tf`
- **SQL** , `migrations/*.sql`

### Step 2 , Run only the checks that match

Run in parallel where possible (independent commands → single Bash tool call set):

| Surface | Command(s) |
|---|---|
| Go | `go vet ./...` then `go test ./<changed-pkg>/...` (not full suite , only packages with diffs) |
| Proto | ` &&  -d` and (if proto changed) `` |
| Web | `# (no frontend currently)` |
| Terraform | `terraform fmt -check -recursive && terraform validate` (from `terraform/`) |
| SQL | `# (no atlas migrations yet) && atlas migrate lint -d file://migrations -t latest` |

### Step 3 , Report

Output exactly one of:

```
✓ verified , all checks green
```

OR

```
✗ blocked , N check(s) failed:
  - go vet: <last 5 lines of failure>
  - # (no frontend): <last 5 lines of failure>
```

Then suggest the smallest concrete next step (which file to look at, which test to run individually).

## What this skill deliberately does NOT do

- Run the full test suite if only a handful of packages changed (slow, wastes signal)
- Auto-fix failures , surface them, let the user decide
- Mask failures behind summaries , show the actual error tail
- Verify generated code that the user didn't touch

## Pairs well with

- `/simplify` , verify before simplifying so you have a green baseline
- `/go` , composite skill that wraps verify → simplify → ship
- Stop hook , same checks, but automatic at end-of-turn

## Gotchas

- **Don't run `mise run test` if only one package changed.** Slow + noisy. Use `go test ./<pkg>/...` with the actual diff scope.
- **`go vet` is fast but incomplete.** Pair with package-scoped tests, not just vet alone.
- **`# (no frontend)` already runs `tsc --noEmit`** , don't double-run `tsc` separately.
- ** passes on missing field comments if `field_behavior` is set.** AIP-192 violations may slip through , also run ``.
- **River workers don't compile-check at proto generation.** Need explicit `go vet ./internal/jobs/...`.
