package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// nowFn is a test seam over time.Now(). Tests inject a deterministic clock so
// placeholder substitution in templates is fully predictable without relying on
// wall time. Production always uses the real UTC clock.
//
//nolint:gochecknoglobals // test seam: tests stub this to inject a deterministic clock
var nowFn = func() time.Time { return time.Now().UTC() }

// resolveTemplatePath maps a --template argument to a vault-relative document
// path. A value containing "/" is treated as a literal vault-relative path (so
// --template templates/daily.md or --template project/template.md both work).
// A value without "/" is treated as a template name under the conventional
// "templates/" prefix (--template daily -> "templates/daily.md"). The .md
// extension is appended when absent.
func resolveTemplatePath(name string) string {
	// A value without "/" is a bare template name under the conventional
	// templates/ prefix; a value with "/" is a literal vault-relative path.
	p := name
	if !strings.Contains(name, "/") {
		p = "templates/" + name
	}
	// The .md extension is appended when absent for BOTH forms, so
	// --template project/weekly resolves to project/weekly.md.
	if !strings.HasSuffix(p, ".md") && !strings.HasSuffix(p, ".markdown") {
		p += ".md"
	}
	return p
}

// defaultJournalPath returns the default document path for a daily note:
// "journal/YYYY-MM-DD.md" using the injected clock. This is the ergonomic
// default when --template is set but no positional <path> is given.
func defaultJournalPath(now time.Time) string {
	return "journal/" + now.UTC().Format("2006-01-02") + ".md"
}

// applyTemplatePlaceholders replaces the standard set of placeholders in the
// template body. Substitution is performed on the full verbatim body
// (frontmatter + markdown), so a template can embed {{date}} in its title key.
//
// Supported placeholders:
//
//	{{date}}   YYYY-MM-DD (UTC)
//	{{time}}   HH:MM (UTC)
//	{{title}}  the document title resolved from --title, the template's own
//	           frontmatter title, or the destination path basename
func applyTemplatePlaceholders(body, title string, now time.Time) string {
	r := strings.NewReplacer(
		"{{date}}", now.UTC().Format("2006-01-02"),
		"{{time}}", now.UTC().Format("15:04"),
		"{{title}}", title,
	)
	return r.Replace(body)
}

// readTemplateBody fetches a template document's verbatim body via the
// readDocument MCP tool. The path must be the vault-relative path of the
// template (e.g. "templates/daily.md"). Returns a descriptive error when no
// such document exists, so callers can give a clear error message.
func readTemplateBody(cmd *cobra.Command, templatePath string) (string, error) {
	reqBody, err := json.Marshal(map[string]any{"path": templatePath})
	if err != nil {
		return "", fmt.Errorf("template: marshal request: %w", err)
	}
	raw, err := invokeNavTool(cmd, "readDocument", reqBody)
	if err != nil {
		return "", fmt.Errorf("template %q: %w", templatePath, err)
	}
	if raw == nil {
		return "", fmt.Errorf("template %q: readDocument not available", templatePath)
	}
	var resp struct {
		Document struct {
			Body string `json:"body"`
		} `json:"document"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("template %q: parse readDocument response: %w", templatePath, err)
	}
	if resp.Document.Body == "" {
		return "", fmt.Errorf("template %q: document exists but has no body", templatePath)
	}
	return resp.Document.Body, nil
}

// templateFrontmatter holds the frontmatter values extracted from a template
// document body. Only the fields that `maz doc new` can synthesize are decoded.
// Explicit CLI flags always override these defaults.
type templateFrontmatter struct {
	Title  string
	Pinned bool
	Tags   []string
}

// parseTemplateFrontmatter extracts synthesizable frontmatter from a template
// body. Returns zero value (no defaults) when the body has no frontmatter block
// or the block is unparseable -- the caller falls through to the normal
// empty-defaults path without an error.
func parseTemplateFrontmatter(body string) templateFrontmatter {
	if !strings.HasPrefix(body, "---\n") && !strings.HasPrefix(body, "---\r\n") {
		return templateFrontmatter{}
	}
	// Find the closing "---" delimiter.
	rest := body[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return templateFrontmatter{}
	}
	block := rest[:end]

	var raw struct {
		Title  string      `yaml:"title"`
		Pinned bool        `yaml:"pinned"`
		Tags   interface{} `yaml:"tags"`
	}
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		return templateFrontmatter{}
	}

	var tags []string
	switch v := raw.Tags.(type) {
	case []interface{}:
		for _, t := range v {
			if s, ok := t.(string); ok && s != "" {
				tags = append(tags, s)
			}
		}
	case string:
		if v != "" {
			tags = []string{v}
		}
	}
	return templateFrontmatter{Title: raw.Title, Pinned: raw.Pinned, Tags: tags}
}

// titleFromPath derives a fallback document title from a vault-relative path:
// the final path segment with any .md/.markdown extension stripped (e.g.
// "notes/project-kickoff" -> "project-kickoff", "journal/2026-06-08.md" ->
// "2026-06-08"). Used when neither --title nor the template's frontmatter
// supplies a title, so {{title}} never expands to an empty string.
func titleFromPath(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	for _, ext := range []string{".md", ".markdown"} {
		if strings.HasSuffix(p, ext) {
			return strings.TrimSuffix(p, ext)
		}
	}
	return p
}

// hasLeadingFrontmatter reports whether body opens with its own YAML frontmatter
// fence. Mirrors the writeDocument tool's bodyHasFrontmatter check so the CLI can
// avoid sending separate metadata fields that the tool would reject when the body
// already carries a frontmatter block.
func hasLeadingFrontmatter(body string) bool {
	return strings.HasPrefix(body, "---\n") || strings.HasPrefix(body, "---\r\n")
}

// splitLeadingFrontmatter splits a body that opens with a YAML frontmatter block
// into the block's inner YAML and the trailing remainder (everything from the
// closing "---" fence onward). The ok result is false when the body does not
// open with a fence or the block is unterminated.
func splitLeadingFrontmatter(body string) (block, tail string, ok bool) {
	var open int
	switch {
	case strings.HasPrefix(body, "---\n"):
		open = len("---\n")
	case strings.HasPrefix(body, "---\r\n"):
		open = len("---\r\n")
	default:
		return "", "", false
	}
	rest := body[open:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", false
	}
	return rest[:end], rest[end+len("\n---"):], true
}

// mergeTemplateFrontmatter folds explicit CLI-flag overrides (--title/--pin/--tag)
// into a template body's own frontmatter block and returns the rewritten body.
// When no override flag is set the body is returned verbatim, so an un-overridden
// template keeps its frontmatter byte-for-byte. Overriding re-serialises the
// frontmatter block (keys sorted by yaml.v3); the markdown content after the
// block is preserved exactly. This lets `doc new --template` honour the documented
// "flags override the template's frontmatter" contract even when writeDocument
// would reject separate metadata fields alongside a body that has frontmatter.
func mergeTemplateFrontmatter(body string, flags *docNewFlags) (string, error) {
	if flags.title == "" && !flags.pin && len(flags.tags) == 0 {
		return body, nil
	}
	block, tail, ok := splitLeadingFrontmatter(body)
	if !ok {
		// Caller already checked hasLeadingFrontmatter; defensively forward as-is
		// rather than corrupt a body we cannot parse.
		return body, nil
	}
	meta := map[string]any{}
	if err := yaml.Unmarshal([]byte(block), &meta); err != nil {
		return "", fmt.Errorf("template frontmatter is not valid YAML: %w", err)
	}
	if flags.title != "" {
		meta["title"] = flags.title
	}
	if flags.pin {
		meta["pinned"] = true
	}
	if len(flags.tags) > 0 {
		meta["tags"] = flags.tags
	}
	out, err := yaml.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("re-encode template frontmatter: %w", err)
	}
	return "---\n" + string(out) + "---" + tail, nil
}
