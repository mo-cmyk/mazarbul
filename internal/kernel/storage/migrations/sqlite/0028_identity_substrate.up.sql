-- 0028_identity_substrate: principals + devices projection tables for the
-- signed per-writer identity substrate (#209).
--
-- principals: one row per registered principal (a person). Holds the
-- public keys other writers verify against, the key fingerprint, and the
-- optional GitHub trust anchor (sameAs + immutable user id + the SSHSIG
-- attestation that binds the ed25519 identity to a GitHub account).
--
-- devices: one row per enrolled device, keyed by device id. The verifier
-- resolves a signing key by sig_key_id to check team-scope event signatures.
-- sig_key_id is NOT unique: identity keys derive deterministically from the
-- Watchword (recoverable, by design), so every device of one principal shares
-- the same key and fingerprint until per-device keys land (#213). The index is
-- a lookup accelerator, not a uniqueness constraint. Both tables are populated
-- by dispatcher handlers replaying principalRegistered / deviceEnrolled events,
-- so a Rebuild reconstructs them from the log.

CREATE TABLE principals (
    principal_uid      TEXT PRIMARY KEY NOT NULL,
    display_name       TEXT NOT NULL,
    sign_public_key    TEXT NOT NULL,           -- base64 ed25519 public key
    wrap_public_key    TEXT NOT NULL,           -- base64 x25519 public key
    sig_key_id         TEXT NOT NULL,           -- "kid:..." fingerprint
    create_time        TEXT NOT NULL,           -- RFC 3339 UTC
    github_login       TEXT,                    -- e.g. "octocat" (NULL = unconfigured)
    github_user_id     INTEGER,                 -- immutable GitHub numeric id
    github_anchor      TEXT NOT NULL DEFAULT 'none' CHECK(github_anchor IN ('ssh','none')),
    github_attestation TEXT,                    -- armored SSHSIG (NULL when anchor='none')
    github_anchor_key  TEXT                     -- vouching authorized_keys line
);

CREATE INDEX principals_github_user_id ON principals (github_user_id);

CREATE TABLE devices (
    device_id        TEXT PRIMARY KEY NOT NULL,
    principal_uid    TEXT NOT NULL,
    sign_public_key  TEXT NOT NULL,             -- base64 ed25519 public key
    sig_key_id       TEXT NOT NULL,             -- "kid:..." fingerprint
    device_label     TEXT,
    signature        TEXT NOT NULL,             -- base64 ed25519 enrollment signature (TOFU at genesis)
    create_time      TEXT NOT NULL
);

-- Verifier hot path: resolve a device public key from an event's sigKeyId.
-- Non-unique (see header): one principal's devices share a derived key today.
CREATE INDEX devices_sig_key_id ON devices (sig_key_id);
CREATE INDEX devices_principal ON devices (principal_uid);
