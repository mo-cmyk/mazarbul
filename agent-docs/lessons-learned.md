# Lessons learned

Lessons recorded by the `/lessons` skill, rotated by `.claude/hooks/lessons-rotate.sh` when over 500 lines.

Each entry: a one-paragraph note on a gotcha, a non-obvious decision, a workaround, or a corrected assumption. Newest at the top.

<!-- entries below this line -->

## 2026-06-24, a security gate inside an append-only projection must fail as a silent no-op, never an error

`Compliance: true`

Building the draft-staging dispatcher gates (`internal/kernel/storage/dispatcher/scope_stage.go`) surfaced a non-obvious rule for any event-handler that enforces a security invariant. The dispatch loop (`dispatcher.go` ~line 576) does `if err != nil { return fmt.Errorf("apply event %d ...") }` — a single handler error aborts the **entire** replay. So if a security gate `return`s an error when it sees a forbidden event (e.g. a `scopePromoted` whose `toScope` is `team`, or whose `fromScope` is not `draft`), a single crafted or corrupted event in the log permanently bricks the projection on every future Rebuild — a denial-of-service baked into the append-only store, which you can never edit out. The correct posture is to **log a Warn and `return nil`** (silent no-op), exactly as the pre-existing `fromScope != "draft"` gate did. The forbidden mutation simply doesn't happen; the log stays replayable. Reserve real errors for genuinely un-projectable corruption (failed JSON decode), not for policy rejections.

Two corollaries from the same work:

- **A "valid" scope is not the same as a "readable" scope.** `kernel.ParseScopeSet` accepts `draft` because it is a real, kernel-internal scope — but the general read verbs (recall, getContext, get, neighbors, relations, entities) must still reject `scopes:["draft"]` with `E_VALIDATION`. The kernel vocabulary is deliberately broader than what any one surface may expose; gate readability at the tool boundary (`internal/mcp/tools/scopes.go`, plus recall/getContext which build their scope set directly), not only at the parser.
- **A destructive projection that has no scope column on a table must gate the DELETE on a row that does.** `provenance` has no `scope`, so `applyScopeDiscarded` gates its provenance delete on `EXISTS (SELECT 1 FROM episodes WHERE uid = ? AND scope='draft')` — otherwise a stray discard naming an already-promoted (live) episode would strip a durable record's provenance. When you can't filter the target table by scope directly, filter by a scoped row it depends on, inside one transaction.

## 2026-05-24, name kernel interfaces by capability, not vendor; pick the maintained fork when one vanishes

`Compliance: true`

In October 2025 Apple quietly acquired Kùzu (the embedded property graph database Mazarbul's M1 four-layer storage was designed around) and archived the upstream repo. The community produced two forks within days: **LadybugDB** (the Kùzu engineering team's continuation, MIT, columnar storage preserved, official Go bindings at `github.com/LadybugDB/go-ladybug` v0.13.x as of Dec 2025, latest engine release v0.16.x May 2026) and **RyuGraph** (Predictable Labs, also MIT, no native Go bindings). Mazarbul moved to LadybugDB because the file format, query semantics, and Cypher dialect carry over unchanged and the Go bindings exist out of the box; RyuGraph would have required either a CGO binding generator or running the engine as a subprocess.

The lesson is two-part:

- **Pick the maintained fork promptly.** A vendor disappearing is a known failure mode for niche infra. The cheap response (rename, swap the binding) is much cheaper than the expensive response (rewrite the storage layer). Switch within a sprint of the upstream archival; do not wait to see if upstream comes back.
- **Name kernel interfaces by capability, not by vendor.** The original kernel had `Storage.OpenKuzu()`, `KuzuConn`, `KuzuResult`. When the vendor disappeared, every doc, every issue, every code reference had to change. The new shape (`Storage.OpenGraph()`, `GraphConn`, `GraphResult`) would have survived the same incident with only `internal/kernel/storage/ladybug.go` touched. The four-layer storage's other slots already followed this pattern (`OpenVector`, `OpenFTS`, `VectorConn`, `FTSConn`); it was only the graph slot that had leaked the vendor name into the kernel surface.

Going forward, every kernel-surface name describes the capability (event log, graph, vector, full text, secret, chat, embed), and the vendor lives only inside the implementing package. The architectural commitment is captured in `internal/kernel/storage.go` (godoc on `Storage`) and `agent-docs/tech/ladybug.md` (last section).

## 2026-05-23, hot-path PRs ship with a benchmark and a recorded baseline

`Compliance: true`

When a PR touches a hot path (event log writer, four-layer storage dispatcher, MCP request handler, recall composer, embedding call, anything else flagged by the issue's `## Hot path?` field), the PR includes or updates a benchmark and records the baseline (`ns/op`, `B/op`, `allocs/op`) in the PR description's `## Performance` section. Bench files sit next to the code under test (`*_bench_test.go`) and use `b.ReportAllocs()`. The `agent-docs/profiling.md` canonical reference covers `mise run bench`, `mise run pprof:cpu`, and `mise run pprof:mem`.

The rule lands as AGENTS.md rule 10 because the infrastructure (mise tasks, pprof, profiling.md) existed during the bootstrap but the prompting did not require a benchmark on hot-path changes; baselines were never recorded and regressions could creep in silently. Profile before optimising. Measure, don't guess. Automatic regression gates land in M2+; for M1, the rule is review-time discipline.

## 2026-05-23, user-facing changes update agent-docs and the Mintlify backlog in the same PR

`Compliance: true`

A user-facing surface is a CLI command, an MCP tool, a TUI screen, a config knob, an env var, an error code, or anything else a human or external agent observes. When a PR ships one, it updates the corresponding `agent-docs/*.md` file and appends a backlog entry to `agent-docs/mintlify-backlog.md` (intended Mintlify path, source PR/issue, status, description, last-updated date). The Mintlify site itself is not live yet; the backlog file is the staging area so when Mintlify activates, `@docs-writer` has a ready list of pages to draft.

Internal-only refactors and tests skip both. The rule applies when behaviour visible from outside `internal/` changes. This lands as AGENTS.md rule 11.

## 2026-05-23, missing glue between subsystems is a follow-up issue, never silent scope expansion

`Compliance: true`

When implementing an issue (or reviewing a PR), if connective tissue between subsystems turns out to be missing or unowned, the correct response is to **open a new issue for it immediately** and link it from the current issue's body or PR description. Do not silently expand the current issue's scope to cover the gap. Do not assume someone else's issue covers it. No issue should ever be implicit; if it does not have a number, it does not exist, and one day no one will feel responsible for it.

Two M1 examples that became their own explicit "glue" issues rather than disappearing into a parent's scope: #55 wires `EventLog.Append` into `Storage.Apply` (sits across the event-log epic #10 and the four-layer-storage epic #12), and #56 ships the M1 hybrid retrieval composer (sits across storage #12 and ontology #13, consumed by remember/recall #16). Neither belongs cleanly to one parent; both are named, numbered, and owned.

The discipline:
- During implementation, if you reach for a method that does not exist on the surface you depend on, stop and open a follow-up issue. Reference it from your current issue and proceed with a stub if the gap blocks progress, or pause if it does not.
- During review, if you can describe a piece of work whose acceptance criteria do not appear in any open issue, open one. The cost of an extra issue is cheap; the cost of an unowned gap discovered at integration time is not.
- Cross-epic glue issues are labeled with the area of the dominant consumer (e.g., #56 is `area:kernel` since the composer lives there); the body explicitly notes the spanning relationship so neither parent assumes the other ships it.

## 2026-05-23, Greptile pass/fail is not enough; read every inline thread and resolve before merge

`Compliance: true`

During the Mazarbul bootstrap, four PRs (#6, #7, #8, #9) were merged after only checking that Greptile had reviewed, not what it actually said. Thirteen inline findings were silently shipped, including a real bug (`scripts/devstack ready` aborts under `set -euo pipefail` when grep returns 1 for dependency-free issues), a doc/code mismatch (`mise run coverage` advertised a browser open that the task never performed), and seven Noop implementations that returned successful empty values instead of `ErrNotImplemented`, masking unwired subsystem stubs at runtime. Greptile's inline P0/P1/P2 review threads are not summarized into the top-level CI status, so they do not show up under `gh pr checks` and a "Greptile reviewed" badge proves nothing about whether findings were addressed.

The discipline: before merging any PR, run `./.claude/scripts/greptile-resolve-threads.sh list <PR>` to enumerate every inline thread, classify each (real bug, valid nit, false positive), fix or justify each, reply on the thread referencing the fix commit SHA, resolve the thread via the GraphQL `resolveReviewThread` mutation, re-request Greptile with a `/greptile` comment, and only merge when `./.claude/scripts/greptile-resolve-threads.sh verify <PR>` prints `VERIFY OK: all Greptile threads resolved`. The `/greptile` and `/babysit-pr` skills wrap this end-to-end; when working manually, perform the same steps by hand. This rule is also in `AGENTS.md` critical rules as rule 9.

## 2026-05-23, agentic content filters trip on certain license boilerplate

Two file bodies tripped Claude content-filtering during the bootstrap scaffold: the full Contributor Covenant 2.1 text (`CODE_OF_CONDUCT.md`) and the full license body (`LICENSE`). The agent that wrote them returned partial output and reported "Output blocked by content filtering policy". The workaround used during bootstrap was:

- Skip `CODE_OF_CONDUCT.md` entirely; add a one-line note in `CONTRIBUTING.md` that a formal CoC will land before the first public release.
- Ship a parameter-only `LICENSE` file and link to the canonical text until the full body could be inlined before public release.

The full canonical license body is now inlined: `LICENSE` is the Apache License 2.0 (Copyright 2026 Moritz Steigerwald) with an accompanying `NOTICE` file. If a future agent needs to ship a long canonical body (CoC or a different license), fetch from the canonical source via WebFetch and write the result without regenerating it. Do not attempt to author such boilerplate from scratch in a single message; split into smaller chunks or use a direct file copy.

## 2026-05-23, go.mod directive must trail the installed Go toolchain by one minor

`golangci-lint` versions are built against a fixed Go release. Setting `go 1.26` in `go.mod` while the linter ships built with Go 1.25 fails with "the Go language version used to build golangci-lint is lower than the targeted Go version". Keep the `go` directive in `go.mod` one minor behind the freshest Go release until the lint ecosystem catches up. In `.mise.toml` you can still pin a newer compiler toolchain; the source-level Go version directive is the constraint.

## 2026-05-23, lefthook pre-push protects main; bootstrap pushes need a different path

The `pre-push` hook from the initial scaffold blocks direct pushes to `main` (Google-style trunk-based development with feature-branch PRs). The first scaffold commit landed on `main` only because `gh repo create --push` predates hook installation. Any subsequent change must go through a feature branch and a PR, even during the bootstrap session. Plan accordingly: do not stage commits directly on `main` after the first push, even for "just docs" changes. The emergency bypass `git push --no-verify` is documented but should be reserved for genuine emergencies.

## 2026-05-27, when the user says "do the full plan really everything", do not pause at logical waypoints

`Recurrence: 1, Last seen: 2026-05-27`

When the user has issued an explicit directive to complete a multi-workstream plan ("keep going and ping me when everything is fully done", "really everything"), do not stop at logical waypoints and ping with "want me to keep going?" Pushback that takes the form of "this is realistically multi-day work, recommend stopping here" is itself a scope correction the user has to undo. The right move is to keep executing, accept that quality on shallower surfaces (TUI polish, reranker prompt) may need a follow-up iteration, and surface remaining items only after the user redirects or the diff genuinely exhausts the session.

How to apply: if a user has used the words "everything", "full plan", "no stopping", or similar, treat that as durable for the rest of the session. Internal estimates of "this is a week of work" are not a reason to short-circuit. Ship what you can, document what's stubbed, surface the actual stop condition (test failure, design ambiguity), not a meta-discussion about scope.
