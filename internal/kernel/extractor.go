package kernel

import "context"

// ExtractorIface is the extraction contract the FileEventLog calls after every
// Episode Append succeeds. Implementations extract Mentions and relation
// candidates from the episode text and return the resulting events ready for
// re-Append. The caller (applyExtraction) handles timing, error isolation, and
// the detached-context pattern so extraction failures never fail the original
// Append.
//
// The returned events are appended to the event log unchanged. Each surviving
// Mention becomes an Event with Kind=="mention"; all relation candidates for
// that Episode are bundled into a single Event with Kind=="relationCandidatesProposed".
//
// Implementations must be safe for concurrent use.
type ExtractorIface interface {
	// ExtractFrom processes ev and returns zero or more new events to be
	// appended to the event log. Events with a Kind the extractor does not
	// handle (i.e., non-episode events) must return nil, nil quickly. Non-nil
	// errors are warn-logged by applyExtraction and do NOT propagate to the
	// original Append caller.
	ExtractFrom(ctx context.Context, ev Event) ([]Event, error)
}

// NoopExtractor is the default ExtractorIface. It returns nil, nil for every
// event, keeping tests that do not inject a real extractor independent of the
// ChatProvider. This is NOT installed by default -- the FileEventLog's extractor
// field stays nil and applyExtraction short-circuits on nil, so no warn path
// fires on a no-op default.
type NoopExtractor struct{}

// ExtractFrom implements ExtractorIface. Returns nil, nil for every event.
func (NoopExtractor) ExtractFrom(_ context.Context, _ Event) ([]Event, error) {
	return nil, nil
}
