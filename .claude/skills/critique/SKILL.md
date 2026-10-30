---
name: critique
description: Self-review of your own branch diff with clean eyes (forked Explore subagent). USE WHEN /go-strict requests a critique pass, you finished implementing >50 LoC, before opening a PR, or when user says 'critique this' or 'review my own diff'. Diffs main..HEAD, audits across 4 dimensions (correctness, simplicity, Mazarbul-specifics, test coverage), produces a punch list with file:line + suggested fix. NOT a substitute for /pr-review (which reviews OTHER PRs).
context: fork
allowed-tools: Bash(git diff *), Bash(git log *), Bash(git status), Read, Grep, Glob
---

# /critique , Self-review of own diff

Critiquing your own code with the same model that wrote it leaks implementation bias. This skill runs in a **forked Explore subagent context** (`context: fork`) , fresh eyes, no memory of why you made each choice.

## When to invoke

- After implementing a feature (>50 LoC), before `/preflight` or `/ship`
- Inside the `/go-strict` auto-iteration loop
- When you're about to ship and want a sanity check
- When the user asks "critique this" / "find the holes" / "what did I miss?"

## How it runs

### Step 1 , Read the diff

```
git diff main..HEAD
git log main..HEAD --oneline
```

If the branch hasn't diverged from main (no commits, only working-tree changes), use `git diff HEAD`.

### Step 2 , Four-dimension audit

For each changed file, ask:

**1. Correctness**
- Does the code do what the commit message / PR description claims?
- Are error paths handled, or only the happy path?
- Are there off-by-ones, nil pointers, race conditions?
- Are panics possible? Should they be?
- Are types tight (no `interface{}` / `any` smuggling)?

**2. Simplicity**
- Could 2 layers of abstraction be 1? (Karpathy principle 2)
- Are there single-use helpers that should be inlined?
- Are options structs with default-only fields hiding a 2-arg function?
- Is anything speculative , handling a case that can't happen?

**3. Mazarbul-specific invariants**
- **Event log is append-only**, no UPDATE or DELETE on event rows.
- **Provenance** populated on every event (source, confidence, timestamp).
- **Scope enforcement** on every read path that surfaces user data.
- **No proto**, types are Go structs with `json:` tags; schemas are JSON Schema.
- **Timestamps**: UTC `time.Time`, JSON RFC 3339, never `*_at`.
- **TUI colors** via `internal/tui/style.Theme` only, never raw hex.

**4. Test coverage**
- Is there a test for the failing case (the one that motivated the change)?
- Are integration tests using testcontainers, not mocks of DB?
- Do tests use the canonical naming pattern (per `scripts/check-test-names.sh`)?
- Has `go test -race -short` been run on changed packages?

### Step 3 , Produce a punch list

Markdown table with file:line, issue, and concrete suggested fix:

```
File:Line                          | Dimension     | Issue                                              | Fix
internal/mcp/tools/relate.go:142    | Mazarbul      | New tool field uses createdAt, not createTime       | Rename per AIP-142
internal/kernel/dispatcher.go:88    | Simplicity    | unused options struct field RetryCount             | Inline default , remove struct
internal/cli/log.go:33              | Correctness   | Formats timestamps in local tz, not UTC             | Use .UTC() before formatting
internal/sync/reducer.go:215        | Coverage      | supersede path has no test; convergence is critical | Add table-driven test
```

Group by severity:
- **MUST FIX** , compliance, correctness, security
- **SHOULD FIX** , simplicity, readability
- **NICE TO HAVE** , style, naming

### Step 4 , Report back

The critique is read-only. Return the punch list to the calling context. The caller (Claude in main context, or `/go-strict`) decides which findings to act on.

## What this skill deliberately does NOT do

- Modify code (read-only audit)
- Run lint or tests (that's `/verify`)
- Block the agent from continuing , outputs guidance only
- Repeat findings that lint or `mise run ci` already caught (focus on judgment-level issues)
- Pretend to be neutral when the diff is genuinely clean , say so

## Gotchas

- **`context: fork` means no shared state with caller** , pass diff info explicitly via the prompt, not via globals.
- **Don't over-critique tiny diffs** , for <30 LoC patches, a 4-dimension audit is overkill. Say "diff is too small to critique meaningfully" and exit.
- **MUST FIX should be RARE** , if you're flagging 5+ "must fix" items per skill invocation, the bar is too low. Reserve for compliance/correctness only.
- **Mazarbul-specific issues are highest-value** , generic JS/Go advice can be found in any linter. Focus on rules that lint can't catch (event-log append-only invariant, provenance on every fact, scope boundaries, AIP naming).

## Pairs well with

- `/go-strict` , invokes `/critique` between `/verify` and `/dead`
- `/preflight` , runs critique implicitly via golangci-lint + test suite
- `/pr-review` , for OTHER people's PRs; this skill is for YOUR OWN
