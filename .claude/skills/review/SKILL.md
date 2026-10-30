---
name: review
description: Fix-loop on an existing PR's CI failures. USE WHEN a PR has failing CI and needs `mise run ci` fixes pushed, when CI checks are red, or when the babysit-tick worker observes ci_fail. Runs mise run ci loop until passing, pushes fix commits to the PR branch. NOT FOR opening a new PR (use /ship), reviewing OTHER people's PRs (use /pr-review), or triaging human/Greptile review comments (use /review-fix or /greptile).
allowed-tools: Bash, Read, Write, Edit, Grep, Glob
argument-hint: [PR-number]
---

Run a fix loop on the current branch's committed code until `mise run ci` passes (or 5 iterations), then update an existing PR or open a new one.

If a PR number is given as `$ARGUMENTS`, target that PR.

## Steps

**1. Inspect branch state**

```bash
git log --oneline origin/main..HEAD
git diff --stat origin/main
```

Understand what this branch changes before starting.

**2. Run checks**

```bash
mise run ci 2>&1
```

For branches touching web code also run:
```bash
# (no frontend currently) 2>&1
```

If all pass: skip to step 4.

**3. Fix loop (max 5 iterations)**

For each iteration:
- Analyse all failure output. Group by type (lint, type errors, test failures).
- Run `make fix` first to auto-fix formatting/import issues.
- Fix as many remaining errors as possible per iteration , do not be incremental.
- Commit fixes as you go with `fix: <description>` commits.
- Re-run checks.
- If checks pass: proceed to step 4.
- If iteration 5 is reached without convergence: report remaining failures and stop. Do not update the PR with broken code.

Rules:
- Never use `--no-verify` or add `// nolint` suppression unless genuinely correct.
- Proto changes need `` first.
- Do not modify tests to make them pass if the production code is actually wrong , fix the code.
- Each fix commit must be atomic and descriptive.

**4. Check for existing PR**

```bash
gh pr view --json url,number,state 2>/dev/null
```

If `$ARGUMENTS` was a PR number, use:
```bash
gh pr view "$ARGUMENTS" --json url,number,state
```

**5a. Update existing PR**

If a PR exists, push the fix commits:
```bash
git push
```

Then add a comment summarising what was fixed:
```bash
gh pr comment <number> --body "$(cat <<'EOF'
**Review fix loop complete**

Iterations needed: <N>

Fixed:
- <list of fixes>

All checks now pass.
EOF
)"
```

**5b. Open new PR**

If no PR exists, open one:
```bash
gh pr create \
  --title "<conventional-commit-title>" \
  --body "$(cat <<'EOF'
## Summary
<bullet points>

## Changes from review fix loop
<what was fixed during the review loop>

## Test plan
- [ ] `mise run ci` passes
- [ ] <manual steps to verify>
EOF
)"
```

**6. Report**

Print:
- Iterations taken
- What was fixed
- PR URL

---

**Never force-push. Never bypass checks. Stop at iteration 5 if not converged.**

## Gotchas

Concrete pitfalls encountered when using this skill. Add new ones as you hit them , this is the compounding-engineering log (see `agent-docs/lessons.md` for the broader pattern).

- _(empty , add the first real entry the next time you hit a non-obvious pitfall)_
