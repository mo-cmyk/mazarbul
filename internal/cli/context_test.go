package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// makeContextCmd builds a minimal command tree carrying the persistent flags
// the context subcommands resolve through cmd.Root().
func makeContextCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	ctx := newContextCmd()
	root.AddCommand(ctx)
	if outBuf != nil {
		root.SetOut(outBuf)
		ctx.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		ctx.SetErr(errBuf)
	}
	return root
}

// ─── help text ────────────────────────────────────────────────────────────────

func TestContextHelp_NonEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "--help"})
	_ = root.Execute()
	out := buf.String()
	if out == "" {
		t.Fatal("context --help produced no output")
	}
	for _, want := range []string{"build", "index", "pin", "brief"} {
		if !strings.Contains(out, want) {
			t.Errorf("context --help missing %q\n%s", want, out)
		}
	}
}

func TestContextBuildHelp_HasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "build", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "maz context build", "--query", "--budget"} {
		if !strings.Contains(out, want) {
			t.Errorf("context build --help missing %q\n%s", want, out)
		}
	}
}

func TestContextIndexHelp_HasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "index", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "hot.md", "index.md", "--hot-path", "--index-path"} {
		if !strings.Contains(out, want) {
			t.Errorf("context index --help missing %q\n%s", want, out)
		}
	}
}

func TestContextPinHelp_HasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "pin", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "maz context pin", "--unpin", "IDEMPOTENT"} {
		if !strings.Contains(out, want) {
			t.Errorf("context pin --help missing %q\n%s", want, out)
		}
	}
}

// ─── steering subcommands (#15): note / correct / unpin ─────────────────────

// TestContextHelp_ListsSteeringVerbs verifies the group help advertises the new
// steering verbs alongside the existing doc-pin verb, so the two pin concepts
// are both discoverable.
func TestContextHelp_ListsSteeringVerbs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"note", "correct", "unpin", "pin"} {
		if !strings.Contains(out, want) {
			t.Errorf("context --help missing steering verb %q\n%s", want, out)
		}
	}
}

func TestContextNoteHelp_HasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "note", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "maz context note", "--target", "--scope", "DISTINCT"} {
		if !strings.Contains(out, want) {
			t.Errorf("context note --help missing %q\n%s", want, out)
		}
	}
}

func TestContextCorrectHelp_HasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "correct", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "maz context correct", "contextPins/", "entities/"} {
		if !strings.Contains(out, want) {
			t.Errorf("context correct --help missing %q\n%s", want, out)
		}
	}
}

func TestContextUnpinHelp_HasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "unpin", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "maz context unpin", "tombstoned", "documentUid"} {
		if !strings.Contains(out, want) {
			t.Errorf("context unpin --help missing %q\n%s", want, out)
		}
	}
}

// TestContextNote_RequiresArg / Correct / Unpin verify argument arity so a
// scripted invocation with the wrong number of args fails fast (exit non-zero)
// instead of blocking or silently succeeding.
func TestContextNote_RequiresArg(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	root := makeContextCmd(nil, &errBuf)
	root.SetArgs([]string{"context", "note"})
	if err := root.Execute(); err == nil {
		t.Fatal("context note with no arg should error")
	}
}

func TestContextCorrect_RequiresTwoArgs(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	root := makeContextCmd(nil, &errBuf)
	root.SetArgs([]string{"context", "correct", "only-one"})
	if err := root.Execute(); err == nil {
		t.Fatal("context correct with one arg should error")
	}
}

func TestContextUnpin_RequiresArg(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	root := makeContextCmd(nil, &errBuf)
	root.SetArgs([]string{"context", "unpin"})
	if err := root.Execute(); err == nil {
		t.Fatal("context unpin with no arg should error")
	}
}

// TestContextNoteFlagDefaults verifies the note flags are wired with empty
// defaults (tool backfills personal scope; no target by default).
func TestContextNoteFlagDefaults(t *testing.T) {
	t.Parallel()
	cmd := newContextNoteCmd()
	target, _ := cmd.Flags().GetString("target")
	scope, _ := cmd.Flags().GetString("scope")
	if target != "" || scope != "" {
		t.Errorf("note defaults: target=%q scope=%q, want both empty", target, scope)
	}
}

// TestContextSteerJSONShape verifies the {name,status} write-result shape is
// stable and parseable, the JSON-mode contract for note/correct/unpin.
func TestContextSteerJSONShape(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(contextSteerResponse{Name: "contextPins/01ARZ3NDEKTSV4RRFFQ69G5FAV", Status: "pinned"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed contextSteerResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw: %s", err, raw)
	}
	if parsed.Name != "contextPins/01ARZ3NDEKTSV4RRFFQ69G5FAV" || parsed.Status != "pinned" {
		t.Errorf("round-trip mismatch: %+v", parsed)
	}
}

// TestContextShowJSONShape verifies the bare `maz context` --json shape carries
// both the brief and the active pins.
func TestContextShowJSONShape(t *testing.T) {
	t.Parallel()
	out := contextShowOutput{
		Brief: contextBriefResponse{Markdown: "# Context\n", Tokens: 2, Budget: 4000},
		Pins: []contextPinView{
			{Name: "contextPins/P1", Type: "pin", Text: "ship friday", Scope: "personal"},
			{Name: "contextPins/C1", Type: "correction", Text: "fixed", TargetRef: "entities/E1"},
		},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"brief"`, `"markdown"`, `"pins"`, `"contextPins/P1"`, `"correction"`, `"targetRef"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("context show JSON missing %q\nraw: %s", want, raw)
		}
	}
	var parsed contextShowOutput
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(parsed.Pins) != 2 || parsed.Pins[0].Name != "contextPins/P1" {
		t.Errorf("pins lost in round-trip: %+v", parsed.Pins)
	}
}

// TestRenderContextPins_PlainText verifies the 📌 section lists each pin with its
// resource name (so an id can be copied into correct/unpin), and that an empty
// set prints the add hint.
func TestRenderContextPins_PlainText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "context"}
	cmd.SetOut(&buf)
	pins := []contextPinView{
		{Name: "contextPins/P1", Type: "pin", Text: "ship friday"},
		{Name: "contextPins/C1", Type: "correction", Text: "fixed", TargetRef: "entities/E1"},
	}
	if err := renderContextPins(cmd, pins); err != nil {
		t.Fatalf("renderContextPins: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"📌 Active pins", "contextPins/P1", "ship friday", "correction → entities/E1", "maz context unpin"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered pins missing %q\n%s", want, out)
		}
	}

	buf.Reset()
	if err := renderContextPins(cmd, nil); err != nil {
		t.Fatalf("renderContextPins(empty): %v", err)
	}
	if !strings.Contains(buf.String(), "No active pins") {
		t.Errorf("empty pins should print add hint, got: %q", buf.String())
	}
}

// ─── flag defaults ─────────────────────────────────────────────────────────────

func TestContextIndexFlagDefaults(t *testing.T) {
	t.Parallel()
	cmd := newContextIndexCmd()
	hotPath, _ := cmd.Flags().GetString("hot-path")
	idxPath, _ := cmd.Flags().GetString("index-path")
	if hotPath != "hot.md" {
		t.Errorf("--hot-path default = %q, want hot.md", hotPath)
	}
	if idxPath != "index.md" {
		t.Errorf("--index-path default = %q, want index.md", idxPath)
	}
	pin, _ := cmd.Flags().GetBool("pin")
	if pin {
		t.Error("--pin default should be false")
	}
}

func TestContextPinFlagDefaults(t *testing.T) {
	t.Parallel()
	cmd := newContextPinCmd()
	unpin, _ := cmd.Flags().GetBool("unpin")
	if unpin {
		t.Error("--unpin default should be false")
	}
}

// ─── looksLikePath ────────────────────────────────────────────────────────────

func TestLooksLikePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ref  string
		want bool
	}{
		{"notes/foo.md", true},
		{"hot.md", true},
		{"index.md", true},
		{"sub/dir/file.md", true},
		// Extensionless, root-level vault names must be treated as paths:
		// CleanVaultPath appends ".md", so "inbox" names "inbox.md".
		{"inbox", true},
		{"drafts", true},
		// A bare ULID (26 Crockford base32 chars) is the only UID-shaped ref.
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV", false},
		// 26 chars but contains an excluded ULID letter ('I'/'L'/'O'/'U') → path.
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ", true},
		// Wrong length → path.
		{"01ARZ3NDEKTSV4RRFFQ69G5FA", true}, // 25 chars
		// edge cases
		{"", true},
	}
	for _, c := range cases {
		if got := looksLikePath(c.ref); got != c.want {
			t.Errorf("looksLikePath(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

// TestBuildPinReadInput verifies the readDocument request maps a ULID ref to a
// documentUid lookup and a path ref to a CleanVaultPath-normalised path (so an
// extensionless name like "inbox" addresses the stored "inbox.md").
func TestBuildPinReadInput(t *testing.T) {
	t.Parallel()
	// Extensionless root name → normalised path with .md.
	in, err := buildPinReadInput("inbox")
	if err != nil {
		t.Fatalf("buildPinReadInput(inbox): %v", err)
	}
	if in["path"] != "inbox.md" {
		t.Errorf("path = %v, want inbox.md", in["path"])
	}
	if _, hasUID := in["documentUid"]; hasUID {
		t.Error("documentUid should be absent for a path ref")
	}

	// Nested extensionless path → normalised with .md.
	in, err = buildPinReadInput("notes/foo")
	if err != nil {
		t.Fatalf("buildPinReadInput(notes/foo): %v", err)
	}
	if in["path"] != "notes/foo.md" {
		t.Errorf("path = %v, want notes/foo.md", in["path"])
	}

	// Already-.md path → passes through normalised.
	in, err = buildPinReadInput("notes/foo.md")
	if err != nil {
		t.Fatalf("buildPinReadInput(notes/foo.md): %v", err)
	}
	if in["path"] != "notes/foo.md" {
		t.Errorf("path = %v, want notes/foo.md", in["path"])
	}

	// ULID ref → documentUid lookup, no normalisation.
	in, err = buildPinReadInput("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("buildPinReadInput(ULID): %v", err)
	}
	if in["documentUid"] != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("documentUid = %v, want the ULID", in["documentUid"])
	}
	if _, hasPath := in["path"]; hasPath {
		t.Error("path should be absent for a ULID ref")
	}

	// A path that escapes the vault root → error surfaced (not silently UID'd).
	if _, err := buildPinReadInput("../escape"); err == nil {
		t.Error("expected error for a vault-escaping path ref")
	}
}

func TestLooksLikeULID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ref  string
		want bool
	}{
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV", true},
		{"01arz3ndektsv4rrffq69g5fav", true},   // lowercase accepted
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ", false},  // contains I/L/O/U
		{"01ARZ3NDEKTSV4RRFFQ69G5FA", false},   // 25 chars
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV1", false}, // 27 chars
		{"inbox", false},
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeULID(c.ref); got != c.want {
			t.Errorf("looksLikeULID(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

// ─── hasFrontmatter ──────────────────────────────────────────────────────────

func TestHasFrontmatter(t *testing.T) {
	t.Parallel()
	if !hasFrontmatter("---\ntitle: foo\n---\nbody") {
		t.Error("expected true for body starting with ---\\n")
	}
	if !hasFrontmatter("---\r\ntitle: foo\r\n---\r\nbody") {
		t.Error("expected true for body starting with ---\\r\\n (CRLF)")
	}
	if hasFrontmatter("# title\nbody") {
		t.Error("expected false for body without frontmatter")
	}
	if hasFrontmatter("") {
		t.Error("expected false for empty body")
	}
}

// TestComposePinWrite_CRLFFrontmatter_PatchesBody verifies a CRLF markdown file
// takes the patch-in-body path (not the synthesize-from-field path that
// writeDocument would reject because the body DOES have frontmatter), and that
// the CRLF line endings are preserved in the patched output.
func TestComposePinWrite_CRLFFrontmatter_PatchesBody(t *testing.T) {
	t.Parallel()
	body := "---\r\ntitle: Foo\r\n---\r\n# body\r\n"
	payload, err := composePinWrite("notes/foo.md", body, true)
	if err != nil {
		t.Fatalf("composePinWrite: %v", err)
	}
	if _, hasPinField := payload["pinned"]; hasPinField {
		t.Error("pinned field should be absent for a CRLF body with frontmatter; it must be patched in-body")
	}
	patched, _ := payload["body"].(string)
	if !strings.Contains(patched, "pinned: true") {
		t.Errorf("patched CRLF body missing pinned: true:\n%q", patched)
	}
	// The CRLF line ending must be preserved on the appended pinned line.
	if !strings.Contains(patched, "pinned: true\r\n") {
		t.Errorf("patched CRLF body did not preserve CRLF on the pinned line:\n%q", patched)
	}
	// The body content + closing fence must survive verbatim.
	if !strings.Contains(patched, "# body\r\n") {
		t.Errorf("CRLF body content lost:\n%q", patched)
	}
}

// ─── patchFrontmatterPinned ───────────────────────────────────────────────────

func TestPatchFrontmatterPinned_SetsPinnedWhenAbsent(t *testing.T) {
	t.Parallel()
	body := "---\ntitle: My Note\n---\n# body\n"
	patched, err := patchFrontmatterPinned(body, true)
	if err != nil {
		t.Fatalf("patchFrontmatterPinned: %v", err)
	}
	if !strings.Contains(patched, "pinned: true") {
		t.Errorf("pinned: true not in patched body:\n%s", patched)
	}
	// Body content must be preserved.
	if !strings.Contains(patched, "# body") {
		t.Errorf("body content missing from patched result:\n%s", patched)
	}
	// Title must be preserved.
	if !strings.Contains(patched, "title: My Note") {
		t.Errorf("title missing from patched result:\n%s", patched)
	}
}

func TestPatchFrontmatterPinned_ReplacesExistingPinned(t *testing.T) {
	t.Parallel()
	body := "---\ntitle: My Note\npinned: true\n---\n# body\n"
	patched, err := patchFrontmatterPinned(body, false)
	if err != nil {
		t.Fatalf("patchFrontmatterPinned: %v", err)
	}
	if !strings.Contains(patched, "pinned: false") {
		t.Errorf("pinned: false not in patched body:\n%s", patched)
	}
	if strings.Contains(patched, "pinned: true") {
		t.Errorf("old pinned: true still present after patch:\n%s", patched)
	}
}

func TestPatchFrontmatterPinned_ErrorOnMissingClosingFence(t *testing.T) {
	t.Parallel()
	body := "---\ntitle: unclosed\n"
	_, err := patchFrontmatterPinned(body, true)
	if err == nil {
		t.Fatal("want error for unclosed frontmatter, got nil")
	}
}

// ─── composePinWrite ─────────────────────────────────────────────────────────

func TestComposePinWrite_NoFrontmatter_UsesField(t *testing.T) {
	t.Parallel()
	// A body without frontmatter: writeDocument should receive a separate
	// pinned field (it will synthesize the frontmatter).
	payload, err := composePinWrite("notes/foo.md", "# body\n", true)
	if err != nil {
		t.Fatalf("composePinWrite: %v", err)
	}
	if payload["path"] != "notes/foo.md" {
		t.Errorf("path = %v, want notes/foo.md", payload["path"])
	}
	if pinned, ok := payload["pinned"].(bool); !ok || !pinned {
		t.Errorf("pinned field = %v, want true", payload["pinned"])
	}
	// Body must be passed through verbatim.
	if payload["body"] != "# body\n" {
		t.Errorf("body = %q, want %q", payload["body"], "# body\n")
	}
}

func TestComposePinWrite_WithFrontmatter_PatchesBody(t *testing.T) {
	t.Parallel()
	body := "---\ntitle: Foo\n---\n# body\n"
	payload, err := composePinWrite("notes/foo.md", body, true)
	if err != nil {
		t.Fatalf("composePinWrite: %v", err)
	}
	// The pinned flag must NOT appear as a separate field (writeDocument
	// would reject it alongside a body with its own frontmatter).
	if _, hasPinField := payload["pinned"]; hasPinField {
		t.Error("pinned field should be absent when body has frontmatter; it was patched in-body instead")
	}
	// The body must have been patched to include pinned: true.
	patchedBody, _ := payload["body"].(string)
	if !strings.Contains(patchedBody, "pinned: true") {
		t.Errorf("patched body missing pinned: true:\n%s", patchedBody)
	}
}

// ─── emitPinResult JSON mode ──────────────────────────────────────────────────

func TestEmitPinResult_JSONMode(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("json", true, "JSON output")
	// Set the --json flag as if the user passed it.
	if err := root.PersistentFlags().Set("json", "true"); err != nil {
		t.Fatalf("set --json: %v", err)
	}
	cmd := &cobra.Command{Use: "pin"}
	root.AddCommand(cmd)
	root.SetOut(&outBuf)
	cmd.SetOut(&outBuf)

	out := contextPinOutput{
		Path:        "notes/foo.md",
		Pinned:      true,
		Changed:     true,
		DocumentUID: "DOCUID",
	}
	if err := emitPinResult(cmd, out); err != nil {
		t.Fatalf("emitPinResult: %v", err)
	}
	line := strings.TrimSpace(outBuf.String())
	var parsed contextPinOutput
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\nraw: %s", err, line)
	}
	if parsed.Path != "notes/foo.md" {
		t.Errorf("path = %q, want notes/foo.md", parsed.Path)
	}
	if !parsed.Pinned {
		t.Error("pinned = false, want true")
	}
	if !parsed.Changed {
		t.Error("changed = false, want true")
	}
}

// TestEmitPinResult_PlainText tests the plain-text rendering path directly,
// bypassing output.From (which defaults to JSON when stdout is a pipe, i.e.
// always in tests). The actionLabel helper is tested instead.
func TestEmitPinResult_PlainTextActionLabel_Pinned(t *testing.T) {
	t.Parallel()
	out := contextPinOutput{Path: "notes/foo.md", Pinned: true, Changed: true}
	label := pinActionLabel(out)
	if label != "pinned" {
		t.Errorf("actionLabel = %q, want pinned", label)
	}
}

func TestEmitPinResult_PlainTextActionLabel_Unpinned(t *testing.T) {
	t.Parallel()
	out := contextPinOutput{Path: "notes/foo.md", Pinned: false, Changed: true}
	label := pinActionLabel(out)
	if label != "unpinned" {
		t.Errorf("actionLabel = %q, want unpinned", label)
	}
}

func TestEmitPinResult_PlainTextActionLabel_NoOp(t *testing.T) {
	t.Parallel()
	out := contextPinOutput{Path: "notes/foo.md", Pinned: true, Changed: false}
	label := pinActionLabel(out)
	if !strings.Contains(label, "no-op") {
		t.Errorf("actionLabel = %q, want to contain no-op", label)
	}
}

// ─── findFrontmatterEnd / setPinnedInYAML ─────────────────────────────────────

func TestFindFrontmatterEnd_Found(t *testing.T) {
	t.Parallel()
	s := "title: foo\n---\nrest"
	got := findFrontmatterEnd(s)
	if got != 11 { // "title: foo\n" = 11 bytes
		t.Errorf("findFrontmatterEnd = %d, want 11", got)
	}
}

func TestFindFrontmatterEnd_NotFound(t *testing.T) {
	t.Parallel()
	if got := findFrontmatterEnd("title: foo\n"); got != -1 {
		t.Errorf("findFrontmatterEnd = %d, want -1", got)
	}
}

func TestSetPinnedInYAML_AppendsWhenAbsent(t *testing.T) {
	t.Parallel()
	result := setPinnedInYAML("title: foo\n", "true", "\n")
	if !strings.Contains(result, "pinned: true") {
		t.Errorf("pinned missing from result: %q", result)
	}
	if !strings.Contains(result, "title: foo") {
		t.Errorf("title missing from result: %q", result)
	}
}

func TestSetPinnedInYAML_ReplacesExisting(t *testing.T) {
	t.Parallel()
	result := setPinnedInYAML("title: foo\npinned: true\n", "false", "\n")
	if !strings.Contains(result, "pinned: false") {
		t.Errorf("pinned: false missing: %q", result)
	}
	if strings.Contains(result, "pinned: true") {
		t.Errorf("old pinned: true still present: %q", result)
	}
	// Only one pinned line.
	count := strings.Count(result, "pinned:")
	if count != 1 {
		t.Errorf("want exactly 1 'pinned:' line, got %d in: %q", count, result)
	}
}

// ─── context pin requires exactly one argument ────────────────────────────────

func TestContextPin_RequiresArg(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	root := makeContextCmd(nil, &errBuf)
	root.SetArgs([]string{"context", "pin"})
	err := root.Execute()
	if err == nil {
		t.Fatal("context pin with no arg should error")
	}
}

// ─── context build --since ────────────────────────────────────────────────────

// TestContextBuildHelp_HasSinceFlag verifies --since appears in the build help.
func TestContextBuildHelp_HasSinceFlag(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "build", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"--since", "since", "7d", "RFC 3339"} {
		if !strings.Contains(out, want) {
			t.Errorf("context build --help missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestContextBuildSince_InvalidValue verifies an unparseable --since value
// exits with code 1 and does not block on tool invocation.
func TestContextBuildSince_InvalidValue(t *testing.T) {
	t.Parallel()
	var outBuf, errBuf bytes.Buffer
	root := makeContextCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"context", "build", "--since", "not-a-time"})
	err := root.Execute()
	if err == nil {
		t.Fatal("context build --since <invalid> should return an error")
	}
	// The error message should mention --since.
	errOut := errBuf.String()
	if !strings.Contains(errOut, "--since") {
		t.Errorf("error output missing '--since', got: %q", errOut)
	}
}

// TestContextBuildSince_FlagDefaults verifies the --since flag is wired with
// an empty default and the right usage text.
func TestContextBuildSince_FlagDefaults(t *testing.T) {
	t.Parallel()
	cmd := newContextBuildCmd()
	since, err := cmd.Flags().GetString("since")
	if err != nil {
		t.Fatalf("--since flag not registered: %v", err)
	}
	if since != "" {
		t.Errorf("--since default should be empty, got %q", since)
	}
}

// TestContextBuildJSON_EmitsBriefAndPins verifies that `maz context build --json`
// (non-since, non-out path) emits top-level "brief" and "pins" keys, matching
// the shape of bare `maz context --json` (contextShowOutput).
func TestContextBuildJSON_EmitsBriefAndPins(t *testing.T) {
	t.Parallel()

	// The shape is validated via struct round-trip. The actual RunE dispatches to
	// the daemon, so we test the output type directly: marshal a contextShowOutput
	// and assert both top-level keys are present and parseable.
	out := contextShowOutput{
		Brief: contextBriefResponse{
			Markdown: "# Context\n\ncold-start brief",
			Tokens:   42,
			Budget:   4000,
			Dropped:  0,
		},
		Pins: []contextPinView{
			{Name: "contextPins/01ARZ3NDEKTSV4RRFFQ69G5FAV", Type: "pin", Text: "ship the wave"},
		},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal contextShowOutput: %v", err)
	}
	line := strings.TrimSpace(string(raw))

	// Must carry top-level "brief" and "pins" keys.
	for _, want := range []string{`"brief"`, `"pins"`, `"markdown"`, `"tokens"`, `"budget"`, `"dropped"`} {
		if !strings.Contains(line, want) {
			t.Errorf("context build JSON shape missing %q\nraw: %s", want, line)
		}
	}

	// Round-trip must decode back to the same type without error.
	var parsed contextShowOutput
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("context build JSON not parseable: %v\nraw: %s", err, line)
	}
	if parsed.Brief.Tokens != 42 {
		t.Errorf("brief.tokens = %d, want 42", parsed.Brief.Tokens)
	}
	if len(parsed.Pins) != 1 || parsed.Pins[0].Type != "pin" {
		t.Errorf("pins round-trip mismatch: %+v", parsed.Pins)
	}
}

// TestContextBuildJSON_HelpDocumentsShape verifies the `context build` Long help
// text describes the {"brief":{...},"pins":[...]} JSON shape so the documented
// contract matches the implementation.
func TestContextBuildJSON_HelpDocumentsShape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeContextCmd(&buf, nil)
	root.SetArgs([]string{"context", "build", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{`"brief"`, `"pins"`, `"markdown"`, `"tokens"`, `"budget"`, `"dropped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("context build --help missing JSON shape key %q\n%s", want, out)
		}
	}
}

// TestContextBuildSince_JSONFieldsPresent verifies the contextSinceOutput JSON
// shape has the expected top-level fields when marshalled.
func TestContextBuildSince_JSONFieldsPresent(t *testing.T) {
	t.Parallel()
	out := contextSinceOutput{
		Since:     "2025-06-01T00:00:00Z",
		Entities:  []contextSinceEntity{{UID: "e1", Kind: "person", DisplayName: "Alice", CreateTime: "2025-06-10T00:00:00Z"}},
		Documents: []contextSinceDocument{{DocumentUID: "d1", Path: "notes/foo.md", Title: "Foo", UpdateTime: "2025-06-12T00:00:00Z"}},
		Truncated: true,
		Markdown:  "# Context\n",
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	for _, want := range []string{`"since"`, `"entities"`, `"documents"`, `"truncated"`, `"markdown"`, `"uid"`, `"displayName"`, `"documentUid"`} {
		if !strings.Contains(line, want) {
			t.Errorf("JSON shape missing %q\nraw: %s", want, line)
		}
	}

	// Round-trip: parse back and check field values.
	var parsed contextSinceOutput
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Since != "2025-06-01T00:00:00Z" {
		t.Errorf("since = %q, want 2025-06-01T00:00:00Z", parsed.Since)
	}
	if len(parsed.Entities) != 1 || parsed.Entities[0].DisplayName != "Alice" {
		t.Errorf("entities = %v, want Alice", parsed.Entities)
	}
	if len(parsed.Documents) != 1 || parsed.Documents[0].Path != "notes/foo.md" {
		t.Errorf("documents = %v, want notes/foo.md", parsed.Documents)
	}
	if !parsed.Truncated {
		t.Error("truncated = false, want true (field must round-trip)")
	}
}
