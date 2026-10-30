// Package git implements transport.Transport over a standard git
// remote (GitHub, Gitea, GitLab, self-hosted, file://). The remote
// holds one bare repository per scope; chunks live as ordinary files
// at <scope>/chunks/<chunkID>.maz. Pushes append one commit per call;
// pulls fast-forward the local mirror.
//
// The transport never decrypts a chunk: it pushes and pulls raw bytes
// only. Authentication is the caller's responsibility — set up SSH
// keys or HTTPS PATs in the user environment; this package shells
// out to the system `git` binary so any credential helper that works
// on the command line works here too.
//
// On-disk layout (local mirror, one per scope):
//
//	<dataDir>/sync/<scope>/repo/        — bare git mirror
//	<dataDir>/sync/<scope>/chunks/      — checked-out working tree
//
// The mirror is rebuilt from the configured remote on first Pull and
// kept up to date in place after that.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// Transport is the git-backed implementation of transport.Transport.
// One instance serves all scopes; each scope is mapped to its own
// local-mirror directory under dataDir.
type Transport struct {
	dataDir    string
	remoteURLs map[kernel.Scope]string // scope -> upstream URL
	authorName string
	authorMail string
	deviceID   string
	// selfPrincipalUID is the principal the committing device authors as; its
	// events are excluded from the Co-authored-by trailer (you don't co-author
	// with yourself), and it labels the Principal trailer.
	selfPrincipalUID string
	// commitMode is the resolved commit_identity mode (noreply|real|anonymous),
	// surfaced in the Mazarbul-Identity trailer and the doctor check.
	commitMode string
	// coAuthors resolves a peer principal UID to a "Name <email>" line for the
	// Co-authored-by trailer. Nil disables co-author attribution (the trailer is
	// simply omitted) — e.g. a test wiring or a daemon with no dispatcher.
	coAuthors CoAuthorResolver

	// mirrorMu serializes all git operations on a given scope's local mirror.
	// The daemon runs a push loop and a consume loop against the same mirror
	// concurrently; without this, two `git fetch`es race on
	// refs/remotes/origin/main ("cannot lock ref") and the failed fast-forward
	// wedges the mirror, so the loser can neither push its chunk nor pull the
	// peer's — a permanent split-brain. Each public mirror-touching method
	// (Push, List, Get, Presence, Recipients) holds the per-scope lock for its
	// whole duration; none call each other, so the plain Mutex never re-enters.
	locksMu     sync.Mutex
	mirrorLocks map[kernel.Scope]*sync.Mutex
}

// mirrorLock returns the per-scope mutex guarding scope's local mirror,
// creating it on first use. Callers hold it around every git invocation that
// reads or mutates the mirror so concurrent daemon loops never collide on the
// git ref lock.
func (t *Transport) mirrorLock(scope kernel.Scope) *sync.Mutex {
	t.locksMu.Lock()
	defer t.locksMu.Unlock()
	if t.mirrorLocks == nil {
		t.mirrorLocks = make(map[kernel.Scope]*sync.Mutex)
	}
	mu, ok := t.mirrorLocks[scope]
	if !ok {
		mu = &sync.Mutex{}
		t.mirrorLocks[scope] = mu
	}
	return mu
}

// CoAuthorResolver maps a principal UID to a git author "Name <email>" line for
// the Co-authored-by trailer. The daemon supplies a dispatcher-backed
// implementation; a miss (ok=false) suppresses the trailer line for that UID so
// a malformed author string never reaches the commit.
type CoAuthorResolver interface {
	CoAuthor(ctx context.Context, principalUID string) (author string, ok bool)
}

// Options configures a new git Transport. RemoteURLs maps each scope
// to the upstream git URL (one URL per scope; team/personal/project
// each push to their own dedicated remote). AuthorName/AuthorMail
// stamp git commits so the audit trail shows which user authored them.
type Options struct {
	DataDir    string
	RemoteURLs map[kernel.Scope]string
	AuthorName string
	AuthorMail string
	DeviceID   string
	// SelfPrincipalUID is the bound principal the committing device authors as.
	SelfPrincipalUID string
	// CommitIdentityMode is the resolved authorship mode for the
	// Mazarbul-Identity trailer + doctor reporting (noreply|real|anonymous).
	CommitIdentityMode string
	// CoAuthors resolves peer principal UIDs to author lines; optional.
	CoAuthors CoAuthorResolver
}

// New constructs a Transport. DataDir must already exist; per-scope
// mirror directories are created lazily on first use. Returns an
// error if no remote URLs are configured (the transport is useless
// without at least one).
func New(opts Options) (*Transport, error) {
	if opts.DataDir == "" {
		return nil, errors.New("git transport: data dir is required")
	}
	if len(opts.RemoteURLs) == 0 {
		return nil, errors.New("git transport: at least one scope remote URL is required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git transport: git binary not in PATH: %w", err)
	}
	author := opts.AuthorName
	if author == "" {
		author = "Mazarbul"
	}
	mail := opts.AuthorMail
	if mail == "" {
		mail = "mazarbul@localhost"
	}
	device := opts.DeviceID
	if device == "" {
		device = "unknown-device"
	}
	mode := opts.CommitIdentityMode
	if mode == "" {
		mode = "anonymous"
	}
	urls := make(map[kernel.Scope]string, len(opts.RemoteURLs))
	for k, v := range opts.RemoteURLs {
		urls[k] = v
	}
	return &Transport{
		dataDir:          opts.DataDir,
		remoteURLs:       urls,
		authorName:       author,
		authorMail:       mail,
		deviceID:         device,
		selfPrincipalUID: opts.SelfPrincipalUID,
		commitMode:       mode,
		coAuthors:        opts.CoAuthors,
	}, nil
}

// Name returns a stable identifier listing the configured remotes.
func (t *Transport) Name() string {
	parts := make([]string, 0, len(t.remoteURLs))
	for scope, url := range t.remoteURLs {
		parts = append(parts, string(scope)+"="+url)
	}
	sort.Strings(parts)
	return "git:[" + strings.Join(parts, ",") + "]"
}

// scopeRepo returns the local repository working tree for scope.
func (t *Transport) scopeRepo(scope kernel.Scope) string {
	return filepath.Join(t.dataDir, "sync", string(scope), "repo")
}

// scopeChunksDir returns the working-tree directory inside the repo
// where chunk blobs live as files.
func (t *Transport) scopeChunksDir(scope kernel.Scope) string {
	return filepath.Join(t.scopeRepo(scope), "chunks")
}

// remoteURL returns the upstream URL for scope, or ErrNotConfigured.
func (t *Transport) remoteURL(scope kernel.Scope) (string, error) {
	url, ok := t.remoteURLs[scope]
	if !ok || url == "" {
		return "", fmt.Errorf("%w: scope=%s", transport.ErrNotConfigured, scope)
	}
	return url, nil
}

// ensureRepo initialises the local mirror for scope if missing. The
// first call clones from the remote; subsequent calls verify that
// origin still points at the configured URL so a remote-URL change
// in state.toml takes effect on the next push instead of silently
// pushing to the previous remote.
func (t *Transport) ensureRepo(ctx context.Context, scope kernel.Scope) error {
	repo := t.scopeRepo(scope)
	url, err := t.remoteURL(scope)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
		return t.reconcileOriginURL(ctx, repo, url)
	}
	if err := os.MkdirAll(filepath.Dir(repo), 0o700); err != nil {
		return fmt.Errorf("git transport: mkdir %s: %w", filepath.Dir(repo), err)
	}
	// Try clone; if the remote is empty (no commits yet), fall back
	// to init + remote-add so the first push from this device creates
	// the initial commit.
	if _, _, err := t.run(ctx, "", "clone", "--quiet", url, repo); err != nil {
		if cloneEmpty(err) {
			return t.initEmptyMirror(ctx, scope, repo, url)
		}
		return fmt.Errorf("%w: clone %s: %w", transport.ErrTransport, url, err)
	}
	if err := t.configureIdentity(ctx, repo); err != nil {
		return err
	}
	return nil
}

// reconcileOriginURL updates the local mirror's `origin` URL when
// state.toml has been changed to point at a different remote. Without
// this, a remote-URL change would be silently ignored because the
// .git directory already exists and ensureRepo would short-circuit.
// `git remote get-url` reads the current value; we only run `set-url`
// when it differs so the no-op case stays a single read.
func (t *Transport) reconcileOriginURL(ctx context.Context, repo, want string) error {
	current, _, err := t.run(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		// "no such remote" → fall through to set-url, which doubles as
		// "add" via git's --add flag below. Any other error escalates.
		current = ""
	}
	if strings.TrimSpace(current) == want {
		return nil
	}
	if _, _, err := t.run(ctx, repo, "remote", "set-url", "origin", want); err != nil {
		// remote may not exist yet (init-only mirror that never pushed);
		// retry with `add`.
		if _, _, addErr := t.run(ctx, repo, "remote", "add", "origin", want); addErr != nil {
			return fmt.Errorf("%w: remote set-url %s: %w", transport.ErrTransport, want, err)
		}
	}
	return nil
}

// initEmptyMirror builds a fresh local repo for a brand-new remote.
// The remote-add is necessary so the eventual Push has somewhere to
// go; the configure-identity step keeps commits attributable.
func (t *Transport) initEmptyMirror(ctx context.Context, _ kernel.Scope, repo, url string) error {
	if err := os.MkdirAll(repo, 0o700); err != nil {
		return fmt.Errorf("git transport: mkdir %s: %w", repo, err)
	}
	if _, _, err := t.run(ctx, repo, "init", "--quiet", "--initial-branch=main"); err != nil {
		return fmt.Errorf("%w: init %s: %w", transport.ErrTransport, repo, err)
	}
	if _, _, err := t.run(ctx, repo, "remote", "add", "origin", url); err != nil {
		return fmt.Errorf("%w: remote add %s: %w", transport.ErrTransport, url, err)
	}
	return t.configureIdentity(ctx, repo)
}

// configureIdentity sets the per-repo user.name / user.email so the
// commits this transport authors are attributable. Running these per
// repo avoids leaking the device identity into the user's global
// gitconfig.
func (t *Transport) configureIdentity(ctx context.Context, repo string) error {
	if _, _, err := t.run(ctx, repo, "config", "user.name", t.authorName); err != nil {
		return fmt.Errorf("git transport: config user.name: %w", err)
	}
	if _, _, err := t.run(ctx, repo, "config", "user.email", t.authorMail); err != nil {
		return fmt.Errorf("git transport: config user.email: %w", err)
	}
	return nil
}

// maxPushRetries is the total number of commit-and-push attempts before
// giving up with ErrPushContention. 8 means 1 initial attempt + 7 retries.
// With the per-scope mirror lock removing intra-process fetch collisions, a
// losing pusher reliably fast-forwards onto the winner and re-commits; the
// larger budget lets a small team outlast a burst of mutual contention rather
// than abandoning a chunk (the split-brain root cause) after only a few tries.
const maxPushRetries = 8

// maxPushRetryDelay caps the exponential back-off so a late retry never stalls
// the push tick for longer than one coalesce window.
const maxPushRetryDelay = 2 * time.Second

// pushRetryDelay returns the back-off duration before the n-th retry (n is
// zero-indexed). It uses FULL jitter (AWS "Exponential Backoff and Jitter"):
// sample the whole [0, ceil) window where ceil = 100ms·2^n capped at
// maxPushRetryDelay, rather than ceil plus a small fixed jitter. Two devices
// that collide start their back-offs together; a small fixed jitter leaves them
// in near-lockstep so they keep re-colliding until the retry budget exhausts
// (the observed livelock). Sampling the whole window desymmetrizes them — one
// backs off far less, wins the push, and the loser then fast-forwards onto it
// and lands cleanly within budget instead of stranding its chunk.
//
//	retry 0 → [0,100ms), retry 1 → [0,200ms), retry 2 → [0,400ms), … capped at 2s
func pushRetryDelay(retry int) time.Duration {
	ceilMS := 100 << retry
	if maxMS := int(maxPushRetryDelay / time.Millisecond); ceilMS > maxMS {
		ceilMS = maxMS
	}
	return time.Duration(rand.IntN(ceilMS)) * time.Millisecond
}

// Push writes every blob into the scope mirror and pushes one commit
// to origin. Blobs whose ChunkID already exists in the working tree
// are skipped (idempotent re-push). Empty pushes are no-ops.
func (t *Transport) Push(ctx context.Context, scope kernel.Scope, blobs []transport.Blob) (int, error) {
	if !scope.Valid() {
		return 0, fmt.Errorf("git transport: invalid scope %q", scope)
	}
	if len(blobs) == 0 {
		return 0, nil
	}
	mu := t.mirrorLock(scope)
	mu.Lock()
	defer mu.Unlock()
	// Cross-process guard: a `maz sync push` CLI invocation opens its own
	// Transport (with its own in-process mirrorLock) against this same on-disk
	// mirror. Without an interprocess lock, one process's destructive recovery
	// (resetMirrorToRemote's `git clean -fd chunks/`, rollbackForRetry's
	// `git checkout -- .`) can delete the other's staged-but-uncommitted chunk
	// mid-push. The loser yields; the daemon re-offers its chunks next tick.
	flock, err := t.tryAcquireMirrorFileLock(scope)
	if err != nil {
		if errors.Is(err, errMirrorBusy) {
			return 0, fmt.Errorf("%w: %w: another process holds the %s mirror lock",
				transport.ErrTransport, transport.ErrPushContention, scope)
		}
		return 0, err
	}
	defer func() {
		if cerr := flock.Close(); cerr != nil {
			_ = cerr // best-effort release; the OS drops the flock on exit anyway
		}
	}()
	if err := t.ensureRepo(ctx, scope); err != nil {
		return 0, err
	}
	// Pull-before-commit so this push does not race a sibling device.
	// Without this, a second device commits on a stale parent and the
	// push gets rejected as non-fast-forward; the unpushed local
	// commit then makes every subsequent List / Get / Presence fail
	// the ff-only merge in fastForward, wedging the mirror until an
	// operator hand-repairs the repo.
	if err := t.fastForward(ctx, scope); err != nil {
		return 0, err
	}
	chunksDir, err := t.prepareScopeWorkingTree(scope)
	if err != nil {
		return 0, err
	}

	tally, freshHeads, err := t.materializeBlobs(blobs, chunksDir)
	if err != nil {
		return tally.total(), err
	}
	if tally.total() == 0 {
		return 0, nil
	}
	summary, err := t.preparePushSummary(ctx, scope, tally, freshHeads, chunksDir)
	if err != nil {
		return tally.total(), err
	}

	// Commit + push with bounded NFF retry, then hard-reset recovery on
	// exhaustion (see pushWithRetry for the full rationale).
	if err := t.pushWithRetry(ctx, scope, blobs, chunksDir, summary); err != nil {
		return tally.total(), err
	}
	return tally.total(), nil
}

// pushWithRetry runs the bounded commit/push loop with exponential backoff. Up
// to maxPushRetries total attempts; only non-fast-forward rejections
// (concurrent pushers) trigger a retry — any other failure (permissions,
// network, bad object) fails fast to avoid masking real errors. PushRetryStep
// rebases the mirror onto the winning pusher's commit between attempts, and
// writeRepoMetadata regenerates the MANIFEST + rendered view from the merged
// chunk set each attempt so the committed metadata matches the post-merge chunk
// files. Returns nil on a successful push, or the terminal error to surface.
func (t *Transport) pushWithRetry(ctx context.Context, scope kernel.Scope, blobs []transport.Blob, chunksDir string, summary pushSummary) error {
	var lastPushErr error
	for attempt := range maxPushRetries {
		if attempt > 0 {
			if err := t.pushRetryStep(ctx, scope, attempt); err != nil {
				return err
			}
		}
		if err := t.writeRepoMetadata(scope, blobs, chunksDir); err != nil {
			return err
		}
		lastPushErr = t.commitAndPush(ctx, scope, summary)
		if lastPushErr == nil {
			return nil
		}
		if !isNonFastForward(lastPushErr) {
			return lastPushErr // fail fast on non-NFF errors
		}
	}
	// All attempts exhausted. Hard-reset the mirror to origin/main so the next
	// Push re-materializes and re-commits the blobs onto the current remote head.
	// A plain soft rollback is NOT enough: it leaves the chunk staged-but-
	// uncommitted on disk, and the next Push's materializeBlobs skips any blob
	// already on disk (errBlobExists) → tally 0 → early return WITHOUT committing,
	// so the chunk is stranded forever (the permanent split-brain the real-binary
	// run found). The blobs are safe in the caller's FSStore (the source of
	// truth); resetMirrorToRemote discards only the uncommitted mirror copy so
	// the daemon's next re-offer lands them. A reset failure is surfaced (not
	// swallowed) so an un-recovered mirror is visible rather than silently
	// wedged — ErrPushContention semantics are preserved either way.
	if rbErr := t.resetMirrorToRemote(ctx, scope); rbErr != nil {
		return fmt.Errorf("%w: %w: push rejected %d times and mirror reset failed: %w",
			transport.ErrTransport, transport.ErrPushContention, maxPushRetries, rbErr)
	}
	return fmt.Errorf("%w: %w: push rejected %d times; another pusher is consistently ahead",
		transport.ErrTransport, transport.ErrPushContention, maxPushRetries)
}

// pushRetryStep prepares the local mirror for the next push attempt after
// a non-fast-forward rejection: rolls back the uncommitted local commit,
// fast-forwards to the winning device's HEAD, then sleeps the exponential
// back-off delay (100ms·2^(attempt-1) + jitter). Returns any error that
// should abort the retry loop immediately (rollback failure, ff failure,
// or context cancellation).
func (t *Transport) pushRetryStep(ctx context.Context, scope kernel.Scope, attempt int) error {
	// Undo the previous commit (chunk files remain on disk as untracked) and
	// clear the derived metadata so fastForward can advance to the winning
	// device's commit; writeRepoMetadata regenerates the metadata afterward.
	if err := t.rollbackForRetry(ctx, scope); err != nil {
		return fmt.Errorf("%w: rollback after rejected push: %w", transport.ErrTransport, err)
	}
	if err := t.fastForward(ctx, scope); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: context cancelled during push retry: %w", transport.ErrTransport, ctx.Err())
	case <-time.After(pushRetryDelay(attempt - 1)):
		return nil
	}
}

// preparePushSummary builds the structured commit summary (commit message +
// authorship). The repo artefacts (README, MANIFEST, rendered view, recipients)
// are written per-attempt in the Push retry loop by writeRepoMetadata, NOT here:
// a retry fast-forwards onto a concurrent pusher's commit, so the MANIFEST and
// rendered view must be regenerated from the merged chunk set each attempt.
// Extracted from Push to keep its cyclomatic complexity within the gocyclo=15 gate.
func (t *Transport) preparePushSummary(ctx context.Context, scope kernel.Scope, tally pushTally, freshHeads []chunk.Header, chunksDir string) (pushSummary, error) {
	summary, err := buildPushSummary(scope, t.deviceID, t.selfPrincipalUID, tally, freshHeads, chunksDir)
	if err != nil {
		return pushSummary{}, err
	}
	t.applyAuthorship(ctx, &summary)
	return summary, nil
}

// prepareScopeWorkingTree ensures the scope's chunks dir exists and runs
// the #204 pre-flight sensitivity scan BEFORE any blob is materialized.
// The ordering is load-bearing. The render guard inside writeRenderedView
// fires only AFTER materializeBlobs has written the fresh safe blobs; if
// the working tree already holds a sensitive plaintext chunk (pulled from
// a remote, or sealed before a marker existed), that later abort would
// leave the safe blobs uncommitted in the worktree, and the next retry
// would idempotently skip them (tally.total()==0) and falsely report
// success — stranding those events. Refusing here writes nothing and
// makes a retry re-run the same check. The scan runs for every non-project
// push, encrypted or plaintext: commitAndPush's `git add .` would stage a
// pre-existing cleartext sensitive chunk regardless of the new blobs' mode.
// Returns the chunks dir for the caller to materialize into.
func (t *Transport) prepareScopeWorkingTree(scope kernel.Scope) (string, error) {
	chunksDir := t.scopeChunksDir(scope)
	if err := os.MkdirAll(chunksDir, 0o700); err != nil {
		return "", fmt.Errorf("git transport: mkdir %s: %w", chunksDir, err)
	}
	if err := refuseSensitiveWorkingTree(scope, chunksDir); err != nil {
		return "", err
	}
	return chunksDir, nil
}

// writeRepoMetadata stamps the three repo-root artefacts that make the
// scope repo self-describing: README (first push only), .gitattributes
// (first push only), and MANIFEST.json (every push, since the chunk
// set changes). Extracted from Push so the orchestration there stays
// under the gocyclo cap.
func (t *Transport) writeRepoMetadata(scope kernel.Scope, blobs []transport.Blob, chunksDir string) error {
	repoDir := t.scopeRepo(scope)
	if _, err := newReadmeWriter(t.deviceID).ensureREADME(repoDir, scope, repoHasEncryptedChunks(blobs)); err != nil {
		return err
	}
	if _, err := ensureGitattributes(repoDir); err != nil {
		return err
	}
	recs, err := listChunkHeaders(chunksDir)
	if err != nil {
		return err
	}
	if err := writeManifest(repoDir, recs); err != nil {
		return err
	}
	if err := t.writeRenderedView(scope, blobs, repoDir, chunksDir); err != nil {
		return err
	}
	// Copy up (encrypted) or prune (plaintext) the recipients.jsonl
	// bootstrap so a fresh device joining an encrypted scope can recover
	// its wrapped DEK. Runs as a sibling of writeRenderedView; the bytes
	// are opaque to the transport.
	if err := t.reconcileRecipients(scope, blobs, repoDir); err != nil {
		return err
	}
	// Copy up (team) or prune (non-team) the TEAM.json genesis trust anchor.
	// This is a no-op for non-genesis devices (nothing staged) and for all
	// non-team scopes (file removed if stale).
	return t.reconcileTeamJSON(scope == kernel.ScopeTeam, repoDir)
}

// pushTally counts accepted blobs split by mode. Both fields are
// always set; buildCommitMessage decides what to surface in the
// subject. The semantic counts (episodes, entities, …) live on
// commitDetails in commit_message.go — pushTally is the transport-
// level view, kept intentionally narrow so the privacy boundary
// (encrypted blobs never decode here) lives in the bridge, not in
// the tally itself.
type pushTally struct {
	plaintext int
	encrypted int
}

func (p pushTally) total() int { return p.plaintext + p.encrypted }

// commitAndPush runs the local `git add chunks && git commit && git
// push origin HEAD:main` sequence with the per-call commit identity
// forced via env. The commit message is the structured form built by
// buildCommitMessage — subject ≤72 chars for git log --oneline, body
// enumerating event-level changes in plaintext mode, body suppressed
// in encrypted mode by the privacy contract. Returns ErrTransport-
// wrapped errors that callers inspect with isNonFastForward.
func (t *Transport) commitAndPush(ctx context.Context, scope kernel.Scope, summary pushSummary) error {
	repo := t.scopeRepo(scope)
	// "add ." picks up chunks/ AND the repo-root files we just wrote
	// (README.md on first push, MANIFEST.json every push, .gitattributes
	// on first push). The chunksDir is part of the working tree so it
	// is covered by the same recursive add.
	if _, _, err := t.run(ctx, repo, "add", "."); err != nil {
		return fmt.Errorf("%w: add: %w", transport.ErrTransport, err)
	}
	msg := buildCommitMessage(summary)
	commitEnv := []string{
		"GIT_AUTHOR_NAME=" + t.authorName,
		"GIT_AUTHOR_EMAIL=" + t.authorMail,
		"GIT_COMMITTER_NAME=" + t.authorName,
		"GIT_COMMITTER_EMAIL=" + t.authorMail,
	}
	if _, _, err := t.runWithEnv(ctx, repo, commitEnv, "commit", "--quiet", "--allow-empty-message", "-m", msg); err != nil {
		return fmt.Errorf("%w: commit: %w", transport.ErrTransport, err)
	}
	if _, _, err := t.run(ctx, repo, "push", "--quiet", "origin", "HEAD:main"); err != nil {
		return fmt.Errorf("%w: push: %w", transport.ErrTransport, err)
	}
	return nil
}

// applyAuthorship stamps the resolved commit identity onto the summary and
// resolves this push's distinct peer principals into Co-authored-by lines.
// Author/committer (the git author line) is set separately via env in
// commitAndPush; this fills only the trailer-side fields. Peer resolution is
// best-effort: a nil resolver or an unnamed principal simply contributes no
// Co-authored-by line (a bare UID is never emitted — it would be a malformed
// author string).
func (t *Transport) applyAuthorship(ctx context.Context, s *pushSummary) {
	s.PrincipalName = t.authorName
	s.PrincipalEmail = t.authorMail
	s.CommitMode = t.commitMode
	if t.coAuthors == nil || len(s.peerPrincipalUIDs) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(s.peerPrincipalUIDs))
	for _, uid := range s.peerPrincipalUIDs {
		author, ok := t.coAuthors.CoAuthor(ctx, uid)
		if !ok || author == "" {
			continue
		}
		if _, dup := seen[author]; dup {
			continue
		}
		seen[author] = struct{}{}
		s.CoAuthors = append(s.CoAuthors, author)
	}
	sort.Strings(s.CoAuthors)
}

// rollbackLastCommit resets HEAD back one commit while keeping the
// working-tree changes (the chunks are still on disk; the materialise
// step is idempotent so the next commitAndPush picks them up). Used
// after a non-fast-forward push rejection so the next fastForward
// merge has a clean linear history to land on.
func (t *Transport) rollbackLastCommit(ctx context.Context, scope kernel.Scope) error {
	repo := t.scopeRepo(scope)
	if _, _, err := t.run(ctx, repo, "reset", "--soft", "HEAD~1"); err != nil {
		return fmt.Errorf("git transport: reset: %w", err)
	}
	return nil
}

// rollbackForRetry undoes the previous rejected attempt's commit while leaving
// the fresh chunk files on disk (untracked) and reverting the derived metadata
// (MANIFEST, rendered view, recipients, …) to the parent. This is load-bearing:
// a plain `reset --soft` leaves those regenerated metadata files STAGED, which
// makes the following `git merge --ff-only` refuse ("your local changes would be
// overwritten by merge"). FastForward swallows that failure, so the pusher never
// advances onto the winner and every attempt is re-rejected until the retry
// budget exhausts — the contention livelock behind the permanent split-brain.
// With a clean tracked tree the ff-merge lands; the Push loop's writeRepoMetadata
// then regenerates the metadata from the merged chunk set before the next commit.
// The chunk blobs (untracked new files) are never overwritten by a fast-forward,
// so they survive to be re-committed onto the winner's head.
func (t *Transport) rollbackForRetry(ctx context.Context, scope kernel.Scope) error {
	repo := t.scopeRepo(scope)
	// --mixed moves HEAD back and unstages, but keeps the working tree: the new
	// chunk files become untracked (preserved), the derived metadata stays as
	// modified tracked files.
	if _, _, err := t.run(ctx, repo, "reset", "--mixed", "HEAD~1"); err != nil {
		return fmt.Errorf("git transport: reset --mixed: %w", err)
	}
	// Revert the tracked metadata modifications to the parent so the ff-merge is
	// not blocked; the untracked chunk files are left untouched.
	if _, _, err := t.run(ctx, repo, "checkout", "--", "."); err != nil {
		return fmt.Errorf("git transport: checkout to clear metadata: %w", err)
	}
	return nil
}

// resetMirrorToRemote discards the mirror's local commits + staged/working
// changes and hard-resets to origin/main. Used on the push-exhaustion path so a
// contended push that could not land leaves the mirror clean at the remote head
// instead of stranding a staged-but-uncommitted chunk that the next Push's
// materializeBlobs would skip (errBlobExists → tally 0 → no commit). The chunk
// blobs are safe in the caller's FSStore (the source of truth); the next Push
// re-materializes and re-commits them onto the current parent. Best-effort: on
// an empty remote (no origin/main yet) it falls back to the soft rollback so we
// never error on the first-push path.
func (t *Transport) resetMirrorToRemote(ctx context.Context, scope kernel.Scope) error {
	repo := t.scopeRepo(scope)
	if _, _, err := t.run(ctx, repo, "rev-parse", "--verify", "--quiet", "origin/main"); err != nil {
		return t.rollbackLastCommit(ctx, scope)
	}
	if _, _, err := t.run(ctx, repo, "reset", "--hard", "origin/main"); err != nil {
		return fmt.Errorf("git transport: reset --hard origin/main: %w", err)
	}
	// A hard reset drops staged/tracked changes; clean removes any purely-
	// untracked chunk files the failed attempt materialized so the working tree
	// matches the remote exactly.
	if _, _, err := t.run(ctx, repo, "clean", "-fd", "--", "chunks/"); err != nil {
		return fmt.Errorf("git transport: clean chunks: %w", err)
	}
	return nil
}

// isNonFastForward reports whether err is a retryable concurrent-push
// rejection from git — a sibling device pushed at the same time, so a
// rebase-and-retry can win. Genuine races phrase themselves several
// ways by git version and remote type: "(non-fast-forward)" and
// "(fetch first)" for a stale local ref, and "cannot lock ref" /
// "failed to update ref" for a lost atomic ref update against a shared
// (e.g. file://) remote. Match those markers explicitly rather than a
// bare "rejected": a permanent server-side refusal ("! [remote
// rejected] main -> main (pre-receive hook declined)", a
// protected-branch hook, or a permission denial) also contains
// "rejected" but must fail fast. Treating it as a race retries to
// exhaustion, resets the mirror, and surfaces ErrPushContention while
// the operator loses the real remote refusal reason.
func isNonFastForward(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "cannot lock ref") ||
		strings.Contains(msg, "failed to update ref")
}

// materializeBlobs writes each blob to chunksDir under its ChunkID
// filename. Existing files are skipped so a re-push after a partial
// failure does not double-commit. Returns the per-mode tally of NEW
// files written and the headers of those fresh writes (ordered by
// ChunkID, which equals seal-time ULID order). The freshHeads slice
// feeds buildPushSummary so the commit message tallies match the
// files git just staged. WriteBlob defers closeBody so every blob's
// reader is released on every path (success, already-on-disk, error).
func (t *Transport) materializeBlobs(blobs []transport.Blob, chunksDir string) (pushTally, []chunk.Header, error) {
	var tally pushTally
	fresh := make([]chunk.Header, 0, len(blobs))
	for _, b := range blobs {
		if err := writeBlob(b, chunksDir); errors.Is(err, errBlobExists) {
			continue
		} else if err != nil {
			return tally, fresh, err
		}
		if b.Header.Encrypted {
			tally.encrypted++
		} else {
			tally.plaintext++
		}
		fresh = append(fresh, b.Header)
	}
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].ChunkID < fresh[j].ChunkID })
	return tally, fresh, nil
}

var errBlobExists = errors.New("git transport: blob already on disk")

// errSensitiveBlobPlaintext is the transport-level mirror of
// chunk.ErrSensitivePayloadPlaintext: a plaintext blob carrying a
// sensitive event reached the push boundary by a path that bypassed the
// seal-time gate (a chunk pulled from the remote, or sealed before a
// marker existed). Block it — never write the file, so materializeBlobs
// aborts the push before commitAndPush and nothing lands in git.
// ErrTransport-wrapped so callers classify it with the other transport
// failures.
var errSensitiveBlobPlaintext = fmt.Errorf(
	"%w: refuse to write sensitive payload into a plaintext chunk", transport.ErrTransport)

// guardedBlobBody returns the reader writeBlob copies to disk. An
// encrypted or project-scoped blob streams b.Body unchanged: an
// encrypted payload cannot be decoded, and project is the intentionally
// public, unencryptable scope. A plaintext, non-project blob is buffered
// and decoded; if any event trips a privacy marker the push is refused
// (errSensitiveBlobPlaintext) — the defence-in-depth mirror of the
// seal-time gate. Buffering also means the body is read exactly once.
func guardedBlobBody(b transport.Blob) (io.Reader, error) {
	if b.Header.Encrypted || b.Header.Scope == kernel.ScopeProject {
		return b.Body, nil
	}
	buf, err := io.ReadAll(b.Body)
	if err != nil {
		return nil, fmt.Errorf("git transport: read blob: %w", err)
	}
	_, events, derr := chunk.DecodePlaintext(buf, b.Header.Scope)
	if derr != nil {
		return nil, fmt.Errorf("git transport: decode plaintext blob: %w", derr)
	}
	for i := range events {
		if privacy.IsSensitivePayload(events[i]) {
			return nil, fmt.Errorf("%w: event uid=%s scope=%q",
				errSensitiveBlobPlaintext, events[i].UID, b.Header.Scope)
		}
	}
	return bytes.NewReader(buf), nil
}

// extensionFor returns the on-disk filename extension to use for
// hdr: .maz for encrypted chunks, .jsonl for plaintext.
func extensionFor(hdr chunk.Header) string {
	if hdr.Encrypted {
		return chunk.Extension
	}
	return chunk.ExtensionJSONL
}

// siblingPath returns the existing chunk path for chunkID in
// chunksDir under either extension, or "" if neither exists.
// Used to make idempotency mode-agnostic.
func siblingPath(chunksDir, chunkID string) string {
	for _, ext := range []string{chunk.Extension, chunk.ExtensionJSONL} {
		path := filepath.Join(chunksDir, chunkID+ext)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// isChunkFilename reports whether name has one of the recognised
// chunk-file extensions. Mirrors the FSStore helper of the same
// name so the transport listing and the local store stay in sync
// about what counts as a chunk.
func isChunkFilename(name string) bool {
	return strings.HasSuffix(name, chunk.Extension) || strings.HasSuffix(name, chunk.ExtensionJSONL)
}

// validChunkID is the regexp matched against every ChunkID before it
// is joined into a filesystem path. ULIDs are exactly 26 characters
// from Crockford's base32 alphabet (excluding I, L, O, U). Rejecting
// anything else defends against a hostile remote header that could
// otherwise smuggle "../" or "/.." through Get and read/write
// outside the scope chunks directory.
var validChunkID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func validateChunkID(id string) error {
	if !validChunkID.MatchString(id) {
		return fmt.Errorf("git transport: invalid chunk id %q (must be a 26-char Crockford base32 ULID)", id)
	}
	return nil
}

func writeBlob(b transport.Blob, chunksDir string) error {
	defer closeBody(b.Body)
	if err := validateChunkID(b.Header.ChunkID); err != nil {
		return err
	}
	final := filepath.Join(chunksDir, b.Header.ChunkID+extensionFor(b.Header))
	// Treat either extension as "already present" so an idempotent
	// re-push of a chunk that landed under the opposite mode (e.g.,
	// the operator toggled encryption mid-stream and re-ran a manual
	// sync push) does not duplicate.
	if existing := siblingPath(chunksDir, b.Header.ChunkID); existing != "" {
		return errBlobExists
	}
	if _, err := os.Stat(final); err == nil {
		return errBlobExists
	}
	body, err := guardedBlobBody(b)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(chunksDir, ".tmp-blob-*")
	if err != nil {
		return fmt.Errorf("git transport: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			_ = rmErr // best-effort cleanup; partial-write detritus is not recoverable here
		}
	}
	if _, err := io.Copy(tmp, body); err != nil {
		closeTmp(tmp)
		cleanup()
		return fmt.Errorf("git transport: copy blob: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		closeTmp(tmp)
		cleanup()
		return fmt.Errorf("git transport: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("git transport: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		cleanup()
		return fmt.Errorf("git transport: rename: %w", err)
	}
	return nil
}

func closeTmp(f *os.File) {
	if cerr := f.Close(); cerr != nil {
		_ = cerr // best-effort close on the error path
	}
}

func closeBody(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	if cerr := rc.Close(); cerr != nil {
		_ = cerr // best-effort close; the body is being discarded
	}
}

// List fetches the remote and returns the headers of every chunk
// currently checked into the scope mirror.
func (t *Transport) List(ctx context.Context, scope kernel.Scope) (transport.Listing, error) {
	if !scope.Valid() {
		return transport.Listing{}, fmt.Errorf("git transport: invalid scope %q", scope)
	}
	mu := t.mirrorLock(scope)
	mu.Lock()
	defer mu.Unlock()
	if err := t.ensureRepo(ctx, scope); err != nil {
		return transport.Listing{}, err
	}
	if err := t.fastForward(ctx, scope); err != nil {
		return transport.Listing{}, err
	}
	chunksDir := t.scopeChunksDir(scope)
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return transport.Listing{Scope: scope}, nil
		}
		return transport.Listing{}, fmt.Errorf("git transport: readdir %s: %w", chunksDir, err)
	}
	heads := make([]chunk.Header, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isChunkFilename(e.Name()) {
			continue
		}
		path := filepath.Join(chunksDir, e.Name())
		blob, rerr := os.ReadFile(path) //nolint:gosec // path bounded by ReadDir
		if rerr != nil {
			return transport.Listing{}, fmt.Errorf("git transport: read %s: %w", path, rerr)
		}
		hdr, perr := chunk.PeekHeader(blob)
		if perr != nil {
			// A single unreadable chunk on the remote must not brick the whole
			// pull listing. Skip a pre-0x02 MAZ1 chunk (ErrBadVersion — a
			// deliberate clean-cut) or a stray non-chunk file (ErrBadMagic);
			// since such a chunk is never returned here it is never selected
			// for Get, so the consumer simply does not replay it. Genuine
			// corruption of a valid chunk still surfaces.
			if errors.Is(perr, chunk.ErrBadVersion) || errors.Is(perr, chunk.ErrBadMagic) {
				continue
			}
			return transport.Listing{}, fmt.Errorf("git transport: peek %s: %w", path, perr)
		}
		// Defend against a hostile remote whose chunk header carries
		// a path-traversal ChunkID. The filename was already
		// validated by ReadDir + extension check, but Headers carry
		// their own ChunkID that downstream callers pass back to Get;
		// reject anything that does not look like a canonical ULID
		// before it can be used as a path component.
		if err := validateChunkID(hdr.ChunkID); err != nil {
			return transport.Listing{}, fmt.Errorf("git transport: chunk %s: %w", path, err)
		}
		heads = append(heads, hdr)
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].ChunkID < heads[j].ChunkID })
	return transport.Listing{
		Scope:         scope,
		Chunks:        heads,
		ContentHashes: readManifestContentHashes(t.scopeRepo(scope)),
	}, nil
}

// Get returns the chunk blob for the named chunk in scope. EnsureRepo
// + fast-forward keeps the mirror current first so a freshly pushed
// chunk on the remote is visible.
func (t *Transport) Get(ctx context.Context, scope kernel.Scope, chunkID string) (transport.Blob, error) {
	if err := validateChunkID(chunkID); err != nil {
		return transport.Blob{}, err
	}
	if !scope.Valid() {
		return transport.Blob{}, fmt.Errorf("git transport: invalid scope %q", scope)
	}
	mu := t.mirrorLock(scope)
	mu.Lock()
	defer mu.Unlock()
	if err := t.ensureRepo(ctx, scope); err != nil {
		return transport.Blob{}, err
	}
	if err := t.fastForward(ctx, scope); err != nil {
		return transport.Blob{}, err
	}
	chunksDir := t.scopeChunksDir(scope)
	path := siblingPath(chunksDir, chunkID)
	if path == "" {
		return transport.Blob{}, fmt.Errorf("%w: chunk %s not present in scope %s mirror", transport.ErrTransport, chunkID, scope)
	}
	blob, err := os.ReadFile(path) //nolint:gosec // path validated by siblingPath against the scope chunks dir
	if err != nil {
		return transport.Blob{}, fmt.Errorf("git transport: read %s: %w", path, err)
	}
	hdr, err := chunk.PeekHeader(blob)
	if err != nil {
		return transport.Blob{}, fmt.Errorf("git transport: peek %s: %w", path, err)
	}
	return transport.Blob{Header: hdr, Body: io.NopCloser(bytes.NewReader(blob))}, nil
}

// Presence reads commit authors from recent history. Each unique
// (author-name, author-email) pair becomes one Device; LastSeenAt is
// the most recent commit by that author.
func (t *Transport) Presence(ctx context.Context, scope kernel.Scope) ([]transport.Device, error) {
	if !scope.Valid() {
		return nil, fmt.Errorf("git transport: invalid scope %q", scope)
	}
	mu := t.mirrorLock(scope)
	mu.Lock()
	defer mu.Unlock()
	if err := t.ensureRepo(ctx, scope); err != nil {
		return nil, err
	}
	if err := t.fastForward(ctx, scope); err != nil {
		return nil, err
	}
	repo := t.scopeRepo(scope)
	// Read the full commit body (%B) alongside author + timestamp so we can
	// prefer the stable Device:/Principal: trailers over the author line, which
	// now changes with the bring-your-own-git identity. Fields are 0x1f-
	// separated, records 0x1e-separated, so a multi-line body never collides
	// with the field/record delimiters.
	out, _, err := t.run(ctx, repo, "log", "-n", "200", "--pretty=format:%at\x1f%an\x1f%ae\x1f%B\x1e")
	if err != nil {
		// Empty repo (no commits yet) is "no presence" rather than an
		// error — the operator has paired but nobody has pushed.
		if strings.Contains(err.Error(), "does not have any commits yet") {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: log: %w", transport.ErrTransport, err)
	}
	devices := make(map[string]transport.Device, 4)
	for _, rec := range strings.Split(out, "\x1e") {
		dev, ok := parsePresenceRecord(rec)
		if !ok {
			continue
		}
		if existing, seen := devices[dev.ID]; !seen || existing.LastSeenAt.Before(dev.LastSeenAt) {
			devices[dev.ID] = dev
		}
	}
	out2 := make([]transport.Device, 0, len(devices))
	for _, d := range devices {
		out2 = append(out2, d)
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].LastSeenAt.After(out2[j].LastSeenAt) })
	return out2, nil
}

// parsePresenceRecord turns one `git log` record (ts \x1f name \x1f email \x1f
// body) into a Device. Identity keys on the Device: trailer when present (the
// stable device tag, unchanged by the author-name switch), then the Principal:
// trailer, then the legacy author name|email — so a repo mixing old "Mazarbul"
// commits with new bring-your-own-git commits still de-duplicates correctly.
// The display name prefers the Principal: trailer's name over the author line.
func parsePresenceRecord(rec string) (transport.Device, bool) {
	fields := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x1f", 4)
	if len(fields) != 4 {
		return transport.Device{}, false
	}
	secs, err := parseUnix(fields[0])
	if err != nil {
		return transport.Device{}, false
	}
	name, mail, body := fields[1], fields[2], fields[3]
	device := trailerValue(body, "Device:")
	principal := trailerValue(body, "Principal:")
	key := name + "|" + mail
	switch {
	case device != "":
		key = "device:" + device
	case principal != "":
		key = "principal:" + principal
	}
	displayName := name
	if principal != "" {
		displayName = principalDisplayName(principal)
	}
	return transport.Device{ID: key, Name: displayName, LastSeenAt: time.Unix(secs, 0).UTC()}, true
}

// trailerValue returns the value of the first "<prefix> value" trailer line in
// body, or "" when absent. Prefix includes the trailing colon (e.g. "Device:").
func trailerValue(body, prefix string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if rest, found := strings.CutPrefix(line, prefix); found {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// principalDisplayName extracts the human name from a "Name <email>" Principal
// trailer value, falling back to the whole value when it carries no email.
func principalDisplayName(principal string) string {
	if i := strings.LastIndex(principal, " <"); i > 0 {
		return principal[:i]
	}
	return principal
}

// fastForward runs git fetch + merge --ff-only to keep the local mirror in
// step with the remote. Networkless or no-remote scenarios surface as
// ErrTransport-wrapped errors.
//
// Force-push detection: after a successful fetch and BEFORE the merge, the function checks the
// maz-last-remote-head marker file for a previously recorded remote HEAD. If
// that HEAD is no longer an ancestor of origin/main (force-push detected) it
// returns ErrHistoryRewritten immediately, leaving the mirror untouched.
// Recovery is explicit operator action: `maz sync repair --scope <scope>`.
// After every successful merge the new remote HEAD is persisted to the marker
// for the next call's comparison.
func (t *Transport) fastForward(ctx context.Context, scope kernel.Scope) error {
	repo := t.scopeRepo(scope)
	if _, _, err := t.run(ctx, repo, "fetch", "--quiet", "origin"); err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			// Remote exists but has no commits yet — treat as no-op.
			return nil
		}
		return fmt.Errorf("%w: fetch: %w", transport.ErrTransport, err)
	}

	// Pre-merge ancestry check: return ErrHistoryRewritten immediately if
	// the recorded remote HEAD is no longer an ancestor of origin/main.
	markerPath := t.remoteHeadMarkerPath(scope)
	oldHead := readRemoteHeadMarker(markerPath)
	newHead := t.resolveRemoteHead(ctx, repo)

	if rwErr := t.checkHistoryRewrite(ctx, repo, oldHead, newHead, scope); rwErr != nil {
		return rwErr
	}

	if _, _, err := t.run(ctx, repo, "merge", "--ff-only", "--quiet", "origin/main"); err != nil {
		// merge --ff-only fails when there is nothing to merge or the
		// branch does not exist yet; both are recoverable.
		if strings.Contains(err.Error(), "Not possible to fast-forward") ||
			strings.Contains(err.Error(), "refusing to merge unrelated histories") {
			// Defense-in-depth: distinguish a force-push wedge from a normal
			// local divergence. checkHistoryRewrite swallows isHistoryRewritten
			// errors (git errors = unknown = false), so a transient git failure
			// surfaces the generic merge error rather than a false positive.
			if rwErr := t.checkHistoryRewrite(ctx, repo, oldHead, newHead, scope); rwErr != nil {
				return rwErr
			}
			return fmt.Errorf("%w: merge: %w", transport.ErrTransport, err)
		}
		// Empty remote = nothing to merge; not an error.
	}

	// Persist the new remote HEAD so the next fastForward can detect a
	// force-push between now and then.
	if newHead != "" {
		if wErr := writeRemoteHeadMarker(markerPath, newHead); wErr != nil {
			// Non-fatal: a marker write failure means we lose detection on
			// the next call, but the current merge succeeded. Log the error
			// via the return-value channel but don't fail the caller.
			_ = wErr // best-effort persistence
		}
	}
	return nil
}

// remoteHeadMarkerPath returns the path to the per-scope marker file that
// records the last successfully merged remote HEAD SHA. The file lives
// inside .git/ so it is excluded from working-tree commits and is deleted
// atomically along with the mirror directory during `maz sync repair`.
func (t *Transport) remoteHeadMarkerPath(scope kernel.Scope) string {
	return filepath.Join(t.scopeRepo(scope), ".git", "maz-last-remote-head")
}

// resolveRemoteHead returns the full SHA of origin/main after a fetch, or ""
// when the ref does not exist (empty remote) or the command fails.
func (t *Transport) resolveRemoteHead(ctx context.Context, repo string) string {
	sha, _, err := t.run(ctx, repo, "rev-parse", "origin/main")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(sha)
}

// isHistoryRewritten runs `git merge-base --is-ancestor <oldHead> origin/main`
// and reports whether oldHead is NOT an ancestor of origin/main (exit code 1 =
// force-push detected). Exit 0 means history is intact; any other non-zero exit
// code (e.g. bad object name, git error) is treated as "unknown" (false, nil)
// so a transient git failure does not block the fast-forward.
func (t *Transport) isHistoryRewritten(ctx context.Context, repo, oldHead string) (bool, error) {
	_, _, err := t.run(ctx, repo, "merge-base", "--is-ancestor", oldHead, "origin/main")
	if err == nil {
		return false, nil // oldHead IS ancestor → history intact
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil // NOT ancestor → history was rewritten
	}
	// Other error (bad SHA, git binary issue) — cannot confirm rewrite.
	return false, nil
}

// checkHistoryRewrite runs the ancestry check and returns a formatted
// ErrHistoryRewritten when the previously recorded remote HEAD (oldHead) is
// no longer an ancestor of the current origin/main (newHead). Returns nil
// when either head is unknown, when history is intact, or when the underlying
// git command errors — errors are treated as "unknown" rather than false
// positives so a transient git failure does not wedge the fast-forward path.
func (t *Transport) checkHistoryRewrite(ctx context.Context, repo, oldHead, newHead string, scope kernel.Scope) error {
	if oldHead == "" || newHead == "" {
		return nil
	}
	rewritten, _ := t.isHistoryRewritten(ctx, repo, oldHead) //nolint:errcheck // error = unknown; only exit 1 confirms rewrite — see isHistoryRewritten godoc
	if !rewritten {
		return nil
	}
	return fmt.Errorf(
		"%w: old head %s is not an ancestor of origin/main %s (scope %s); run `maz sync repair --scope %s` to recover",
		transport.ErrHistoryRewritten, shortSHA(oldHead), shortSHA(newHead), scope, scope)
}

// readRemoteHeadMarker returns the SHA stored in the marker file, or ""
// when the file is absent or unreadable. The value is whitespace-trimmed.
func readRemoteHeadMarker(path string) string {
	raw, err := os.ReadFile(path) //nolint:gosec // path is <repo>/.git/maz-last-remote-head, constructed internally
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// writeRemoteHeadMarker atomically persists sha to path via a tmp-then-rename
// so a mid-write crash does not corrupt the marker.
func writeRemoteHeadMarker(path, sha string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".maz-rh-*")
	if err != nil {
		return fmt.Errorf("marker: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			_ = rmErr // best-effort; partial-write detritus is not recoverable here
		}
	}
	if _, werr := fmt.Fprintln(tmp, sha); werr != nil {
		closeTmp(tmp)
		cleanup()
		return fmt.Errorf("marker: write: %w", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		cleanup()
		return fmt.Errorf("marker: close: %w", cerr)
	}
	if rerr := os.Rename(tmpPath, path); rerr != nil {
		cleanup()
		return fmt.Errorf("marker: rename: %w", rerr)
	}
	return nil
}

// shortSHA returns the first 7 characters of a commit SHA for use in
// human-readable error messages. Handles strings shorter than 7 chars.
func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}

// run executes `git <args>` in dir, returning stdout and stderr. The
// caller wraps the returned error with transport-specific context.
func (t *Transport) run(ctx context.Context, dir string, args ...string) (string, string, error) {
	return t.runWithEnv(ctx, dir, nil, args...)
}

// runWithEnv runs git with extra environment variables appended on
// top of the daemon's environment. Used by the commit path to force
// GIT_AUTHOR_* / GIT_COMMITTER_* explicitly — per-repo `git config
// user.name` is silently overridden when these env vars are set in
// the parent process, so we set them here and overwrite any leakage.
func (t *Transport) runWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Disable any interactive prompting from credential helpers so a
	// missing credential surfaces as an error instead of blocking the
	// daemon goroutine forever.
	//
	// GIT_CONFIG_GLOBAL / GIT_CONFIG_SYSTEM = /dev/null and GIT_CEILING_DIRECTORIES
	// prevent test runs from walking up to a developer's parent repo and writing
	// `[remote "origin"]` or `core.worktree` into it (the source of the historical
	// pollution we cleaned in this PR). Production daemons running as their own
	// user still see their own global/system config because the daemon is invoked
	// outside `go test`; this only fires through this transport's per-call env.
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CEILING_DIRECTORIES="+dir,
	)
	env = append(env, extraEnv...)
	// Strip every repo-location variable that could redirect git away from
	// cmd.Dir. GIT_DIR / GIT_WORK_TREE are the obvious pair, but GIT_INDEX_FILE
	// is the dangerous omission: `git commit` exports it (an absolute path to
	// the live index) to its hooks, so a `go test` run from a pre-commit hook
	// inherits it and every `git add` / `git commit` this transport issues
	// writes into the developer's real index instead of the test's temp repo —
	// silently corrupting the working repository. GIT_CEILING_DIRECTORIES does
	// not guard against these: they are explicit absolute paths, not a parent
	// directory walk. Dropping them all makes cmd.Dir the sole source of truth.
	env = filterEnv(env,
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX",
	)
	cmd.Env = env
	err := cmd.Run()
	if err != nil {
		return stdout.String(), stderr.String(),
			fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}

// filterEnv returns env with any KEY=VALUE entries whose key matches one
// of the provided keys removed. Used by runWithEnv to drop GIT_DIR /
// GIT_WORK_TREE leaks from a parent shell so cmd.Dir is the only thing
// that controls which repository git touches.
func filterEnv(env []string, keys ...string) []string {
	if len(keys) == 0 {
		return env
	}
	out := env[:0]
	for _, kv := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// cloneEmpty reports whether err is the "remote has no commits yet"
// message git emits on cloning an empty bare repo. Used to fall back
// to init + remote-add instead of failing the first Push.
func cloneEmpty(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "You appear to have cloned an empty repository") ||
		strings.Contains(msg, "remote HEAD refers to nonexistent ref") ||
		strings.Contains(msg, "warning: You appear to have cloned an empty repository")
}

func parseUnix(s string) (int64, error) {
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, fmt.Errorf("git transport: parse unix timestamp %q: %w", s, err)
	}
	return v, nil
}
