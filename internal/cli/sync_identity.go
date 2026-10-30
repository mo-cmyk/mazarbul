package cli

// sync_identity.go backs syncidentity.Source for the daemon-less `maz sync
// push` path. The daemon resolves the commit author through the dispatcher; a
// manual push has no dispatcher, so it reads the principals projection
// (index.db) directly — the same lightweight read activity.go / digest.go
// already do for name resolution — so a CLI push authors as the same user the
// daemon would, instead of the generic "Mazarbul" sentinel. Best-effort: when
// the index cannot be opened, identity degrades to git-config + sentinel.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/syncidentity"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
)

// syncCLIDeviceID tags commits/presence from a manual `maz sync push`, kept
// distinct from a paired device's own id (the daemon stamps the real device).
const syncCLIDeviceID = "maz-sync-cli"

// pushIdentity carries the resolved commit author + co-author resolver from
// runSyncPush into each pushScope call, so a daemon-less push authors commits as
// the real user (bring-your-own-git) rather than the Mazarbul sentinel.
type pushIdentity struct {
	resolved         syncidentity.Resolved
	selfPrincipalUID string
	coAuthors        gittransport.CoAuthorResolver
}

// resolvePushIdentity resolves the commit identity once for a push and returns
// it with a closer for the backing index. It mirrors the daemon's resolution
// (shared syncidentity chain) but reads the principals projection directly,
// since a manual push has no live dispatcher. Best-effort throughout: a missing
// index or principal degrades to git-config, then the Mazarbul sentinel.
func resolvePushIdentity(ctx context.Context, dataDir string, syncCfg config.SyncTable) (*pushIdentity, func()) {
	// Non-fatal: an unreadable self-principal degrades to git-config + sentinel
	// (the daemon path logs a warning here, but the CLI has no logger).
	selfPrincipalUID, _ := config.SelfPrincipalUID(dataDir) //nolint:errcheck
	src, closeSrc := newCLIIdentitySource(ctx, dataDir, nil)
	return &pushIdentity{
		resolved:         syncidentity.Resolve(ctx, syncCfg.Identity, syncCLIDeviceID, selfPrincipalUID, src, nil),
		selfPrincipalUID: selfPrincipalUID,
		coAuthors:        syncidentity.CoAuthorResolverOrNil(src),
	}, closeSrc
}

// cliIdentitySource implements syncidentity.Source over a read-only principals
// index. A nil store (index absent / unopenable) makes every lookup miss, which
// the resolver treats as "no bound principal" and falls back accordingly.
type cliIdentitySource struct {
	store *sqlite.Store
}

// newCLIIdentitySource opens index.db best-effort and returns the source plus a
// closer. The closer is always safe to call (a no-op when nothing was opened).
func newCLIIdentitySource(ctx context.Context, dataDir string, logger *zap.Logger) (*cliIdentitySource, func()) {
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "index.db"))
	if err != nil {
		if logger != nil {
			logger.Debug("sync push: open index for commit identity", zap.Error(err))
		}
		return &cliIdentitySource{}, func() {}
	}
	return &cliIdentitySource{store: store}, func() {
		if cerr := store.Close(); cerr != nil && logger != nil {
			logger.Debug("sync push: close identity index", zap.Error(cerr))
		}
	}
}

// PrincipalGitIdentity reads the principal's recorded GitHub identity. The
// github_login column stores the anchor "github:<login>"; the prefix is
// stripped so the synthesized no-reply email is the real "<id>+<login>@...".
func (s *cliIdentitySource) PrincipalGitIdentity(ctx context.Context, principalUID string) (login string, userID int64, displayName string, ok bool, err error) {
	if s.store == nil || principalUID == "" {
		return "", 0, "", false, nil
	}
	var (
		l  sql.NullString
		id sql.NullInt64
		dn sql.NullString
	)
	qerr := s.store.Reader().QueryRowContext(ctx,
		`SELECT github_login, github_user_id, display_name FROM principals WHERE principal_uid = ?`,
		principalUID).Scan(&l, &id, &dn)
	if errors.Is(qerr, sql.ErrNoRows) {
		return "", 0, "", false, nil
	}
	if qerr != nil {
		return "", 0, "", false, fmt.Errorf("query principal git identity: %w", qerr)
	}
	return strings.TrimPrefix(l.String, ontology.GitHubAnchorPrefix), id.Int64, dn.String, true, nil
}

// CoAuthor renders a peer principal's "Name <noreply-email>" line, mirroring the
// daemon's resolver so CLI and daemon pushes emit identical Co-authored-by lines.
func (s *cliIdentitySource) CoAuthor(ctx context.Context, principalUID string) (string, bool) {
	login, userID, displayName, ok, err := s.PrincipalGitIdentity(ctx, principalUID)
	if err != nil || !ok {
		return "", false
	}
	email := identity.NoreplyEmail(login, userID)
	if email == "" {
		return "", false
	}
	name := displayName
	if name == "" {
		name = identity.SanitizeGitHubLogin(login)
	}
	if name == "" {
		return "", false
	}
	return name + " <" + email + ">", true
}
