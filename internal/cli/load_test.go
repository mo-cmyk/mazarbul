//go:build integration && sqlite_fts5

package cli

// load_test.go — integration tests for "maz load".
//
// Each test boots the in-process backend (migrations + ladybug + sqlite-vec)
// so it is gated behind //go:build integration && sqlite_fts5 and respects
// skipIfShortLiveBackend.  Run with:
//
//	go test -tags='integration sqlite_fts5' -timeout 5m ./internal/cli/...

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
)

// makeLoadRoot wires a minimal cobra root for load tests.
func makeLoadRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newLoadCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// writeEventJSONL writes events to a JSONL temp file and returns the path.
func writeEventJSONL(t *testing.T, events []kernel.Event) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "events-*.jsonl")
	if err != nil {
		t.Fatalf("create tmp jsonl: %v", err)
	}
	enc := json.NewEncoder(f)
	for i := range events {
		if err := enc.Encode(events[i]); err != nil {
			t.Fatalf("encode event: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tmp jsonl: %v", err)
	}
	return f.Name()
}

// countLogEvents reads the log and returns the number of events with the
// given kind. Returns total count when kind is empty.
func countLogEvents(t *testing.T, dataDir string, kind string) int {
	t.Helper()
	ctx := context.Background()
	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			t.Logf("close log: %v", cerr)
		}
	}()
	n := 0
	for ev, ferr := range log.Read(ctx, kernel.ReadOpts{}) {
		if ferr != nil {
			t.Fatalf("read log: %v", ferr)
		}
		if kind == "" || string(ev.Kind) == kind {
			n++
		}
	}
	return n
}

// TestLoad_AppendMode adds events and verifies they appear in the log.
func TestLoad_AppendMode(t *testing.T) {
	skipIfShortLiveBackend(t)
	dir := t.TempDir()
	seedConfig(t, dir)

	ev := kernel.Event{
		UID:        "01LOADTEST0000000000000001",
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		Payload:    []byte(`{"text":"load append test","source":"test","confidence":1.0}`),
	}
	file := writeEventJSONL(t, []kernel.Event{ev})

	var outBuf, errBuf bytes.Buffer
	root := makeLoadRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "load", file})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("load append: %v (stderr=%q)", err, errBuf.String())
	}

	var res loadResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &res); err != nil {
		t.Fatalf("decode output: %v\nraw: %s", err, outBuf.String())
	}
	if res.Appended != 1 {
		t.Errorf("appended = %d, want 1 (result: %+v)", res.Appended, res)
	}
	if res.DryRun {
		t.Error("dryRun should be false in normal mode")
	}
	// Verify the event actually landed in the log.
	if n := countLogEvents(t, dir, "episode"); n < 1 {
		t.Errorf("expected at least 1 episode in log after load, got %d", n)
	}
}

// TestLoad_MergeMode skips duplicate UIDs on second load.
func TestLoad_MergeMode(t *testing.T) {
	skipIfShortLiveBackend(t)
	dir := t.TempDir()
	seedConfig(t, dir)

	ev := kernel.Event{
		UID:        "01LOADTEST0000000000000002",
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		Payload:    []byte(`{"text":"merge mode test","source":"test","confidence":1.0}`),
	}
	file := writeEventJSONL(t, []kernel.Event{ev})

	runLoad := func() loadResult {
		t.Helper()
		var outBuf, errBuf bytes.Buffer
		root := makeLoadRoot(&outBuf, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "--output", "json", "load", file, "--mode", "merge"})
		root.SetContext(context.Background())
		if err := root.Execute(); err != nil {
			t.Fatalf("load merge: %v (stderr=%q)", err, errBuf.String())
		}
		var res loadResult
		if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &res); err != nil {
			t.Fatalf("decode output: %v\nraw: %s", err, outBuf.String())
		}
		return res
	}

	// First load: should append the event.
	first := runLoad()
	if first.Appended != 1 || first.Skipped != 0 {
		t.Errorf("first load: appended=%d skipped=%d, want 1/0", first.Appended, first.Skipped)
	}

	// Second load (same file, merge mode): the UID is already present, should skip.
	second := runLoad()
	if second.Appended != 0 || second.Skipped != 1 {
		t.Errorf("second load (merge): appended=%d skipped=%d, want 0/1", second.Appended, second.Skipped)
	}
	// Verify the warning code is present.
	if len(second.Warnings) == 0 {
		t.Error("expected a MAZ-LOAD-001 warning on the merged load")
	} else {
		found := false
		for _, w := range second.Warnings {
			if w.Code == "MAZ-LOAD-001" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected warning code MAZ-LOAD-001, got %v", second.Warnings)
		}
	}
}

// TestLoad_DryRun reports counts but writes no events.
func TestLoad_DryRun(t *testing.T) {
	skipIfShortLiveBackend(t)
	dir := t.TempDir()
	seedConfig(t, dir)

	ev := kernel.Event{
		UID:        "01LOADTEST0000000000000003",
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		Payload:    []byte(`{"text":"dry run test","source":"test","confidence":1.0}`),
	}
	file := writeEventJSONL(t, []kernel.Event{ev})

	before := countLogEvents(t, dir, "episode")

	var outBuf, errBuf bytes.Buffer
	root := makeLoadRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "load", file, "--dry-run"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("load --dry-run: %v (stderr=%q)", err, errBuf.String())
	}

	var res loadResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &res); err != nil {
		t.Fatalf("decode output: %v\nraw: %s", err, outBuf.String())
	}
	if !res.DryRun {
		t.Error("dryRun should be true")
	}
	if res.Appended != 1 {
		t.Errorf("dryRun appended count = %d, want 1 (should report what would be written)", res.Appended)
	}

	// The log must be unchanged: no new episode events.
	after := countLogEvents(t, dir, "episode")
	if after != before {
		t.Errorf("dry-run must not write: log had %d episodes before, %d after", before, after)
	}
}

// TestLoad_OverwriteRejected verifies that --mode overwrite returns a clear
// actionable error and non-zero exit code.
func TestLoad_OverwriteRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	file := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeLoadRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "load", file, "--mode", "overwrite"})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("--mode overwrite should fail but exited 0")
	}
	if !strings.Contains(errBuf.String(), "overwrite") {
		t.Errorf("error message should mention 'overwrite', got: %q", errBuf.String())
	}
}

// TestLoad_InvalidLinesCountedNotFatal verifies that a JSONL file with a
// malformed line still loads the valid lines and reports the invalid count.
func TestLoad_InvalidLinesCountedNotFatal(t *testing.T) {
	skipIfShortLiveBackend(t)
	dir := t.TempDir()
	seedConfig(t, dir)

	// Write a file with one valid event and one garbage line.
	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "mixed.jsonl")
	valid := `{"uid":"01LOADTEST0000000000000004","kind":"episode","source":"test","confidence":1.0,"scope":"personal","payload":{"text":"valid line","source":"test","confidence":1.0}}`
	invalid := `not json at all`
	content := valid + "\n" + invalid + "\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeLoadRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "load", file})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("load mixed file: %v (stderr=%q)", err, errBuf.String())
	}

	var res loadResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &res); err != nil {
		t.Fatalf("decode output: %v\nraw: %s", err, outBuf.String())
	}
	if res.Appended != 1 {
		t.Errorf("appended = %d, want 1", res.Appended)
	}
	if res.Invalid != 1 {
		t.Errorf("invalid = %d, want 1", res.Invalid)
	}
	// Expect MAZ-LOAD-002 warning.
	found := false
	for _, w := range res.Warnings {
		if w.Code == "MAZ-LOAD-002" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected MAZ-LOAD-002 warning for invalid record, got %v", res.Warnings)
	}
}
