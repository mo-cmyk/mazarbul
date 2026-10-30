package tools

import (
	"fmt"
	"time"
)

// parseCurrentDate parses the optional "currentDate" argument shared by the
// recall and ask MCP tools. It accepts either RFC 3339 (with or without a
// fractional second) or YYYY-MM-DD. YYYY-MM-DD is treated as 00:00:00 UTC.
// Always returns a UTC time on success.
func parseCurrentDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("must be RFC 3339 UTC or YYYY-MM-DD, got %q", s)
}
