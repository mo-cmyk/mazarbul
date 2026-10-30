package colorizer

import (
	"io"
	"strings"
	"testing"
)

// esc is the ANSI escape byte that prefixes every SGR colour sequence.
const esc = "\x1b"

func TestColorizer_DisabledIsPlain(t *testing.T) {
	c := New(false, io.Discard)
	cases := map[string]func(string) string{
		"added":      c.Added,
		"removed":    c.Removed,
		"changed":    c.Changed,
		"superseded": c.Superseded,
		"conflict":   c.Conflict,
		"ident":      c.Ident,
		"meta":       c.Meta,
		"dim":        c.Dim,
		"accent":     c.Accent,
		"success":    c.Success,
		"warn":       c.Warn,
		"err":        c.Err,
	}
	for name, fn := range cases {
		t.Run(name+" returns input unchanged", func(t *testing.T) {
			const in = "checkpoint 01HXYZ"
			if got := fn(in); got != in {
				t.Errorf("disabled %s = %q, want unchanged %q", name, got, in)
			}
		})
	}
}

func TestColorizer_EnabledEmitsColour(t *testing.T) {
	// Ensure NO_COLOR is not inherited from another test, so the palette and
	// the profile both resolve to real colour.
	t.Setenv("NO_COLOR", "")
	c := New(true, io.Discard)
	cases := map[string]func(string) string{
		"added":      c.Added,
		"removed":    c.Removed,
		"changed":    c.Changed,
		"superseded": c.Superseded,
		"conflict":   c.Conflict,
		"ident":      c.Ident,
		"meta":       c.Meta,
		"dim":        c.Dim,
		"accent":     c.Accent,
		"success":    c.Success,
		"warn":       c.Warn,
		"err":        c.Err,
	}
	for name, fn := range cases {
		t.Run(name+" wraps input in an escape", func(t *testing.T) {
			const in = "payload"
			got := fn(in)
			if !strings.Contains(got, in) {
				t.Errorf("enabled %s dropped the payload: %q", name, got)
			}
			if !strings.Contains(got, esc) {
				t.Errorf("enabled %s emitted no escape: %q", name, got)
			}
		})
	}
}

func TestColorizer_NoColorEnvCollapsesToPlain(t *testing.T) {
	// Even if a caller wrongly enables colour, NO_COLOR collapses the palette
	// to NoColor so no escape leaks.
	t.Setenv("NO_COLOR", "1")
	c := New(true, io.Discard)
	const in = "payload"
	if got := c.Added(in); strings.Contains(got, esc) {
		t.Errorf("NO_COLOR set but escape leaked: %q", got)
	}
}
