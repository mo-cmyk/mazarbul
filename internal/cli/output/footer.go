package output

import (
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
)

// ShowingNofM emits an honest "showing N of M" footer for a limited list, so a
// truncated result never reads as the whole set. It routes through Writer.Footer
// (which lands on stderr in JSON mode, keeping `| jq` clean). Pass the same
// colorizer the command built (disabled in JSON mode) so colour stays automatic:
//
//   - total known and shown >= total  → green "showing N of N" (complete)
//   - total known and shown <  total  → green "showing N of M — raise --limit …"
//   - total unknown, shown == limit    → yellow "! showing N (limit reached …)"
//   - total unknown, otherwise         → green "showing N"
//
// total <= 0 means the backing store did not return a grand total (the common
// case today); pass limit so a full page can still be flagged as possibly capped.
//
// Hint is the caller-specific pagination guidance appended to the limit-reached
// message (e.g. "raise --limit" or "use --offset 10"). Commands expose different
// flags — recall has no --offset, entities does — so the suggestion cannot be
// hardcoded here; an empty hint omits it.
func ShowingNofM(w Writer, c *colorizer.Colorizer, shown, total, limit int, hint string) {
	if c == nil {
		c = colorizer.New(false, nil)
	}
	var msg string
	switch {
	case total > 0 && shown >= total:
		msg = c.Success(fmt.Sprintf("showing %d of %d", shown, total))
	case total > 0:
		msg = c.Success(fmt.Sprintf("showing %d of %d — raise --limit or refine the query", shown, total))
	case limit > 0 && shown >= limit:
		base := fmt.Sprintf("! showing %d (limit reached; more may exist)", shown)
		if hint != "" {
			base += " — " + hint
		}
		msg = c.Warn(base)
	default:
		msg = c.Success(fmt.Sprintf("showing %d", shown))
	}
	w.Footer(msg)
}
