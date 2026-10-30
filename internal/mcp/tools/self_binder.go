package tools

import "context"

// selfBinder resolves the device-local self->principal binding so a write
// against the "self" sentinel persists the concrete principal UID — which
// syncs cleanly across a team — rather than the unshareable local literal
// (which would collapse every teammate's first-person data on sync).
// Satisfied by *config.SelfBinder. A nil binder disables rebinding, which
// preserves pre-identity behavior (the literal "self" is written verbatim).
type selfBinder interface {
	SelfPrincipal(ctx context.Context) (string, bool)
}

// rebindSelf maps the "self" sentinel to the bound principal UID when a binder
// is configured and the device is bound. Every other uid — and the unbound
// case (no Watchword / genesis deferred) — passes through unchanged, so the
// "self" literal remains a valid local subject until identity is provisioned.
func rebindSelf(ctx context.Context, b selfBinder, uid string) string {
	if uid != selfEntityUID || b == nil {
		return uid
	}
	if principalUID, ok := b.SelfPrincipal(ctx); ok {
		return principalUID
	}
	return uid
}
