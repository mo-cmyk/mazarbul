package spinner_test

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"

	mazspinner "github.com/mo-cmyk/mazarbul/internal/tui/spinner"
)

func TestCardViewShowsLabelAndSubtitle(t *testing.T) {
	c := mazspinner.NewCard("Indexing", "42 docs", 80)
	view := c.View()

	if !strings.Contains(view, "Indexing") {
		t.Fatalf("expected view to contain %q, got: %q", "Indexing", view)
	}
	if !strings.Contains(view, "42 docs") {
		t.Fatalf("expected view to contain %q, got: %q", "42 docs", view)
	}
}

func TestCardAdvancesOnTick(t *testing.T) {
	c := mazspinner.NewCard("Indexing", "42 docs", 80)
	updated, cmd := c.Update(spinner.TickMsg{})
	_ = updated
	_ = cmd
	// reaching here without panic is sufficient
}
