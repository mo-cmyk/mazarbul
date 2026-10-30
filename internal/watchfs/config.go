// Package watchfs implements an agent-agnostic filesystem watcher: it
// observes one or more directory trees and emits a Change for every
// text-file write or delete, after honouring ignore rules, a binary
// filter, and a per-path debounce window.
//
// The package is deliberately decoupled from the memory layer. Its only
// output is a stream of Change values handed to an emit callback; the
// caller (the maz watch-fs command) is responsible for turning each
// Change into a signed remember() episode. This keeps the watcher pure
// and unit-testable without standing up the four-layer storage stack.
package watchfs

import (
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

const (
	// DefaultSizeCap is the maximum number of bytes of file content
	// captured per change. Matches the competitor's ~4 KB window: large
	// enough for substring matchability, small enough to keep the event
	// log lean.
	DefaultSizeCap = 4096

	// DefaultDebounce coalesces a burst of rapid writes to the same path
	// into a single capture. Editors emit several inotify events per save
	// (write + rename + chmod); 500 ms sits above every observed burst and
	// below human-perceptible latency.
	DefaultDebounce = 500 * time.Millisecond

	// A zero or negative size cap would capture no content; anything below
	// minSizeCap is bumped to DefaultSizeCap.
	minSizeCap = 1
)

// Options configures a Watcher. Roots is required; the remaining fields
// fall back to defaults via withDefaults when left zero.
type Options struct {
	// Roots is the set of directory trees to watch. Each is cleaned to an
	// absolute path during validation. At least one is required.
	Roots []string

	// SizeCap bounds the bytes of file content read per change. Zero means
	// DefaultSizeCap.
	SizeCap int

	// Debounce is the per-path coalescing window. Zero means
	// DefaultDebounce.
	Debounce time.Duration

	// ExtraIgnores is an optional set of additional .gitignore-syntax
	// patterns applied across every root, on top of the default denylist
	// and each root's own .gitignore.
	ExtraIgnores []string

	// Logger receives skip/error diagnostics. Nil is replaced with a
	// no-op logger.
	Logger *zap.Logger
}

// withDefaults returns a copy of o with zero fields replaced by their
// defaults. It does not validate Roots; call validate for that.
func (o Options) withDefaults() Options {
	if o.SizeCap < minSizeCap {
		o.SizeCap = DefaultSizeCap
	}
	if o.Debounce <= 0 {
		o.Debounce = DefaultDebounce
	}
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	return o
}

// validate reports the first structural problem with o. Roots must be
// non-empty; callers are expected to have resolved each root to an
// existing directory beforehand.
func (o Options) validate() error {
	if len(o.Roots) == 0 {
		return errors.New("watchfs: at least one root directory is required")
	}
	for _, r := range o.Roots {
		if r == "" {
			return fmt.Errorf("watchfs: empty root path in %v", o.Roots)
		}
	}
	return nil
}
