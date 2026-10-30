---
name: ship
description: Open a NEW PR after local fix-loop. USE WHEN ready to commit working tree + open a first-time PR, or when /go composite reaches ship phase. Runs mise run ci loop (max 5 iterations), then opens PR via gh CLI. NOT FOR updating an existing PR with CI fixes (use /review instead).
allowed-tools: Bash, Read, Write, Edit, Grep, Glob
argument-hint: [--no-pr] [branch-description]
---

Run a fix loop on the current working tree until `mise run ci` passes (or 5 iterations), then open a PR.

## Steps

**1. Baseline check**

Run `mise run ci` (fmt + lint-fast + test-short) and capture all output:

```bash
mise run ci 2>&1
```

If it passes on the first try, skip directly to step 4.

**2. Fix loop (max 5 iterations)**

For each iteration:
- Analyse the failure output carefully. Group errors by file/type.
- Fix all errors you can in this iteration (don't fix one error per loop).
- Re-run `mise run ci` to verify.
- If it passes: proceed to step 3.
- If it fails again and you've reached iteration 5: report what remains unfixed and stop , do not open a PR.

Rules during fixing:
- Never skip checks with `--no-verify` or `// nolint` unless the suppression is genuinely correct.
- Proto changes require `` before `mise run ci`.
- Web-only changes: also run `# (no frontend currently)`.
- If a test is broken because the test itself is wrong, fix the test. If the production code is wrong, fix it.
- Use `make fix` first to auto-fix formatting/import issues before tackling manual fixes.

**3. Commit any uncommitted changes**

If there are staged or unstaged changes after the fix loop:

```bash
git diff --stat
git status --short
```

Stage and commit all changes made during the fix loop with a conventional commit message. Use `git add` on specific files (not `git add -A`). Follow the format: `fix: <what-was-broken>` or `chore: fix lint and type errors`.

**4. Validate final state**

Run once more to confirm:
```bash
mise run ci 2>&1
```

If this fails: do not open a PR. Report the failure.

**5. Open PR**

Check if a PR already exists for this branch:
```bash
gh pr view --json url,number 2>/dev/null
```

If no PR exists, create one:
```bash
gh pr create \
  --title "<conventional-commit-title>" \
  --body "$(cat <<'EOF'
## Summary
<bullet points describing what changed>

## Test plan
- [ ] `mise run ci` passes
- [ ] Manually verified: <describe what to test>
EOF
)"
```

If a PR already exists, just report its URL , don't create a duplicate.

**6. Report**

Print:
- Number of fix iterations needed
- Final `mise run ci` result
- PR URL

---

**Do not push to `main` directly. Do not use `--no-verify`. Stop at iteration 5 if not converged.**

## Gotchas

Concrete pitfalls encountered when using this skill. Add new ones as you hit them , this is the compounding-engineering log (see `agent-docs/lessons.md` for the broader pattern).

- _(empty , add the first real entry the next time you hit a non-obvious pitfall)_
