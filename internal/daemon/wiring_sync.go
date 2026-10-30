package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	"github.com/mo-cmyk/mazarbul/internal/sync/syncidentity"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// Default cadences for the sync supervisor. The plan ships these as
// state.toml-tunable; the defaults below match the documented
// behaviour ("seal every hour, push every 30s, hard cap 4/min").
const (
	defaultSealInterval     = time.Hour
	defaultPushInterval     = 30 * time.Second
	defaultSealMax          = 10_000
	defaultSealMaxPlaintext = 2_000
)

// syncSupervisorConfig collects the daemon-side wiring inputs for the
// Moria sync supervisor. Constructed in OpenWiring once the resolved
// config + secret chain + event log are known.
type syncSupervisorConfig struct {
	DataDir     string
	DeviceID    string
	Source      chunk.EventSource
	Sync        config.SyncTable
	SecretChain secret.Provider
	// Recipients renders the team scope's recipients.jsonl from the admitted
	// ACL after each team-scope seal so the transport publishes the wrapped
	// DEKs a fresh device needs to bootstrap. Nil disables the render (e.g. a
	// test wiring with no dispatcher); the team scope then seals chunks but
	// stages no recipients file.
	Recipients teamRecipientSource
	// Grafter persists decoded team chunks verbatim into the event log on the
	// consume/pull path — the seal/produce path's counterpart. Nil disables the
	// team consume loop (a test wiring with no kernel); production passes the
	// FileEventLog (which satisfies teamGrafter via GraftSealed).
	Grafter teamGrafter
	// Members answers the recipients ⊆ ACL cross-check at the consume boundary
	// (a roster row with no backing signed grant is reported). Optional; nil
	// skips the advisory cross-check.
	Members teamMembershipReader
	// DedupSweep runs the post-consume duplicate-detection sweep over the entity
	// UIDs touched by the just-grafted batch. Nil disables the sweep (test wiring
	// with no kernel, or a daemon without a live dispatcher).
	DedupSweep *DedupSweep
	// ConflictDetector runs the supersede-conflict detection scan after each
	// successful graft batch and emits relationSupersedeConflict events for any
	// newly-detected open functional-slot collisions. Nil disables the check (test
	// wiring with no kernel, or a daemon without a live dispatcher). Non-fatal: a
	// detection error is logged WARN and never fails the consume pass.
	ConflictDetector conflictDetector
	// GenesisKeys resolves the fold genesis principal's enrolled signing key so
	// the consume loop can verify a pulled TEAM.json anchor BEFORE persisting
	// the genesis pin. Nil disables the TEAM.json pin import entirely (test
	// wiring with no kernel, or a daemon without a live dispatcher): an
	// unverifiable anchor is never persisted, because a poisoned pin
	// fail-closes the whole team fold until a manual re-pin.
	GenesisKeys genesisAnchorKeySource

	// AuthorName/AuthorMail are the resolved git author identity stamped on
	// every sync commit (bring-your-own-git, Workstream A). CommitMode is the
	// resolved noreply|real|anonymous label for the trailer + doctor surface.
	// SelfPrincipalUID is the committing principal (excluded from co-author
	// attribution). CoAuthors resolves peer principals to author lines; nil
	// disables co-author trailers. All resolved by startSyncSupervisor.
	AuthorName       string
	AuthorMail       string
	CommitMode       string
	SelfPrincipalUID string
	CoAuthors        gittransport.CoAuthorResolver

	Logger *zap.Logger
}

// gitIdentitySource resolves a principal's recorded GitHub identity (login,
// numeric id, display name) so the sync transport can author commits as the
// real user. The daemon adapts the dispatcher onto it; the resolver also
// satisfies gittransport.CoAuthorResolver so one adapter serves both the
// self-identity resolution and the peer Co-authored-by trailer. It is an alias
// of syncidentity.Source so the daemon and the `maz sync push` CLI resolve the
// author through the one shared chain.
type gitIdentitySource = syncidentity.Source

// teamRecipientSource yields the current admitted-ACL recipient rows (wrapped
// DEKs, no PII) for staging into recipients.jsonl. The daemon adapts the
// dispatcher onto it; the supervisor stays free of a dispatcher import.
type teamRecipientSource interface {
	TeamRecipients(ctx context.Context) ([]recipients.Row, error)
}

// genesisAnchorKeySource resolves the fold genesis principal and the signing
// public key it has enrolled under a given key fingerprint, from the local
// projections. Satisfied by *dispatcher.Dispatcher (GenesisAnchorKey); the
// supervisor stays free of a dispatcher import. A false ok with a nil error
// means the fold has no genesis yet or the key is not enrolled for the
// genesis principal — retry after a later graft.
type genesisAnchorKeySource interface {
	GenesisAnchorKey(ctx context.Context, sigKeyID string) (principalUID string, signPub ed25519.PublicKey, ok bool, err error)
}

// syncSupervisor coordinates the background seal-and-push loops for
// every configured scope. Lifecycle: Start spawns goroutines; Close
// cancels the context and joins them. Idempotent on Close.
type syncSupervisor struct {
	cfg *syncSupervisorConfig

	store  *chunk.FSStore
	cursor *chunk.FileCursor
	// scopeKeys is the boot snapshot of each static (Watchword/plaintext) scope's
	// seal key. Envelope (team) scopes are NOT read from here on the seal path —
	// resolveSealKey re-probes the keychain per pass so a DEK rotation is honored
	// without a restart (F5). Kept for the static scopes whose key never rotates.
	scopeKeys map[kernel.Scope][]byte
	trans     transport.Transport

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
	logger   *zap.Logger
}

// newSyncSupervisor constructs but does NOT start the supervisor.
// Returns (nil, nil) when sync is structurally disabled — either no
// remotes are configured AND no Watchword is set (nothing to seal,
// nothing to push). Returns (nil, err) on fatal config errors
// (corrupt Watchword, invalid scope key). Returns (sv, nil) when the
// supervisor is ready to Start.
//
// The split between newSyncSupervisor and (sv).Start lets OpenWiring
// finish constructing the rest of Wired before kicking off goroutines,
// so an error there triggers a clean rollback that does not leak the
// supervisor's goroutines.
func newSyncSupervisor(ctx context.Context, cfg *syncSupervisorConfig) (*syncSupervisor, error) {
	if cfg == nil {
		return nil, errors.New("sync supervisor: config is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("sync supervisor: data dir is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	// Check for configured remotes BEFORE touching the OS keychain: with no
	// sync remotes (the default for a fresh instance and for CI, containers,
	// and any headless host without a secret service) the supervisor is a
	// no-op, so reading the watchword there would fail wiring for no reason.
	configured := configuredScopes(cfg.Sync)
	if len(configured) == 0 {
		cfg.Logger.Info("daemon: sync supervisor not started (no remotes configured)")
		return nil, nil //nolint:nilnil // documented no-op sentinel
	}
	mnemonic, err := loadWatchword(ctx, cfg.SecretChain)
	if err != nil {
		return nil, fmt.Errorf("sync supervisor: load watchword: %w", err)
	}
	scopeKeys, _, err := buildScopeKeyMap(ctx, mnemonic, cfg.Sync, cfg.SecretChain, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync supervisor: derive scope keys: %w", err)
	}
	if len(scopeKeys) == 0 {
		cfg.Logger.Info("daemon: sync supervisor not started (no scopes ready to seal)")
		return nil, nil //nolint:nilnil
	}

	store, err := chunk.NewFSStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("sync supervisor: open chunk store: %w", err)
	}
	cursor, err := chunk.NewFileCursor(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("sync supervisor: open cursor: %w", err)
	}
	hasRemote := len(configured) > 0

	var trans transport.Transport
	if hasRemote {
		opts := gittransport.Options{
			DataDir:            cfg.DataDir,
			RemoteURLs:         collectRemoteURLs(cfg.Sync),
			DeviceID:           cfg.DeviceID,
			AuthorName:         cfg.AuthorName,
			AuthorMail:         cfg.AuthorMail,
			CommitIdentityMode: cfg.CommitMode,
			SelfPrincipalUID:   cfg.SelfPrincipalUID,
			CoAuthors:          cfg.CoAuthors,
		}
		t, terr := gittransport.New(opts)
		if terr != nil {
			cfg.Logger.Warn("daemon: sync remote(s) configured but git transport refused to start; auto-push disabled",
				zap.Error(terr))
		} else {
			trans = t
		}
	}

	return &syncSupervisor{
		cfg:       cfg,
		store:     store,
		cursor:    cursor,
		scopeKeys: scopeKeys,
		trans:     trans,
		logger:    cfg.Logger,
	}, nil
}

// Start spawns one (sealer, pusher) goroutine pair per scope that has
// a derived key. The loops inherit parent so daemon shutdown
// (typically a SIGTERM-cancelled parent context) flows into the seal
// pass even before Close runs. Caller wires Close into Wired.closers
// for the deterministic LIFO shutdown path.
func (s *syncSupervisor) Start(parent context.Context) {
	if s == nil {
		return
	}
	s.stop = make(chan struct{})
	// Launch seal + push loops from the CONFIGURED scope set (symmetric with the
	// config-driven consume loops below), not the boot key snapshot: a team scope
	// whose DEK bootstraps only after join must still get a seal loop, which
	// sealOnce skips until resolveSealKey succeeds (F4).
	sealScopes := s.sealScopes()
	for _, scope := range sealScopes {
		s.wg.Add(1)
		go s.sealLoop(parent, scope)
		// Start the pusher when the scope should auto-publish: the global
		// AutoPush flag, OR the scope is an envelope (team) scope (a configured
		// team scope exists to be published to its peers, so it auto-publishes
		// regardless of AutoPush, which governs the single-device
		// personal/project convenience push).
		if s.trans != nil && s.shouldAutoPush(scope) {
			s.wg.Add(1)
			go s.pushLoop(parent, scope)
		}
	}
	// Consume (pull → bootstrap DEK → decode → graft) is driven off the config,
	// not s.scopeKeys: a freshly-joined device holds no team DEK at boot, so the
	// team scope is absent from scopeKeys until the consume loop bootstraps one
	// from the pulled recipients.jsonl. One loop per configured scope remote,
	// independent of AutoPush — pulling a remote brain down is always wanted
	// when a remote is configured, for personal/project and team alike.
	scopes := consumeScopes(s.cfg.Sync)
	if s.trans != nil && s.cfg.Grafter != nil {
		for _, scope := range scopes {
			s.wg.Add(1)
			go s.consumeLoop(parent, scope)
		}
	}
	s.logger.Info("daemon: sync supervisor started",
		zap.Int("scopes", len(sealScopes)),
		zap.Int("consume_scopes", len(scopes)),
		zap.Bool("auto_push", s.cfg.Sync.AutoPush),
		zap.Bool("transport_ready", s.trans != nil),
	)
}

// Close signals the supervisor to stop and waits for every background
// loop to return. Safe to call on a nil receiver and idempotent. The
// stop channel is closed exactly once via sync.Once so a double Close
// (LIFO teardown + explicit shutdown handler) does not panic. Before
// signalling stop, every configured scope gets a final synchronous
// seal pass: a laptop that closes after recent writes must seal those
// events into a chunk so the next boot (or another paired device)
// sees them, not leave them stranded in the open JSONL.
func (s *syncSupervisor) Close() error {
	if s == nil {
		return nil
	}
	s.stopOnce.Do(func() {
		// Final-seal pass uses a fresh context with a bounded budget so
		// a hung extractor or stuck disk cannot block shutdown forever.
		// 30s is plenty for the largest documented chunk (10k events).
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Final-seal every configured scope (not just those keyed at boot): a
		// team scope whose DEK bootstrapped after boot has pending events too.
		// sealOnce re-resolves the key and skips any scope still without one.
		for _, scope := range s.sealScopes() {
			s.sealOnce(ctx, scope)
		}
		if s.stop != nil {
			close(s.stop)
		}
	})
	s.wg.Wait()
	return nil
}

// sealInterval is the per-scope tick between background seal passes. An
// explicit operator override (SealIntervalSeconds) always wins. Otherwise an
// envelope (team) scope seals on the short defaultPushInterval (30s) so team
// events propagate promptly rather than waiting the 1h batching default that
// suits a single-device personal/project scope; a non-envelope scope keeps the
// hour. SealOnce is a no-op when nothing changed, so the short tick creates a
// chunk only when there is a new team event — no idle churn.
func (s *syncSupervisor) sealInterval(scope kernel.Scope) time.Duration {
	if s.cfg.Sync.SealIntervalSeconds > 0 {
		return time.Duration(s.cfg.Sync.SealIntervalSeconds) * time.Second
	}
	if s.cfg.Sync.Remotes[string(scope)].Envelope() {
		return defaultPushInterval
	}
	return defaultSealInterval
}

func (s *syncSupervisor) pushInterval() time.Duration {
	if s.cfg.Sync.CoalesceSeconds != nil {
		return time.Duration(*s.cfg.Sync.CoalesceSeconds) * time.Second
	}
	return defaultPushInterval
}

// shouldAutoPush reports whether scope's seal output is published to its remote
// without operator intervention. True when the global AutoPush flag is set, OR
// the scope is an envelope (team) scope — a configured team scope exists to be
// shared with its peers, so it auto-publishes regardless of AutoPush (which
// governs only the single-device personal/project convenience push). Used both
// to decide whether to start a scope's pushLoop and inside the loop's tick.
func (s *syncSupervisor) shouldAutoPush(scope kernel.Scope) bool {
	return s.cfg.Sync.AutoPush || s.cfg.Sync.Remotes[string(scope)].Envelope()
}

// sealMax picks the per-chunk event cap based on whether the scope is
// encrypted. Plaintext chunks default to 2_000 (so .jsonl files stay
// under github.com's ~1 MiB web-UI render ceiling); encrypted chunks
// default to 10_000 (gzip compression amortises the larger batch).
// An explicit operator override in config wins over both defaults.
func (s *syncSupervisor) sealMax(key []byte) int {
	if key == nil {
		if s.cfg.Sync.SealMaxEventsPlaintext > 0 {
			return s.cfg.Sync.SealMaxEventsPlaintext
		}
		return defaultSealMaxPlaintext
	}
	if s.cfg.Sync.SealMaxEvents > 0 {
		return s.cfg.Sync.SealMaxEvents
	}
	return defaultSealMax
}

// sealScopes is the set of scopes the seal + push loops run for: every
// configured remote scope, whether or not its key resolved at boot. This keeps
// the seal side symmetric with the config-driven consume side (consumeScopes) —
// a team scope whose DEK only bootstraps after join thus gets a seal loop from
// the start, and its sealOnce simply skips passes until resolveSealKey succeeds
// (F4). Invalid scope names in config are dropped with the rest of the pipeline.
func (s *syncSupervisor) sealScopes() []kernel.Scope {
	var out []kernel.Scope
	for _, name := range configuredScopes(s.cfg.Sync) {
		if scope := kernel.Scope(name); scope.Valid() {
			out = append(out, scope)
		}
	}
	return out
}

// resolveSealKey resolves the seal key + DEK epoch for scope on EACH pass rather
// than reading the boot snapshot — the fix that keeps the seal side symmetric
// with the consume side (which already re-resolves per pass). Consequences:
// after a revoke rotates the team DEK to a new epoch the very next seal pass
// seals under it (forward secrecy — F5), and a scope whose DEK only bootstraps
// after boot (a fresh joiner) begins sealing once the DEK lands (F4) — both with
// no daemon restart.
//
// Envelope (team) scopes re-probe the keychain via resolveCurrentDEK every pass.
// Watchword-derived and plaintext scopes never rotate, so they read the boot
// snapshot (re-deriving the KDF each pass would be wasteful and yields the
// identical key). Ok=false means "no key resolvable yet" — skip this pass.
func (s *syncSupervisor) resolveSealKey(ctx context.Context, scope kernel.Scope) (key []byte, epoch int, ok bool) {
	if s.cfg.Sync.Remotes[string(scope)].Envelope() {
		dek, ep, found := resolveCurrentDEK(ctx, s.cfg.SecretChain)
		if !found {
			return nil, 0, false
		}
		return dek, ep, true
	}
	// Static scope: the boot snapshot is authoritative. Present-with-nil is the
	// legitimate plaintext case (seal as JSONL); absent means the key could not
	// be derived at boot (encryption requested without a Watchword) — skip.
	k, present := s.scopeKeys[scope]
	if !present {
		return nil, 0, false
	}
	return k, 0, true
}

func (s *syncSupervisor) sealLoop(ctx context.Context, scope kernel.Scope) {
	defer s.wg.Done()
	interval := s.sealInterval(scope)
	t := time.NewTicker(interval)
	defer t.Stop()
	// Seal once on startup so a daemon that has been off long enough
	// to accumulate events catches up immediately rather than waiting
	// a full interval.
	s.sealOnce(ctx, scope)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.sealOnce(ctx, scope)
		}
	}
}

func (s *syncSupervisor) sealOnce(ctx context.Context, scope kernel.Scope) {
	if !scope.Valid() {
		return
	}
	// Defensive nil-check for the test wiring path: production wiring
	// always supplies orch.Log as the source. The supervisor unit tests
	// (no real kernel) leave Source nil and rely on Start/Close
	// lifecycle correctness without driving a seal pass.
	if s.cfg.Source == nil {
		return
	}
	// Re-resolve the key + DEK epoch on EVERY pass (not the boot snapshot) so a
	// post-revoke rotation seals under the new epoch (F5) and a joiner whose DEK
	// bootstraps after boot starts sealing once it lands (F4). ok=false means no
	// key is resolvable yet — skip this pass; the next ticker fire retries.
	key, epoch, ok := s.resolveSealKey(ctx, scope)
	if !ok {
		return
	}
	// Take the interprocess seal lock so a concurrent `maz sync push`
	// cannot race us into double-sealing the same UIDs with a
	// different device tag. ErrSealBusy means the CLI is already
	// sealing — skip this tick; the next ticker fire covers it.
	lock, lerr := chunk.TryAcquireSealLock(s.cfg.DataDir)
	if lerr != nil {
		if errors.Is(lerr, chunk.ErrSealBusy) {
			s.logger.Debug("daemon: sync seal busy (CLI holds the seal lock); skipping tick",
				zap.String("scope", string(scope)))
			return
		}
		s.logger.Warn("daemon: sync acquire seal lock failed",
			zap.String("scope", string(scope)), zap.Error(lerr))
		return
	}
	defer func() {
		if cerr := lock.Close(); cerr != nil {
			s.logger.Debug("daemon: sync release seal lock failed", zap.Error(cerr))
		}
	}()
	// key may legitimately be nil (plaintext mode). SealOnceWithEpoch
	// routes via chunk.Encode which interprets nil as "JSONL"; the epoch
	// stamps Header.DEKEpoch for envelope-mode scopes (0 otherwise).
	written, lastUID, err := chunk.SealOnceWithEpoch(ctx, s.cfg.Source, scope, key, epoch,
		s.store, s.cursor, s.sealMax(key), s.cfg.DeviceID)
	if err != nil {
		s.logger.Warn("daemon: sync seal failed",
			zap.String("scope", string(scope)),
			zap.Error(err))
		return
	}
	if written > 0 {
		s.logger.Info("daemon: sync seal",
			zap.String("scope", string(scope)),
			zap.Int("chunks_written", written),
			zap.String("last_uid", lastUID),
			zap.Bool("encrypted", key != nil),
			zap.Int("dek_epoch", epoch))
	}
	// Stage recipients.jsonl for an encrypted team scope so the transport
	// publishes the wrapped DEKs a fresh device needs to bootstrap. Done on
	// every seal pass (cheap; idempotent — identical ACL renders identical
	// bytes), so a membership change since the last push is reflected.
	if key != nil && scope == kernel.ScopeTeam {
		s.stageRecipients(ctx, scope)
	}
}

// stageRecipients renders the team scope's admitted ACL into the local
// recipients.jsonl staging file (<dataDir>/sync/<scope>/recipients.jsonl) that
// the transport copies into the repo on an encrypted push. Best-effort: a
// missing recipient source (test wiring) or a read error warns and skips —
// the chunks are still sealed; only the bootstrap artifact is deferred.
func (s *syncSupervisor) stageRecipients(ctx context.Context, scope kernel.Scope) {
	if s.cfg.Recipients == nil {
		return
	}
	rows, err := s.cfg.Recipients.TeamRecipients(ctx)
	if err != nil {
		s.logger.Warn("daemon: sync stage recipients failed",
			zap.String("scope", string(scope)), zap.Error(err))
		return
	}
	// An admitted team always has at least the genesis member, so an empty
	// roster is a transient read (mid-recompute, mid-rebuild) rather than a
	// real state. Skip the write so a stale-but-valid recipients.jsonl is left
	// in place instead of being clobbered to zero bytes — wiping it would
	// destroy the bootstrap roster a joining device needs to unwrap its DEK.
	if len(rows) == 0 {
		s.logger.Warn("daemon: sync stage recipients skipped: empty roster (transient); leaving prior file",
			zap.String("scope", string(scope)))
		return
	}
	path := filepath.Join(s.cfg.DataDir, "sync", string(scope), recipients.Filename)
	if werr := recipients.Write(path, rows); werr != nil {
		s.logger.Warn("daemon: sync write recipients failed",
			zap.String("scope", string(scope)), zap.Error(werr))
	}
}

func (s *syncSupervisor) pushLoop(ctx context.Context, scope kernel.Scope) {
	defer s.wg.Done()
	interval := s.pushInterval()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			// Push when the scope should auto-publish (AutoPush on, or an
			// envelope team scope) — matching the loop-start condition in Start.
			if !s.shouldAutoPush(scope) {
				continue
			}
			s.pushOnce(ctx, scope)
		}
	}
}

func (s *syncSupervisor) pushOnce(ctx context.Context, scope kernel.Scope) {
	if s.trans == nil {
		return
	}
	local, err := s.store.List(ctx, scope)
	if err != nil {
		s.logger.Warn("daemon: sync push list-local failed",
			zap.String("scope", string(scope)), zap.Error(err))
		return
	}
	if len(local) == 0 {
		return
	}
	remote, err := s.trans.List(ctx, scope)
	if err != nil {
		s.logger.Warn("daemon: sync push list-remote failed",
			zap.String("scope", string(scope)), zap.Error(err))
		return
	}
	seen := make(map[string]struct{}, len(remote.Chunks))
	for i := range remote.Chunks {
		seen[remote.Chunks[i].ChunkID] = struct{}{}
	}
	blobs := make([]transport.Blob, 0, len(local))
	for i := range local {
		if _, dup := seen[local[i].ChunkID]; dup {
			continue
		}
		data, gerr := s.store.Get(ctx, scope, local[i].ChunkID)
		if gerr != nil {
			s.logger.Warn("daemon: sync push get-blob failed",
				zap.String("chunk_id", local[i].ChunkID),
				zap.Error(gerr))
			continue
		}
		blobs = append(blobs, transport.Blob{
			Header: local[i].Header,
			Body:   io.NopCloser(bytes.NewReader(data)),
		})
	}
	if len(blobs) == 0 {
		return
	}
	accepted, perr := s.trans.Push(ctx, scope, blobs)
	if perr != nil {
		s.logger.Warn("daemon: sync push failed",
			zap.String("scope", string(scope)), zap.Error(perr))
		return
	}
	s.logger.Info("daemon: sync push",
		zap.String("scope", string(scope)),
		zap.Int("chunks_offered", len(blobs)),
		zap.Int("chunks_accepted", accepted),
		zap.String("remote", s.trans.Name()))
	if uerr := s.recordLastPush(scope, time.Now().UTC()); uerr != nil {
		s.logger.Debug("daemon: sync record last_push failed",
			zap.String("scope", string(scope)), zap.Error(uerr))
	}
}

// recordLastPush persists the timestamp of the latest successful push
// per scope into state.toml so the Settings → Sync panel shows an
// accurate "Last push" row instead of "never". Best-effort: a failure
// here logs but does not abort the push (the chunk is already on the
// remote; losing the timestamp is a UX regression, not data loss).
func (s *syncSupervisor) recordLastPush(scope kernel.Scope, at time.Time) error {
	state, lerr := config.LoadUserState(s.cfg.DataDir)
	if lerr != nil && !errors.Is(lerr, config.ErrUserStateNotFound) {
		return fmt.Errorf("load state.toml: %w", lerr)
	}
	if state.Sync.LastPush == nil {
		state.Sync.LastPush = map[string]string{}
	}
	state.Sync.LastPush[string(scope)] = at.Format(time.RFC3339)
	if err := config.WriteUserState(s.cfg.DataDir, state); err != nil {
		return fmt.Errorf("write state.toml: %w", err)
	}
	return nil
}

// loadWatchword pulls the user's mnemonic from the secret chain. An
// absent key is not an error — the supervisor handles the empty
// string as "Watchword not configured yet".
func loadWatchword(ctx context.Context, chain secret.Provider) (string, error) {
	if chain == nil {
		return "", nil
	}
	v, err := chain.Get(ctx, watchword.KeychainKey)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// buildScopeKeyMap inspects each configured remote and returns a
// per-scope key map suitable for the sealer goroutines:
//
//   - scope with Encrypted=true AND a Watchword present → 32-byte key
//     (encrypted seal path)
//   - scope with Encrypted=false (the plaintext default) → nil entry
//     (plaintext seal path)
//   - scope with Encrypted=true but no Watchword → entry omitted +
//     WARN logged (sealing skipped; doctor surfaces the misconfig)
//   - scope with no URL configured → entry omitted (nothing to seal
//     locally for a scope that has no remote)
//
// A nil value in the returned map is meaningful: the supervisor
// spawns a sealer for that scope and passes scopeKey=nil to
// chunk.SealOnceWith, producing plaintext JSONL chunks.
func buildScopeKeyMap(ctx context.Context, mnemonic string, sync config.SyncTable, secrets secret.Provider, logger *zap.Logger) (map[kernel.Scope][]byte, map[kernel.Scope]int, error) {
	out := map[kernel.Scope][]byte{}
	epochs := map[kernel.Scope]int{}
	for scopeName, remote := range sync.Remotes {
		if remote.URL == "" {
			continue
		}
		scope := kernel.Scope(scopeName)
		if !scope.Valid() {
			logger.Warn("daemon: skipping invalid scope in sync config",
				zap.String("scope", scopeName))
			continue
		}
		if !remote.EncryptedOrDefault() {
			out[scope] = nil
			continue
		}
		key, epoch, include, err := resolveEncryptedScopeKey(ctx, scopeName, remote, mnemonic, secrets, logger)
		if err != nil {
			return nil, nil, err
		}
		if !include {
			continue
		}
		out[scope] = key
		if epoch > 0 {
			epochs[scope] = epoch
		}
	}
	return out, epochs, nil
}

// resolveEncryptedScopeKey resolves the seal key for one encrypted scope.
// Envelope mode (team) reads the current-epoch DEK from the keychain;
// Watchword mode derives the per-scope key from the mnemonic. Include=false
// (no error) means "skip sealing this scope" — a device with no team DEK yet,
// or encryption requested with no Watchword — mirroring the documented
// encrypted-without-key posture. Epoch is 0 for Watchword scopes.
func resolveEncryptedScopeKey(ctx context.Context, scopeName string, remote config.ScopeRemoteTable, mnemonic string, secrets secret.Provider, logger *zap.Logger) (key []byte, epoch int, include bool, err error) {
	if remote.Envelope() {
		dek, ep, ok := resolveCurrentDEK(ctx, secrets)
		if !ok {
			logger.Warn("daemon: scope opted into envelope encryption but no team DEK is in the keychain; sealing disabled for this scope (run `maz team create`, or `maz sync pull` to bootstrap a granted DEK)",
				zap.String("scope", scopeName))
			return nil, 0, false, nil
		}
		return dek, ep, true, nil
	}
	if mnemonic == "" {
		logger.Warn("daemon: scope opted into encryption but Watchword is absent; sealing disabled for this scope",
			zap.String("scope", scopeName))
		return nil, 0, false, nil
	}
	root, derr := watchword.DeriveRoot(mnemonic)
	if derr != nil {
		return nil, 0, false, fmt.Errorf("derive root: %w", derr)
	}
	k, kerr := watchword.DeriveScopeKey(root, scopeName)
	if kerr != nil {
		return nil, 0, false, fmt.Errorf("derive %s: %w", scopeName, kerr)
	}
	return k, 0, true, nil
}

// resolveCurrentDEK probes the keychain for the highest contiguous team DEK
// epoch (0, 1, 2, …) and returns its raw key bytes + epoch. Epochs are minted
// contiguously (genesis=0, +1 per revoke re-key), so the first miss marks the
// boundary; the last hit is the current key. Returns ok=false when epoch 0 is
// absent (no team DEK on this device yet).
func resolveCurrentDEK(ctx context.Context, secrets secret.Provider) (dek []byte, epoch int, ok bool) {
	const maxEpochProbe = 100_000 // defensive bound; real epoch counts stay tiny
	var (
		bestDEK   []byte
		bestEpoch int
		found     bool
	)
	for e := range maxEpochProbe {
		raw, err := secrets.Get(ctx, watchword.KeychainTeamDEKPrefix+strconv.Itoa(e))
		if err != nil || raw == "" {
			break // first gap ends the contiguous run
		}
		decoded, derr := base64.StdEncoding.DecodeString(raw)
		if derr != nil || len(decoded) != watchword.DEKSize {
			break // corrupt entry: treat as the boundary
		}
		bestDEK = decoded
		bestEpoch = e
		found = true
	}
	if !found {
		return nil, 0, false
	}
	return bestDEK, bestEpoch, true
}

// applyEncryptionMigration runs the encrypted-default →
// plaintext-default bridge once per
// boot. For every scope that has a remote configured + a Watchword
// available + no explicit encryption setting, it pins Encrypted=true
// into state.toml so the operator's existing chunks continue to be
// sealed rather than silently flipping to plaintext on the next
// sealer tick. Persists state.toml when changes occur. Returns the
// (possibly mutated) in-memory SyncTable so the supervisor's
// scope-key derivation sees the post-migration view.
//
// Non-fatal: any failure (missing state.toml, secret-chain error,
// write failure) is logged and the function returns the un-mutated
// syncCfg. The migration is best-effort; a failure here will simply
// re-run on the next boot and the operator can hand-edit state.toml
// in the meantime.
//

func applyEncryptionMigration(
	ctx context.Context,
	dataDir string,
	syncCfg config.SyncTable,
	secrets secret.Provider,
	logger *zap.Logger,
) config.SyncTable {
	mnemonic, err := loadWatchword(ctx, secrets)
	if err != nil {
		logger.Warn("daemon: encryption migration: load watchword failed; skipping",
			zap.Error(err))
		return syncCfg
	}
	if mnemonic == "" {
		return syncCfg
	}
	state, err := config.LoadUserState(dataDir)
	switch {
	case errors.Is(err, config.ErrUserStateNotFound):
		state = config.UserState{}
	case err != nil:
		logger.Warn("daemon: encryption migration: load state.toml failed; skipping",
			zap.Error(err))
		return syncCfg
	}
	changed := config.MigrateSyncEncryptionDefault(syncCfg, &state, true)
	if !changed {
		return syncCfg
	}
	if err := config.WriteUserState(dataDir, state); err != nil {
		logger.Warn("daemon: encryption migration: persist state.toml failed; in-memory only",
			zap.Error(err))
		// fall through — apply the in-memory migration anyway so this
		// boot's supervisor sees the encryption-preserving view.
	} else {
		logger.Info("daemon: encryption migration applied (encrypted-default → plaintext-default bridge)")
	}
	// Reflect the migration into the in-memory SyncTable too so the
	// supervisor honours it without re-loading the merged config.
	for scope, sr := range state.Sync.Remotes {
		if sr.Encrypted == nil {
			continue
		}
		base := syncCfg.Remotes[scope]
		v := *sr.Encrypted
		base.Encrypted = &v
		if syncCfg.Remotes == nil {
			syncCfg.Remotes = map[string]config.ScopeRemoteTable{}
		}
		syncCfg.Remotes[scope] = base
	}
	return syncCfg
}

// configuredScopes returns every scope (by string name) that has a
// non-empty remote URL after config + state merge. The supervisor
// uses this to decide whether to boot at all — empty result means
// "operator has not pointed any scope at a remote yet.".
func configuredScopes(sync config.SyncTable) []string {
	var out []string
	for scope, r := range sync.Remotes {
		if r.URL != "" {
			out = append(out, scope)
		}
	}
	return out
}

// collectRemoteURLs filters out blank URLs (an operator with a
// half-configured state.toml shouldn't crash the transport).
func collectRemoteURLs(s config.SyncTable) map[kernel.Scope]string {
	out := map[kernel.Scope]string{}
	for scope, r := range s.Remotes {
		if r.URL == "" {
			continue
		}
		out[kernel.Scope(scope)] = r.URL
	}
	return out
}

// hostnameOrUnknown returns the machine hostname suitable as a default
// DeviceID. Daemon wiring resolves the deviceID once at startup; the
// helper is here so the test wiring can inject a deterministic one.
func hostnameOrUnknown() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-device"
	}
	return h
}
