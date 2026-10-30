package identity

import (
	"fmt"
	"strings"
)

// NoreplyEmail synthesizes the GitHub no-reply commit email for an account.
//
// The id-prefixed form (e.g. "12345+octocat@users.noreply.github.com") is
// exactly what GitHub itself emits when a user has "keep my email address
// private" enabled: a commit authored with it renders AS the user — avatar,
// profile link, and contribution graph — while never exposing the real email
// to clones or scrapers. Because the immutable numeric id leads, the binding
// survives a login rename.
//
// A zero (unknown) userID falls back to the legacy "<login>@users.noreply.
// Github.com" form, which GitHub still attributes by login. An empty login
// yields "" so the caller can fall through to the next identity source.
func NoreplyEmail(login string, userID int64) string {
	login = SanitizeGitHubLogin(login)
	if login == "" {
		return ""
	}
	if userID > 0 {
		return fmt.Sprintf("%d+%s@users.noreply.github.com", userID, login)
	}
	return login + "@users.noreply.github.com"
}

// SanitizeGitHubLogin strips a login down to the characters GitHub permits
// (ASCII letters, digits, and hyphen), guarding the synthesized address
// against an adversarial display value smuggling an "@" or whitespace into the
// commit author line. Returns "" when nothing valid remains.
func SanitizeGitHubLogin(login string) string {
	login = strings.TrimSpace(login)
	var b strings.Builder
	b.Grow(len(login))
	for _, r := range login {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}
