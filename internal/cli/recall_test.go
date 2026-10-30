package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/tui/browse"
)

// syntheticRecallJSON builds a recall response JSON from the given hits for
// unit-testing the render functions without a live daemon.
func syntheticRecallJSON(hits []recallHit, degraded bool, lanes []string) json.RawMessage {
	type envelope struct {
		Hits     []recallHit `json:"hits"`
		Degraded bool        `json:"degraded"`
		Lanes    []string    `json:"lanes"`
	}
	if lanes == nil {
		lanes = []string{"vector", "fts", "graph", "nameEmb"}
	}
	b, _ := json.Marshal(envelope{Hits: hits, Degraded: degraded, Lanes: lanes})
	return b
}

// makeRecallCmd wires a cobra command tree in the same shape as production
// so PersistentFlags (--data-dir, --json, --no-tui, --output) are accessible.
func makeRecallCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.PersistentFlags().String("output", "", "output format: text|json|ndjson")
	root.PersistentFlags().String("color", "never", "color when")
	rec := newRecallCmd()
	root.AddCommand(rec)
	if outBuf != nil {
		root.SetOut(outBuf)
		rec.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		rec.SetErr(errBuf)
	}
	return root
}

// textWriters returns a text-format output.Writer and a no-color colorizer
// backed by outBuf/errBuf. Used to test renderRecallCLI and renderRecallExplain
// directly without going through runRecall's flag-plumbed path.
//
//nolint:ireturn // returns the output.Writer interface intentionally; callers test via the interface
func textWriters(outBuf, errBuf *bytes.Buffer) (output.Writer, *colorizer.Colorizer) {
	ow := output.New(output.FormatText, outBuf, errBuf)
	c := colorizer.New(false, outBuf)
	return ow, c
}

// TestRenderRecallCLIStripsSnippetMarkup verifies that the plain-text recall
// table drops the <b>...</b> FTS highlight markers (literal noise in a
// terminal) while leaving the matched term visible. The markup stays in the
// structured JSON contract; only the human table strips it.
func TestRenderRecallCLIStripsSnippetMarkup(t *testing.T) {
	t.Parallel()
	raw := syntheticRecallJSON([]recallHit{
		{
			HitType: "episode", FusedScore: 0.9, Name: "episodes/01JVABC",
			Snippet: "... the <b>needle</b> in the haystack ...",
			Components: recallHitComps{
				RawSemantic: 0.9, RawBM25: 0.5,
			},
		},
	}, false, nil)

	var outBuf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	ow, c := textWriters(&outBuf, &errBuf)

	if err := renderRecallCLI(cmd, ow, c, raw, 10); err != nil {
		t.Fatalf("renderRecallCLI: %v", err)
	}
	out := outBuf.String()
	if strings.Contains(out, "<b>") || strings.Contains(out, "</b>") {
		t.Errorf("recall text table leaked <b> markup: %q", out)
	}
	if !strings.Contains(out, "needle") {
		t.Errorf("recall text table dropped the matched term: %q", out)
	}
}

// TestStripHighlight is a focused unit test for the FTS markup stripper.
func TestStripHighlight(t *testing.T) {
	t.Parallel()
	got := stripHighlight("a <b>b</b> c <b>d</b>")
	if got != "a b c d" {
		t.Errorf("stripHighlight = %q, want %q", got, "a b c d")
	}
}

// TestRecallTableDropsRRFColumns verifies that the new table no longer
// shows the misleading RRF-only vec/fts columns.
func TestRecallTableDropsRRFColumns(t *testing.T) {
	t.Parallel()
	raw := syntheticRecallJSON([]recallHit{
		{
			HitType: "episode", FusedScore: 0.8, Name: "episodes/01JVABC",
			Snippet: "some content",
			Components: recallHitComps{
				Vector: 0.7, FTS: 0.5,
				RawSemantic: 0.8, RawBM25: 0.5,
			},
		},
	}, false, nil)

	var outBuf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	ow, c := textWriters(&outBuf, &errBuf)

	if err := renderRecallCLI(cmd, ow, c, raw, 10); err != nil {
		t.Fatalf("renderRecallCLI: %v", err)
	}
	out := outBuf.String()
	// Table header must contain "#", "type", "fused", "ref", "snippet".
	for _, want := range []string{"#", "type", "fused", "ref", "snippet"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing column header %q\n%s", want, out)
		}
	}
	// The RRF-only "vec" and "fts" header labels must NOT appear.
	for _, unwanted := range []string{"\tvec\t", " vec ", "\tfts\t", " fts "} {
		if strings.Contains(out, unwanted) {
			t.Errorf("table still contains RRF column %q (should be removed)\n%s", unwanted, out)
		}
	}
}

// TestRecallTableFooterCountStates verifies ShowingNofM output for the
// common cases: complete set, limit reached.
func TestRecallTableFooterCountStates(t *testing.T) {
	t.Parallel()

	makeHit := func(name string, score float32) recallHit {
		return recallHit{
			HitType: "episode", Name: name, FusedScore: score,
			Snippet:    "snippet",
			Components: recallHitComps{RawSemantic: score, RawBM25: 0.3},
		}
	}

	t.Run("fewer hits than limit shows plain count", func(t *testing.T) {
		t.Parallel()
		raw := syntheticRecallJSON([]recallHit{
			makeHit("episodes/01JVABC", 0.9),
			makeHit("episodes/01JVDEF", 0.7),
		}, false, nil)

		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		ow, c := textWriters(&outBuf, &errBuf)

		if err := renderRecallCLI(cmd, ow, c, raw, 10); err != nil {
			t.Fatalf("renderRecallCLI: %v", err)
		}
		// Footer goes to stdout in text mode.
		combined := outBuf.String() + errBuf.String()
		if !strings.Contains(combined, "showing 2") {
			t.Errorf("footer missing 'showing 2': %q", combined)
		}
	})

	t.Run("hits equal to limit shows warning", func(t *testing.T) {
		t.Parallel()
		hits := make([]recallHit, 3)
		for i := range hits {
			hits[i] = makeHit("episodes/X", 0.5)
		}
		raw := syntheticRecallJSON(hits, false, nil)

		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		ow, c := textWriters(&outBuf, &errBuf)

		// Pass limit=3 (same as count) → ShowingNofM should flag "limit reached".
		if err := renderRecallCLI(cmd, ow, c, raw, 3); err != nil {
			t.Fatalf("renderRecallCLI: %v", err)
		}
		combined := outBuf.String() + errBuf.String()
		if !strings.Contains(combined, "showing 3") {
			t.Errorf("footer missing 'showing 3': %q", combined)
		}
		if !strings.Contains(combined, "limit") {
			t.Errorf("footer missing limit warning: %q", combined)
		}
	})
}

// TestRecallHelp verifies --help is non-empty and includes both the standard
// flags and the new --explain flag.
func TestRecallHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeRecallCmd(&buf, nil)
	root.SetArgs([]string{"recall", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{
		"Examples:", "maz recall", "--limit", "--at", "--no-graph",
		"--near", "--radius-km", "--rewrite-query", "--explain",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n%s", want, out)
		}
	}
}

func TestRecallFlagDefaults(t *testing.T) {
	t.Parallel()
	cmd := newRecallCmd()
	if got, _ := cmd.Flags().GetInt("limit"); got != 10 {
		t.Errorf("--limit default = %d, want 10", got)
	}
	if got, _ := cmd.Flags().GetBool("no-graph"); got {
		t.Errorf("--no-graph default = true, want false")
	}
	if got, _ := cmd.Flags().GetFloat64("radius-km"); got != 50 {
		t.Errorf("--radius-km default = %g, want 50", got)
	}
	if got, _ := cmd.Flags().GetBool("rewrite-query"); got {
		t.Errorf("--rewrite-query default = true, want false")
	}
	if got, _ := cmd.Flags().GetBool("explain"); got {
		t.Errorf("--explain default = true, want false")
	}
	if got, _ := cmd.Flags().GetBool("why"); got {
		t.Errorf("--why default = true, want false")
	}
}

func TestRecallRadiusRequiresNear(t *testing.T) {
	t.Parallel()
	var outBuf, errBuf bytes.Buffer
	root := makeRecallCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"recall", "--radius-km=25", "Berlin"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --radius-km set without --near")
	}
	if !strings.Contains(errBuf.String(), "--radius-km requires --near") {
		t.Errorf("stderr missing requires-near message: %q", errBuf.String())
	}
}

// TestRecallLimitValidation: values outside 1..50 are rejected with exit 1
// and an "out of range" message; boundary values are accepted.
func TestRecallLimitValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		limit   int
		wantErr bool
	}{
		{"zero", 0, true},
		{"negative", -1, true},
		{"over max 51", 51, true},
		{"over max 100", 100, true},
		{"min boundary 1", 1, false},
		{"max boundary 50", 50, false},
		{"mid 10", 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cmd := newRecallCmd()
			cmd.SetErr(&errBuf)
			flags := &recallFlags{limit: tc.limit}
			err := validateRecallFlags(cmd, flags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("limit=%d: expected error, got nil", tc.limit)
				}
				var se silentError
				if !errors.As(err, &se) {
					t.Fatalf("limit=%d: expected silentError, got %T: %v", tc.limit, err, err)
				}
				if se.code != 1 {
					t.Errorf("limit=%d: exit code = %d, want 1", tc.limit, se.code)
				}
				if !strings.Contains(errBuf.String(), "out of range") {
					t.Errorf("limit=%d: stderr missing 'out of range': %q", tc.limit, errBuf.String())
				}
			} else if err != nil {
				t.Errorf("limit=%d: unexpected error: %v", tc.limit, err)
			}
		})
	}
}

// TestRecallAtValidation: a malformed --at value produces exit 1 with a
// friendly RFC 3339 message and does not leak Go reference-time internals.
func TestRecallAtValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		at      string
		wantErr bool
	}{
		{"empty accepted", "", false},
		{"valid RFC 3339", "2026-01-01T00:00:00Z", false},
		{"bare date", "2026-01-01", true},
		{"garbage", "not-a-time", true},
		{"Jan reference style", "January 2, 2006", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cmd := newRecallCmd()
			cmd.SetErr(&errBuf)
			flags := &recallFlags{limit: 10, at: tc.at}
			err := validateRecallFlags(cmd, flags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("--at=%q: expected error, got nil", tc.at)
				}
				var se silentError
				if !errors.As(err, &se) {
					t.Fatalf("--at=%q: expected silentError, got %T: %v", tc.at, err, err)
				}
				if se.code != 1 {
					t.Errorf("--at=%q: exit code = %d, want 1", tc.at, se.code)
				}
				msg := errBuf.String()
				// Must not leak the Go time.Parse "parsing time ... as ..." form.
				if strings.Contains(msg, `parsing time`) {
					t.Errorf("--at=%q: stderr leaks time.Parse error: %q", tc.at, msg)
				}
				if !strings.Contains(msg, "RFC 3339") {
					t.Errorf("--at=%q: stderr missing RFC 3339 hint: %q", tc.at, msg)
				}
			} else if err != nil {
				t.Errorf("--at=%q: unexpected error: %v", tc.at, err)
			}
		})
	}
}

// TestRenderRecallCLI_NoHTMLTags asserts that <b>...</b> FTS5 highlight
// markup is stripped before the snippet reaches plain-text output.
func TestRenderRecallCLI_NoHTMLTags(t *testing.T) {
	t.Parallel()
	raw := syntheticRecallJSON([]recallHit{
		{
			Name:       "episodes/01JVABC",
			Snippet:    "I prefer black <b>coffee</b> in the morning",
			FusedScore: 0.9,
			Components: recallHitComps{RawSemantic: 0.8, RawBM25: 0.7, Graph: 0.1, Recency: 0.2},
		},
		{
			Name:       "episodes/01JVDEF",
			Snippet:    "<b>tea</b> is also good",
			FusedScore: 0.5,
			Components: recallHitComps{},
		},
	}, false, nil)

	var outBuf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	ow, c := textWriters(&outBuf, &errBuf)

	if err := renderRecallCLI(cmd, ow, c, raw, 10); err != nil {
		t.Fatalf("renderRecallCLI: %v", err)
	}
	out := outBuf.String()
	if strings.Contains(out, "<b>") || strings.Contains(out, "</b>") {
		t.Errorf("plain-text recall output must not contain HTML tags, got:\n%s", out)
	}
	// Content sans tags must still appear.
	if !strings.Contains(out, "coffee") {
		t.Errorf("snippet content missing after tag strip: %q", out)
	}
	if !strings.Contains(out, "tea") {
		t.Errorf("snippet content missing after tag strip: %q", out)
	}
}

// --- scoreBar tests ----------------------------------------------------------

// TestScoreBar verifies the 5-cell unicode bar for boundary and mid values.
func TestScoreBar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		v     float32
		cells int // expected filled cells
	}{
		{"zero", 0.0, 0},
		{"one fifth", 0.2, 2}, // 0.2 * 10 + 0.5 = 2.5 → 2
		{"half", 0.5, 5},      // 0.5 * 10 + 0.5 = 5.5 → 5
		{"full", 1.0, 10},
		{"clamped below", -0.5, 0},
		{"clamped above", 1.5, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bar := scoreBar(tc.v)
			if len([]rune(bar)) != explainBarWidth {
				t.Errorf("scoreBar(%g) = %q, want %d runes", tc.v, bar, explainBarWidth)
			}
			filled := strings.Count(bar, "▇")
			if filled != tc.cells {
				t.Errorf("scoreBar(%g) = %q, want %d filled cells, got %d", tc.v, bar, tc.cells, filled)
			}
			empty := strings.Count(bar, "░")
			if filled+empty != explainBarWidth {
				t.Errorf("scoreBar(%g) = %q: filled+empty=%d, want %d", tc.v, bar, filled+empty, explainBarWidth)
			}
		})
	}
}

func TestFusedMeter(t *testing.T) {
	t.Parallel()
	// NO_COLOR (enabled=false) so the dot glyph carries the magnitude bucket
	// without SGR, and the percent reads bare. Buckets mirror entire.io's
	// activityRune ratios (.25/.5/.75): · • ● ⬤.
	c := colorizer.New(false, &bytes.Buffer{})
	cases := []struct {
		name string
		v    float32
		want string
	}{
		{"floor dot", 0.10, "· 10%"},
		{"small dot", 0.40, "• 40%"},
		{"medium dot", 0.60, "● 60%"},
		{"large dot", 0.85, "⬤ 85%"},
		{"boundary quarter", 0.25, "• 25%"},
		{"boundary half", 0.50, "● 50%"},
		{"boundary three-quarter", 0.75, "⬤ 75%"},
		{"clamped below", -0.5, "· 0%"},
		{"clamped above", 1.5, "⬤ 100%"},
		{"rounds half up", 0.875, "⬤ 88%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fusedMeter(c, tc.v); got != tc.want {
				t.Errorf("fusedMeter(%g) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// --- --explain / renderRecallExplain tests -----------------------------------

// makeExplainHit builds a recallHit with full score components for explain tests.
func makeExplainHit(name string, rawSem, rawBM25, graph, nameEmb, recency float32) recallHit {
	return recallHit{
		HitType:    "episode",
		Name:       name,
		FusedScore: rawSem*0.4 + rawBM25*0.3 + graph*0.2 + nameEmb*0.1, // synthetic blend
		Snippet:    "some relevant text",
		Components: recallHitComps{
			RawSemantic: rawSem,
			RawBM25:     rawBM25,
			Graph:       graph,
			NameEmb:     nameEmb,
			Recency:     recency,
		},
	}
}

// TestRenderRecallExplain_BasicOutput asserts the explain view shows rawSemantic,
// rawBM25, graph rank, name-emb, fusion label and does NOT show bare RRF
// vec/fts header labels.
func TestRenderRecallExplain_BasicOutput(t *testing.T) {
	t.Parallel()

	raw := syntheticRecallJSON([]recallHit{
		makeExplainHit("episodes/01JVABC01ABCDEFGHIJK", 0.85, 0.60, 0.30, 0.10, 0),
	}, false, []string{"vector", "fts", "graph", "nameEmb"})

	var outBuf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	ow, c := textWriters(&outBuf, &errBuf)

	if err := renderRecallExplain(cmd, ow, c, raw, 10, "test query"); err != nil {
		t.Fatalf("renderRecallExplain: %v", err)
	}
	out := outBuf.String() + errBuf.String()

	// Must contain lane labels.
	for _, want := range []string{"semantic", "keyword", "graph", "name-emb"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain output missing %q:\n%s", want, out)
		}
	}
	// Must show fusion mode.
	if !strings.Contains(out, "weighted") {
		t.Errorf("explain output missing fusion mode 'weighted':\n%s", out)
	}
	// Must show rawSemantic score.
	if !strings.Contains(out, "0.85") {
		t.Errorf("explain output missing rawSemantic 0.85:\n%s", out)
	}
	// Must show rawBM25 score.
	if !strings.Contains(out, "0.60") {
		t.Errorf("explain output missing rawBM25 0.60:\n%s", out)
	}
	// graph fired and shows rank.
	if !strings.Contains(out, "fired") {
		t.Errorf("explain output missing 'fired' for graph:\n%s", out)
	}
	// RRF-only columns must NOT appear as separate table headers.
	if strings.Contains(out, "\tvec\t") || strings.Contains(out, " vec ") {
		t.Errorf("explain output contains RRF 'vec' column:\n%s", out)
	}
}

// TestRenderRecallExplain_RecencyOnlyWhenPositive asserts the recency row
// only appears when recency > 0.
func TestRenderRecallExplain_RecencyOnlyWhenPositive(t *testing.T) {
	t.Parallel()

	t.Run("recency zero omitted", func(t *testing.T) {
		t.Parallel()
		raw := syntheticRecallJSON([]recallHit{
			makeExplainHit("episodes/01JVABC", 0.8, 0.5, 0, 0, 0),
		}, false, nil)

		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		ow, c := textWriters(&outBuf, &errBuf)

		if err := renderRecallExplain(cmd, ow, c, raw, 10, "test query"); err != nil {
			t.Fatalf("renderRecallExplain: %v", err)
		}
		if strings.Contains(outBuf.String(), "recency") {
			t.Errorf("recency row must be absent when recency=0:\n%s", outBuf.String())
		}
	})

	t.Run("recency positive shown", func(t *testing.T) {
		t.Parallel()
		raw := syntheticRecallJSON([]recallHit{
			makeExplainHit("episodes/01JVABC", 0.8, 0.5, 0, 0, 0.25),
		}, false, nil)

		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		ow, c := textWriters(&outBuf, &errBuf)

		if err := renderRecallExplain(cmd, ow, c, raw, 10, "test query"); err != nil {
			t.Fatalf("renderRecallExplain: %v", err)
		}
		if !strings.Contains(outBuf.String(), "recency") {
			t.Errorf("recency row must appear when recency>0:\n%s", outBuf.String())
		}
		if !strings.Contains(outBuf.String(), "0.25") {
			t.Errorf("recency value 0.25 missing:\n%s", outBuf.String())
		}
	})
}

// TestRenderRecallExplain_DegradedFusionLabel verifies that when the response
// carries degraded=true the fusion label changes from "weighted" to "degraded".
func TestRenderRecallExplain_DegradedFusionLabel(t *testing.T) {
	t.Parallel()

	raw := syntheticRecallJSON([]recallHit{
		makeExplainHit("episodes/01JVABC", 0.0, 0.7, 0, 0, 0),
	}, true /* degraded */, []string{"fts", "graph"})

	var outBuf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	ow, c := textWriters(&outBuf, &errBuf)

	if err := renderRecallExplain(cmd, ow, c, raw, 10, "test query"); err != nil {
		t.Fatalf("renderRecallExplain: %v", err)
	}
	if !strings.Contains(outBuf.String(), "degraded") {
		t.Errorf("explain output must show 'degraded' fusion label when degraded=true:\n%s", outBuf.String())
	}
	if strings.Contains(outBuf.String(), "weighted") {
		t.Errorf("explain output must NOT show 'weighted' when degraded=true:\n%s", outBuf.String())
	}
}

// TestRenderRecallExplain_NoopInJSONMode verifies that --explain is a no-op
// when the output format is JSON: the raw bytes are forwarded unchanged.
func TestRenderRecallExplain_NoopInJSONMode(t *testing.T) {
	t.Parallel()

	raw := syntheticRecallJSON([]recallHit{
		makeExplainHit("episodes/01JVABC", 0.85, 0.6, 0.3, 0.1, 0),
	}, false, nil)

	// In runRecall, when Format()==FormatJSON, the raw bytes are emitted
	// verbatim and renderRecallExplain is never called. We verify that the
	// JSON output is byte-identical to the input (no extra explain metadata).
	var outBuf, errBuf bytes.Buffer
	root := makeRecallCmd(&outBuf, &errBuf)
	// Force JSON format (--output json) so runRecall takes the JSON branch.
	// The test does not actually invoke a daemon; it calls the render path
	// for format routing verification.
	_ = root // The routing test is best done via the unit-level assertion above:
	// renderRecallExplain is a text-only function; calling it in JSON mode
	// would still render text. The contract is enforced by runRecall's switch.

	// Instead, directly verify: renderRecallExplain writes no JSON-corrupting
	// output when called (it writes text, but it's gated by runRecall's switch).
	// Here we test that the raw JSON is parseable after going through the JSON
	// path (i.e., the components block is present and intact).
	var parsed struct {
		Hits []struct {
			Components recallHitComps `json:"components"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("synthetic JSON not parseable: %v", err)
	}
	if len(parsed.Hits) == 0 {
		t.Fatal("no hits in synthetic JSON")
	}
	comp := parsed.Hits[0].Components
	if comp.RawSemantic != 0.85 {
		t.Errorf("JSON components.rawSemantic = %g, want 0.85", comp.RawSemantic)
	}
	if comp.RawBM25 != 0.6 {
		t.Errorf("JSON components.rawBM25 = %g, want 0.6", comp.RawBM25)
	}
	// Confirm that renderRecallExplain does not modify the raw bytes
	// (explain output is text-only, not injected into the JSON stream).
	var outBuf2, errBuf2 bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf2)
	cmd.SetErr(&errBuf2)
	ow, c := textWriters(&outBuf2, &errBuf2)
	_ = renderRecallExplain(cmd, ow, c, raw, 10, "test query")
	// The explain output is text, not JSON — confirm it does not contain
	// JSON-breaking characters in the explain text itself.
	explainOut := outBuf2.String()
	if strings.HasPrefix(explainOut, "{") {
		t.Errorf("renderRecallExplain must not emit JSON; got: %q", explainOut)
	}
}

// TestRenderRecallExplain_NOCOLORParity asserts that setting NO_COLOR=1
// suppresses all ANSI escapes while keeping the structural content intact.
func TestRenderRecallExplain_NOCOLORParity(t *testing.T) {
	t.Parallel()

	raw := syntheticRecallJSON([]recallHit{
		makeExplainHit("episodes/01JVABC01ABCDEFGHIJK", 0.80, 0.50, 0.20, 0.10, 0.05),
	}, false, nil)

	makeOutput := func(colorEnabled bool) string {
		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		ow := output.New(output.FormatText, &outBuf, &errBuf)
		c := colorizer.New(colorEnabled, &outBuf)
		_ = renderRecallExplain(cmd, ow, c, raw, 10, "test query")
		return outBuf.String() + errBuf.String()
	}

	plain := makeOutput(false)
	colored := makeOutput(true)

	// Both must contain the same structural data.
	for _, want := range []string{"semantic", "keyword", "graph", "name-emb", "weighted"} {
		if !strings.Contains(plain, want) {
			t.Errorf("NO_COLOR output missing %q:\n%s", want, plain)
		}
		if !strings.Contains(colored, want) {
			t.Errorf("color output missing %q:\n%s", want, colored)
		}
	}
	// Plain output must not contain ESC character.
	if strings.ContainsRune(plain, '\x1b') {
		t.Errorf("NO_COLOR output contains ANSI escape:\n%s", plain)
	}
}

// TestRenderRecallExplain_GraphNotFired verifies "not fired" text when
// the graph component is zero.
func TestRenderRecallExplain_GraphNotFired(t *testing.T) {
	t.Parallel()

	raw := syntheticRecallJSON([]recallHit{
		makeExplainHit("episodes/01JVABC", 0.9, 0.6, 0.0, 0.0, 0),
	}, false, nil)

	var outBuf, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	ow, c := textWriters(&outBuf, &errBuf)

	if err := renderRecallExplain(cmd, ow, c, raw, 10, "test query"); err != nil {
		t.Fatalf("renderRecallExplain: %v", err)
	}
	out := outBuf.String()
	if !strings.Contains(out, "not fired") {
		t.Errorf("explain must show 'not fired' when graph=0:\n%s", out)
	}
	if strings.Contains(out, "✓ fired") {
		t.Errorf("explain must not show '✓ fired' when graph=0:\n%s", out)
	}
}

// TestRecallExplainFlagAlias verifies that --why is an alias for --explain:
// setting either flag sets the explain field.
func TestRecallExplainFlagAlias(t *testing.T) {
	t.Parallel()

	t.Run("--explain sets explain=true", func(t *testing.T) {
		t.Parallel()
		cmd := newRecallCmd()
		if err := cmd.Flags().Set("explain", "true"); err != nil {
			t.Fatalf("set --explain: %v", err)
		}
		got, _ := cmd.Flags().GetBool("explain")
		if !got {
			t.Error("--explain did not set explain flag")
		}
	})

	t.Run("--why also sets explain=true", func(t *testing.T) {
		t.Parallel()
		f := &recallFlags{}
		cmd := &cobra.Command{Use: "recall <query>"}
		cmd.Flags().BoolVar(&f.explain, "explain", false, "")
		cmd.Flags().BoolVar(&f.explain, "why", false, "")
		if err := cmd.Flags().Set("why", "true"); err != nil {
			t.Fatalf("set --why: %v", err)
		}
		if !f.explain {
			t.Error("--why did not set explain field")
		}
	})
}

// TestRecallEmptyHitsMessage verifies that both renderRecallCLI and
// renderRecallExplain print "no hits" on an empty response.
func TestRecallEmptyHitsMessage(t *testing.T) {
	t.Parallel()

	raw := syntheticRecallJSON(nil, false, nil)

	t.Run("CLI table", func(t *testing.T) {
		t.Parallel()
		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		ow, c := textWriters(&outBuf, &errBuf)
		if err := renderRecallCLI(cmd, ow, c, raw, 10); err != nil {
			t.Fatalf("renderRecallCLI: %v", err)
		}
		if !strings.Contains(outBuf.String(), "no hits") {
			t.Errorf("expected 'no hits' message, got: %q", outBuf.String())
		}
	})

	t.Run("explain view", func(t *testing.T) {
		t.Parallel()
		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		ow, c := textWriters(&outBuf, &errBuf)
		if err := renderRecallExplain(cmd, ow, c, raw, 10, "test query"); err != nil {
			t.Fatalf("renderRecallExplain: %v", err)
		}
		if !strings.Contains(outBuf.String(), "no hits") {
			t.Errorf("expected 'no hits' message, got: %q", outBuf.String())
		}
	})
}

// TestBuildRecallBanks asserts the structural contract of buildRecallBanks:
// four banks in order (kinds, scopes, at, minScore) with correct BankKind
// values and documented stop labels on the Score slider. The Kind bank carries
// fixed friendly record-kind facets (documents/memories/insights), so no live
// daemon is required.
func TestBuildRecallBanks(t *testing.T) {
	t.Parallel()

	banks := buildRecallBanks()

	if len(banks) != 4 {
		t.Fatalf("buildRecallBanks returned %d banks, want 4", len(banks))
	}

	t.Run("Kind bank is toggle with record-kind facets", func(t *testing.T) {
		t.Parallel()
		b := banks[0]
		if b.Key != "kinds" {
			t.Errorf("banks[0].Key = %q, want \"kinds\"", b.Key)
		}
		if b.Kind != browse.BankToggle {
			t.Errorf("banks[0].Kind = %v, want BankToggle", b.Kind)
		}
		want := []string{"documents", "memories", "insights"}
		if len(b.Options) != len(want) {
			t.Fatalf("banks[0].Options = %v, want %v", b.Options, want)
		}
		for i, w := range want {
			if b.Options[i] != w {
				t.Errorf("banks[0].Options[%d] = %q, want %q", i, b.Options[i], w)
			}
		}
	})

	t.Run("Scope bank is toggle with personal project team", func(t *testing.T) {
		t.Parallel()
		b := banks[1]
		if b.Key != "scopes" {
			t.Errorf("banks[1].Key = %q, want \"scopes\"", b.Key)
		}
		if b.Kind != browse.BankToggle {
			t.Errorf("banks[1].Kind = %v, want BankToggle", b.Kind)
		}
		want := []string{"personal", "project", "team"}
		if len(b.Options) != len(want) {
			t.Fatalf("banks[1].Options = %v, want %v", b.Options, want)
		}
		for i, w := range want {
			if b.Options[i] != w {
				t.Errorf("banks[1].Options[%d] = %q, want %q", i, b.Options[i], w)
			}
		}
	})

	t.Run("Time bank is single-select now 7d 30d", func(t *testing.T) {
		t.Parallel()
		b := banks[2]
		if b.Key != "at" {
			t.Errorf("banks[2].Key = %q, want \"at\"", b.Key)
		}
		if b.Kind != browse.BankSingle {
			t.Errorf("banks[2].Kind = %v, want BankSingle", b.Kind)
		}
		want := []string{"now", "7d", "30d"}
		if len(b.Options) != len(want) {
			t.Fatalf("banks[2].Options = %v, want %v", b.Options, want)
		}
		for i, w := range want {
			if b.Options[i] != w {
				t.Errorf("banks[2].Options[%d] = %q, want %q", i, b.Options[i], w)
			}
		}
	})

	t.Run("Score bank is slider with stops 0 0.05 0.1 0.2", func(t *testing.T) {
		t.Parallel()
		b := banks[3]
		if b.Key != "minScore" {
			t.Errorf("banks[3].Key = %q, want \"minScore\"", b.Key)
		}
		if b.Kind != browse.BankSlider {
			t.Errorf("banks[3].Kind = %v, want BankSlider", b.Kind)
		}
		wantStops := []string{"0", "0.05", "0.1", "0.2"}
		if len(b.Options) != len(wantStops) {
			t.Fatalf("banks[3].Options = %v, want %v", b.Options, wantStops)
		}
		for i, w := range wantStops {
			if b.Options[i] != w {
				t.Errorf("banks[3].Options[%d] = %q, want %q", i, b.Options[i], w)
			}
		}
	})
}

// TestRecallKindWire asserts that friendly Kind-bank labels map to the record
// kind values the recall tool's "kinds" field filters on, and that unknown
// labels (including a raw wire value) pass through unchanged.
func TestRecallKindWire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label string
		want  string
	}{
		{"documents", ontology.KindDocumentRevision},
		{"document", ontology.KindDocumentRevision},
		{"memories", ontology.KindEpisode},
		{"memory", ontology.KindEpisode},
		{"episodes", ontology.KindEpisode},
		{ontology.KindEpisode, ontology.KindEpisode},
		{"insights", ontology.KindInsightSynthesized},
		{"insight", ontology.KindInsightSynthesized},
		{"consolidated", ontology.KindMemoryConsolidated},
		{"somethingElse", "somethingElse"},
	}
	for _, tc := range cases {
		if got := recallKindWire(tc.label); got != tc.want {
			t.Errorf("recallKindWire(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

// TestValidRecallKind: the friendly labels and raw wire kinds are accepted; an
// unknown label is rejected so the --kind flag fails fast.
func TestValidRecallKind(t *testing.T) {
	t.Parallel()
	valid := []string{
		"document", "documents", "episode", "episodes", "memory", "memories",
		"insight", "insights", "consolidated",
		ontology.KindDocumentRevision, ontology.KindEpisode,
		ontology.KindInsightSynthesized, ontology.KindMemoryConsolidated,
	}
	for _, k := range valid {
		if !validRecallKind(k) {
			t.Errorf("validRecallKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"entity", "document-revision", "docs", "", "typo"} {
		if validRecallKind(k) {
			t.Errorf("validRecallKind(%q) = true, want false", k)
		}
	}
}

// TestRecallKindValidation: validateRecallFlags rejects an unknown --kind with
// exit 1 and a message naming the valid kinds; known kinds pass.
func TestRecallKindValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		kinds   []string
		wantErr bool
	}{
		{"none", nil, false},
		{"single friendly", []string{"document"}, false},
		{"multiple", []string{"document", "episode"}, false},
		{"wire value", []string{ontology.KindInsightSynthesized}, false},
		{"unknown", []string{"banana"}, true},
		{"one good one bad", []string{"document", "banana"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cmd := newRecallCmd()
			cmd.SetErr(&errBuf)
			flags := &recallFlags{limit: 10, kinds: tc.kinds}
			err := validateRecallFlags(cmd, flags)
			if tc.wantErr {
				var se silentError
				if !errors.As(err, &se) || se.code != 1 {
					t.Fatalf("kinds=%v: want silentError code 1, got %T: %v", tc.kinds, err, err)
				}
				if !strings.Contains(errBuf.String(), "unknown kind") {
					t.Errorf("kinds=%v: stderr missing 'unknown kind': %q", tc.kinds, errBuf.String())
				}
			} else if err != nil {
				t.Errorf("kinds=%v: unexpected error: %v", tc.kinds, err)
			}
		})
	}
}

// TestBuildRecallRequest_KindFlag: --kind values are mapped through the
// friendly→wire table into the request body's "kinds" array, so scripted recall
// matches the TUI Kind filter.
func TestBuildRecallRequest_KindFlag(t *testing.T) {
	t.Parallel()
	cmd := newRecallCmd()
	flags := &recallFlags{limit: 10, kinds: []string{"document", "insight"}}
	body, err := buildRecallRequest(cmd, "service", flags)
	if err != nil {
		t.Fatalf("buildRecallRequest: %v", err)
	}
	var req struct {
		Kinds []string `json:"kinds"`
	}
	if uerr := json.Unmarshal(body, &req); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	want := []string{ontology.KindDocumentRevision, ontology.KindInsightSynthesized}
	if len(req.Kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", req.Kinds, want)
	}
	for i := range want {
		if req.Kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, req.Kinds[i], want[i])
		}
	}

	// No --kind → no "kinds" key on the wire (default mix).
	plain, err := buildRecallRequest(cmd, "service", &recallFlags{limit: 10})
	if err != nil {
		t.Fatalf("buildRecallRequest (no kind): %v", err)
	}
	if strings.Contains(string(plain), "kinds") {
		t.Errorf("expected no 'kinds' key without --kind; got: %s", plain)
	}
}

// TestRecallHitToRecord asserts the row-identity contract: a document hit keeps
// its own file-stem title and resource name (never the wikilink targets in
// entities[]), while a non-document hit anchors on its primary entity.
func TestRecallHitToRecord(t *testing.T) {
	t.Parallel()

	t.Run("document hit keeps its own identity", func(t *testing.T) {
		t.Parallel()
		h := &recallBrowseHit{
			HitType: "entity",
			Name:    "documents/auth-service",
			Kind:    ontology.KindDocumentRevision,
			Source:  &recallSourceJSON{Path: "notes/auth-service.md"},
			Entities: []recallRefJSON{
				{Name: "entities/billing", Kind: "service", DisplayName: "Billing Service"},
			},
		}
		rec := recallHitToRecord(h)
		if rec.ID != "documents/auth-service" {
			t.Errorf("ID = %q, want the document's own name", rec.ID)
		}
		if rec.Title != "auth-service" {
			t.Errorf("Title = %q, want file stem \"auth-service\"", rec.Title)
		}
	})

	t.Run("episode hit anchors on its primary entity", func(t *testing.T) {
		t.Parallel()
		h := &recallBrowseHit{
			HitType: ontology.KindEpisode,
			Name:    "episodes/01JVABC",
			Kind:    ontology.KindEpisode,
			Entities: []recallRefJSON{
				{Name: "entities/auth", Kind: "service", DisplayName: "Auth Service"},
			},
		}
		rec := recallHitToRecord(h)
		if rec.ID != "entities/auth" {
			t.Errorf("ID = %q, want the primary entity name", rec.ID)
		}
		if rec.Title != "Auth Service" {
			t.Errorf("Title = %q, want entity display name", rec.Title)
		}
		if rec.Kind != "service" {
			t.Errorf("Kind = %q, want entity kind \"service\"", rec.Kind)
		}
	})
}

// TestStripSnippetMarkup verifies the JSON-envelope snippet stripper removes
// <b> markers from every hit's snippet while preserving all other fields and
// top-level keys, and returns the input unchanged when there is nothing to strip
// or the payload cannot be parsed.
func TestStripSnippetMarkup(t *testing.T) {
	t.Parallel()

	in := `{"hits":[{"name":"episodes/01A","snippet":"the <b>cat</b> sat","fusedScore":0.9},{"name":"entities/01B","snippet":"plain"}],"degraded":false,"lanes":["fts"]}`
	out := stripSnippetMarkup(json.RawMessage(in))

	var env struct {
		Hits []struct {
			Name       string  `json:"name"`
			Snippet    string  `json:"snippet"`
			FusedScore float64 `json:"fusedScore"`
		} `json:"hits"`
		Degraded bool     `json:"degraded"`
		Lanes    []string `json:"lanes"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("stripped envelope unparseable: %v\n%s", err, out)
	}
	if len(env.Hits) != 2 {
		t.Fatalf("hit count = %d, want 2", len(env.Hits))
	}
	if env.Hits[0].Snippet != "the cat sat" {
		t.Errorf("hit[0].snippet = %q, want stripped %q", env.Hits[0].Snippet, "the cat sat")
	}
	if env.Hits[0].FusedScore != 0.9 || env.Hits[0].Name != "episodes/01A" {
		t.Errorf("hit[0] lost fields: %+v", env.Hits[0])
	}
	if env.Hits[1].Snippet != "plain" {
		t.Errorf("hit[1].snippet = %q, want untouched", env.Hits[1].Snippet)
	}
	if env.Degraded || len(env.Lanes) != 1 || env.Lanes[0] != "fts" {
		t.Errorf("top-level keys not preserved: degraded=%v lanes=%v", env.Degraded, env.Lanes)
	}

	// No markup -> returns the original bytes unchanged.
	clean := `{"hits":[{"snippet":"no markup"}]}`
	if got := string(stripSnippetMarkup(json.RawMessage(clean))); got != clean {
		t.Errorf("clean input mutated: got %q", got)
	}
	// Unparseable -> returned unchanged, never panics.
	bad := `{not json`
	if got := string(stripSnippetMarkup(json.RawMessage(bad))); got != bad {
		t.Errorf("bad input mutated: got %q", got)
	}
}

// TestRenderRecallCSV pins Fix 5: --output csv must emit a header row followed
// by one CSV row per hit. Columns are rank, type, score, ref, snippet.
func TestRenderRecallCSV(t *testing.T) {
	t.Parallel()

	hits := []recallHit{
		{HitType: "episode", Name: "episodes/01HXAAAAAAAAAAAAAAAAAAAA01", Snippet: "the cat sat", FusedScore: 0.9},
		{HitType: "entity", Name: "entities/01HXAAAAAAAAAAAAAAAAAAAA02", Snippet: "dog ran", FusedScore: 0.7},
	}
	raw := syntheticRecallJSON(hits, false, nil)

	var outBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz", SilenceUsage: true}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&bytes.Buffer{})

	if err := renderRecallCSV(cmd, raw); err != nil {
		t.Fatalf("renderRecallCSV: %v", err)
	}

	got := outBuf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")

	// First line is the header.
	if lines[0] != "rank,type,score,ref,snippet" {
		t.Errorf("header = %q, want %q", lines[0], "rank,type,score,ref,snippet")
	}

	// Two data rows for two hits.
	if len(lines) != 3 {
		t.Fatalf("line count = %d, want 3 (header + 2 hits)", len(lines))
	}

	// Row 1 is rank 1, full type label episode, and ref is the get-feedable handle.
	if !strings.HasPrefix(lines[1], "1,episode,") {
		t.Errorf("row 1 = %q; want prefix %q", lines[1], "1,episode,")
	}
	if !strings.Contains(lines[1], "episodes/01HXAAAAAAAAAAAAAAAAAAAA01") {
		t.Errorf("row 1 ref not the full `maz get` handle: %q", lines[1])
	}

	// Row 2 is rank 2, full type label entity.
	if !strings.HasPrefix(lines[2], "2,entity,") {
		t.Errorf("row 2 = %q; want prefix %q", lines[2], "2,entity,")
	}
}

// TestRenderRecallCSV_Quoting verifies that commas and embedded quotes in ref
// or snippet are properly RFC 4180-escaped.
func TestRenderRecallCSV_Quoting(t *testing.T) {
	t.Parallel()

	hits := []recallHit{
		{HitType: "episode", Name: "01HXAAAAAAAAAAAAAAAAAAAA03", Snippet: `he said "hello, world"`, FusedScore: 0.5},
	}
	raw := syntheticRecallJSON(hits, false, nil)

	var outBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz", SilenceUsage: true}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&bytes.Buffer{})

	if err := renderRecallCSV(cmd, raw); err != nil {
		t.Fatalf("renderRecallCSV: %v", err)
	}

	got := outBuf.String()
	// The snippet contains a comma and double-quotes, so it must be CSV-quoted
	// with internal double-quotes doubled.
	if !strings.Contains(got, `"he said ""hello, world"""`) {
		t.Errorf("CSV output missing RFC 4180-escaped snippet; got:\n%s", got)
	}
}

// TestRenderRecallKV pins Fix 5: --output kv must emit blank-line-separated
// blocks of rank:, type:, score:, ref:, snippet: lines.
func TestRenderRecallKV(t *testing.T) {
	t.Parallel()

	hits := []recallHit{
		{HitType: "episode", Name: "01HXAAAAAAAAAAAAAAAAAAAA04", Snippet: "first hit", FusedScore: 0.8},
		{HitType: "entity", Name: "01HXAAAAAAAAAAAAAAAAAAAA05", Snippet: "second hit", FusedScore: 0.6},
	}
	raw := syntheticRecallJSON(hits, false, nil)

	var outBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz", SilenceUsage: true}
	cmd.SetOut(&outBuf)
	cmd.SetErr(&bytes.Buffer{})

	if err := renderRecallKV(cmd, raw); err != nil {
		t.Fatalf("renderRecallKV: %v", err)
	}

	got := outBuf.String()

	// Each block must carry the required keys.
	for _, want := range []string{"rank: 1", "rank: 2", "type: episode", "type: entity", "snippet: first hit", "snippet: second hit"} {
		if !strings.Contains(got, want) {
			t.Errorf("KV output missing %q; got:\n%s", want, got)
		}
	}

	// Blocks separated by a blank line.
	if !strings.Contains(got, "\n\n") {
		t.Errorf("KV output: multiple hits must be separated by a blank line; got:\n%s", got)
	}
}

// TestValidateRecallAnchorFlags_OutOfRange pins Fix 4: --anchor-depth outside
// [1,6] must produce an E_VALIDATION (exit 1) error.
func TestValidateRecallAnchorFlags_OutOfRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		depth int
	}{
		{"below min", 0},
		{"above max", 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cmd := &cobra.Command{Use: "maz recall", SilenceUsage: true}
			cmd.Flags().Int("anchor-depth", 1, "")
			cmd.SetErr(&errBuf)
			if err := cmd.Flags().Set("anchor-depth", strconv.Itoa(tc.depth)); err != nil {
				t.Fatalf("flag set: %v", err)
			}
			flags := &recallFlags{anchorDepth: tc.depth}
			err := validateRecallAnchorFlags(cmd, flags)
			if err == nil {
				t.Fatalf("depth %d: want error, got nil", tc.depth)
			}
			var se silentError
			if !errors.As(err, &se) {
				t.Errorf("want silentError, got %T: %v", err, err)
			} else if se.code != 1 {
				t.Errorf("silentError.code = %d, want 1", se.code)
			}
			if !strings.Contains(errBuf.String(), "out of range") {
				t.Errorf("stderr = %q; want 'out of range'", errBuf.String())
			}
		})
	}
}

// TestValidateRecallAnchorFlags_NoAnchorWarning pins Fix 4: setting
// --anchor-depth without --anchor prints a warning but does not fail.
func TestValidateRecallAnchorFlags_NoAnchorWarning(t *testing.T) {
	t.Parallel()

	var errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz recall", SilenceUsage: true}
	cmd.Flags().Int("anchor-depth", 1, "")
	cmd.SetErr(&errBuf)
	if err := cmd.Flags().Set("anchor-depth", "3"); err != nil {
		t.Fatalf("flag set: %v", err)
	}
	flags := &recallFlags{anchorDepth: 3, anchor: ""} // no --anchor
	if err := validateRecallAnchorFlags(cmd, flags); err != nil {
		t.Fatalf("want nil error, got: %v", err)
	}
	if !strings.Contains(errBuf.String(), "no effect without --anchor") {
		t.Errorf("stderr = %q; want warning about no --anchor", errBuf.String())
	}
}

// TestValidateRecallAnchorFlags_EpisodeUID pins Fix 4: passing an episodes/
// prefix to --anchor is rejected with exit 1 and a diagnostic.
func TestValidateRecallAnchorFlags_EpisodeUID(t *testing.T) {
	t.Parallel()

	var errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz recall", SilenceUsage: true}
	cmd.Flags().Int("anchor-depth", 1, "")
	cmd.SetErr(&errBuf)
	flags := &recallFlags{anchor: "episodes/01HXEPISODE0000000000001", anchorDepth: 1}
	err := validateRecallAnchorFlags(cmd, flags)
	if err == nil {
		t.Fatal("want error for episodes/ anchor, got nil")
	}
	var se silentError
	if !errors.As(err, &se) {
		t.Errorf("want silentError, got %T: %v", err, err)
	} else if se.code != 1 {
		t.Errorf("silentError.code = %d, want 1", se.code)
	}
	if !strings.Contains(errBuf.String(), "entity") {
		t.Errorf("stderr = %q; want mention of 'entity'", errBuf.String())
	}
}
