package syncidentity_test

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/sync/syncidentity"
)

// fakeSource is a deterministic syncidentity.Source for the precedence tests;
// it never touches a dispatcher or a projection.
type fakeSource struct {
	login       string
	userID      int64
	displayName string
	ok          bool
}

func (f fakeSource) PrincipalGitIdentity(_ context.Context, _ string) (string, int64, string, bool, error) {
	return f.login, f.userID, f.displayName, f.ok, nil
}

func (f fakeSource) CoAuthor(_ context.Context, _ string) (string, bool) {
	if !f.ok || f.login == "" {
		return "", false
	}
	return f.displayName + " <" + identity.NoreplyEmail(f.login, f.userID) + ">", true
}

func TestResolve_Precedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	withPrincipal := fakeSource{login: "octocat", userID: 99, displayName: "Octo Cat", ok: true}

	t.Run("explicit override wins", func(t *testing.T) {
		t.Parallel()
		got := syncidentity.Resolve(ctx,
			&config.SyncIdentityTable{CommitIdentity: "noreply", Name: "Pinned", Email: "pin@example.com"},
			"dev-1", "principal-1", withPrincipal, zap.NewNop())
		if got.Name != "Pinned" || got.Email != "pin@example.com" {
			t.Errorf("override = %+v; want Pinned <pin@example.com>", got)
		}
	})

	t.Run("noreply synthesizes from principal", func(t *testing.T) {
		t.Parallel()
		got := syncidentity.Resolve(ctx, &config.SyncIdentityTable{}, "dev-1", "principal-1", withPrincipal, zap.NewNop())
		if got.Name != "Octo Cat" || got.Email != "99+octocat@users.noreply.github.com" || got.Mode != "noreply" {
			t.Errorf("noreply = %+v; want Octo Cat <99+octocat@...> mode noreply", got)
		}
	})

	t.Run("noreply falls to login when display empty", func(t *testing.T) {
		t.Parallel()
		src := fakeSource{login: "octocat", userID: 99, ok: true}
		got := syncidentity.Resolve(ctx, &config.SyncIdentityTable{CommitIdentity: "noreply"}, "dev-1", "principal-1", src, zap.NewNop())
		if got.Name != "octocat" {
			t.Errorf("name = %q; want login fallback octocat", got.Name)
		}
	})

	t.Run("anonymous is the sentinel", func(t *testing.T) {
		t.Parallel()
		got := syncidentity.Resolve(ctx, &config.SyncIdentityTable{CommitIdentity: "anonymous"}, "dev-xyz", "principal-1", withPrincipal, zap.NewNop())
		if got.Name != "Mazarbul" || got.Email != "mazarbul@dev-xyz" || got.Mode != "anonymous" {
			t.Errorf("anonymous = %+v; want Mazarbul <mazarbul@dev-xyz>", got)
		}
	})

	t.Run("noreply with no principal and nil source degrades safely", func(t *testing.T) {
		t.Parallel()
		// No bound principal and no resolver: the chain falls through to git
		// config and finally the sentinel. We can't assert the exact identity
		// (it depends on the host's git config), but it must never be empty and
		// must keep mode=noreply.
		got := syncidentity.Resolve(ctx, nil, "dev-2", "", nil, zap.NewNop())
		if got.Name == "" || got.Email == "" {
			t.Errorf("degraded identity must not be empty, got %+v", got)
		}
		if got.Mode != "noreply" {
			t.Errorf("mode = %q; want noreply preserved through fallback", got.Mode)
		}
	})
}

func TestNormalizeCommitMode(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":          config.CommitIdentityNoreply,
		"noreply":   config.CommitIdentityNoreply,
		"real":      config.CommitIdentityReal,
		"anonymous": config.CommitIdentityAnonymous,
		"bogus":     config.CommitIdentityNoreply,
	}
	for in, want := range cases {
		if got := syncidentity.NormalizeCommitMode(in); got != want {
			t.Errorf("NormalizeCommitMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCoAuthorResolverOrNil(t *testing.T) {
	t.Parallel()
	if r := syncidentity.CoAuthorResolverOrNil(nil); r != nil {
		t.Errorf("nil source must yield a nil resolver (not a typed-nil), got %v", r)
	}
	src := fakeSource{login: "sam", userID: 5, displayName: "Sam", ok: true}
	r := syncidentity.CoAuthorResolverOrNil(src)
	if r == nil {
		t.Fatalf("non-nil source must yield a resolver")
	}
	if author, ok := r.CoAuthor(context.Background(), "p"); !ok || author != "Sam <5+sam@users.noreply.github.com>" {
		t.Errorf("CoAuthor = %q ok=%v; want Sam <5+sam@...>", author, ok)
	}
}
