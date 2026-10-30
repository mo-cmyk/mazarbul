// Package document turns markdown vault files into event-sourced documents.
//
// It is the interop + ingest layer for the markdown-vault feature: Parse turns
// raw file bytes into a Parsed view (verbatim body, frontmatter, wikilinks,
// title, tags, content hash); Importer turns a Parsed document into the
// entity + documentRevision + relation events that make it a first-class node
// in the same knowledge graph as structured memory.
//
// Design axioms (plan §A, §E and ontology/document.go):
//
//   - Body is the ENTIRE verbatim file (frontmatter included). ContentHash is
//     sha256(Body), so the vault projector (P2) reproduces a byte-identical
//     file with a plain write and the I1 content-hash round-trip holds.
//     FrontmatterRaw / Frontmatter are derived views (a prefix of Body), never
//     used to reconstruct the file.
//   - Parsing is deterministic and dependency-free: no LLM, no embedder. The
//     same bytes always produce the same Parsed, so re-import is a no-op.
package document

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Parsed is the deterministic result of parsing one markdown file's bytes.
// Body and ContentHash are always populated (parsing them cannot fail);
// frontmatter decoding and wikilink scanning are the only steps that can
// error, and Parse still returns a usable Parsed (Body + FrontmatterRaw +
// whatever links were found) alongside the error so a forgiving importer can
// ingest the body and skip the metadata.
type Parsed struct {
	// Body is the entire verbatim file content, frontmatter included. The
	// vault projector writes this byte-for-byte; ContentHash is its digest.
	Body string

	// Title is the document title: frontmatter `title` wins, else the first
	// top-level `# ` heading in the body, else empty (the importer falls back
	// to a humanised filename).
	Title string

	// Frontmatter is the YAML frontmatter decoded to a JSON object, or nil
	// when the file has no frontmatter block (or it failed to decode). The
	// `tags` key is normalised to a JSON array of strings (see frontmatterTags)
	// so storage-side queries over $.tags never see the string form; the
	// verbatim author bytes live in FrontmatterRaw.
	Frontmatter json.RawMessage

	// FrontmatterRaw is the exact frontmatter bytes including the opening and
	// closing `---` delimiters, or empty when no frontmatter block is present.
	FrontmatterRaw string

	// ContentHash is the SHA-256 hex digest of Body (= the exact file bytes).
	ContentHash string

	// Wikilinks holds every [[wikilink]] found in the body, in document order,
	// excluding those inside fenced or inline code. May contain duplicate
	// targets; the importer dedupes by (resolved UID, predicate) when emitting
	// relation edges.
	Wikilinks []ontology.WikiLink

	// Observations holds every `- [category] text #tag (context)` line found in
	// the body, in document order, excluding those inside fenced code. The
	// importer projects each into a fact/relation event in P6.
	Observations []ontology.Observation

	// Pinned reflects frontmatter `pinned: true`.
	Pinned bool

	// Canonical reflects frontmatter `canonical: true`. When true the document
	// claims ownership of its display name and wins same-name resolution over
	// non-document entities (guardrail G5). Defaults to false when the key is
	// absent or not a boolean.
	Canonical bool

	// Tags is the normalised set of frontmatter `tags` values (list or
	// whitespace/comma-separated string). Order follows the source.
	Tags []string

	// FrontmatterLinks holds the typed links extracted from frontmatter keys
	// that carry entity references:
	//
	//   about: [[Rosie Cotton]]          -- scalar wikilink target; RelType="describes"
	//   about: [Rosie Cotton, [[Mithril]])  -- list of targets; RelType="describes"
	//   entity: self                    -- explicit self-entity connection; RelType="mentions"
	//
	// Each entry is an ontology.WikiLink with Target set to the raw name and
	// RelType set to the predicate that the key implies: "describes" for the
	// `about:` key (a high-confidence authorial statement that this document
	// describes its subject) and "mentions" for the `entity:` key. The special
	// target "self" maps to the seeded self-entity UID at resolution time (see
	// resolve.go).
	//
	// Entries are deduplicated by target. Parse does not resolve UIDs; that
	// happens in the importer via resolveTarget, exactly like body wikilinks.
	FrontmatterLinks []ontology.WikiLink
}

// frontmatterFence is the line that opens and closes a YAML frontmatter block.
const frontmatterFence = "---"

// typedLinkRe matches a [[...]] wikilink with an optional typed/confidence
// prefix immediately preceding it: "[relType] [[Target]]" or
// "[relType:0.8] [[Target]]" (plan §3). Group 1 is the prefix relType, group 2
// the optional confidence, group 3 the inner link text. The prefix is anchored
// to the link by allowing only spaces/tabs between, so a far-off "[decision]"
// observation marker never attaches to a later link. The inner group forbids
// '[' and ']' so adjacent links and stray brackets do not merge two matches.
var typedLinkRe = regexp.MustCompile(`(?:\[([A-Za-z_][\w-]*)(?::([0-9]*\.?[0-9]+))?\][ \t]*)?\[\[([^\[\]]+)\]\]`)

// observationRe matches a basic-memory / claude-obsidian observation line:
// "- [category] text #tag (context)". The bullet may be -, * or +; the
// bracketed category is required (it is what distinguishes an observation from
// an ordinary list item). Group 1 is the category, group 2 the remaining text.
var observationRe = regexp.MustCompile(`^[-*+][ \t]+\[([^\[\]]+)\][ \t]*(.*)$`)

// obsTagRe matches a #tag token in observation text: a '#' followed by at least
// one tag rune (word chars, '-', '/'), not preceded by a word char (so "C#" is
// not a tag but "#c-sharp" is).
var obsTagRe = regexp.MustCompile(`(^|[^\w])#([\w][\w/-]*)`)

// obsContextRe captures a trailing parenthetical "(context)" at the very end of
// an observation's text. Only the final top-level parenthetical is treated as
// context; earlier parentheticals stay part of the text.
var obsContextRe = regexp.MustCompile(`\s*\(([^()]*)\)\s*$`)

// inlineCodeRe matches inline code spans (backtick-delimited). Wikilinks inside
// inline code are examples, not links, so they are masked before scanning.
var inlineCodeRe = regexp.MustCompile("`+[^`]*`+")

// h1Re matches the first ATX level-1 heading line ("# Title"), capturing the
// trimmed title text. Setext headings and deeper levels are intentionally not
// treated as the document title.
var h1Re = regexp.MustCompile(`(?m)^#\s+(.+?)\s*$`)

// Parse turns raw file bytes into a Parsed view. Body and ContentHash are
// always set. A frontmatter block that fails to decode, or a wikilink scan
// that aborts (pathological line length), yields a non-nil error while still
// returning every field that did parse, so callers can choose to ingest the
// body and drop the metadata rather than abort an entire vault import.
func Parse(raw []byte) (Parsed, error) {
	body := string(raw)
	p := Parsed{
		Body:        body,
		ContentHash: hashHex(raw),
	}

	var errs []error
	fmRaw, fmInner, rest, hasFM := splitFrontmatter(body)
	if hasFM {
		p.FrontmatterRaw = fmRaw
		fm, err := decodeFrontmatter(fmInner)
		if err != nil {
			errs = append(errs, fmt.Errorf("decode frontmatter: %w", err))
		} else {
			p.Title = frontmatterTitle(fm)
			p.Pinned = frontmatterBool(fm, "pinned")
			p.Canonical = frontmatterBool(fm, "canonical")
			p.Tags = frontmatterTags(fm)
			if _, ok := fm["tags"]; ok {
				// Normalise the decoded view: the stored frontmatter_json must
				// always carry `tags` as a JSON array of strings so storage-side
				// json_each('$.tags') queries match documents that used the
				// string form ("tags: alpha, beta"). FrontmatterRaw keeps the
				// author's verbatim bytes for byte-stable round-trips.
				norm := p.Tags
				if norm == nil {
					norm = []string{}
				}
				fm["tags"] = norm
			}
			if obj, mErr := json.Marshal(fm); mErr == nil {
				p.Frontmatter = obj
			}
			p.FrontmatterLinks = parseFrontmatterLinks(fm)
		}
	}

	// Wikilinks are scanned over the body sans frontmatter so YAML values
	// (e.g. an aliases list) never masquerade as links.
	links, scanErr := scanWikilinks(rest)
	p.Wikilinks = links
	if scanErr != nil {
		errs = append(errs, fmt.Errorf("scan wikilinks: %w", scanErr))
	}

	// Observations are scanned over the same body-sans-frontmatter so a YAML
	// list item never masquerades as an observation.
	obs, obsErr := scanObservations(rest)
	p.Observations = obs
	if obsErr != nil {
		errs = append(errs, fmt.Errorf("scan observations: %w", obsErr))
	}

	if p.Title == "" {
		p.Title = firstH1(rest)
	}
	if err := errors.Join(errs...); err != nil {
		return p, fmt.Errorf("parse document: %w", err)
	}
	return p, nil
}

// hashHex returns the lowercase SHA-256 hex digest of b.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// splitFrontmatter detects a leading YAML frontmatter block delimited by `---`
// lines. It returns the raw block (including both delimiters and the trailing
// newline), the inner YAML text, the body with the block removed, and whether a
// block was present. Both LF and CRLF line endings are accepted: a vault
// written on Windows must not silently lose its titles, pins, and tags. A file
// that does not start with a `---` line, or whose opening delimiter has no
// matching closing delimiter, is treated as having no frontmatter (raw="",
// rest=body, ok=false) — matching Obsidian's behaviour.
func splitFrontmatter(body string) (raw, inner, rest string, ok bool) {
	// Must start with the fence on its own line ("---\n" or "---\r\n"). A file
	// that is exactly "---" or starts with "----" (a horizontal rule / longer
	// fence) is not a frontmatter opener.
	var openLen int
	switch {
	case strings.HasPrefix(body, frontmatterFence+"\n"):
		openLen = len(frontmatterFence) + 1
	case strings.HasPrefix(body, frontmatterFence+"\r\n"):
		openLen = len(frontmatterFence) + 2
	default:
		return "", "", body, false
	}

	// Scan for the closing fence on its own line.
	afterOpen := body[openLen:]
	idx, fenceLen := indexClosingFence(afterOpen)
	if idx < 0 {
		return "", "", body, false
	}
	inner = afterOpen[:idx]
	// The closing fence line is "---" (fenceLen includes a trailing \r when the
	// file uses CRLF). It may end the file or be followed by "\n"; include that
	// newline in the consumed block so rest starts at the body proper.
	consumed := fenceLen
	remainder := afterOpen[idx:]
	if len(remainder) > consumed && remainder[consumed] == '\n' {
		consumed++
	}
	rawLen := openLen + idx + consumed
	raw = body[:rawLen]
	rest = body[rawLen:]
	return raw, inner, rest, true
}

// indexClosingFence returns the byte offset within s of the start of the first
// line whose content is exactly "---" — tolerating a trailing "\r" from CRLF
// line endings — plus the length of that fence text (3, or 4 including the
// "\r"). Returns (-1, 0) when no closing fence exists.
func indexClosingFence(s string) (offset, fenceLen int) {
	offset = 0
	for {
		nl := strings.IndexByte(s[offset:], '\n')
		var line string
		if nl < 0 {
			line = s[offset:]
		} else {
			line = s[offset : offset+nl]
		}
		switch line {
		case frontmatterFence:
			return offset, len(frontmatterFence)
		case frontmatterFence + "\r":
			return offset, len(frontmatterFence) + 1
		}
		if nl < 0 {
			return -1, 0
		}
		offset += nl + 1
	}
}

// decodeFrontmatter unmarshals the inner YAML into a JSON-friendly map. Yaml.v3
// decodes mappings into map[string]interface{} when the target is interface{},
// so the result marshals straight to a JSON object.
func decodeFrontmatter(inner string) (map[string]any, error) {
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(inner), &m); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// frontmatterTitle extracts a string `title` field, trimmed. Returns "" when
// absent or not a scalar string.
func frontmatterTitle(fm map[string]any) string {
	if v, ok := fm["title"]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// frontmatterBool reads a boolean field, tolerating the string forms "true"/
// "false" some authors write unquoted-but-decoded-as-string.
func frontmatterBool(fm map[string]any, key string) bool {
	switch v := fm[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

// frontmatterTags normalises a `tags` field that may be a YAML list, a single
// string, or a comma/space-separated string into a deduplicated slice in source
// order. Empty entries are dropped.
func frontmatterTags(fm map[string]any) []string {
	v, ok := fm["tags"]
	if !ok {
		return nil
	}
	var out []string
	seen := make(map[string]struct{})
	add := func(s string) {
		s = strings.TrimPrefix(strings.TrimSpace(s), "#")
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				add(s)
			}
		}
	case string:
		for _, field := range strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == ' ' }) {
			add(field)
		}
	}
	return out
}

// firstH1 returns the text of the first ATX level-1 heading in body, or "".
func firstH1(body string) string {
	m := h1Re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// scanWikilinks finds every [[wikilink]] in body, skipping fenced code blocks
// (``` or ~~~ delimited) and inline code spans. Links are returned in document
// order with duplicates preserved. A non-nil error means the scan stopped
// early (a single line exceeded the scanner's 8 MiB token limit); the links
// found up to that point are still returned so the caller can decide whether a
// partial link graph is acceptable.
func scanWikilinks(body string) ([]ontology.WikiLink, error) {
	var links []ontology.WikiLink
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	inFence := false
	var fenceMarker string
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if marker, isFence := codeFenceMarker(trimmed); isFence {
			switch {
			case !inFence:
				inFence = true
				fenceMarker = marker
			case strings.HasPrefix(trimmed, fenceMarker):
				inFence = false
				fenceMarker = ""
			}
			continue
		}
		if inFence {
			continue
		}
		masked := inlineCodeRe.ReplaceAllStringFunc(line, maskRun)
		for _, m := range typedLinkRe.FindAllStringSubmatch(masked, -1) {
			// m[1]=prefix relType, m[2]=prefix confidence, m[3]=inner link text.
			if wl, ok := parseWikilink(m[3], m[1], m[2]); ok {
				links = append(links, wl)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return links, fmt.Errorf("scan body lines: %w", err)
	}
	return links, nil
}

// scanObservations finds every "- [category] text #tag (context)" line in body,
// skipping fenced code blocks, and returns them in document order. The fence
// state machine mirrors scanWikilinks: an observation marker inside a code
// fence is an example, not data. A non-nil error means a pathological line
// exceeded the scanner's 8 MiB token limit; observations found up to that point
// are still returned.
func scanObservations(body string) ([]ontology.Observation, error) {
	var obs []ontology.Observation
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	inFence := false
	var fenceMarker string
	for sc.Scan() {
		trimmed := strings.TrimSpace(sc.Text())
		if marker, isFence := codeFenceMarker(trimmed); isFence {
			switch {
			case !inFence:
				inFence = true
				fenceMarker = marker
			case strings.HasPrefix(trimmed, fenceMarker):
				inFence = false
				fenceMarker = ""
			}
			continue
		}
		if inFence {
			continue
		}
		if ob, ok := parseObservation(trimmed); ok {
			obs = append(obs, ob)
		}
	}
	if err := sc.Err(); err != nil {
		return obs, fmt.Errorf("scan body lines: %w", err)
	}
	return obs, nil
}

// parseObservation decomposes a single trimmed line into an Observation. It
// returns ok=false unless the line is a bullet item whose first token is a
// bracketed [category] and whose remaining text is non-empty. Tags are the
// #-prefixed tokens in the text; a trailing "(context)" parenthetical is pulled
// into Context. The category and tags stay in neither — Text keeps the prose
// (including inline #tags, which are part of the sentence) minus the trailing
// context.
func parseObservation(trimmed string) (ontology.Observation, bool) {
	m := observationRe.FindStringSubmatch(trimmed)
	if m == nil {
		return ontology.Observation{}, false
	}
	category := strings.TrimSpace(m[1])
	text := strings.TrimSpace(m[2])
	if category == "" || text == "" {
		return ontology.Observation{}, false
	}

	ob := ontology.Observation{Category: category}
	// Trailing "(context)" → Context, removed from Text.
	if cm := obsContextRe.FindStringSubmatch(text); cm != nil {
		ob.Context = strings.TrimSpace(cm[1])
		text = strings.TrimSpace(text[:len(text)-len(cm[0])])
	}
	ob.Tags = observationTags(text)
	ob.Text = text
	if ob.Text == "" {
		return ontology.Observation{}, false
	}
	return ob, true
}

// observationTags extracts the deduplicated set of #tags from observation text,
// in source order, with the leading '#' stripped.
func observationTags(text string) []string {
	matches := obsTagRe.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{})
	for _, m := range matches {
		tag := m[2]
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// codeFenceMarker reports whether a trimmed line opens or closes a code fence
// and returns the fence marker (``` or ~~~) so only a matching marker closes it.
func codeFenceMarker(trimmed string) (string, bool) {
	switch {
	case strings.HasPrefix(trimmed, "```"):
		return "```", true
	case strings.HasPrefix(trimmed, "~~~"):
		return "~~~", true
	default:
		return "", false
	}
}

// maskRun replaces a matched inline-code run with spaces of equal length so
// wikilink byte offsets in the surrounding text are preserved.
func maskRun(s string) string {
	return strings.Repeat(" ", len(s))
}

// parseWikilink decomposes a [[...]] link into a WikiLink. Inner is the text
// between the brackets; prefixRel and prefixConf are the optional typed/
// confidence prefix captured from "[relType:0.8] [[Target]]" (empty when the
// link had no prefix). Grammar of the inner text (all parts optional except
// the target):
//
//	[[ relType :: target # section | alias ]]
//
// The prefix form and the inner "::" form both set RelType; the prefix wins
// when both are present (it is the form that also carries confidence). Returns
// ok=false when the target resolves to empty after trimming.
func parseWikilink(inner, prefixRel, prefixConf string) (ontology.WikiLink, bool) {
	wl := ontology.WikiLink{Raw: "[[" + inner + "]]"}
	work := inner

	// Typed extension: "relType::target...". Split on the first "::".
	if i := strings.Index(work, "::"); i >= 0 {
		wl.RelType = strings.TrimSpace(work[:i])
		work = work[i+2:]
	}
	// Alias: "...|alias". Split on the first "|".
	if i := strings.Index(work, "|"); i >= 0 {
		wl.Alias = strings.TrimSpace(work[i+1:])
		work = work[:i]
	}
	// Section: "target#section". Split on the first "#".
	if i := strings.Index(work, "#"); i >= 0 {
		wl.Section = strings.TrimSpace(work[i+1:])
		work = work[:i]
	}
	wl.Target = strings.TrimSpace(work)
	if wl.Target == "" {
		return ontology.WikiLink{}, false
	}

	// Prefix typed/confidence form: "[relType:conf] [[Target]]". The prefix
	// relType overrides the inner "::" relType; confidence is clamped to 0..1.
	if prefixRel != "" {
		wl.RelType = prefixRel
	}
	if prefixConf != "" {
		if c, err := strconv.ParseFloat(prefixConf, 64); err == nil {
			wl.Confidence = clampConfidence(c)
		}
	}
	return wl, true
}

// parseFrontmatterLinks extracts typed entity-reference links from frontmatter
// keys that carry entity targets. Two keys are supported:
//
//   - "about" — scalar or list of wikilink/plain targets, e.g.
//     `about: [[Rosie Cotton]]` or `about: [Rosie Cotton, Mithril Corp]`.
//     Each target becomes a WikiLink with RelType="describes": a high-confidence
//     authorial statement that this document describes its subject.
//   - "entity" — the special value "self" connects the document to the
//     canonical seeded self-entity; any other non-empty string is treated as a
//     plain entity name. Each target becomes a WikiLink with RelType="mentions".
//
// Each parsed target becomes an ontology.WikiLink with Target set to the bare
// name (wikilink brackets stripped) and RelType set to the key-specific
// predicate (describes for about:, mentions for entity:). Duplicates by target
// are removed; the order follows the source.
func parseFrontmatterLinks(fm map[string]any) []ontology.WikiLink {
	seen := make(map[string]struct{})
	var out []ontology.WikiLink

	add := func(raw string, relType string) {
		wl, ok := frontmatterLinkFromRaw(raw, relType)
		if !ok {
			return
		}
		if _, dup := seen[wl.Target]; dup {
			return
		}
		seen[wl.Target] = struct{}{}
		out = append(out, wl)
	}

	// "about" key: scalar string or list of strings — produces describes edges.
	if v, ok := fm["about"]; ok {
		collectFrontmatterAbout(v, func(raw string) {
			add(raw, predicateDescribes)
		})
	}

	// "entity" key: "self" is the special value; any other non-empty string
	// is treated as a plain entity name target — produces mentions edges.
	if v, ok := fm["entity"]; ok {
		if s, ok2 := v.(string); ok2 {
			if target := strings.TrimSpace(s); target != "" {
				add(target, predicateMentions)
			}
		}
	}

	return out
}

// frontmatterLinkFromRaw normalises one raw frontmatter string value into a
// WikiLink with the given relType. It strips wikilink brackets and inline
// #section / |alias suffixes. Returns (wl, false) when the result is an empty
// target.
func frontmatterLinkFromRaw(raw string, relType string) (ontology.WikiLink, bool) {
	target := strings.TrimSpace(raw)
	if strings.HasPrefix(target, "[[") && strings.HasSuffix(target, "]]") {
		target = strings.TrimSpace(target[2 : len(target)-2])
	}
	if i := strings.Index(target, "#"); i >= 0 {
		target = strings.TrimSpace(target[:i])
	}
	if i := strings.Index(target, "|"); i >= 0 {
		target = strings.TrimSpace(target[:i])
	}
	if target == "" {
		return ontology.WikiLink{}, false
	}
	return ontology.WikiLink{Raw: raw, Target: target, RelType: relType}, true
}

// collectFrontmatterAbout fans out the value of the "about" frontmatter key
// (scalar string or list of any) into individual add() calls.
//
// YAML flow lists decode an unquoted `[[Mithril]]` entry as a nested sequence
// rather than the string "[[Mithril]]" — in yaml.v3 the outer `[ ... ]` is one
// level and each `[[Mithril]]` element is a further single-element nested
// sequence (`[]any{[]any{"Mithril"}}`). The documented
// `about: [Rosie Cotton, [[Mithril]]]` shape would silently drop the wikilink
// target without the nested-sequence handling below: any chain of
// single-element sequences that bottoms out at one string is recovered as a
// `[[string]]` wikilink. Multi-element or empty nesting is ignored.
func collectFrontmatterAbout(v any, add func(string)) {
	switch t := v.(type) {
	case string:
		add(strings.TrimSpace(t))
	case []any:
		for _, elem := range t {
			switch e := elem.(type) {
			case string:
				add(strings.TrimSpace(e))
			case []any:
				if s, ok := flowWikilinkString(e); ok {
					add("[[" + s + "]]")
				}
			}
		}
	}
}

// flowWikilinkString unwraps a chain of single-element nested sequences that
// bottoms out at one trimmed string — the shape yaml.v3 produces for an
// unquoted `[[Target]]` element inside a flow list. It returns ("", false) for
// any other shape (multi-element or non-string leaf), so a malformed nesting is
// dropped rather than guessed.
func flowWikilinkString(seq []any) (string, bool) {
	for len(seq) == 1 {
		switch leaf := seq[0].(type) {
		case string:
			return strings.TrimSpace(leaf), true
		case []any:
			seq = leaf
		default:
			return "", false
		}
	}
	return "", false
}

// clampConfidence bounds a stated confidence to the closed interval [0, 1].
func clampConfidence(c float64) float64 {
	switch {
	case c < 0:
		return 0
	case c > 1:
		return 1
	default:
		return c
	}
}
