# Contributing to Mazarbul

Thanks for your interest. Mazarbul is pre-alpha; expect rough edges, fast iteration, and frequent refactors. The bar for new code is high (Google Go Style Guide, table-driven tests, race-clean), but the bar for asking questions is low.

Be kind, give thoughtful reviews, and assume good faith. This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## Before you start

- Read [DEVELOPMENT.md](DEVELOPMENT.md) for environment setup.
- Read [agent-docs/AGENTS.md](agent-docs/AGENTS.md) (if present) for the project conventions agents and humans share.
- Skim open issues and discussions to avoid duplicating work.
- For non-trivial work, open a discussion first to align on direction.

## Branch naming

Use a typed prefix matching the change's intent.

- `feat/<slug>` for new features
- `fix/<slug>` for bug fixes
- `chore/<slug>` for tooling, deps, refactors that don't change behaviour
- `docs/<slug>` for docs-only changes
- `refactor/<slug>` for code restructuring without behaviour changes

Examples: `feat/event-log-iterator`, `fix/missing-data-dir`, `docs/quickstart`.

## Commit format

[Conventional Commits](https://www.conventionalcommits.org). Subject is an imperative sentence, lowercase, no trailing period.

Format: `<type>(<scope>): <description>`

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`, `perf`, `build`, `ci`, `revert`.

Examples:

```
feat(kernel): add event-log iterator
fix(cli): handle missing data dir
docs(architecture): describe four-layer storage
chore(deps): bump golangci-lint to 1.62
refactor(tui): extract panel component
test(plugin): cover manifest loader edge cases
```

DCO sign-off is not required.

## Pull request process

1. Fork or branch off `main`.
2. Make focused changes (one objective per PR; aim for under 300 LoC).
3. Run `mise run ci` locally; do not push red.
4. Open a PR using the template. Fill every section honestly.
5. CI must be green. CODEOWNERS review must approve once reviewers exist.
6. Squash-merge via GitHub UI. Keep the squash subject in Conventional Commits form.

PRs that touch JSON Schemas, MCP tools, or event types must include doc comments and follow Google AIP naming (`createTime`, plural collection paths, CRUD verbs).

If you learn a non-obvious lesson while working, add a line to [agent-docs/lessons-learned.md](agent-docs/lessons-learned.md) so the next contributor (or agent) does not repeat it.

## Security issues

Do not open a public issue for vulnerabilities. Use GitHub's private vulnerability reporting (Security tab → "Report a vulnerability") or see [SECURITY.md](SECURITY.md) for the full disclosure process.

## Questions

Open a [GitHub Discussion](https://github.com/mo-cmyk/mazarbul/discussions). For security issues, follow [SECURITY.md](SECURITY.md) instead.
