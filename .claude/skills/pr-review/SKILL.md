---
name: pr-review
description: Review someone else's pull request. USE WHEN user asks 'review PR #N', 'look at this PR', or you need to give feedback on an existing PR. Applies Google Engineering Practices ordering (design > functionality > complexity > tests > naming > comments > style) plus Mazarbul-specific checks (event log, MCP, CLI, privacy). NOT FOR reviewing your OWN diff (use /critique) or fixing your own PR's CI (use /review).
---

# PR Review Guide

## Diff-size guard , switch to lens-reviewer mode for large PRs

For high-risk PRs, a single reviewer dilutes attention across security, perf, and tests. Switch to a 3-person Agent Team of lens specialists when:

- `gh pr diff $PR | wc -l` exceeds 500 lines, OR
- the diff touches three or more of `internal/kernel/`, `internal/mcp/`, `internal/cli/`, `internal/tui/`, OR
- the user says "deep review", "multi-angle review", or "high-risk PR".

In lens mode, spawn three teammates in parallel , each focused on one dimension. They post separate `gh pr review` comments with section headers; the Lead consolidates the APPROVE/REQUEST_CHANGES.

```
Spawn a 3-teammate Lens Review Team for PR #$PR:
- security-engineer (sec): owns privacy/scope/secret review. Walks scope boundaries, encryption and key handling, the ingest scrubber, and token/secret handling.
- qa-engineer (qa): owns test coverage review. Walks new code's matching test files (*_test.go, teatest snapshots), checks if tests fail when the bug is induced, identifies coverage gaps.
- backend-engineer (be): owns perf + Go style review, OR tui-developer (tui) for UX review when internal/tui/ dominates. Pick one based on which surface has more LoC.

Each posts findings as a separate `gh pr review --body "..."` comment with their dimension as a section header:
  ## Security (security-engineer)
  ## Test coverage (qa-engineer)
  ## Performance / Style (backend-engineer)   OR  ## TUI / UX (tui-developer)

Lead consolidates after all three finish. APPROVE only if no teammate flags a blocker; REQUEST_CHANGES otherwise. Lead Delegate Mode (Shift+Tab) , coordinate, don't review yourself.
```

For smaller / single-surface PRs, proceed solo using the priority order + checklist below.

## Review philosophy

Follow Google Engineering Practices (https://google.github.io/eng-practices/review/). Key principles:

- Critique the code, not the author. "This function should wrap the error with context" not "You forgot to..."
- Be direct. "Change X to Y" is better than "Consider changing X to Y maybe?"
- Use suggestion blocks for concrete code fixes.
- Accept the author's approach when it is equally valid to yours , this is not about preference.
- Stay focused. No praise, no summary paragraph. If the code is fine, say LGTM and move on.

## Priority order

Review in this order. Stop at the first blocker , do not request style changes on fundamentally broken logic.

1. **Design** , Does the approach make sense? Is it solving the right problem?
2. **Functionality** , Does it work correctly for all cases (including error paths)?
3. **Complexity** , Is it simpler than it needs to be? Can it be deleted and replaced with an existing helper?
4. **Tests** , Are the tests testing the right behavior? Do they actually fail when the code is broken?
5. **Naming** , Are names accurate and unambiguous?
6. **Comments** , Are there comments that explain why (not what)? Are there misleading comments?
7. **Style** , Does it follow the project style guide?

When the PR claims to fix a runtime issue, reproduce it locally before and after rather than trusting the description. `/inspect-runtime` walks the daemon logs and `maz doctor` output.

## Checklist: what Claude checks on every PR

### Event log and storage
- [ ] Events are only appended or superseded, never rewritten or deleted
- [ ] New event kinds have a JSON Schema and replay correctly under `maz rebuild`
- [ ] New derived state is rebuildable from the log
- [ ] Every new fact, entity, or relation carries source, confidence, and timestamp

### Public surfaces (MCP tools, CLI, config, env vars, error codes)
- [ ] AIP naming: `createTime` not `createdAt`, plural lowercase resource paths, `Create/Get/List/Update/Delete` verbs
- [ ] Timestamps are UTC `time.Time`, RFC 3339 on the wire
- [ ] CLI commands support `--json` / scripted mode, and theming follows `agent-docs/lifecycle-theming.md`
- [ ] User-facing change updates `agent-docs/` and `agent-docs/mintlify-backlog.md`

### Go style
- [ ] Error strings: lowercase, no trailing punctuation
- [ ] Log messages: Capitalized
- [ ] No `_ = err` (blank identifier error discard)
- [ ] Context is the first parameter on all functions that need it
- [ ] No `panic` in library code , errors are returned
- [ ] No global state introduced
- [ ] No new `util`, `common`, `helpers` packages

### Security and privacy
- [ ] No credentials, tokens, API keys, or user content in log fields
- [ ] SQL uses placeholders (no string concatenation)
- [ ] Scope boundaries respected (personal, draft, team); no cross-scope leaks
- [ ] Ingested text passes through the scrubber before it is stored or sent to a provider

### Hot paths
- [ ] Hot-path changes include a benchmark with the baseline in the PR description

## Comment labels

Use these prefixes to communicate the priority of a comment:

| Label | Meaning | Must fix? |
|-------|---------|----------|
| *(no label)* | Should fix , correctness, compliance, or significant quality issue | Yes |
| `Nit:` | Optional style preference | No |
| `Optional:` | An improvement to consider but take or leave | No |
| `Question:` | Asking for clarification, not requesting a change | No |

Examples:
```
This should wrap the error with `fmt.Errorf("get entity: %w", err)` so the caller sees where it failed.

Nit: variable name `d` could be `deadline` for clarity.

Optional: Consider extracting this into a helper if the same pattern appears in more than 2 places.
```

## Suggestion blocks

For concrete code fixes, use GitHub suggestion blocks:

````markdown
```suggestion
return nil, fmt.Errorf("get entity %s: %w", uid, err)
```
````

Suggestion blocks let the author apply the fix with a single click. Use them for specific, localized changes.

## Approval guidance

**APPROVE with LGTM**: No substantive issues. Nits and optionals noted but not blocking.

**APPROVE with comments**: Minor issues that the author can address without re-review. Add "No re-review needed" to make this clear.

**REQUEST_CHANGES**: One or more of the following:
- Security or correctness bug
- Event-log invariant violated (rewrite/delete instead of append/supersede)
- Privacy or scope leak
- Breaking change to a public surface without an explicit decision
- Missing test for critical business logic

**Never REQUEST_CHANGES for**:
- Pure style preferences when both approaches are valid
- Performance micro-optimizations that are not causing real problems
- Requesting additional features that were not in scope

## Gotchas

Concrete pitfalls encountered when using this skill. Add new ones as you hit them , this is the compounding-engineering log (see `agent-docs/lessons.md` for the broader pattern).

- _(empty , add the first real entry the next time you hit a non-obvious pitfall)_
