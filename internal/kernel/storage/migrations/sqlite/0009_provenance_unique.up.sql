-- 0009_provenance_unique: narrow the provenance table to one row per event
-- so the dispatcher can use ON CONFLICT(event_uid) DO UPDATE for an
-- idempotent rebuild.
--
-- Background. The provenance table was created in migration 0001 with FKs
-- and a non-unique index on event_uid, but no code path ever wrote to it
-- (Apply rule 4 from AGENTS.md, "provenance on every fact", was documented
-- but unenforced). W1.1 of the M1-completion plan starts writing one row
-- per Episode event from the dispatcher. To make Rebuild safely replayable
-- (re-applying every event in the JSONL log must not duplicate the
-- provenance rows already present), event_uid must carry a UNIQUE
-- constraint so the writer can use ON CONFLICT DO UPDATE.
--
-- Derived records (mentions, relations) inherit provenance through their
-- source_event_uid foreign key back to the source Episode; the dispatcher
-- does not write a separate provenance row per mention or relation. If a
-- richer many-to-one model is needed in M2 (e.g. for multi-source voting)
-- we can drop this UNIQUE constraint and add a synthetic primary key in a
-- follow-up migration.

DROP INDEX IF EXISTS idx_provenance_event_uid;
CREATE UNIQUE INDEX idx_provenance_event_uid ON provenance(event_uid);
