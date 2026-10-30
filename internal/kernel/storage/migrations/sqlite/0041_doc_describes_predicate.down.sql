-- Down migration 0041: remove the describes predicate registration.
--
-- This file is NOT embedded by the forward-only migration runner (it embeds
-- only *.up.sql). It exists as a manual recovery reference for development
-- databases (same convention as 0033_relation_retractions_signer.down.sql);
-- production databases are rebuilt from the JSONL event log via `maz rebuild`
-- rather than rolled back.

DELETE FROM predicates WHERE predicate = 'describes';
