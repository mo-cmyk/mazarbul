package render

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// frontmatterField is a key/value pair for YAML frontmatter output.
// A nil Value causes the field to be omitted entirely.
type frontmatterField struct {
	key   string
	value any
}

// renderFrontmatterValue converts a Go value to its YAML scalar/flow representation.
func renderFrontmatterValue(v any) string {
	switch val := v.(type) {
	case string:
		return renderYAMLString(val)
	case []string:
		return renderYAMLStringSlice(val)
	case time.Time:
		return val.UTC().Format(time.RFC3339Nano)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(val, 'f', 3, 64)
	case int:
		return strconv.Itoa(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// renderYAMLString emits s as a YAML scalar, double-quoting (and escaping)
// whenever a plain scalar could be misparsed. Quoting triggers on: an empty
// string; leading/trailing whitespace; a YAML indicator as the first rune
// (-?:,[]{}#&*!|>'"%@` plus a leading space); or any interior character that
// breaks a plain scalar (": ", " #", control chars, or newlines/tabs). Within
// a double-quoted scalar, backslashes and double quotes are escaped and
// newline/tab/carriage-return are rendered as YAML escape sequences so the
// value round-trips on a single line.
func renderYAMLString(s string) string {
	if !needsYAMLQuoting(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// needsYAMLQuoting reports whether s cannot be emitted as a bare plain scalar.
func needsYAMLQuoting(s string) bool {
	if s == "" {
		return true
	}
	// Leading/trailing whitespace is stripped by plain scalars.
	if s != strings.TrimSpace(s) {
		return true
	}
	// A YAML indicator (or whitespace) as the first character forces quoting.
	if strings.IndexByte("-?:,[]{}#&*!|>'\"%@` \t", s[0]) >= 0 {
		return true
	}
	for i := range len(s) {
		c := s[i]
		if c < 0x20 { // control characters, incl. newline/tab/CR
			return true
		}
		switch c {
		case ':':
			// "key:" or "a: b" ambiguity: a colon followed by space or EOL.
			if i+1 == len(s) || s[i+1] == ' ' {
				return true
			}
		case '#':
			// " #" starts a comment.
			if i > 0 && s[i-1] == ' ' {
				return true
			}
		}
	}
	return false
}

// renderYAMLStringSlice renders a string slice in YAML flow style.
func renderYAMLStringSlice(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = renderYAMLString(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// renderedDir is the POSIX path prefix for all rendered output files.
const renderedDir = "rendered"

// renderedPath joins the rendered directory with subpath components.
func renderedPath(parts ...string) string {
	all := make([]string, 0, 1+len(parts))
	all = append(all, renderedDir)
	all = append(all, parts...)
	return path.Join(all...)
}
