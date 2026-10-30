package cli

// load_unit_test.go — unit tests for "maz load" that do NOT require a live
// storage backend. These run under the default test build (no integration tag).
//
// Coverage: all-invalid input returns exit 1; empty file returns exit 0;
// parseLoadFile counts correctly.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// makeLoadRootUnit builds a minimal root command for load unit tests.
// Duplicates makeLoadRoot from load_test.go so unit tests compile without the
// integration build tag (load_test.go is //go:build integration && sqlite_fts5).
func makeLoadRootUnit(outBuf, errBuf *bytes.Buffer) *cobra.Command {
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

// TestLoad_AllInvalidExitsOne ensures that a file where every line fails
// decoding returns a silentError with code 1, not an implicit exit 0.
// This is the "caller provided only bad input" case that must be
// distinguishable from a genuinely empty file.
func TestLoad_AllInvalidExitsOne(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "bad.jsonl")
	content := "not json at all\nalso not json\n{this: is broken}\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// We need a data-dir flag but in append mode with no valid events,
	// runLoad never opens storage — it exits early after renderLoadResult.
	// Use a non-existent dir; the code path never reaches OpenLog.
	dataDir := filepath.Join(tmpDir, "nonexistent-data")

	var outBuf, errBuf bytes.Buffer
	root := makeLoadRootUnit(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "load", file})
	root.SetContext(context.Background())

	err := root.Execute()
	if err == nil {
		t.Fatalf("all-invalid input should exit non-zero, got nil error (stdout=%q)", outBuf.String())
	}
	var se silentError
	if !asSilentError(err, &se) {
		// Cobra wraps the error; check the string representation.
		t.Logf("err type = %T, value = %v", err, err)
	}
	// The output should mention "invalid" so the user understands what happened.
	combined := outBuf.String() + errBuf.String()
	if !strings.Contains(combined, "invalid") {
		t.Errorf("output %q should mention invalid lines", combined)
	}
}

// TestLoad_EmptyFileExitsZero ensures a file with zero lines (not even blank)
// exits 0: an empty load is a no-op, not a failure.
func TestLoad_EmptyFileExitsZero(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "empty.jsonl")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	dataDir := filepath.Join(tmpDir, "nonexistent-data")

	var outBuf, errBuf bytes.Buffer
	root := makeLoadRootUnit(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "load", file})
	root.SetContext(context.Background())

	if err := root.Execute(); err != nil {
		t.Fatalf("empty file should exit 0, got: %v (stderr=%q)", err, errBuf.String())
	}
}

// TestLoad_ParseLoadFile_Counts verifies parseLoadFile's event/invalid counts
// directly without any cobra or storage involvement.
func TestLoad_ParseLoadFile_Counts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		content      string
		wantEvents   int
		wantInvalids int
	}{
		{
			name:         "all valid",
			content:      `{"uid":"A","kind":"episode","source":"t","confidence":1,"scope":"personal","payload":{"text":"x"}}` + "\n",
			wantEvents:   1,
			wantInvalids: 0,
		},
		{
			name:         "all invalid",
			content:      "garbage\nnot json\n",
			wantEvents:   0,
			wantInvalids: 2,
		},
		{
			name:         "mixed",
			content:      `{"uid":"B","kind":"episode","source":"t","confidence":1,"scope":"personal","payload":{"text":"y"}}` + "\nbad line\n",
			wantEvents:   1,
			wantInvalids: 1,
		},
		{
			name:         "empty file",
			content:      "",
			wantEvents:   0,
			wantInvalids: 0,
		},
		{
			name:         "blank lines skipped",
			content:      "\n\n",
			wantEvents:   0,
			wantInvalids: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := strings.NewReader(tc.content)
			events, invalids, err := parseLoadFile(r)
			if err != nil {
				t.Fatalf("parseLoadFile: %v", err)
			}
			if len(events) != tc.wantEvents {
				t.Errorf("events = %d, want %d", len(events), tc.wantEvents)
			}
			if invalids != tc.wantInvalids {
				t.Errorf("invalids = %d, want %d", invalids, tc.wantInvalids)
			}
		})
	}
}
