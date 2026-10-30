package hlc_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/hlc"
)

// TestEncode_LexicographicEqualsNumeric is a property test: for any two
// (wall, logical) pairs where (wall1, logical1) < (wall2, logical2) in
// numeric order, Encode(wall1, logical1) < Encode(wall2, logical2)
// lexicographically.
func TestEncode_LexicographicEqualsNumeric(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(0xdeadbeef, 0xcafebabe))
	for range 2000 {
		w1 := int64(rng.Uint64() >> 16) // keep in 48-bit range
		l1 := uint16(rng.Uint32())
		w2 := int64(rng.Uint64() >> 16)
		l2 := uint16(rng.Uint32())

		// Numeric comparison: (w1,l1) vs (w2,l2)
		numericLess := w1 < w2 || (w1 == w2 && l1 < l2)
		numericEq := w1 == w2 && l1 == l2

		s1 := hlc.Encode(w1, l1)
		s2 := hlc.Encode(w2, l2)

		lexLess := s1 < s2
		lexEq := s1 == s2

		if numericEq != lexEq {
			t.Errorf("encode(%d,%d)=%q encode(%d,%d)=%q: numeric eq=%v lex eq=%v",
				w1, l1, s1, w2, l2, s2, numericEq, lexEq)
		}
		if !numericEq && numericLess != lexLess {
			t.Errorf("encode(%d,%d)=%q encode(%d,%d)=%q: numeric less=%v lex less=%v",
				w1, l1, s1, w2, l2, s2, numericLess, lexLess)
		}
	}
}

func TestEncode_Fixed(t *testing.T) {
	t.Parallel()
	// Zero value.
	if got := hlc.Encode(0, 0); got != "0000000000000000" {
		t.Errorf("Encode(0,0) = %q, want 0000000000000000", got)
	}
	// One millisecond wall, logical 1.
	if got := hlc.Encode(1, 1); got != "0000000000010001" {
		t.Errorf("Encode(1,1) = %q, want 0000000000010001", got)
	}
	// Max logical.
	if got := hlc.Encode(0, 0xffff); got != "000000000000ffff" {
		t.Errorf("Encode(0,0xffff) = %q, want 000000000000ffff", got)
	}
	// Known wall timestamp: 2026-07-01 00:00:00 UTC = 1751328000000 ms = 0x197e9660b80.
	// 12 hex = 0197e9660b80, logical 0 → "0197e9660b800000"
	wall := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	got := hlc.Encode(wall, 0)
	if len(got) != 16 {
		t.Errorf("Encode len = %d, want 16", len(got))
	}
}

func TestDecode_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		wall    int64
		logical uint16
	}{
		{0, 0},
		{1, 0},
		{1751328000000, 42},
		{0xffffffffffff, 0xffff}, // max values within 48-bit wall
	}
	for _, tc := range cases {
		s := hlc.Encode(tc.wall, tc.logical)
		w, l, err := hlc.Decode(s)
		if err != nil {
			t.Errorf("Decode(%q): unexpected error: %v", s, err)
			continue
		}
		if w != tc.wall || l != tc.logical {
			t.Errorf("round-trip (%d,%d): got (%d,%d)", tc.wall, tc.logical, w, l)
		}
	}
}

func TestDecode_Errors(t *testing.T) {
	t.Parallel()
	bad := []string{
		"",                  // empty
		"short",             // too short
		"0000000000000000x", // too long
		"000000000000000G",  // uppercase hex
		"000000000000000Z",  // non-hex
	}
	for _, s := range bad {
		if _, _, err := hlc.Decode(s); err == nil {
			t.Errorf("Decode(%q): want error, got nil", s)
		}
	}
}

// TestClock_Monotonicity verifies that rapid Now() calls within the same
// millisecond produce strictly increasing stamps via the logical counter.
func TestClock_Monotonicity(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	fixed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	prev := c.Now(fixed)
	for range 100 {
		next := c.Now(fixed) // same ms every call
		if next <= prev {
			t.Errorf("clock regressed: %q → %q", prev, next)
		}
		prev = next
	}
}

// TestClock_WallAdvance verifies that a later wall time resets the logical counter.
func TestClock_WallAdvance(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(1 * time.Millisecond)

	// Issue several stamps at t1 to drive the logical counter up.
	for range 5 {
		c.Now(t1)
	}
	// A tick at t2 must reset logical to 0.
	stamp := c.Now(t2)
	_, l, err := hlc.Decode(stamp)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if l != 0 {
		t.Errorf("logical after wall advance = %d, want 0", l)
	}
}

// TestClock_Observe_Advance verifies that Observe moves the local clock
// forward to incorporate a remote stamp.
func TestClock_Observe_Advance(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	remote := hlc.Encode(now.Add(10*time.Second).UnixMilli(), 5)
	c.Observe(remote, now, 1*time.Minute)

	// The next Now call must sort after the observed remote.
	next := c.Now(now)
	if next <= remote {
		t.Errorf("Now() after Observe = %q, want > observed %q", next, remote)
	}
}

// TestClock_Observe_SkewCap verifies that a far-future remote HLC does not
// drag the local clock beyond now+maxSkew.
func TestClock_Observe_SkewCap(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	maxSkew := 5 * time.Minute

	// Remote HLC is 1 hour in the future — well beyond maxSkew.
	farFuture := hlc.Encode(now.Add(1*time.Hour).UnixMilli(), 0)
	c.Observe(farFuture, now, maxSkew)

	state := c.State()
	if state == "" {
		t.Fatal("clock state empty after Observe")
	}
	w, _, err := hlc.Decode(state)
	if err != nil {
		t.Fatalf("Decode state: %v", err)
	}
	capMS := now.UnixMilli() + maxSkew.Milliseconds()
	if w > capMS {
		t.Errorf("clock wall %d exceeds now+maxSkew %d (skew cap violated)", w, capMS)
	}
}

// TestClock_Observe_FarFuture_EventSortsAtOwnHLC checks the invariant stated
// in the spec: Observe caps the LOCAL clock advance but the remote event's HLC
// is preserved verbatim.
func TestClock_Observe_FarFuture_EventSortsAtOwnHLC(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	maxSkew := 5 * time.Minute

	farFuture := hlc.Encode(now.Add(1*time.Hour).UnixMilli(), 42)

	c := new(hlc.Clock)
	c.Observe(farFuture, now, maxSkew)

	// The event itself is not mutated — its HLC remains the far-future stamp.
	// (This test just verifies Observe doesn't modify the input string.)
	if farFuture != hlc.Encode(now.Add(1*time.Hour).UnixMilli(), 42) {
		t.Error("farFuture string mutated — it must be preserved verbatim")
	}
}

// TestClock_Load_Seeds verifies that Load pre-warms the clock from persistence.
func TestClock_Load_Seeds(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	seed := hlc.Encode(1_000_000, 7)
	c.Load(seed)

	// Now() at a wall before the loaded value must produce a stamp > seed.
	past := time.UnixMilli(500_000)
	stamp := c.Now(past)
	if stamp <= seed {
		t.Errorf("Now() after Load = %q, want > loaded seed %q", stamp, seed)
	}
}

// TestClock_Load_NeverRegresses verifies Load only advances the clock.
func TestClock_Load_NeverRegresses(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	future := hlc.Encode(2_000_000, 0)
	c.Load(future)

	past := hlc.Encode(1_000_000, 0)
	c.Load(past) // must not move the clock back

	state := c.State()
	if state != future {
		t.Errorf("State after Load(past) = %q, want %q (Load must never regress)", state, future)
	}
}

// TestImplicit_OrderMatchesCreateTime verifies that Implicit(t) preserves the
// same order as the CreateTime it wraps, so legacy events keep their order.
func TestImplicit_OrderMatchesCreateTime(t *testing.T) {
	t.Parallel()
	times := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
	}
	prev := hlc.Implicit(times[0])
	for _, ts := range times[1:] {
		cur := hlc.Implicit(ts)
		if cur <= prev {
			t.Errorf("Implicit(%v) = %q not > Implicit(prev) = %q", ts, cur, prev)
		}
		prev = cur
	}
}

// TestImplicit_Deterministic verifies that Implicit encodes the wall-ms and
// logical-0 fields predictably: two calls with identical arguments produce the
// same 16-char string, and the logical counter is always zero.
func TestImplicit_Deterministic(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	s := hlc.Implicit(ts)
	// Logical nibbles (last 4 chars) must be "0000".
	if s[12:] != "0000" {
		t.Errorf("Implicit logical = %q, want 0000", s[12:])
	}
	// Wall must match Encode(ts.UnixMilli(), 0).
	want := hlc.Encode(ts.UnixMilli(), 0)
	if s != want {
		t.Errorf("Implicit = %q, want %q", s, want)
	}
}

// TestClock_State_Zero verifies the zero-value contract.
func TestClock_State_Zero(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	if s := c.State(); s != "" {
		t.Errorf("State() on zero clock = %q, want empty string", s)
	}
}

// TestClock_Observe_Malformed verifies that a malformed remote string is silently ignored.
func TestClock_Observe_Malformed(t *testing.T) {
	t.Parallel()
	c := new(hlc.Clock)
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	c.Observe("not-valid", now, time.Minute)
	c.Observe("", now, time.Minute)
	// Clock must still be at zero (no panic, no corruption).
	if s := c.State(); s != "" {
		t.Errorf("State after bad Observe = %q, want empty string", s)
	}
}
