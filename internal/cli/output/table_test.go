package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
)

func TestTableRendersAlignedHeaderAndRows(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatText, &out, &errb)
	c := colorizer.New(false, &out)
	tbl := NewTable(w, c, []Column{
		{Title: "NAME", Align: AlignLeft},
		{Title: "COUNT", Align: AlignRight},
	})
	tbl.Row("Anke Brandt", 118)
	tbl.Row("claude-code", 24)
	if err := tbl.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "NAME") || !strings.Contains(got, "COUNT") {
		t.Errorf("missing header: %q", got)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines: %q", len(lines), got)
	}
	// Right-aligned numbers: "118" and "24" end-justified to the same width,
	// so "24" is preceded by a space within its column.
	if !strings.Contains(lines[2], " 24") {
		t.Errorf("expected right-aligned count, got %q", lines[2])
	}
}

func TestTableTruncatesLongCellWithOverflowLine(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatText, &out, &errb)
	c := colorizer.New(false, &out)
	tbl := NewTable(w, c, []Column{
		{Title: "SUBJECT", Align: AlignLeft, MaxWidth: 10, Truncate: true},
	})
	tbl.Row("Quarterly planning doc 2026 Q3")
	if err := tbl.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation ellipsis, got %q", got)
	}
	if !strings.Contains(got, "└ Quarterly planning doc 2026 Q3") {
		t.Errorf("expected hanging full-value line, got %q", got)
	}
}

func TestTableIsNoOpInJSONMode(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	w := New(FormatJSON, &out, &errb)
	c := colorizer.New(false, &out)
	tbl := NewTable(w, c, []Column{{Title: "NAME"}})
	tbl.Row("ignored")
	if err := tbl.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("table must not write in JSON mode, got %q", out.String())
	}
}
