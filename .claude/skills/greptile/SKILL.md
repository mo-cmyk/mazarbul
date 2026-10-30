---
name: greptile
description: Triages Greptile bot comments on the current PR and resolves UI threads as fixed-or-disagreed. USE WHEN user says 'triage Greptile on this PR', 'fix Greptile', 'address the Greptile comments', or there are open Greptile threads on the current PR. Reads Greptile findings via gh CLI, classifies real-bug vs false-positive, applies fixes for real bugs, posts disagreement reply + resolves thread for false-positives in the GitHub UI. NOT FOR human review comments (use /review-fix , broader scope).
allowed-tools: Bash, Read, Write, Edit, Grep, Glob, Agent, Skill
argument-hint: [PR-number]
---

Triage and resolve all Greptile review comments on the current PR. **Every addressed thread must end with `isResolved: true` in the GitHub UI.** This skill exits non-zero if any Greptile thread remains unresolved after the run , the autonomous loop relies on that signal.

## Steps

**Phase 1 , Fetch & Triage**

Determine PR number:

```bash
PR="${ARGUMENTS:-$(gh pr view --json number -q .number)}"
```

List every Greptile thread with its body and resolution state:

```bash
.claude/scripts/greptile-resolve-threads.sh list $PR
```

(That script wraps `gh api graphql` with the right filter.) For each thread, classify:

1. **True positive** , A real issue. Fix it.
2. **False positive** , Not a real issue (prepare a one-sentence justification).
3. **Ambiguous** , Requires deeper investigation.

Present the triage as a markdown summary table before making any changes. Columns: `# | path:line | Class | Greptile confidence | Rationale | Action`.

**Phase 2 , Resolve Ambiguous Findings**

For each ambiguous finding, spawn a team of agents IN PARALLEL via the Agent tool:

```
Agent({ subagent_type: "Explore",
        description: "Investigate Greptile finding #N at path:line",
        prompt: "..." })
```

Agents research the codebase + relevant best-practice source. Reach consensus. Report the conclusion + reasoning. Update the triage table. Reclassify as true or false positive.

**Phase 3 , Fix True Positives**

For each confirmed true positive, apply the fix following the authoritative standard for that layer:

- Go backend: Google Go Style Guide, idiomatic Go, project conventions (`agent-docs/`)
- Proto/API: Google AIPs, Protobuf Style Guide
- Frontend: Next.js docs, React 19, project frontend rules (Geist/shadcn/TanStack/Connect-ES)

If multiple true positives are independent (different files), spawn parallel Agent calls to fix them concurrently. Group findings that touch the same file.

Run linters + tests after fixes to confirm no regressions:

```bash
mise run lint && mise run test
# (no frontend currently) && pnpm tsc --noEmit && # (no frontend)   # if web/ touched
```

**Phase 4 , Respond & Resolve (CRITICAL , DO NOT SKIP EITHER STEP)**

> ⚠ The single most common failure mode of this skill is skipping the GraphQL `resolveReviewThread` mutation , leaving threads visually unresolved in the GitHub UI even though replies were posted. **You must run both `gh api` calls below for EVERY Greptile thread, true positive AND false positive, with no exceptions.**

For each thread, in order, do BOTH steps. Iterate through the thread list from `.claude/scripts/greptile-resolve-threads.sh list $PR` , every row in that output requires both calls below before moving on.

**Step 4a (every thread) , Reply via `gh api`:**

```bash
# COMMENT_ID is the `commentId` (databaseId) from `list` output, not the threadId.
gh api repos/{owner}/{repo}/pulls/$PR/comments/$COMMENT_ID/replies \
  -f body="$REPLY_BODY"
```

Reply body conventions:
- **True positive** (we applied the fix): `"Fixed in <new-commit-sha> , <one-line what changed>"`
- **False positive** (we disagree): `"No change: <one-sentence justification>"`

**Step 4b (every thread) , Resolve via GraphQL mutation:**

```bash
# THREAD_ID is the `threadId` (GraphQL node ID) from `list` output, not the commentId.
.claude/scripts/greptile-resolve-threads.sh resolve $THREAD_ID
```

This calls `resolveReviewThread` and is what flips the thread to "Resolved" in the GitHub UI. **A reply alone does not resolve the thread , it stays open and looks unaddressed to a human reviewer.**

**Loop invariant:** after Step 4b for thread N, the thread MUST show `isResolved: true`. If `.claude/scripts/greptile-resolve-threads.sh verify $PR` finds any unresolved Greptile thread at the end, this skill has failed , `verify` exits non-zero and the babysit loop will keep iterating.

**Phase 5 , Push & Re-request Review**

Use `/commit-push` to rebase, lint, test, and push the fix commit(s):

```
Invoke /commit-push (Skill tool)
```

Commit subject: `fix: address greptile review feedback`. Body lists what changed per file.

After push, tag Greptile to trigger a re-review:

```bash
gh pr comment $PR --body "@greptileai please re-review"
```

**Phase 6 , Verify All Resolved**

Run the verification step:

```bash
.claude/scripts/greptile-resolve-threads.sh verify $PR
```

This exits 1 if **any** Greptile thread on the PR is still `isResolved: false`. The skill MUST propagate that exit code , the autonomous babysit loop uses it to decide whether to iterate again.

**Phase 7 , Report**

Print:
- Total Greptile threads: N
- True positives fixed: N
- False positives explained: N
- Ambiguous resolved: N (true: A, false: B)
- All threads resolved in UI: ✅ / ✗
- Re-review requested: ✅

---

**Mark every addressed thread as resolved in the UI. Without that, the loop won't converge.**

## Gotchas

- **The skill is NOT done until every Greptile thread shows `isResolved: true`.** The reply via `gh api .../comments/$ID/replies` is half the work , the other half is the GraphQL `resolveReviewThread` mutation (Phase 4b). Skipping 4b leaves threads visually open in the UI even though we've addressed them in commits. Run `.claude/scripts/greptile-resolve-threads.sh verify $PR` at the end of every run; non-zero exit means re-iterate.
- **`@greptileai` mention is how you both trigger and re-trigger Greptile.** The bot's name in GitHub Apps autocomplete is hidden , type the mention literally. Initial review: `gh pr comment $PR --body "@greptileai please review"`. Re-review after fixes: `gh pr comment $PR --body "@greptileai please re-review"`. Never call a non-existent "request re-review" API , there isn't one; the mention IS the API.
- **`commentId` vs `threadId` are different IDs.** `.claude/scripts/greptile-resolve-threads.sh list $PR` returns both , use `commentId` (REST databaseId) for `gh api .../comments/{id}/replies`, and `threadId` (GraphQL node ID, starts `PRRT_`) for the resolve mutation. Mixing them up yields cryptic GraphQL errors.
