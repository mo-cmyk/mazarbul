package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
)

// makeShowCmd builds a minimal cobra tree for testing show in isolation.
func makeShowCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.PersistentFlags().String("output", "", "output format: text|json|ndjson")
	show := newShowCmd()
	root.AddCommand(show)
	if outBuf != nil {
		root.SetOut(outBuf)
		show.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		show.SetErr(errBuf)
	}
	return root
}

// TestEffectiveShowFormat_ExplicitFormatWins guards the contract that an
// explicit --format wins over the global --output. runShowHistory routes through
// this same resolver, so `show <e> --history --format json` must yield "json"
// (the prior bug switched on --output only and silently rendered text).
func TestEffectiveShowFormat_ExplicitFormatWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		format string
		want   string
	}{
		{"json", "json", diffFormatJSON},
		{"markdown maps to itself (history switch treats it as text)", "markdown", "markdown"},
		{"text", "text", "text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flags := &showFlags{}
			cmd := &cobra.Command{Use: "show"}
			cmd.Flags().StringVar(&flags.format, "format", "text", "output format")
			cmd.Flags().String("output", "", "output format")
			if err := cmd.Flags().Set("format", tc.format); err != nil {
				t.Fatalf("set --format: %v", err)
			}
			if got := effectiveShowFormat(cmd, flags); got != tc.want {
				t.Errorf("effectiveShowFormat(--format %s) = %q, want %q", tc.format, got, tc.want)
			}
		})
	}
}

// TestShowHelpContainsHistory asserts --help output is non-empty, contains the
// Examples section, and documents the --history flag.
func TestShowHelpContainsHistory(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeShowCmd(&buf, nil)
	root.SetArgs([]string{"show", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"--history",
		"Examples:",
		"maz show sarah --history",
		"--output json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestShowHistoryPartition asserts partitionHistoryRows correctly classifies
// rows into the three buckets.
func TestShowHistoryPartition(t *testing.T) {
	t.Parallel()
	rows := []showRelRow{
		{UID: "01ACTIVE", Predicate: "knows"},
		{UID: "01SUPERSEDED", Predicate: "works_at", SupersededByUID: "01NEW", ValidUntil: "2022-01-01T00:00:00Z"},
		{UID: "01EXPIRED", Predicate: "member_of", ValidUntil: "2021-01-01T00:00:00Z"},
	}
	b := partitionHistoryRows(rows)

	if len(b.active) != 1 || b.active[0].UID != "01ACTIVE" {
		t.Errorf("active bucket: want [01ACTIVE], got %v", b.active)
	}
	if len(b.superseded) != 1 || b.superseded[0].UID != "01SUPERSEDED" {
		t.Errorf("superseded bucket: want [01SUPERSEDED], got %v", b.superseded)
	}
	if len(b.expired) != 1 || b.expired[0].UID != "01EXPIRED" {
		t.Errorf("expired bucket: want [01EXPIRED], got %v", b.expired)
	}
}

// TestShowHistoryPartitionPriority asserts that SupersededByUID takes priority
// when both SupersededByUID and ValidUntil are set on the same row.
func TestShowHistoryPartitionPriority(t *testing.T) {
	t.Parallel()
	rows := []showRelRow{
		{UID: "01BOTH", Predicate: "works_at", SupersededByUID: "01NEW", ValidUntil: "2022-01-01T00:00:00Z"},
	}
	b := partitionHistoryRows(rows)
	if len(b.superseded) != 1 {
		t.Errorf("want row in superseded bucket when both fields set, got active=%d expired=%d", len(b.active), len(b.expired))
	}
}

// TestShowHistoryRenderText asserts the text renderer emits the correct section
// headers and glyphs for a three-bucket payload.
func TestShowHistoryRenderText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	clr := colorizer.New(false, &buf) // color disabled for deterministic output

	b := showHistoryBuckets{
		active: []showRelRow{
			{
				UID: "01A", Predicate: "knows", Direction: "outgoing",
				SubjectName: "Alice", ObjectName: "Bob",
			},
		},
		superseded: []showRelRow{
			{
				UID: "01S", Predicate: "works_at", Direction: "outgoing",
				SubjectName: "Alice", ObjectName: "OldCorp",
				SupersededByUID: "01NEWUID", ValidUntil: "2022-01-01T00:00:00Z",
			},
		},
		expired: []showRelRow{
			{
				UID: "01E", Predicate: "member_of", Direction: "outgoing",
				SubjectName: "Alice", ObjectName: "Club",
				ValidUntil: "2021-01-01T00:00:00Z",
			},
		},
	}

	if err := renderShowHistoryBuckets(&buf, clr, entityCard{displayName: "Alice"}, b); err != nil {
		t.Fatalf("renderShowHistoryBuckets error: %v", err)
	}

	out := buf.String()

	// Header.
	if !strings.Contains(out, "◆ Alice") {
		t.Errorf("missing ◆ entity-card header: %q", out)
	}

	// Active section.
	if !strings.Contains(out, "active") {
		t.Errorf("missing active section header: %q", out)
	}
	if !strings.Contains(out, "●") {
		t.Errorf("missing ● glyph for active row: %q", out)
	}
	if !strings.Contains(out, "knows") {
		t.Errorf("missing predicate 'knows' in active section: %q", out)
	}

	// Superseded section.
	if !strings.Contains(out, "superseded / closed") {
		t.Errorf("missing superseded / closed section header: %q", out)
	}
	if !strings.Contains(out, "✗") {
		t.Errorf("missing ✗ glyph for superseded row: %q", out)
	}

	// Expired row in the same section.
	if !strings.Contains(out, "~") {
		t.Errorf("missing ~ glyph for expired row: %q", out)
	}
}

// TestShowHistoryRenderSupersederLink asserts the superseded row renders
// a "└─→ superseded by relations/<uid>" child line with the full UID.
func TestShowHistoryRenderSupersederLink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	clr := colorizer.New(false, &buf)

	b := showHistoryBuckets{
		superseded: []showRelRow{
			{
				UID: "01S", Predicate: "works_at", Direction: "outgoing",
				SubjectName: "Alice", ObjectName: "OldCorp",
				SupersededByUID: "SUPERSEDING-UID-FULL",
			},
		},
	}

	if err := renderShowHistoryBuckets(&buf, clr, entityCard{displayName: "Alice"}, b); err != nil {
		t.Fatalf("renderShowHistoryBuckets error: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "└─→") {
		t.Errorf("missing superseder arrow glyph └─→: %q", out)
	}
	if !strings.Contains(out, "SUPERSEDING-UID-FULL") {
		t.Errorf("superseder UID must be shown in full (no abbreviation): %q", out)
	}
	if !strings.Contains(out, "relations/SUPERSEDING-UID-FULL") {
		t.Errorf("superseder must be shown as relations/<uid>: %q", out)
	}
}

// TestShowHistoryRenderNoColor asserts that disabling color produces no ANSI
// escape sequences while glyphs remain present (symbol-first principle).
// Note: t.Setenv requires sequential execution — no t.Parallel here.
func TestShowHistoryRenderNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	clr := colorizer.New(false, &buf)

	b := showHistoryBuckets{
		active: []showRelRow{
			{
				UID: "01A", Predicate: "knows", Direction: "outgoing",
				SubjectName: "Alice", ObjectName: "Bob",
			},
		},
		superseded: []showRelRow{
			{
				UID: "01S", Predicate: "works_at", Direction: "outgoing",
				SubjectName: "Alice", ObjectName: "OldCorp",
				SupersededByUID: "01NEW",
			},
		},
	}

	if err := renderShowHistoryBuckets(&buf, clr, entityCard{displayName: "Alice"}, b); err != nil {
		t.Fatalf("renderShowHistoryBuckets error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR output must not contain ANSI escapes: %q", out)
	}
	// Glyphs must still appear — colour is the accelerator, not the signal.
	for _, glyph := range []string{"●", "✗", "└─→"} {
		if !strings.Contains(out, glyph) {
			t.Errorf("glyph %q must appear even in NO_COLOR output: %q", glyph, out)
		}
	}
}

// TestShowHistoryRenderEmpty asserts that an entity with no relations at all
// renders the "no relations" message rather than empty sections.
func TestShowHistoryRenderEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	clr := colorizer.New(false, &buf)

	if err := renderShowHistoryBuckets(&buf, clr, entityCard{displayName: "Alice"}, showHistoryBuckets{}); err != nil {
		t.Fatalf("renderShowHistoryBuckets error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "no relations") {
		t.Errorf("empty buckets must render 'no relations', got: %q", out)
	}
}

// TestShowHistoryRelWindow asserts showRelWindow formats the validity window
// correctly for all three cases (both / from-only / until-only).
func TestShowHistoryRelWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		row  showRelRow
		want string
	}{
		{
			name: "from and until",
			row:  showRelRow{ValidFrom: "2020-01-01T00:00:00Z", ValidUntil: "2022-06-15T00:00:00Z"},
			want: "[2020-01-01 – 2022-06-15]",
		},
		{
			name: "from only",
			row:  showRelRow{ValidFrom: "2020-01-01T00:00:00Z"},
			want: "[since 2020-01-01]",
		},
		{
			name: "until only",
			row:  showRelRow{ValidUntil: "2022-06-15T00:00:00Z"},
			want: "[until 2022-06-15]",
		},
		{
			name: "neither",
			row:  showRelRow{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			got := showRelWindow(&row)
			if got != tc.want {
				t.Errorf("showRelWindow(%+v) = %q, want %q", tc.row, got, tc.want)
			}
		})
	}
}

// TestShowCandidateLines asserts showCandidateLines formats candidates as
// newline-separated bullet points suitable for an error message.
func TestShowCandidateLines(t *testing.T) {
	t.Parallel()
	cands := []string{
		"Sarah Connor  entities/01A",
		"Sarah Miles  entities/01B",
	}
	got := showCandidateLines(cands)

	if !strings.Contains(got, "\n  • Sarah Connor") {
		t.Errorf("expected bullet-point for first candidate, got: %q", got)
	}
	if !strings.Contains(got, "\n  • Sarah Miles") {
		t.Errorf("expected bullet-point for second candidate, got: %q", got)
	}
}

// TestShowCandidateLinesEmpty asserts showCandidateLines returns an empty
// string for an empty candidate slice.
func TestShowCandidateLinesEmpty(t *testing.T) {
	t.Parallel()
	if got := showCandidateLines(nil); got != "" {
		t.Errorf("expected empty string for nil candidates, got: %q", got)
	}
}

// TestExtractEntityCard asserts extractEntityCard parses the get-tool response
// envelope correctly, including the kind / resolution / confidence card fields.
func TestExtractEntityCard(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"entity","entity":{"uid":"01HZAB","displayName":"Alice","kind":"person","resolutionStatus":"resolved","confidence":0.91}}`)
	card, err := extractEntityCard(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if card.uid != "01HZAB" {
		t.Errorf("uid: want 01HZAB, got %q", card.uid)
	}
	if card.displayName != "Alice" {
		t.Errorf("displayName: want Alice, got %q", card.displayName)
	}
	if card.kind != "person" {
		t.Errorf("kind: want person, got %q", card.kind)
	}
	if card.resolutionStatus != "resolved" {
		t.Errorf("resolutionStatus: want resolved, got %q", card.resolutionStatus)
	}
	if card.confidence != 0.91 {
		t.Errorf("confidence: want 0.91, got %v", card.confidence)
	}
}

// TestExtractEntityCardMissingUID asserts that a missing uid field returns an
// error rather than an empty card.
func TestExtractEntityCardMissingUID(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"entity","entity":{"displayName":"Alice"}}`)
	_, err := extractEntityCard(raw)
	if err == nil {
		t.Fatal("expected error for missing uid, got nil")
	}
}

// TestShowDatePart asserts that showDatePart returns only the YYYY-MM-DD
// prefix of a full RFC3339 timestamp.
func TestShowDatePart(t *testing.T) {
	t.Parallel()
	got := showDatePart("2024-03-15T14:30:00Z")
	if got != "2024-03-15" {
		t.Errorf("showDatePart: want 2024-03-15, got %q", got)
	}
	// Short values pass through unchanged.
	if showDatePart("2024") != "2024" {
		t.Error("showDatePart: short value should pass through unchanged")
	}
}
