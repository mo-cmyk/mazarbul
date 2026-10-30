package dispatcher_test

// genesis_anchor_test.go — GenesisAnchorKey resolves the fold genesis
// principal's enrolled signing key, backing the consume loop's TEAM.json
// verification. Pinned here: the key resolves only when BOTH the genesis
// self-grant and the device enrollment are projected, and only for the
// genesis principal's own key id.

import (
	"bytes"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func TestGenesisAnchorKey(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	pub, pubB64 := identityTestKey(t)

	// Before any membership state exists the anchor key must not resolve.
	if _, _, ok, err := d.GenesisAnchorKey(ctx, testSigKeyID); err != nil || ok {
		t.Fatalf("GenesisAnchorKey on empty projections = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	// Seed the fold genesis (self-grant) and the genesis device enrollment.
	events := []kernel.Event{
		grantEvent(t, "01EVGRANTGENESIS0000000001", testPrincipalUID, testPrincipalUID, 0),
		deviceEvent(t, pubB64),
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	uid, gotPub, ok, err := d.GenesisAnchorKey(ctx, testSigKeyID)
	if err != nil {
		t.Fatalf("GenesisAnchorKey: %v", err)
	}
	if !ok || uid != testPrincipalUID || !bytes.Equal(gotPub, pub) {
		t.Errorf("GenesisAnchorKey = (uid=%q, ok=%v, keyMatch=%v), want (%q, true, true)",
			uid, ok, bytes.Equal(gotPub, pub), testPrincipalUID)
	}

	// A key id the genesis principal never enrolled must not resolve.
	if _, _, ok, err := d.GenesisAnchorKey(ctx, "kid:never-enrolled"); err != nil || ok {
		t.Errorf("GenesisAnchorKey(unknown kid) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	// An empty key id must not resolve.
	if _, _, ok, err := d.GenesisAnchorKey(ctx, ""); err != nil || ok {
		t.Errorf("GenesisAnchorKey(empty kid) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}
