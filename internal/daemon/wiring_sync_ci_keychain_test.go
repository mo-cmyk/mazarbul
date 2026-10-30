package daemon

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// unavailableKeychain mimics a headless environment (CI, container, or a server
// with no secret service) where every keychain read fails with a NON-ErrNotFound
// error — e.g. the Linux "The name org.freedesktop.secrets was not provided by
// any .service files". loadWatchword only degrades on secret.ErrNotFound, so a
// chain like this would otherwise propagate and fail the entire daemon wiring.
type unavailableKeychain struct{}

func (unavailableKeychain) Get(context.Context, string) (string, error) {
	return "", errors.New(`keychain get "mazarbul:watchword": The name org.freedesktop.secrets was not provided by any .service files`)
}

func (unavailableKeychain) Set(context.Context, string, string) error {
	return errors.New("read-only")
}

func (unavailableKeychain) Delete(context.Context, string) error {
	return errors.New("read-only")
}

// TestNewSyncSupervisor_NoRemotesSkipsUnavailableKeychain is the regression
// guard for the CI wiring failure. When no sync remotes are configured — the
// default for a fresh instance and for every CI test that builds wiring — the
// supervisor must return the no-op sentinel WITHOUT reading the OS keychain, so
// a headless environment with no secret service can still OpenWiring. Before the
// fix, loadWatchword ran before the no-remotes check and a service-unavailable
// keychain error killed the whole daemon.
func TestNewSyncSupervisor_NoRemotesSkipsUnavailableKeychain(t *testing.T) {
	t.Parallel()
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:     t.TempDir(),
		DeviceID:    "test-device",
		SecretChain: unavailableKeychain{},
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("newSyncSupervisor with no remotes must not touch the keychain, got: %v", err)
	}
	if sv != nil {
		t.Errorf("expected nil (no-op) supervisor with no remotes, got %T", sv)
	}
}
