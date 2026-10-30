package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// makeDigestCmd wires a cobra command tree so PersistentFlags (--data-dir,
// --output, --json, --no-tui, --color) are accessible, matching the production
// shape for all digest tests.
func makeDigestCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.PersistentFlags().String("color", "never", "colour mode")
	dig := newDigestCmd()
	root.AddCommand(dig)
	if outBuf != nil {
		root.SetOut(outBuf)
		dig.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		dig.SetErr(errBuf)
	}
	return root
}

// seedPrincipal inserts a row into the principals table so that
// lookupPrincipalName can resolve it.
func seedPrincipal(t *testing.T, dir, uid, displayName string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite for principal seed: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT OR IGNORE INTO principals (principal_uid, display_name) VALUES (?, ?)`,
		uid, displayName,
	); err != nil {
		t.Fatalf("insert principal %s: %v", uid, err)
	}
}

// seedEntityRow inserts a row directly into the entities SQLite table so that
// queryEntityKindCounts has rows to count.
func seedEntityRow(t *testing.T, dir, uid, kind, createTime, scope string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite for entity seed: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT OR IGNORE INTO entities
		 (uid, kind, display_name, confidence, resolution_status, create_time, scope)
		 VALUES (?, ?, ?, 1.0, 'confirmed', ?, ?)`,
		uid, kind, kind+"-"+uid, createTime, scope,
	); err != nil {
		t.Fatalf("insert entity %s: %v", uid, err)
	}
}

// --- Help ---

// TestDigestHelp verifies that --help output is non-empty and contains the
// documented examples and key flags.
func TestDigestHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeDigestCmd(&buf, nil)
	root.SetArgs([]string{"digest", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz digest",
		"--since",
		"--by",
		"--output json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// --- Pure-function unit tests ---

// TestDigestSparklineStr exercises the sparkline generator with known inputs.
func TestDigestSparklineStr(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty string", func(t *testing.T) {
		t.Parallel()
		got := sparklineStr(nil)
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("all zeros renders spaces", func(t *testing.T) {
		t.Parallel()
		got := sparklineStr([]int{0, 0, 0})
		if got != "   " {
			t.Errorf("got %q, want three spaces", got)
		}
	})

	t.Run("single non-zero day renders top level", func(t *testing.T) {
		t.Parallel()
		got := sparklineStr([]int{5})
		// A single non-zero value should map to the highest level.
		if got != string(sparkLevels[len(sparkLevels)-1]) {
			t.Errorf("got %q, want %q", got, string(sparkLevels[len(sparkLevels)-1]))
		}
	})

	t.Run("length matches input length", func(t *testing.T) {
		t.Parallel()
		counts := []int{1, 0, 3, 2, 0, 5}
		got := sparklineStr(counts)
		// Each slot is exactly one Unicode rune.
		runes := []rune(got)
		if len(runes) != len(counts) {
			t.Errorf("sparkline length = %d, want %d (input: %v, got: %q)",
				len(runes), len(counts), counts, got)
		}
	})

	t.Run("levels increase with counts", func(t *testing.T) {
		t.Parallel()
		counts := []int{1, 5, 10}
		got := sparklineStr(counts)
		runes := []rune(got)
		// Each subsequent rune should be >= the previous since counts grow.
		for i := 1; i < len(runes); i++ {
			if runes[i] < runes[i-1] {
				t.Errorf("sparkline level decreased at index %d: %q < %q (full: %q)",
					i, string(runes[i]), string(runes[i-1]), got)
			}
		}
	})
}

// TestDigestSparklineCounts verifies that sparklineCounts fills in zeros for
// missing days within the window.
func TestDigestSparklineCounts(t *testing.T) {
	t.Parallel()

	t.Run("fills missing days with zeros", func(t *testing.T) {
		t.Parallel()
		since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
		daily := map[string]int{
			"2026-06-01": 3,
			"2026-06-03": 7,
		}
		got := sparklineCounts(daily, since, now)
		want := []int{3, 0, 7, 0, 0}
		if len(got) != len(want) {
			t.Fatalf("length = %d, want %d; got %v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("slot[%d] = %d, want %d", i, got[i], w)
			}
		}
	})

	t.Run("single day window has length one", func(t *testing.T) {
		t.Parallel()
		day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
		counts := sparklineCounts(map[string]int{"2026-06-10": 4}, day, day)
		if len(counts) != 1 || counts[0] != 4 {
			t.Errorf("got %v, want [4]", counts)
		}
	})
}

// TestDigestComputeStats tests the pure event-processing function with
// synthetic kernel.Event values.
func TestDigestComputeStats(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	makeEvent := func(uid, principalUID string, kind kernel.EventKind, ts time.Time) kernel.Event {
		return kernel.Event{
			UID:        uid,
			Kind:       kind,
			CreateTime: ts,
			Scope:      kernel.ScopePersonal,
			Author:     kernel.EventAuthor{PrincipalUID: principalUID},
		}
	}

	events := []kernel.Event{
		makeEvent("01", "alice", "episode", since.Add(1*time.Hour)),
		makeEvent("02", "alice", "episode", since.Add(2*time.Hour)),
		makeEvent("03", "bob", "entity", since.Add(3*time.Hour)),
		makeEvent("04", "bob", "episode", since.Add(24*time.Hour)),
		makeEvent("05", "alice", "relation", since.Add(25*time.Hour)),
	}

	t.Run("by writer counts correct totals", func(t *testing.T) {
		t.Parallel()
		writerMap, _, _ := computeDigestStats(events)

		aliceStat, ok := writerMap["alice"]
		if !ok {
			t.Fatal("alice not in writer map")
		}
		if aliceStat.Total != 3 {
			t.Errorf("alice total = %d, want 3", aliceStat.Total)
		}

		bobStat, ok := writerMap["bob"]
		if !ok {
			t.Fatal("bob not in writer map")
		}
		if bobStat.Total != 2 {
			t.Errorf("bob total = %d, want 2", bobStat.Total)
		}
	})

	t.Run("by writer kind breakdown correct", func(t *testing.T) {
		t.Parallel()
		writerMap, _, _ := computeDigestStats(events)

		alice := writerMap["alice"]
		if alice.ByKind["episode"] != 2 {
			t.Errorf("alice episode count = %d, want 2", alice.ByKind["episode"])
		}
		if alice.ByKind["relation"] != 1 {
			t.Errorf("alice relation count = %d, want 1", alice.ByKind["relation"])
		}

		bob := writerMap["bob"]
		if bob.ByKind["entity"] != 1 {
			t.Errorf("bob entity count = %d, want 1", bob.ByKind["entity"])
		}
		if bob.ByKind["episode"] != 1 {
			t.Errorf("bob episode count = %d, want 1", bob.ByKind["episode"])
		}
	})

	t.Run("by kind aggregates all writers", func(t *testing.T) {
		t.Parallel()
		_, kindMap, _ := computeDigestStats(events)

		if kindMap["episode"] != 3 {
			t.Errorf("episode count = %d, want 3", kindMap["episode"])
		}
		if kindMap["entity"] != 1 {
			t.Errorf("entity count = %d, want 1", kindMap["entity"])
		}
		if kindMap["relation"] != 1 {
			t.Errorf("relation count = %d, want 1", kindMap["relation"])
		}
	})

	t.Run("daily buckets group by calendar day", func(t *testing.T) {
		t.Parallel()
		_, _, daily := computeDigestStats(events)

		day1 := since.Format("2006-01-02")
		day2 := since.Add(24 * time.Hour).Format("2006-01-02")

		if daily[day1] != 3 {
			t.Errorf("day1 count = %d, want 3", daily[day1])
		}
		if daily[day2] != 2 {
			t.Errorf("day2 count = %d, want 2", daily[day2])
		}
	})

	t.Run("events with empty author uid use sentinel local", func(t *testing.T) {
		t.Parallel()
		ev := kernel.Event{
			UID:        "99",
			Kind:       "episode",
			CreateTime: since,
			Scope:      kernel.ScopePersonal,
			// Author.PrincipalUID intentionally empty.
		}
		writerMap, _, _ := computeDigestStats([]kernel.Event{ev})
		if _, ok := writerMap["local"]; !ok {
			t.Error("empty PrincipalUID should fall back to 'local' key")
		}
	})
}

// TestDigestKindMapToSlice verifies sorting by count desc, kind asc.
func TestDigestKindMapToSlice(t *testing.T) {
	t.Parallel()
	m := map[string]int{"b": 5, "a": 10, "c": 5}
	got := kindMapToSlice(m)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Kind != "a" || got[0].Count != 10 {
		t.Errorf("first = {%s %d}, want {a 10}", got[0].Kind, got[0].Count)
	}
	// Among equal counts, kinds are ordered alphabetically.
	if got[1].Kind != "b" || got[2].Kind != "c" {
		t.Errorf("tied kinds out of order: got %s, %s; want b, c", got[1].Kind, got[2].Kind)
	}
}

// TestDigestTopKindsStr verifies the compact breakdown string.
func TestDigestTopKindsStr(t *testing.T) {
	t.Parallel()
	m := map[string]int{"episode": 10, "entity": 3, "relation": 1}
	got := topKindsStr(m, 2)
	// Should only include the top 2 kinds.
	if !strings.Contains(got, "episode(10)") {
		t.Errorf("missing top kind: %q", got)
	}
	if !strings.Contains(got, "entity(3)") {
		t.Errorf("missing second kind: %q", got)
	}
	if strings.Contains(got, "relation") {
		t.Errorf("should not include third kind: %q", got)
	}
}

// --- Render-function tests ---

// testDigestMeta returns synthetic header/axis labels for renderDigestText
// tests (the meta is derived from flags in production, not part of the result).
func testDigestMeta() *digestRender {
	return &digestRender{
		spanLabel:  "last 7 days",
		scopeLabel: "project, personal",
		startLabel: "Jun 1",
		endLabel:   "Jun 8",
	}
}

// TestDigestRenderText exercises renderDigestText with a pre-built result to
// avoid needing a real data dir.
func TestDigestRenderText(t *testing.T) {
	t.Parallel()

	result := &digestResult{
		Since:       "2026-06-01T00:00:00Z",
		TotalEvents: 5,
		Writers: []writerStat{
			{
				PrincipalUID: "alice", DisplayName: "Alice", Total: 3,
				ByKind: map[string]int{"episode": 2, "relation": 1},
			},
			{
				PrincipalUID: "bob", DisplayName: "Bob", Total: 2,
				ByKind: map[string]int{"entity": 1, "episode": 1},
			},
		},
		NewEntities: []entityKindCount{
			{Kind: "person", Count: 2},
		},
		KindCounts: []kindStat{
			{Kind: "episode", Count: 3},
			{Kind: "entity", Count: 1},
			{Kind: "relation", Count: 1},
		},
		DailyBuckets: []dailyBucket{
			{Date: "2026-06-01", Count: 3},
			{Date: "2026-06-02", Count: 2},
		},
		Sparkline: "▃█",
	}

	t.Run("shows writers section", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cmd := newDigestCmd()
		cmd.SetOut(&buf)
		// Use a no-color colorizer for predictable output.
		clr := noopColorizer()
		if err := renderDigestText(cmd, &buf, clr, result, "", testDigestMeta()); err != nil {
			t.Fatalf("renderDigestText: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"Alice", "Bob", "▓"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q\nfull:\n%s", want, out)
			}
		}
	})

	t.Run("shows new entities section", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cmd := newDigestCmd()
		cmd.SetOut(&buf)
		clr := noopColorizer()
		if err := renderDigestText(cmd, &buf, clr, result, "", testDigestMeta()); err != nil {
			t.Fatalf("renderDigestText: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "person") {
			t.Errorf("output missing entity kind 'person'\nfull:\n%s", out)
		}
	})

	t.Run("shows kind section", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cmd := newDigestCmd()
		cmd.SetOut(&buf)
		clr := noopColorizer()
		if err := renderDigestText(cmd, &buf, clr, result, "", testDigestMeta()); err != nil {
			t.Fatalf("renderDigestText: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "episode") {
			t.Errorf("output missing 'episode' in kind section\nfull:\n%s", out)
		}
	})

	t.Run("shows sparkline", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cmd := newDigestCmd()
		cmd.SetOut(&buf)
		clr := noopColorizer()
		if err := renderDigestText(cmd, &buf, clr, result, "", testDigestMeta()); err != nil {
			t.Fatalf("renderDigestText: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "▃█") {
			t.Errorf("output missing sparkline '▃█'\nfull:\n%s", out)
		}
	})

	t.Run("by writer only shows writer section not kind", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cmd := newDigestCmd()
		cmd.SetOut(&buf)
		clr := noopColorizer()
		if err := renderDigestText(cmd, &buf, clr, result, "writer", testDigestMeta()); err != nil {
			t.Fatalf("renderDigestText: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Alice") {
			t.Errorf("writer mode missing 'Alice'\nfull:\n%s", out)
		}
		// Kind section and sparkline should be absent.
		if strings.Contains(out, "by event kind") {
			t.Errorf("writer mode must not show kind section\nfull:\n%s", out)
		}
		if strings.Contains(out, "▃█") {
			t.Errorf("writer mode must not show sparkline\nfull:\n%s", out)
		}
	})

	t.Run("by kind only shows kind section not writer", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		cmd := newDigestCmd()
		cmd.SetOut(&buf)
		clr := noopColorizer()
		if err := renderDigestText(cmd, &buf, clr, result, "kind", testDigestMeta()); err != nil {
			t.Fatalf("renderDigestText: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "episode") {
			t.Errorf("kind mode missing 'episode'\nfull:\n%s", out)
		}
		// Writer section should be absent.
		if strings.Contains(out, "by writer") {
			t.Errorf("kind mode must not show writer section\nfull:\n%s", out)
		}
	})
}

// TestDigestNoCOLOR verifies that NO_COLOR=1 suppresses ANSI and that the
// output still carries all meaningful data.
func TestDigestNoCOLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	result := &digestResult{
		Since:       "2026-06-01T00:00:00Z",
		TotalEvents: 2,
		Writers: []writerStat{
			{
				PrincipalUID: "alice", DisplayName: "Alice", Total: 2,
				ByKind: map[string]int{"episode": 2},
			},
		},
		KindCounts: []kindStat{{Kind: "episode", Count: 2}},
		Sparkline:  "▁█",
	}

	var buf bytes.Buffer
	cmd := newDigestCmd()
	cmd.SetOut(&buf)
	clr := noopColorizer()
	if err := renderDigestText(cmd, &buf, clr, result, "", testDigestMeta()); err != nil {
		t.Fatalf("renderDigestText: %v", err)
	}
	out := buf.String()

	// No ANSI escape sequences should appear.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR output must not contain ANSI escapes\nfull:\n%s", out)
	}
	// Data must still be present.
	if !strings.Contains(out, "Alice") {
		t.Errorf("missing 'Alice'\nfull:\n%s", out)
	}
}

// --- JSON output tests ---

// TestDigestJSONShape verifies that --output json emits a parseable document
// with all required keys.
func TestDigestJSONShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Open sqlite to run migrations (no events needed for JSON shape test).
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeDigestCmd(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--data-dir", dir,
		"digest",
		"--since", "7d",
		"--output", "json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr: %s)", err, errBuf.String())
	}

	raw := strings.TrimSpace(outBuf.String())
	if len(raw) == 0 {
		t.Fatal("JSON output is empty")
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("parse JSON: %v\nraw: %s", err, raw)
	}

	requiredKeys := []string{"since", "totalEvents", "writers", "newEntities", "kindCounts", "dailyBuckets", "sparkline"}
	for _, key := range requiredKeys {
		if _, ok := parsed[key]; !ok {
			t.Errorf("JSON output missing key %q\nfull: %s", key, raw)
		}
	}
}

// TestDigestJSONWriterResolution seeds a principal and two entity rows, then
// verifies that the JSON output reflects the display name and entity counts.
func TestDigestJSONWriterResolution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Run migrations by opening sqlite once.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	// Seed a principal so name resolution works.
	seedPrincipal(t, dir, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "Pippin Took")

	// Seed entity rows created comfortably inside the --since 1h window so
	// queryEntityKindCounts finds them. Use 5m (not exactly 1h): the cutoff is
	// recomputed at query time, so a timestamp on the 1h boundary drifts outside
	// the window under scheduler delay and the test flakes (count → 0).
	recentTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	seedEntityRow(t, dir, "ent01", "person", recentTime, "personal")
	seedEntityRow(t, dir, "ent02", "person", recentTime, "personal")
	seedEntityRow(t, dir, "ent03", "place", recentTime, "personal")

	// No JSONL events written — entity counts come purely from SQLite.
	var outBuf, errBuf bytes.Buffer
	root := makeDigestCmd(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--data-dir", dir,
		"digest",
		"--since", "1h",
		"--output", "json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr: %s)", err, errBuf.String())
	}

	var result digestResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &result); err != nil {
		t.Fatalf("parse JSON: %v\nraw: %s", err, outBuf.String())
	}

	// Entity counts: 2 person + 1 place.
	kindMap := make(map[string]int)
	for _, ec := range result.NewEntities {
		kindMap[ec.Kind] = ec.Count
	}
	if kindMap["person"] != 2 {
		t.Errorf("newEntities person count = %d, want 2 (got %+v)", kindMap["person"], result.NewEntities)
	}
	if kindMap["place"] != 1 {
		t.Errorf("newEntities place count = %d, want 1 (got %+v)", kindMap["place"], result.NewEntities)
	}
}

// TestDigestByFlagValidation verifies that --by with an invalid value returns
// exit code 1 with a helpful message.
func TestDigestByFlagValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Run migrations.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeDigestCmd(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--data-dir", dir,
		"digest",
		"--by", "invalid-value",
	})
	err = root.Execute()
	if err == nil {
		t.Fatal("expected error for invalid --by, got nil")
	}
	if !strings.Contains(errBuf.String(), "--by") {
		t.Errorf("stderr missing --by error hint: %q", errBuf.String())
	}
}

// noopColorizer returns a colorizer with colour disabled so tests produce
// plain ASCII output without ANSI sequences, ensuring stable assertions.
func noopColorizer() *colorizer.Colorizer {
	return colorizer.New(false, nil)
}
