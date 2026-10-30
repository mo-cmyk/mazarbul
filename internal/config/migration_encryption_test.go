package config_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// TestMigrateSyncEncryptionDefault covers the encrypted-default →
// plaintext-default bridge.
// Each case pins one operator state from the docs (existing user,
// fresh user, partial-migration retry, explicit-plaintext user)
// against the expected outcome: a flip-to-encrypted ONLY for the
// exact existing-user case.
func TestMigrateSyncEncryptionDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		resolved     config.SyncTable
		state        config.UserState
		hasWatchword bool
		wantChanged  bool
		// post-migration assertion: pointer-bool value (or nil if
		// no override was set / pointer remains absent).
		wantEncrypted map[string]*bool
	}{
		{
			name: "encrypted-default user with Watchword and remote gets encryption pinned",
			resolved: config.SyncTable{
				Remotes: map[string]config.ScopeRemoteTable{
					"personal": {URL: "git@github.com:me/maz-personal.git"},
				},
			},
			state:         config.UserState{},
			hasWatchword:  true,
			wantChanged:   true,
			wantEncrypted: map[string]*bool{"personal": boolPtr(true)},
		},
		{
			name: "fresh plaintext-default user with no Watchword stays plaintext",
			resolved: config.SyncTable{
				Remotes: map[string]config.ScopeRemoteTable{
					"personal": {URL: "git@github.com:me/maz-personal.git"},
				},
			},
			state:         config.UserState{},
			hasWatchword:  false,
			wantChanged:   false,
			wantEncrypted: map[string]*bool{"personal": nil},
		},
		{
			name: "operator explicit plaintext is honoured even with Watchword",
			resolved: config.SyncTable{
				Remotes: map[string]config.ScopeRemoteTable{
					"personal": {
						URL:       "git@github.com:me/maz-personal.git",
						Encrypted: boolPtr(false),
					},
				},
			},
			state:         config.UserState{},
			hasWatchword:  true,
			wantChanged:   false,
			wantEncrypted: map[string]*bool{"personal": nil},
		},
		{
			name: "operator explicit encrypted is a no-op (already set)",
			resolved: config.SyncTable{
				Remotes: map[string]config.ScopeRemoteTable{
					"personal": {
						URL:       "git@github.com:me/maz-personal.git",
						Encrypted: boolPtr(true),
					},
				},
			},
			state:         config.UserState{},
			hasWatchword:  true,
			wantChanged:   false,
			wantEncrypted: map[string]*bool{"personal": nil},
		},
		{
			name: "scope with no URL is skipped",
			resolved: config.SyncTable{
				Remotes: map[string]config.ScopeRemoteTable{
					"personal": {URL: ""},
				},
			},
			state:         config.UserState{},
			hasWatchword:  true,
			wantChanged:   false,
			wantEncrypted: map[string]*bool{"personal": nil},
		},
		{
			name: "second run is idempotent (state already encrypted)",
			resolved: config.SyncTable{
				Remotes: map[string]config.ScopeRemoteTable{
					"personal": {URL: "git@github.com:me/maz-personal.git"},
				},
			},
			state: config.UserState{
				Sync: config.SyncStateTable{
					Remotes: map[string]config.ScopeRemoteStateTable{
						"personal": {Encrypted: boolPtr(true)},
					},
				},
			},
			hasWatchword:  true,
			wantChanged:   false,
			wantEncrypted: map[string]*bool{"personal": boolPtr(true)},
		},
		{
			name: "mixed: project plaintext (no remote), personal encrypted (remote)",
			resolved: config.SyncTable{
				Remotes: map[string]config.ScopeRemoteTable{
					"personal": {URL: "git@github.com:me/maz-personal.git"},
				},
			},
			state:         config.UserState{},
			hasWatchword:  true,
			wantChanged:   true,
			wantEncrypted: map[string]*bool{"personal": boolPtr(true)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := tc.state
			got := config.MigrateSyncEncryptionDefault(tc.resolved, &st, tc.hasWatchword)
			if got != tc.wantChanged {
				t.Errorf("MigrateSyncEncryptionDefault changed = %v, want %v", got, tc.wantChanged)
			}
			for scope, want := range tc.wantEncrypted {
				if st.Sync.Remotes == nil {
					if want != nil {
						t.Errorf("scope %s: state has no remotes map, want Encrypted=%v", scope, *want)
					}
					continue
				}
				gotEnc := st.Sync.Remotes[scope].Encrypted
				switch {
				case want == nil && gotEnc != nil:
					t.Errorf("scope %s: Encrypted = &%v, want nil", scope, *gotEnc)
				case want != nil && gotEnc == nil:
					t.Errorf("scope %s: Encrypted = nil, want &%v", scope, *want)
				case want != nil && gotEnc != nil && *want != *gotEnc:
					t.Errorf("scope %s: Encrypted = &%v, want &%v", scope, *gotEnc, *want)
				}
			}
		})
	}
}

// TestMigrateSyncEncryptionDefault_NilStateIsSafe documents the
// receiver contract: a nil pointer is a no-op rather than a panic.
// Defends against future callers wiring boot phases out of order.
func TestMigrateSyncEncryptionDefault_NilStateIsSafe(t *testing.T) {
	t.Parallel()
	resolved := config.SyncTable{
		Remotes: map[string]config.ScopeRemoteTable{
			"personal": {URL: "x"},
		},
	}
	if changed := config.MigrateSyncEncryptionDefault(resolved, nil, true); changed {
		t.Errorf("MigrateSyncEncryptionDefault(nil) returned changed=true, want false")
	}
}
