-- Down migration 0038: drop the insights projection.
DROP TABLE IF EXISTS insights_vec;
DROP TABLE IF EXISTS insights_vec_ids;
DROP TABLE IF EXISTS insights;
