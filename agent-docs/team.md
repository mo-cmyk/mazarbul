# Team scope — trust layer

Reference for the multi-user team scope: roles, per-device keys, genesis
trust, and promoting personal records to the team. For the transport and
encryption mechanics see [sync.md](sync.md) and [privacy.md](privacy.md); for
principal/`self` resolution see [identity.md](identity.md).

The team scope is a **shared, always-encrypted brain**. Its data-encryption
key (DEK) is wrapped to each member's public key and published as
`recipients.jsonl` in the sync repo. Everything below governs *who may write
what*, *which device signed it*, and *whose genesis roots the team* — the
trust decisions that a plaintext transport alone cannot make.

Every trust decision is a **pure function of the event set**: the same accept /
reject / role verdict is reached on every device, whether it is applying a
live graft or replaying the whole log from scratch. That is what keeps a
team convergent without a central server.

## Roles (RBAC)

Four roles, increasing authority: **reader → member → admin → owner**.

| Role   | Read team | Write content | Grant / revoke / role-change |
|--------|:---------:|:-------------:|:----------------------------:|
| reader | ✓ | — | — |
| member | ✓ | ✓ | — |
| admin  | ✓ | ✓ | ✓ (not owner-level roles) |
| owner  | ✓ | ✓ | ✓ (including owner/admin) |

- The **genesis principal is always owner**, independent of any later event.
- A grant carries an optional `role` (absent ⇒ member). A separate
  `teamRoleChanged` event changes a role without re-wrapping key material.
- **Effective role** = newest-by-ULID among the admitting grant's role and any
  later `teamRoleChanged` events for that principal.
- **Last-owner protection**: a revoke or demote that would leave the team with
  zero owners is deterministically ignored (a pure property of the set, so
  every device agrees).
- **Authority**: role changes and revokes are honored only when authored by an
  owner/admin *at that event's position* in the ULID-ordered membership
  history; only an owner may set admin/owner roles.

Enforcement lives in two places, and they agree by construction:
- **Write time** — `AuthorizeTeamWrite` rejects a content write from a reader
  (`ErrPermission` → `E_PERMISSION`).
- **Graft / rebuild time** — the team verify path consults
  `TeamMemberRoleAt(author, eventUID)`, which re-folds only the membership
  events with UID `< eventUID`. A demote that post-dates a write can never
  retroactively drop that write; a reader-authored content event is dropped to
  Rejections on every device alike.

CLI / MCP:

```bash
maz team grant --from-file alice.joinkey --role member   # admit at a role
maz team role set <principalUid> --role admin            # change a role
maz team members --output json                            # roles + DEK epoch
```

MCP tools: `team_grant` (role param), `team_role_set`. The ACL projection
carries a `role` column; an existing single-owner team needs no data
migration — the fold derives genesis→owner, everyone else→member.

## Per-device keys

A principal identity key is Watchword-derived and recoverable. Each **device**
additionally mints a random ed25519 **subkey** at enrollment, so two devices
sharing one Watchword still sign distinctly.

- `deviceEnrolled` carries the device pubkey **vouched by a principal-key
  signature** over (deviceID, devicePubKey). `sig_key_id` is the device-pubkey
  fingerprint — unique per device.
- Events are signed by the device subkey; verification resolves
  `sig_key_id` → (principal, device) via the `devices` projection and requires
  the enrollment vouch chain. A subkey may never vouch another subkey.
- **Revocation** — `deviceRevoked{deviceId, sigKeyId}` (authored by the
  device's own principal or an owner/admin). Fold rule: events signed by a
  revoked device with UID `>` the revocation's UID are dropped; earlier events
  are retained. The device gate covers **all** team kinds, so a revoked
  (e.g. stolen) owner device cannot author grants, role changes, or a
  revenge-revocation. Concurrent cross-revocations resolve first-wins by UID.
- **Mellon pairing** shares the Watchword but the new device mints its own
  subkey and appends its own principal-vouched `deviceEnrolled` on first boot.

```bash
maz team devices list
maz team devices revoke <deviceId>
```

The original Watchword-derived key remains valid as "device 0"; old events
verify unchanged — no re-stamp.

- **Device 0 is vouchless and personal-scope only.** The Watchword-derived
  device-0 key carries no principal vouch (`vouch_signature IS NULL`); teammates
  learn its signing key through the **grant vouch**, not through a team-scope
  `deviceEnrolled`. So `ensureDeviceEnrolled` publishes a team-scope enrollment
  **only for a vouched subkey** — never for device 0. Symmetrically,
  `applyDeviceEnrolled` treats a vouchless team-scope `deviceEnrolled` as a
  benign idempotent no-op (skip, inject no `devices` row) rather than a hard
  apply error. Without this, a member's own vouchless team-scope enrollments
  (emitted by a pre-fix producer) failed `requireEnrollmentVouch` on replay, so
  `maz rebuild` errored (`ok=false`) for every non-genesis member — a member
  could never rebuild or repair their projection. Live-graft and rebuild now
  agree (the determinism invariant below).

## Genesis pinning

Without a pin, the membership fold trusts the lowest-ULID self-grant as
genesis (trust-on-first-use). A **pin** hardens this: it names the genesis
principal UID (optionally its signing-key fingerprint) so a malicious
low-ULID self-grant injected into the repo is quarantined instead of adopted.

- The pin is stored locally in `state.toml`, and published as a
  genesis-signed **`TEAM.json`** at the sync repo root — tamper-evident and
  readable before joining.
- A joining device **imports the pin automatically** from a *verified*
  `TEAM.json` on its next team pull: the anchor must name the fold's own
  genesis AND verify against the key that fold's genesis enrolled. An
  unverifiable or fold-mismatched `TEAM.json` is never pinned (a poisoned pin
  would fail-close the whole fold — a denial of service — so it is refused,
  not persisted).
- `maz team pin` inspects the local pin and the repo `TEAM.json`;
  `maz team pin --clear` drops a local pin so the next verified `TEAM.json`
  re-imports it (the poison-recovery loop). Both are **local-only** — a
  poisoned pin would break the daemon, so pinning never routes through it.

Doctor check `team.genesis_pin`: (1) a pin is present, (2) it matches the
fold's genesis, (3) the `TEAM.json` signature verifies against the enrolled
genesis key.

## Share to team (personal → team)

Promoting is **re-publish, not re-scope**: the sealer seals events by scope, so
re-scoping a row never reaches the team remote. `maz share` / the `shareToTeam`
MCP tool reads a personal record and appends *ordinary team-scoped events* of
the same kinds, with provenance `Source: "shared:<orig-uid>"`. The personal
original is untouched; both copies coexist as separate scope lenses. There is
no team→personal downscope.

```bash
maz share entities/<uid> --with-facts --with-relations   # share a subtree
maz share facts/<uid>                                     # single record
maz share "Alice Smith"                                   # by display name
```

- **Dedup**: before minting a team entity the tool matches the team scope by
  strong identifier (when a matcher is wired) then exact name+kind. A hit is
  returned as `status=reused` instead of duplicating.
- **Endpoint remap** (the correctness crux): a shared fact/relation must not
  reference a personal-scope UID that teammates cannot resolve. A subtree
  share threads a personal→team UID remap; a bare fact/relation share resolves
  its endpoints against the team scope via the same dedup lanes.
  - Bare share, endpoint has no team counterpart → `E_VALIDATION` (share the
    entity first).
  - Subtree share, a relation's object has no team counterpart →
    `status=skipped` with a `reason`, rather than an appended dangling
    reference.
- A reader-role caller surfaces `E_PERMISSION` at append.

Alias/identifier events inherit the parent entity's scope (create-entity and
add-identifier both stamp it), so a shared team entity is resolvable by its
identifiers on every member's device.

## New event kinds (this layer)

- `teamRoleChanged{subjectPrincipalUid, role}` — role change without re-wrap.
- `deviceRevoked{deviceId, sigKeyId}` — device revocation (fold-governed).
- `teamRegrantRequested{...}` — a member signals a DEK-epoch gap; an admin
  re-grants via the normal Grant (human in the loop, no auto-grant).

The `teamMemberGranted` event gained an optional `role`. Grant is still what
wraps DEK history to a member's key; role is orthogonal metadata on it.

## Doctor checks (this layer)

| Check | Warns when |
|-------|------------|
| `team.genesis_pin` | pin absent, fold-inconsistent, or `TEAM.json` sig invalid |
| `sync.dek_coverage` | local DEK ring is missing epochs present on the remote (revocation gap or under-grant) — repair via `maz team key --request-regrant` |
| `sync.history_rewrite` | the remote head is no longer a descendant of the last-seen head (force-push) — repair via `maz sync repair --scope team` |
| `memory.supersede_conflicts` | open functional-slot conflicts from concurrent writes |
| `memory.dup_candidates` | post-consume dedup found probable duplicates for review (`maz review dups`) |

## Convergence invariants (why it holds)

- **HLC order.** Every event sorts by `(effectiveHLC, UID)`; legacy events use
  an implicit HLC derived from immutable fields, identical on every machine.
  Fact slots and every "latest wins" reducer resolve by this order, so
  live-graft equals rebuild.
- **Position-pure trust.** Role, device-revocation, and genesis decisions
  re-fold only events strictly before the event under test — arrival order
  cannot flip a verdict.
- **Own-device sealing.** A device seals only the events it authored (across
  all scopes, since every scope is now consume-enabled); re-sealing a grafted
  peer event under this device's header would make the chunk fail
  `AuthorsMatchDevice` on every consumer. Pre-identity bootstrap events (signed
  by the public `local` placeholder) never seal into a device-tagged chunk —
  each device self-bootstraps its own ontology registry.

These are pinned by the two-user E2E suite (`wiring_sync_team_lifecycle_test`,
`wiring_sync_team_scenarios_test`) and the dispatcher permutation/property
tests (`dispatcher_convergence_property_test`).
