package daemon

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// envelopeRemote builds a ScopeRemoteTable opted into envelope (team) mode.
func envelopeRemote() config.ScopeRemoteTable {
	return config.ScopeRemoteTable{URL: "/tmp/maz-team.git", Encrypted: truePtr(), KeyMode: config.KeyModeEnvelope}
}

// TestSealInterval_EnvelopeScopeUsesShortInterval pins Change Set 4a: an
// envelope (team) scope seals on the short defaultPushInterval (30s) so team
// events propagate promptly, while a non-envelope scope keeps the 1h default.
// An explicit SealIntervalSeconds override still wins for every scope.
func TestSealInterval_EnvelopeScopeUsesShortInterval(t *testing.T) {
	t.Parallel()

	s := &syncSupervisor{cfg: &syncSupervisorConfig{
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"team":     envelopeRemote(),
				"personal": {URL: "/tmp/maz-personal.git"}, // plaintext default
			},
		},
	}}

	if got := s.sealInterval(kernel.ScopeTeam); got != defaultPushInterval {
		t.Errorf("team sealInterval = %s, want the short %s", got, defaultPushInterval)
	}
	if got := s.sealInterval(kernel.ScopePersonal); got != defaultSealInterval {
		t.Errorf("personal sealInterval = %s, want the default %s", got, defaultSealInterval)
	}

	// An explicit operator override pre-empts the envelope short-interval.
	override := &syncSupervisor{cfg: &syncSupervisorConfig{
		Sync: config.SyncTable{
			SealIntervalSeconds: 600,
			Remotes:             map[string]config.ScopeRemoteTable{"team": envelopeRemote()},
		},
	}}
	if got := override.sealInterval(kernel.ScopeTeam); got.Seconds() != 600 {
		t.Errorf("override sealInterval = %s, want 600s (override wins over envelope short-interval)", got)
	}
}

// TestShouldAutoPush_EnvelopeScopeAutoPublishesWithoutAutoPush pins Change Set
// 4b: an envelope (team) scope auto-publishes even when the global AutoPush
// flag is off, while a non-envelope scope only auto-pushes when AutoPush is on.
func TestShouldAutoPush_EnvelopeScopeAutoPublishesWithoutAutoPush(t *testing.T) {
	t.Parallel()

	// AutoPush off: only the envelope scope auto-publishes.
	off := &syncSupervisor{cfg: &syncSupervisorConfig{
		Sync: config.SyncTable{
			AutoPush: false,
			Remotes: map[string]config.ScopeRemoteTable{
				"team":     envelopeRemote(),
				"personal": {URL: "/tmp/maz-personal.git"},
			},
		},
	}}
	if !off.shouldAutoPush(kernel.ScopeTeam) {
		t.Error("envelope team scope must auto-publish even with AutoPush=false")
	}
	if off.shouldAutoPush(kernel.ScopePersonal) {
		t.Error("non-envelope scope must NOT auto-push with AutoPush=false")
	}

	// AutoPush on: every configured scope auto-pushes.
	on := &syncSupervisor{cfg: &syncSupervisorConfig{
		Sync: config.SyncTable{
			AutoPush: true,
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "/tmp/maz-personal.git"},
			},
		},
	}}
	if !on.shouldAutoPush(kernel.ScopePersonal) {
		t.Error("with AutoPush=true every configured scope must auto-push")
	}
}
