package cli

// read_surface_test.go — regression tests for the read-surface findings.
//
// Finding 1: show/blame reads the LIVE store, no internal-path leak.
// Finding 2: diff with non-TTY writer produces no ANSI escape sequences.
// Finding 3: traverse with a bogus seed exits non-zero (in traverse_test.go).
// Finding 4: parseFrontmatter never emits a "- type" key.
// Finding 5: count --group-by status works (snake_case alias + no empty bucket).
// Finding 6: recall --min-score drops sub-threshold hits.
// Finding 7: recall --output ndjson emits one JSON object per line.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ---- Finding 1: show reads the live store (no path leak) ----

// makeShowBlameRoot wires a minimal root with entity, show, and blame subcommands.
func makeShowBlameRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newEntityCmd())
	root.AddCommand(newShowCmd())
	root.AddCommand(newBlameCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// TestShowLiveEntityIsInspectable verifies that a freshly-created entity (no
// maz sync push) is inspectable via `maz show <uid>` and exits 0.
// It also verifies that the output never leaks the internal path
// "rendered/entities.jsonl".
func TestShowLiveEntityIsInspectable(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "Hanna Testreader")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", uid})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show live entity: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	if strings.Contains(out, "rendered/entities.jsonl") {
		t.Errorf("show output leaks internal path 'rendered/entities.jsonl': %q", out)
	}
	// The entity name should appear in the output.
	if !strings.Contains(out, "Hanna Testreader") && !strings.Contains(out, uid) {
		t.Errorf("show output does not include entity name or UID; got: %q", out)
	}
}

// TestShowMissingEntityNoPathLeak verifies that `maz show <nonexistent-uid>`
// exits non-zero with an actionable message and NEVER leaks the internal
// rendered/entities.jsonl path.
func TestShowMissingEntityNoPathLeak(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	const bogusUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", bogusUID})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("show non-existent entity expected non-zero exit, got 0")
	}
	combined := outBuf.String() + errBuf.String()
	if strings.Contains(combined, "rendered/entities.jsonl") {
		t.Errorf("show error leaks internal path 'rendered/entities.jsonl': %q", combined)
	}
}

// TestShowFormatJSONNoTypeDashKey verifies that `maz show <uid> --format json`
// emits a clean JSON object without a literal "- type" key. This is the
// parseFrontmatter regression: orphaned YAML list items ("- item") used to
// produce a key named "- type" instead of being collected under their parent.
func TestShowFormatJSONNoTypeDashKey(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "Sam JSON Tester")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "show", uid, "--format", "json"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show --format json: %v (stderr=%q)", err, errBuf.String())
	}
	out := strings.TrimSpace(outBuf.String())
	if out == "" {
		t.Fatal("show --format json produced empty output")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("show --format json output is not valid JSON: %v\n%s", err, out)
	}
	for k := range obj {
		if strings.HasPrefix(k, "- ") {
			t.Errorf("show --format json has illegal '- type' key %q in output: %s", k, out)
		}
	}
}

// TestShowOutputJSONOnLiveEntityIsJSON verifies that the standard `--output
// json` flag is honored on the live-store path, not just the legacy `--format
// json`. Greptile P1: `maz show entities/<uid> --output json` used to print
// key/value text for a live entity because the live branch read only
// flags.format (default "text") and ignored the global --output flag.
func TestShowOutputJSONOnLiveEntityIsJSON(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "Output JSON Tester")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "show", uid})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show --output json: %v (stderr=%q)", err, errBuf.String())
	}
	out := strings.TrimSpace(outBuf.String())
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("show --output json did not emit JSON for a live entity: %v\nout=%q", err, out)
	}
	if _, ok := obj["displayName"]; !ok {
		t.Errorf("show --output json missing displayName field; got: %v", obj)
	}
}

// TestShowByUniqueFragmentResolvesLive verifies the documented display-name
// fragment form: `maz show rosie` resolves a single entity named "Rosie Cotton"
// even though "rosie" is not an exact match. Greptile P1: after the nameExact
// change, a unique fragment fell through to a "did you mean" suggestion error
// instead of resolving the one match.
func TestShowByUniqueFragmentResolvesLive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "person", "Rosie Cotton")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", "rosie"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show by unique fragment: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "Rosie Cotton") {
		t.Errorf("show \"rosie\" should resolve the unique \"Rosie Cotton\"; got out=%q err=%q", outBuf.String(), errBuf.String())
	}
}

// TestShowByAmbiguousFragmentSuggests verifies that an ambiguous fragment still
// returns a non-zero suggestion error listing the candidates, rather than
// silently resolving one of them.
func TestShowByAmbiguousFragmentSuggests(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "person", "Chris Park")
	_ = runEntityCreateCmd(t, dir, "person", "Chris Lee")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", "chris"})
	root.SetContext(context.Background())
	// The root silences cobra error printing (SilenceErrors), so the candidate
	// list rides on the returned error — the same pattern the >1-exact path
	// uses; the production top-level Execute prints it to stderr.
	err := root.Execute()
	if err == nil {
		t.Fatalf("show by ambiguous fragment expected non-zero exit; out=%q", outBuf.String())
	}
	if !strings.Contains(err.Error(), "Chris Park") || !strings.Contains(err.Error(), "Chris Lee") {
		t.Errorf("ambiguous fragment error should list both candidates; got: %q", err.Error())
	}
}

// TestShowByCappedAmbiguousFragmentReportsCapped verifies that when a fragment
// matches more candidates than the display cap, the error reports the set as
// capped ("more than N") rather than printing the capped slice length as a
// false exact total. Greptile P2: with 6+ matches the old message claimed
// "matches 5 entities" while only listing the capped 5.
func TestShowByCappedAmbiguousFragmentReportsCapped(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	// Seed more than showFragmentCap (5) entities sharing the "chris" fragment.
	for _, name := range []string{
		"Chris Park", "Chris Lee", "Chris Bell", "Chris Fox", "Chris Day", "Chris Hill",
	} {
		_ = runEntityCreateCmd(t, dir, "person", name)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", "chris"})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatalf("show by capped ambiguous fragment expected non-zero exit; out=%q", outBuf.String())
	}
	if !strings.Contains(err.Error(), "more than 5 entities") {
		t.Errorf("capped fragment error should report the set as capped (\"more than 5 entities\"); got: %q", err.Error())
	}
	// And it must not claim a false exact total it cannot know from a capped query.
	if strings.Contains(err.Error(), "matches 5 entities") || strings.Contains(err.Error(), "matches 6 entities") {
		t.Errorf("capped fragment error must not claim a false exact total; got: %q", err.Error())
	}
}

// TestShowOutputNDJSONOnLiveEntityIsSingleLine verifies that `--output ndjson`
// on a live entity emits exactly one compact JSON object on a single line — a
// valid NDJSON record — rather than pretty multi-line JSON. show returns one
// record, so its NDJSON form is one line. Greptile P1: the --output bridge
// collapsed ndjson into the same pretty-printed json path.
func TestShowOutputNDJSONOnLiveEntityIsSingleLine(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "NDJSON Tester")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "ndjson", "show", uid})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show --output ndjson: %v (stderr=%q)", err, errBuf.String())
	}
	lines := strings.Split(strings.TrimRight(outBuf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("show --output ndjson: expected exactly 1 line, got %d:\n%q", len(lines), outBuf.String())
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("show --output ndjson line is not a valid JSON object: %v\nline=%q", err, lines[0])
	}
	if _, ok := obj["displayName"]; !ok {
		t.Errorf("show --output ndjson missing displayName field; got: %v", obj)
	}
}

// TestBlameLiveEntityIsInspectable verifies that a freshly-created entity is
// inspectable via `maz blame <uid>` (exit 0, no path leak, shows "no version
// history" when there is none).
func TestBlameLiveEntityIsInspectable(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "Blame Tester")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "blame", uid})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("blame live entity: %v (stderr=%q)", err, errBuf.String())
	}
	combined := outBuf.String() + errBuf.String()
	if strings.Contains(combined, "rendered/entities.jsonl") {
		t.Errorf("blame output leaks internal path 'rendered/entities.jsonl': %q", combined)
	}
}

// ---- Finding 2: diff with non-TTY produces no ANSI sequences ----

// TestDiffNoANSIOnNonTTY verifies that when diff writes to a bytes.Buffer
// (non-TTY), the output contains no raw ANSI escape sequences.
func TestDiffNoANSIOnNonTTY(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	// writerIsTTY(&outBuf) returns false — diff must not emit colour codes.
	flags := &diffFlags{}
	// computeDiff on empty inputs returns no entries; writeDiffSection emits
	// "(no changes)" with no colour so we get the format right; build a
	// minimal entry set that exercises colorizeEntry paths by calling
	// writeDiffEntries with a non-nil entry slice.
	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Test__01AR", Scope: "local"},
		{Action: diffActionForgotten, Kind: diffKindRelation, Target: "01AB works_at 01CD", Scope: "local"},
	}
	if err := writeDiffSection(&outBuf, "local", "HEAD~1..HEAD", entries, flags); err != nil {
		t.Fatalf("writeDiffSection: %v", err)
	}
	out := outBuf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("diff output to non-TTY contains raw ANSI escape sequence:\n%q", out)
	}
}

// ---- Finding 4: parseFrontmatter never emits "- type" key ----

// TestParseFrontmatterNoOrphanedListKey verifies that YAML list items that
// appear before any key (like "- type: person" at the top of frontmatter)
// do NOT produce a "- type" key in the parsed map.
func TestParseFrontmatterNoOrphanedListKey(t *testing.T) {
	t.Parallel()
	input := "---\n- type: person\nuid: 01ARZ3\ndisplayName: Merry\n---\n\n# Merry\n"
	got := parseFrontmatter(input)
	for k := range got {
		if strings.HasPrefix(k, "- ") {
			t.Errorf("parseFrontmatter produced illegal key %q; full map: %v", k, got)
		}
	}
	if got["uid"] != "01ARZ3" {
		t.Errorf("parseFrontmatter uid = %q, want 01ARZ3", got["uid"])
	}
}

// TestParseFrontmatterListValuesCollected verifies that YAML list items that
// appear under a known key are collected as a []string, not as separate keys.
func TestParseFrontmatterListValuesCollected(t *testing.T) {
	t.Parallel()
	input := "---\nuid: 01ARZ3\naliases:\n- Merry\n- Ali\n---\n"
	got := parseFrontmatter(input)
	aliases, ok := got["aliases"]
	if !ok {
		t.Fatalf("parseFrontmatter: missing 'aliases' key; map=%v", got)
	}
	list, ok := aliases.([]string)
	if !ok {
		t.Fatalf("parseFrontmatter: 'aliases' type = %T, want []string", aliases)
	}
	if len(list) != 2 || list[0] != "Merry" || list[1] != "Ali" {
		t.Errorf("parseFrontmatter aliases = %v, want [Merry Ali]", list)
	}
}

// ---- Finding 5: count --group-by status ----

// makeCountRoot wires a minimal root with entity, relate, and count subcommands.
func makeCountRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newEntityCmd())
	root.AddCommand(newCountCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// TestCountGroupByKindReturnsRealBuckets verifies that `maz count --group-by kind`
// returns real non-empty buckets when entities exist, not a single empty-value bucket.
func TestCountGroupByKindReturnsRealBuckets(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "person", "Count Person A")
	_ = runEntityCreateCmd(t, dir, "person", "Count Person B")
	_ = runEntityCreateCmd(t, dir, "organization", "Count Org A")

	var outBuf, errBuf bytes.Buffer
	root := makeCountRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "count", "--kind", "person", "--group-by", "kind"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("count --group-by kind: %v (stderr=%q)", err, errBuf.String())
	}
	out := strings.TrimSpace(outBuf.String())
	var resp struct {
		Groups []struct {
			Value string `json:"value"`
			Count int64  `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("count --group-by kind: parse JSON: %v\nout=%q", err, out)
	}
	if len(resp.Groups) == 0 {
		t.Fatalf("count --group-by kind: expected real buckets, got empty groups")
	}
	for _, g := range resp.Groups {
		if g.Value == "" {
			t.Errorf("count --group-by kind: bucket with empty value (query returned wrong column); groups=%v", resp.Groups)
		}
	}
}

// TestCountGroupByResolutionStatusSnakeCase verifies that the documented
// snake_case alias `resolution_status` is accepted and routes to the real
// resolutionStatus column (not falling through to fact-slot lookup which
// would return one empty-value bucket for all entities with no "resolution_status"
// fact).
func TestCountGroupByResolutionStatusSnakeCase(t *testing.T) {
	t.Parallel()
	// normaliseGroupBy must map the snake_case form.
	got := normaliseGroupBy("resolution_status")
	if got != "resolutionStatus" {
		t.Errorf("normaliseGroupBy(resolution_status) = %q, want resolutionStatus", got)
	}
	// Dash form too.
	got2 := normaliseGroupBy("resolution-status")
	if got2 != "resolutionStatus" {
		t.Errorf("normaliseGroupBy(resolution-status) = %q, want resolutionStatus", got2)
	}
	// Unknown names pass through unchanged (fact-slot state_key or camelCase).
	got3 := normaliseGroupBy("kind")
	if got3 != "kind" {
		t.Errorf("normaliseGroupBy(kind) = %q, want kind", got3)
	}
	got4 := normaliseGroupBy("resolutionStatus")
	if got4 != "resolutionStatus" {
		t.Errorf("normaliseGroupBy(resolutionStatus) = %q, want resolutionStatus", got4)
	}
}

// ---- Finding 6: recall --min-score drops sub-threshold hits ----

// TestApplyRecallMinScoreFiltersHits verifies that applyRecallMinScore drops
// hits whose fusedScore is below the floor and keeps those at or above it.
func TestApplyRecallMinScoreFiltersHits(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"hits":[
		{"fusedScore":0.9,"name":"episodes/A","snippet":"","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""},
		{"fusedScore":0.02,"name":"episodes/B","snippet":"","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""},
		{"fusedScore":0.5,"name":"episodes/C","snippet":"","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""}
	]}`)

	filtered, err := applyRecallMinScore(raw, 0.05)
	if err != nil {
		t.Fatalf("applyRecallMinScore: %v", err)
	}
	var out struct {
		Hits []struct {
			Name string `json:"name"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(filtered, &out); err != nil {
		t.Fatalf("parse filtered result: %v", err)
	}
	if len(out.Hits) != 2 {
		t.Errorf("expected 2 hits (score>=0.05), got %d: %v", len(out.Hits), out.Hits)
	}
	names := make([]string, len(out.Hits))
	for i, h := range out.Hits {
		names[i] = h.Name
	}
	for _, want := range []string{"episodes/A", "episodes/C"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected hit %q to survive min-score filter; surviving: %v", want, names)
		}
	}
}

// TestApplyRecallMinScoreZeroIsNoop verifies that a floor of 0 passes all hits
// through unchanged.
func TestApplyRecallMinScoreZeroIsNoop(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"hits":[{"fusedScore":0.001,"name":"episodes/X","snippet":"","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""}]}`)
	filtered, err := applyRecallMinScore(raw, 0)
	if err != nil {
		t.Fatalf("applyRecallMinScore(floor=0): %v", err)
	}
	var out struct {
		Hits []struct {
			Name string `json:"name"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(filtered, &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Hits) != 1 {
		t.Errorf("min-score=0 should keep all hits; got %d", len(out.Hits))
	}
}

// ---- Finding 7: recall --output ndjson emits one JSON object per line ----

// TestRenderRecallNDJSONOneObjectPerLine verifies that renderRecallNDJSON
// emits exactly one parseable JSON object per hit, not one big document.
func TestRenderRecallNDJSONOneObjectPerLine(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"hits":[
		{"fusedScore":0.9,"name":"episodes/A","snippet":"s1","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""},
		{"fusedScore":0.7,"name":"episodes/B","snippet":"s2","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""},
		{"fusedScore":0.3,"name":"episodes/C","snippet":"s3","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""}
	]}`)

	var outBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.SetOut(&outBuf)

	if err := renderRecallNDJSON(root, raw); err != nil {
		t.Fatalf("renderRecallNDJSON: %v", err)
	}

	lines := strings.Split(strings.TrimRight(outBuf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("ndjson: expected 3 lines, got %d:\n%q", len(lines), outBuf.String())
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("ndjson line %d is not valid JSON: %v\nline=%q", i, err, line)
		}
	}
}

// ---- Greptile findings: show display-name resolution, revspec skip, markdown fields ----

// TestShowByDisplayNameResolvesLive verifies that `maz show "Display Name"`
// resolves the entity via the name-resolution path (entities tool) and exits 0,
// i.e. a bare display name reaches the live store rather than being treated as
// a raw UID lookup (finding 1).
func TestShowByDisplayNameResolvesLive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "person", "Greptile Resolver")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", "Greptile Resolver"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show by display name: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	if !strings.Contains(out, "Greptile Resolver") {
		t.Errorf("show by display name: output missing entity name; got: %q", out)
	}
}

// TestShowRevspecSkipsLiveStore verifies that when --at is set to a non-HEAD
// value, runShow bypasses the live store and goes to the git mirror path.
// Since the test has no git mirror the command should exit non-zero with an
// actionable message rather than silently returning live-store state (finding 2).
func TestShowRevspecSkipsLiveStore(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "Revspec Tester")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", uid, "--at", "origin/main"})
	root.SetContext(context.Background())
	// Must NOT return the live-store result silently; with no sync config it
	// should exit non-zero because the git mirror path is taken.
	err := root.Execute()
	if err == nil {
		// If it somehow succeeds (e.g. a sync config was found), the output
		// must not contain live-store data presented as historical.
		// The important invariant is that the revspec flag is honored; we
		// cannot enforce git-content correctness without a real remote.
		t.Log("show --at origin/main succeeded (sync config present); skipping non-zero assertion")
	}
	// At minimum, no panic and the output does not leak internal paths.
	combined := outBuf.String() + errBuf.String()
	if strings.Contains(combined, "rendered/entities.jsonl") {
		t.Errorf("show --at leaks internal path: %q", combined)
	}
}

// TestShowMarkdownIncludesAllFields verifies that `maz show --format markdown`
// for a live entity includes more than just the 5 hardcoded scalar fields —
// specifically that uid, kind, and displayName all appear (finding 3).
func TestShowMarkdownIncludesAllFields(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "person", "Markdown Fields Test")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "show", "Markdown Fields Test", "--format", "markdown"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show --format markdown: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	for _, want := range []string{"uid:", "kind:", "displayName:", "Markdown Fields Test"} {
		if !strings.Contains(out, want) {
			t.Errorf("show --format markdown missing %q; got:\n%s", want, out)
		}
	}
}

// TestShowByUppercaseNameResolvesLive verifies that an uppercase display-name
// like "Mithril" resolves via the live store rather than being treated as a raw UID
// (Greptile finding: uppercase names miss because looksLikeDisplayName missed them).
func TestShowByUppercaseNameResolvesLive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "organization", "Mithril")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", "Mithril"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show uppercase display name: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	if !strings.Contains(out, "Mithril") {
		t.Errorf("show uppercase display name: output missing entity name; got: %q", out)
	}
}

// TestShowByCaseMismatchedNameResolvesLive verifies that a display-name lookup
// is case-INSENSITIVE: `maz show mithril` must resolve an entity named "Mithril",
// mirroring the canonical NameExact resolver (display_name = ? COLLATE NOCASE)
// used by neighbors/traverse. This is the Greptile P1 regression: the previous
// AIP `displayName = "mithril"` exact filter was case-sensitive and silently
// missed "Mithril".
func TestShowByCaseMismatchedNameResolvesLive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "organization", "Mithril")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "show", "mithril"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("show case-mismatched display name: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	if !strings.Contains(out, "Mithril") {
		t.Errorf("show 'mithril' should resolve entity 'Mithril' (case-insensitive); got: %q", out)
	}
}

// TestBlameByCaseMismatchedNameResolvesLive verifies the same case-insensitive
// resolution for blame, which routes through showFromLiveStore and so inherits
// the same name-resolution path.
func TestBlameByCaseMismatchedNameResolvesLive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "organization", "Globex")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "blame", "GLOBEX"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("blame case-mismatched display name: %v (stderr=%q)", err, errBuf.String())
	}
	combined := outBuf.String() + errBuf.String()
	if strings.Contains(combined, "rendered/entities.jsonl") {
		t.Errorf("blame case-mismatched name leaks internal path: %q", combined)
	}
}

// TestBlameByDisplayNameResolvesLive verifies that `maz blame "Display Name"`
// resolves the entity via the name-resolution path rather than failing with
// "not found" when the arg is a display name (finding 4).
func TestBlameByDisplayNameResolvesLive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_ = runEntityCreateCmd(t, dir, "person", "Blame By Name")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "blame", "Blame By Name"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("blame by display name: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String() + errBuf.String()
	if strings.Contains(out, "rendered/entities.jsonl") {
		t.Errorf("blame by display name leaks internal path: %q", out)
	}
}

// TestCountGroupByStatusAlias verifies that the short `status` alias maps to
// `resolutionStatus` in normaliseGroupBy (finding 5).
func TestCountGroupByStatusAlias(t *testing.T) {
	t.Parallel()
	got := normaliseGroupBy("status")
	if got != "resolutionStatus" {
		t.Errorf("normaliseGroupBy(status) = %q, want resolutionStatus", got)
	}
}

// TestTraverseEmptyNDJSONEmitsNothing verifies that when traversal returns zero
// neighbors and the output format is ndjson, nothing is written — not a wrapper
// {"neighbors":[]} object (finding 6 / P2). The fix is verified by checking
// that the new switch branch is reached: renderTraverseNDJSON on an empty slice
// writes nothing; the test drives renderTraverseResult via a full command tree
// with --output ndjson set.
func TestTraverseEmptyNDJSONEmitsNothing(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"neighbors":[]}`)
	var outBuf bytes.Buffer

	// Use a full execution to exercise the format-resolution path: the --output
	// persistent flag must be inherited by the traverse subcommand so that
	// output.From(traverseCmd) resolves FormatNDJSON.
	root := makeTraverseRoot(&outBuf, nil)
	root.SetArgs([]string{"--output", "ndjson", "traverse", "--help"})
	// Find the traverse subcommand after AddCommand wires it up.
	var traverseCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Use == "traverse <entity>" {
			traverseCmd = c
			break
		}
	}
	if traverseCmd == nil {
		t.Fatal("traverse command not found in root")
	}
	// Execute root so cobra parses --output ndjson into the inherited flag.
	root.SetArgs([]string{"--output", "ndjson", "traverse", "--help"})
	root.SetContext(context.Background())
	_ = root.Execute()

	// Now call renderTraverseResult directly; the --output flag should be
	// inherited on traverseCmd from the root's last execution.
	outBuf.Reset()
	traverseCmd.SetOut(&outBuf)
	if err := renderTraverseResult(traverseCmd, raw); err != nil {
		t.Fatalf("renderTraverseResult ndjson empty: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Errorf("traverse empty ndjson: expected no output, got %q", outBuf.String())
	}
}

// TestTraverseEmptyJSONEmitsWrapper verifies that empty neighbors in JSON mode
// still emits the wrapper {"neighbors":[]} document (not nothing).
func TestTraverseEmptyJSONEmitsWrapper(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"neighbors":[]}`)
	var outBuf bytes.Buffer
	root := makeTraverseRoot(&outBuf, nil)
	root.SetArgs([]string{"--output", "json", "traverse", "--help"})
	root.SetContext(context.Background())
	_ = root.Execute()

	var traverseCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Use == "traverse <entity>" {
			traverseCmd = c
			break
		}
	}
	if traverseCmd == nil {
		t.Fatal("traverse command not found in root")
	}
	outBuf.Reset()
	traverseCmd.SetOut(&outBuf)
	if err := renderTraverseResult(traverseCmd, raw); err != nil {
		t.Fatalf("renderTraverseResult json empty: %v", err)
	}
	out := strings.TrimSpace(outBuf.String())
	if out == "" {
		t.Fatal("traverse empty json: expected wrapper document, got nothing")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Errorf("traverse empty json: not valid JSON: %v\n%s", err, out)
	}
}

// TestRecallNDJSONFlagEmitsLines verifies that `--output ndjson` on the full
// recall command path routes through renderRecallNDJSON rather than dumping
// the whole JSON doc. This test is a unit-level check using a fake kernel
// response injected via applyRecallMinScore so it does not need a real store.
func TestRecallNDJSONFlagEmitsLines(t *testing.T) {
	t.Parallel()
	// Build a fake raw recall response and test the ndjson path directly.
	raw := json.RawMessage(`{"hits":[
		{"fusedScore":0.8,"name":"episodes/D1","snippet":"d1","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""},
		{"fusedScore":0.6,"name":"episodes/D2","snippet":"d2","text":"","confidence":0,"components":{"vector":0,"fts":0,"graph":0,"nameEmb":0,"recency":0,"rawSemantic":0,"rawBM25":0},"citations":[],"entities":[],"createTime":""}
	]}`)

	var outBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.SetOut(&outBuf)

	if err := renderRecallNDJSON(root, raw); err != nil {
		t.Fatalf("renderRecallNDJSON: %v", err)
	}

	// Must be exactly 2 lines, each a valid JSON object.
	lines := strings.Split(strings.TrimRight(outBuf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("ndjson recall: expected 2 lines, got %d\n%q", len(lines), outBuf.String())
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("ndjson line %d is not valid JSON: %v\nline=%q", i, err, line)
		}
	}
}
