package output

import (
	"bytes"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// MaybePage renders long text-mode output through a pager when it makes sense.
// It calls render to produce the bytes, then:
//
//   - streams them straight to stdout when NOT a text-mode TTY, when --no-pager
//     is set, or when --output is json/ndjson (never page machine output);
//   - otherwise pipes them through $PAGER (default "less -R", so the colorizer's
//     ANSI survives) and waits for the pager to exit.
//
// render must write to the io.Writer it is given (not directly to os.Stdout).
// Output is bounded by --limit at every call site, so buffering is acceptable
// and avoids the goroutine/pipe complexity of a live stream.
func MaybePage(cmd *cobra.Command, render func(io.Writer) error) error {
	w := From(cmd)
	stdout := cmd.OutOrStdout()

	noPager, _ := cmd.Flags().GetBool("no-pager")
	if noPager || w.Format() != FormatText || !w.IsTTY() {
		return render(stdout)
	}

	var buf bytes.Buffer
	if err := render(&buf); err != nil {
		return err
	}

	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less -R"
	}
	pc := exec.CommandContext(cmd.Context(), "sh", "-c", pager)
	pc.Stdin = bytes.NewReader(buf.Bytes())
	pc.Stdout = stdout
	pc.Stderr = cmd.ErrOrStderr()
	if err := pc.Run(); err != nil {
		// Pager missing or failed (e.g. no `less`): fall back to raw output
		// rather than dropping the result.
		_, werr := stdout.Write(buf.Bytes())
		return werr
	}
	return nil
}
