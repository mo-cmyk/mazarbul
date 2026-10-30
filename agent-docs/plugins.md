# Plugins

Mazarbul's plugin model splits extensibility into three categories. Manifests are TOML. Install command is plain: `maz install <name>` (no category prefix). The full plugin loader (third-party signed packages, registry, version pinning) is roadmap work; this page documents the shipped model.

## Three categories

### Providers

Implementations of kernel-required capabilities. **One active per slot.** Slots:

- `chat`: LLM chat completion (Anthropic / OpenAI / Ollama / etc.)
- `embed`: embeddings for the vector index
- `storage`: event log persistence (default: local JSONL)
- `vector`: vector index (default: sqlite-vec)
- `graph`: graph database (default: LadybugDB)
- `fts`: full-text search (default: FTS5)
- `sync`: sync transport (default: git)
- `auth`: auth provider for shared scopes
- `parser`: document parser (default: Docling)

Switching providers is a config change; the kernel only ever talks to slots.

### Connectors

Bridges to external services. **Many active simultaneously.** Typically MCP servers. Examples: Gmail, Slack, Notion, Linear, GitHub, Drive, Dropbox, OpenRegister, browser history, meeting transcripts.

### Extensions

Everything else: schema packs (`business-de`), enrichment sources (Crunchbase), custom parsers, custom commands. Loadable but optional.

## Install UX

```
maz install anthropic       # provider
maz install gmail           # connector
maz install crunchbase      # extension
```

Resolves via the plugin registry, fetches the manifest + binary (or MCP server config), verifies signature, installs into `MAZ_DATA_DIR/plugins/<name>/`, registers in the active slot or connector list. TUI mode renders a progress spinner with phase labels; CLI mode prints one line of confirmation per phase.

<!-- TODO(m3): expand: signing scheme, registry URL, offline install via tarball, version pinning, conflict resolution between competing providers in the same slot. -->

## Remote distribution: schema packs and presets (#224)

Beyond a builtin name or a local directory, `maz install` and `maz preset install` accept a **remote git source**, so packs and presets can be shared the way Claude Code distributes skills:

```
maz install github.com/acme/crm-pack
maz install github.com/acme/packs@v1.2.0#crm --checksum sha256:abc123...
maz preset install github.com/acme/crm-preset --yes
maz preset install github.com/acme/presets@v1.0.0#crm --yes --checksum sha256:...
```

**Spec syntax** — `github.com/user/repo[@ref][#subdir]`, plus full git URLs (`https://`, `ssh://`, `git@host:owner/repo`). `@ref` pins a branch, tag, or commit; `#subdir` selects a bundle nested inside a multi-pack repo. Short `github.com|gitlab.com|bitbucket.org/owner/repo` handles synthesize an `https://` clone URL; anything else is taken verbatim.

**Fetch mechanism** — a system `git` shell-out (no new dependency, supports any host the user's git already authenticates). Detection is lexical (`fetch.IsRemoteSpec`); a remote arg routes through `internal/plugin/fetch`, everything else stays on the unchanged local-dir / builtin path. The clone runs with a hardened, hermetic environment (`GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/true`, `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM=/dev/null`, and a scrub of any inherited `GIT_DIR`/`GIT_INDEX_FILE`/`GIT_WORK_TREE`), then resolves `HEAD` to a 40-char commit SHA — the **pin**. The bundle is copied into a content-addressed cache under `MAZ_DATA_DIR/packs-cache/<key>`; a verified cache hit is reused without re-cloning (offline-tolerant).

**No new trust path.** Fetching changes only *where the bytes came from*, never how they are trusted. Once on disk, a remote bundle flows through the identical `schemapack.ParseDir` + #210 install pipeline: the parse-time instructions byte cap, the sanitize + non-spoofable fence, render-exclusion, the scope-aware privacy marker, and the human-confirm gate. Because the `instructions.md` body flows into the highest-priority agent-control surface (the getContext "Operating instructions" section), the distribution layer **strengthens** #210's defenses rather than weakening them.

**Human gate** — a non-builtin preset (local **or** remote) surfaces its verbatim instructions body and refuses to install until confirmed: answer the interactive `[y/N]` prompt, or pass `--yes`. A non-interactive install (JSON/non-TTY) without `--yes` is refused with exit 1. The gate keys off `source != sourceBuiltin` (fail-closed): only builtins, shipped and reviewed in the binary, skip it.

**Integrity** — `--checksum sha256:...` is an always-optional tamper gate. When supplied, a fetched bundle whose canonical contents do not match is refused **before** the body is parsed, cached, or surfaced (a malformed manifest never even surfaces — the checksum error wins first). The canonical checksum covers only the four-file contract `ParseDir` consumes (`manifest.toml`, `predicates.yaml`, `instructions.md`, sorted `kinds/*.yaml|*.yml`), each framed `path\0 || uint64LE(len) || raw-bytes` into one running sha256 — so it is stable across unrelated repo edits (README, LICENSE, CI) and reproducible from the bundle directory alone. Publishers compute and publish it with `maz preset package`:

```
maz preset package ./my-preset/
#  → Checksum  sha256:abc123...
#  Publish this checksum so installers can pin it.
```

`maz preset package <dir>` is read-only and daemon-free: it runs the same #210 validators (naming the offending file on a bad manifest/kind/predicate or an over-cap `instructions.md`) and prints the canonical checksum plus a summary. `--output json` emits a machine-readable result.

**SECURITY — symlinks.** A malicious bundle can contain symlinks (e.g. `instructions.md → ~/.ssh/id_ed25519`) whose target would otherwise be dumped into the review block or hashed into the checksum. Both `BundleChecksum` and the fetcher's tree copy refuse any symlinked canonical file (`ErrBundleSymlink`), so the scan is load-bearing for the human gate.

**Provenance** — a remote preset install records `sourceUrl`, `pinnedRef` (the resolved commit SHA), and `checksum` (always computed, even without `--checksum`) onto the `presetInstalled` event (`v0.2.0`), projected to the `presets` table (migration `0034_preset_provenance`) and surfaced by `maz preset show`. Plain schema packs have no `presets` row, so the `--checksum` gate still fires on `maz install` but the provenance is discarded. Provenance is **never** exposed to agents: `listPresets` stays read-only and drops the body, source, ref, and checksum — an agent is already steered via getContext and never needs the raw bundle identity. There is deliberately **no** `installPreset`/`fetchPreset`/`packagePreset` MCP tool; remote fetch and the human gate live entirely in the CLI.

**Deferred to follow-ups** — a curated marketplace/discovery index (handle → git source, `maz preset search`, install-by-handle) and optional SSHSIG bundle signatures (reusing the #209/#220 author-key machinery). By-URL install + verifiable publish is the complete distribution substrate; the index and signatures are separate efforts.

## Manifest schema (TOML)

The canonical reference manifest lives at `internal/provider/anthropic/plugin.toml` (Anthropic is the built-in provider; a future full plugin loader will read this file for third-party installed plugins). The shape:

```toml
# plugin.toml
name = "anthropic"
version = "0.1.0"
category = "provider"
slot = "chat"
description = "Anthropic Claude chat provider (Messages API, non-streaming)"
license = "Apache-2.0"
homepage = "https://github.com/mo-cmyk/mazarbul"

[entrypoint]
type = "go-plugin"          # or "mcp-server" for connectors
binary = "anthropic-provider"

[config]
api_key_env = "ANTHROPIC_API_KEY"
default_model = "claude-opus-4-7"
anthropic_version = "2023-06-01"
```

`api_key_env` is the legacy environment variable a user may already have exported; the canonical form is `MAZ_SECRET_<UPPER_DOT_TO_UNDERSCORE>` (e.g., `MAZ_SECRET_ANTHROPIC_API_KEY`). Both are read by `secret.EnvProvider`; the canonical form wins when both are set.

### Compatibility and scopes (`api_version`, `scopes`)

Two **optional** manifest fields gate install. Both are absent-safe: a manifest declaring neither installs exactly as before they existed, with no prompt and no gate.

```toml
api_version = "v1.0.0"                   # host plugin-API version this plugin targets
scopes      = ["project", "personal"]    # read scopes the plugin requests
```

- **`api_version`** — the host plugin-API version the plugin was built against (semver; a leading `v` is optional). The host implements `plugin.HostAPIVersion`; `plugin.CheckAPICompatibility` refuses a plugin whose **major differs** from the host's, or whose version is **newer** than the host's (a plugin from the future). An incompatible version fails install with a clear `plugin requires X but this host implements Y` message; a malformed value is `E_VALIDATION`. The rule: bump **MAJOR** on a breaking interface change, **MINOR** on a backward-compatible addition.
- **`scopes`** — the read scopes the plugin requests, each a recognised kernel scope (`project`, `personal`, `team`; validated by `kernel.ParseScopeSet`). An unknown token fails install with `E_VALIDATION`. When a manifest declares scopes, `maz install` / `maz preset install` prompt for consent (listing the requested scopes) before proceeding; `--yes` grants the declared set without prompting, and a non-interactive run without `--yes` is refused (scopes are never granted silently). The granted set is recorded in the install result (`grantedScopes`). Scope enforcement at read time is the kernel's job (`ScopeSet`, #206); a manifest can only ever *request* a scope the kernel already recognises.

<!-- TODO(m3): expand: full schema with JSON Schema, capability declarations, dependency expressions. -->

## Secret storage

Provider configuration that includes credentials (API keys, OAuth tokens) reads through `internal/secret`. The interface (`secret.Provider`) ships with two implementations:

- `secret.KeychainProvider`: macOS Keychain, Linux libsecret over D-Bus, Windows Credential Manager. Default for `maz init`.
- `secret.EnvProvider`: read-only, reads `MAZ_SECRET_*` and legacy env vars. CI fallback.

Use `secret.NewChain(env, keychain)` to compose them so env wins in CI without a branch at the call site. The Anthropic chat provider already does this; see `agent-docs/designs/chat-provider.md` for the full integration.

## User-editable config (`${data_dir}/config.toml`)

`maz init` writes the active provider selections to `${data_dir}/config.toml` (mode 0600). The kernel reads this file to decide which provider fills each slot. Schema (snake_case keys, table per provider):

```toml
schema_version = 1

[providers.anthropic]
model  = "claude-opus-4-7"
secret = "secrets://anthropic.api_key"

[providers.ollama]
base_url    = "http://localhost:11434"
chat_model  = ""            # only set when chat = "ollama"
embed_model = "snowflake-arctic-embed2"

[defaults]
chat  = "anthropic"
embed = "ollama"
```

Notes:

- **Table per provider, not per slot.** A future Voyage embed plugin lands as `[providers.voyage]` without disturbing the existing tables. The slot assignment lives under `[defaults]`.
- **Secrets are referenced, not stored.** `secret = "secrets://<canonical-name>"` is a lookup URI; the plugin loader strips the `secrets://` prefix and calls `secret.Provider.Get(name)`. The TOML never contains key material.
- **`schema_version` is the migration anchor.** Bumped on incompatible changes; readers refuse versions they do not understand.

The Go types live in `internal/config/userconfig.go` (`UserConfig`, `ProviderTable`, `DefaultsTable`) along with atomic `WriteUserConfig` and `LoadUserConfig` helpers.

## Examples

<!-- TODO(m3): worked examples for each of the three categories, including a connector that wraps an MCP server, a provider that implements the `chat` slot, and an extension that ships a YAML schema pack. -->
