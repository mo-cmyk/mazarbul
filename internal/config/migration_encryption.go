package config

// migration_encryption.go — the encrypted-default → plaintext-default bridge.
//
// Originally every sync chunk was encrypted by default; the
// ScopeRemoteTable had no opt-out. The default later flipped: a
// fresh install gets plaintext JSONL chunks unless the operator
// explicitly opts in to encryption. Without a migration, a pre-flip
// user who simply upgrades the binary would discover on the next
// sealer tick that their chunks went from `.maz` (ciphertext) to
// `.jsonl` (events readable on GitHub) — an unannounced privacy
// regression.
//
// MigrateSyncEncryptionDefault prevents that. It runs once at
// daemon boot before the supervisor spins up: for every scope that
// has a remote configured AND the operator has a Watchword in the
// keychain AND no explicit encryption preference yet, it pins
// Encrypted=true into state.toml. Subsequent boots see the
// preference already set and skip — the migration is idempotent.
//
// New users (no Watchword, no remotes) and users who arrived
// after the flip get the plaintext default unchanged.

// MigrateSyncEncryptionDefault writes Encrypted=true into state for
// every scope that meets all three conditions:
//
//  1. a remote URL is configured (in resolved, the post-merge view)
//  2. resolved.Remotes[scope].Encrypted is nil (no explicit setting
//     in either config.toml or state.toml today)
//  3. the operator has a Watchword in the keychain (so encryption is
//     actually usable — flipping a scope to encrypted without a key
//     would just disable that scope)
//
// resolved is the post-merge SyncTable (what the daemon would see
// if it consulted UserConfig.Sync.Remotes today). State is the
// on-disk overlay; the function mutates state.Sync.Remotes in
// place. Returns true when a mutation occurred so the caller can
// persist state.toml; false when no work was needed.
func MigrateSyncEncryptionDefault(resolved SyncTable, state *UserState, hasWatchword bool) bool {
	if state == nil || !hasWatchword {
		return false
	}
	changed := false
	for scope, rr := range resolved.Remotes {
		if rr.URL == "" {
			continue
		}
		if rr.Encrypted != nil {
			continue
		}
		if state.Sync.Remotes == nil {
			state.Sync.Remotes = map[string]ScopeRemoteStateTable{}
		}
		sr := state.Sync.Remotes[scope]
		if sr.Encrypted != nil {
			// state.toml already has an explicit value — even if false,
			// honour the operator's choice (they may have just toggled
			// plaintext via Settings; the resolved view inherits it).
			continue
		}
		v := true
		sr.Encrypted = &v
		state.Sync.Remotes[scope] = sr
		changed = true
	}
	return changed
}
