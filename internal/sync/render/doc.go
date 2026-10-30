// Package render is a pure, deterministic, single-threaded renderer that
// turns Mazarbul event-log events into a human-readable git-committed view.
// Given a stream of kernel.Event values in canonical order (CreateTime
// ascending, ULID tiebreak), it builds an in-memory GraphState that mirrors
// the current entity graph, then emits a View containing markdown entity
// stubs, JSONL indexes, and per-push CHANGELOG fragments -- all suitable
// for committing byte-for-byte to a scope git repo. The package makes
// hard determinism guarantees: same input produces byte-identical output
// across any number of runs, any device, any Go version within a major.
// It contains no calls to time.Now, no math/rand, no crypto/rand, and no
// goroutines. All map iterations go through sortedKeys. It sits at the same
// altitude as internal/sync/chunk and is consumed by the transport layer
// (internal/sync/transport/git/render_view.go) which decides when to write
// the View files to disk.
package render
