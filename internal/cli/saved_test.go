package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// makeSavedCmd wires a minimal cobra command tree with the same PersistentFlags
// as production so --data-dir, --json, --no-tui, and --output resolve correctly.
func makeSavedCmd(dataDir string, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", dataDir, "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.PersistentFlags().String("output", "", "output format: text|json|ndjson|csv|kv")
	saved := newSavedCmd()
	root.AddCommand(saved)
	if outBuf != nil {
		root.SetOut(outBuf)
		saved.SetOut(outBuf)
		for _, sub := range saved.Commands() {
			sub.SetOut(outBuf)
		}
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		saved.SetErr(errBuf)
		for _, sub := range saved.Commands() {
			sub.SetErr(errBuf)
		}
	}
	return root
}

// TestSavedHelp verifies the top-level `maz saved --help` output is non-empty and
// contains the expected examples.
func TestSavedHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSavedCmd("", &buf, nil)
	root.SetArgs([]string{"saved", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz saved add",
		"maz saved list",
		"maz saved recall",
		"maz saved remove",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestSavedAddHelpContainsExamples verifies the add sub-command help has examples.
func TestSavedAddHelpContainsExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSavedCmd("", &buf, nil)
	root.SetArgs([]string{"saved", "add", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"Examples:", "--query", "--limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("add help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestSavedListHelpContainsExamples verifies the list sub-command help has examples.
func TestSavedListHelpContainsExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSavedCmd("", &buf, nil)
	root.SetArgs([]string{"saved", "list", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	if !strings.Contains(out, "Examples:") {
		t.Errorf("list help output missing Examples:\nfull:\n%s", out)
	}
}

// TestSavedAddAndList verifies the add→list round-trip in text mode.
func TestSavedAddAndList(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	// Add a query.
	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{
		"saved", "add", "standup",
		"--query", "daily standup", "--limit", "5",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved add: %v\nstderr: %s", err, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "standup") {
		t.Errorf("add output missing name: %q", outBuf.String())
	}

	// List must surface the just-added entry.
	outBuf.Reset()
	errBuf.Reset()
	root2 := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root2.SetArgs([]string{"saved", "list"})
	if err := root2.Execute(); err != nil {
		t.Fatalf("saved list: %v\nstderr: %s", err, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "standup") {
		t.Errorf("list output missing %q: %q", "standup", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "daily standup") {
		t.Errorf("list output missing query %q: %q", "daily standup", outBuf.String())
	}
}

// TestSavedListJSON verifies that `--output json` produces a parseable JSON array.
func TestSavedListJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	// Seed two entries via config.WriteUserConfig directly (faster than cobra round-trip).
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"alpha": {Query: "alpha query", Limit: 3},
			"beta":  {Query: "beta query"},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "list", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved list --output json: %v\nstderr: %s", err, errBuf.String())
	}

	var entries []savedRecallEntry
	if err := json.Unmarshal(outBuf.Bytes(), &entries); err != nil {
		t.Fatalf("output is not valid JSON array: %v\nraw: %s", err, outBuf.String())
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(entries), entries)
	}
	// Entries are sorted by name; alpha < beta.
	if entries[0].Name != "alpha" || entries[1].Name != "beta" {
		t.Errorf("entries not sorted: %v", entries)
	}
}

// TestSavedListEmptyJSON verifies that an empty store emits `[]` not `null`.
func TestSavedListEmptyJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "list", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved list: %v\nstderr: %s", err, errBuf.String())
	}
	out := strings.TrimSpace(outBuf.String())
	if out != "[]" {
		t.Errorf("empty list must emit `[]`, got: %q", out)
	}
}

// TestSavedShowJSON verifies that `maz saved show <name> --output json` emits a
// parseable JSON object and that missing names exit 1.
func TestSavedShowJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"standup": {Query: "daily standup", Limit: 5, MinScore: 0.1, Scopes: []string{"personal"}},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	t.Run("existing name returns JSON", func(t *testing.T) {
		t.Parallel()
		var outBuf, errBuf bytes.Buffer
		root := makeSavedCmd(dataDir, &outBuf, &errBuf)
		root.SetArgs([]string{"saved", "show", "standup", "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("saved show: %v\nstderr: %s", err, errBuf.String())
		}
		var entry savedRecallEntry
		if err := json.Unmarshal(outBuf.Bytes(), &entry); err != nil {
			t.Fatalf("not valid JSON: %v\nraw: %s", err, outBuf.String())
		}
		if entry.Name != "standup" {
			t.Errorf("name = %q, want %q", entry.Name, "standup")
		}
		if entry.Query != "daily standup" {
			t.Errorf("query = %q, want %q", entry.Query, "daily standup")
		}
		if entry.Limit != 5 {
			t.Errorf("limit = %d, want 5", entry.Limit)
		}
	})

	t.Run("missing name exits 1", func(t *testing.T) {
		t.Parallel()
		var outBuf, errBuf bytes.Buffer
		root := makeSavedCmd(dataDir, &outBuf, &errBuf)
		root.SetArgs([]string{"saved", "show", "nonexistent"})
		err := root.Execute()
		var se silentError
		if !errors.As(err, &se) {
			t.Fatalf("expected silentError, got %T: %v", err, err)
		}
		if se.code != 1 {
			t.Errorf("exit code = %d, want 1", se.code)
		}
	})
}

// TestSavedRemove verifies that removing a known entry exits 0 and removing an
// unknown entry exits 1.
func TestSavedRemove(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"toremove": {Query: "will be removed"},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	t.Run("existing name exits 0", func(t *testing.T) {
		t.Parallel()
		var outBuf, errBuf bytes.Buffer
		root := makeSavedCmd(dataDir, &outBuf, &errBuf)
		root.SetArgs([]string{"saved", "remove", "toremove"})
		if err := root.Execute(); err != nil {
			t.Fatalf("saved remove: %v\nstderr: %s", err, errBuf.String())
		}
		// Verify the entry is gone.
		loaded, loadErr := loadSavedRecalls(dataDir)
		if loadErr != nil {
			t.Fatalf("loadSavedRecalls after remove: %v", loadErr)
		}
		if _, ok := loaded["toremove"]; ok {
			t.Error("entry still present after remove")
		}
	})

	t.Run("missing name exits 1", func(t *testing.T) {
		t.Parallel()
		var outBuf, errBuf bytes.Buffer
		root := makeSavedCmd(dataDir, &outBuf, &errBuf)
		root.SetArgs([]string{"saved", "remove", "doesnotexist"})
		err := root.Execute()
		var se silentError
		if !errors.As(err, &se) {
			t.Fatalf("expected silentError, got %T: %v", err, err)
		}
		if se.code != 1 {
			t.Errorf("exit code = %d, want 1", se.code)
		}
	})
}

// TestSavedAddMissingQuery verifies that omitting --query exits 1 with an error.
func TestSavedAddMissingQuery(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "add", "test-name"})
	err := root.Execute()
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
}

// TestSavedRecallMissingName verifies that `maz saved recall <unknown>` exits 1.
func TestSavedRecallMissingName(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "recall", "nonexistent"})
	err := root.Execute()
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
}

// TestSavedAddJSON verifies that `maz saved add --output json` emits parseable JSON.
func TestSavedAddJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{
		"saved", "add", "myjson",
		"--query", "test query", "--output", "json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved add --output json: %v\nstderr: %s", err, errBuf.String())
	}
	var result map[string]any
	if err := json.Unmarshal(outBuf.Bytes(), &result); err != nil {
		t.Fatalf("output not parseable JSON: %v\nraw: %s", err, outBuf.String())
	}
	if result["ok"] != true {
		t.Errorf("ok = %v, want true", result["ok"])
	}
	if result["name"] != "myjson" {
		t.Errorf("name = %v, want %q", result["name"], "myjson")
	}
}

// TestSavedRemoveJSON verifies that `maz saved remove --output json` emits parseable JSON.
func TestSavedRemoveJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"myjson": {Query: "test"},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "remove", "myjson", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved remove --output json: %v\nstderr: %s", err, errBuf.String())
	}
	var result map[string]any
	if err := json.Unmarshal(outBuf.Bytes(), &result); err != nil {
		t.Fatalf("output not parseable JSON: %v\nraw: %s", err, outBuf.String())
	}
	if result["ok"] != true {
		t.Errorf("ok = %v, want true", result["ok"])
	}
	if result["removed"] != "myjson" {
		t.Errorf("removed = %v, want %q", result["removed"], "myjson")
	}
}

// TestSavedListNDJSON verifies that --output ndjson emits one JSON object per
// line (true NDJSON), not a one-line JSON array.
func TestSavedListNDJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"alpha": {Query: "alpha query"},
			"beta":  {Query: "beta query"},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "list", "--output", "ndjson"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved list --output ndjson: %v\nstderr: %s", err, errBuf.String())
	}

	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 NDJSON lines, got %d:\n%s", len(lines), outBuf.String())
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("line %d is not valid JSON: %v\nline: %s", i, err, line)
		}
		if _, ok := obj["name"]; !ok {
			t.Errorf("line %d missing 'name' field: %s", i, line)
		}
	}
}

// TestSavedListEmptyNDJSON verifies that --output ndjson on an empty store
// produces no output (not a stray [] or null).
func TestSavedListEmptyNDJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "list", "--output", "ndjson"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved list --output ndjson (empty): %v", err)
	}
	if got := strings.TrimSpace(outBuf.String()); got != "" {
		t.Errorf("empty ndjson list must produce no output, got: %q", got)
	}
}

// TestSavedListEmptyCSV verifies that --output csv on an empty store emits
// parseable CSV with a header row (not the "no saved recalls" text message).
// The implementation emits a sentinel empty record so the header columns are
// always present, which produces a header row plus one all-empty data row.
func TestSavedListEmptyCSV(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "list", "--output", "csv"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved list --output csv (empty): %v", err)
	}
	out := outBuf.String()
	if strings.Contains(out, "no saved recalls") {
		t.Errorf("empty csv must not emit text message, got: %q", out)
	}
	r := csv.NewReader(strings.NewReader(out))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\nraw:\n%s", err, out)
	}
	// Must have at least a header row (the sentinel emit writes header + one empty
	// data row, so len >= 1 is the invariant; len == 0 means no output at all).
	if len(records) == 0 {
		t.Errorf("empty csv must emit at least a header row, got no rows:\n%s", out)
	}
	// Header must contain the column names.
	header := records[0]
	hasName, hasQuery := false, false
	for _, col := range header {
		if col == "name" {
			hasName = true
		}
		if col == "query" {
			hasQuery = true
		}
	}
	if !hasName || !hasQuery {
		t.Errorf("csv header %v must contain 'name' and 'query' columns", header)
	}
}

// TestSavedListCSV verifies that --output csv emits a parseable header + data rows.
func TestSavedListCSV(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"standup": {Query: "daily standup", Limit: 5},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "list", "--output", "csv"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved list --output csv: %v\nstderr: %s", err, errBuf.String())
	}

	r := csv.NewReader(strings.NewReader(outBuf.String()))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\nraw:\n%s", err, outBuf.String())
	}
	// Header + at least one data row.
	if len(records) < 2 {
		t.Fatalf("expected header + data rows, got %d rows:\n%s", len(records), outBuf.String())
	}
	// Header must contain "name" and "query".
	header := records[0]
	hasName, hasQuery := false, false
	for _, col := range header {
		if col == "name" {
			hasName = true
		}
		if col == "query" {
			hasQuery = true
		}
	}
	if !hasName || !hasQuery {
		t.Errorf("csv header %v missing name or query column", header)
	}
}

// TestSavedShowCSV verifies that `maz saved show --output csv` emits a
// parseable header + one data row (not text-format "name: value" lines).
func TestSavedShowCSV(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"standup": {Query: "daily standup", Limit: 5, MinScore: 0.1, Scopes: []string{"personal"}},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "show", "standup", "--output", "csv"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved show --output csv: %v\nstderr: %s", err, errBuf.String())
	}

	out := outBuf.String()
	// Must not contain text-format lines.
	if strings.Contains(out, "name:") || strings.Contains(out, "query:") {
		t.Errorf("csv output must not contain text-format lines, got:\n%s", out)
	}
	r := csv.NewReader(strings.NewReader(out))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\nraw:\n%s", err, out)
	}
	// header + one data row.
	if len(records) != 2 {
		t.Fatalf("expected 2 rows (header + data), got %d:\n%s", len(records), out)
	}
}

// TestSavedShowKV verifies that `maz saved show --output kv` emits
// "key: value" lines (not the text-format "name:  value" aligned layout).
func TestSavedShowKV(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		SavedRecalls: config.SavedRecallsTable{
			"standup": {Query: "daily standup"},
		},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeSavedCmd(dataDir, &outBuf, &errBuf)
	root.SetArgs([]string{"saved", "show", "standup", "--output", "kv"})
	if err := root.Execute(); err != nil {
		t.Fatalf("saved show --output kv: %v\nstderr: %s", err, errBuf.String())
	}

	out := outBuf.String()
	// kv format: "key: value" lines, not "key:      value" with extra spaces.
	if !strings.Contains(out, "name: standup") {
		t.Errorf("kv output missing 'name: standup', got:\n%s", out)
	}
	if !strings.Contains(out, "query: daily standup") {
		t.Errorf("kv output missing 'query: daily standup', got:\n%s", out)
	}
}
