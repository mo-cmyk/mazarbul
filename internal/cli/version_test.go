package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/version"
)

// versionTestRoot wires a minimal root with the output flags the version
// command reads, plus the version subcommand.
func versionTestRoot(out *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	v := newVersionCmd()
	root.AddCommand(v)
	if out != nil {
		root.SetOut(out)
		v.SetOut(out)
	}
	return root
}

// TestVersionPlain: text output carries version, commit, and date across
// separate lines, leading with "maz <version>".
func TestVersionPlain(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := versionTestRoot(&buf)
	root.SetArgs([]string{"version", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version: %v", err)
	}
	out := buf.String()
	wantLines := []string{
		"maz " + version.Version,
		"commit " + version.Commit,
		"built " + version.Date,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("version plain output %q missing line %q", out, want)
		}
	}
	if first := strings.SplitN(out, "\n", 2)[0]; first != "maz "+version.Version {
		t.Errorf("version plain first line = %q, want %q", first, "maz "+version.Version)
	}
}

// TestVersionJSON: `version --output json` carries version, commit, and date.
func TestVersionJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := versionTestRoot(&buf)
	root.SetArgs([]string{"version", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version --output json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode version JSON %q: %v", buf.String(), err)
	}
	for _, key := range []string{"version", "commit", "date"} {
		if _, ok := got[key]; !ok {
			t.Errorf("version JSON missing %q key: %v", key, got)
		}
	}
	if got["version"] != version.Version {
		t.Errorf("version JSON version = %q, want %q", got["version"], version.Version)
	}
}

// TestVersionJSONFlag: the legacy `--json` boolean flag is a separate
// resolution branch from `--output json`; assert it carries the same
// version, commit, and date keys so that path stays covered too.
func TestVersionJSONFlag(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := versionTestRoot(&buf)
	root.SetArgs([]string{"version", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version --json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode version JSON %q: %v", buf.String(), err)
	}
	for _, key := range []string{"version", "commit", "date"} {
		if _, ok := got[key]; !ok {
			t.Errorf("version --json missing %q key: %v", key, got)
		}
	}
	if got["version"] != version.Version {
		t.Errorf("version --json version = %q, want %q", got["version"], version.Version)
	}
}
