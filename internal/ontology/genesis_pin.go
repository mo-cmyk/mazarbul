package ontology

// GenesisPin is the trust anchor that closes the TOFU genesis-hijack surface.
// It pins the genesis principal UID and the fingerprint of the genesis signing
// key so ReduceMembershipPinned can identify the real genesis self-grant on
// replay — even when an adversary with remote-write access injects a crafted
// self-grant with a lower ULID.
//
// A nil *GenesisPin passed to ReduceMembershipPinned means "unpinned" and
// produces behaviour identical to ReduceMembership: lowest-ULID TOFU root.
type GenesisPin struct {
	// PrincipalUID is the genesis principal's ULID. Required when the pin
	// is non-nil.
	PrincipalUID string
	// SigKeyID is the fingerprint ("kid:…") of the genesis signing key.
	// When non-empty, the fold also validates that the matching self-grant's
	// AuthorSigKeyID equals this value. An empty SigKeyID matches any key
	// (useful when the key-id was not available at pin time).
	SigKeyID string
}

// GenesisPinBytes returns the canonical bytes a genesis device signs over
// when producing the TEAM.json self-attestation. The signature lets peers
// verify the TEAM.json was produced by the holder of the genesis signing key
// and was not tampered with in transit or in the git repo.
//
// The format is domain-separated and NUL-delimited, matching the pattern
// established by DeviceEnrollmentBytes and DeviceVouchBytes so signers and
// verifiers across versions produce identical inputs.
func GenesisPinBytes(principalUID, sigKeyID string) []byte {
	return []byte("mazarbul-genesis-pin-v1\x00" + principalUID + "\x00" + sigKeyID)
}
