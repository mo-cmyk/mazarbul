package daemon

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// TestSyncSupervisorSealMax_PlaintextRoutesToPlaintextDefault verifies
// the plaintext/encrypted split: when the per-scope key is nil, sealMax returns the
// plaintext default (2_000) so .jsonl chunks stay under github.com's
// ~1 MiB web-UI render ceiling.
func TestSyncSupervisorSealMax_PlaintextRoutesToPlaintextDefault(t *testing.T) {
	t.Parallel()
	s := &syncSupervisor{cfg: &syncSupervisorConfig{}}
	if got := s.sealMax(nil); got != defaultSealMaxPlaintext {
		t.Errorf("sealMax(nil) = %d, want %d", got, defaultSealMaxPlaintext)
	}
}

// TestSyncSupervisorSealMax_EncryptedRoutesToEncryptedDefault: a
// non-nil scope key picks the larger 10_000 default — gzip
// compression makes the larger batch land in the 0.5–2 MiB range
// after sealing.
func TestSyncSupervisorSealMax_EncryptedRoutesToEncryptedDefault(t *testing.T) {
	t.Parallel()
	s := &syncSupervisor{cfg: &syncSupervisorConfig{}}
	if got := s.sealMax(make([]byte, 32)); got != defaultSealMax {
		t.Errorf("sealMax(key) = %d, want %d", got, defaultSealMax)
	}
}

// TestSyncSupervisorSealMax_OperatorOverrideWinsOverBothDefaults
// asserts an explicit config value pre-empts the default for the
// matching mode without leaking to the other mode.
func TestSyncSupervisorSealMax_OperatorOverrideWinsOverBothDefaults(t *testing.T) {
	t.Parallel()
	s := &syncSupervisor{cfg: &syncSupervisorConfig{
		Sync: config.SyncTable{
			SealMaxEvents:          7777,
			SealMaxEventsPlaintext: 333,
		},
	}}
	if got := s.sealMax(nil); got != 333 {
		t.Errorf("plaintext override = %d, want 333", got)
	}
	if got := s.sealMax(make([]byte, 32)); got != 7777 {
		t.Errorf("encrypted override = %d, want 7777", got)
	}
}
