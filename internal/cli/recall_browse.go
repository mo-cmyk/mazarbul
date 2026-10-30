package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/tui/browse"
)

// truncateRunes truncates s to at most n runes, appending "..." when truncated.
// Used by the browse loader to keep record subtitles compact.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

// recallBrowseLoader implements browse.Loader by driving the recall, get, and
// neighbors tools through invokeNavTool. It carries the cobra command (for the
// daemon/in-process tool path and its context) and the parsed recall flags so a
// browse Search reuses the same retrieval settings as the table render.
type recallBrowseLoader struct {
	cmd   *cobra.Command
	flags *recallFlags
}

// recallBrowseHit is the partial decode of a recall hit needed to build a
// browsable record: the resource name, kind, snippet, score, and the entity
// anchors used to follow the graph.
type recallBrowseHit struct {
	HitType    string            `json:"hitType"`
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Snippet    string            `json:"snippet"`
	FusedScore float32           `json:"fusedScore"`
	Entities   []recallRefJSON   `json:"entities"`
	Source     *recallSourceJSON `json:"source"`
}

// recallSourceJSON locates a vault-document hit within its source file. Its
// presence is the signal that a hit IS a document (kind "documentRevision"),
// which the row mapper uses to keep the document's own identity as the title.
type recallSourceJSON struct {
	Path string `json:"path"`
}

// recallRefJSON is the entity-anchor wire shape carried by a hit and by a
// neighbor row.
type recallRefJSON struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
}

// Search runs the recall tool for query and maps each hit to a browse.Record,
// preferring the hit's primary entity anchor (so Detail/Neighbors can follow
// the graph) and falling back to the hit's own resource name.
//
// Incoming Filters override matching recallFlags fields: Scopes→scopes,
// MinScore→minScore, At→at, Kinds→kinds. MergeFiltersIntoFlags folds all of
// them (an active strip Kind pill overriding a CLI --kind), then
// buildRecallRequest patches kinds into the JSON body exactly once — patching
// a second time here would silently drop the CLI --kind whenever the strip set
// any kind.
func (l *recallBrowseLoader) Search(query string, f browse.Filters) ([]browse.Record, error) { //nolint:gocritic // hugeParam: browse.Filters is passed by value to satisfy the browse.Loader interface; pointer signature would break the contract
	merged := mergeFiltersIntoFlags(l.flags, &f)

	reqBody, err := buildRecallRequest(l.cmd, query, &merged)
	if err != nil {
		return nil, err
	}

	raw, err := invokeNavTool(l.cmd, "recall", reqBody)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("%s", errRecallDisabledMsg)
	}
	if merged.minScore > 0 {
		if filtered, ferr := applyRecallMinScore(raw, float32(merged.minScore)); ferr == nil {
			raw = filtered
		}
	}
	var out struct {
		Hits []recallBrowseHit `json:"hits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("browse: parse recall hits: %w", err)
	}
	records := make([]browse.Record, 0, len(out.Hits))
	for i := range out.Hits {
		records = append(records, recallHitToRecord(&out.Hits[i]))
	}
	return records, nil
}

// recallHitToRecord maps one recall hit to a browse.Record. A vault-document
// hit (it carries a source.path) keeps its OWN document identity: the title is
// the document's file stem and the id stays on the document entity — the hit's
// entities[] are its wikilink TARGETS, not its identity, so anchoring on them
// would mislabel the row and open the wrong document on Detail. Every other hit
// (episode/insight) anchors on its primary entity so Detail/Neighbors can pivot
// the graph.
func recallHitToRecord(h *recallBrowseHit) browse.Record {
	id := h.Name
	title := resourceUID(h.Name, 12)
	kind := h.Kind
	if kind == "" {
		kind = h.HitType
	}
	switch {
	case h.Source != nil && h.Source.Path != "":
		if t := docTitleFromPath(h.Source.Path); t != "" {
			title = t
		}
	case len(h.Entities) > 0:
		a := h.Entities[0]
		id = a.Name
		if a.DisplayName != "" {
			title = a.DisplayName
		}
		if a.Kind != "" {
			kind = a.Kind
		}
	}
	snippet := truncateRunes(stripHighlight(strings.ReplaceAll(h.Snippet, "\n", " ")), 72)
	subtitle := fmt.Sprintf("%s · %.3f", kind, h.FusedScore)
	if snippet != "" {
		subtitle += " · " + snippet
	}
	return browse.Record{ID: id, Title: title, Subtitle: subtitle, Kind: kind}
}

// docTitleFromPath returns a friendly document title from a vault-relative path:
// the base filename without its .md extension (e.g. "notes/auth-service.md" →
// "auth-service"). An empty path yields "".
func docTitleFromPath(p string) string {
	base := p
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".md")
}

// Detail fetches the record by resource name via the get tool and renders it as
// a compact markdown document for the detail card.
func (l *recallBrowseLoader) Detail(id string) (string, error) {
	in := map[string]any{"name": id}
	if len(l.flags.scopes) > 0 {
		in["scopes"] = l.flags.scopes
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	raw, err := invokeNavTool(l.cmd, "get", reqBody)
	if err != nil {
		return "", err
	}
	return recordToMarkdown(id, raw), nil
}

// Neighbors expands an entity's one-hop neighborhood via the neighbors tool.
// Episodes have no graph neighbors, so an episodes/ id returns an empty slice
// rather than an error.
func (l *recallBrowseLoader) Neighbors(id string) ([]browse.Record, error) {
	if strings.HasPrefix(id, "episodes/") {
		return nil, nil
	}
	in := map[string]any{"seed": id, "depth": 1}
	if len(l.flags.scopes) > 0 {
		in["scopes"] = l.flags.scopes
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	raw, err := invokeNavTool(l.cmd, "neighbors", reqBody)
	if err != nil {
		return nil, err
	}
	var out struct {
		Neighbors []struct {
			Entity    recallRefJSON              `json:"entity"`
			Via       struct{ Predicate string } `json:"via"`
			Direction string                     `json:"direction"`
			Hops      int                        `json:"hops"`
		} `json:"neighbors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("browse: parse neighbors: %w", err)
	}
	records := make([]browse.Record, 0, len(out.Neighbors))
	for _, n := range out.Neighbors {
		title := n.Entity.DisplayName
		if title == "" {
			title = resourceUID(n.Entity.Name, 12)
		}
		arrow := "→"
		if n.Direction == "in" {
			arrow = "←"
		}
		records = append(records, browse.Record{
			ID:       n.Entity.Name,
			Title:    title,
			Subtitle: fmt.Sprintf("%s %s · %dh", arrow, n.Via.Predicate, n.Hops),
			Kind:     n.Entity.Kind,
		})
	}
	return records, nil
}

// recordToMarkdown renders a get-tool discriminated-union record as a compact
// markdown document: a heading from the type plus a sorted field list. A parse
// failure falls back to a fenced JSON block so the card never renders empty.
func recordToMarkdown(id string, raw json.RawMessage) string {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "```json\n" + string(raw) + "\n```"
	}
	var kind string
	if uerr := json.Unmarshal(rec["type"], &kind); uerr != nil {
		return "```json\n" + string(raw) + "\n```"
	}
	heading := kind
	if heading == "" {
		heading = id
	}
	lines := []string{"# " + heading, ""}
	if body, ok := rec[kind]; ok {
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err == nil {
			for _, k := range sortedKeys(fields) {
				lines = append(lines, fmt.Sprintf("- **%s**: %v", k, fields[k]))
			}
		}
	}
	return strings.Join(lines, "\n")
}

// mergeFiltersIntoFlags returns a shallow copy of base with non-zero Filters
// fields applied. Zero/nil Filters values leave the corresponding flag unchanged
// so existing recall behaviour is fully preserved when no strip is active.
func mergeFiltersIntoFlags(base *recallFlags, f *browse.Filters) recallFlags {
	merged := *base
	if f == nil {
		return merged
	}
	if len(f.Scopes) > 0 {
		merged.scopes = f.Scopes
	}
	if f.MinScore != 0 {
		merged.minScore = f.MinScore
	}
	if at := resolveAtAnchor(f.At); at != "" {
		merged.at = at
	}
	if len(f.Kinds) > 0 {
		merged.kinds = f.Kinds
	}
	return merged
}

// resolveAtAnchor maps a friendly Time-bank label to an RFC 3339 valid-time
// anchor for the recall "at" field. "now" (and "") mean the current instant
// and yield "" (no anchor); "7d"/"30d" resolve to that many days before now.
// An unrecognized value is treated as a literal anchor (e.g. a raw --at value).
func resolveAtAnchor(label string) string {
	switch label {
	case "", "now":
		return ""
	case "7d":
		return time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	case "30d":
		return time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	default:
		return label
	}
}

// recallKindWire maps a friendly Kind-bank label to the record-kind value the
// recall tool's "kinds" field accepts (see internal/mcp/tools/recall.go: the
// field filters on record kind — documentRevision/episode/insightSynthesized —
// not on entity kind). The friendly labels are deliberately NOT entity kinds
// (person/organization/…): the recall "kinds" field is parsed as a record kind,
// so an entity-kind label like "document" never matches a "documentRevision"
// hit and would silently return zero results. An unrecognized label passes
// through unchanged so a raw wire value still works.
func recallKindWire(label string) string {
	switch label {
	case "documents", entityKindDocument:
		return ontology.KindDocumentRevision
	case "memories", "memory", "episodes", ontology.KindEpisode:
		return ontology.KindEpisode
	case "insights", "insight":
		return ontology.KindInsightSynthesized
	case "consolidated":
		return ontology.KindMemoryConsolidated
	default:
		return label
	}
}

// validRecallKind reports whether label is a kind the recall "kinds" filter can
// match. A label is valid when recallKindWire maps it to one of the four known
// record kinds; this accepts the friendly forms (document/episode/insight/
// consolidated and their plurals) as well as the raw wire values. Unknown
// labels are rejected so the scripted --kind flag fails fast rather than
// silently matching nothing (toEventKinds casts any string, so a typo would
// otherwise return zero hits with no error).
func validRecallKind(label string) bool {
	switch recallKindWire(label) {
	case ontology.KindDocumentRevision, ontology.KindEpisode,
		ontology.KindInsightSynthesized, ontology.KindMemoryConsolidated:
		return true
	default:
		return false
	}
}

// buildRecallBanks constructs the filter-strip bank definitions for the recall
// TUI. Four banks are built, in mockup order Kind / Scope / Time / Score:
//
//   - Kind (BankToggle, Key "kinds"): recall record kinds (documents / memories /
//     insights), mapped to their wire values by recallKindWire at query time.
//   - Scope (BankToggle, Key "scopes"): personal / project / team.
//   - Time (BankSingle, Key "at"): now / 7d / 30d, resolved to an RFC 3339
//     as-of valid-time anchor by resolveAtAnchor (now = current instant = no
//     anchor; 7d/30d = that many days before now).
//   - Score (BankSlider, Key "minScore"): ordered score-floor stops.
func buildRecallBanks() []browse.Bank {
	return []browse.Bank{
		{
			// Friendly record-kind facets; recallKindWire maps each label to
			// its wire value (documents→documentRevision, etc.) at query time.
			Label:   "Kind",
			Kind:    browse.BankToggle,
			Options: []string{"documents", "memories", "insights"},
			Key:     "kinds",
		},
		{
			Label:   "Scope",
			Kind:    browse.BankToggle,
			Options: []string{"personal", "project", "team"},
			Key:     "scopes",
		},
		{
			Label:   "Time",
			Kind:    browse.BankSingle,
			Options: []string{"now", "7d", "30d"},
			Key:     "at",
		},
		{
			Label:   "Score",
			Kind:    browse.BankSlider,
			Options: []string{"0", "0.05", "0.1", "0.2"},
			Key:     "minScore",
		},
	}
}

// patchKindsIntoRequest injects a "kinds" array into the recall JSON request
// body when kinds is non-empty. The strip emits friendly Kind-bank labels
// (documents/memories/insights); each is mapped to the record-kind wire value
// the recall MCP tool's "kinds" field expects (see internal/mcp/tools/recall.go)
// via recallKindWire. A parse or marshal failure returns the original body
// unchanged.
func patchKindsIntoRequest(reqBody json.RawMessage, kinds []string) json.RawMessage {
	if len(kinds) == 0 {
		return reqBody
	}
	wire := make([]string, 0, len(kinds))
	for _, k := range kinds {
		wire = append(wire, recallKindWire(k))
	}
	var req map[string]interface{}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return reqBody
	}
	req["kinds"] = wire
	patched, err := json.Marshal(req)
	if err != nil {
		return reqBody
	}
	return patched
}
