-- Down migration 0037: drop the memory-strength projection.
DROP TABLE IF EXISTS memory_strength;
DROP TABLE IF EXISTS memory_accesses;
