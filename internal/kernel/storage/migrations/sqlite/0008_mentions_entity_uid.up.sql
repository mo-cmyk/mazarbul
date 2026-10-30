-- 0008_mentions_entity_uid: link mentions to their resolved entity by UID.
--
-- Before this migration, recall joined mentions to entities on
-- mentions.display_name = entities.display_name. That collapses homonyms:
-- two distinct people both named "Frodo" (different entity UIDs) share
-- the surface form and produce the same join key, so recall surfaces the
-- wrong entity. The four join sites in query_hybrid.go are:
--
--   - entityUIDsForEpisodesBatch     (find entity UIDs cited by an episode set)
--   - episodesForEntitiesBatch       (find episodes citing an entity set)
--   - episodesCitingEntitiesBatch    (spatial: episodes citing nearby entities)
--   - nameMatchEpisodes              (spatial fallback by display_name LIKE)
--
-- All four switch to JOIN ON m.entity_uid = e.uid in the same change that
-- adds this column. The dispatcher's applyEntity is updated to write the
-- entity UID back to each mention listed in entity.MentionUIDs so the column
-- is populated within the same transaction as the entity row.
--
-- Backfill: NULL for rows written before this migration; the event log is
-- the source of truth and `maz rebuild` re-applies every EntityCreated/
-- Updated event in order, populating the column. We deliberately do not run
-- a UPDATE...JOIN backfill in this migration because the only available
-- join key is display_name, which is precisely the collapse this fix
-- exists to remove.

ALTER TABLE mentions ADD COLUMN entity_uid TEXT;

CREATE INDEX idx_mentions_entity_uid ON mentions(entity_uid);
