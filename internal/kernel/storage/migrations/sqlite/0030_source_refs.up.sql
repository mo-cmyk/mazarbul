-- Migration 0030: external-source provenance primitive (issue #207).
--
-- The single shared provenance substrate (W3C PROV wasDerivedFrom) consumed by
-- external references (#208) and the ingestion connectors (#211/#212), so the
-- open source_type taxonomy and field set never fork. Each sourceRefRecorded
-- event materializes one row here, keyed by the SourceRef's ULID — which the
-- dispatcher DERIVES from dedup_key (ontology.SourceRefUID), not from the event
-- payload. uid is therefore a pure function of dedup_key, so PRIMARY KEY(uid)
-- and UNIQUE(dedup_key) are 1:1 and the sourceRefs/{uid} name recordSourceRef
-- returns always resolves.
--
-- UNIQUE(dedup_key) is the idempotency constraint: the dispatcher upserts via
-- INSERT ... ON CONFLICT(dedup_key) DO UPDATE, so two events sharing a dedup_key
-- — a Rebuild replay of the same event, or the same external item recorded twice
-- — collapse to one stable row. The incoming uid always equals the existing
-- row's uid (both derived from dedup_key), so identity never moves; source_event_uid
-- IS re-stamped, so the back-link tracks the most recently applied carrying event.
-- `maz rebuild` re-Applies every sourceRefRecorded event (HasEvent does not track
-- it) and the upsert is idempotent, so the projection reconstructs deterministically.
--
-- scope DEFAULT 'personal' matches kernel.DefaultScope and is the fail-safe
-- direction for the #206 read boundary: an untagged event materializes as
-- personal (visible under the default {project,personal} read, never leaked to
-- team) rather than as an out-of-scope blank.
--
-- Provenance recorded here is SELF-ASSERTED and UNVERIFIED — there is no
-- principal/signature trust for a SourceRef yet (#209 signs the carrying
-- event's authorship but does not attest the external claim). getSourceRef
-- labels this explicitly. The plaintext-scope render projection (#207 render
-- rule, building on #204) omits external_url and hashes external_id; the stored
-- row keeps both verbatim for local reads.
--
--   uid               SourceRef ULID identity (sourceRefs/{uid} resource name)
--   subject_uid       the record this source was derived from (PROV subject)
--   source_type       open taxonomy string e.g. email|slack_message|web_page
--   connector/stream  connector + sub-channel that produced the reference
--   external_id       source-system opaque id (hashed in plaintext render)
--   external_url      dereferenceable location (omitted in plaintext render)
--   content_hash      appender-supplied SHA-256 hex of the fetched content
--   fetch_time        RFC3339Nano instant the content was fetched (AIP-142)
--   dedup_key         idempotency key (UNIQUE upsert target)
--   source_event_uid  the carrying sourceRefRecorded event UID
--   create_time       RFC3339Nano event create time
--   scope             personal|project|team (#206 read isolation)

CREATE TABLE source_refs (
    uid              TEXT PRIMARY KEY,
    subject_uid      TEXT NOT NULL,
    source_type      TEXT NOT NULL,
    connector        TEXT,
    stream           TEXT,
    external_id      TEXT,
    external_url     TEXT,
    content_hash     TEXT,
    fetch_time       TEXT NOT NULL,
    dedup_key        TEXT NOT NULL,
    source_event_uid TEXT NOT NULL,
    create_time      TEXT NOT NULL,
    scope            TEXT NOT NULL DEFAULT 'personal',
    UNIQUE(dedup_key)
);

CREATE INDEX idx_source_refs_subject_uid ON source_refs (subject_uid);
CREATE INDEX idx_source_refs_scope       ON source_refs (scope);
