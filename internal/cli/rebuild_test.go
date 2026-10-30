package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// --- helpers -----------------------------------------------------------------

// fakeStorage is a minimal kernel.Storage implementation for unit tests.
// It records Apply calls and can be configured to return errors.
type fakeStorage struct {
	kernel.NoopStorage
	applyErr   error
	hasEventFn func(uid string) (bool, error)
	applyCalls int
}

func (f *fakeStorage) Apply(_ context.Context, events []kernel.Event) error {
	f.applyCalls += len(events)
	return f.applyErr
}

func (f *fakeStorage) HasEvent(_ context.Context, uid string) (bool, error) {
	if f.hasEventFn != nil {
		return f.hasEventFn(uid)
	}
	return false, nil
}

func (f *fakeStorage) Close() error { return nil }

// fakeEventLog is a minimal kernel.EventLog for unit tests. Rebuild replays
// the configured events through the provided storage.
type fakeEventLog struct {
	events     []kernel.Event
	rebuildErr error
}

func (f *fakeEventLog) Append(_ context.Context, _ []kernel.Event) ([]kernel.Receipt, error) {
	return nil, kernel.ErrNotImplemented
}

// Read implements kernel.EventLog. The gocritic hugeParam hint is suppressed:
// ReadOpts is part of the kernel.EventLog contract and cannot be changed to a
// pointer without breaking the interface.
//
//nolint:gocritic // hugeParam: ReadOpts is defined by kernel.EventLog interface
func (f *fakeEventLog) Read(_ context.Context, _ kernel.ReadOpts) iter.Seq2[kernel.Event, error] {
	return func(yield func(kernel.Event, error) bool) {
		for _, ev := range f.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (f *fakeEventLog) At(_ context.Context, _ string) (kernel.Event, error) {
	return kernel.Event{}, kernel.ErrNotImplemented
}

func (f *fakeEventLog) Provenance(_ context.Context, _ string) ([]kernel.ProvenanceRecord, error) {
	return nil, nil
}

func (f *fakeEventLog) Rebuild(ctx context.Context, into kernel.Storage, _ ...kernel.RebuildOption) ([]kernel.ApplyError, error) {
	if f.rebuildErr != nil {
		return nil, f.rebuildErr
	}
	var applyErrs []kernel.ApplyError
	for i := range f.events {
		ev := &f.events[i]
		if err := ctx.Err(); err != nil {
			return applyErrs, err
		}
		has, err := into.HasEvent(ctx, ev.UID)
		if err != nil {
			return applyErrs, err
		}
		if has {
			continue
		}
		if err := into.Apply(ctx, []kernel.Event{*ev}); err != nil {
			applyErrs = append(applyErrs, kernel.ApplyError{UID: ev.UID, Err: err})
		}
	}
	return applyErrs, nil
}

// makeRebuildCmd wires a cobra command tree in the same shape as production
// so PersistentFlags (--data-dir, --output, --json, --no-tui) are accessible.
func makeRebuildCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	rebuild := newRebuildCmd()
	root.AddCommand(rebuild)
	if outBuf != nil {
		root.SetOut(outBuf)
		rebuild.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		rebuild.SetErr(errBuf)
	}
	return root
}

// newRebuildCmdWithOutput returns a rebuild command that has the --output
// persistent flag registered directly on itself (not on a parent) and pre-set
// to the given format. This allows execRebuild to be called directly in unit
// tests without going through cobra.Execute, which would otherwise be required
// to propagate parent persistent flags to child commands.
func newRebuildCmdWithOutput(format string, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	cmd := newRebuildCmd()
	// Register --output as a local flag on the cmd itself. cobra's Flags()
	// includes local flags, so output.From(cmd) via cmd.Flags().GetString finds it.
	// PersistentFlags() is NOT visible via cmd.Flags() for the cmd's own pflags —
	// only parent pflags are merged in. Use local flags to avoid that caveat.
	cmd.Flags().String("output", format, "output format")
	if outBuf != nil {
		cmd.SetOut(outBuf)
	}
	if errBuf != nil {
		cmd.SetErr(errBuf)
	}
	return cmd
}

// --- tests -------------------------------------------------------------------

// TestRebuildHelp: --help output is non-empty and includes the examples section.
// Does not mutate global TUI mode, so it is safe to run in parallel.
func TestRebuildHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeRebuildCmd(&buf, nil)
	root.SetArgs([]string{"rebuild", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"Examples:", "maz rebuild --yes", "--strict", "--json"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRebuildScriptedRequiresYes: CLI mode (non-TTY stdout) without --yes
// must exit 1 and print the message, not block on stdin.
func TestRebuildScriptedRequiresYes(t *testing.T) {
	t.Parallel()

	var outBuf, errBuf bytes.Buffer
	root := makeRebuildCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"rebuild"}) // no --yes; non-TTY stdout → scripted path
	err := root.Execute()

	var re *rebuildExitError
	if !errors.As(err, &re) {
		t.Fatalf("expected rebuildExitError, got %T: %v", err, err)
	}
	if re.Code() != 1 {
		t.Errorf("exit code = %d, want 1", re.Code())
	}
	if !strings.Contains(errBuf.String(), "--yes") {
		t.Errorf("stderr missing --yes hint: %q", errBuf.String())
	}
}

// TestRebuildJSONRequiresYes: JSON mode (--json flag) without --yes must also exit 1.
func TestRebuildJSONRequiresYes(t *testing.T) {
	t.Parallel()

	var outBuf, errBuf bytes.Buffer
	root := makeRebuildCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"rebuild", "--json"})
	err := root.Execute()

	var re *rebuildExitError
	if !errors.As(err, &re) {
		t.Fatalf("expected rebuildExitError, got %T: %v", err, err)
	}
	if re.Code() != 1 {
		t.Errorf("exit code = %d, want 1", re.Code())
	}
}

// TestRebuildCLITextOK: with --yes in text mode, a clean rebuild prints the
// "ok  rebuild complete" line to stdout.
func TestRebuildCLITextOK(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{}
	log := &fakeEventLog{events: []kernel.Event{
		{UID: "01HN0000000000000000000001", Kind: "episode"},
	}}

	var outBuf, errBuf bytes.Buffer
	// Use newRebuildCmdWithOutput to force text mode so the non-TTY stdout
	// does not trigger automatic JSON mode.
	cmd := newRebuildCmdWithOutput("text", &outBuf, &errBuf)

	err := execRebuild(context.Background(), cmd, log, store, &rebuildFlags{yes: true})
	if err != nil {
		t.Fatalf("execRebuild: %v", err)
	}
	if !strings.Contains(outBuf.String(), "ok  rebuild complete") {
		t.Errorf("stdout missing ok line: %q", outBuf.String())
	}
	if errBuf.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errBuf.String())
	}
}

// TestRebuildCLIWithErrors: Apply errors are reported to stderr and exit
// code is 1.
func TestRebuildCLIWithErrors(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{applyErr: errors.New("disk full")}
	log := &fakeEventLog{events: []kernel.Event{
		{UID: "01HN0000000000000000000001", Kind: "episode"},
		{UID: "01HN0000000000000000000002", Kind: "episode"},
		{UID: "01HN0000000000000000000003", Kind: "episode"},
		{UID: "01HN0000000000000000000004", Kind: "episode"}, // >3 to test truncation
	}}

	var outBuf, errBuf bytes.Buffer
	// Force text mode so the non-TTY stdout does not trigger JSON mode.
	cmd := newRebuildCmdWithOutput("text", &outBuf, &errBuf)

	err := execRebuild(context.Background(), cmd, log, store, &rebuildFlags{yes: true})
	var re *rebuildExitError
	if !errors.As(err, &re) || re.Code() != 1 {
		t.Fatalf("expected rebuildExitError{1}, got %T: %v", err, err)
	}
	errOut := errBuf.String()
	if !strings.Contains(errOut, "warning:") {
		t.Errorf("stderr missing warning: %q", errOut)
	}
	if !strings.Contains(errOut, "and 1 more") {
		t.Errorf("stderr missing truncation hint: %q", errOut)
	}
	if !strings.Contains(outBuf.String(), "ok  rebuild complete (with errors)") {
		t.Errorf("stdout missing error summary: %q", outBuf.String())
	}
}

// TestRebuildJSONSummaryParseable: JSON mode emits a parseable summary
// object with expected fields. The non-TTY bytes.Buffer stdout triggers
// automatic JSON mode, so no explicit flag is needed.
func TestRebuildJSONSummaryParseable(t *testing.T) {
	t.Parallel()

	store := &fakeStorage{}
	log := &fakeEventLog{events: []kernel.Event{
		{UID: "01HN0000000000000000000001", Kind: "episode"},
	}}

	var outBuf, errBuf bytes.Buffer
	cmd := newRebuildCmd()
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)

	err := execRebuild(context.Background(), cmd, log, store, &rebuildFlags{yes: true})
	if err != nil {
		t.Fatalf("execRebuild: %v", err)
	}

	lines := strings.Split(strings.TrimRight(outBuf.String(), "\n"), "\n")
	// At minimum one summary line; there may also be progress lines.
	if len(lines) == 0 {
		t.Fatal("no JSON output")
	}
	lastLine := lines[len(lines)-1]
	var summary map[string]any
	if err := json.Unmarshal([]byte(lastLine), &summary); err != nil {
		t.Fatalf("parse summary: %v (%q)", err, lastLine)
	}
	for _, key := range []string{"type", "ok", "applied", "skipped", "failed", "elapsed"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary missing key %q", key)
		}
	}
	if summary["type"] != "summary" {
		t.Errorf("type = %q, want summary", summary["type"])
	}
	if summary["ok"] != true {
		t.Errorf("ok = %v, want true", summary["ok"])
	}
}

// TestRebuildExitErrorCodes: ResolveRebuildExitCode returns the right code
// and (0, false) for nil/non-rebuild errors.
// Does not mutate global state; safe to run in parallel.
func TestRebuildExitErrorCodes(t *testing.T) {
	t.Parallel()
	if code, ok := ResolveRebuildExitCode(nil); ok || code != 0 {
		t.Errorf("nil: got (%d, %v), want (0, false)", code, ok)
	}
	if code, ok := ResolveRebuildExitCode(errors.New("other")); ok || code != 0 {
		t.Errorf("generic: got (%d, %v), want (0, false)", code, ok)
	}
	for _, tc := range []struct{ code int }{{1}, {2}, {130}} {
		code, ok := ResolveRebuildExitCode(&rebuildExitError{code: tc.code})
		if !ok || code != tc.code {
			t.Errorf("rebuildExitError{%d}: got (%d, %v), want (%d, true)", tc.code, code, ok, tc.code)
		}
	}
}

// TestRebuildFatalError: a Rebuild-level error (not an ApplyError) exits 2.
func TestRebuildFatalError(t *testing.T) {
	t.Parallel()

	log := &fakeEventLog{rebuildErr: errors.New("disk read error")}
	store := &fakeStorage{}

	var outBuf, errBuf bytes.Buffer
	// Force text mode so the non-TTY stdout does not trigger JSON mode;
	// the fatal-error path (execRebuildCLI) must emit to stderr.
	cmd := newRebuildCmdWithOutput("text", &outBuf, &errBuf)

	err := execRebuild(context.Background(), cmd, log, store, &rebuildFlags{yes: true})
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 2 {
		t.Errorf("code = %d, want 2", se.code)
	}
	if !strings.Contains(errBuf.String(), "disk read error") {
		t.Errorf("stderr missing error text: %q", errBuf.String())
	}
}

// TestCountingStorageEmitsAtCadence: countingStorage emits a JSON progress
// tick every cadence Apply calls.
// Does not mutate global TUI mode; uses its own encoder. Safe to run in parallel.
func TestCountingStorageEmitsAtCadence(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	cs := &countingStorage{
		inner:   &fakeStorage{},
		cadence: 2,
		emit:    enc,
		start:   time.Now(),
	}

	ctx := context.Background()
	ev := kernel.Event{UID: "01HN0000000000000000000001", Kind: "episode"}

	// First Apply: total=1, no tick yet.
	if err := cs.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected output after first Apply: %q", buf.String())
	}

	// Second Apply: total=2, tick fires.
	if err := cs.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected JSON tick after second Apply, got none")
	}
	var tick map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(buf.String(), "\n")), &tick); err != nil {
		t.Fatalf("parse tick: %v (%q)", err, buf.String())
	}
	if tick["type"] != "progress" {
		t.Errorf("type = %q, want progress", tick["type"])
	}
	for _, key := range []string{"applied", "elapsed", "rate"} {
		if _, ok := tick[key]; !ok {
			t.Errorf("tick missing key %q", key)
		}
	}
}

// TestRebuildStorageOpenError pins the rebuild UX fix: when dispatcher.Open
// fails on the LadybugDB single-opener lock, the error must carry the
// actionable "stop the holder" hint (the user hit a bare "status 1"); any other
// open failure keeps the plain "open storage:" wrap.
func TestRebuildStorageOpenError(t *testing.T) {
	t.Run("held lock surfaces the stop-the-daemon hint", func(t *testing.T) {
		prevProbe := probeDaemonListening
		probeDaemonListening = func(string) bool { return true }
		t.Cleanup(func() { probeDaemonListening = prevProbe })

		raw := errors.New("dispatcher: open ladybug: failed to open database with status 1")
		got := rebuildStorageOpenError("/data/dir", raw)
		msg := got.Error()
		if !strings.Contains(msg, "maz daemon stop") {
			t.Errorf("held-lock error %q should steer the user to `maz daemon stop`", msg)
		}
		if !errors.Is(got, raw) {
			t.Errorf("held-lock error must wrap the original driver error for debugging")
		}
	})

	t.Run("unrelated open failure keeps the plain wrap", func(t *testing.T) {
		raw := errors.New("dispatcher: migrate: no such module: fts5")
		got := rebuildStorageOpenError("/data/dir", raw)
		msg := got.Error()
		if !strings.HasPrefix(msg, "open storage:") {
			t.Errorf("non-lock error %q should keep the `open storage:` prefix", msg)
		}
		if strings.Contains(msg, "maz daemon stop") {
			t.Errorf("non-lock error %q must not misattribute a held lock", msg)
		}
	})
}
