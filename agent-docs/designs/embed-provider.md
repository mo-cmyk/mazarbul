# Embed provider design (optional in M1)

**Status**: Implemented (#105)
**Last updated**: 2026-05-25

## Context

The M1 walking skeleton shipped with Ollama BGE-M3 as the only embedding
option. `maz init` hard-required a running Ollama daemon — even for users
who picked Anthropic for chat — because the wizard probed Ollama before
the embed-confirm step, and `internal/cli/init.go` unconditionally wrote
`[providers.ollama]` plus `defaults.embed = "ollama"`.

That left no clean exit path for:

- A privacy-minded user who wants local-first chat *but* hasn't installed
  Ollama yet.
- An Anthropic-only user who wants chat without recall and doesn't want a
  second daemon on their machine.

Both populations were stranded at `providerStateOllamaUnreachable` with
no recourse. Issue #105 makes embeddings opt-out so the walking skeleton
boots with a single provider.

## Goals

- [x] `maz init` completes without contacting Ollama when the user opts out.
- [x] Recall (MCP + CLI) returns a clear, agent-routable error when
      embeddings are disabled — not a probe failure on first use.
- [x] `maz doctor` reports the disabled state as **WARN** (not FAIL) and
      stops registering Ollama probes the user never asked for.
- [x] The contract is durable enough for `maz remember` (#43) to lean on
      without re-deriving it.

## Non-goals

- Voyage / Cohere embed clients. They are M3 plugin-loader work. This
  design names them as the first remote targets so the slot is shaped
  correctly, but no provider code lands with #105.
- Lazy / on-demand embedding (compute at recall time). Different
  refactor, M2+ if it lands at all.
- Backwards-compatibility migrations from older config shapes. M1 is
  pre-1.0; the only valid prior state is `defaults.embed = "ollama"`.

## Surface

### The sentinel

The disabled state is the string `"none"`, written into
`UserConfig.Defaults.Embed` (TOML key `defaults.embed`):

```toml
[defaults]
chat = "anthropic"
embed = "none"
```

Two constants pin the value at compile time so a typo in either consumer
fails the build instead of silently mis-routing:

| Location | Symbol |
|---|---|
| `internal/tui/wizard/provider.go` | `EmbedProviderNone = "none"` |
| `internal/daemon/wiring.go` | `embedProviderNone = "none"` (unexported, daemon-internal) |
| `internal/doctor/checks.go` | `embedProviderNone = "none"` (unexported, mirror for the doctor) |

Empty string (`defaults.embed = ""`) keeps its pre-#105 semantics:
**fall back to the M1 default "ollama"**. This preserves the synthesised
default the daemon constructs when no `config.toml` exists yet
(`loadUserConfigOrDefault` in `internal/daemon/wiring.go`). The explicit
`"none"` sentinel and the implicit-empty fallback are different states
on purpose: the explicit form encodes a user decision, the implicit form
encodes "we haven't asked yet".

### The wizard

`internal/tui/wizard/provider.go` replaces the pre-#105 `huh.Confirm`
with a `huh.Select[string]` carrying three options:

| Option label | Value | Effect |
|---|---|---|
| `Yes — use Ollama BGE-M3 (local; recall on)` | `embedChoiceYes` | Existing path. `ProviderChoice.EmbedProvider = "ollama"`, `EmbedModel = ollama.DefaultEmbedModel`. |
| `Skip — no embeddings (recall disabled; re-init later to enable)` | `embedChoiceSkip` | `ProviderChoice.EmbedProvider = "none"`, `EmbedModel = ""`. |
| `Cancel — exit the wizard` | `embedChoiceCancel` | `outcomeCancel`. |

Default is `Yes` so the pre-#105 happy-path keystroke sequence (Enter on
every step) still produces the M1 Ollama setup. The 3-way select sits in
state `providerStateEmbedSelect` (renamed from `providerStateEmbedConfirm`).

`'s'` (skip) is also accepted on the `providerStateOllamaUnreachable` and
`providerStateOllamaPullHint` screens. Without those escape hatches, an
Anthropic-chat user whose machine has no Ollama daemon would still be
stranded — the probe runs before the embed-select form, so a permanent
probe failure renders the form unreachable. Pressing `s` coerces the
choice to `(chat=anthropic, embed=none)` and finalises the wizard.

**Key-check routing.** When the user pressed `s` after originally
selecting Ollama for chat, the wizard never ran the Anthropic
key-check (Ollama-chat opts out of the keychain round-trip). A naive
finalize would persist a `chat=anthropic` config with no secret
behind it. The skip path therefore sets `pendingEmbedSkip` and
routes through `providerStateKeyCheck`; if the keychain has no key,
the user is prompted for one before the wizard finalises with
`embed=none`. Anthropic-chat callers (key already verified) finalise
directly.

### The CLI

`maz init` grows a `--embed` flag:

```
maz init --embed=ollama    # default; previous behaviour
maz init --embed=none      # disabled
maz init --embed=voyage    # rejected at validation (until M3 plugin)
```

`--embed-model` is documented as "ignored when --embed=none" and the
non-TUI writer drops `[providers.ollama].embed_model` accordingly.

### The init writer

`internal/cli/init.go` builds the `Providers` map incrementally so
`[providers.ollama]` is omitted entirely when neither chat nor embed
resolves to Ollama. An Anthropic + Skip install produces the
minimum-viable config:

```toml
schema_version = 1

[providers.anthropic]
model = "claude-opus-4-7"
secret = "secrets://anthropic.api_key"

[defaults]
chat = "anthropic"
embed = "none"
```

This matches the "one provider" framing in the issue and keeps
`maz doctor` honest: it can't try to probe a daemon the user never asked
for.

The conditional is shared by the TUI path (`runInitTUI`), the CLI path
(`runInitCLI`), and the JSON path (`runInitJSON`) via two small helpers:
`ollamaTableNeeded(chat, embed string) bool` and
`ollamaEmbedFor(embedProvider, embedModel string) string`.

### The daemon wiring

`internal/daemon/wiring.go` evolves on three fronts:

1. **`buildEmbedder` return type** changes from `*ollama.Client` to
   `kernel.Embedder`. `(nil, nil)` is a first-class result documented in
   the godoc:

   ```go
   func buildEmbedder(ctx context.Context, user config.UserConfig, logger *zap.Logger) (kernel.Embedder, error) {
       key := user.Defaults.Embed
       if key == "" {
           key = embedProviderOllama
       }
       if key == embedProviderNone {
           logger.Info("daemon: embeddings disabled by config ...")
           return nil, nil
       }
       // ... existing Ollama construction
   }
   ```

   Both `ireturn` and `nilnil` linters object — both are suppressed with
   inline `//nolint` directives that name the design rationale (nil
   sentinel for the disabled state; interface return so `*Wired.Embedder`
   keeps its slot shape).

2. **`OpenWiring`** branches on `embedder == nil` early: skip the
   sqlite-vec layer, the FTS layer, and the `kernel.NewHybridQuery` call
   entirely (the latter would reject the nil embedder anyway). The
   recall slot in `w.Tools` is filled by `tools.NewRecallDisabled()`.

3. **`Wired.Query`** is nil when embeddings are disabled. Callers MUST
   check for nil before invoking; today only the recall tool consumes
   it, and the disabled variant short-circuits before touching `Query`.

### The recall tool

`internal/mcp/tools/recall.go` exposes a second constructor:

```go
func NewRecall(q kernel.Query) *Recall
func NewRecallDisabled() *Recall
```

Both return the same `*Recall` shape with the same `Name`, `Description`,
`InputSchema`, and `OutputSchema` (so `tools/list` reports the same
surface in both states — agents discovering the tool see it advertised
either way). The disabled variant's `Invoke` short-circuits with:

```go
mcp.NewValidationError(
    "embeddings disabled in config.toml; run `maz init --embed=ollama` to enable",
    nil,
)
```

The recovery command names the actual flag (`--embed`, not `--provider`):
`--provider` selects the chat provider and would not re-enable recall.

#### Why E_VALIDATION, not E_INTERNAL

The recall request itself is well-formed; the *server* is the one in a
configuration the user explicitly chose. `E_VALIDATION` is the wire
contract for "the request is unservable under current configuration",
and it is the code agents already know to route on (the input-validator
path returns the same code, so a calling agent already has a handler
for it).

`E_INTERNAL` would imply a transient or unknown-cause failure that a
retry might fix. The disabled state is neither — it is a stable,
user-controlled configuration. Surfacing it as `E_INTERNAL` would
encourage retries that can never succeed.

We could introduce a third code (`E_UNAVAILABLE` / `E_FAILED_PRECONDITION`),
but the wire surface is small enough that a fourth code is unjustified
for a single use site. If a second "configured-off" tool lands later,
we'll revisit.

### The doctor

`internal/doctor/checks.go` registers a new check **`embed.disabled`**
under the `provider` subsystem when `defaults.embed == "none"`:

| Field | Value |
|---|---|
| `Name()` | `embed.disabled` |
| `Subsystem()` | `provider` |
| `Status` | `StatusWarn` |
| `Detail` | `embeddings disabled (defaults.embed=none); recall returns E_VALIDATION until `maz init --embed=ollama`` |

The Ollama-probe checks (`provider.ollama.daemon`,
`provider.ollama.embed-model`, `provider.chat.ping`) only register when a
slot actually resolves to Ollama. The pre-#105 heuristic that flagged an
Ollama install via residual per-table fields (`ollamaTable.ChatModel != ""
|| ollamaTable.EmbedModel != ""`) is dropped: a Skip install writes no
`[providers.ollama]` table at all, so the heuristic is moot; and a stale
field on an Anthropic+none install would otherwise cause spurious FAILs.

**Doctor mirrors the daemon resolver exactly.** Two contract details
that matter:

- *Case sensitivity:* the daemon compares `Defaults.Embed` to
  `"ollama"` / `"none"` byte-for-byte (see `internal/daemon/wiring.go:buildEmbedder`).
  Doctor uses the raw config value too — a hand-edited
  `defaults.embed = "None"` is rejected at daemon startup, so doctor
  reports the Ollama-probe FAIL rows instead of the disabled WARN.
  Production configs are unaffected because the wizard always writes
  lowercase.
- *Empty-embed fallback:* the daemon treats an empty `defaults.embed`
  as the M1 Ollama default (so a fresh install with no `config.toml`
  boots). Doctor applies the same fallback for `wantOllama`, so a
  legacy / hand-written config with an omitted embed default still
  triggers the Ollama daemon + model probes (which is what the user
  needs to see if the daemon then fails to start).

## Privacy and provenance

| Concern | Impact | Mitigation |
|---|---|---|
| Provenance attached | n/a | No new event types. |
| Scope tagging | n/a | No data-write path. |
| Append-only invariant | Yes | Preserved. The disabled path is read-side only. |
| Audit log entry | n/a | No new reads of sensitive scopes. |

The disabled state is a privacy *improvement*: a user who opts out of
embeddings ships zero text to a local model — nothing crosses the
process boundary.

## Risks

- **`maz remember` (#43) must respect the contract.** When #43 lands,
  its handler must check `Embedder == nil` (or equivalent gate on the
  wired state) and write the Episode event without the embed step.
  Without that gate, `remember` would crash on the disabled-embed
  install. #105 added an explicit acceptance-criterion bullet to #43's
  issue body to bake this in.

- **Lint suppressions on `buildEmbedder`.** Two inline `//nolint`
  directives (`ireturn`, `nilnil`) capture the deliberate API shape. A
  future refactor that removes the disabled sentinel must also remove
  the suppressions; the godoc cross-references both.

- **Schema discriminator deferred.** The longer-term fix for "rename
  `[providers.ollama]` to `[providers.ollama-local]`" is a `type` field
  on `ProviderTable`. Until that lands, the resolver still rejects any
  key that is neither `"ollama"` nor `"none"`. The error message names
  both supported keys so the rejection is actionable.

## Migration

None required for M1. The only valid prior state is
`defaults.embed = "ollama"`; pre-#105 builds did not emit any other
value. Existing config files continue to load and resolve to the Ollama
embedder.

A `maz init` re-run is idempotent: writing the new `Skip` choice
replaces the `[providers.ollama]` table with an empty/absent one, and
the daemon picks up the change on next start.

## M3 outlook (forward-looking, not in scope)

When the plugin loader lands, the embed slot accepts additional values:

```toml
[defaults]
chat = "anthropic"
embed = "voyage"            # remote managed embed provider

[providers.voyage]
model = "voyage-3"
secret = "secrets://voyage.api_key"
```

The disabled-state contract should carry forward unchanged:
`defaults.embed = "none"` means no embedder regardless of which other
providers are installed. The recall tool's disabled variant is the
contract surface the plugin loader will compose against; the validation
message stays the same so agents see one consistent shape.

The remote embed clients (Voyage, Cohere) are not tracked by a GitHub
issue today. They will be filed when M3 planning starts; this design
doc is the durable note that they are the expected first remote
embed providers.

## Acceptance criteria

- [x] Wizard offers Yes / Skip / Cancel.
- [x] `'s'` key on `OllamaUnreachable` and `OllamaPullHint` screens.
- [x] `[providers.ollama]` omitted from `config.toml` when neither slot uses Ollama.
- [x] `defaults.embed = "none"` is a first-class value at every layer.
- [x] Recall returns `E_VALIDATION` with the documented message.
- [x] `maz doctor` reports the disabled state as WARN, drops Ollama checks when the user opted out.
- [x] `--embed` CLI flag for the non-interactive path.
- [x] Tests at every layer: wizard, init writer, daemon wiring, recall tool, doctor.
- [x] #43 acceptance criterion updated to require nil-embedder tolerance.

## References

- Issue #105 — the embeddings-optional request.
- Issue #43 — `remember` MCP tool (nil-embedder contract added by #105).
- `internal/tui/wizard/provider.go` — the embed-select form and skip handlers.
- `internal/cli/init.go` — the conditional Ollama-table writer.
- `internal/daemon/wiring.go` — `buildEmbedder` returning the sentinel and `OpenWiring` branching on it.
- `internal/mcp/tools/recall.go` — `NewRecallDisabled` and the E_VALIDATION constant message.
- `internal/doctor/checks.go` — `newEmbedDisabledCheck` and the conditional Ollama probes.
