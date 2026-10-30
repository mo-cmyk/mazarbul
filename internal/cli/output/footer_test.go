package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
)

func TestShowingNofMCompleteCount(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatText, &out, &errb)
	ShowingNofM(w, colorizer.New(false, &out), 5, 5, 5, "")
	if got := out.String(); !strings.Contains(got, "showing 5 of 5") {
		t.Errorf("want complete count, got %q", got)
	}
}

func TestShowingNofMPartialCount(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatText, &out, &errb)
	ShowingNofM(w, colorizer.New(false, &out), 5, 23, 5, "")
	if got := out.String(); !strings.Contains(got, "showing 5 of 23") || !strings.Contains(got, "--limit") {
		t.Errorf("want partial count with hint, got %q", got)
	}
}

func TestShowingNofMLimitReachedUnknownTotal(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatText, &out, &errb)
	ShowingNofM(w, colorizer.New(false, &out), 50, 0, 50, "use --offset 50")
	if got := out.String(); !strings.Contains(got, "limit reached") || !strings.Contains(got, "--offset 50") {
		t.Errorf("want limit-reached footer with hint, got %q", got)
	}
}

// TestShowingNofMLimitReachedEmptyHint guards the recall case: a caller with no
// --offset flag passes an empty hint, and the limit-reached footer must NOT
// suggest a flag that does not exist.
func TestShowingNofMLimitReachedEmptyHint(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatText, &out, &errb)
	ShowingNofM(w, colorizer.New(false, &out), 10, 0, 10, "")
	got := out.String()
	if !strings.Contains(got, "limit reached") {
		t.Errorf("want limit-reached footer, got %q", got)
	}
	if strings.Contains(got, "--offset") {
		t.Errorf("empty hint must not suggest --offset, got %q", got)
	}
}

func TestShowingNofMRoutesToStderrInJSONMode(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatJSON, &out, &errb)
	ShowingNofM(w, colorizer.New(false, &out), 5, 23, 5, "")
	if out.Len() != 0 {
		t.Errorf("footer must not touch stdout in JSON mode, got %q", out.String())
	}
	if !strings.Contains(errb.String(), "showing 5 of 23") {
		t.Errorf("footer should land on stderr in JSON mode, got %q", errb.String())
	}
}
