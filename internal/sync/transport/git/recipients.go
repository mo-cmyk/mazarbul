package git

// recipients.go — copy-up of the recipients.jsonl bootstrap artifact.
//
// For an ENCRYPTED scope push the repo carries only opaque .maz chunks
// plus MANIFEST.json. A fresh device joining the team needs the symmetric
// DEK before it can decrypt anything; recipients.jsonl is the plaintext
// bootstrap that publishes each member's wrapped DEK so they can recover
// it (see internal/sync/recipients). The daemon stages the file at
// <dataDir>/sync/<scope>/recipients.jsonl; this transport copies it into
// the repo root so commitAndPush's `git add .` commits it next to the
// chunks. The bytes are treated as OPAQUE — the transport never parses
// the DEK.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// Recipients reads the recipients.jsonl bootstrap roster published at the
// scope repo root, fast-forwarding the local mirror first so a roster a peer
// just pushed is visible. It is the pull-side counterpart of
// reconcileRecipients (the copy-up). A missing file returns (nil, nil): the
// scope is plaintext or no roster has been published yet. The wrapped DEKs in
// each row stay opaque — the transport reads the roster's envelope but never
// interprets the keys.
func (t *Transport) Recipients(ctx context.Context, scope kernel.Scope) ([]recipients.Row, error) {
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
	return recipients.Read(filepath.Join(t.scopeRepo(scope), recipients.Filename))
}

// reconcileRecipients copies the daemon-staged recipients.jsonl into the
// repo root for an encrypted scope push, or removes a stale copy on a
// plaintext push. It is the recipients sibling of writeRenderedView:
//
//   - Encrypted push: if the staged file exists, atomically copy it to
//     the repo root so `git add .` commits it next to the chunks. If it
//     is absent, do nothing (the daemon has not staged it yet).
//   - Plaintext push: recipients is an envelope-mode artifact, so it is
//     never copied. If a stale recipients.jsonl from a previous encrypted
//     push lingers at the repo root, remove it — a scope that flipped from
//     encrypted to plaintext must not leak the old wrapped-DEK roster.
func (t *Transport) reconcileRecipients(scope kernel.Scope, blobs []transport.Blob, repoDir string) error {
	dest := filepath.Join(repoDir, recipients.Filename)
	if !repoHasEncryptedChunks(blobs) {
		return removeStaleRecipients(dest)
	}
	staged := filepath.Join(t.dataDir, "sync", string(scope), recipients.Filename)
	data, err := os.ReadFile(staged) //nolint:gosec // staged path derived from dataDir + validated scope
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing staged yet — not an error
		}
		return fmt.Errorf("git transport: read staged recipients: %w", err)
	}
	if err := writeFileAtomic(dest, data); err != nil {
		return fmt.Errorf("git transport: copy recipients: %w", err)
	}
	return nil
}

// removeStaleRecipients deletes a repo-root recipients.jsonl if present.
// A missing file is a no-op. Used on plaintext pushes so a scope that
// flipped from encrypted to plaintext does not keep publishing the old
// wrapped-DEK roster.
func removeStaleRecipients(dest string) error {
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("git transport: remove stale recipients: %w", err)
	}
	return nil
}
