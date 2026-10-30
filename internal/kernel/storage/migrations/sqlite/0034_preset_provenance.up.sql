-- Migration 0034: preset provenance columns (issue #224).
--
-- Records where a remotely-installed preset came from: the source git spec, the
-- pinned commit SHA, and the canonical bundle checksum. Empty string for
-- local-directory and builtin installs (which have no remote provenance).
--
-- NOT NULL DEFAULT '' so the columns add cleanly to existing rows and a replay
-- of an older presetInstalled payload (no provenance fields) projects to '' —
-- matching the struct zero value — so `maz rebuild` stays deterministic.

ALTER TABLE presets ADD COLUMN source_url TEXT NOT NULL DEFAULT '';
ALTER TABLE presets ADD COLUMN pinned_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE presets ADD COLUMN checksum   TEXT NOT NULL DEFAULT '';
