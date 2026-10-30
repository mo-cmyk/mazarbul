-- 0031_team_membership: team membership ledger for cryptographically
-- enforced multi-writer team scopes (#213).
--
-- team_membership_events is the RAW append-only OR-set: one row per
-- teamMemberGranted / teamMemberRevoked event, keyed by the event's own
-- ULID (INSERT OR IGNORE ⇒ idempotent under replay). This is the durable
-- source of truth; the ACL below is a recomputed cache. The author_*
-- columns record the SIGNED authority (ev.Author), which the membership
-- fold uses to decide admission — a grant/revoke counts only when authored
-- by a principal reachable from the genesis self-grant.
--
-- team_membership_acl is the DERIVED materialized ACL: the least-fixpoint
-- result, fully recomputed (DELETE + re-INSERT) on every grant/revoke
-- apply, so a Rebuild reconstructs identical membership regardless of the
-- order events replay in. A principal absent from this table is NOT a
-- member (fail-closed). Quarantined events are simply the raw rows whose
-- author never reaches the ACL — derived, never a divergent write path.
--
-- The grantee_* columns persist the verification identity a grant vouches for
-- (signing key + device + name). They are stored here — not consumed inline at
-- apply time — so device enrollment can run AFTER the fold, over the admitted
-- set only: a quarantined (non-admitted) grant must never inject or overwrite a
-- signing key in the devices table before the fold's quarantine verdict.

CREATE TABLE team_membership_events (
    event_uid               TEXT PRIMARY KEY NOT NULL,  -- the grant/revoke event ULID (OR-set tag)
    event_kind              TEXT NOT NULL,              -- 'teamMemberGranted' | 'teamMemberRevoked'
    author_principal        TEXT NOT NULL,              -- ev.Author.PrincipalUID (signed authority)
    author_sig_key_id       TEXT NOT NULL DEFAULT '',   -- ev.Author.SigKeyID (audit)
    subject_principal       TEXT NOT NULL,              -- grantee | revoked principal
    grantee_wrap_public_key TEXT,                       -- grant only: base64 x25519 pub the DEK was sealed to
    wrapped_dek             TEXT,                       -- grant only: base64 NaCl sealed-box of the DEK
    dek_epoch               INTEGER,                    -- grant only: epoch of the wrapped DEK
    grantee_sign_public_key TEXT NOT NULL DEFAULT '',   -- grant only: base64 ed25519 verification key vouched for the grantee
    grantee_sig_key_id      TEXT NOT NULL DEFAULT '',   -- grant only: the grantee's sig key id
    grantee_device_id       TEXT NOT NULL DEFAULT '',   -- grant only: the grantee's device ULID
    grantee_display_name    TEXT NOT NULL DEFAULT '',   -- grant only: the grantee's self-reported name (for the team-scoped person entity)
    occurred_at             TEXT NOT NULL,              -- grantedAt | revokedAt, RFC3339Nano
    create_time             TEXT NOT NULL,              -- ev.CreateTime, RFC3339Nano
    scope                   TEXT NOT NULL DEFAULT 'team' -- carried for symmetry; always 'team'
);

CREATE INDEX team_membership_events_subject ON team_membership_events (subject_principal);
CREATE INDEX team_membership_events_author ON team_membership_events (author_principal);

CREATE TABLE team_membership_acl (
    principal_uid   TEXT PRIMARY KEY NOT NULL,  -- admitted, non-revoked member
    admitted_by_uid TEXT NOT NULL,              -- event_uid of the lowest-ULID grant that admitted them
    dek_epoch       INTEGER NOT NULL DEFAULT 0, -- epoch of the wrapped DEK in force for them
    wrapped_dek     TEXT NOT NULL DEFAULT '',   -- their current sealed DEK
    wrap_public_key TEXT NOT NULL DEFAULT '',   -- base64 x25519 pub the DEK was sealed to
    is_genesis      INTEGER NOT NULL DEFAULT 0  -- 1 for the TOFU root, else 0
);
