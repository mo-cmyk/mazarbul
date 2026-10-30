package doctor

// checks_team.go — doctor check for the genesis trust anchor (B3).
//
// team.genesis_pin probes three things in order:
//
//  1. State file: is a pin stored in state.toml?  (WARN if not)
//  2. Fold consistency: does the pinned principal UID match the
//     is_genesis=1 row in team_membership_acl?  (FAIL if mismatch)
//  3. TEAM.json signature: does the repo-root TEAM.json carry the same
//     principal UID + key-id, and does the ed25519 self-signature verify
//     against the genesis key loaded from the devices table?  (WARN if
//     file absent; FAIL if sig invalid)
//
// The check is config-independent (reads state.toml + SQLite) and MUST be
// registered before the config-missing early return in DefaultChecks.

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/mo-cmyk/mazarbul/internal/config"
	gitxport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
)

// subsystemTeam groups checks over the team membership and trust-anchor state.
const subsystemTeam = "team"

// newGenesisPinCheck returns the team.genesis_pin doctor check. It reads the
// genesis pin from state.toml, compares it against the live fold result in
// team_membership_acl, and (when TEAM.json is present in the team sync repo)
// verifies the ed25519 self-signature produced by the genesis device.
func newGenesisPinCheck(dataDir string, s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "team.genesis_pin",
		Sub: subsystemTeam,
		Fn: func(ctx context.Context) (Status, string, error) {
			return runGenesisPinCheck(ctx, dataDir, s)
		},
	}
}

// runGenesisPinCheck is the implementation body extracted so gocognit can be
// satisfied by the thin closure above.
func runGenesisPinCheck(ctx context.Context, dataDir string, s *sharedSQLite) (Status, string, error) {
	// --- Step 1: state.toml pin ---
	uid, kid, ok, err := config.LoadTeamGenesisPin(dataDir)
	if err != nil {
		return StatusFail, "", fmt.Errorf("load genesis pin from state.toml: %w", err)
	}
	if !ok {
		return StatusWarn,
			"no genesis pin stored in state.toml; fold uses TOFU lowest-ULID root; " +
				"the daemon pins automatically from a verified TEAM.json on the next team pull — " +
				"run `maz team pin` to inspect",
			nil
	}

	// --- Step 2: fold consistency ---
	store, serr := s.acquire(ctx)
	if serr != nil {
		return StatusFail, "", fmt.Errorf("open sqlite: %w", serr)
	}
	status, msg, err := checkFoldGenesis(ctx, store.Reader(), uid)
	if err != nil || status != StatusOK {
		return status, msg, err
	}

	// --- Step 3: TEAM.json signature ---
	return checkTeamJSON(ctx, store.Reader(), dataDir, uid, kid)
}

// checkFoldGenesis verifies that the pinned principal UID matches the genesis
// row in team_membership_acl. Returns StatusOK with empty message on match so
// the caller can proceed to the TEAM.json check.
func checkFoldGenesis(ctx context.Context, reader queryContexter, pinnedUID string) (Status, string, error) {
	var foldUID string
	qerr := reader.QueryRowContext(ctx,
		`SELECT principal_uid FROM team_membership_acl WHERE is_genesis = 1 LIMIT 1`,
	).Scan(&foldUID)
	switch {
	case errors.Is(qerr, sql.ErrNoRows) || foldUID == "":
		return StatusWarn,
			fmt.Sprintf("pin stored (%q) but fold has no genesis yet; team may not be created", pinnedUID),
			nil
	case qerr != nil:
		return StatusFail, "", fmt.Errorf("query fold genesis: %w", qerr)
	case foldUID != pinnedUID:
		return StatusFail,
			fmt.Sprintf("genesis pin CONFLICT: state.toml pins %q but fold resolves %q as genesis", pinnedUID, foldUID),
			nil
	}
	return StatusOK, "", nil
}

// checkTeamJSON checks the TEAM.json in the sync repo mirror (if present).
// Returns StatusWarn when the file is absent, StatusFail on content or
// signature mismatch, and StatusOK when everything is consistent and the
// ed25519 self-signature verifies.
func checkTeamJSON(ctx context.Context, reader queryContexter, dataDir, uid, kid string) (Status, string, error) {
	repoDir := filepath.Join(dataDir, "sync", "team", "repo")
	tj, found, rErr := gitxport.ReadTeamJSON(repoDir)
	if rErr != nil {
		return StatusFail, "", fmt.Errorf("read TEAM.json from sync repo: %w", rErr)
	}
	if !found {
		return StatusWarn,
			fmt.Sprintf("pin stored and fold consistent (principal %q); TEAM.json not yet published to sync repo", uid),
			nil
	}
	if tj.PrincipalUID != uid || tj.SigKeyID != kid {
		return StatusFail,
			fmt.Sprintf("TEAM.json content mismatch: file principal=%q key=%q, state principal=%q key=%q",
				tj.PrincipalUID, tj.SigKeyID, uid, kid),
			nil
	}
	return verifyTeamJSONSig(ctx, reader, tj, uid, kid)
}

// verifyTeamJSONSig loads the genesis signing public key from the devices table
// and verifies the TEAM.json self-signature. Returns StatusWarn when the key is
// not yet enrolled (fresh join), StatusFail on signature failure, StatusOK on
// a clean verify.
func verifyTeamJSONSig(ctx context.Context, reader queryContexter, tj gitxport.TeamJSON, uid, kid string) (Status, string, error) {
	var rawPub string
	kqerr := reader.QueryRowContext(ctx,
		`SELECT sign_public_key FROM devices WHERE sig_key_id = ? LIMIT 1`, kid,
	).Scan(&rawPub)
	if errors.Is(kqerr, sql.ErrNoRows) {
		return StatusWarn,
			fmt.Sprintf("pin and TEAM.json consistent; genesis signing key %q not yet enrolled locally", kid),
			nil
	}
	if kqerr != nil {
		return StatusFail, "", fmt.Errorf("query genesis signing key: %w", kqerr)
	}
	pubBytes, decErr := base64.StdEncoding.DecodeString(rawPub)
	if decErr != nil {
		return StatusFail, "", fmt.Errorf("decode genesis signing key: %w", decErr)
	}
	if verErr := gitxport.VerifyTeamJSON(tj, ed25519.PublicKey(pubBytes)); verErr != nil {
		return StatusFail, "TEAM.json self-signature invalid: " + verErr.Error(), nil //nolint:nilerr // verification failure is a check verdict, not an internal error
	}
	return StatusOK,
		fmt.Sprintf("pin present, fold consistent, TEAM.json sig valid (principal %q, key %q)", uid, kid),
		nil
}

// queryContexter is the minimal interface the genesis-pin check needs from
// the SQLite reader (satisfied by both *sql.DB and *sql.Tx).
type queryContexter interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
