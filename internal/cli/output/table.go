package output

import (
	"fmt"
	"strings"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
)

// Align selects horizontal cell alignment within a column.
type Align int

const (
	// AlignLeft left-justifies the cell (the default).
	AlignLeft Align = iota
	// AlignRight right-justifies the cell (numbers, counts).
	AlignRight
)

// Column describes one table column. The model is explicit (not derived by
// struct reflection) because the CLI's tabular sites are too heterogeneous to
// share a single record shape — a person-list column and a relation-direction
// column want different widths, alignment, and truncation rules.
type Column struct {
	// Title is the header label (rendered Accent in colour mode).
	Title string
	// Align is the horizontal alignment of cells in this column.
	Align Align
	// MaxWidth caps the rendered cell width in runes (0 = unbounded).
	MaxWidth int
	// Truncate, when set with MaxWidth > 0, shortens an over-long cell to
	// MaxWidth with a dim ellipsis and emits the full value on a hanging
	// "└ …" line beneath the row, so nothing is silently lost.
	Truncate bool
}

// Table accumulates rows and renders one themed, aligned, truncation-honest
// table in TEXT mode. It is deliberately text-only: JSON/NDJSON callers should
// guard with w.Format() != output.FormatText and Emit their structured records
// directly (the wire shape stays rich and typed). Render is a no-op in non-text
// mode so a caller that forgets the guard never double-emits a malformed table.
type Table struct {
	w        Writer
	c        *colorizer.Colorizer
	cols     []Column
	rows     [][]string
	overflow [][]string // per-row hanging "└ full value" notes (nil when none)
}

// NewTable starts a table bound to w, coloured through c, with the given
// columns. Pass the same colorizer the command built from interactive.ShouldStyle
// (disabled in JSON mode) so NO_COLOR / profile handling stays automatic.
func NewTable(w Writer, c *colorizer.Colorizer, cols []Column) *Table {
	return &Table{w: w, c: c, cols: cols}
}

// Row appends a row. Each cell is stringified via fmt; a cell may already carry
// ANSI (e.g. a caller-styled status glyph) — width is measured ANSI-aware.
// Excess cells beyond the column count are ignored; missing cells render empty.
func (t *Table) Row(cells ...any) {
	row := make([]string, len(t.cols))
	notes := make([]string, 0, 1)
	for i := range t.cols {
		if i >= len(cells) {
			continue
		}
		s := strings.ReplaceAll(fmt.Sprint(cells[i]), "\n", " ")
		col := t.cols[i]
		if col.Truncate && col.MaxWidth > 0 && lipgloss.Width(s) > col.MaxWidth {
			notes = append(notes, "└ "+s)
			s = truncateVisible(s, col.MaxWidth, t.c)
		}
		row[i] = s
	}
	t.rows = append(t.rows, row)
	if len(notes) > 0 {
		t.overflow = append(t.overflow, notes)
	} else {
		t.overflow = append(t.overflow, nil)
	}
}

// Render writes the assembled table to the writer. It is a no-op (returning nil)
// when the active format is not text, so JSON/NDJSON callers can call it
// unconditionally after emitting their records without corrupting the stream.
func (t *Table) Render() error {
	if t.w.Format() != FormatText {
		return nil
	}
	widths := t.columnWidths()
	var b strings.Builder

	// Header row, dim-ruled separators between commands are the caller's job.
	for i, col := range t.cols {
		cell := pad(col.Title, widths[i], col.Align)
		b.WriteString(t.c.Accent(cell))
		if i < len(t.cols)-1 {
			b.WriteString("  ")
		}
	}
	b.WriteByte('\n')

	for r, row := range t.rows {
		for i, col := range t.cols {
			b.WriteString(pad(row[i], widths[i], col.Align))
			if i < len(t.cols)-1 {
				b.WriteString("  ")
			}
		}
		b.WriteByte('\n')
		for _, note := range t.overflow[r] {
			b.WriteString("    ")
			b.WriteString(t.c.Dim(note))
			b.WriteByte('\n')
		}
	}

	// In text mode Emit routes a []byte straight to stdout via Write (no extra
	// newline added — the buffer already terminates each line).
	return t.w.Emit([]byte(b.String()))
}

// columnWidths computes the rendered width of each column as the max visible
// width of its header and cells, capped at MaxWidth when set.
func (t *Table) columnWidths() []int {
	widths := make([]int, len(t.cols))
	for i, col := range t.cols {
		w := lipgloss.Width(col.Title)
		if col.MaxWidth > 0 && w > col.MaxWidth {
			w = col.MaxWidth
		}
		widths[i] = w
	}
	for _, row := range t.rows {
		for i := range t.cols {
			cw := lipgloss.Width(row[i])
			if t.cols[i].MaxWidth > 0 && cw > t.cols[i].MaxWidth {
				cw = t.cols[i].MaxWidth
			}
			if cw > widths[i] {
				widths[i] = cw
			}
		}
	}
	return widths
}

// pad pads s to width visible columns with spaces on the alignment side.
func pad(s string, width int, align Align) string {
	gap := width - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	fill := strings.Repeat(" ", gap)
	if align == AlignRight {
		return fill + s
	}
	return s + fill
}

// truncateVisible shortens s to at most width display columns, appending a dim
// ellipsis. It assumes Truncate columns carry plain (un-styled) free text —
// status-glyph cells belong in non-truncating columns.
func truncateVisible(s string, width int, c *colorizer.Colorizer) string {
	// Guard on display width, not rune count: a wide CJK/emoji glyph occupies
	// two columns, so len(runes) can be <= width while the rendered string
	// overflows. (The caller already gates on lipgloss.Width, so this keeps the
	// two in agreement instead of silently returning the untruncated string.)
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 1 {
		return c.Dim("…")
	}
	// Trim runes until the visible width leaves one column for the ellipsis.
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width-1 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + c.Dim("…")
}
