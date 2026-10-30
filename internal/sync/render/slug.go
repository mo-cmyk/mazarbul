package render

import (
	"strings"
	"unicode"
)

const (
	slugMaxLen    = 60
	slugSuffixLen = 6
	slugUnnamed   = "unnamed"
)

// slugify builds a deterministic, URL-safe slug from displayName and the
// entity ULID. Algorithm: ASCII-lowercase the displayName, replace runs of
// non-alnum with "-", trim leading/trailing "-", truncate to slugMaxLen
// chars, then append "__<last6OfULID>". An empty displayName uses "unnamed".
// The slug is frozen at first sight of an entity; subsequent renames update
// the frontmatter but never change the file path.
func slugify(displayName, uid string) string {
	base := slugBase(displayName)
	suffix := slugSuffix(uid)
	return base + "__" + suffix
}

// slugBase converts displayName into its truncated, sanitised base.
func slugBase(displayName string) string {
	name := displayName
	if strings.TrimSpace(name) == "" {
		name = slugUnnamed
	}

	lower := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII {
			return '-'
		}
		lr := unicode.ToLower(r)
		if (lr >= 'a' && lr <= 'z') || (lr >= '0' && lr <= '9') {
			return lr
		}
		return '-'
	}, name)

	// Collapse consecutive hyphens into one.
	result := collapseHyphens(lower)
	result = strings.Trim(result, "-")
	if result == "" {
		result = slugUnnamed
	}

	// Truncate to slugMaxLen runes.
	runes := []rune(result)
	if len(runes) > slugMaxLen {
		runes = runes[:slugMaxLen]
		// Trim trailing hyphen that truncation may have exposed.
		for len(runes) > 0 && runes[len(runes)-1] == '-' {
			runes = runes[:len(runes)-1]
		}
	}
	result = string(runes)
	if result == "" {
		result = slugUnnamed
	}
	return result
}

// collapseHyphens replaces runs of hyphens with a single hyphen.
func collapseHyphens(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, ch := range s {
		if ch == '-' {
			if !prevHyphen {
				b.WriteRune(ch)
			}
			prevHyphen = true
		} else {
			b.WriteRune(ch)
			prevHyphen = false
		}
	}
	return b.String()
}

// slugSuffix returns the last 6 characters of the ULID.
// ULIDs are 26 Crockford Base32 characters; if the UID is shorter than 6
// (which only happens in tests with synthetic IDs), the full UID is used.
func slugSuffix(uid string) string {
	if len(uid) <= slugSuffixLen {
		return uid
	}
	return uid[len(uid)-slugSuffixLen:]
}
