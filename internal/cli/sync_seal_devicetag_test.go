package cli

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/identity"
)

// TestResolveSealDeviceTag verifies the CLI pre-push seal resolves THIS
// device's real identity id (so chunk Header.Device matches the authoring
// device recorded in every event), and returns "" — signalling "skip the
// pre-push seal" — when no identity is established yet. A wrong tag would make
// the sealer's own-device filter drop every real event, or a consumer reject
// the chunk at AuthorsMatchDevice.
func TestResolveSealDeviceTag(t *testing.T) {
	ctx := context.Background()

	t.Run("returns persisted device id", func(t *testing.T) {
		fake := installFakeSecretStore(t)
		fake.data[identity.KeychainDeviceID] = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		if got := resolveSealDeviceTag(ctx); got != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
			t.Fatalf("resolveSealDeviceTag = %q, want the persisted device id", got)
		}
	})

	t.Run("returns empty when no identity", func(t *testing.T) {
		installFakeSecretStore(t) // empty store: KeychainDeviceID absent
		if got := resolveSealDeviceTag(ctx); got != "" {
			t.Fatalf("resolveSealDeviceTag = %q, want empty (no identity → skip seal)", got)
		}
	})

	t.Run("never mints a device id", func(t *testing.T) {
		fake := installFakeSecretStore(t)
		_ = resolveSealDeviceTag(ctx)
		if _, ok := fake.data[identity.KeychainDeviceID]; ok {
			t.Fatal("resolveSealDeviceTag minted a device id; it must be read-only")
		}
	})
}
