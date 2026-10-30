---
name: review-fix
description: Fixes the comments on the current PR , Greptile, human reviewers, failing CI. USE WHEN user says 'fix the review comments', 'address the feedback', 'fix the PR', 'resolve the review threads', or there is an open PR with unresolved review comments or red CI. Reads review threads via gh CLI, classifies actionable vs nit, applies fixes, pushes commits, resolves resolved threads in GitHub UI. NOT FOR opening a new PR (use /ship) or reviewing someone else's PR (use /pr-review).
allowed-tools: Bash, Read, Write, Edit, Grep, Glob, Agent
argument-hint: [PR-number]
---

Triage and resolve all review findings on the current PR. Pre-production (0.x) project , **breaking changes are acceptable; prioritize correctness over backwards compatibility**.

## Steps

**Phase 1 , Triage**

If `$ARGUMENTS` is a PR number, use it; otherwise `gh pr view --json number`.

Fetch all open review comments AND CI failure logs:

```bash
PR=$(gh pr view --json number -q .number)
# Review thread comments (line-level)
gh api repos/{owner}/{repo}/pulls/$PR/comments --paginate \
  --jq '.[] | {id, author: .user.login, path, line, body, in_reply_to_id}'
# Review summary comments
gh pr view $PR --json reviews --jq '.reviews[] | {author: .author.login, state, body, submittedAt}'
# CI failures
gh pr checks $PR --json name,state,link,bucket --jq '.[] | select(.bucket=="fail")'
```

Classify every finding as one of:

1. **True positive** , A real problem that should be fixed.
2. **False positive** , Not actually an issue (write one-sentence justification).
3. **Ambiguous** , Unclear whether real; needs deeper investigation.
4. **Intentional / out of scope** , Valid concern but not applicable here (explain why).

Present the triage as a markdown summary table **before making any changes**. Columns: `# | Author | Location | Class | Rationale | Action`.

**Phase 2 , Resolve Ambiguous Findings**

For any finding classified Ambiguous, spawn a team of agents IN PARALLEL via the Agent tool:

```
Agent({ subagent_type: "Explore",
        description: "Investigate Greptile/reviewer finding #N",
        prompt: "..." })
```

Each agent researches the codebase, related code paths, and the relevant best-practice source. Agents reach a consensus. Report the conclusion + reasoning, then reclassify the finding as true or false positive. Update the triage table.

**Phase 3 , Fix True Positives**

For each confirmed true positive, apply the fix following the authoritative standard for that layer:

- Proto/API: Google AIPs, Protobuf Style Guide, project CLAUDE.md
- Go: Google Go Style Guide, idiomatic Go, project conventions
- Frontend: Next.js docs, React 19, Geist/shadcn/TanStack/Connect-ES
- **General**: prefer patterns already established in this codebase

If multiple true positives are independent and touch different files, spawn parallel Agent calls (one per file group) to fix them concurrently. Group related findings that touch the same files.

**Phase 4 , Verify**

After all fixes:

- `mise run lint` (Go)
- `# (no frontend currently) && pnpm tsc --noEmit` (if web/ touched)
- `mise run test` (Go)
- `# (no frontend currently)` (if web/ touched)

Confirm no new issues were introduced.

**Phase 5 , Push & Comment**

```bash
git add <specific paths>
git commit -m "fix: address review feedback"
git push
```

Reply to each thread you addressed with a one-line explanation of the change. If this is a Greptile-only review fix, **prefer `/greptile` over this skill** , it also resolves threads in the UI which `/review-fix` does not do automatically.

For non-Greptile threads (human reviewers), don't mark threads resolved automatically , that's the reviewer's call.

---

**Never force-push. Never bypass `--no-verify`. Pre-production rules: prioritize correctness over backward compatibility.**

## Gotchas

- _(empty)_
