package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func makeRememberCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	rem := newRememberCmd()
	root.AddCommand(rem)
	if outBuf != nil {
		root.SetOut(outBuf)
		rem.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		rem.SetErr(errBuf)
	}
	return root
}

func TestRememberHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeRememberCmd(&buf, nil)
	root.SetArgs([]string{"remember", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "maz remember", "--coords", "--coords-provenance", "--source", "--confidence"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n%s", want, out)
		}
	}
}

func TestRememberFlagDefaults(t *testing.T) {
	t.Parallel()
	cmd := newRememberCmd()
	if got, _ := cmd.Flags().GetString("source"); got != "cli" {
		t.Errorf("--source default = %q, want cli", got)
	}
	if got, _ := cmd.Flags().GetFloat64("confidence"); got != 1.0 {
		t.Errorf("--confidence default = %g, want 1.0", got)
	}
	if got, _ := cmd.Flags().GetString("coords-provenance"); got != "user_provided" {
		t.Errorf("--coords-provenance default = %q, want user_provided", got)
	}
}

func TestParseCoords_Valid(t *testing.T) {
	t.Parallel()
	lat, lng, err := parseCoords("52.52,13.40")
	if err != nil {
		t.Fatalf("parseCoords: %v", err)
	}
	if lat != 52.52 || lng != 13.40 {
		t.Errorf("got (%g,%g), want (52.52,13.40)", lat, lng)
	}
}

func TestParseCoords_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"not-a-pair", "expected format"},
		{"abc,13.40", "latitude"},
		{"52.52,xyz", "longitude"},
		{"100,13.40", "out of range"},
		{"52.52,200", "out of range"},
	}
	for _, tc := range cases {
		_, _, err := parseCoords(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseCoords(%q) err = %v, want substring %q", tc.in, err, tc.want)
		}
	}
}

func TestRememberRequiresCoordsForProvenance(t *testing.T) {
	t.Parallel()
	var outBuf, errBuf bytes.Buffer
	root := makeRememberCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"remember", "--coords-provenance=geocoded:nominatim", "some text"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --coords-provenance set without --coords")
	}
	if !strings.Contains(errBuf.String(), "--coords-provenance requires --coords") {
		t.Errorf("stderr missing requires-coords message: %q", errBuf.String())
	}
}

// TestRememberNoArgExitsOne ensures bare "maz remember" exits non-zero and
// prints a usage hint with an example rather than the bare cobra message.
func TestRememberNoArgExitsOne(t *testing.T) {
	t.Parallel()
	var outBuf, errBuf bytes.Buffer
	root := makeRememberCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"remember"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing text argument, got nil")
	}
	// The custom args validator should produce an actionable message.
	combined := errBuf.String() + err.Error()
	if !strings.Contains(combined, "text argument required") {
		t.Errorf("error %q should mention 'text argument required'", combined)
	}
	if !strings.Contains(combined, "maz remember") {
		t.Errorf("error %q should include usage example", combined)
	}
}
