package output

import (
	"bytes"
	"io"
	"testing"

	"github.com/spf13/cobra"
)

// newPagerCmd builds a command whose stdout/stderr are buffers (never a TTY),
// so MaybePage always takes the raw-streaming branch — the real pager path is
// exercised by the pty E2E harness, not unit tests.
func newPagerCmd(out, errb *bytes.Buffer, noPager bool) *cobra.Command {
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().Bool("no-pager", noPager, "")
	cmd.SetOut(out)
	cmd.SetErr(errb)
	return cmd
}

func TestMaybePageStreamsRawWhenNotTTY(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	cmd := newPagerCmd(&out, &errb, false)
	err := MaybePage(cmd, func(w io.Writer) error {
		_, e := io.WriteString(w, "hello world\n")
		return e
	})
	if err != nil {
		t.Fatalf("MaybePage: %v", err)
	}
	if out.String() != "hello world\n" {
		t.Errorf("want raw stream to stdout, got %q", out.String())
	}
}

func TestMaybePageHonorsNoPagerFlag(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	cmd := newPagerCmd(&out, &errb, true)
	err := MaybePage(cmd, func(w io.Writer) error {
		_, e := io.WriteString(w, "no pager\n")
		return e
	})
	if err != nil {
		t.Fatalf("MaybePage: %v", err)
	}
	if out.String() != "no pager\n" {
		t.Errorf("want raw output with --no-pager, got %q", out.String())
	}
}
