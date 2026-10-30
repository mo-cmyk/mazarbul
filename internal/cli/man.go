package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

func newManCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "man",
		Short: "Generate man pages for all maz commands",
		Long: `Generate UNIX man pages for the entire maz command tree.

A maz installed via the install script or "maz upgrade" already ships its man
pages, so "man maz" works out of the box. (Note: "man ./bin/maz" can never work —
passing a path makes man format that file as roff source, and the binary is not
roff.)

With no --output-dir, the root command's man page is rendered in an interactive
terminal via the system man; when piped or redirected it is emitted as raw troff
for scripting.

With --output-dir, a .1 file per command is written under that directory. Point
man at it with -M (works on macOS and Linux), giving a <manpath>/man1 layout:

  maz man --output-dir man/man1
  man -M man maz
  man -M man maz-recall

Exit codes:
  0   success
  2   unexpected failure (I/O error)

Examples:
  maz man                                          # render in your terminal
  maz man > maz.1                                  # raw troff for scripting
  maz man --output-dir man/man1 && man -M man maz`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMan(cmd)
		},
	}
	cmd.Flags().String("output-dir", "", "write man pages under this directory (default: stdout)")
	return cmd
}

func runMan(cmd *cobra.Command) error {
	outDir, _ := cmd.Flags().GetString("output-dir")
	root := cmd.Root()

	header := &doc.GenManHeader{
		Title:   "MAZ",
		Section: "1",
		Date:    ptrTime(time.Now()),
		Source:  "Mazarbul",
		Manual:  "Mazarbul Manual",
	}

	if outDir == "" {
		// Generate the root command's man page.
		var buf bytes.Buffer
		if err := doc.GenMan(root, header, &buf); err != nil {
			//nolint:errcheck // best-effort stderr write on error path
			fmt.Fprintf(cmd.ErrOrStderr(), "man: %v\n", err)
			return silentError{code: 2, err: err}
		}
		// At an interactive terminal, render through the system `man` so the
		// user sees a formatted, paged page instead of raw troff. When piped or
		// redirected (e.g. `maz man > maz.1`), emit raw troff.
		if output.From(cmd).IsTTY() {
			if manPath, lookErr := exec.LookPath("man"); lookErr == nil {
				return renderMan(cmd, manPath, buf.Bytes())
			}
			//nolint:errcheck // best-effort hint before falling back to raw troff
			fmt.Fprintln(cmd.ErrOrStderr(), "man: `man` not found on PATH; emitting raw troff (save it with `maz man > maz.1`, or use `maz man --output-dir <dir>`)")
		}
		_, err := cmd.OutOrStdout().Write(buf.Bytes())
		return err
	}

	// Write the full tree under outDir.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "man: mkdir %s: %v\n", outDir, err)
		return silentError{code: 2, err: err}
	}
	if err := doc.GenManTree(root, header, outDir); err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "man: generate tree: %v\n", err)
		return silentError{code: 2, err: err}
	}
	//nolint:errcheck // best-effort stdout write
	fmt.Fprintf(cmd.OutOrStdout(), "man pages written to %s\n", outDir)
	return nil
}

// renderMan shows the generated troff in the terminal via the system man. Man
// reads a page by name from its search path rather than troff on stdin, and
// BSD/macOS man has no -l flag, so the page is written to a temp file and man is
// pointed at it: BSD man formats a path argument directly while GNU man-db needs
// -l/--local-file. Man's pager needs the real terminal, so stdin/stdout/stderr
// are inherited (this path is reached only when stdout is a TTY). On any setup
// failure it falls back to emitting raw troff.
func renderMan(cmd *cobra.Command, manPath string, troff []byte) error {
	dir, err := os.MkdirTemp("", "maz-man-*")
	if err != nil {
		_, werr := cmd.OutOrStdout().Write(troff)
		return werr
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort temp cleanup
	page := filepath.Join(dir, "maz.1")
	if err := os.WriteFile(page, troff, 0o600); err != nil {
		_, werr := cmd.OutOrStdout().Write(troff)
		return werr
	}
	args := []string{page}
	if runtime.GOOS == "linux" {
		args = []string{"-l", page} // GNU man-db local-file mode
	}
	mc := exec.CommandContext(cmd.Context(), manPath, args...) // #nosec G204 -- manPath resolved from PATH; the only arg is a temp file we wrote
	mc.Stdin = os.Stdin
	mc.Stdout = os.Stdout
	mc.Stderr = os.Stderr
	if err := mc.Run(); err != nil {
		if pagerInterrupted(err) {
			// User quit the pager via a signal (e.g. Ctrl+C). That is a normal
			// interaction, not a maz failure — exit cleanly without the noise.
			return nil
		}
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "man: render via %s: %v\n", manPath, err)
		return silentError{code: 2, err: err}
	}
	return nil
}

// pagerInterrupted reports whether err is the pager terminating from a signal
// (the user pressing Ctrl+C to quit `less`) rather than a genuine failure.
func pagerInterrupted(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			return ws.Signaled()
		}
	}
	return false
}

func ptrTime(t time.Time) *time.Time { return &t }
