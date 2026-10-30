// Package hlc implements the Kulkarni hybrid logical clock (HLC) used by the
// Mazarbul kernel to assign a convergent total order to all events.
//
// # Encoding
//
// Each stamp is 16 lowercase hex characters:
//
//	<wall-ms: 12 hex = 48 bits><logical: 4 hex = 16 bits>
//
// Both sub-fields are zero-padded big-endian integers, so lexicographic order
// equals numeric order for valid stamps. A 48-bit wall covers millisecond
// timestamps up to year 10889, which is ample.
//
// # Algorithm (Kulkarni et al. "Logical Physical Clocks and Consistent
// Snapshots in Globally Distributed Databases", 2014)
//
// Local event: wall' = max(now_ms, last.wall); logical = last.logical+1 if
// wall' unchanged, else 0.
//
// Observe remote: advance local state to max(local, capped_remote), where the
// cap is now+maxSkew. A far-future remote stamp never drags the local clock
// beyond now+maxSkew; the remote event still sorts at its own stamp everywhere.
//
// Implicit fallback for legacy events (no HLC field): Encode(t.UnixMilli(), 0),
// a pure function of the immutable CreateTime. Two machines independently derive
// the same implicit stamp for the same event, keeping cross-machine order
// convergent for pre-wave logs.
package hlc

import (
	"fmt"
	"sync"
	"time"
)

// Clock is a mutex-guarded hybrid logical clock. The zero value is ready to
// use: it starts at Unix epoch wall = 0, logical = 0.
type Clock struct {
	mu      sync.Mutex
	wall    int64  // ms since Unix epoch
	logical uint16 // 0–65535 monotonic tiebreaker within the same ms
}

// Now returns a fresh HLC stamp for a new local event and advances the clock
// state:
//
//	wall' = max(now.UnixMilli(), c.wall)
//	logical = c.logical+1  if wall' == c.wall
//	logical = 0            otherwise
//
// Overflow of the logical counter (at 65535) is not clamped: it wraps to zero.
// In practice this requires 65536 events within the same millisecond, which
// is not a realistic load for this system.
func (c *Clock) Now(now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := now.UnixMilli()
	if w < c.wall {
		w = c.wall
	}
	if w == c.wall {
		c.logical++
	} else {
		c.wall = w
		c.logical = 0
	}
	return Encode(c.wall, c.logical)
}

// Observe advances the local clock state to incorporate a remote HLC stamp,
// capping the wall advance at now+maxSkew.
//
// A remote stamp whose wall component exceeds now+maxSkew is treated as if
// its wall were exactly now+maxSkew (with logical 0) for the purpose of
// updating the local clock. The remote event itself retains its own HLC
// unchanged — Observe only affects subsequent Now() calls.
//
// Malformed or empty remote strings are silently ignored.
func (c *Clock) Observe(remote string, now time.Time, maxSkew time.Duration) {
	if remote == "" {
		return
	}
	rWall, rLogical, err := Decode(remote)
	if err != nil {
		return
	}
	capMS := now.UnixMilli() + maxSkew.Milliseconds()
	effWall, effLogical := rWall, rLogical
	if rWall > capMS {
		effWall = capMS
		effLogical = 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if effWall > c.wall || (effWall == c.wall && effLogical > c.logical) {
		c.wall = effWall
		c.logical = effLogical
	}
}

// Load seeds the clock from a persisted HLC string (e.g., the value stored in
// the bbolt sidecar on a previous run). Only advances the clock; never moves
// it backwards. Silently ignores an empty or malformed string.
func (c *Clock) Load(initial string) {
	if initial == "" {
		return
	}
	w, l, err := Decode(initial)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if w > c.wall || (w == c.wall && l > c.logical) {
		c.wall = w
		c.logical = l
	}
}

// State returns the current clock state as an encoded HLC string. Returns the
// empty string when the clock has never been advanced (zero value), so callers
// can skip persistence of an un-initialised clock.
func (c *Clock) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wall == 0 && c.logical == 0 {
		return ""
	}
	return Encode(c.wall, c.logical)
}

// Encode encodes wall milliseconds and a logical counter as 16 lowercase hex
// characters. The output is always exactly 16 chars; lexicographic order is
// identical to numeric order for all non-negative wall values.
func Encode(wall int64, logical uint16) string {
	return fmt.Sprintf("%012x%04x", wall, logical)
}

// Decode recovers wall milliseconds and a logical counter from the 16-char hex
// string produced by Encode. Returns an error when the string is not exactly 16
// lowercase hex characters, so callers always detect malformed stamps.
func Decode(s string) (wall int64, logical uint16, err error) {
	if len(s) != 16 {
		return 0, 0, fmt.Errorf("hlc: decode: want 16 hex chars, got %d", len(s))
	}
	for i := range 16 {
		r := s[i]
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return 0, 0, fmt.Errorf("hlc: decode: non-hex char %q at position %d", rune(r), i)
		}
	}
	var w int64
	for i := range 12 {
		w = w<<4 + int64(hexNibble(s[i]))
	}
	var l uint16
	for i := 12; i < 16; i++ {
		l = l<<4 + uint16(hexNibble(s[i]))
	}
	return w, l, nil
}

// hexNibble converts a single lowercase hex byte to its 0–15 value.
// The caller guarantees b ∈ {'0'..'9','a'..'f'}.
func hexNibble(b byte) byte {
	if b >= 'a' {
		return b - 'a' + 10
	}
	return b - '0'
}

// Implicit returns the implicit HLC for a legacy event that carries no HLC
// field. It is a pure function of the event's immutable CreateTime:
//
//	Implicit(t) = Encode(t.UnixMilli(), 0)
//
// Two machines independently computing Implicit for the same event derive the
// same string, keeping cross-machine sort order convergent for pre-wave logs.
// Legacy-only event logs therefore preserve their current (CreateTime, UID)
// ordering: Implicit(t) is lexicographically ordered by UnixMilli(t) and
// same-millisecond events still use the UID tiebreaker in sortDayEvents.
func Implicit(t time.Time) string {
	return Encode(t.UnixMilli(), 0)
}
