-- 0053_device_subkey: principal-vouched device subkeys and revocation.
--
-- devices gains two optional vouch columns populated only for subkey devices
-- (devices using a random signing key rather than the Watchword-derived
-- principal key). Absent values mean a legacy device-0 enrollment, which
-- remains valid without any back-fill.
--
-- device_revocations tracks every deviceRevoked event for the convergent
-- fold: an event authored by a revoked device with a UID greater than the
-- revocation's UID is dropped during graft and rebuild. Both device_id and
-- sig_key_id are indexed for the position-restricted lookup.

-- team_published latches whether this device's enrollment has been seen in a
-- TEAM-scoped deviceEnrolled event. Teammates can only verify a subkey's
-- events once the enrollment reaches them through the team log, so the boot
-- seeder re-publishes an enrollment to team scope until this latch is set.
-- Derived purely from applied events (scope of the enrollment), so rebuild
-- reproduces it deterministically on every peer.

ALTER TABLE devices ADD COLUMN vouch_sig_key_id TEXT;
ALTER TABLE devices ADD COLUMN vouch_signature TEXT;
ALTER TABLE devices ADD COLUMN team_published INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS device_revocations (
    event_uid        TEXT NOT NULL PRIMARY KEY,
    device_id        TEXT NOT NULL,
    sig_key_id       TEXT NOT NULL,
    principal_uid    TEXT NOT NULL,
    author_principal TEXT NOT NULL,
    revoked_at       TEXT NOT NULL,
    create_time      TEXT NOT NULL,
    reason           TEXT
);

CREATE INDEX IF NOT EXISTS idx_device_revocations_device  ON device_revocations (device_id);
CREATE INDEX IF NOT EXISTS idx_device_revocations_sig_key ON device_revocations (sig_key_id);
