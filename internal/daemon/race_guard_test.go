//go:build race && darwin

package daemon_test

import (
	"fmt"
	"os"
	"testing"
)

// TestMain skips the entire daemon package when the race detector is active on
// macOS.
//
// The daemon tests open the cgo-backed storage stack — sqlite-vec AND
// go-ladybug — whose large mmap reservations collide with the race detector's
// shadow-memory layout on darwin/arm64. The runtime then aborts at startup with
// "fatal error: too many address space collisions for -race mode". This is a
// platform limitation of the race detector with these libraries, NOT a data
// race in this code: the package passes cleanly under -race on linux (full CI
// coverage is retained there) and without -race on darwin. The sister cgo
// package internal/cli runs -race fine on darwin because it never opens ladybug;
// daemon is the only package that trips the collision, so the skip is scoped to
// it alone.
//
// Skipping here keeps `go test -race ./...` (and thus `mise run ci`) green on
// the macos-latest runner while preserving every assertion on linux.
func TestMain(_ *testing.M) {
	fmt.Fprintln(os.Stderr,
		"daemon: skipping under -race on darwin (cgo mmap vs race shadow-memory collision; fully covered on linux)")
	os.Exit(0)
}
