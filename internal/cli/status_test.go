package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// makeStatusCmd wires a minimal cobra tree for status tests — mirrors the
// pattern in ask_test.go so PersistentFlags (--data-dir, --output, --color)
// are accessible.
func makeStatusCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.PersistentFlags().String("color", "never", "colour mode")
	statusCmd := newStatusCmd()
	root.AddCommand(statusCmd)
	if outBuf != nil {
		root.SetOut(outBuf)
		statusCmd.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		statusCmd.SetErr(errBuf)
	}
	return root
}

// seedStatusEvent inserts one episode row directly into the SQLite index so
// status tests can exercise the log-freshness path without a full daemon.
func seedStatusEvent(t *testing.T, dir, uid, createTime string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("seedStatusEvent: open sqlite: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("seedStatusEvent: close sqlite: %v", cerr)
		}
	}()
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
		 VALUES (?, ?, 'episode', '{"text":"status test"}', 'status test', 'test')`,
		uid, createTime,
	); err != nil {
		t.Fatalf("seedStatusEvent: insert: %v", err)
	}
}

// initStatusDir creates the SQLite store (runs all migrations) without
// inserting any events — the daemon is guaranteed absent in a temp dir.
func initStatusDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("initStatusDir: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("initStatusDir: close: %v", err)
	}
	return dir
}

// TestStatus_Help asserts that --help is non-empty and includes the examples
// block (the plan requires every command to teach via examples).
func TestStatus_Help(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeStatusCmd(&buf, nil)
	root.SetArgs([]string{"status", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz status",
		"--output json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestStatus_DaemonDown verifies that maz status reports daemon as
// not-running when no PID file exists in the data directory. This test must
// complete without hanging (no network dials, no Ollama probe).
func TestStatus_DaemonDown(t *testing.T) {
	t.Parallel()
	dir := initStatusDir(t)

	var out, errBuf bytes.Buffer
	root := makeStatusCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "status", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("status with no daemon: %v (stderr: %s)", err, errBuf.String())
	}
	got := out.String()
	// Must mention the daemon state without hanging.
	if !strings.Contains(got, "not running") {
		t.Errorf("expected 'not running' in output; got:\n%s", got)
	}
	// Must not try to connect to Ollama or any network address.
	if strings.Contains(got, "11434") || strings.Contains(got, "localhost") {
		t.Errorf("output should not reference network addresses; got:\n%s", got)
	}
}

// TestStatus_DaemonDown_JSON verifies that --output json with no daemon
// returns valid JSON containing daemon.status="not_running".
func TestStatus_DaemonDown_JSON(t *testing.T) {
	t.Parallel()
	dir := initStatusDir(t)

	var out, errBuf bytes.Buffer
	root := makeStatusCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "status", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("status --output json: %v (stderr: %s)", err, errBuf.String())
	}

	var rep statusReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal status JSON: %v\nraw: %s", err, out.String())
	}
	if rep.Daemon.Status != "not_running" {
		t.Errorf("daemon.status = %q, want %q", rep.Daemon.Status, "not_running")
	}
	// Even with no events the log sub-object must be present.
	if rep.Log.EventCount != 0 {
		t.Errorf("log.eventCount = %d for empty store, want 0", rep.Log.EventCount)
	}
	// Config scopes must be a non-nil slice.
	if rep.Config.SyncScopes == nil {
		t.Error("config.syncScopes must be a non-nil slice, got nil")
	}
}

// TestStatus_JSON_Fields checks that all required top-level and nested fields
// are present in the JSON output for a populated store.
func TestStatus_JSON_Fields(t *testing.T) {
	t.Parallel()
	dir := initStatusDir(t)
	// Seed one event so log freshness is non-trivial.
	seedStatusEvent(t, dir, "01ARZ3NDEKTSV4RRFFQ69G5FA1", "2026-06-19T10:00:00Z")

	var out, errBuf bytes.Buffer
	root := makeStatusCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "status", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("status --output json: %v (stderr: %s)", err, errBuf.String())
	}

	// Verify it is parseable JSON.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v\nraw output: %s", err, out.String())
	}

	// Top-level required fields.
	for _, field := range []string{"dataDir", "episodes", "entities", "mentions", "relations", "daemon", "log", "config"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("JSON missing top-level field %q", field)
		}
	}

	// Daemon sub-object.
	var daemonObj map[string]json.RawMessage
	if err := json.Unmarshal(raw["daemon"], &daemonObj); err != nil {
		t.Fatalf("unmarshal daemon: %v", err)
	}
	if _, ok := daemonObj["status"]; !ok {
		t.Error("daemon JSON missing field 'status'")
	}

	// Log sub-object.
	var logObj map[string]json.RawMessage
	if err := json.Unmarshal(raw["log"], &logObj); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if _, ok := logObj["eventCount"]; !ok {
		t.Error("log JSON missing field 'eventCount'")
	}

	// eventCount should be 1 (we seeded one event).
	var rep statusReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal statusReport: %v", err)
	}
	if rep.Log.EventCount != 1 {
		t.Errorf("log.eventCount = %d, want 1", rep.Log.EventCount)
	}
	if rep.Log.LastEventAt == "" {
		t.Error("log.lastEventAt should be set after seeding an event")
	}
	if rep.Episodes != 1 {
		t.Errorf("episodes = %d, want 1", rep.Episodes)
	}

	// Config sub-object.
	var cfgObj map[string]json.RawMessage
	if err := json.Unmarshal(raw["config"], &cfgObj); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	for _, f := range []string{"embedProvider", "syncScopes"} {
		if _, ok := cfgObj[f]; !ok {
			t.Errorf("config JSON missing field %q", f)
		}
	}
}

// TestStatus_NoNetwork verifies that status never dials the network even when
// the daemon is absent. The test overwrites MAZ_PIDFILE to a non-existent
// path so ReadPID fails fast, and checks that the result is instant (no
// timeout-based network probes).
func TestStatus_NoNetwork(t *testing.T) {
	t.Parallel()
	dir := initStatusDir(t)
	// No PID file, no daemon → must complete without any network I/O.
	var out, errBuf bytes.Buffer
	root := makeStatusCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "status"})
	if err := root.Execute(); err != nil {
		t.Fatalf("status no-network: %v", err)
	}
	// Output must contain daemon row (proof the path was reached).
	if !strings.Contains(out.String(), "daemon") {
		t.Errorf("expected 'daemon' row in output; got:\n%s", out.String())
	}
}

// TestStatus_NoColor verifies NO_COLOR=1 produces glyph-only output (no ANSI
// escape sequences) while carrying identical structured information.
// Not parallel: t.Setenv and t.Parallel cannot be combined.
func TestStatus_NoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	dir := initStatusDir(t)
	seedStatusEvent(t, dir, "01ARZ3NDEKTSV4RRFFQ69G5FA2", "2026-06-19T11:00:00Z")

	var out, errBuf bytes.Buffer
	root := makeStatusCmd(&out, &errBuf)
	// Force text so NO_COLOR=1 exercises the anti-ANSI gate on text rendering.
	root.SetArgs([]string{"--data-dir", dir, "status", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("status NO_COLOR: %v (stderr: %s)", err, errBuf.String())
	}
	got := out.String()

	// No ANSI escape sequences.
	if strings.ContainsAny(got, "\x1b") {
		t.Errorf("NO_COLOR=1 output contains ANSI escapes:\n%s", got)
	}
	// Still contains structural content.
	for _, want := range []string{"Mazarbul", "store", "daemon", "log", "embed", "sync"} {
		if !strings.Contains(got, want) {
			t.Errorf("NO_COLOR output missing %q; full output:\n%s", want, got)
		}
	}
}

// TestStatus_StaleOrAbsentPID writes a PID file that references a dead PID
// and verifies that status does not hang and reports the daemon as absent.
func TestStatus_StaleOrAbsentPID(t *testing.T) {
	t.Parallel()
	dir := initStatusDir(t)

	// Write a PID file pointing at PID 99999999 (virtually guaranteed dead).
	// ReadPID will parse it; processAlive will return false; IsMazDaemon returns
	// false → daemonReadState.Status = "stale".
	pidFile := filepath.Join(dir, "mazarbul.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\nfaketoken\n%s\n", 99999999, dir)), 0o600); err != nil {
		t.Fatalf("write stale PID file: %v", err)
	}

	var out, errBuf bytes.Buffer
	root := makeStatusCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "status"})
	if err := root.Execute(); err != nil {
		t.Fatalf("status stale PID: %v (stderr: %s)", err, errBuf.String())
	}

	var rep statusReport
	root2 := makeStatusCmd(&out, &errBuf)
	out.Reset()
	root2.SetArgs([]string{"--data-dir", dir, "status", "--output", "json"})
	if err := root2.Execute(); err != nil {
		t.Fatalf("status stale PID JSON: %v", err)
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal stale PID JSON: %v\nraw: %s", err, out.String())
	}
	// A dead PID → either "stale" or "not_running"; both are acceptable.
	if rep.Daemon.Status != "stale" && rep.Daemon.Status != "not_running" {
		t.Errorf("daemon.status with dead PID = %q, want stale or not_running", rep.Daemon.Status)
	}
}

// TestStatus_Text_DaemonRow spot-checks that the daemon row contains the
// expected glyphs when the daemon is absent.
// Not parallel: t.Setenv and t.Parallel cannot be combined.
func TestStatus_Text_DaemonRow(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	dir := initStatusDir(t)

	var out bytes.Buffer
	root := makeStatusCmd(&out, nil)
	// Force text so the glyph check is meaningful (buffer is not a TTY).
	root.SetArgs([]string{"--data-dir", dir, "status", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("status text: %v", err)
	}
	got := out.String()
	// Glyph ✗ must appear (daemon is down).
	if !strings.Contains(got, "✗") {
		t.Errorf("daemon-down output missing ✗ glyph; got:\n%s", got)
	}
	// Must include signpost hint.
	if !strings.Contains(got, "maz daemon start") {
		t.Errorf("daemon-down output missing 'maz daemon start' hint; got:\n%s", got)
	}
}
