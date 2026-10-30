---
name: dead
description: Detect and remove unused code across Go and web. USE WHEN diff is substantive (>30 LoC), after implementing a feature, when /go-strict requests dead-code audit, when Stop hook flags Cleanup-opportunities, or when user asks 'is anything unused' or 'clean up dead code'. Runs `mise run dead` (Go reachability) + `pnpm exec knip` (web unused exports/files/deps) + reports a unified punch list. NOT FOR active development , only after a feature lands.
allowed-tools: Bash(mise run dead), Bash(pnpm exec knip*), Bash(deadcode *), Bash(go *), Bash(rg *), Bash(grep *), Bash(git diff *), Read, Edit
---

# /dead , Unified dead-code detection

Detect and report unused code in one pass across both backends. Apply surgical cuts only after user confirmation.

## When to invoke

- After a feature lands (>30 LoC) and before opening a PR
- When `/go-strict` reports Cleanup-opportunities
- When Stop hook's `last-check.md` shows unreachable-code warnings
- When the user types `/dead` or asks "anything unused?"

## How it runs

### Step 1 , Scope detection

```
git diff --name-only HEAD
```

Bucket files:
- **Go** changes → run `mise run dead` (deadcode reachability scan)
- **Web** changes under `web/` → run `pnpm exec knip` (unused exports + files + deps)

Skip if neither surface changed.

### Step 2 , Run scans in parallel

| Tool | Command | What it finds |
|---|---|---|
| **deadcode** | `mise run dead` (from repo root) | Unreachable Go functions/methods |
| **knip** | `cd web && pnpm exec knip` | Unused exports, files, dependencies, types in TS/TSX |

Both are read-only , they only report, never modify.

### Step 3 , Report

Unified table:

```
File                                  | Symbol / Export      | Type      | Recommendation
internal/kernel/navigate/walk.go:142  | listOrphanedEdges    | function  | delete , no callers
web/src/lib/format-priority.ts:1      | (entire file)        | file      | delete , no imports
web/src/components/legacy/old-card.tsx| OldCard             | export    | delete , superseded by Card
go.mod / package.json                 | unused dep: lodash   | dep       | run pnpm rm lodash
```

Sum the LoC saved estimate. Ask user which cuts to apply.

### Step 4 , Apply selected cuts surgically

Only after explicit "apply X, Y, Z" from the user. Each cut is a separate Edit. Verify with `go build ./...` (for Go) or `# (no frontend)` (for web) after each batch.

## What this skill deliberately does NOT do

- Apply cuts without user approval
- Touch generated code (`gen/`, `web/src/gen/`)
- Modify audit log code (append-only, compliance-protected)
- Remove "almost dead" code that has one caller in a test file (often legitimate)
- Remove proto-generated unused types (regenerate the proto instead)

## Gotchas

- **knip false-positives on Next.js dynamic routes**: pages under `app/[param]/page.tsx` look unused but are entry points. The `web/knip.json` config lists these as `entry:`. If knip flags one, check the config first.
- **deadcode misses RPC handlers**: methods registered via reflection (MCP tool registration) aren't always reachable from `main`. Cross-check with `grep -r "MethodName" internal/api/` before deleting.
- **`init()` functions can hide live code**: deadcode treats init as a root. If you delete a file with only init-side-effect code, the side effect disappears silently.
- **Tests don't anchor production code**: a function only called by `_test.go` is still considered dead by deadcode. Decide case-by-case , sometimes it's a helper meant for both.
- **knip's `--include-entry-exports` matters**: by default knip won't flag exports from entry files. If a re-export in `index.ts` is unused, knip needs `--include-entry-exports` to see it.

## Pairs well with

- `/simplify` , dead code is one of simplify's 4 audit dimensions
- `/go-strict` , invokes `/dead` inside its auto-iteration loop
- Stop hook , writes unreachable-code findings to `.claude/last-check.md` Cleanup-opportunities
