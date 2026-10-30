package daemon

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	gitxport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// The consume path is the pull-side counterpart of the seal/push path. For an
// envelope-encrypted team scope the synced repo carries ONLY opaque chunks +
// MANIFEST + recipients.jsonl — there is no plaintext rendered graph to import
// — so a joining device reconstructs the shared brain by:
//
//  1. pulling the remote chunks into the local FSStore (pullChunks),
//  2. bootstrapping its per-epoch DEKs from recipients.jsonl: find its own
//     rows, unwrap each wrapped DEK with its wrap private key, cache to the
//     keychain (bootstrapTeamDEKs),
//  3. decoding every local chunk under the matching epoch DEK, checking the
//     header's device claim against the events' signed authorship, and
//     grafting the events verbatim into the event log, which projects them
//     into the four-layer store via the dispatcher (decodeAndGraft → the
//     kernel's GraftSealed verifies each team signature before persistence).
//
// The loop is idempotent: GraftSealed skips UIDs already in the log, so every
// tick re-decodes every chunk cheaply and persists only genuinely new events.

// teamGrafter persists decoded team chunks verbatim into the event log. The
// kernel's *FileEventLog satisfies it via GraftSealed; a nil grafter disables
// the consume loop (test wiring with no kernel).
type teamGrafter interface {
	GraftSealed(ctx context.Context, events []kernel.Event) (kernel.GraftResult, error)
}

// teamMembershipReader answers the post-graft recipients ⊆ ACL cross-check:
// a recipients.jsonl row with no backing signed grant is reported (a roster
// entry a remote writer injected without a corresponding admitted member).
// The dispatcher satisfies it; nil skips the cross-check.
type teamMembershipReader interface {
	IsTeamMember(ctx context.Context, principalUID string) (bool, error)
}

// recipientReader is the pull-side roster source — the git transport's
// Recipients, which fast-forwards the mirror then parses recipients.jsonl.
// The supervisor type-asserts its transport onto this so a transport without
// the capability simply yields no roster (and thus no bootstrap).
type recipientReader interface {
	Recipients(ctx context.Context, scope kernel.Scope) ([]recipients.Row, error)
}

// conflictDetector scans the relations table for functional slots with more
// than one open arm (a sign of concurrent supersedes from two devices) and
// emits one relationSupersedeConflict event per newly-detected pair. The
// detect+emit cycle is idempotent: already-recorded pairs are skipped. The
// daemon wires this against the dispatcher's SQLite reader + predicate
// registry + event log; nil disables the check (test wiring with no kernel).
//
// HasOpenConflicts is the per-entity query used by getEntityContext to
// annotate the entity card with a provenance warning when open conflicts exist.
type conflictDetector interface {
	DetectAndEmit(ctx context.Context) error
	HasOpenConflicts(ctx context.Context, entityUID string) (bool, error)
}

// resolveDeviceTag returns this device's stable identity device ID for use as
// the sealer's chunk Header.Device tag. This MUST match the device ID the
// event signer stamps into Author.DeviceID (identity.Store.DeviceID is the
// single source for both): a peer's replay-boundary check
// (chunk.Header.AuthorsMatchDevice) rejects a chunk whose header device claim
// disagrees with its events' signed authorship, so a hostname tag (which the
// signer never uses) would make every team chunk this device seals
// unreadable. Falls back to the hostname only when no keychain is available
// (pre-identity / test wiring), where team scopes do not exist anyway.
func resolveDeviceTag(ctx context.Context, secrets secret.Provider, logger *zap.Logger) string {
	if secrets == nil {
		return hostnameOrUnknown()
	}
	id, err := identity.New(secrets).DeviceID(ctx)
	if err != nil || id == "" {
		logger.Warn("daemon: resolve identity device id failed; using hostname for chunk device tag (team chunks need the identity id)",
			zap.Error(err))
		return hostnameOrUnknown()
	}
	return id
}

// envelopeScopes returns the scopes configured with an envelope-mode remote —
// the scopes that use the shared-DEK namespace (currently the team scope).
// Kept for use where only the envelope path matters (e.g. recipients staging).
func envelopeScopes(sync config.SyncTable) []kernel.Scope {
	var out []kernel.Scope
	for scopeName, r := range sync.Remotes {
		if r.URL == "" || !r.Envelope() {
			continue
		}
		if scope := kernel.Scope(scopeName); scope.Valid() {
			out = append(out, scope)
		}
	}
	return out
}

// consumeScopes returns ALL scopes that have a configured remote URL.
// The consume loop runs for every such scope regardless of encryption mode
// (envelope, Watchword-derived, or plaintext) and independent of AutoPush —
// pulling a remote is always wanted when a remote is configured.
func consumeScopes(sync config.SyncTable) []kernel.Scope {
	var out []kernel.Scope
	for scopeName, r := range sync.Remotes {
		if r.URL == "" {
			continue
		}
		if scope := kernel.Scope(scopeName); scope.Valid() {
			out = append(out, scope)
		}
	}
	return out
}

// consumeLoop pulls + decodes + grafts a team scope on the push cadence,
// plus once eagerly on start so a freshly-joined device catches up
// immediately rather than waiting a full interval.
func (s *syncSupervisor) consumeLoop(ctx context.Context, scope kernel.Scope) {
	defer s.wg.Done()
	t := time.NewTicker(s.pushInterval())
	defer t.Stop()
	s.consumeOnce(ctx, scope)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.consumeOnce(ctx, scope)
		}
	}
}

// consumeOnce runs one pull → bootstrap → decode → graft pass. Every step is
// best-effort and logged: a transient pull failure still lets the decoder
// work the chunks already on disk, and a missing DEK (not yet granted) is a
// quiet no-op until the grant arrives.
//
// Scope routing: envelope scopes (team) use the shared-DEK ring bootstrapped
// from recipients.jsonl; Watchword-derived scopes use a single derived key;
// plaintext scopes use a nil key (chunk.Decode auto-detects the JSONL format).
// The team scope is hard-guarded to the envelope path only — Watchword fallback
// on a team chunk would silently misattribute authorship; we fail closed.
func (s *syncSupervisor) consumeOnce(ctx context.Context, scope kernel.Scope) {
	if s.trans == nil || s.cfg.Grafter == nil {
		return
	}
	if err := s.pullChunks(ctx, scope); err != nil {
		s.logger.Warn("daemon: sync consume pull failed (will retry; decoding chunks already on disk)",
			zap.String("scope", string(scope)), zap.Error(err))
	}

	// After each pull, try to import the genesis trust anchor from TEAM.json if
	// it has not been pinned yet. This is the primary way a joining device pins
	// the genesis without needing to trust the ULID ordering of the event log.
	// Best-effort and idempotent: a missing TEAM.json (older remote) or an
	// already-stored pin are both silent no-ops.
	if scope == kernel.ScopeTeam {
		s.maybeImportTeamJSONPin(ctx)
	}

	remote := s.cfg.Sync.Remotes[string(scope)]
	var (
		ring        map[int][]byte
		touchedUIDs []string
	)
	switch {
	case remote.Envelope():
		// Envelope (team) scope: bootstrap the DEK ring from recipients.jsonl.
		// Hard guard: team must only use the envelope path, never Watchword fallback.
		if scope == kernel.ScopeTeam && !remote.Envelope() {
			s.logger.Error("daemon: sync consume team scope must use envelope encryption; refusing Watchword fallback",
				zap.String("scope", string(scope)))
			return
		}
		rows := s.readRecipientRows(ctx, scope)
		ring = s.bootstrapAndResolveRing(ctx, scope, rows)
		if len(ring) == 0 {
			// No DEK on this device yet — not granted, or recipients.jsonl not
			// published. Nothing to decode; the next tick retries.
			return
		}
		touchedUIDs = s.decodeAndGraft(ctx, scope, ring)
		s.crossCheckRecipients(ctx, scope, rows)
	default:
		// Watchword-derived or plaintext scope: resolve a single key and build
		// a synthetic ring at epoch 0 so decodeAndGraft can share the same path.
		// Plaintext scopes use nil (chunk.Decode auto-detects the JSONL format).
		key, ok := s.resolveWatchwordOrPlaintextKey(ctx, scope)
		if !ok {
			return
		}
		ring = map[int][]byte{0: key}
		touchedUIDs = s.decodeAndGraft(ctx, scope, ring)
	}

	// Post-graft dedup sweep: run the entitymatch lanes over every entity that
	// arrived in this batch. Strong-identifier hits auto-merge; exact-name and
	// name-KNN hits write entity_dup_candidates rows for human review (see
	// DedupSweep's doc comment for the lane definitions).
	if s.cfg.DedupSweep != nil && len(touchedUIDs) > 0 {
		s.cfg.DedupSweep.Run(ctx, touchedUIDs)
	}
	// Post-graft supersede-conflict detection: scan for functional slots with
	// multiple open arms introduced by this device or a peer. Non-fatal: a
	// detector error is logged WARN and never fails the consume pass.
	if s.cfg.ConflictDetector != nil && len(touchedUIDs) > 0 {
		if err := s.cfg.ConflictDetector.DetectAndEmit(ctx); err != nil {
			s.logger.Warn("daemon: sync consume conflict detection failed (advisory; will retry next pass)",
				zap.String("scope", string(scope)), zap.Error(err))
		}
	}
}

// resolveWatchwordOrPlaintextKey returns the single consume key for a
// non-envelope scope: a 32-byte Watchword-derived key when the scope is
// configured for encrypted mode, or nil for a plaintext scope. Returns
// ok=false only when encryption is requested but the Watchword is absent
// (logged WARN; the next tick retries after the mnemonic is set).
func (s *syncSupervisor) resolveWatchwordOrPlaintextKey(ctx context.Context, scope kernel.Scope) ([]byte, bool) {
	remote := s.cfg.Sync.Remotes[string(scope)]
	if !remote.EncryptedOrDefault() {
		// Plaintext scope: nil key signals chunk.Decode to use the JSONL path.
		return nil, true
	}
	// Encrypted Watchword scope: derive the per-scope key from the mnemonic.
	mnemonic, err := loadWatchword(ctx, s.cfg.SecretChain)
	if err != nil {
		s.logger.Warn("daemon: sync consume Watchword load failed for scope",
			zap.String("scope", string(scope)), zap.Error(err))
		return nil, false
	}
	if mnemonic == "" {
		s.logger.Warn("daemon: sync consume scope uses encryption but Watchword is absent; skipping until mnemonic is set",
			zap.String("scope", string(scope)))
		return nil, false
	}
	root, derr := watchword.DeriveRoot(mnemonic)
	if derr != nil {
		s.logger.Warn("daemon: sync consume Watchword root derivation failed",
			zap.String("scope", string(scope)), zap.Error(derr))
		return nil, false
	}
	key, kerr := watchword.DeriveScopeKey(root, string(scope))
	if kerr != nil {
		s.logger.Warn("daemon: sync consume scope key derivation failed",
			zap.String("scope", string(scope)), zap.Error(kerr))
		return nil, false
	}
	return key, true
}

// readRecipientRows fetches the pulled roster once per consume pass. It returns
// nil when the transport has no recipient capability or the read fails (logged)
// — both callers treat a nil roster as "nothing to bootstrap / cross-check".
func (s *syncSupervisor) readRecipientRows(ctx context.Context, scope kernel.Scope) []recipients.Row {
	rr, ok := s.trans.(recipientReader)
	if !ok {
		return nil
	}
	rows, err := rr.Recipients(ctx, scope)
	if err != nil {
		s.logger.Warn("daemon: sync consume read recipients failed",
			zap.String("scope", string(scope)), zap.Error(err))
		return nil
	}
	return rows
}

// pullChunks mirrors the remote's chunks into the local FSStore — the inverse
// of pushOnce. EnsureRepo + fast-forward happen inside List/Get, so the local
// mirror (and its recipients.jsonl) is current afterwards.
func (s *syncSupervisor) pullChunks(ctx context.Context, scope kernel.Scope) error {
	remote, err := s.trans.List(ctx, scope)
	if err != nil {
		return fmt.Errorf("list remote: %w", err)
	}
	local, err := s.store.List(ctx, scope)
	if err != nil {
		return fmt.Errorf("list local: %w", err)
	}
	have := make(map[string]struct{}, len(local))
	for i := range local {
		have[local[i].ChunkID] = struct{}{}
	}
	for i := range remote.Chunks {
		hdr := remote.Chunks[i]
		if _, dup := have[hdr.ChunkID]; dup {
			continue
		}
		blob, gerr := s.trans.Get(ctx, scope, hdr.ChunkID)
		if gerr != nil {
			s.logger.Warn("daemon: sync consume get-chunk failed",
				zap.String("chunk_id", hdr.ChunkID), zap.Error(gerr))
			continue
		}
		body, rerr := readAllAndClose(blob.Body)
		if rerr != nil {
			s.logger.Warn("daemon: sync consume read-chunk failed",
				zap.String("chunk_id", hdr.ChunkID), zap.Error(rerr))
			continue
		}
		// Integrity guard: a chunk hand-edited outside Mazarbul on the
		// remote (the plaintext case the AEAD tag cannot cover) is
		// detected here, before it can reach the local store or graft.
		// A chunk absent from the recorded hashes (legacy pre-v2 MANIFEST,
		// or a transport without the capability) is treated as unverified,
		// not tampered — so a clean upgrade path is preserved.
		// A rejected chunk is skipped, not fatal: the rest of the pull still
		// grafts. It will be re-fetched and re-rejected on every cycle until the
		// remote is repaired — that re-reject is intentional (we never graft
		// unverified bytes), and the standing condition surfaces on demand via
		// the `sync.chunk_integrity` doctor check rather than a one-shot log.
		if want, ok := remote.ContentHashes[hdr.ChunkID]; ok && want != chunk.ContentHash(body) {
			s.logger.Error("daemon: sync consume rejected tampered chunk; not grafting",
				zap.String("scope", string(scope)),
				zap.String("chunk_id", hdr.ChunkID),
				zap.String("hint", "this chunk was edited outside Mazarbul; restore it from git history. chunks/ is machine-managed and must not be hand-edited"))
			continue
		}
		if perr := s.store.Put(ctx, scope, hdr, body); perr != nil {
			s.logger.Warn("daemon: sync consume put-chunk failed",
				zap.String("chunk_id", hdr.ChunkID), zap.Error(perr))
		}
	}
	return nil
}

// bootstrapAndResolveRing reads the pulled recipients roster, unwraps any rows
// addressed to this device into the keychain, then resolves the full per-epoch
// DEK ring the decoder selects from. Returns an empty ring when this device
// holds no team DEK (not yet granted).
func (s *syncSupervisor) bootstrapAndResolveRing(ctx context.Context, scope kernel.Scope, rows []recipients.Row) map[int][]byte {
	if len(rows) > 0 {
		if n, berr := bootstrapTeamDEKs(ctx, rows, s.cfg.DataDir, s.cfg.SecretChain, s.logger); berr != nil {
			s.logger.Warn("daemon: sync consume DEK bootstrap failed",
				zap.String("scope", string(scope)), zap.Error(berr))
		} else if n > 0 {
			s.logger.Info("daemon: sync consume bootstrapped team DEK(s) from recipients",
				zap.String("scope", string(scope)), zap.Int("epochs_unwrapped", n))
		}
	}
	return resolveTeamDEKRing(ctx, s.cfg.SecretChain, s.logger)
}

// decodeAndGraft decodes every local chunk for scope under its epoch DEK,
// rejects any chunk whose authenticated device header disagrees with its
// events' signed authorship, and grafts the survivors. Per-chunk graft keeps
// memory bounded and gives per-chunk reject granularity. Returns the set of
// entity UIDs from entity-bearing events in the batch so the caller can run
// the post-graft dedup sweep over the newly-arrived entities.
func (s *syncSupervisor) decodeAndGraft(ctx context.Context, scope kernel.Scope, ring map[int][]byte) []string {
	refs, err := s.store.List(ctx, scope)
	if err != nil {
		s.logger.Warn("daemon: sync consume list-local failed",
			zap.String("scope", string(scope)), zap.Error(err))
		return nil
	}
	grafted, skipped, rejected := 0, 0, 0
	var touchedUIDs []string
	for i := range refs {
		res, uids, ok := s.decodeAndGraftOne(ctx, scope, refs[i].ChunkID, ring)
		if !ok {
			continue
		}
		grafted += res.Grafted
		skipped += res.Skipped
		rejected += len(res.Rejections)
		touchedUIDs = append(touchedUIDs, uids...)
	}
	if grafted > 0 || rejected > 0 {
		s.logger.Info("daemon: sync consume graft",
			zap.String("scope", string(scope)),
			zap.Int("grafted", grafted),
			zap.Int("skipped", skipped),
			zap.Int("rejected", rejected))
	}
	return touchedUIDs
}

// decodeAndGraftOne handles a single chunk. Returns ok=false when the chunk
// could not be turned into a graftable batch (missing DEK epoch, decode fault,
// or a header-device mismatch) — all logged, none fatal. On success it also
// returns the entity UIDs gathered from entity-bearing events in the decoded
// batch, for the post-graft dedup sweep.
func (s *syncSupervisor) decodeAndGraftOne(ctx context.Context, scope kernel.Scope, chunkID string, ring map[int][]byte) (kernel.GraftResult, []string, bool) {
	blob, err := s.store.Get(ctx, scope, chunkID)
	if err != nil {
		s.logger.Warn("daemon: sync consume get-local failed", zap.String("chunk_id", chunkID), zap.Error(err))
		return kernel.GraftResult{}, nil, false
	}
	hdr, err := chunk.PeekHeader(blob)
	if err != nil {
		s.logger.Warn("daemon: sync consume peek failed", zap.String("chunk_id", chunkID), zap.Error(err))
		return kernel.GraftResult{}, nil, false
	}
	dek, ok := ring[hdr.DEKEpoch]
	if !ok {
		// No DEK for this epoch on this device: a chunk from before we were
		// granted, or from an epoch a revoke cut us out of. Skip quietly.
		return kernel.GraftResult{}, nil, false
	}
	h, events, err := chunk.Decode(blob, scope, dek)
	if err != nil {
		s.logger.Warn("daemon: sync consume decode failed", zap.String("chunk_id", chunkID), zap.Error(err))
		return kernel.GraftResult{}, nil, false
	}
	// Bind the chunk's authenticated device header to the per-event signed
	// authorship: a chunk claiming Device=X but carrying events authored by
	// device Y is a misattribution — reject the whole chunk.
	if merr := h.AuthorsMatchDevice(events); merr != nil {
		s.logger.Warn("daemon: sync consume rejected chunk (device/author mismatch)",
			zap.String("chunk_id", chunkID), zap.Error(merr))
		return kernel.GraftResult{}, nil, false
	}
	// Gather entity UIDs before grafting so the dedup sweep can process the
	// full batch regardless of how many events GraftSealed skips as duplicates.
	entityUIDs := gatherEntityUIDs(events)
	res, gerr := s.cfg.Grafter.GraftSealed(ctx, events)
	if gerr != nil {
		s.logger.Warn("daemon: sync consume graft failed", zap.String("chunk_id", chunkID), zap.Error(gerr))
		return kernel.GraftResult{}, nil, false
	}
	for _, rej := range res.Rejections {
		s.logger.Warn("daemon: sync consume dropped unverifiable event",
			zap.String("chunk_id", chunkID), zap.String("event_uid", rej.UID), zap.Error(rej.Err))
	}
	return res, entityUIDs, true
}

// gatherEntityUIDs extracts entity UIDs from entity-bearing events in a
// decoded batch for use by the post-graft dedup sweep. It picks up new entity
// creations (kind="entity") and identifier additions (kind="entityIdentifierAdded")
// since a new identifier on an existing entity may open a new identifier-lane
// match. Malformed payloads are skipped without error.
func gatherEntityUIDs(events []kernel.Event) []string {
	var uids []string
	for i := range events {
		ev := &events[i]
		switch string(ev.Kind) {
		case ontology.KindEntity:
			var e struct {
				UID string `json:"uid"`
			}
			if jerr := json.Unmarshal(ev.Payload, &e); jerr == nil && e.UID != "" {
				uids = append(uids, e.UID)
			}
		case ontology.KindEntityIdentifierAdded:
			var e struct {
				EntityUID string `json:"entityUid"`
			}
			if jerr := json.Unmarshal(ev.Payload, &e); jerr == nil && e.EntityUID != "" {
				uids = append(uids, e.EntityUID)
			}
		}
	}
	return uids
}

// crossCheckRecipients reports recipients.jsonl rows that no signed grant
// backs (a roster entry injected by a remote writer without an admitted
// member). Best-effort and advisory: the authoritative gate is the membership
// fold; this only surfaces a roster/ACL divergence for the operator. Runs
// post-graft so the ACL reflects the grants this pass just replayed.
func (s *syncSupervisor) crossCheckRecipients(ctx context.Context, scope kernel.Scope, rows []recipients.Row) {
	if s.cfg.Members == nil || len(rows) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(rows))
	for i := range rows {
		uid := rows[i].PrincipalUID
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		member, merr := s.cfg.Members.IsTeamMember(ctx, uid)
		if merr != nil {
			continue
		}
		if !member {
			s.logger.Warn("daemon: sync consume recipients row has no backing admitted grant (ignored)",
				zap.String("scope", string(scope)), zap.String("principal_uid", uid))
		}
	}
}

// bootstrapTeamDEKs finds rows addressed to this device's principal in the
// pulled recipients roster and unwraps each row's wrapped DEK into the
// keychain so the sealer/decoder can use it. Idempotent: an epoch already in
// the keychain is skipped. A row that does not unwrap (sealed to a different
// device, or corrupt) is skipped, not fatal. Returns the count newly unwrapped.
func bootstrapTeamDEKs(ctx context.Context, rows []recipients.Row, dataDir string, secrets secret.Provider, logger *zap.Logger) (int, error) {
	if secrets == nil {
		return 0, nil
	}
	self, err := config.SelfPrincipalUID(dataDir)
	if err != nil {
		// A real read/decode fault must surface (the caller logs it and retries
		// next tick) rather than masquerade as "no binding yet" — otherwise a
		// transiently-unreadable state file silently stalls all team decryption.
		// SelfPrincipalUID already maps a genuinely-absent binding to ("", nil).
		return 0, fmt.Errorf("resolve self principal: %w", err)
	}
	if self == "" {
		// No self→principal binding yet (pre-genesis). Nothing to match.
		return 0, nil
	}
	wrapPriv, err := identity.New(secrets).WrappingKey(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve wrap key: %w", err)
	}
	unwrapped := 0
	for i := range rows {
		if rows[i].PrincipalUID != self {
			continue
		}
		if ok := bootstrapOneDEK(ctx, &rows[i], wrapPriv, secrets, logger); ok {
			unwrapped++
		}
	}
	return unwrapped, nil
}

// bootstrapOneDEK unwraps a single recipient row into the keychain, returning
// true only when it persisted a DEK not previously present.
func bootstrapOneDEK(ctx context.Context, row *recipients.Row, wrapPriv *ecdh.PrivateKey, secrets secret.Provider, logger *zap.Logger) bool {
	key := watchword.KeychainTeamDEKPrefix + strconv.Itoa(row.DEKEpoch)
	if existing, gerr := secrets.Get(ctx, key); gerr == nil {
		if existing != "" {
			return false // already bootstrapped this epoch
		}
	} else if !errors.Is(gerr, secret.ErrNotFound) {
		// A real keychain read fault (not a clean miss) is worth surfacing: we
		// fall through and re-bootstrap (idempotent), but a recurring warning
		// here flags a degrading key store rather than hiding it.
		logger.Warn("daemon: sync consume DEK keychain read failed (will attempt re-bootstrap)",
			zap.Int("dek_epoch", row.DEKEpoch), zap.Error(gerr))
	}
	sealed, derr := base64.StdEncoding.DecodeString(row.WrappedDEK)
	if derr != nil {
		logger.Warn("daemon: sync consume recipients row has malformed wrapped DEK",
			zap.Int("dek_epoch", row.DEKEpoch), zap.Error(derr))
		return false
	}
	dek, uerr := watchword.UnwrapDEK(sealed, wrapPriv)
	if uerr != nil {
		logger.Warn("daemon: sync consume could not unwrap DEK (not for this device, or corrupt)",
			zap.Int("dek_epoch", row.DEKEpoch), zap.Error(uerr))
		return false
	}
	if serr := secrets.Set(ctx, key, base64.StdEncoding.EncodeToString(dek)); serr != nil {
		logger.Warn("daemon: sync consume persist DEK failed",
			zap.Int("dek_epoch", row.DEKEpoch), zap.Error(serr))
		return false
	}
	return true
}

// readAllAndClose drains a chunk body and closes it, surfacing a read error
// over a close error (a close failure after a full read does not invalidate
// the bytes we already hold).
func readAllAndClose(body io.ReadCloser) ([]byte, error) {
	data, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read chunk body: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close chunk body: %w", closeErr)
	}
	return data, nil
}

// resolveTeamDEKRing reads the team DEK epochs this device holds from the
// keychain into an epoch→key map for the decoder. Epochs are minted densely
// (genesis=0, +1 per revoke re-key) and a full-history grant back-fills every
// epoch for a new member, so a member normally holds a contiguous run from 0.
//
// The scan is gap-tolerant: a single missing or corrupt epoch (e.g. a transient
// keychain write during bootstrap) must NOT silently drop every HIGHER epoch —
// that would render already-bootstrapped chunks permanently undecryptable for
// the process lifetime. It therefore continues past gaps, only stopping after a
// long run of consecutive empty epochs (far past any real epoch count). Epoch 0
// (genesis) is the one DEK every member holds, so its absence means this device
// holds no team DEK at all — short-circuit there to keep a non-member daemon's
// ~30s consume loop from probing for absent epochs. An empty map means no DEK.
func resolveTeamDEKRing(ctx context.Context, secrets secret.Provider, logger *zap.Logger) map[int][]byte {
	if secrets == nil {
		return nil
	}
	const (
		maxEpochProbe   = 100_000 // defensive bound; real epoch counts stay tiny
		maxConsecMisses = 64      // stop after this many absent epochs in a row
	)
	ring := map[int][]byte{}
	misses := 0
	for e := range maxEpochProbe {
		key, ok := readEpochDEK(ctx, secrets, e, logger)
		if !ok {
			// Epoch 0 (genesis) is the DEK every member holds, so its absence
			// means this device holds no team DEK at all — short-circuit to keep
			// a non-member daemon's consume loop from probing for absent epochs.
			if e == 0 {
				return ring
			}
			misses++
			if misses >= maxConsecMisses {
				break
			}
			continue
		}
		misses = 0
		ring[e] = key
	}
	return ring
}

// readEpochDEK reads and validates one epoch's DEK from the keychain. It returns
// ok=false for an absent epoch, a corrupt row, OR a transient keychain fault —
// all of which the gap-tolerant ring scan treats as a single miss. A non-clean
// fault (not ErrNotFound) is logged so a degrading key store stays visible
// rather than silently shrinking the ring (mirrors bootstrapOneDEK's warning).
func readEpochDEK(ctx context.Context, secrets secret.Provider, epoch int, logger *zap.Logger) ([]byte, bool) {
	raw, err := secrets.Get(ctx, watchword.KeychainTeamDEKPrefix+strconv.Itoa(epoch))
	if err != nil {
		if !errors.Is(err, secret.ErrNotFound) {
			logger.Warn("daemon: sync consume DEK ring keychain read failed (treated as missing epoch)",
				zap.Int("dek_epoch", epoch), zap.Error(err))
		}
		return nil, false
	}
	if raw == "" {
		return nil, false
	}
	decoded, derr := base64.StdEncoding.DecodeString(raw)
	if derr != nil || len(decoded) != watchword.DEKSize {
		return nil, false // corrupt row: a miss for THIS epoch, higher epochs still scan
	}
	return decoded, true
}

// maybeImportTeamJSONPin reads TEAM.json from the team-scope sync repo mirror
// and persists the genesis pin to state.toml when no pin is already stored.
// It is called on every team-scope consume pass (after pullChunks) so a
// joining device pins the genesis anchor on its first successful pull rather
// than relying on ULID ordering alone.
//
// Design choices:
//   - Idempotent: once a pin is stored it is never overwritten by this path
//     (use `maz team pin --clear` to deliberately re-anchor).
//   - VERIFIED before persisting: the anchor must (a) name the fold's own
//     genesis principal and (b) carry a valid ed25519 self-signature by a key
//     the just-grafted event log enrolled for that principal. Without this,
//     an adversary with bare repo-write access could plant a foreign anchor;
//     the poisoned pin would then fail-close the entire team fold until a
//     manual re-pin. A missing projection (fresh mirror, enrollment not yet
//     grafted) skips silently and retries next pass. The doctor check
//     (team.genesis_pin) re-verifies continuously.
//   - Best-effort: any error is logged WARN and the consume pass continues.
func (s *syncSupervisor) maybeImportTeamJSONPin(ctx context.Context) {
	if s.cfg.GenesisKeys == nil {
		// No projection access (test wiring / no dispatcher): an anchor that
		// cannot be verified is never persisted.
		return
	}
	dataDir := s.cfg.DataDir
	// If a pin is already stored, nothing to do.
	_, _, alreadyPinned, lerr := config.LoadTeamGenesisPin(dataDir)
	if lerr != nil {
		s.logger.Warn("daemon: sync consume team-json: failed to check existing genesis pin",
			zap.Error(lerr))
		return
	}
	if alreadyPinned {
		return
	}
	// Read TEAM.json from the git repo mirror.
	repoDir := filepath.Join(s.cfg.DataDir, "sync", "team", "repo")
	tj, found, rErr := gitxport.ReadTeamJSON(repoDir)
	if rErr != nil {
		s.logger.Warn("daemon: sync consume team-json: read failed (will retry next pull)",
			zap.Error(rErr))
		return
	}
	if !found {
		// Older remote or genesis device has not pushed yet. Normal; silent.
		return
	}
	if tj.PrincipalUID == "" || tj.SigKeyID == "" {
		s.logger.Warn("daemon: sync consume team-json: TEAM.json has empty principal or key id; skipping")
		return
	}
	genesisUID, signPub, ok, kErr := s.cfg.GenesisKeys.GenesisAnchorKey(ctx, tj.SigKeyID)
	if kErr != nil {
		s.logger.Warn("daemon: sync consume team-json: genesis key lookup failed (will retry next pull)",
			zap.Error(kErr))
		return
	}
	if !ok {
		// Fold has no genesis yet, or the anchor's key is not enrolled for the
		// genesis principal: either the enrollment has not been grafted yet
		// (retry next pass) or the anchor is foreign (doctor keeps WARNing
		// unpinned, so the operator sees it).
		s.logger.Debug("daemon: sync consume team-json: anchor key not resolvable from projections; not pinning",
			zap.String("sig_key_id", tj.SigKeyID))
		return
	}
	if genesisUID != tj.PrincipalUID {
		s.logger.Warn("daemon: sync consume team-json: TEAM.json principal does not match fold genesis; NOT pinning",
			zap.String("team_json_principal", tj.PrincipalUID),
			zap.String("fold_genesis", genesisUID))
		return
	}
	if vErr := gitxport.VerifyTeamJSON(tj, signPub); vErr != nil {
		s.logger.Warn("daemon: sync consume team-json: anchor signature INVALID; NOT pinning",
			zap.String("principal_uid", tj.PrincipalUID),
			zap.String("sig_key_id", tj.SigKeyID),
			zap.Error(vErr))
		return
	}
	if err := config.SetTeamGenesisPin(dataDir, tj.PrincipalUID, tj.SigKeyID); err != nil {
		s.logger.Warn("daemon: sync consume team-json: failed to persist genesis pin",
			zap.String("principal_uid", tj.PrincipalUID), zap.Error(err))
		return
	}
	s.logger.Info("daemon: sync consume team-json: genesis pin imported from verified TEAM.json",
		zap.String("principal_uid", tj.PrincipalUID),
		zap.String("sig_key_id", tj.SigKeyID),
	)
}
