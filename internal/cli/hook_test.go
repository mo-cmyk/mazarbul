package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// makeHookRoot wires a minimal cobra root for the `maz hook` subtree, matching
// the persistent-flags shape the command reads via cmd.Root().PersistentFlags()
// and routing stdin/stdout through the parent chain (InOrStdin/OutOrStdout walk
// to the root).
func makeHookRoot(in io.Reader, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newHookCmd())
	if in != nil {
		root.SetIn(in)
	}
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	root.SetContext(context.Background())
	return root
}

// transcriptLine builds one Claude Code transcript JSONL record with a string
// content body.
func transcriptLine(t *testing.T, typ, role, content string) string {
	t.Helper()
	rec := map[string]any{
		"type":    typ,
		"message": map[string]any{"role": role, "content": content},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal transcript line: %v", err)
	}
	return string(b)
}

// writeTranscript writes lines (newline-joined, trailing newline) to a temp
// transcript file and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func TestHookHelpListsSubcommands(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeHookRoot(nil, &buf, nil)
	root.SetArgs([]string{"hook", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("hook --help: %v", err)
	}
	for _, want := range []string{"stop", "session-start", "FAIL-OPEN"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("hook help missing %q\n%s", want, buf.String())
		}
	}
}

func TestCaptureDelta_DistillsTurns(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	path := writeTranscript(t,
		transcriptLine(t, "user", "user", "ship the release"),
		`{"type":"tool_result","message":{"role":"user","content":[{"type":"tool_result","content":"noise"}]}}`,
		transcriptLine(t, "assistant", "assistant", "released v2"),
	)

	text, off, err := captureDelta(dataDir, "s1", path)
	if err != nil {
		t.Fatalf("captureDelta: %v", err)
	}
	if !strings.Contains(text, "user: ship the release") || !strings.Contains(text, "assistant: released v2") {
		t.Errorf("distilled text missing turns:\n%s", text)
	}
	if strings.Contains(text, "noise") {
		t.Errorf("tool_result content should be dropped:\n%s", text)
	}
	info, _ := os.Stat(path)
	if off != info.Size() {
		t.Errorf("newOffset = %d, want file size %d", off, info.Size())
	}
}

func TestCaptureDelta_OnlyNewTurnsSinceCursor(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	tdir := t.TempDir()
	path := filepath.Join(tdir, "transcript.jsonl")
	first := transcriptLine(t, "user", "user", "first turn") + "\n"
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	text1, off1, err := captureDelta(dataDir, "s1", path)
	if err != nil {
		t.Fatalf("captureDelta #1: %v", err)
	}
	if !strings.Contains(text1, "first turn") {
		t.Fatalf("first capture missing turn: %q", text1)
	}
	if err := writeCursor(dataDir, "s1", off1); err != nil {
		t.Fatalf("writeCursor: %v", err)
	}

	// Re-fire with no new content: empty delta, cursor unchanged.
	text2, off2, err := captureDelta(dataDir, "s1", path)
	if err != nil {
		t.Fatalf("captureDelta #2: %v", err)
	}
	if strings.TrimSpace(text2) != "" {
		t.Errorf("no-new-content capture should be empty, got %q", text2)
	}
	if off2 != off1 {
		t.Errorf("offset advanced with no new content: %d != %d", off2, off1)
	}

	// Append a new turn: only the delta is captured.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(transcriptLine(t, "assistant", "assistant", "second turn") + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	text3, _, err := captureDelta(dataDir, "s1", path)
	if err != nil {
		t.Fatalf("captureDelta #3: %v", err)
	}
	if strings.Contains(text3, "first turn") {
		t.Errorf("delta capture re-included an already-captured turn:\n%s", text3)
	}
	if !strings.Contains(text3, "second turn") {
		t.Errorf("delta capture missing the new turn:\n%s", text3)
	}
}

// TestCaptureDelta_PartialTrailingLineNotAdvanced pins the in-flight-write
// guard: when the transcript tail is an unterminated record (no newline in the
// delta window), capture returns no text AND leaves the cursor at its prior
// offset, so the line is re-scanned once it completes rather than dropped.
func TestCaptureDelta_PartialTrailingLineNotAdvanced(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	tdir := t.TempDir()
	path := filepath.Join(tdir, "transcript.jsonl")
	// A single record with NO trailing newline (mid-write).
	if err := os.WriteFile(path, []byte(transcriptLine(t, "user", "user", "half-written")), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	text, off, err := captureDelta(dataDir, "s1", path)
	if err != nil {
		t.Fatalf("captureDelta: %v", err)
	}
	if strings.TrimSpace(text) != "" {
		t.Errorf("partial line should yield no text, got %q", text)
	}
	if off != 0 {
		t.Errorf("cursor must stay at 0 for an unterminated record, got %d", off)
	}

	// Complete the line; the now-terminated record is captured.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatalf("append newline: %v", err)
	}
	_ = f.Close()

	text2, off2, err := captureDelta(dataDir, "s1", path)
	if err != nil {
		t.Fatalf("captureDelta #2: %v", err)
	}
	if !strings.Contains(text2, "half-written") {
		t.Errorf("completed record was not captured: %q", text2)
	}
	if off2 == 0 {
		t.Error("cursor should advance once the record is terminated")
	}
}

// TestCaptureDelta_OverScanBudgetSkipsFirstPartial pins the mid-record-seek
// path: when the unread delta exceeds the scan budget, capture seeks into the
// tail and the first (now-fragmentary) line is dropped, while complete trailing
// turns are still captured. The cursor advances past the skipped bytes.
func TestCaptureDelta_OverScanBudgetSkipsFirstPartial(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// A first turn larger than the 256 KiB scan budget forces a mid-record seek.
	huge := strings.Repeat("X", 300<<10)
	path := writeTranscript(t,
		transcriptLine(t, "user", "user", huge),
		transcriptLine(t, "assistant", "assistant", "tail turn"),
	)
	info, _ := os.Stat(path)

	text, off, err := captureDelta(dataDir, "s1", path)
	if err != nil {
		t.Fatalf("captureDelta: %v", err)
	}
	if strings.Contains(text, "XXXX") {
		t.Error("the over-budget first record's fragment should be dropped, not captured")
	}
	if !strings.Contains(text, "tail turn") {
		t.Errorf("the complete trailing turn should be captured:\n%.200q", text)
	}
	if off != info.Size() {
		t.Errorf("cursor should advance to EOF after a full scan, got %d want %d", off, info.Size())
	}
}

// TestExtractTurn_RoleOnlyFromMessageRole pins that role is read solely from
// message.role: a record carrying a top-level type but no message.role is
// metadata and is skipped (the removed type→role fallback no longer
// misclassifies tool/metadata records as turns).
func TestExtractTurn_RoleOnlyFromMessageRole(t *testing.T) {
	t.Parallel()
	// type="assistant" with NO message.role and no conversational text.
	if _, _, ok := extractTurn([]byte(`{"type":"assistant","summary":"compacted"}`)); ok {
		t.Error("a record with no message.role should be skipped")
	}
	// A genuine turn (message.role present) is captured.
	role, text, ok := extractTurn([]byte(`{"type":"user","message":{"role":"user","content":"hello"}}`))
	if !ok || role != "user" || text != "hello" {
		t.Errorf("extractTurn = (%q,%q,%v), want (user,hello,true)", role, text, ok)
	}
}

func TestDistillTurns_TailKeepWithinBudget(t *testing.T) {
	t.Parallel()
	turns := []string{
		"user: oldest\n",
		"assistant: middle\n",
		"user: newest\n",
	}
	// Budget admits only the last turn.
	got := distillTurns(turns, len("user: newest\n"))
	if got != "user: newest\n" {
		t.Errorf("tail-keep = %q, want only the newest turn", got)
	}
	// A single turn over budget is still kept (never dropped entirely).
	got = distillTurns([]string{"user: a very long single turn\n"}, 4)
	if got != "user: a very long single turn\n" {
		t.Errorf("single oversize turn = %q, want it kept", got)
	}
}

func TestBuildHookRememberRequest_Provenance(t *testing.T) {
	t.Parallel()
	body, err := buildHookRememberRequest("user: hi\nassistant: hello\n")
	if err != nil {
		t.Fatalf("buildHookRememberRequest: %v", err)
	}
	var in struct {
		Text       string  `json:"text"`
		Source     string  `json:"source"`
		Actor      string  `json:"actor"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if in.Source != "hook:claude-code/stop" {
		t.Errorf("source = %q, want hook:claude-code/stop", in.Source)
	}
	if in.Actor != "assistant" {
		t.Errorf("actor = %q, want assistant", in.Actor)
	}
	if in.Confidence != passiveCaptureConfidence {
		t.Errorf("confidence = %g, want %g", in.Confidence, passiveCaptureConfidence)
	}
}

func TestSanitizeSessionID_StripsPathSeparators(t *testing.T) {
	t.Parallel()
	// A hostile id must collapse to a single safe filename component: no path
	// separators survive, so cursorPath can never escape the sessions dir.
	for _, id := range []string{"../../etc/passwd", `..\..\win`, "a/b/c", ".", ".."} {
		got := sanitizeSessionID(id)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeSessionID(%q) = %q retains a path separator", id, got)
		}
		if got == "." || got == ".." || got == "" {
			t.Errorf("sanitizeSessionID(%q) = %q is not a usable filename component", id, got)
		}
	}
}

// TestSanitizeSessionID_RealUUIDPassesThrough pins that a real Claude Code
// session id (lowercase-hex UUID) is used verbatim, so cursor files stay
// human-readable for the common case.
func TestSanitizeSessionID_RealUUIDPassesThrough(t *testing.T) {
	t.Parallel()
	uuid := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	if got := sanitizeSessionID(uuid); got != uuid {
		t.Errorf("sanitizeSessionID(%q) = %q, want it unchanged", uuid, got)
	}
}

// TestSanitizeSessionID_DistinctHostileIDsDoNotCollide pins that two different
// ids requiring sanitization map to DIFFERENT cursor files (in-place character
// replacement would have collapsed "a/b" and "a_b" to the same name).
func TestSanitizeSessionID_DistinctHostileIDsDoNotCollide(t *testing.T) {
	t.Parallel()
	a := sanitizeSessionID("a/b")
	b := sanitizeSessionID("a_b") // "a_b" is itself safe → verbatim; must differ from hashed "a/b"
	c := sanitizeSessionID("a\\b")
	if a == b {
		t.Errorf("distinct ids collided: %q == %q", a, b)
	}
	if a == c {
		t.Errorf("distinct hostile ids collided: %q == %q", a, c)
	}
}

// TestHookStop_FailOpen pins that every degenerate input exits 0 (no error from
// Execute) and writes nothing to stdout — a Stop hook must never block.
func TestHookStop_FailOpen(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope.jsonl")
	cases := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"malformed stdin", "not json at all", []string{"hook", "stop"}},
		{"empty transcript path", `{"session_id":"s1","hook_event_name":"Stop"}`, []string{"hook", "stop"}},
		{"missing transcript file", `{"session_id":"s1","transcript_path":"` + missing + `","hook_event_name":"Stop"}`, []string{"hook", "stop"}},
		{"stop hook active", `{"session_id":"s1","transcript_path":"` + missing + `","stop_hook_active":true}`, []string{"hook", "stop"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var outBuf, errBuf bytes.Buffer
			root := makeHookRoot(strings.NewReader(tc.stdin), &outBuf, &errBuf)
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("hook stop must exit 0, got error: %v (stderr=%q)", err, errBuf.String())
			}
			if outBuf.Len() != 0 {
				t.Errorf("hook stop wrote to stdout: %q", outBuf.String())
			}
		})
	}
}

// TestHookStop_ReentrancyWritesNoCursor pins that stop_hook_active short-circuits
// before any capture work — no cursor file is created.
func TestHookStop_ReentrancyWritesNoCursor(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	path := writeTranscript(t, transcriptLine(t, "user", "user", "hi"))
	payload := `{"session_id":"s1","transcript_path":"` + path + `","stop_hook_active":true}`
	root := makeHookRoot(strings.NewReader(payload), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{"hook", "stop", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("hook stop: %v", err)
	}
	if _, err := os.Stat(cursorPath(dataDir, "s1")); !os.IsNotExist(err) {
		t.Errorf("re-entrant Stop should not write a cursor; stat err=%v", err)
	}
}

func TestEmitSessionStartContext_Shape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := emitSessionStartContext(&buf, "# Brief\nhello"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal emitted JSON: %v\n%s", err, buf.String())
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", out.HookSpecificOutput.HookEventName)
	}
	if out.HookSpecificOutput.AdditionalContext != "# Brief\nhello" {
		t.Errorf("additionalContext = %q", out.HookSpecificOutput.AdditionalContext)
	}
}

// TestHookSessionStart_NoWarmStartIsNoop pins that without --warm-start the
// command emits nothing (injection is opt-in).
func TestHookSessionStart_NoWarmStartIsNoop(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	root := makeHookRoot(strings.NewReader(`{"hook_event_name":"SessionStart","source":"startup"}`), &outBuf, &bytes.Buffer{})
	root.SetArgs([]string{"hook", "session-start"})
	if err := root.Execute(); err != nil {
		t.Fatalf("session-start: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Errorf("session-start without --warm-start should emit nothing, got %q", outBuf.String())
	}
}

// TestHookStop_DispatchFailureDoesNotAdvanceCursor pins the retry contract: a
// failed capture dispatch (here a stubbed wiring-open failure) must exit 0 AND
// leave the cursor unadvanced so the delta is retried on the next Stop.
func TestHookStop_DispatchFailureDoesNotAdvanceCursor(t *testing.T) {
	// No t.Parallel: stubs package-level seams.
	prevOpen := openWiringFn
	openWiringFn = func(context.Context, daemon.WiringConfig) (*daemon.Wired, error) {
		return nil, errors.New("simulated wiring failure")
	}
	t.Cleanup(func() { openWiringFn = prevOpen })
	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	dataDir := t.TempDir()
	path := writeTranscript(t, transcriptLine(t, "user", "user", "capture me"))
	payload := `{"session_id":"sFail","transcript_path":"` + path + `","hook_event_name":"Stop","stop_hook_active":false}`
	root := makeHookRoot(strings.NewReader(payload), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{"hook", "stop", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("hook stop must exit 0 even when dispatch fails: %v", err)
	}
	if _, err := os.Stat(cursorPath(dataDir, "sFail")); !os.IsNotExist(err) {
		t.Errorf("cursor must not advance when capture dispatch fails; stat err = %v", err)
	}
}

// TestHookSessionStart_WarmStartE2E drives `maz hook session-start --warm-start`
// against a real in-process store and asserts it emits a non-empty brief in the
// hookSpecificOutput shape — reusing getContext/contextbrief end-to-end.
func TestHookSessionStart_WarmStartE2E(t *testing.T) {
	seedConfig(t, t.TempDir()) // gate on -short
	dataDir := t.TempDir()
	seedConfig(t, dataDir)

	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	var outBuf, errBuf bytes.Buffer
	root := makeHookRoot(strings.NewReader(`{"hook_event_name":"SessionStart","source":"startup"}`), &outBuf, &errBuf)
	root.SetArgs([]string{"hook", "session-start", "--warm-start", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("session-start --warm-start: %v (stderr=%q)", err, errBuf.String())
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(outBuf.Bytes(), &out); err != nil {
		t.Fatalf("warm-start did not emit hookSpecificOutput JSON: %v\n%s", err, outBuf.String())
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", out.HookSpecificOutput.HookEventName)
	}
	if strings.TrimSpace(out.HookSpecificOutput.AdditionalContext) == "" {
		t.Error("warm-start emitted an empty brief")
	}
}

// TestHookStop_CaptureE2E drives `maz hook stop` against a real in-process store
// and asserts the captured episode carries a signed EventAuthor and
// actor=assistant — issue #228's attribution criterion.
func TestHookStop_CaptureE2E(t *testing.T) {
	seedConfig(t, t.TempDir()) // gate on -short before any heavy work
	// Re-seed in a dir we keep (seedConfig's temp dir above only gates).
	dataDir := t.TempDir()
	seedConfig(t, dataDir)

	// A real daemon on :7842 would steal the dispatch; force the in-process
	// path deterministically.
	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	path := writeTranscript(t,
		transcriptLine(t, "user", "user", "remember that Sam works at Erebor Global"),
		transcriptLine(t, "assistant", "assistant", "Noted: Sam is at Erebor Global."),
	)
	payload := `{"session_id":"sE2E","transcript_path":"` + path + `","hook_event_name":"Stop","stop_hook_active":false}`

	var errBuf bytes.Buffer
	root := makeHookRoot(strings.NewReader(payload), &bytes.Buffer{}, &errBuf)
	root.SetArgs([]string{"hook", "stop", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("hook stop: %v (stderr=%q)", err, errBuf.String())
	}

	// Re-open the store and read the event log back.
	ev := findCapturedEpisode(t, dataDir)
	if ev.Author.PrincipalUID == "" || len(ev.Author.Sig) == 0 {
		t.Errorf("captured episode lacks a signed EventAuthor: principal=%q sigLen=%d",
			ev.Author.PrincipalUID, len(ev.Author.Sig))
	}
	var ep ontology.Episode
	if err := json.Unmarshal(ev.Payload, &ep); err != nil {
		t.Fatalf("unmarshal episode payload: %v", err)
	}
	if ep.Actor != ontology.ActorAssistant {
		t.Errorf("captured episode actor = %q, want assistant", ep.Actor)
	}
	if ev.Source != "hook:claude-code/stop" {
		t.Errorf("captured episode source = %q, want hook:claude-code/stop", ev.Source)
	}

	// The cursor advanced to the transcript size after a successful capture.
	info, _ := os.Stat(path)
	if got := readCursor(dataDir, "sE2E"); got != info.Size() {
		t.Errorf("cursor = %d after capture, want transcript size %d", got, info.Size())
	}
}

// TestHookStop_NoSessionIDSkipsCapture pins that a Stop payload without a
// session_id captures nothing: without a per-session cursor we cannot dedup
// across turns, so capturing would re-dispatch the full transcript tail every
// fire. The guard must skip (exit 0, no episode) rather than write duplicates.
func TestHookStop_NoSessionIDSkipsCapture(t *testing.T) {
	dataDir := t.TempDir()
	seedConfig(t, dataDir) // gates on -short

	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	path := writeTranscript(t, transcriptLine(t, "user", "user", "this should not be captured"))
	payload := `{"session_id":"","transcript_path":"` + path + `","hook_event_name":"Stop","stop_hook_active":false}`
	root := makeHookRoot(strings.NewReader(payload), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{"hook", "stop", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("hook stop must exit 0: %v", err)
	}
	if n := countCapturedEpisodes(t, dataDir); n != 0 {
		t.Errorf("empty session_id captured %d episode(s), want 0", n)
	}
}

// countCapturedEpisodes opens a fresh wiring on dataDir and counts hook-captured
// episode events.
func countCapturedEpisodes(t *testing.T, dataDir string) int {
	t.Helper()
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dataDir})
	if err != nil {
		t.Fatalf("re-open wiring: %v", err)
	}
	t.Cleanup(func() { _ = wired.Close(t.Context()) })
	n := 0
	for ev, ferr := range wired.Orchestrator.Log.Read(t.Context(), kernel.ReadOpts{}) {
		if ferr != nil {
			t.Fatalf("read event log: %v", ferr)
		}
		if ev.Kind == ontology.KindEpisode && ev.Source == "hook:claude-code/stop" {
			n++
		}
	}
	return n
}

// findCapturedEpisode opens a fresh wiring on dataDir and returns the first
// hook-captured episode event, or fatals if none is present.
func findCapturedEpisode(t *testing.T, dataDir string) kernel.Event {
	t.Helper()
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dataDir})
	if err != nil {
		t.Fatalf("re-open wiring: %v", err)
	}
	t.Cleanup(func() { _ = wired.Close(t.Context()) })
	for ev, ferr := range wired.Orchestrator.Log.Read(t.Context(), kernel.ReadOpts{}) {
		if ferr != nil {
			t.Fatalf("read event log: %v", ferr)
		}
		if ev.Kind == ontology.KindEpisode && ev.Source == "hook:claude-code/stop" {
			return ev
		}
	}
	t.Fatal("no hook-captured episode found in the event log")
	return kernel.Event{}
}
