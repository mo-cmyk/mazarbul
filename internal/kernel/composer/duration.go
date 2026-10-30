package composer

import (
	"fmt"
	"strings"
	"time"
)

// renderDurationHint produces the DURATION HINT block consumed by the
// compose-v1 prompt for IntentDuration questions. For each citation it
// emits the integer days / weeks / months / years between today's
// anchor and the citation's CreateTime, so qwen2.5:7b can echo the
// answer instead of having to derive it from raw ISO timestamps.
//
// Output shape (one line per entry):
//
//	[1] (UID, 2026-02-15T14:30:00Z): 104 days ago (~15 weeks, ~3 months)
//
// Notes:
//   - "ago" is signed: future-dated citations (clock skew) read as
//     "in N days" rather than producing a negative number that the
//     model would copy verbatim.
//   - "today" (Δ=0) collapses to a single token rather than "0 days
//     ago" so the prompt reads naturally.
//   - The block is rendered as a flat text list; no nested structure
//     because qwen2.5:7b regresses on bullet-nested numerics.
func renderDurationHint(citations []Citation, anchor time.Time) string {
	if len(citations) == 0 || anchor.IsZero() {
		return ""
	}
	anchorUTC := anchor.UTC()
	var sb strings.Builder
	for _, c := range citations {
		ct := c.CreateTime.UTC()
		if ct.IsZero() {
			continue
		}
		delta := anchorUTC.Sub(ct)
		days := int(delta.Hours() / 24.0)
		line := formatDurationLine(c.Index, c.EpisodeUID, ct, days)
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatDurationLine renders one entry of the DURATION HINT block.
// Extracted so the unit test can pin the wording without needing to
// stand up a full Citation slice.
func formatDurationLine(idx int, uid string, ct time.Time, days int) string {
	tsStr := ct.Format(time.RFC3339)
	switch {
	case days == 0:
		return fmt.Sprintf("[%d] (%s, %s): today", idx, uid, tsStr)
	case days > 0:
		weeks := days / 7
		months := days / 30
		return fmt.Sprintf("[%d] (%s, %s): %d days ago (~%d weeks, ~%d months)",
			idx, uid, tsStr, days, weeks, months)
	default:
		ahead := -days
		weeks := ahead / 7
		return fmt.Sprintf("[%d] (%s, %s): in %d days (~%d weeks)",
			idx, uid, tsStr, ahead, weeks)
	}
}
