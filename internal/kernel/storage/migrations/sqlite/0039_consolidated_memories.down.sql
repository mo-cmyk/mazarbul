-- Down migration 0039: drop the consolidated-memories projection.
DROP TABLE IF EXISTS consolidated_vec;
DROP TABLE IF EXISTS consolidated_vec_ids;
DROP TABLE IF EXISTS consolidated_memories;
