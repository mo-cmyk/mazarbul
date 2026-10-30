package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// staleRebuildAge is how old an online-rebuild shadow directory must be before
// the boot hygiene pass removes it. An online rebuild completes in
// seconds-to-minutes, so anything older is an orphan from a crashed build; the
// margin guards against removing a dir from an in-flight rebuild.
const staleRebuildAge = time.Hour

// cleanStaleRebuildArtifacts removes orphaned online-rebuild shadow directories
// (.rebuild-<ulid>) left by a crashed shadow build, and warns when a commit
// sentinel (.rebuild-commit) indicates a swap was interrupted mid-rename — in
// which case the SQLite and graph projections may be out of sync. The JSONL is
// the source of truth and is never touched here; `maz rebuild` repairs the
// projection (and clears the sentinel on success). Called from OpenWiring on
// every boot and serving-stack reload.
func cleanStaleRebuildArtifacts(dataDir string, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}

	if _, err := os.Stat(filepath.Join(dataDir, rebuildCommitSentinel)); err == nil {
		logger.Warn("daemon: interrupted rebuild detected — SQLite and graph projections may be out of sync; run `maz rebuild` to repair (the event log is intact)",
			zap.String("dataDir", dataDir))
	}

	matches, err := filepath.Glob(filepath.Join(dataDir, rebuildShadowPrefix+"*"))
	if err != nil {
		logger.Warn("daemon: glob stale rebuild artifacts", zap.Error(err))
		return
	}
	for _, p := range matches {
		if filepath.Base(p) == rebuildCommitSentinel {
			continue // keep the sentinel until a successful rebuild clears it
		}
		info, serr := os.Stat(p)
		if serr != nil || !info.IsDir() || time.Since(info.ModTime()) < staleRebuildAge {
			continue // missing, not a shadow dir, or possibly in-flight
		}
		if rerr := os.RemoveAll(p); rerr != nil {
			logger.Warn("daemon: remove stale rebuild shadow dir", zap.String("dir", p), zap.Error(rerr))
			continue
		}
		logger.Info("daemon: removed stale rebuild shadow dir", zap.String("dir", p))
	}
}

// spotlightMarker is the Apple-documented opt-out filename: any
// directory containing this file is skipped by Spotlight indexing.
// The file is empty; its presence is the signal.
const spotlightMarker = ".metadata_never_index"

// ApplyDataDirHygiene marks the data directory so macOS background
// indexers leave it alone. Two markers, both idempotent:
//
//  1. <dataDir>/.metadata_never_index, the documented Spotlight
//     opt-out file. Spotlight skips the entire subtree on any
//     macOS version that respects the convention.
//  2. com.apple.metadata:com_apple_backup_excludeItem xattr on
//     <dataDir>, the documented Time Machine opt-out. tmutil
//     addexclusion writes the same flag; presence is the signal.
//
// The function exists because macOS Tahoe (26.x) regressed both
// subsystems on Apple Silicon: corespotlightd can enter a 100-200%
// CPU loop indexing rapidly-changing SQLite WAL files, and Time
// Machine 26.2-26.3 freezes the system when backing up paths that
// are being actively written. The daemon's long-lived process holds
// hot WAL handles in the data dir, so without these markers the
// kernel can hang within seconds of `maz daemon start` on Tahoe.
//
// Cross-platform behaviour:
//   - Darwin: both markers are applied.
//   - Other platforms: only the .metadata_never_index marker is
//     written. It is harmless everywhere and keeps the API uniform
//     for tests that exercise OpenWiring on Linux CI runners.
//
// Returns an error only when dataDir does not exist or the marker
// file cannot be created. Xattr failures are warn-logged and
// swallowed: the daemon must still start in environments where the
// filesystem rejects extended attributes (network mounts, sandbox).
func ApplyDataDirHygiene(dataDir string, logger *zap.Logger) error {
	if dataDir == "" {
		return errors.New("daemon: ApplyDataDirHygiene: empty dataDir")
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	info, err := os.Stat(dataDir)
	if err != nil {
		return fmt.Errorf("daemon: hygiene: stat %s: %w", dataDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("daemon: hygiene: %s is not a directory", dataDir)
	}

	marker := filepath.Join(dataDir, spotlightMarker)
	if _, err := os.Stat(marker); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("daemon: hygiene: stat marker: %w", err)
		}
		// Spotlight's mds_stores runs as the _mds_stores UID, not
		// the current user. The opt-out marker MUST be world-
		// readable or the indexer cannot stat it and the directory
		// is indexed normally.
		if werr := os.WriteFile(marker, nil, 0o644); werr != nil { //nolint:gosec // Spotlight requires world-readable marker; private contents (empty file) carry no secrets
			return fmt.Errorf("daemon: hygiene: write marker: %w", werr)
		}
		logger.Info("daemon: data dir Spotlight exclusion applied",
			zap.String("dataDir", dataDir),
			zap.String("marker", marker),
		)
	}

	applyTMExclusionAndLog(dataDir, logger)
	return nil
}

// applyTMExclusionAndLog runs the Time Machine xattr write and logs
// the (best-effort) outcome. Extracted so ApplyDataDirHygiene stays
// linear and the OpenWiring call site does not have to know about
// the Darwin-vs-other split.
func applyTMExclusionAndLog(dataDir string, logger *zap.Logger) {
	if xerr := applyTimeMachineExclusion(dataDir); xerr != nil {
		// Best-effort: xattr support is filesystem-dependent (APFS
		// honours it, some network/sandboxed mounts reject it). A
		// failure here weakens the Tahoe protection but must not
		// keep the daemon from starting.
		logger.Warn("daemon: Time Machine exclusion xattr failed; daemon will still start",
			zap.String("dataDir", dataDir),
			zap.Error(xerr),
		)
	}
}

// applyDataDirHygieneOrWarn calls ApplyDataDirHygiene and logs at
// Warn on failure. Wrapping it here keeps OpenWiring's cyclomatic
// complexity under the project's gocyclo threshold without leaking
// the marker-vs-xattr split into the wiring file.
func applyDataDirHygieneOrWarn(dataDir string, logger *zap.Logger) {
	if err := ApplyDataDirHygiene(dataDir, logger); err != nil {
		logger.Warn("daemon: data dir hygiene failed; daemon will still start",
			zap.String("dataDir", dataDir),
			zap.Error(err),
		)
	}
}
