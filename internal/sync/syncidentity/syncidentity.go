// Package syncidentity resolves the git author identity the sync transport
// stamps on every commit — the "bring your own git" surface (Workstream A).
//
// Both the daemon supervisor and the `maz sync push` CLI resolve through this
// one chain, so a manual push attributes to the same user the daemon would
// (rather than the generic Mazarbul sentinel). Precedence, highest wins:
//
//  1. explicit [sync.identity] name + email override (verbatim escape hatch);
//  2. the bound principal's recorded GitHub identity, rendered per the
//     commit_identity mode (noreply synthesizes the no-reply email; real uses
//     the operator's git-config email; anonymous is the Mazarbul sentinel);
//  3. the operator's git config user.name / user.email;
//  4. the Mazarbul <mazarbul@device> sentinel (headless / no identity).
//
// Resolution never fails: an unresolvable identity degrades to the sentinel so
// a headless or pre-genesis device still pushes.
package syncidentity

import (
	"context"
	"os/exec"
	"strings"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
)

// Source supplies the bound principal's recorded GitHub identity. The daemon
// backs it with the dispatcher; the CLI backs it with a read-only principals
// index. A nil Source degrades resolution to git-config + sentinel, so callers
// without projection access still resolve a real author from git config.
type Source interface {
	PrincipalGitIdentity(ctx context.Context, principalUID string) (login string, userID int64, displayName string, ok bool, err error)
	CoAuthor(ctx context.Context, principalUID string) (author string, ok bool)
}

// CoAuthorResolverOrNil adapts the identity Source onto the transport's
// CoAuthorResolver, preserving a genuine nil interface when there is no Source
// so the transport emits no Co-authored-by lines (rather than carrying a
// typed-nil that would panic on call).
func CoAuthorResolverOrNil(src Source) gittransport.CoAuthorResolver {
	if src == nil {
		return nil
	}
	return src
}

// Resolved is the git author identity the sync transport stamps, plus the mode
// label surfaced in the commit trailer and the doctor check.
type Resolved struct {
	Name  string
	Email string
	Mode  string
}

// Resolve applies the precedence chain and returns the author identity to
// stamp. It never fails: an unresolvable identity degrades to the Mazarbul
// sentinel so a headless or pre-genesis device still pushes.
func Resolve(
	ctx context.Context,
	idCfg *config.SyncIdentityTable,
	deviceID, selfPrincipalUID string,
	src Source,
	logger *zap.Logger,
) Resolved {
	// An absent [sync.identity] block defaults to noreply mode with no override.
	if idCfg == nil {
		idCfg = &config.SyncIdentityTable{}
	}
	mode := NormalizeCommitMode(idCfg.CommitIdentity)

	// 1. Explicit verbatim override wins outright.
	if idCfg.Name != "" && idCfg.Email != "" {
		return Resolved{Name: idCfg.Name, Email: idCfg.Email, Mode: mode}
	}

	sentinel := Resolved{Name: "Mazarbul", Email: "mazarbul@" + deviceID, Mode: mode}

	// 2. Principal-derived identity, per mode.
	switch mode {
	case config.CommitIdentityAnonymous:
		return sentinel
	case config.CommitIdentityNoreply:
		if id, ok := resolveNoreplyIdentity(ctx, selfPrincipalUID, src); ok {
			id.Mode = mode
			return id
		}
	case config.CommitIdentityReal:
		if id, ok := resolveRealIdentity(ctx, selfPrincipalUID, src); ok {
			id.Mode = mode
			return id
		}
	}

	// 3. git config fallback.
	if name, email, ok := gitConfigIdentity(ctx); ok {
		if logger != nil {
			logger.Debug("sync identity from git config", zap.String("mode", mode))
		}
		return Resolved{Name: name, Email: email, Mode: mode}
	}

	// 4. Sentinel.
	return sentinel
}

// NormalizeCommitMode maps the configured value to a known mode, defaulting an
// empty or unrecognized value to noreply (the recommended bring-your-own-git
// default).
func NormalizeCommitMode(m string) string {
	switch m {
	case config.CommitIdentityReal, config.CommitIdentityAnonymous:
		return m
	default:
		return config.CommitIdentityNoreply
	}
}

// resolveNoreplyIdentity renders the bound principal as a GitHub no-reply
// identity. Returns ok=false when there is no bound principal, no resolver, or
// the principal has no recorded GitHub login (the caller then falls through).
func resolveNoreplyIdentity(ctx context.Context, selfPrincipalUID string, src Source) (Resolved, bool) {
	if selfPrincipalUID == "" || src == nil {
		return Resolved{}, false
	}
	login, userID, displayName, ok, err := src.PrincipalGitIdentity(ctx, selfPrincipalUID)
	if err != nil || !ok {
		return Resolved{}, false
	}
	email := identity.NoreplyEmail(login, userID)
	if email == "" {
		return Resolved{}, false
	}
	name := displayName
	if name == "" {
		name = identity.SanitizeGitHubLogin(login)
	}
	if name == "" {
		return Resolved{}, false
	}
	return Resolved{Name: name, Email: email}, true
}

// resolveRealIdentity uses the operator's real git-config email (the principals
// projection records no real address) with the principal's display name when
// available. Returns ok=false when no git-config email exists.
func resolveRealIdentity(ctx context.Context, selfPrincipalUID string, src Source) (Resolved, bool) {
	gname, gemail, ok := gitConfigIdentity(ctx)
	if !ok {
		return Resolved{}, false
	}
	name := gname
	if selfPrincipalUID != "" && src != nil {
		if _, _, displayName, found, err := src.PrincipalGitIdentity(ctx, selfPrincipalUID); err == nil && found && displayName != "" {
			name = displayName
		}
	}
	if name == "" {
		name = gname
	}
	return Resolved{Name: name, Email: gemail}, true
}

// gitConfigIdentity reads the operator's configured user.name / user.email.
// Both must be present for the result to be usable.
func gitConfigIdentity(ctx context.Context) (name, email string, ok bool) {
	name = runGitConfigGet(ctx, "user.name")
	email = runGitConfigGet(ctx, "user.email")
	return name, email, name != "" && email != ""
}

// runGitConfigGet shells out to `git config --get <key>`, returning the trimmed
// value or "" on any error (git absent, key unset). Best-effort by design.
func runGitConfigGet(ctx context.Context, key string) string {
	out, err := exec.CommandContext(ctx, "git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
