package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
)

// makeDocRehomeCmd wires a cobra command tree in the same shape as production
// so PersistentFlags (--data-dir, --output, --json, --no-tui) are accessible
// and the data-dir flag can be injected.
func makeDocRehomeCmd(dataDir string, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", dataDir, "data directory")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	doc := newDocCmd()
	root.AddCommand(doc)
	if outBuf != nil {
		root.SetOut(outBuf)
		doc.SetOut(outBuf)
		for _, sub := range doc.Commands() {
			sub.SetOut(outBuf)
		}
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		doc.SetErr(errBuf)
		for _, sub := range doc.Commands() {
			sub.SetErr(errBuf)
		}
	}
	return root
}

// TestDocRehomeHelp verifies that the --help output mentions the key concepts
// an operator needs: about:/factRehomed events, daemon stop requirement, and
// idempotency.
func TestDocRehomeHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeDocRehomeCmd("", &buf, nil)
	root.SetArgs([]string{"doc", "rehome", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"about:",
		"factRehomed",
		"maz daemon stop",
		"idempotent",
		"--dry-run",
		"--yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestDocRehomeScriptedRequiresYes mirrors TestRebuildScriptedRequiresYes:
// a non-TTY invocation without --yes must exit non-zero and print the --yes
// hint to stderr, never blocking on stdin.
func TestDocRehomeScriptedRequiresYes(t *testing.T) {
	t.Parallel()

	var outBuf, errBuf bytes.Buffer
	// The confirmation gate runs before the store opens, so any data-dir works.
	root := makeDocRehomeCmd(t.TempDir(), &outBuf, &errBuf)
	root.SetArgs([]string{"doc", "rehome"}) // no --yes; non-TTY stdout triggers scripted path
	err := root.Execute()

	var re *rehomeExitError
	if !errors.As(err, &re) {
		t.Fatalf("expected rehomeExitError, got %T: %v", err, err)
	}
	if re.Code() != 1 {
		t.Errorf("exit code = %d, want 1", re.Code())
	}
	if !strings.Contains(errBuf.String(), "--yes") {
		t.Errorf("stderr missing --yes hint: %q", errBuf.String())
	}
}

// TestDocRehomeDryRunNoOp opens a real (empty) dispatcher and verifies that
// --dry-run on a store with no about: documents reports factsRehomed=0 and
// exits cleanly. The JSON summary shape is also verified.
func TestDocRehomeDryRunNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: opens a real dispatcher")
	}
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()

	// Seed an empty but valid store. Embedder is nil: vector inserts are skipped,
	// which is fine for a dry-run test that never writes any facts.
	dis, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	if err := dis.Close(); err != nil {
		t.Fatalf("dis.Close: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeDocRehomeCmd(dataDir, &outBuf, &errBuf)
	// Force JSON output so we can parse the summary shape deterministically.
	root.SetArgs([]string{"doc", "rehome", "--dry-run", "--json"})
	if execErr := root.Execute(); execErr != nil {
		t.Fatalf("Execute: %v (stderr: %s)", execErr, errBuf.String())
	}

	line := strings.TrimSpace(outBuf.String())
	if line == "" {
		t.Fatal("no JSON output")
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(line), &summary); err != nil {
		t.Fatalf("parse JSON summary: %v (%q)", err, line)
	}

	for _, key := range []string{"factsOnDocuments", "factsRehomed", "documentsAffected", "dryRun"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary missing key %q; got %v", key, summary)
		}
	}
	if summary["dryRun"] != true {
		t.Errorf("dryRun = %v, want true", summary["dryRun"])
	}
	if summary["factsRehomed"] != float64(0) {
		t.Errorf("factsRehomed = %v, want 0 (empty store is a no-op)", summary["factsRehomed"])
	}
}

// TestDocRehomeYesOnEmptyStore verifies that --yes on an empty store succeeds
// with zero facts re-homed (idempotent no-op) and emits the expected JSON shape.
func TestDocRehomeYesOnEmptyStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: opens a real dispatcher + event log")
	}
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()

	// Seed a store with a minimal event so the event log file exists on disk.
	// Embedder is nil: vector inserts are skipped for this test.
	dis, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	log := kernel.NewFileEventLog(dataDir, kernel.NoopOntology{}, kernel.WithStorage(dis))
	// Append a minimal episode. The NoopOntology skips schema validation, so
	// the minimal payload is accepted. We only need the log file to exist on
	// disk so that kernelboot.OpenLog can open it during the rehome run.
	_, _ = log.Append(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload: []byte(`{"uid":"` + ulid.Make().String() +
			`","content":"init","createTime":"` + time.Now().UTC().Format(time.RFC3339) + `"}`),
	}})
	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}
	if err := dis.Close(); err != nil {
		t.Fatalf("dis.Close: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeDocRehomeCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"doc", "rehome", "--yes", "--json"})
	if execErr := root.Execute(); execErr != nil {
		t.Fatalf("Execute: %v (stderr: %s)", execErr, errBuf.String())
	}

	line := strings.TrimSpace(outBuf.String())
	if line == "" {
		t.Fatal("no JSON output")
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(line), &summary); err != nil {
		t.Fatalf("parse JSON summary: %v (%q)", err, line)
	}

	for _, key := range []string{"factsOnDocuments", "factsRehomed", "documentsAffected", "dryRun"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary missing key %q; got %v", key, summary)
		}
	}
	if summary["dryRun"] != false {
		t.Errorf("dryRun = %v, want false", summary["dryRun"])
	}
	if summary["factsRehomed"] != float64(0) {
		t.Errorf("factsRehomed = %v, want 0 (no about: documents)", summary["factsRehomed"])
	}
}
