-- Down migration 0042: drop the context pins + corrections projection.
DROP TABLE IF EXISTS context_pin_tombstones;
DROP TABLE IF EXISTS context_pins;
