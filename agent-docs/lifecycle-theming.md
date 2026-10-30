# Lifecycle theming

Mazarbul's voice is bicameral. Thematic at emotional moments where richness lands. Plain and technical everywhere else. This page is the binding reference.

## The rule

Theme at emotional moments only. Stay plain everywhere mechanical. When in doubt, choose plain. Themed text earns its place by lining up with a moment the user feels; everywhere else, themed text feels like noise.

## Themed moments

These are the moments where rich language, light Tolkien references, and the Book-of-Records voice are appropriate.

- **First init.** `maz init` is a welcome moment. Slogan front and centre: *"Mazarbul, the system of records for the era of agents."* A single scroll glyph on the splash. Wizard step labels can carry voice ("Choose where the records will live", "Inscribe the first page").
- **`maz mellon` device pairing.** *"Mellon, speak, friend, and enter."* Pairing code rendered with theatre; success animates a quiet handshake.
- **Watchword recovery.** *"Restore from your Watchword."* The 20-word Watchword block is the centrepiece; surrounding copy reinforces the gravity ("These twenty words are your only path back").
- **Deletion.** *"The Book retains the record of this forgetting."* A user-initiated `maz forget` runs a tombstone event, not a delete; the language reinforces that.
- **Shared review workflows.** *"Calling the council."* Multi-scope review surfaces (team-wide pending queue, shared edits) frame the moment with voice.
- **Key rotation.** *"Rekey the seals."* Confirmation banner before the rotation begins.
- **Sync launch.** First-time `maz sync` setup carries voice; subsequent runs are plain.
- **Team formation.** First creation of a team scope (`maz team create`) is a moment; member adds are plain.

## Plain moments

These are mechanical and must stay neutral.

- **Env vars.** `MAZ_API_KEY`, `MAZ_DATA_DIR`, `MAZ_THEME`. **Never** `MELLON_KEY`, `WATCHWORD_DIR`, `MITHRIL_THEME`.
- **Config keys.** `[providers.anthropic]`, `[connectors.gmail]`, `[tui.theme]`. **Never** `[forges.anthropic.mithrandir]`.
- **Error codes.** `E_AUTH_FAILED`, `E_SYNC_CONFLICT`, `E_BUDGET_EXCEEDED`. **Never** themed.
- **Public APIs and OpenAPI specs.** `POST /v1/events`, `entities/{ulid}`, `createTime`. Standard AIP. **Never** themed.
- **SDK method names.** `client.Remember(ctx, fact)`, `client.Recall(ctx, query)`, `client.Forget(ctx, id)`. **Never** `client.Inscribe()` or `client.Scry()`.
- **Log messages.** Structured zap logs use plain operational language. *"failed to acquire lock on event log"*, not *"the seal would not break"*.
- **Error messages to users.** Plain. *"Could not connect to the sync remote. Check `MAZ_SYNC_REMOTE`."*

## Semantic color is not voice

Mazarbul's CLI uses **git-style semantic color** for scannable command output — diffs, status lines, sync results, doctor checks, log output, recall. This is a separate **technical register** with its own rules. The two are orthogonal: the voice rules above govern *language*; semantic color governs *terminal appearance*.

### What semantic color is

- **Gated.** Every colour-bearing call flows through `internal/cli/interactive.ShouldStyle`, which honours `NO_COLOR`, `--color=auto|always|never`, `CLICOLOR`, and `FORCE_COLOR`. JSON and ndjson output is never coloured regardless of flags.
- **Symbol-first.** A glyph (✓ ✗ ⚠ ● + −) always carries the semantic meaning; colour is layered on top as an accelerator. Turning off colour via `NO_COLOR` never loses information.
- **Palette-sourced.** All values come from `internal/tui/style`. No raw hex literals at call sites. Every colour collapses to `lipgloss.NoColor{}` when `NO_COLOR` is set.
- **Universal conventions.** Green means added, red means removed, amber marks identifiers, cyan marks metadata. These are cross-product developer conventions — not Tolkien references.

### What semantic color is not

- Semantic color is not governed by the "themed moments" or "plain moments" rules above. Those rules govern *what words are written*; semantic color governs *how the terminal renders those words*.
- Semantic color does not carry narrative voice. `c.Added("+entity created")` is a diff marker, not an opportunity for *"The Book receives its new page."*
- Semantic color does not apply to log output, error codes, API responses, or SDK method names — the plain-moment rules still govern the *text* in those places.

---

## Skill names are thematic

Internal skill names (the files under `.claude/skills/`) **are** thematic because they inform the agent's mental model and never leak to users. Examples (subject to actual adoption):

- `scrying-deeply`: deep multi-pass recall.
- `calling-the-council`: multi-agent review.
- `inscribing-records`: bulk ingestion.

These names appear in agent prompts and `agent-docs/`, never in user-facing CLI output.

## Examples

### Good themed text

`maz init` welcome splash:

```
              Mazarbul
   the system of records for the era of agents

   Where will your records live?
   > ~/.mazarbul   (default)
```

`maz mellon` confirmation:

```
   Mellon. Speak, friend, and enter.

   Pairing code: a4f2-91c8-2b7e
   Expires in 60 seconds.
```

### Bad themed text (mechanical place)

```
# Bad: env var with theme
MELLON_KEY=anth-sk-...

# Bad: themed config key
[forge.scrying.depth]
hops = 3

# Bad: themed error
E_THE_PATH_IS_LOST

# Bad: themed SDK method
client.Inscribe(ctx, "remember this")
```

### Good plain text (mechanical place)

```
# Good
MAZ_API_KEY=anth-sk-...

[providers.anthropic]
model = "claude-opus-4-7"

E_AUTH_FAILED

client.Remember(ctx, "remember this")
```

### Good plain text (themed moment, still restrained)

```
   The Book retains the record of this forgetting.

   Tombstone written for entities/01HNXKQ3M7VP8R4G5T2N9F0H
   3 derived facts hidden from recall.
```

Restrained, not florid. One themed line, then operational detail.
