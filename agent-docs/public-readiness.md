# Public-repo readiness

This checklist captures what must hold before the Mazarbul repository is made
public, and how to verify each item. It is the durable record of the
public-readiness pass tracked in issue #238; re-run the verification commands
before any future visibility change.

## Licensing

- [x] `LICENSE` is the canonical Apache License 2.0 full text (no placeholder,
      no Business Source License / Additional Use Grant language).
- [x] `NOTICE` exists and attributes copyright: `Copyright 2026 Moritz
      Steigerwald`, Apache-2.0.
- [x] License is referenced consistently in `README.md` and `AGENTS.md`
      ("Apache-2.0; see LICENSE and NOTICE").
- [x] The release workflow (`.github/workflows/release.yml`) ships both `LICENSE` and `NOTICE` in release archives.

Lightweight SPDX policy: a repo-root `LICENSE` + `NOTICE` only. We do **not**
add per-file SPDX headers — they add churn without changing the legal posture
for a single-license repo.

Verify:

```sh
git grep -i "business source\|additional use grant\|change license" -- . \
  ':(exclude)agent-docs/memory/*'   # expect: no matches on shippable surfaces
```

## No internal milestone / process codes

Internal milestone codes (`M2.5` / `M2.6` / `M2.7`) and plan/PR-stage labels
(`PR4`, `PR7`, `Phase A/B/P`) are meaningless to outside readers and read as
leaked process cruft. They are replaced with feature-oriented language:

- `M2.5` → encrypted-default sealed-chunk sync
- `M2.6` → plaintext-default sync
- `M2.7` → the renderer

Verify (expect empty):

```sh
git grep -nE "M2\.[567]|Phase [PAB]\b" -- '*.go' '*.md' '*.mdx' '*.yaml' \
  '*.yml' '*.json' '*.sql' '*.cypher' | grep -vE "/\.claude/|agent-docs/memory/"
```

## Security contact

- [x] `SECURITY.md` routes reports through GitHub private vulnerability
      reporting (`Security` tab → `Report a vulnerability`), not a personal or
      company email.
- [x] No personal/company email addresses remain in `SECURITY.md` or
      `CONTRIBUTING.md`.

Verify (expect empty):

```sh
git grep -in "arcos-global.com" -- . ':(exclude)agent-docs/memory/*'
```

## Generic, non-identifying examples

All example data uses a tasteful, international Lord of the Rings theme
(matching the project's own naming: Moria, Mellon, Watchword). No real company,
colleague, or region-locked business form appears in shippable files.

- People: Frodo Baggins, Rosie Cotton, Sam Gamgee, Eowyn, Gandalf, Merry
  Brandybuck, …
- Companies use international forms: Erebor Inc., Mithril Corp, Rivendell,
  Bree Ltd., Gondor Inc., Rohan Ltd., Isengard Ltd.
- Generic `alice` / `bob` placeholders are acceptable and retained where they
  read as canonical examples.

Kept deliberately (not "leaked" data):

- The `business-de` installable schema pack — a real internationalization
  feature (German company registry: GmbH/AG/UG, HRB, Handelsregister) — and its
  multilingual test coverage.
- The umlaut-preservation example in `internal/kernel/extractor/prompt.go`
  ("Geschäftsführer"), which documents Unicode handling.
- Real LLM-provider identifiers in config/code (Anthropic, Mistral, Cohere,
  Ollama) where they name an actual provider rather than an example entity.
- Author attribution `Moritz Steigerwald` in `LICENSE`, `NOTICE`, `AGENTS.md`,
  and design docs.

Verify (expect only the deliberate keeps above):

```sh
git grep -inE "arcos|climeworks|\bflorian\b|sarah jones|mustermann|bergmann|immobilien|leitstelle" \
  -- . ':(exclude)agent-docs/memory/*' \
  ':(exclude)internal/plugin/schemapack/packs/business-de/*'
```

## Honest comments (no stale deferral claims)

Comments that say a feature is "deferred" / "TODO" / "for now" / "not yet" must
reflect the **current** state of the code, not the state when written. They rot
silently and mislead readers about real capabilities.

- [x] `internal/cli/sync.go` header lists the real subcommands (push, status,
      pull, inspect, diff, log) — the stale "pull is deferred" note is gone.
- [x] The dead `_ = json.Marshal` import-silencer and its "JSON output mode is
      still on the TODO list" comment are removed (JSON output mode is wired via
      the `--format json` flag).
- [x] Stale forward-looking claims (e.g. stats "bolt-backed daily roll-ups")
      are reworded to honest present/future tense; defunct internal `PR<n>`
      references are dropped.

The daemon v1 scope endpoints (`listScopes`/`pushScope`/`pullScope`) genuinely
return `501 UNIMPLEMENTED` and point users to the working `maz sync …` CLI —
those comments are accurate and intentionally kept.

## CI gate

The change is comment/example/test-data only — **zero behavior change**. The
bar is that `mise run ci` (full `golangci-lint ./...` + `go test -race` + build)
stays green, matching the pre-change baseline.

```sh
mise run ci
```

## Repo furniture

- [x] `CHANGELOG.md` is release-please-owned; left untouched and presentable.
- [x] `.github/` (issue templates, workflows) is launch-appropriate.
- [x] `README.md` opens with a clear value proposition and quickstart.
