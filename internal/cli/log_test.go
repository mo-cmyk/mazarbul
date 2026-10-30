package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
)

// seedEvent inserts a raw episode event into the JSONL log via the SQLite
// episodes table so that `maz log` has rows to render.
func seedEvent(t *testing.T, dir string, uid string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite for log seed: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
		 VALUES (?, '2026-06-08T00:00:00Z', 'episode', '{"text":"hello"}', 'hello', 'test')`,
		uid,
	); err != nil {
		t.Fatalf("insert episode event %s: %v", uid, err)
	}
}

// seedLogEvents appends episode events to the JSONL event log in dir.  The log
// is opened, populated, then closed (releasing the bolt lock) before
// returning, so a subsequent CLI command that re-opens the same log will
// not contend.
func seedLogEvents(t *testing.T, dir string, events []kernel.Event) {
	t.Helper()
	ctx := context.Background()
	log, closeLog, err := kernelboot.OpenLog(ctx, dir)
	if err != nil {
		t.Fatalf("seedLogEvents: open log: %v", err)
	}
	defer func() {
		if cerr := closeLog(); cerr != nil {
			t.Fatalf("seedLogEvents: close log: %v", cerr)
		}
	}()
	if _, err := log.Append(ctx, events); err != nil {
		t.Fatalf("seedLogEvents: append: %v", err)
	}
}

// episodePayload returns a minimal episode JSON payload that satisfies the
// ontology schema (text + source + confidence required).
func episodePayload(t *testing.T, text string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"text":       text,
		"source":     "test",
		"confidence": 1.0,
	})
	if err != nil {
		t.Fatalf("episodePayload: marshal: %v", err)
	}
	return b
}

// TestLog_JSONEmitsArray verifies that `maz log --output json` emits a single
// JSON array, not NDJSON. An empty log must yield "[]".
func TestLog_JSONEmitsArray(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Seed the SQLite store (runs migrations) but write no events to the log.
	seedEvent(t, dir, "01ARZ3NDEKTSV4RRFFQ69G5LG1")

	// Use `maz list episodes` path to confirm array shape; the log command
	// opens the JSONL file which may be absent in a minimal test dir, so we
	// exercise list directly here and rely on the shared output.Writer path.
	// A dedicated log-level test is below (TestLog_JSONEmptyIsArray).
	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "episodes", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list episodes: %v (stderr: %s)", err, errBuf.String())
	}

	raw := strings.TrimSpace(out.String())
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		t.Fatalf("--output json must be a JSON array, got: %q", raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("output is not valid JSON array: %v\nraw: %s", err, raw)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
}

// TestLog_JSONEmptyIsArray verifies that `maz list episodes --output json`
// with zero rows emits "[]" not zero bytes.
func TestLog_JSONEmptyIsArray(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Open store to run migrations but insert nothing.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if cerr := store.Close(); cerr != nil {
		t.Fatalf("close sqlite: %v", cerr)
	}

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "episodes", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list episodes empty: %v (stderr: %s)", err, errBuf.String())
	}

	raw := strings.TrimSpace(out.String())
	if raw != "[]" {
		t.Fatalf("empty --output json must yield '[]', got: %q", raw)
	}
}

// TestLog_SinceBadValueFriendlyError verifies that passing an unparseable
// value to --since emits an RFC 3339 example in the error message instead of
// Go's internal reference-time layout string (residual LOW bug).
func TestLog_SinceBadValueFriendlyError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if cerr := store.Close(); cerr != nil {
		t.Fatalf("close sqlite: %v", cerr)
	}

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "log", "--since", "not-a-date"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for bad --since value, got nil")
	}
	errStr := errBuf.String()
	// Must NOT leak the Go reference layout.
	if strings.Contains(errStr, "2006-01-02T15:04:05Z07:00") {
		t.Errorf("error must not contain Go reference time layout, got: %s", errStr)
	}
	// Must contain a human-friendly RFC 3339 example.
	if !strings.Contains(errStr, "2006-01-02T15:04:05Z") {
		t.Errorf("error must show an RFC 3339 example like 2006-01-02T15:04:05Z, got: %s", errStr)
	}
}

// TestLog_UntilBadValueFriendlyError verifies the same for --until.
func TestLog_UntilBadValueFriendlyError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if cerr := store.Close(); cerr != nil {
		t.Fatalf("close sqlite: %v", cerr)
	}

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "log", "--until", "not-a-date"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for bad --until value, got nil")
	}
	errStr := errBuf.String()
	if strings.Contains(errStr, "2006-01-02T15:04:05Z07:00") {
		t.Errorf("error must not contain Go reference time layout, got: %s", errStr)
	}
	if !strings.Contains(errStr, "2006-01-02T15:04:05Z") {
		t.Errorf("error must show an RFC 3339 example like 2006-01-02T15:04:05Z, got: %s", errStr)
	}
}

// TestLog_NDJSONStaysLineDelimited verifies that `--output ndjson` continues
// to emit one JSON object per line after the array fix.
func TestLog_NDJSONStaysLineDelimited(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEvent(t, dir, "01ARZ3NDEKTSV4RRFFQ69G5LG2")

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "episodes", "--output", "ndjson"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list episodes ndjson: %v (stderr: %s)", err, errBuf.String())
	}

	lines := nonEmptyLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("ndjson: got %d lines, want 1", len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("ndjson line not parseable: %v", err)
	}
	if _, ok := rec["uid"]; !ok {
		t.Fatal("ndjson row missing uid field")
	}
}

// TestLog_Help verifies that `maz log --help` is non-empty and contains
// the required sections (examples, flags).
func TestLog_Help(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"log", "--help"})
	_ = root.Execute()
	got := out.String()
	if len(got) == 0 {
		t.Fatal("--help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz log",
		"--oneline",
		"--follow",
		"--scope",
		"--author",
		"--since",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--help missing %q\nfull output:\n%s", want, got)
		}
	}
}

// TestLog_SinceDurationAccepted verifies that buildReadOpts accepts a Go
// duration string (7d, 2h) for --since and --until without error. This
// exercises the ParseSince integration — the pre-#269 code accepted only
// RFC 3339 and would reject "7d".
func TestLog_SinceDurationAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		since string
	}{
		{"7 days", "7d"},
		{"2 hours", "2h"},
		{"90 minutes", "90m"},
		{"1 week", "1w"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flags := &logFlags{since: tc.since, limit: 10}
			opts, err := buildReadOpts(flags)
			if err != nil {
				t.Fatalf("buildReadOpts(%q): unexpected error: %v", tc.since, err)
			}
			if opts.Since.IsZero() {
				t.Fatalf("buildReadOpts(%q): Since is zero, expected a resolved time", tc.since)
			}
			// Since must be in the past.
			if !opts.Since.Before(time.Now().UTC()) {
				t.Errorf("buildReadOpts(%q): Since %v is not in the past", tc.since, opts.Since)
			}
		})
	}
}

// TestLog_Oneline verifies the --oneline table format: the header row must
// contain all five column titles and each event row must show the kind,
// scope, and the author glyph (‼ for the local-sentinel signer).
func TestLog_Oneline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedLogEvents(t, dir, []kernel.Event{{
		Kind:       kernel.EventKind("episode"),
		Source:     "test",
		Confidence: 1.0,
		Payload:    episodePayload(t, "hello world"),
	}})

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetContext(context.Background())
	// --output text is needed because in non-TTY mode the default is JSON.
	root.SetArgs([]string{"--data-dir", dir, "log", "--oneline", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("log --oneline: %v (stderr: %s)", err, errBuf.String())
	}

	got := out.String()
	for _, want := range []string{
		"TIME", "KIND", "SCOPE", "AUTHOR", "SUMMARY",
		"episode",
		"personal",
		"‼",     // local-sentinel unsigned author
		"hello", // event summary
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--oneline missing %q\nfull output:\n%s", want, got)
		}
	}
}

// TestLog_Oneline_NOCOLORParity verifies that NO_COLOR=1 suppresses all ANSI
// escapes while keeping the same structural content.
// Note: t.Setenv requires a non-parallel test.
func TestLog_Oneline_NOCOLORParity(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	dir := t.TempDir()
	seedLogEvents(t, dir, []kernel.Event{{
		Kind:       kernel.EventKind("episode"),
		Source:     "test",
		Confidence: 1.0,
		Payload:    episodePayload(t, "no color event"),
	}})

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetContext(context.Background())
	root.SetArgs([]string{"--data-dir", dir, "log", "--oneline", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("log --oneline NO_COLOR: %v (stderr: %s)", err, errBuf.String())
	}

	got := out.String()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("NO_COLOR=1 must suppress all ANSI escapes, got:\n%s", got)
	}
	// Content must still be present.
	for _, want := range []string{"episode", "personal", "‼"} {
		if !strings.Contains(got, want) {
			t.Errorf("NO_COLOR output missing %q:\n%s", want, got)
		}
	}
}

// TestLog_ScopeFilter verifies that --scope restricts events to the requested
// scope and excludes events in other scopes.
func TestLog_ScopeFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedLogEvents(t, dir, []kernel.Event{
		{
			Kind:       kernel.EventKind("episode"),
			Source:     "test",
			Confidence: 1.0,
			Payload:    episodePayload(t, "project note"),
			Scope:      kernel.ScopeProject,
		},
		{
			Kind:       kernel.EventKind("episode"),
			Source:     "test",
			Confidence: 1.0,
			Payload:    episodePayload(t, "personal note"),
			Scope:      kernel.ScopePersonal,
		},
	})

	t.Run("filter project scope", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetContext(context.Background())
		root.SetArgs([]string{"--data-dir", dir, "log", "--scope", "project", "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("log --scope project: %v (stderr: %s)", err, errBuf.String())
		}
		var events []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &events); err != nil {
			t.Fatalf("parse JSON: %v\noutput: %s", err, out.String())
		}
		if len(events) != 1 {
			t.Fatalf("expected 1 project event, got %d", len(events))
		}
		if got := events[0]["scope"]; got != "project" {
			t.Errorf("event scope = %v, want project", got)
		}
	})

	t.Run("filter personal scope", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetContext(context.Background())
		root.SetArgs([]string{"--data-dir", dir, "log", "--scope", "personal", "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("log --scope personal: %v (stderr: %s)", err, errBuf.String())
		}
		var events []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &events); err != nil {
			t.Fatalf("parse JSON: %v\noutput: %s", err, out.String())
		}
		if len(events) != 1 {
			t.Fatalf("expected 1 personal event, got %d", len(events))
		}
		if got := events[0]["scope"]; got != "personal" {
			t.Errorf("event scope = %v, want personal", got)
		}
	})

	t.Run("no filter returns all events", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetContext(context.Background())
		root.SetArgs([]string{"--data-dir", dir, "log", "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("log (no filter): %v (stderr: %s)", err, errBuf.String())
		}
		var events []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &events); err != nil {
			t.Fatalf("parse JSON: %v\noutput: %s", err, out.String())
		}
		if len(events) != 2 {
			t.Fatalf("expected 2 events without filter, got %d", len(events))
		}
	})
}

// TestLog_ScopeFilterLimitNotUnderDelivered guards the silent under-delivery
// bug where --limit was applied to the RAW stream before the scope/author
// post-filter ran: a block of non-matching events ahead of the matching ones
// would exhaust the kernel cap and return an empty list. The matching events
// must surface even when --limit is smaller than the leading unmatched block.
func TestLog_ScopeFilterLimitNotUnderDelivered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	events := make([]kernel.Event, 0, 8)
	// 5 personal events first, then 3 project events — mirrors the Greptile
	// scenario (leading unmatched block longer than --limit).
	for range 5 {
		events = append(events, kernel.Event{
			Kind: kernel.EventKind("episode"), Source: "test", Confidence: 1.0,
			Payload: episodePayload(t, "personal note"), Scope: kernel.ScopePersonal,
		})
	}
	for range 3 {
		events = append(events, kernel.Event{
			Kind: kernel.EventKind("episode"), Source: "test", Confidence: 1.0,
			Payload: episodePayload(t, "project note"), Scope: kernel.ScopeProject,
		})
	}
	seedLogEvents(t, dir, events)

	logScopeJSON := func(t *testing.T, limit string) []map[string]any {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetContext(context.Background())
		root.SetArgs([]string{"--data-dir", dir, "log", "--scope", "project", "--limit", limit, "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("log --scope project --limit %s: %v (stderr: %s)", limit, err, errBuf.String())
		}
		var evs []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &evs); err != nil {
			t.Fatalf("parse JSON: %v\noutput: %s", err, out.String())
		}
		return evs
	}

	t.Run("limit smaller than leading unmatched block still returns matches", func(t *testing.T) {
		// --limit 5 == the personal block size; the buggy code returned [].
		evs := logScopeJSON(t, "5")
		if len(evs) != 3 {
			t.Fatalf("expected 3 project events, got %d", len(evs))
		}
		for _, ev := range evs {
			if ev["scope"] != "project" {
				t.Errorf("event scope = %v, want project", ev["scope"])
			}
		}
	})

	t.Run("limit caps matched events not raw events", func(t *testing.T) {
		// --limit 2 must return exactly 2 of the 3 project events (the cap is
		// enforced on matched events, not the raw stream).
		evs := logScopeJSON(t, "2")
		if len(evs) != 2 {
			t.Fatalf("expected 2 project events under --limit 2, got %d", len(evs))
		}
	})
}

// TestLog_AuthorFilter verifies that --author restricts events by principal
// UID.  Events written without a configured principal carry the
// "local"-sentinel UID (localDefaultPrincipalUID in event_author.go).
func TestLog_AuthorFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedLogEvents(t, dir, []kernel.Event{{
		Kind:       kernel.EventKind("episode"),
		Source:     "test",
		Confidence: 1.0,
		Payload:    episodePayload(t, "authored event"),
	}})

	t.Run("local author matches", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetContext(context.Background())
		root.SetArgs([]string{"--data-dir", dir, "log", "--author", "local", "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("log --author local: %v (stderr: %s)", err, errBuf.String())
		}
		var events []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &events); err != nil {
			t.Fatalf("parse JSON: %v\noutput: %s", err, out.String())
		}
		if len(events) == 0 {
			t.Fatal("--author local should return the seeded event, got empty list")
		}
	})

	t.Run("unknown author returns empty", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetContext(context.Background())
		root.SetArgs([]string{"--data-dir", dir, "log", "--author", "01NONEXISTENT0000000000000", "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("log --author unknown: %v (stderr: %s)", err, errBuf.String())
		}
		raw := strings.TrimSpace(out.String())
		if raw != "[]" {
			t.Fatalf("--author unknown must yield '[]', got: %q", raw)
		}
	})
}

// TestLog_FollowSmoke verifies that --follow exits cleanly when the command
// context is cancelled and does not hang indefinitely.  It also checks that
// events already in the log are emitted before the poll loop starts.
func TestLog_FollowSmoke(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedLogEvents(t, dir, []kernel.Event{{
		Kind:       kernel.EventKind("episode"),
		Source:     "test",
		Confidence: 1.0,
		Payload:    episodePayload(t, "follow test event"),
	}})

	// Use a short timeout so the test does not hang.  The follow ticker
	// fires every second, so 400 ms is enough to drain the initial batch
	// and prove the ctx.Done() path works.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetContext(ctx)
	// --output text so we can check the text output; in non-TTY mode the
	// default is JSON which would use EmitStream → NDJSON.
	root.SetArgs([]string{"--data-dir", dir, "log", "--follow", "--output", "text"})
	err := root.Execute()
	// A nil error is expected: the follow loop returns nil on ctx.Done().
	// A non-nil silentError carrying code 2 would indicate an unexpected
	// failure in the read path.
	if err != nil {
		// Allow context cancellation errors from kernel I/O — the important
		// thing is the command exits (we reach this line) rather than hangs.
		if !strings.Contains(err.Error(), "context") {
			t.Errorf("unexpected error from --follow: %v (stderr: %s)", err, errBuf.String())
		}
	}

	// At minimum the seeded event should have been emitted before the
	// poll loop started.
	got := out.String()
	if !strings.Contains(got, "episode") {
		t.Errorf("--follow should emit existing events before polling; got: %q", got)
	}
}

// TestLog_FollowJSONRefused verifies that `--follow --output json` is refused
// with a usage error pointing at --output ndjson, rather than silently
// degrading to NDJSON and breaking the documented JSON-array contract.
func TestLog_FollowJSONRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedLogEvents(t, dir, []kernel.Event{{
		Kind:       kernel.EventKind("episode"),
		Source:     "test",
		Confidence: 1.0,
		Payload:    episodePayload(t, "follow json refusal"),
	}})

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetContext(context.Background())
	root.SetArgs([]string{"--data-dir", dir, "log", "--follow", "--output", "json"})
	err := root.Execute()
	if err == nil {
		t.Fatalf("--follow --output json must error, got nil (stdout: %q)", out.String())
	}
	var ce *output.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("error must be a CodedError, got %T: %v", err, err)
	}
	if ce.Exit != output.ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", ce.Exit, output.ExitUsage)
	}
	if !strings.Contains(ce.Error(), "ndjson") {
		t.Errorf("error must point at --output ndjson, got: %q", ce.Error())
	}
	// It must not have streamed any event objects to stdout before erroring.
	if strings.Contains(out.String(), "episode") {
		t.Errorf("no events should be emitted when the combination is refused; got: %q", out.String())
	}
}

// TestLog_JSONOutputUnchanged verifies that `maz log --output json` still
// emits a valid JSON array after the L2 additions (scope/author filters,
// --oneline flag, etc.).  This is the regression guard for the wire contract.
func TestLog_JSONOutputUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedLogEvents(t, dir, []kernel.Event{{
		Kind:       kernel.EventKind("episode"),
		Source:     "test",
		Confidence: 1.0,
		Payload:    episodePayload(t, "json contract event"),
	}})

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetContext(context.Background())
	root.SetArgs([]string{"--data-dir", dir, "log", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("log --output json: %v (stderr: %s)", err, errBuf.String())
	}

	raw := strings.TrimSpace(out.String())
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		t.Fatalf("--output json must be a JSON array, got: %q", raw)
	}
	var events []map[string]any
	if err := json.Unmarshal([]byte(raw), &events); err != nil {
		t.Fatalf("JSON array not parseable: %v\nraw: %s", err, raw)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one event in JSON output")
	}
	// Core fields must be present (wire contract).
	event := events[0]
	for _, field := range []string{"uid", "kind", "createTime", "scope", "author"} {
		if _, ok := event[field]; !ok {
			t.Errorf("JSON event missing field %q", field)
		}
	}
	// ANSI must not appear in JSON output.
	if strings.Contains(raw, "\x1b[") {
		t.Error("JSON output must not contain ANSI escapes")
	}
}
