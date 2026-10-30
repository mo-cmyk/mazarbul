package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newDocTestCmd builds a minimal command tree carrying the data-dir flag the
// doc helpers resolve through cmd.Root().
func newDocTestCmd(t *testing.T, dataDir string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "maz"}
	root.PersistentFlags().String("data-dir", dataDir, "")
	cmd := &cobra.Command{Use: "doc"}
	root.AddCommand(cmd)
	cmd.SetContext(context.Background())
	return cmd
}

// stubLocalVaultResolution points resolveVaultFileFn at the pure local
// computation for the test's lifetime, so the editor-backed doc-new path stays
// hermetic (no daemon wiring, no risk of hitting a live daemon's prod vault)
// while still exercising the daemon-aware seed-path resolution. The local
// resolution returns the same <dataDir>/vault/... layout a no-daemon run would.
func stubLocalVaultResolution(t *testing.T) {
	t.Helper()
	prev := resolveVaultFileFn
	resolveVaultFileFn = localVaultFile
	t.Cleanup(func() { resolveVaultFileFn = prev })
}

// fakeEditor writes an executable script that appends a line to the file it
// is invoked on — a deterministic stand-in for $VISUAL/$EDITOR.
func fakeEditor(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	return path
}

func TestResolveEditor_Precedence(t *testing.T) {
	t.Setenv("VISUAL", "visual-editor")
	t.Setenv("EDITOR", "plain-editor")
	if got := resolveEditor(); got != "visual-editor" {
		t.Fatalf("editor = %q, want $VISUAL first", got)
	}
	t.Setenv("VISUAL", "")
	if got := resolveEditor(); got != "plain-editor" {
		t.Fatalf("editor = %q, want $EDITOR second", got)
	}
	t.Setenv("EDITOR", "")
	if got := resolveEditor(); got != "vi" {
		t.Fatalf("editor = %q, want vi fallback", got)
	}
}

// TestEditorCommand_SplitsArguments pins that $VISUAL/$EDITOR values carrying
// flags ("code --wait", "vim -n") are split into executable + arguments with
// the file appended last, instead of being treated as one executable name
// (which made `maz doc new|edit` fail before the editor ever opened).
func TestEditorCommand_SplitsArguments(t *testing.T) {
	for _, tc := range []struct {
		editor   string
		wantName string
		wantArgs []string
	}{
		{"code --wait", "code", []string{"--wait", "f.md"}},
		{"vim -n", "vim", []string{"-n", "f.md"}},
		{"emacsclient -c", "emacsclient", []string{"-c", "f.md"}},
		{"vi", "vi", []string{"f.md"}},
		{"  ", "vi", []string{"f.md"}}, // whitespace-only falls back to vi
	} {
		name, args := editorCommand(tc.editor, "f.md")
		if name != tc.wantName {
			t.Errorf("editorCommand(%q) name = %q, want %q", tc.editor, name, tc.wantName)
		}
		if len(args) != len(tc.wantArgs) {
			t.Errorf("editorCommand(%q) args = %v, want %v", tc.editor, args, tc.wantArgs)
			continue
		}
		for i := range args {
			if args[i] != tc.wantArgs[i] {
				t.Errorf("editorCommand(%q) args = %v, want %v", tc.editor, args, tc.wantArgs)
				break
			}
		}
	}
}

// TestDocNewViaEditor_EditorWithFlags drives the real runEditor path with an
// $EDITOR value that carries an argument, pinning the end-to-end behaviour.
func TestDocNewViaEditor_EditorWithFlags(t *testing.T) {
	dataDir := t.TempDir()
	cmd := newDocTestCmd(t, dataDir)
	// The fake editor script ignores $2 unless invoked with the extra flag
	// first; passing the value with a flag exercises the argv split.
	t.Setenv("VISUAL", fakeEditor(t, `[ "$1" = "--flag" ] || exit 64
printf 'flagged edit\n' >> "$2"`)+" --flag")
	t.Setenv("EDITOR", "")

	if err := runEditor(cmd, filepath.Join(dataDir, "scratch.md")); err != nil {
		t.Fatalf("runEditor with flagged $VISUAL: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dataDir, "scratch.md"))
	if err != nil {
		t.Fatalf("read scratch: %v", err)
	}
	if string(body) != "flagged edit\n" {
		t.Fatalf("scratch body = %q, want %q", string(body), "flagged edit\n")
	}
}

func TestDocNewViaEditor_RoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	cmd := newDocTestCmd(t, dataDir)
	stubLocalVaultResolution(t)
	t.Setenv("VISUAL", fakeEditor(t, `printf 'written by editor\n' >> "$1"`))

	body, err := docNewViaEditor(cmd, "notes/fresh.md", "Fresh")
	if err != nil {
		t.Fatalf("docNewViaEditor: %v", err)
	}
	// Seeded heading from --title plus the editor's addition, verbatim.
	if body != "# Fresh\n\nwritten by editor\n" {
		t.Fatalf("body = %q", body)
	}
	// The session edited the real future vault file (default personal partition).
	file := filepath.Join(dataDir, "vault", "personal", "notes", "fresh.md")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("vault file: %v", err)
	}
	if string(raw) != body {
		t.Fatalf("vault file = %q, want the editor's bytes", raw)
	}
}

func TestDocNewViaEditor_EmptySessionRemovesSeededFile(t *testing.T) {
	dataDir := t.TempDir()
	cmd := newDocTestCmd(t, dataDir)
	stubLocalVaultResolution(t)
	t.Setenv("VISUAL", fakeEditor(t, `:`)) // editor saves nothing

	body, err := docNewViaEditor(cmd, "empty.md", "")
	if err != nil {
		t.Fatalf("docNewViaEditor: %v", err)
	}
	if body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "vault", "personal", "empty.md")); !os.IsNotExist(err) {
		t.Fatal("empty session must remove the seeded file")
	}
}

func TestDocNewViaEditor_ExistingFileRefused(t *testing.T) {
	dataDir := t.TempDir()
	cmd := newDocTestCmd(t, dataDir)
	stubLocalVaultResolution(t)
	target := filepath.Join(dataDir, "vault", "personal", "taken.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := docNewViaEditor(cmd, "taken.md", "")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already-exists refusal", err)
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "existing\n" {
		t.Fatalf("existing file modified: %q", raw)
	}
}

func TestDocNewViaEditor_FailingEditorCleansUp(t *testing.T) {
	dataDir := t.TempDir()
	cmd := newDocTestCmd(t, dataDir)
	stubLocalVaultResolution(t)
	t.Setenv("VISUAL", fakeEditor(t, `exit 1`))

	if _, err := docNewViaEditor(cmd, "fail.md", ""); err == nil {
		t.Fatal("want editor failure surfaced")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "vault", "personal", "fail.md")); !os.IsNotExist(err) {
		t.Fatal("failed session must remove the seeded file")
	}
}

// TestDocNewCmd_ScopeFlag pins that `maz doc new` exposes a --scope flag that
// defaults to empty (personal at the tool layer) and documents the four scopes.
// The flag value is forwarded verbatim into the writeDocument tool args, where
// parseWriteScope validates it (an unknown scope surfaces there as E_VALIDATION,
// covered by the tools-layer tests); the CLI is a pass-through.
func TestDocNewCmd_ScopeFlag(t *testing.T) {
	cmd := newDocNewCmd()
	f := cmd.Flags().Lookup("scope")
	if f == nil {
		t.Fatal("maz doc new is missing the --scope flag")
	}
	if f.DefValue != "" {
		t.Errorf("--scope default = %q, want empty (personal is applied at the tool layer)", f.DefValue)
	}
	for _, want := range []string{"personal", "project", "team", "draft"} {
		if !strings.Contains(f.Usage, want) {
			t.Errorf("--scope usage %q does not mention the %q scope", f.Usage, want)
		}
	}
}

func TestLocalVaultFile_MappingAndEscapes(t *testing.T) {
	dataDir := t.TempDir()
	cmd := newDocTestCmd(t, dataDir)

	got, err := localVaultFile(cmd, "notes/foo") // extension defaulted
	if err != nil {
		t.Fatalf("localVaultFile: %v", err)
	}
	// The vault is partitioned by scope; the offline fallback assumes the default
	// (personal) partition, so the path is <root>/personal/<rel>.
	want := filepath.Join(dataDir, "vault", "personal", "notes", "foo.md")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}

	for _, bad := range []string{"../escape.md", "/abs.md", "a/../../x.md"} {
		if _, err := localVaultFile(cmd, bad); err == nil {
			t.Errorf("localVaultFile(%q) accepted an escaping path", bad)
		}
	}
}
