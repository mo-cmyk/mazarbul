# Quality and security

## Lint policy

Mazarbul runs golangci-lint with a strict 46-linter set in `.golangci.yml`. The ruleset includes gofumpt, goimports (with `prefix(github.com/mo-cmyk/mazarbul)`), gci, and the standard `errcheck`, `gosec`, `revive`, `gocritic`, `staticcheck`, `unparam`, and friends. **Ratchet up, never down**: a new linter rule lands only after the codebase passes it. Existing exclusions live in `.golangci.yml`'s `exclude-rules` section; adding a new one needs a justification comment.

Format on save (gofumpt + goimports) is automatic via `.claude/hooks/post-tool-format.sh`. Pre-commit runs `mise run lint-fast`; pre-push runs `mise run ci`. Hooks installed by `mise run setup`.

## Secret scanning

Deferred. Mazarbul has no secret material in the repo yet (no API tokens, no service keys, no embedded credentials). Once secrets enter the configuration surface (M1, when chat providers are wired), the secret-scanning policy becomes:

- `gitleaks` runs in pre-push and CI.
- A `.gitleaks.toml` configures the patterns to catch (Anthropic API keys, OpenAI keys, generic high-entropy strings).
- Pre-commit catches obvious leaks; CI is the safety net.

<!-- TODO(m1): land gitleaks alongside the first chat provider integration. -->

## Dependency hygiene

Dependabot runs weekly on `gomod` and `github-actions`. PRs are grouped (`go-deps`) so a single review handles a batch. Vulnerability scanning via `govulncheck` lands as a CI step alongside gitleaks.

## Security policy

See `SECURITY.md` in the repo root for the full vulnerability-reporting policy. The primary channel is **GitHub private vulnerability reporting** (Security tab → "Report a vulnerability"). Never open a public issue for a security bug. Severity is scored with CVSS v3.1; coordinated disclosure happens via a published GHSA advisory after a fix lands.

## Test coverage

Coverage is uploaded to Codecov on every CI run and surfaced as a badge in `README.md`. Configuration lives in `codecov.yml`.

**Current policy (pre-alpha):**

- **Project coverage:** target = previous commit's coverage on `main`, threshold = 5% slip allowed, `informational: true`. Never blocks merge while we churn.
- **Patch coverage:** target = 60% on new / changed code, `informational: true`. Surfaced in the PR comment but not gated.
- **Ignored from metrics:** `**/*_test.go`, `internal/testutil/**`, `internal/version/**`, `cmd/maz/main.go`, `gen/**`, plus the stub packages (`internal/kernel`, `internal/plugin`, `internal/mcp`, `internal/observability`) until they hold real code. Remove from the ignore list one-by-one as M1+ features land.

**How to run locally:**

```bash
mise run coverage       # generates coverage.out + writes coverage.html (open it yourself)
mise run coverage:ci    # generates coverage.out only (matches CI shape)
```

**Ratchet-up plan:** stay informational through pre-alpha. Once a package has been stable for two release cycles and consistently above 70%, flip the corresponding flag's `informational` to `false`. Bump global project target from `auto +5%` to `auto +0%` once we're past v0.3. Bump patch target from 60% to 70% at v0.5.

**Required secret:** `CODECOV_TOKEN` on the GitHub repo. Mazarbul is private, so Codecov requires an upload token. Add via `gh secret set CODECOV_TOKEN` after signing into codecov.io and authorizing the repo. Without the token the upload step silently no-ops (`fail_ci_if_error: false`), so the absence does not block merges either.

## Code review automation: Greptile

Greptile (https://greptile.com) is wired up to review every PR automatically. The full agent-side workflow (triage, fix, resolve threads in the GitHub UI, re-request review, babysit until merge-ready) is documented in `greptile-workflow.md`. Short version:

- Greptile is installed as a GitHub App on `mo-cmyk/mazarbul`; it auto-reviews on PR open and every `synchronize` event (push to the PR branch).
- The `/greptile` skill (`.claude/skills/greptile/`) triages findings, classifies real-bug vs false-positive, applies fixes, replies on every thread, and resolves the GitHub thread via the GraphQL `resolveReviewThread` mutation.
- The `/babysit-pr` skill (`.claude/skills/babysit-pr/`) schedules a 240s Cron loop that watches CI + Greptile until merge-ready, then push-notifies.
- The `/review-fix` skill (`.claude/skills/review-fix/`) handles broader review feedback (humans + Greptile + failing CI). For Greptile-only fixes, `/greptile` is preferred because it also resolves the UI threads, which `/review-fix` does not.
