package mellon_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mellon"
)

// sampleMnemonic returns the canonical all-zeros BIP-39 vector so the
// bundle Watchword field round-trips on byte-level comparisons.
func sampleMnemonic() string {
	parts := make([]string, 24)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[len(parts)-1] = "art"
	return strings.Join(parts, " ")
}

func sampleBundle() mellon.PairBundle {
	return mellon.PairBundle{
		Watchword: sampleMnemonic(),
		RemoteURLs: map[kernel.Scope]string{
			kernel.ScopePersonal: "git@github.com:operator/mazarbul-personal.git",
		},
		DeviceID: "device-b",
		Note:     "test fixture",
	}
}

func TestGenerateCode_SixDigitDecimal(t *testing.T) {
	t.Parallel()
	for range 32 {
		c, err := mellon.GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		if len(c) != mellon.CodeWidth {
			t.Errorf("code length = %d, want %d", len(c), mellon.CodeWidth)
		}
		for _, r := range c {
			if r < '0' || r > '9' {
				t.Errorf("code %q contains non-digit %q", c, r)
			}
		}
	}
}

func TestGenerate_Pair_Roundtrip(t *testing.T) {
	t.Parallel()
	code, err := mellon.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	wire, err := mellon.Generate(code, sampleBundle())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, err := mellon.Pair(code, wire)
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if got.Watchword != sampleMnemonic() {
		t.Errorf("Watchword drift")
	}
	if got.DeviceID != "device-b" {
		t.Errorf("DeviceID drift: %q", got.DeviceID)
	}
	if got.RemoteURLs[kernel.ScopePersonal] != "git@github.com:operator/mazarbul-personal.git" {
		t.Errorf("RemoteURLs drift: %+v", got.RemoteURLs)
	}
	if got.SchemaVersion != 1 {
		t.Errorf("SchemaVersion drift: %d", got.SchemaVersion)
	}
}

func TestPair_RejectsWrongCode(t *testing.T) {
	t.Parallel()
	wire, err := mellon.Generate("123456", sampleBundle())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_, err = mellon.Pair("654321", wire)
	if err == nil {
		t.Fatalf("Pair must reject wrong code")
	}
	if !errors.Is(err, mellon.ErrWrongCode) {
		t.Errorf("err = %v, want ErrWrongCode", err)
	}
}

func TestPair_RejectsInvalidCode(t *testing.T) {
	t.Parallel()
	wire, err := mellon.Generate("123456", sampleBundle())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cases := []struct {
		name string
		code string
	}{
		{"too short", "12345"},
		{"too long", "1234567"},
		{"contains letter", "12345a"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := mellon.Pair(tc.code, wire)
			if !errors.Is(err, mellon.ErrInvalidCode) {
				t.Errorf("err = %v, want ErrInvalidCode", err)
			}
		})
	}
}

func TestPair_RejectsCorruptBundle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		wire string
	}{
		{"not base64", "!!!!"},
		{"too short", "AAAA"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := mellon.Pair("123456", tc.wire)
			if !errors.Is(err, mellon.ErrCorruptBundle) {
				t.Errorf("err = %v, want ErrCorruptBundle", err)
			}
		})
	}
}

func TestGenerate_RejectsMissingWatchword(t *testing.T) {
	t.Parallel()
	b := sampleBundle()
	b.Watchword = ""
	_, err := mellon.Generate("123456", b)
	if err == nil {
		t.Fatalf("Generate must reject empty Watchword")
	}
}

func TestGenerate_RejectsMissingDeviceID(t *testing.T) {
	t.Parallel()
	b := sampleBundle()
	b.DeviceID = ""
	_, err := mellon.Generate("123456", b)
	if err == nil {
		t.Fatalf("Generate must reject empty DeviceID")
	}
}

func TestGenerate_ProducesDifferentWireForSameInput(t *testing.T) {
	t.Parallel()
	// Two Generate calls with the same code + bundle should produce
	// distinct wire payloads because the bundle-ID prefix is random
	// per call. Pins the property that a producer cannot accidentally
	// emit identical wire (and thus reusable salt) for two pairings.
	a, err := mellon.Generate("123456", sampleBundle())
	if err != nil {
		t.Fatalf("Generate a: %v", err)
	}
	b, err := mellon.Generate("123456", sampleBundle())
	if err != nil {
		t.Fatalf("Generate b: %v", err)
	}
	if a == b {
		t.Errorf("two Generate calls returned identical wire (salt reuse)")
	}
}
