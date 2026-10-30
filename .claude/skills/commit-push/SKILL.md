---
name: commit-push
description: Stages changes, writes a conventional-commit message, commits, and pushes , without opening a PR. USE WHEN user says 'commit it', 'commit these changes', 'push this', 'commit and push', or wants a single atomic save without a PR. Generates feat:/fix:/refactor: message, commits specific paths, pushes. NOT FOR the full ship flow (use /wrap-up).
allowed-tools: Bash, Read, Edit, Grep, Glob
argument-hint: [commit-subject-override]
---

Prepare, validate, and push a clean commit (or sequence of commits) for the current branch. Does NOT open a PR , use `/ship` or `/wrap-up` for that.

Follows Google Eng Practices CL conventions: imperative first line, body explains what+why, self-contained changes, no Linear issue refs.

## Steps

**Phase 1 , Rebase onto Main**

```bash
git fetch origin main
git rebase origin/main
```

If merge conflicts arise, resolve them , prefer the intent of this branch's changes while incorporating upstream updates. After rebasing, confirm the codebase compiles and is in a clean state.

**Phase 2 , Static Analysis & Linting**

Run all linters , these are enforced in CI/CD. Zero warnings, zero errors:

```bash
# Proto (if proto/ changed)
 && 

# Go
mise run lint

# Frontend (if web/ changed)
# (no frontend currently)
cd web && pnpm tsc --noEmit
```

Fix any issues found before proceeding.

**Phase 3 , Run Tests**

```bash
mise run test     # Go, with race detector
# Frontend (if web/ changed)
# (no frontend currently)
```

If any test fails, investigate and fix , **do not skip or disable**.

**Phase 4 , Git Hooks Validation**

Pre-commit and pre-push hooks must pass. **Never use `--no-verify`.** If a hook fails, fix the underlying issue and retry.

**Phase 5 , Clean Commit History**

Inspect what's staged:

```bash
git status --short
git diff --stat
```

Per [Google CL descriptions guide](https://google.github.io/eng-practices/review/developer/cl-descriptions.html):

- Each commit is **one self-contained, logical change**.
- Use conventional commit prefixes: `feat:`, `fix:`, `refactor:`, `chore:`, `docs:`, `test:`.
- First line: imperative, concise, stands alone as a summary (e.g., `feat: add cursor-based pagination to entity listing`).
- Body (after blank line): explain **what changed and why** , enough context that a future reader understands without external links.
- **Do not mention Linear issue identifiers** in any commit message.

Stage specific files (avoid `git add -A` / `git add .`):

```bash
git add path/to/specific/files...
```

Commit with HEREDOC for proper formatting:

```bash
git commit -m "$(cat <<'EOF'
feat: <imperative summary>

<body explaining what + why>
EOF
)"
```

If multiple logical changes, split into multiple commits.

**Phase 6 , Push**

```bash
git push -u origin "$(git branch --show-current)"
```

If the branch hasn't been pushed before, this sets up tracking. **Never force-push** without invoking `.claude/scripts/guard-destructive.sh force-push "<recent user message>"` first.

**Phase 7 , Report**

Print:
- Number of commits created
- Their subjects
- Branch + remote tracking
- Pre-commit / pre-push hook results

---

**Never `--no-verify`. Never force-push without explicit confirmation. No Linear refs in commits.**

## Gotchas

- _(empty)_
