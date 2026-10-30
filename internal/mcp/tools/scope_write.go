package tools

import (
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// parseWriteScope validates an optional scope string for a write. Empty
// yields "" — the event log backfills DefaultScope (personal) — so callers
// that omit scope keep today's behaviour. An unknown value is E_VALIDATION.
// The draft scope is accepted: a caller may stage a write into draft and later
// promote it via promoteDraft.
func parseWriteScope(raw string) (kernel.Scope, error) {
	if raw == "" {
		return "", nil
	}
	s := kernel.Scope(raw)
	if !s.Valid() {
		return "", mcp.NewValidationError(
			fmt.Sprintf("scope %q is not in {project|personal|team|draft}", raw), nil)
	}
	return s, nil
}

// guardTeamSelf rejects a team-scope write whose resolved endpoint is still
// the unresolved "self" sentinel. A verbatim "self" in a shared team scope
// would leak a device-local literal into a synced edge/fact instead of a
// shareable principal — so it is refused at the write boundary unless the
// self→principal binding has been provisioned (which rebinds it away from
// the sentinel before this check). Role labels the endpoint for the error.
func guardTeamSelf(scope kernel.Scope, resolvedUID, role string) error {
	if scope == kernel.ScopeTeam && resolvedUID == selfEntityUID {
		return mcp.NewValidationError(
			fmt.Sprintf("team-scope write cannot reference the unresolved \"self\" sentinel as %s; provision the self→principal binding (team genesis) first", role), nil)
	}
	return nil
}

// mapAppendError maps an event-log Append failure to a stable tool error.
// A kernel.ErrPermission (a team-scope write by a non-member) becomes
// E_PERMISSION; a transient database lock or busy contention error becomes
// E_UNAVAILABLE so callers know retry-with-backoff is appropriate; anything
// else falls back to E_INTERNAL. Action names the write for the message,
// e.g. "append relation".
func mapAppendError(action string, err error) error {
	if errors.Is(err, kernel.ErrPermission) {
		return mcp.NewPermissionError(fmt.Sprintf("%s: %v", action, err), err)
	}
	if mcp.IsTransientLockErr(err) {
		return mcp.NewUnavailableError(fmt.Sprintf("%s: %v", action, err), err)
	}
	return mcp.NewInternalError(fmt.Errorf("%s: %w", action, err))
}

// mapWriteAppendError maps an Append failure for a scope-aware write tool. A
// schema rejection (ErrInvalidEvent) is the caller's input mistake →
// E_VALIDATION; everything else defers to mapAppendError (ErrPermission →
// E_PERMISSION, transient lock → E_UNAVAILABLE, else E_INTERNAL). It names the
// two-arm idiom create_entity/document_write inline so a scope=team write by a
// non-member never mislabels as E_INTERNAL.
func mapWriteAppendError(action string, err error) error {
	if errors.Is(err, kernel.ErrInvalidEvent) {
		return mcp.NewValidationError(fmt.Sprintf("%s: %v", action, err), nil)
	}
	return mapAppendError(action, err)
}
