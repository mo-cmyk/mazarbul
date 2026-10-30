package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// makeDocCanvasRoot wires a minimal cobra root with the entity, relate, and doc
// subcommands so `maz doc canvas` can be exercised end-to-end against in-process
// wiring (no daemon).
func makeDocCanvasRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newEntityCmd())
	root.AddCommand(newRelateCmd())
	root.AddCommand(newDocCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

func runDocCanvasCmd(t *testing.T, dataDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeDocCanvasRoot(&outBuf, &errBuf)
	full := append([]string{"--data-dir", dataDir, "--output", "text", "doc", "canvas"}, args...)
	root.SetArgs(full)
	root.SetContext(context.Background())
	execErr := root.Execute()
	return outBuf.String(), errBuf.String(), execErr
}

// TestDocCanvasWritesToOutputFile drives the headline flow: seed two entities +
// a relation, export with -o, and assert a valid JSON Canvas lands at the path
// with the seed node and the relation edge.
func TestDocCanvasWritesToOutputFile(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	aliceName := runEntityCreateCmd(t, dir, "person", "Sarah Vance")
	zurichName := runEntityCreateCmd(t, dir, "place", "Zurich")
	alice := strings.TrimPrefix(aliceName, "entities/")
	zurich := strings.TrimPrefix(zurichName, "entities/")
	_ = runRelateCmd(t, dir, alice, "works_at", zurich)

	out := filepath.Join(dir, "graph.canvas")
	stdout, stderr, err := runDocCanvasCmd(t, dir, "Sarah Vance", "--hops", "1", "-o", out)
	if err != nil {
		t.Fatalf("doc canvas: %v (stderr=%q)", err, stderr)
	}

	data, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("read canvas: %v", rerr)
	}
	var c struct {
		Nodes []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"nodes"`
		Edges []struct {
			FromNode string `json:"fromNode"`
			ToNode   string `json:"toNode"`
			Label    string `json:"label"`
		} `json:"edges"`
	}
	if uerr := json.Unmarshal(data, &c); uerr != nil {
		t.Fatalf("canvas is not valid JSON: %v\n%s", uerr, data)
	}

	ids := map[string]bool{}
	for _, n := range c.Nodes {
		ids[n.ID] = true
	}
	if !ids[alice] || !ids[zurich] {
		t.Fatalf("canvas nodes %v missing seed/neighbor (%s/%s)", ids, alice, zurich)
	}
	foundEdge := false
	for _, e := range c.Edges {
		if e.FromNode == alice && e.ToNode == zurich && e.Label == "works_at" {
			foundEdge = true
		}
	}
	if !foundEdge {
		t.Fatalf("canvas edges missing works_at %s->%s:\n%s", alice, zurich, data)
	}
	if !strings.Contains(stdout, "wrote") {
		t.Errorf("expected a 'wrote ...' summary line, got %q", stdout)
	}
}

// TestDocCanvasOutputJSON checks that --output json makes the write summary a
// parseable JSON object (path + counts) instead of the plaintext line, so a
// script can consume it like every other Mazarbul command.
func TestDocCanvasOutputJSON(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	aliceName := runEntityCreateCmd(t, dir, "person", "Sarah Vance")
	zurichName := runEntityCreateCmd(t, dir, "place", "Zurich")
	alice := strings.TrimPrefix(aliceName, "entities/")
	zurich := strings.TrimPrefix(zurichName, "entities/")
	_ = runRelateCmd(t, dir, alice, "works_at", zurich)

	out := filepath.Join(dir, "g.canvas")
	var outBuf, errBuf bytes.Buffer
	root := makeDocCanvasRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "doc", "canvas", "Sarah Vance", "--hops", "1", "-o", out})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("doc canvas --output json: %v (stderr=%q)", err, errBuf.String())
	}

	var summary struct {
		Path      string `json:"path"`
		Nodes     int    `json:"nodes"`
		Edges     int    `json:"edges"`
		Documents int    `json:"documents"`
	}
	if uerr := json.Unmarshal(outBuf.Bytes(), &summary); uerr != nil {
		t.Fatalf("stdout is not a JSON summary: %v\n%s", uerr, outBuf.String())
	}
	if summary.Path != out {
		t.Errorf("summary.path = %q, want %q", summary.Path, out)
	}
	if summary.Nodes < 2 {
		t.Errorf("summary.nodes = %d, want >= 2 (seed + neighbor)", summary.Nodes)
	}
}

// TestDocCanvasRejectsBadHops checks the client-side hops bound. The check runs
// before any tool round-trip or config read, so no live backend is needed.
func TestDocCanvasRejectsBadHops(t *testing.T) {
	t.Parallel()
	_, stderr, err := runDocCanvasCmd(t, t.TempDir(), "whatever", "--hops", "0")
	if err == nil {
		t.Fatal("expected non-zero exit for --hops 0")
	}
	if !strings.Contains(stderr, "hops") {
		t.Errorf("expected a hops range error, got %q", stderr)
	}
}

func TestDocCanvasHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeDocCanvasRoot(&buf, nil)
	root.SetArgs([]string{"doc", "canvas", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("doc canvas help output is empty")
	}
	for _, want := range []string{"Examples:", "maz doc canvas", "--hops", "-o", "--at", "--as-of", "--include-inactive"} {
		if !strings.Contains(out, want) {
			t.Errorf("doc canvas help missing %q\n%s", want, out)
		}
	}
}

// TestDocCanvasRejectsBadTimeAnchor pins the exit-code contract for the threaded
// anchor: a malformed --at is an expected failure (E_VALIDATION) that exits 1.
// The exportCanvas tool validates the anchor before resolving the seed, so this
// holds without a matching entity in the store.
func TestDocCanvasRejectsBadTimeAnchor(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)
	_, stderr, err := runDocCanvasCmd(t, dir, "Whoever", "--at", "not-a-time")
	if err == nil {
		t.Fatal("doc canvas --at not-a-time: expected a non-zero exit")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Fatalf("doc canvas --at not-a-time exit = %v, want silentError code 1", err)
	}
	if !strings.Contains(stderr, "at") {
		t.Errorf("doc canvas --at not-a-time stderr = %q, want a parse-'at' validation error", stderr)
	}
}

func TestCanvasSlug(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"entities/01ARZ3NDEK", "01arz3ndek"},
		{"Sarah Vance", "sarah-vance"},
		{"  multiple   spaces ", "multiple-spaces"},
		{"weird/+@chars!", "weird-chars"},
		{"", "canvas"},
		{"!!!", "canvas"},
	}
	for _, tc := range cases {
		if got := canvasSlug(tc.in); got != tc.want {
			t.Errorf("canvasSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
