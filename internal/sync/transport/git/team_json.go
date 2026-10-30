package git

// team_json.go — TEAM.json copy-up for the team-scope repo root.
//
// TEAM.json is the tamper-evident genesis trust anchor published at the repo
// root of every team-scope sync repository. It records the genesis principal
// UID + genesis signing-key fingerprint so every device that pulls the repo
// can pin the same genesis without trusting the event log's ULID ordering.
//
// Signing guarantee
//
//	content = GenesisPinBytes(principalUID, sigKeyID)
//	signature = ed25519.Sign(genesis_signing_key, content)
//
// A joining device verifies the signature against the genesis device's signing
// public key (recovered from the team membership fold, which is itself
// bootstrapped from recipients.jsonl).
//
// Staging protocol (daemon-side, not in this file)
//
//	1. At team CREATE the daemon stages
//	   <dataDir>/sync/team/TEAM.json immediately after writing the genesis grant.
//	2. On JOIN the daemon reads TEAM.json from the repo and persists the pin to
//	   state.toml; it never writes TEAM.json itself (that is genesis-only).
//	3. On PUSH the transport copies <dataDir>/sync/team/TEAM.json → <repoDir>/TEAM.json
//	   (this file). If the staged file is absent the push is a no-op for TEAM.json
//	   — non-genesis devices skip the step.
//	4. On PULL the transport exposes ReadTeamJSON so the daemon can verify and
//	   import the pin.
//
// Tamper evidence
//
//	reconcileTeamJSON never overwrites a TEAM.json that is already present in
//	the repo with DIFFERENT content — a diverging file triggers an error that
//	halts the push rather than silently replacing the genesis anchor. This gives
//	each team member a local tripwire against an adversary with repo-write access
//	who tries to swap the genesis file.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// TeamJSONFilename is the canonical repo-root file name for the genesis pin.
const TeamJSONFilename = "TEAM.json"

// TeamJSON is the on-disk representation of the genesis trust anchor at the
// repo root. The Signature field holds the genesis device's ed25519 signature
// over GenesisPinBytes(PrincipalUID, SigKeyID), encoded as standard base64.
type TeamJSON struct {
	// PrincipalUID is the genesis principal's ULID.
	PrincipalUID string `json:"principalUid"`
	// SigKeyID is the fingerprint ("kid:…") of the genesis signing key.
	SigKeyID string `json:"sigKeyId"`
	// Signature is base64(ed25519.Sign(genesis_priv, GenesisPinBytes(UID, KID))).
	// Verifiable with the genesis signing public key from the membership fold.
	Signature string `json:"signature"`
}

// StageTeamJSON writes a signed genesis trust anchor to
// <dataDir>/sync/team/TEAM.json. It is called by the genesis device
// immediately after team CREATE succeeds, using signPrivKey (the genesis
// device's ed25519 private key) to self-sign the pin so that every peer
// can verify the file was produced by the holder of the genesis signing key.
//
// The staging directory is created on demand. The write is atomic (temp
// file + rename) so a crash mid-write never leaves a partially-written
// TEAM.json that a subsequent reconcileTeamJSON would refuse to replace.
func StageTeamJSON(dataDir, principalUID, sigKeyID string, signPrivKey ed25519.PrivateKey) error {
	msg := genesisPinBytes(principalUID, sigKeyID)
	sig := ed25519.Sign(signPrivKey, msg)
	tj := TeamJSON{
		PrincipalUID: principalUID,
		SigKeyID:     sigKeyID,
		Signature:    base64.StdEncoding.EncodeToString(sig),
	}
	raw, err := json.Marshal(tj)
	if err != nil {
		return fmt.Errorf("git transport: marshal TEAM.json: %w", err)
	}
	dir := filepath.Join(dataDir, "sync", "team")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("git transport: create sync/team dir: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, TeamJSONFilename), raw)
}

// ReadTeamJSON reads and JSON-parses the TEAM.json at repoDir. Returns
// (zero, false, nil) when the file is absent — an older remote without a
// genesis anchor is not an error. Returns an error on parse failure.
func ReadTeamJSON(repoDir string) (TeamJSON, bool, error) {
	path := filepath.Join(repoDir, TeamJSONFilename)
	data, err := os.ReadFile(path) //nolint:gosec // repoDir comes from validated Transport paths
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TeamJSON{}, false, nil
		}
		return TeamJSON{}, false, fmt.Errorf("git transport: read TEAM.json: %w", err)
	}
	var tj TeamJSON
	if err := json.Unmarshal(data, &tj); err != nil {
		return TeamJSON{}, false, fmt.Errorf("git transport: parse TEAM.json: %w", err)
	}
	return tj, true, nil
}

// VerifyTeamJSON checks the genesis self-signature in tj against the genesis
// device's raw ed25519 signing public key (32 bytes). The canonical bytes
// signed by the genesis device are produced by the ontology package's
// GenesisPinBytes function; callers may import that directly or use the local
// equivalent here to avoid an import cycle.
func VerifyTeamJSON(tj TeamJSON, signPublicKey ed25519.PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(tj.Signature)
	if err != nil {
		return fmt.Errorf("git transport: TEAM.json signature is not valid base64: %w", err)
	}
	msg := genesisPinBytes(tj.PrincipalUID, tj.SigKeyID)
	if !ed25519.Verify(signPublicKey, msg, sig) {
		return errors.New("git transport: TEAM.json signature verification failed")
	}
	return nil
}

// genesisPinBytes returns the same canonical byte slice that the ontology
// package's GenesisPinBytes function returns. Duplicated here to avoid an
// import cycle (the git transport already imports kernel; importing ontology
// through the same path is fine, but duplication keeps the package graph clean
// and makes the format self-documenting next to the verification code).
func genesisPinBytes(principalUID, sigKeyID string) []byte {
	return []byte("mazarbul-genesis-pin-v1\x00" + principalUID + "\x00" + sigKeyID)
}

// reconcileTeamJSON copies the daemon-staged TEAM.json into the team-scope
// repo root, or removes a stale copy on a non-team-scope push. It mirrors the
// pattern used by reconcileRecipients for recipients.jsonl.
//
//   - Team-scope push: if the staged file exists, copy it to the repo root.
//     If a TEAM.json already exists there, it must have identical content —
//     diverging content means the genesis anchor was tampered with and the push
//     is aborted. If the staged file is absent (non-genesis device or not yet
//     staged), the existing repo-root TEAM.json is left untouched.
//   - Non-team-scope push: TEAM.json is a team-only artefact; remove any stale
//     copy so the transport does not leak genesis metadata into a personal or
//     project scope.
func (t *Transport) reconcileTeamJSON(isTeamScope bool, repoDir string) error {
	dest := filepath.Join(repoDir, TeamJSONFilename)
	if !isTeamScope {
		return removeStaleTeamJSON(dest)
	}
	staged := filepath.Join(t.dataDir, "sync", "team", TeamJSONFilename)
	data, err := os.ReadFile(staged) //nolint:gosec // staged path derived from dataDir + constant
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Non-genesis device: nothing staged yet — leave existing repo
			// TEAM.json (if any) untouched.
			return nil
		}
		return fmt.Errorf("git transport: read staged TEAM.json: %w", err)
	}
	// Tamper check: if the repo already has a TEAM.json it must match exactly.
	existing, readErr := os.ReadFile(dest) //nolint:gosec // dest is under repoDir
	if readErr == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("git transport: TEAM.json tamper detected: repo-root content differs from staged content; push aborted")
		}
		// Identical — no write needed.
		return nil
	}
	if !errors.Is(readErr, fs.ErrNotExist) {
		return fmt.Errorf("git transport: read existing TEAM.json: %w", readErr)
	}
	// Repo does not have TEAM.json yet; copy it in.
	if err := writeFileAtomic(dest, data); err != nil {
		return fmt.Errorf("git transport: copy TEAM.json: %w", err)
	}
	return nil
}

// removeStaleTeamJSON deletes a repo-root TEAM.json if present. A missing
// file is not an error. Used on non-team-scope pushes so genesis metadata
// never leaks into personal or project repos.
func removeStaleTeamJSON(dest string) error {
	err := os.Remove(dest)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("git transport: remove stale TEAM.json: %w", err)
}
