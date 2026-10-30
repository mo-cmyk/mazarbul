package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// makeTestRoot wires a minimal root with common persistent flags for
// man and completion testing.
func makeTestRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", Short: "test root", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	man := newManCmd()
	completion := newCompletionCmd()
	root.AddCommand(man)
	root.AddCommand(completion)
	if outBuf != nil {
		root.SetOut(outBuf)
		man.SetOut(outBuf)
		completion.SetOut(outBuf)
		for _, sub := range completion.Commands() {
			sub.SetOut(outBuf)
		}
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		man.SetErr(errBuf)
		completion.SetErr(errBuf)
		for _, sub := range completion.Commands() {
			sub.SetErr(errBuf)
		}
	}
	return root
}

// TestManHelp: --help is non-empty and includes the --output-dir + man -M example.
func TestManHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeTestRoot(&buf, nil)
	root.SetArgs([]string{"man", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("man --help output is empty")
	}
	for _, want := range []string{"--output-dir", "man -M man maz"} {
		if !strings.Contains(out, want) {
			t.Errorf("man --help missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestManStdout: maz man (no flags) produces troff-like output to stdout.
func TestManStdout(t *testing.T) {
	t.Parallel()
	var buf, errBuf bytes.Buffer
	root := makeTestRoot(&buf, &errBuf)
	root.SetArgs([]string{"man"})
	if err := root.Execute(); err != nil {
		t.Fatalf("man: %v (stderr: %s)", err, errBuf.String())
	}
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("man stdout is empty")
	}
	// Troff man pages start with a .TH header.
	if !strings.Contains(out, ".TH") && !strings.Contains(out, "NAME") {
		t.Errorf("man output does not look like a man page\nfirst 200 chars:\n%s", out[:min200(out)])
	}
}

// TestManOutputDir: --output-dir writes files and reports success.
func TestManOutputDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var buf, errBuf bytes.Buffer
	root := makeTestRoot(&buf, &errBuf)
	root.SetArgs([]string{"man", "--output-dir", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("man --output-dir: %v (stderr: %s)", err, errBuf.String())
	}
	if !strings.Contains(buf.String(), "written to") {
		t.Errorf("expected 'written to' in stdout: %q", buf.String())
	}
}

// TestCompletionHelp: --help is non-empty and lists all shells.
func TestCompletionHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeTestRoot(&buf, nil)
	root.SetArgs([]string{"completion", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("completion --help is empty")
	}
	for _, want := range []string{"bash", "zsh", "fish", "powershell"} {
		if !strings.Contains(out, want) {
			t.Errorf("completion --help missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestCompletionBash: bash output is non-empty and references the binary name.
func TestCompletionBash(t *testing.T) {
	t.Parallel()
	var buf, errBuf bytes.Buffer
	root := makeTestRoot(&buf, &errBuf)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatalf("completion bash: %v", err)
	}
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("completion bash output is empty")
	}
	// Bash completion scripts typically contain the binary name.
	if !strings.Contains(out, "maz") {
		t.Errorf("completion bash output does not reference 'maz'")
	}
}

// TestCompletionZsh: zsh output is non-empty.
func TestCompletionZsh(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeTestRoot(&buf, nil)
	root.SetArgs([]string{"completion", "zsh"})
	_ = root.Execute()
	if len(buf.String()) == 0 {
		t.Fatal("completion zsh output is empty")
	}
}

// TestCompletionFish: fish output is non-empty.
func TestCompletionFish(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeTestRoot(&buf, nil)
	root.SetArgs([]string{"completion", "fish"})
	_ = root.Execute()
	if len(buf.String()) == 0 {
		t.Fatal("completion fish output is empty")
	}
}

// TestCompletionUnknownShell: unknown shell arg is rejected by cobra ValidArgs.
func TestCompletionUnknownShell(t *testing.T) {
	t.Parallel()
	var buf, errBuf bytes.Buffer
	root := makeTestRoot(&buf, &errBuf)
	// "ksh" is not in ValidArgs, so cobra rejects it before RunE.
	root.SetArgs([]string{"completion", "ksh"})
	err := root.Execute()
	// cobra's OnlyValidArgs returns an error for unknown args.
	if err == nil {
		t.Fatal("expected error for unknown shell, got nil")
	}
}

// TestRunCompletion_UnknownShell: runCompletion directly returns silentError code 1.
func TestRunCompletion_UnknownShell(t *testing.T) {
	t.Parallel()
	var buf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "test", SilenceUsage: true, SilenceErrors: true}
	cmd.SetOut(&buf)
	cmd.SetErr(&errBuf)
	err := runCompletion(cmd, "ksh")
	if err == nil {
		t.Fatal("expected error for ksh, got nil")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("expected silentError{code:1}, got %v (type %T)", err, err)
	}
}

// TestCompletionCmds_InRoot: completion and man are in the root command tree.
func TestCompletionCmds_InRoot(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	for _, name := range []string{"completion", "man"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Errorf("root.Find(%s): %v", name, err)
			continue
		}
		if cmd == nil || cmd.Name() != name {
			t.Errorf("%s not found in root", name)
		}
	}
}

func min200(s string) int {
	if len(s) < 200 {
		return len(s)
	}
	return 200
}
