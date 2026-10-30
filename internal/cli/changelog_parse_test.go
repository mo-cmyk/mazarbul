package cli

// changelog_parse_test.go pins the contract that `maz changelog` picks
// the first SEMANTIC bullet from a CHANGELOG fragment as its summary,
// not the `Device:` metadata header. Without this, the changelog row
// printed "Device: maz-sync-cli" for every push — useless when the
// outer row already has a Device column.

import (
	"strings"
	"testing"
)

func TestPickChangelogSummary_PrefersSemanticBulletOverDeviceLine(t *testing.T) {
	body := `# Push 01KT8ZP9H7R3K3AGCMD3AYQS64

Device: maz-sync-cli

- + entity person/01KT8ZFV26XBGN139W394DZ160  "Frodo"
- + entity organization/01KT8ZFV72JV1MJK9DMD7A5JR7  "Erebor"
- + relation 01KT... works_at → 01KT...
`
	got := pickChangelogSummary(body)
	if !strings.HasPrefix(got, "- + entity") {
		t.Errorf("expected first bullet, got %q", got)
	}
}

func TestPickChangelogSummary_SkipsHeadingsAndBlanksAndSeparators(t *testing.T) {
	body := `# Heading

---

- + first bullet
- + second bullet
`
	got := pickChangelogSummary(body)
	if got != "- + first bullet" {
		t.Errorf("got %q, want %q", got, "- + first bullet")
	}
}

func TestPickChangelogSummary_EmptyBody(t *testing.T) {
	if got := pickChangelogSummary(""); got != "" {
		t.Errorf("empty body should yield empty summary, got %q", got)
	}
}

func TestPickChangelogSummary_LegacyFormatWithoutBullets(t *testing.T) {
	// Older fragments without bullet rows fall through to the first
	// non-metadata line — better than nothing.
	body := `# Heading

Some prose describing what changed in this push.
`
	got := pickChangelogSummary(body)
	if !strings.Contains(got, "Some prose") {
		t.Errorf("legacy prose summary lost: got %q", got)
	}
}
