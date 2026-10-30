-- Migration 0042: canonical opt-in for document name resolution (guardrail G5).
--
-- A document whose frontmatter sets `canonical: true` claims ownership of its
-- display name and wins same-name resolution over non-document entities of the
-- same name. This prevents a later connector-imported entity from silently
-- stealing a wikilink resolution that the document author intended to own.
--
-- Non-canonical documents (canonical:false or absent) keep the existing default:
-- a non-document entity of the same name wins resolution (ShadowedDocument).
--
-- This column is projected by the dispatcher from the documentRevision event's
-- frontmatter_json payload: a `canonical: true` value stores 1; all other cases
-- (false, absent, or non-boolean) store 0. DEFAULT 0 back-fills every pre-0042
-- row as non-canonical, which matches the intended "opt-in" semantics.

ALTER TABLE documents ADD COLUMN canonical INTEGER NOT NULL DEFAULT 0;

-- Partial index covering only canonical=1 rows. The name-resolution LEFT JOIN
-- accesses documents by document_uid (the PRIMARY KEY, already indexed), so
-- this index supports direct queries on canonical documents without enlarging
-- the default-0 majority.
CREATE INDEX documents_canonical ON documents (document_uid) WHERE canonical = 1;
