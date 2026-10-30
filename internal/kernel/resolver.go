package kernel

import "context"

// ResolverIface is the resolution contract the FileEventLog calls after every
// applyExtraction batch. Implementations consume the Mention and
// RelationCandidatesProposed events the extractor produced and emit canonical
// Entity / Relation / RelationRetracted / PredicateRegistered events ready
// for re-Append. The caller (applyResolution) handles timing, error isolation,
// and the detached-context pattern so resolution failures never fail the
// original Append.
//
// Implementations must be safe for concurrent use.
type ResolverIface interface {
	// ResolveFrom processes the prepared batch and returns zero or more
	// new events to be appended to the event log. Events whose Kind the
	// resolver does not handle (e.g. Episode) must contribute nothing.
	// Non-nil errors are warn-logged by applyResolution and do NOT
	// propagate to the original Append caller.
	ResolveFrom(ctx context.Context, batch []Event) ([]Event, error)
}

// NoopResolver is the default ResolverIface. It returns nil, nil for every
// batch. NOT installed by default; the FileEventLog's resolver field stays
// nil and applyResolution short-circuits.
type NoopResolver struct{}

// ResolveFrom implements ResolverIface. Returns nil, nil for every batch.
func (NoopResolver) ResolveFrom(_ context.Context, _ []Event) ([]Event, error) {
	return nil, nil
}
