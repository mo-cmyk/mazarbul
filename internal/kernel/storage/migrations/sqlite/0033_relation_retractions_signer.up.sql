-- Migration 0033: record the signed signer on every relation retraction
-- tombstone (#213, D3).
--
-- The convergent retraction tombstone set (#205, migration 0026) records WHAT
-- a relationRetracted closes, but not WHO signed it. Authorized signed
-- retraction (#213) needs the signer's principal to decide whether a
-- team-scope tombstone is allowed to close its target relation, and the device
-- for audit. The SIGNED authority is ev.Author (#209), so applyRelationRetracted
-- stamps both columns from the retraction event's author.
--
-- recomputeRelationClose folds ONLY the authorized tombstones for a relation:
-- a team-scope tombstone counts only when signer_principal equals the
-- relation's asserted_by_principal (migration 0032) OR the team's genesis
-- owner. Unauthorized team-scope tombstones stay RECORDED here for audit but
-- never close the relation on any device. Non-team tombstones always pass the
-- filter (personal/project retraction is unchanged from #205).
--
-- These columns make the close self-describing and auditable. The cryptographic
-- signature itself lives in the JSONL event and is verified at the team-scope
-- replay gate (#209); this is the materialized projection of that authority.
--
-- DEFAULT '' keeps the additive ALTER valid against the existing NOT NULL-free
-- table shape; a legacy pre-0033 tombstone (empty signer) folds in as
-- authorized only for non-team relations, and `maz rebuild` repopulates the
-- accurate signer from each retraction event's author.

ALTER TABLE relation_retractions ADD COLUMN signer_principal TEXT NOT NULL DEFAULT '';
ALTER TABLE relation_retractions ADD COLUMN signer_device    TEXT NOT NULL DEFAULT '';
