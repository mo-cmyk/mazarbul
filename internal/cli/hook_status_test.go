package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/connect"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
)

// writeClaudeSettings writes a ~/.claude/settings.json under home carrying the
// Mazarbul Stop and SessionStart hook entries, built with the same upsert helper
// `maz connect` uses so the structural detection under test matches reality.
func writeClaudeSettings(t *testing.T, home string) {
	t.Helper()
	doc := map[string]any{}
	connect.UpsertHookEntry(doc, "Stop", "maz hook stop")
	connect.UpsertHookEntry(doc, "SessionStart", "maz hook session-start --warm-start")
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}

// seedHookEpisodes appends hook-sourced capture episodes (plus one unrelated
// episode that must be excluded from the capture count) to the event log under
// dataDir, then closes the log so the writes are durable for the command to
// read back through its own log handle.
func seedHookEpisodes(t *testing.T, dataDir string, texts ...string) {
	t.Helper()
	ctx := context.Background()
	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	events := make([]kernel.Event, 0, len(texts)+1)
	for _, text := range texts {
		payload := json.RawMessage(`{"text":` + mustJSONString(t, text) +
			`,"source":"hook:claude-code/stop","confidence":0.6}`)
		events = append(events, kernel.Event{
			Kind:       "episode",
			Source:     "hook:claude-code/stop",
			Confidence: 0.6,
			Payload:    payload,
		})
	}
	// A deliberate (non-hook) remember must NOT be counted as a capture.
	events = append(events, kernel.Event{
		Kind:       "episode",
		Source:     "cli:remember",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"a deliberate note","source":"cli:remember","confidence":1.0}`),
	})

	if _, err := log.Append(ctx, events); err != nil {
		_ = closeFn()
		t.Fatalf("append episodes: %v", err)
	}
	if err := closeFn(); err != nil {
		t.Fatalf("close log: %v", err)
	}
}

// mustJSONString quotes s as a JSON string literal.
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal string: %v", err)
	}
	return string(b)
}

// runHookStatusCmd executes `maz hook status <extraArgs...>` against an isolated
// HOME (settings.json) and data dir, returning stdout, stderr, and the exit
// error.
func runHookStatusCmd(t *testing.T, home, dataDir string, extraArgs ...string) (string, string, error) {
	t.Helper()
	t.Setenv("HOME", home)

	var outBuf, errBuf bytes.Buffer
	root := makeHookRoot(nil, &outBuf, &errBuf)
	args := append([]string{"hook", "status", "--data-dir", dataDir}, extraArgs...)
	root.SetArgs(args)
	err := root.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestHookStatusHelpIncludesExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeHookRoot(nil, &buf, nil)
	root.SetArgs([]string{"hook", "status", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("hook status --help: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"Examples:", "maz hook status", "--json"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("help missing %q\n%s", want, out)
		}
	}
}

func TestHookStatusInstalledWithCaptures(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	writeClaudeSettings(t, home)
	seedHookEpisodes(t, dataDir, "ship the release", "fix the tick handler")

	out, errOut, err := runHookStatusCmd(t, home, dataDir, "--output", "text")
	if err != nil {
		t.Fatalf("hook status: %v (stderr=%s)", err, errOut)
	}

	// Both hooks detected as installed.
	if c := bytes.Count([]byte(out), []byte("installed")); c < 2 {
		t.Errorf("expected both hooks reported installed, output:\n%s", out)
	}
	if bytes.Contains([]byte(out), []byte("not installed")) {
		t.Errorf("did not expect a not-installed marker when both hooks present:\n%s", out)
	}
	for _, want := range []string{"Stop hook", "SessionStart hook", "Capture episodes", "2", "hook:claude-code/stop", "Configured in ~/.claude/settings.json"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

func TestHookStatusJSONShape(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	writeClaudeSettings(t, home)
	seedHookEpisodes(t, dataDir, "ship the release", "fix the tick handler")

	out, errOut, err := runHookStatusCmd(t, home, dataDir, "--json")
	if err != nil {
		t.Fatalf("hook status --json: %v (stderr=%s)", err, errOut)
	}

	var got hookStatusReport
	if uerr := json.Unmarshal([]byte(out), &got); uerr != nil {
		t.Fatalf("parse JSON: %v\n%s", uerr, out)
	}
	if !got.StopInstalled || !got.SessionStartInstalled {
		t.Errorf("want both hooks installed, got stop=%v sessionStart=%v", got.StopInstalled, got.SessionStartInstalled)
	}
	if got.CaptureEpisodeCount != 2 {
		t.Errorf("captureEpisodeCount = %d, want 2", got.CaptureEpisodeCount)
	}
	if got.Newest == nil || got.LastFired == nil {
		t.Fatalf("want newest + lastFired populated, got newest=%v lastFired=%v", got.Newest, got.LastFired)
	}
	if got.Newest.Source != "hook:claude-code/stop" {
		t.Errorf("newest source = %q, want hook:claude-code/stop", got.Newest.Source)
	}
	if got.ConfigPath == "" {
		t.Error("configPath should be populated")
	}
}

func TestHookStatusNotInstalledGraceful(t *testing.T) {
	home := t.TempDir() // no ~/.claude/settings.json
	dataDir := t.TempDir()

	out, errOut, err := runHookStatusCmd(t, home, dataDir, "--output", "text")
	if err != nil {
		t.Fatalf("absent settings.json must exit 0, got err=%v (stderr=%s)", err, errOut)
	}
	if !bytes.Contains([]byte(out), []byte("not installed · run `maz connect claude-code --capture`")) {
		t.Errorf("missing not-installed hint:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("Configured in ~/.claude/settings.json")) {
		t.Errorf("missing config-path line:\n%s", out)
	}
}

func TestHookStatusNotInstalledJSONExitsZero(t *testing.T) {
	home := t.TempDir() // no settings.json
	dataDir := t.TempDir()

	out, errOut, err := runHookStatusCmd(t, home, dataDir, "--json")
	if err != nil {
		t.Fatalf("absent settings.json (json) must exit 0, got err=%v (stderr=%s)", err, errOut)
	}
	var got hookStatusReport
	if uerr := json.Unmarshal([]byte(out), &got); uerr != nil {
		t.Fatalf("parse JSON: %v\n%s", uerr, out)
	}
	if got.StopInstalled || got.SessionStartInstalled {
		t.Errorf("want both hooks not installed, got stop=%v sessionStart=%v", got.StopInstalled, got.SessionStartInstalled)
	}
	if got.CaptureEpisodeCount != 0 {
		t.Errorf("captureEpisodeCount = %d, want 0", got.CaptureEpisodeCount)
	}
	if got.Newest != nil || got.LastFired != nil {
		t.Errorf("want no newest/lastFired with zero captures, got newest=%v lastFired=%v", got.Newest, got.LastFired)
	}
}
