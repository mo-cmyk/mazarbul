// Package ladybug wraps the LadybugDB CGO bindings behind the kernel's
// vendor-neutral GraphConn contract. Only this package imports
// github.com/LadybugDB/go-ladybug; the rest of the kernel sees the
// kernel.GraphConn and kernel.GraphResult interfaces only.
//
// LadybugDB is the maintained continuation of Kùzu after Apple acquired and
// archived the upstream project in October 2025. File format, Cypher
// dialect, and columnar engine carry over unchanged. See
// agent-docs/tech/ladybug.md for the per-platform build requirements.
// The binding is CGO (single binary, no subprocess).
//
// The local dynamic library is not bundled in the Go module proxy; run
// `mise run setup-graph` once per worktree to install it into the Go module
// cache before `go build` or `go test`.
package ladybug

import "github.com/mo-cmyk/mazarbul/internal/kernel"

// Compile-time assertions that Conn / result satisfy the kernel contracts.
// Wiring drift surfaces here at build time rather than at the first call.
var (
	_ kernel.GraphConn   = (*Conn)(nil)
	_ kernel.GraphResult = (*result)(nil)
)
