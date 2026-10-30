package cli

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/identity"
)

// canonicalMnemonic builds the all-zeros BIP-39 24-word mnemonic
// programmatically so the source rewriter cannot collapse the
// repeated token. Same shape as the chunk + watchword tests.
func canonicalMnemonic() string {
	parts := make([]string, 24)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[len(parts)-1] = "art"
	return strings.Join(parts, " ")
}

func TestRunMellonCode_RoundtripsThroughRunMellonPair(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == darwinGOOS {
		t.Skip("macOS race detector collides with mellon's Argon2id 64MiB working set; Linux CI runs this under race")
	}
	// Emit a code+bundle, parse the printed lines, feed them back to
	// runMellonPair, and verify the pair output names the original
	// device-id. The whole CLI surface exercised end-to-end without
	// shelling out.
	var emit bytes.Buffer
	if err := runMellonCode(context.Background(), &emit,
		canonicalMnemonic(), "device-b",
		"git@github.com:x/personal.git", "", "", "fixture"); err != nil {
		t.Fatalf("runMellonCode: %v", err)
	}
	code, bundle := parseEmitted(t, emit.String())

	var out bytes.Buffer
	if err := runMellonPair(context.Background(),
		strings.NewReader(""), &out, "", code, bundle, true); err != nil {
		t.Fatalf("runMellonPair: %v", err)
	}
	if !strings.Contains(out.String(), "device-b") {
		t.Errorf("pair output missing device-id, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "git@github.com:x/personal.git") {
		t.Errorf("pair output missing personal remote, got:\n%s", out.String())
	}
	// Greptile finding #3: the output must contain the recovered
	// Watchword text, not a literal placeholder. Without this the
	// receiving operator cannot run `maz init --restore` because
	// they never see the secret the bundle was supposed to ferry.
	if !strings.Contains(out.String(), canonicalMnemonic()) {
		t.Errorf("pair output missing recovered Watchword, got:\n%s", out.String())
	}
}

func TestRunMellonCode_RejectsMissingWatchword(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runMellonCode(context.Background(), &out, "", "device-b", "", "", "", "")
	if err == nil {
		t.Fatalf("runMellonCode must reject empty watchword")
	}
}

func TestRunMellonCode_RejectsMissingDeviceID(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runMellonCode(context.Background(), &out, canonicalMnemonic(), "", "", "", "", "")
	if err == nil {
		t.Fatalf("runMellonCode must reject empty device-id")
	}
}

func TestRunMellonPair_PromptsForCodeAndBundleWhenFlagsEmpty(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == darwinGOOS {
		t.Skip("macOS race detector collides with mellon's Argon2id 64MiB working set; Linux CI runs this under race")
	}
	// Emit one bundle so we have a valid (code, wire) pair to feed in.
	var emit bytes.Buffer
	if err := runMellonCode(context.Background(), &emit,
		canonicalMnemonic(), "device-b", "", "", "", ""); err != nil {
		t.Fatalf("runMellonCode: %v", err)
	}
	code, bundle := parseEmitted(t, emit.String())

	// Feed the prompts via the input reader.
	stdin := strings.NewReader(code + "\n" + bundle + "\n")
	var out bytes.Buffer
	if err := runMellonPair(context.Background(), stdin, &out, "", "", "", true); err != nil {
		t.Fatalf("runMellonPair: %v", err)
	}
	if !strings.Contains(out.String(), "device-b") {
		t.Errorf("pair-via-prompt output missing device-id, got:\n%s", out.String())
	}
}

// parseEmitted pulls the printed Code: / Bundle: values out of the
// `maz mellon code` output. The format is line-stable so a simple
// prefix scan works.
func parseEmitted(t *testing.T, output string) (code, bundle string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "Code:"):
			code = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Code:"))
		case strings.HasPrefix(strings.TrimSpace(line), "Bundle:"):
			bundle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Bundle:"))
		}
	}
	if code == "" || bundle == "" {
		t.Fatalf("could not parse code/bundle from output:\n%s", output)
	}
	return code, bundle
}

// TestProvisionPairedDevice covers the pairing identity handoff: a virgin
// device adopts the bundle device id and mints a signing subkey; an
// established device (self binding present) keeps its active key untouched.
func TestProvisionPairedDevice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("virgin device adopts id and mints subkey", func(t *testing.T) {
		t.Parallel()
		chain := newFakeSecrets()
		if err := provisionPairedDevice(ctx, chain, "", "01BUNDLEDEV000000000000001"); err != nil {
			t.Fatalf("provisionPairedDevice: %v", err)
		}
		idStore := identity.New(chain)
		devID, err := idStore.DeviceID(ctx)
		if err != nil {
			t.Fatalf("DeviceID: %v", err)
		}
		if devID != "01BUNDLEDEV000000000000001" {
			t.Errorf("device id = %q, want the bundle id", devID)
		}
		has, err := idStore.HasDeviceSubkey(ctx)
		if err != nil {
			t.Fatalf("HasDeviceSubkey: %v", err)
		}
		if !has {
			t.Error("expected a minted device subkey on a virgin paired device")
		}
	})

	t.Run("established device keeps its active key", func(t *testing.T) {
		t.Parallel()
		chain := newFakeSecrets()
		if err := provisionPairedDevice(ctx, chain, "01PRINCIPALBOUND000000001", "01BUNDLEDEV000000000000002"); err != nil {
			t.Fatalf("provisionPairedDevice: %v", err)
		}
		idStore := identity.New(chain)
		has, err := idStore.HasDeviceSubkey(ctx)
		if err != nil {
			t.Fatalf("HasDeviceSubkey: %v", err)
		}
		if has {
			t.Error("an established device must not silently switch to an unenrolled subkey")
		}
	})
}
