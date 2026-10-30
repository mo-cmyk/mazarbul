package watchword_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// Canonical BIP-39 reference mnemonics for 256-bit entropy. Constructed
// programmatically so source-code-rewriting tools cannot silently
// collapse the repeated tokens.
func zeroEntropyMnemonic() string {
	parts := make([]string, watchword.Words)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[len(parts)-1] = "art" // checksum for all-zeros 256-bit entropy
	return strings.Join(parts, " ")
}

func allOnesMnemonic() string {
	parts := make([]string, watchword.Words)
	for i := range parts {
		parts[i] = "zoo"
	}
	parts[len(parts)-1] = "vote" // checksum for all-0xff 256-bit entropy
	return strings.Join(parts, " ")
}

func brokenChecksumMnemonic() string {
	parts := make([]string, watchword.Words)
	for i := range parts {
		parts[i] = "abandon"
	}
	// Last word "abandon" instead of "art" — every other word matches a
	// valid mnemonic but the last 11 bits no longer satisfy the SHA-256
	// checksum, so bip39.IsMnemonicValid returns false.
	return strings.Join(parts, " ")
}

func wrongLengthMnemonic() string {
	// 12 words — valid BIP-39 length, just not the one we accept.
	parts := []string{
		"abandon", "abandon", "abandon", "abandon",
		"abandon", "abandon", "abandon", "abandon",
		"abandon", "abandon", "abandon", "about",
	}
	return strings.Join(parts, " ")
}

func nonWordlistMnemonic() string {
	parts := make([]string, watchword.Words)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[0] = "frodo" // not in the BIP-39 English wordlist
	return strings.Join(parts, " ")
}

func TestValidate_KnownMnemonics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mnemonic string
		wantErr  bool
	}{
		{name: "all-zeros 256-bit BIP-39 vector", mnemonic: zeroEntropyMnemonic(), wantErr: false},
		{name: "all-ones 256-bit BIP-39 vector", mnemonic: allOnesMnemonic(), wantErr: false},
		{name: "12 words rejected (not canonical 24)", mnemonic: wrongLengthMnemonic(), wantErr: true},
		{name: "broken checksum (24 abandons)", mnemonic: brokenChecksumMnemonic(), wantErr: true},
		{name: "non-wordlist word rejected", mnemonic: nonWordlistMnemonic(), wantErr: true},
		{name: "empty string rejected", mnemonic: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := watchword.Validate(tc.mnemonic)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate must reject %q, got nil", tc.mnemonic)
				}
				if !errors.Is(err, watchword.ErrInvalidMnemonic) {
					t.Errorf("err = %v, want errors.Is ErrInvalidMnemonic", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate must accept %q, got %v", tc.mnemonic, err)
			}
		})
	}
}

func TestGenerate_ProducesValidMnemonic(t *testing.T) {
	t.Parallel()
	for range 16 {
		got, err := watchword.Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if err := watchword.Validate(got); err != nil {
			t.Errorf("Generate produced invalid mnemonic %q: %v", got, err)
		}
		if want, have := watchword.Words, strings.Count(got, " ")+1; want != have {
			t.Errorf("Generate produced %d words, want %d", have, want)
		}
	}
}

func TestDeriveRoot_DeterministicAcrossCalls(t *testing.T) {
	t.Parallel()
	m := zeroEntropyMnemonic()
	a, err := watchword.DeriveRoot(m)
	if err != nil {
		t.Fatalf("DeriveRoot first call: %v", err)
	}
	b, err := watchword.DeriveRoot(m)
	if err != nil {
		t.Fatalf("DeriveRoot second call: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("DeriveRoot is non-deterministic: %x != %x", a, b)
	}
	if len(a) != 32 {
		t.Errorf("root key length = %d, want 32", len(a))
	}
}

func TestDeriveRoot_DifferentMnemonicsDeriveDifferentKeys(t *testing.T) {
	t.Parallel()
	a, err := watchword.DeriveRoot(zeroEntropyMnemonic())
	if err != nil {
		t.Fatalf("DeriveRoot zeros: %v", err)
	}
	b, err := watchword.DeriveRoot(allOnesMnemonic())
	if err != nil {
		t.Fatalf("DeriveRoot ones: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Errorf("Distinct mnemonics must derive distinct root keys")
	}
}

func TestDeriveRoot_RejectsInvalidMnemonic(t *testing.T) {
	t.Parallel()
	_, err := watchword.DeriveRoot("not a real mnemonic")
	if err == nil {
		t.Fatalf("DeriveRoot must reject bad input")
	}
	if !errors.Is(err, watchword.ErrInvalidMnemonic) {
		t.Errorf("err = %v, want errors.Is ErrInvalidMnemonic", err)
	}
}

func TestDeriveScopeKey_DomainSeparated(t *testing.T) {
	t.Parallel()
	root, err := watchword.DeriveRoot(zeroEntropyMnemonic())
	if err != nil {
		t.Fatalf("DeriveRoot: %v", err)
	}
	personal, err := watchword.DeriveScopeKey(root, "personal")
	if err != nil {
		t.Fatalf("DeriveScopeKey personal: %v", err)
	}
	team, err := watchword.DeriveScopeKey(root, "team")
	if err != nil {
		t.Fatalf("DeriveScopeKey team: %v", err)
	}
	if bytes.Equal(personal, team) {
		t.Errorf("scope keys must differ across scopes: %x == %x", personal, team)
	}
	if len(personal) != 32 {
		t.Errorf("scope key length = %d, want 32", len(personal))
	}

	personalAgain, err := watchword.DeriveScopeKey(root, "personal")
	if err != nil {
		t.Fatalf("DeriveScopeKey re-derive: %v", err)
	}
	if !bytes.Equal(personal, personalAgain) {
		t.Errorf("DeriveScopeKey is non-deterministic")
	}
}

func TestDeriveScopeKey_RejectsEmptyScope(t *testing.T) {
	t.Parallel()
	root := bytes.Repeat([]byte{0x01}, 32)
	_, err := watchword.DeriveScopeKey(root, "")
	if err == nil {
		t.Fatalf("DeriveScopeKey must reject empty scope")
	}
}

func TestDeriveScopeKey_RejectsWrongLengthRoot(t *testing.T) {
	t.Parallel()
	_, err := watchword.DeriveScopeKey(make([]byte, 16), "personal")
	if err == nil {
		t.Fatalf("DeriveScopeKey must reject short root key")
	}
}

func TestSealOpen_Roundtrip(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plain := []byte("the quick brown fox jumps over the lazy dog")
	sealed, err := watchword.Seal(key, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, plain) {
		t.Errorf("ciphertext leaks plaintext bytes")
	}
	opened, err := watchword.Open(key, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plain) {
		t.Errorf("Open roundtrip mismatch:\n got = %q\nwant = %q", opened, plain)
	}
}

func TestSealOpen_DifferentNoncePerCall(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	plain := []byte("same plaintext")
	a, err := watchword.Seal(key, plain)
	if err != nil {
		t.Fatalf("Seal a: %v", err)
	}
	b, err := watchword.Seal(key, plain)
	if err != nil {
		t.Fatalf("Seal b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Errorf("identical ciphertexts from identical input — nonce is not random")
	}
}

func TestOpen_FailsOnWrongKey(t *testing.T) {
	t.Parallel()
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	if _, err := rand.Read(k1); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := rand.Read(k2); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealed, err := watchword.Seal(k1, []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_, err = watchword.Open(k2, sealed)
	if err == nil {
		t.Fatalf("Open must fail with wrong key")
	}
	if !errors.Is(err, watchword.ErrAuthFailed) {
		t.Errorf("err = %v, want errors.Is ErrAuthFailed", err)
	}
}

func TestOpen_FailsOnTamperedCiphertext(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealed, err := watchword.Seal(key, []byte("authentic"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed[len(sealed)-1] ^= 0x01
	_, err = watchword.Open(key, sealed)
	if err == nil {
		t.Fatalf("Open must reject tampered ciphertext")
	}
	if !errors.Is(err, watchword.ErrAuthFailed) {
		t.Errorf("err = %v, want errors.Is ErrAuthFailed", err)
	}
}

func TestOpen_FailsOnShortCiphertext(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, err := watchword.Open(key, []byte("too short"))
	if err == nil {
		t.Fatalf("Open must reject too-short input")
	}
}

func TestSealWithAAD_Roundtrip(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plain := []byte("envelope payload")
	aad := []byte(`{"chunkId":"01","scope":"team","count":3}`)
	sealed, err := watchword.SealWithAAD(key, plain, aad)
	if err != nil {
		t.Fatalf("SealWithAAD: %v", err)
	}
	if bytes.Contains(sealed, aad) {
		t.Errorf("AAD must NOT be stored in the ciphertext (caller reproduces it on Open)")
	}
	opened, err := watchword.OpenWithAAD(key, sealed, aad)
	if err != nil {
		t.Fatalf("OpenWithAAD: %v", err)
	}
	if !bytes.Equal(opened, plain) {
		t.Errorf("roundtrip mismatch:\n got = %q\nwant = %q", opened, plain)
	}
}

func TestOpenWithAAD_FailsOnTamperedAAD(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plain := []byte("authentic")
	sealed, err := watchword.SealWithAAD(key, plain, []byte(`{"count":3}`))
	if err != nil {
		t.Fatalf("SealWithAAD: %v", err)
	}
	// Opening with AAD that differs by a single byte must fail the tag.
	_, err = watchword.OpenWithAAD(key, sealed, []byte(`{"count":4}`))
	if err == nil {
		t.Fatalf("OpenWithAAD must reject mismatched AAD")
	}
	if !errors.Is(err, watchword.ErrAuthFailed) {
		t.Errorf("err = %v, want errors.Is ErrAuthFailed", err)
	}
	// The nil-AAD form must also reject a chunk sealed with non-nil AAD.
	if _, err := watchword.Open(key, sealed); !errors.Is(err, watchword.ErrAuthFailed) {
		t.Errorf("Open(nil aad) on AAD-bound chunk: err = %v, want ErrAuthFailed", err)
	}
}

func TestSeal_DelegatesToNilAAD(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plain := []byte("legacy path")
	sealed, err := watchword.Seal(key, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Seal == SealWithAAD(…, nil): OpenWithAAD(…, nil) and Open both work.
	if got, err := watchword.OpenWithAAD(key, sealed, nil); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("OpenWithAAD(nil): got %q err %v", got, err)
	}
}
