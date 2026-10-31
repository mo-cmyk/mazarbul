# Release pipeline

How a portable `maz` is built and shipped. The machinery is wired and verified.
**Public** distribution stays dormant until launch; a **token-gated private soak**
against the private repo is the active Phase-1 path (#243, see below).

## The hard constraint: `maz` is not self-contained

`maz` CGO-links the LadybugDB engine as a **dynamic** library
(`liblbug.dylib` / `liblbug.so`). There is no static lib upstream, and the
`go-ladybug` cgo directives bake an rpath pointing at the build machine's Go
module cache (`-Wl,-rpath,${SRCDIR}/lib`, where `setup-graph-lib.sh` extracts the
engine's `lbug.h` + lib). Two consequences:

1. **The lib must travel with the binary.** Every release archive bundles the
   matching `liblbug.*` under `lib/`, and `scripts/release-rpath.sh` rewrites the
   binary's rpath to the relocatable forms `@loader_path/lib` + `@loader_path/../lib`
   (macOS, via `install_name_tool`) and `$ORIGIN/lib:$ORIGIN/../lib` (Linux, via
   `patchelf`). It also deletes the dev module-cache rpath. The binary then
   resolves the lib whether it sits beside the binary (`./lib`) or in an FHS
   prefix (`bin/maz` + `lib/`).
2. **No cross-compilation.** `setup-graph-lib.sh` fetches only the host
   platform's lib, and the lib is a per-platform dynamic object. So each target
   is built on a **native runner**.

## Why not goreleaser

`.goreleaser.yaml` used to exist but no CI ran it. goreleaser's only OSS-viable
way to unify native multi-runner builds into one release — the `prebuilt`
builder and `release --split`/`continue --merge` — are **Pro-only**
(`builder` enum in OSS is `go|rust|zig|bun|deno`). Rather than pay for Pro or
risk an unproven zig cross-build against a Mach-O dylib, the pipeline builds
natively and assembles the release with `tar` + `sha256sum` + `gh`.

## The workflow — `.github/workflows/release.yml`

Reusable, two entry points:

- `workflow_dispatch` — **snapshot validation**. Builds + archives all targets,
  uploads them as workflow artifacts (`release-dist`). Publishes nothing. Use
  this to verify the pipeline without cutting a release.
- `workflow_call` — invoked (gated) by `release-please.yml`; with `publish: true`
  it attaches archives + `checksums.txt` to the tag's release.

**`build` matrix** (native, no cross-compile):

| target        | runner            |
| ------------- | ----------------- |
| darwin/arm64  | `macos-latest`    |
| darwin/amd64  | `macos-15-intel`  |
| linux/amd64   | `ubuntu-latest`   |
| linux/arm64   | `ubuntu-24.04-arm`|

Each job: `mise run setup` (fetches the native lib) → `go build` with
version/commit/date ldflags → generate man pages + completions from the fresh
binary → `release-rpath.sh` → stage (`maz`, `lib/`, `man/`, `completions/`,
`LICENSE`, `NOTICE`, `README.md`) → **relocate-and-run smoke test** (copy to a clean dir
with no module-cache rpath, run `maz --version`, assert the version, then
`maz init` a temp data dir and assert `maz doctor` reports `ladybug.open` ok, so
the engine is actually loaded and a DB opened) → MCP Bundle
(`scripts/mcpb-bundle.sh`, see below) → `tar.gz`.

**`assemble` job**: download all archives + bundles →
`sha256sum mazarbul_*.tar.gz mazarbul_*.mcpb > checksums.txt` → upload combined
dist; if `publish`, `gh release upload <tag>` (tarballs, `.mcpb`s, checksums).

## MCP Bundles (`.mcpb`) and the MCP Registry

Every release also ships one **MCP Bundle** per target,
`mazarbul_<tag>_<os>_<arch>.mcpb`, next to the tarballs and in `checksums.txt`.
An MCPB (formerly DXT, spec: `github.com/modelcontextprotocol/mcpb`,
`MANIFEST.md`, `manifest_version` 0.3) is a zip with `manifest.json` at the
root that Claude Desktop and other MCPB clients install in one click.

- **Layout:** `manifest.json`, `server/maz`, `server/lib/<liblbug soname>`
  (only the file the binary links, dereferenced: zip packers drop symlinks, and
  the rpath `@loader_path/lib` / `$ORIGIN/lib` finds it), `LICENSE`, `NOTICE`,
  `README.md`.
- **Manifest:** `server.type: binary`, `mcp_config.command:
  ${__dirname}/server/maz`, `args: ["mcp", "serve"]`, `compatibility.platforms:
  [<os>]`, version = tag without `v`. No data dir is set, so the bundle uses the
  user's default data dir and shares memory with the CLI; `maz mcp serve`
  initialises a missing data dir on first start (`maz init` only picks the
  provider).
- **One bundle per OS/arch:** the manifest can constrain the OS but not the
  CPU, and liblbug is a per-platform dynamic lib, so there is no universal bundle.
- **Build:** `scripts/mcpb-bundle.sh <stage> <tag> <goos> <out.mcpb>` writes the
  manifest with `jq`, then runs the pinned `@anthropic-ai/mcpb` CLI (`validate`,
  `pack`, `info`) via `npx`. The build job sets up Node for it.

**`mcp-registry` job** (`release-please.yml`, after `release-build`, parallel to
`homebrew`, skipped when no release is cut): downloads `checksums.txt`, renders
`server.json` with `scripts/mcp-server-json.sh <tag> <checksums.txt>` (the
repo-root `server.json` is the template: static metadata, placeholder
`version`, empty `packages`; the script fills in the version and one
`registryType: mcpb` package per bundle with its release download URL and
`fileSha256`), installs a pinned, Sigstore-verified `mcp-publisher`, runs
`mcp-publisher validate`, `login github-oidc` and `publish`. It publishes
`io.github.mo-cmyk/mazarbul` to `registry.modelcontextprotocol.io`; PulseMCP
and other aggregators sync from there.

- **Auth:** GitHub Actions OIDC (`permissions: id-token: write`). The registry
  grants `io.github.<repo owner>/*` from the token, so there is no secret and
  no one-time namespace step.
- **Registry rules:** `description` at most 100 chars; MCPB `identifier` must be
  a public `https://github.com/<owner>/<repo>/releases/download/<tag>/<file>`
  URL containing `mcp` (the `.mcpb` extension satisfies this) that the
  registry HEAD-checks at publish time; `fileSha256` is required. A published
  version is immutable, so re-running the job for the same tag fails harmlessly.
- **Platform selection:** `server.json` packages carry no OS/arch field;
  clients tell the bundles apart by file name (and the bundle manifest's
  `compatibility.platforms`). darwin/arm64 is listed first.
- **Upgrade `mcp-publisher`:** bump `MCP_PUBLISHER_VERSION` in the job; the
  cosign check pins the certificate identity to that tag.

Verify locally without publishing:

```bash
scripts/mcpb-bundle.sh <stage> v0.0.0 darwin /tmp/m.mcpb   # stage = maz + lib/
scripts/mcp-server-json.sh v0.0.0 checksums.txt > /tmp/server.json
mcp-publisher validate /tmp/server.json
```

## Activation gates (dormant by default)

- **Release publishing** is gated by the **`ENABLE_RELEASE_BUILD`** repo
  variable in `release-please.yml`. Until it is set to `true`, merging a
  release-please PR cuts the tag/release but builds nothing.
- **Homebrew tap** does not exist yet; the formula will be authored in the
  tap-activation issue against `mo-cmyk/homebrew-mazarbul`.
- **`scripts/install.sh`** is written and verified but not yet hosted/referenced
  as the primary install — there is no published release to download. It is a
  best-practice installer: OS/arch detect → resolve release → download +
  **sha256-verify before placing** → install `maz`+lib (FHS `bin/`+`lib/`) →
  **add `bin/` to PATH** (idempotent, shell-aware: zsh/bash/fish; `--no-modify-path`
  to opt out) → **bootstrap the provider**: install Ollama if missing + pull the
  embedding/chat models maz needs (`--no-ollama` to opt out; skipped if present).
  `--init` runs `maz init` to finish. Env knobs: `MAZ_VERSION`, `MAZ_CHANNEL`,
  `MAZ_BASE_URL`, `MAZ_INSTALL_PREFIX`, `MAZ_INSTALL_TOKEN` / `GITHUB_TOKEN`,
  `MAZ_API_BASE`, `MAZ_EMBED_MODEL`, `MAZ_CHAT_MODEL`.

To activate (child issues): **#243** publishes a real release — **Phase 1**
(private soak) sets `ENABLE_RELEASE_BUILD=true`, lets release-please cut `vX.Y.Z`,
and ships token-gated install + the `MAZ_CHANNEL` switch (see next section);
**Phase 2** (at launch) flips the README to lead with the tokenless `curl | sh`
one-liner. **#244** creates the tap + `HOMEBREW_TAP_GITHUB_TOKEN` secret + the
`brew` formula.

## Token-gated private soak (#243, Phase 1)

Before the repo goes public we soak the install path against the **private** repo.
`scripts/install.sh` reads an auth token from the environment and, when one is set,
fetches assets through the GitHub REST API instead of the public browser-download
URL (which does not serve private-repo assets):

```bash
curl -fsSL https://raw.githubusercontent.com/mo-cmyk/mazarbul/main/scripts/install.sh \
  | MAZ_INSTALL_TOKEN=github_pat_… sh
```

- **Token:** `MAZ_INSTALL_TOKEN` (preferred) or `GITHUB_TOKEN` (fallback). A
  fine-grained PAT with **Contents: read** on the repo is enough; a classic PAT
  with `repo` scope also works. The token is env-only — never logged, written to
  disk, or placed on a URL — and is **dropped on the redirect** to the asset's
  signed storage URL (curl ≥ 7.83 strips `Authorization` cross-origin; the script
  never uses `--location-trusted`).
- **Download path:** token present → `GET /releases/tags/<tag>` to resolve each
  asset id, then `GET /releases/assets/<id>` (`Accept: application/octet-stream`).
  No token → the public CDN `releases/download/<tag>/<asset>` path (unchanged, not
  API-rate-limited). The sha256-verify gate runs identically on both.
- **Channel:** `MAZ_CHANNEL=stable` (default) → `releases/latest`;
  `MAZ_CHANNEL=nightly` → the newest release entry (forward-compatible placeholder;
  the nightly tag scheme lands in #258). A pinned `MAZ_VERSION` always wins.
- **`MAZ_API_BASE`** (default `https://api.github.com`) overrides the API host —
  used to test the token path without a real private repo (and GHE-forward).
- The self-update command (#257) reads the same token + channel env contract.

## Self-update (#257)

`maz upgrade` re-runs install.sh's download/verify/place steps in Go (package
`internal/selfupdate`), against the same env contract (`MAZ_CHANNEL`,
`MAZ_INSTALL_TOKEN`/`GITHUB_TOKEN`, `MAZ_API_BASE`, `MAZ_BASE_URL`), and skips
PATH/Ollama/init:

- **Resolve → download → verify → extract → smoke → swap.** It resolves the
  latest tag for the channel (or `--version <tag>`), downloads the
  `<os>_<arch>` archive + `checksums.txt`, sha256-verifies before touching
  anything, extracts into a staging tree, runs `<stage>/bin/maz version` as a
  smoke test (proves the new binary resolves liblbug via the `../lib` rpath),
  then replaces `bin/maz` + the `lib/liblbug.*` set atomically (per-file
  temp+rename with `.bak` rollback; stale engine libs pruned). Because `maz`
  bundles a dynamic engine lib, the binary and lib are always swapped together.
- **Install-method aware.** A Homebrew binary (resolved path under a Cellar /
  `/opt/homebrew` / linuxbrew prefix) refuses to self-replace and points at
  `brew upgrade mazarbul` (#244 owns that path). A non-`bin/`+`lib/` layout, or
  an unwritable prefix, fails up front — never a half-update.
- **Token + redirect.** The private-soak path carries the token on the API
  asset GET and drops it on the cross-host redirect to signed storage (a
  `CheckRedirect` strips `Authorization` on any host change). No token against a
  private repo surfaces an actionable "set GITHUB_TOKEN" error.
- **Daemon.** After a swap, a running daemon (which holds the old binary/lib
  open) is restarted onto the new version: `maz daemon stop` (a launchd-managed
  daemon is booted out and relaunched by its KeepAlive), then a respawn for a
  manually-started daemon. A restart failure degrades to an advisory.
- **`maz upgrade --check`** resolves and compares without downloading.

### Passive update-check

After any one-shot command, `maz` may print a one-line
`a newer maz is available: vX.Y.Z (run maz upgrade)` hint to **stderr**. It is:

- **Cached ~24h** at `<cache>/update-check.json` (`config.OwnedDaemonLogDir()`),
  read synchronously for the hint and refreshed by a fail-silent 2s background
  goroutine. No telemetry: a plain unauthenticated (or token-carrying) GET to the
  Releases API, sending no identifiers.
- **Gated:** suppressed for `dev`/non-semver builds, in CI (`CI` set),
  off-TTY/non-text output, long-running server commands, the `upgrade` command
  itself, and by `--no-update-check` / `MAZ_NO_UPDATE_CHECK=1`.

`maz version` prints `version` + `commit` + `date` (plain and `--output json`);
the check compares the build's `version.Version` against the resolved latest.

## Verifying locally

```bash
# rpath rewrite + portability proof
go build -tags=sqlite_fts5 -o /tmp/m/bin/maz ./cmd/maz
scripts/release-rpath.sh /tmp/m/bin/maz darwin   # or linux
# stage the bundled lib under /tmp/m/lib, then:
/tmp/m/bin/maz --version    # resolves liblbug from ../lib, not the module cache

# installer against a local "release"
MAZ_VERSION=v0.0.0 MAZ_BASE_URL="file:///path/to/dist" \
  scripts/install.sh --prefix /tmp/prefix
```
