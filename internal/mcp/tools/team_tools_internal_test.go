package tools

// Internal (white-box) test for mapTeamError, which is unexported. The
// public-facing team tool tests live in team_tools_test.go (package
// tools_test); this file exists only to pin the sentinel→code routing table,
// most importantly that an append-boundary kernel.ErrPermission surfaces as
// E_PERMISSION rather than collapsing to E_INTERNAL.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/team"
)

func TestMapTeamError_RoutesSentinelsToStableCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want mcp.ToolErrorCode
	}{
		{"append-boundary permission", kernel.ErrPermission, mcp.CodePermission},
		{"wrapped permission unwraps", fmt.Errorf("append: %w", kernel.ErrPermission), mcp.CodePermission},
		{"not a member", team.ErrNotMember, mcp.CodePermission},
		{"no team", team.ErrNoTeam, mcp.CodeNotFound},
		{"team already exists", team.ErrTeamExists, mcp.CodeValidation},
		{"self unbound", team.ErrSelfUnbound, mcp.CodeValidation},
		{"unknown fault", errors.New("disk on fire"), mcp.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var te *mcp.ToolError
			if !errors.As(mapTeamError("grant", tc.err), &te) {
				t.Fatalf("mapTeamError(%v) did not produce a *mcp.ToolError", tc.err)
			}
			if te.Code != tc.want {
				t.Errorf("mapTeamError(%v).Code = %q, want %q", tc.err, te.Code, tc.want)
			}
		})
	}
}

// TestMapTeamError_PermissionPreservesCause pins that the kernel.ErrPermission
// sentinel stays reachable through the returned ToolError, so a caller can
// errors.Is it (rather than only reading the opaque string).
func TestMapTeamError_PermissionPreservesCause(t *testing.T) {
	t.Parallel()
	got := mapTeamError("grant", kernel.ErrPermission)
	if !errors.Is(got, kernel.ErrPermission) {
		t.Errorf("mapTeamError lost the kernel.ErrPermission cause: %v", got)
	}
}
