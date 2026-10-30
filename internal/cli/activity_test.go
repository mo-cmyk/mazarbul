package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
)

// makeActivityCmd wires a cobra command tree in the same shape as production
// so PersistentFlags (--data-dir, --json, --no-tui) are accessible.
// Mirrors the makeAskCmd pattern from ask_test.go:16.
func makeActivityCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.PersistentFlags().String("output", "", "output format: text|json|ndjson")
	act := newActivityCmd()
	root.AddCommand(act)
	if outBuf != nil {
		root.SetOut(outBuf)
		act.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		act.SetErr(errBuf)
	}
	return root
}

// seedEvents appends n events with the given CreateTime into a kernelboot log.
// It uses the local-default signer so no keychain is needed.
func seedEvents(t *testing.T, log *kernel.FileEventLog, createTime time.Time, author string, n int) {
	t.Helper()
	ctx := context.Background()
	events := make([]kernel.Event, n)
	for i := range events {
		events[i] = kernel.Event{
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			// Episode schema requires text, source, confidence fields.
			Payload:    []byte(`{"text":"test event","source":"test","confidence":1.0}`),
			CreateTime: createTime,
		}
		_ = author // default signer sets Author.PrincipalUID = "local"
	}
	if _, err := log.Append(ctx, events); err != nil {
		t.Fatalf("seedEvents(%v, n=%d): %v", createTime.Format("2006-01-02"), n, err)
	}
}

// TestActivityHelp verifies --help is non-empty and includes Examples.
func TestActivityHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeActivityCmd(&buf, nil)
	root.SetArgs([]string{"activity", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz activity",
		"--span",
		"--output json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q; got:\n%s", want, out)
		}
	}
}

// TestActivitySpanDays verifies the span flag parser.
func TestActivitySpanDays(t *testing.T) {
	t.Parallel()
	cases := []struct {
		span    string
		want    int
		wantErr bool
	}{
		{span: "30d", want: 30},
		{span: "90d", want: 90},
		{span: "52w", want: 52 * 7},
		{span: "7d", wantErr: true},
		{span: "", wantErr: true},
		{span: "1y", wantErr: true},
	}
	for _, tc := range cases {
		t.Run("span "+tc.span, func(t *testing.T) {
			t.Parallel()
			got, err := spanDays(tc.span)
			if tc.wantErr {
				if err == nil {
					t.Errorf("spanDays(%q) want error, got %d", tc.span, got)
				}
				return
			}
			if err != nil {
				t.Errorf("spanDays(%q) unexpected error: %v", tc.span, err)
			}
			if got != tc.want {
				t.Errorf("spanDays(%q) = %d, want %d", tc.span, got, tc.want)
			}
		})
	}
}

// TestActivityHeatmapCell checks the cell intensity mapping.
func TestActivityHeatmapCell(t *testing.T) {
	t.Parallel()
	noColor := colorizer.New(false, nil) // colorizer with color disabled

	cases := []struct {
		count    int
		maxCount int
		wantChar string
	}{
		{count: 0, maxCount: 10, wantChar: "░"}, // empty → lowest glyph
		{count: 1, maxCount: 10, wantChar: "▁"}, // ~10% → level 1
		{count: 3, maxCount: 10, wantChar: "▂"}, // ~30% → level 2
		{count: 6, maxCount: 10, wantChar: "▅"}, // ~60% → level 4
		{count: 9, maxCount: 10, wantChar: "█"}, // ~90% → level 6
		{count: 5, maxCount: 0, wantChar: "█"},  // maxCount=0 → top level
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("count %d max %d", tc.count, tc.maxCount), func(t *testing.T) {
			t.Parallel()
			cell := heatmapCell(noColor, tc.count, tc.maxCount)
			if !strings.Contains(cell, tc.wantChar) {
				t.Errorf("heatmapCell(%d,%d) = %q, want to contain %q",
					tc.count, tc.maxCount, cell, tc.wantChar)
			}
		})
	}
}

// TestActivityAlignToMonday checks ISO week alignment.
func TestActivityAlignToMonday(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string // "2006-01-02" (UTC)
		want  string // expected Monday
	}{
		{"2026-06-15", "2026-06-15"}, // already Monday
		{"2026-06-16", "2026-06-15"}, // Tuesday
		{"2026-06-17", "2026-06-15"}, // Wednesday
		{"2026-06-21", "2026-06-15"}, // Sunday
		{"2026-06-22", "2026-06-22"}, // Monday again
	}
	for _, tc := range cases {
		t.Run("align "+tc.input, func(t *testing.T) {
			t.Parallel()
			d, err := time.Parse("2006-01-02", tc.input)
			if err != nil {
				t.Fatal(err)
			}
			got := alignToMonday(d).Format("2006-01-02")
			if got != tc.want {
				t.Errorf("alignToMonday(%s) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

// TestActivityDefaultSpan verifies the default span is 90d.
func TestActivityDefaultSpan(t *testing.T) {
	t.Parallel()
	cmd := newActivityCmd()
	spanFlag := cmd.Flags().Lookup("span")
	if spanFlag == nil {
		t.Fatal("--span flag not registered")
	}
	if spanFlag.DefValue != "90d" {
		t.Errorf("--span default = %q, want %q", spanFlag.DefValue, "90d")
	}
}

// TestActivityBuildBuckets checks JSON bucket construction.
func TestActivityBuildBuckets(t *testing.T) {
	t.Parallel()
	t.Run("empty map yields empty slice", func(t *testing.T) {
		t.Parallel()
		buckets := buildBuckets(map[string]int{})
		if len(buckets) != 0 {
			t.Errorf("want empty, got %+v", buckets)
		}
		// Must be non-nil so JSON encodes as [] not null.
		b, _ := json.Marshal(buckets)
		if string(b) != "[]" {
			t.Errorf("empty buckets JSON = %s, want []", b)
		}
	})
	t.Run("sorted ascending by date", func(t *testing.T) {
		t.Parallel()
		counts := map[string]int{
			"2026-06-10": 3,
			"2026-06-05": 1,
			"2026-06-15": 7,
		}
		buckets := buildBuckets(counts)
		if len(buckets) != 3 {
			t.Fatalf("want 3 buckets, got %d", len(buckets))
		}
		if buckets[0].Date != "2026-06-05" || buckets[1].Date != "2026-06-10" || buckets[2].Date != "2026-06-15" {
			t.Errorf("wrong date order: %v", buckets)
		}
	})
	t.Run("zero counts excluded", func(t *testing.T) {
		t.Parallel()
		counts := map[string]int{
			"2026-06-01": 0,
			"2026-06-02": 2,
		}
		buckets := buildBuckets(counts)
		if len(buckets) != 1 || buckets[0].Date != "2026-06-02" {
			t.Errorf("zero-count date should be excluded; got %+v", buckets)
		}
	})
}

// TestActivityJSONOutput exercises the full command path with a seeded log,
// asserting {date,count} JSON shape and parseable output.
func TestActivityJSONOutput(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()

	// Seed a known number of events today via kernelboot.OpenLog.
	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	seedEvents(t, log, today, "local", 5)
	if err := closeFn(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	// Run the command in JSON mode (non-TTY stdout → FormatJSON default).
	var out, errBuf bytes.Buffer
	root := makeActivityCmd(&out, &errBuf)
	root.SetArgs([]string{
		"activity",
		"--data-dir", dataDir,
		"--output", "json",
		"--span", "30d",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, errBuf.String())
	}

	// Must parse as a JSON array.
	var buckets []activityBucket
	if err := json.Unmarshal(out.Bytes(), &buckets); err != nil {
		t.Fatalf("output is not valid JSON array: %v\nraw: %s", err, out.String())
	}

	// Today's date must appear with count=5.
	todayKey := today.Format("2006-01-02")
	found := false
	for _, b := range buckets {
		if b.Date == todayKey {
			found = true
			if b.Count != 5 {
				t.Errorf("bucket %s count = %d, want 5", todayKey, b.Count)
			}
		}
	}
	if !found {
		t.Errorf("today's bucket %s not found in JSON output: %v", todayKey, buckets)
	}
}

// TestActivityNDJSONIsLineDelimited guards the NDJSON contract: `--output ndjson`
// must emit one bucket object per line, not a single JSON-array line. (Emitting
// the whole []activityBucket slice through the writer would marshal an array.)
func TestActivityNDJSONIsLineDelimited(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()
	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	// Seed three distinct days so buildBuckets (which emits only days that have
	// events) yields multiple buckets — proving line-delimited output.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	seedEvents(t, log, today, "local", 3)
	seedEvents(t, log, today.AddDate(0, 0, -2), "local", 1)
	seedEvents(t, log, today.AddDate(0, 0, -5), "local", 2)
	if err := closeFn(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	var out, errBuf bytes.Buffer
	root := makeActivityCmd(&out, &errBuf)
	root.SetArgs([]string{"activity", "--data-dir", dataDir, "--output", "ndjson", "--span", "30d"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, errBuf.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("ndjson must be multiple lines (a 30d span has many buckets), got %d:\n%s", len(lines), out.String())
	}
	// Every non-empty line must be a single bucket object, never a JSON array.
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "[") {
			t.Fatalf("line %d is a JSON array, not a bucket object (NDJSON violation): %s", i, ln)
		}
		var b activityBucket
		if err := json.Unmarshal([]byte(ln), &b); err != nil {
			t.Fatalf("line %d is not a valid bucket object: %v\nline: %s", i, err, ln)
		}
	}
}

// TestActivityTextOutput exercises the text rendering path with a seeded log.
func TestActivityTextOutput(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()

	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	seedEvents(t, log, today, "local", 3)
	if err := closeFn(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	var out, errBuf bytes.Buffer
	root := makeActivityCmd(&out, &errBuf)
	// Force text mode so the heatmap renders (non-TTY default is JSON; pass --output text).
	root.SetArgs([]string{
		"activity",
		"--data-dir", dataDir,
		"--output", "text",
		"--span", "90d",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, errBuf.String())
	}

	rendered := out.String()

	// Header must mention the friendly span and total events.
	if !strings.Contains(rendered, "last 90 days") {
		t.Errorf("text output missing span label 'last 90 days'; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "3 events") {
		t.Errorf("text output missing '3 events'; got:\n%s", rendered)
	}

	// Heatmap day labels appear only on Mon/Wed/Fri (GitHub convention).
	for _, day := range []string{"Mon", "Wed", "Fri"} {
		if !strings.Contains(rendered, day) {
			t.Errorf("text output missing day label %q; got:\n%s", day, rendered)
		}
	}

	// Authors section must appear with "local" (the default signer's PrincipalUID).
	if !strings.Contains(rendered, "local") {
		t.Errorf("text output missing 'local' author; got:\n%s", rendered)
	}
}

// TestActivityNOCOLOR verifies that NO_COLOR=1 produces the same structural
// output as the plain run (no ANSI escapes in output).
func TestActivityNOCOLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	dataDir := t.TempDir()
	ctx := context.Background()

	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	seedEvents(t, log, today, "local", 2)
	if err := closeFn(); err != nil {
		t.Fatalf("close log: %v", err)
	}

	var out bytes.Buffer
	root := makeActivityCmd(&out, nil)
	root.SetArgs([]string{
		"activity",
		"--data-dir", dataDir,
		"--output", "text",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	rendered := out.String()

	// No ANSI escape sequences must appear when NO_COLOR is set.
	if strings.Contains(rendered, "\x1b[") {
		t.Errorf("NO_COLOR=1: output contains ANSI escapes:\n%s", rendered)
	}

	// Structural content must still be present.
	if !strings.Contains(rendered, "2 events") {
		t.Errorf("NO_COLOR=1: missing '2 events'; got:\n%s", rendered)
	}
}

// TestActivityPerAuthorBars exercises renderActivityTo directly with a synthetic
// activityData that has two authors.
func TestActivityPerAuthorBars(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	data := &activityData{
		dateCounts: map[string]int{
			now.Format("2006-01-02"): 10,
		},
		authorCounts: map[string]int{
			"claude": 8,
			"local":  2,
		},
		total: 10,
		since: now.Add(-90 * 24 * time.Hour),
		span:  "90d",
	}

	var out bytes.Buffer
	clr := colorizer.New(false, &out) // color disabled
	if err := renderActivityTo(&out, clr, false, data); err != nil {
		t.Fatalf("renderActivityTo: %v", err)
	}

	rendered := out.String()

	// Both authors must appear in the bar section.
	if !strings.Contains(rendered, "claude") {
		t.Errorf("missing author 'claude'; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "local") {
		t.Errorf("missing author 'local'; got:\n%s", rendered)
	}

	// The by-author section label must appear.
	if !strings.Contains(rendered, "by author") {
		t.Errorf("missing 'by author' section; got:\n%s", rendered)
	}

	// claude (8/10 = 80%) should come before local (2/10 = 20%) — higher count first.
	claudePos := strings.Index(rendered, "claude")
	localPos := strings.Index(rendered, "local")
	if claudePos < 0 || localPos < 0 {
		t.Fatalf("author positions not found")
	}
	if claudePos > localPos {
		t.Errorf("claude (8 events) should appear before local (2 events)")
	}
}

// TestActivityScopeFilter verifies --scope filters events in-loop.
func TestActivityScopeFilter(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	// Only set fields the test actually reads to avoid govet unusedwrite.
	flags := &activityFlags{scopes: []string{"project"}}
	data := &activityData{
		dateCounts: make(map[string]int),
	}

	// Manually exercise scope filtering as collectActivity does.
	testEvents := []struct {
		scope string
		count int
	}{
		{"project", 3},
		{"personal", 5},
		{"project", 2},
	}
	for _, e := range testEvents {
		if len(flags.scopes) > 0 && !scopeMatches(e.scope, flags.scopes) {
			continue
		}
		data.dateCounts[now.Format("2006-01-02")] += e.count
		data.total += e.count
	}

	if data.total != 5 { // only project events: 3+2=5
		t.Errorf("scope filter: total = %d, want 5 (project events only)", data.total)
	}
}

// TestAuthorMatches locks the --author matcher: the query must match as a
// prefix of either the principalUID or the resolved display name. The
// display-name path is the fix for the Greptile P1 (a name query like "alice"
// previously matched nothing because only the UID was checked).
func TestAuthorMatches(t *testing.T) {
	t.Parallel()
	const uid = "01HXYZULIDPRINCIPAL"
	cases := []struct {
		name        string
		uid         string
		displayName string
		query       string
		want        bool
	}{
		{"uid prefix", uid, "alice", "01HXYZ", true},
		{"display-name prefix", uid, "alice", "ali", true},
		{"full display name", uid, "alice", "alice", true},
		{"neither matches", uid, "alice", "bob", false},
		{"local sentinel by name", "", "local", "loc", true},
		{"local sentinel non-match", "", "local", "01", false},
		{"uid match with unresolved name", uid, "", "01HX", true},
		{"unresolved name, uid miss", uid, "", "zzz", false},
		{"empty uid never prefix-matches uid", "", "alice", "", true}, // empty query is a prefix of the name
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := authorMatches(tc.uid, tc.displayName, tc.query); got != tc.want {
				t.Errorf("authorMatches(%q, %q, %q) = %v, want %v", tc.uid, tc.displayName, tc.query, got, tc.want)
			}
		})
	}
}

// TestFriendlySpan locks the span-label prose to the unit semantics in
// timeparse.go: 'm' is minutes (not months — ParseSince has no month unit), so
// the banner never claims a window the data does not cover.
func TestFriendlySpan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		span string
		want string
	}{
		{"", "all time"},
		{"90d", "last 90 days"},
		{"52w", "last 52 weeks"},
		{"2h", "last 2 hours"},
		{"30m", "last 30 minutes"}, // regression: must be minutes, never months
		{"90m", "last 90 minutes"}, // PR-documented --since example
		{"45s", "last 45 seconds"},
		{"1d", "last 1 day"},    // singular: no trailing "s"
		{"1w", "last 1 week"},   // singular
		{"1h", "last 1 hour"},   // singular
		{"1m", "last 1 minute"}, // singular
		{"1s", "last 1 second"}, // singular
		// A non-duration suffix (e.g. an RFC 3339 lower bound) falls through verbatim.
		{"2025-06-01T00:00:00Z", "last 2025-06-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.span, func(t *testing.T) {
			t.Parallel()
			if got := friendlySpan(tc.span); got != tc.want {
				t.Errorf("friendlySpan(%q) = %q, want %q", tc.span, got, tc.want)
			}
		})
	}
}
