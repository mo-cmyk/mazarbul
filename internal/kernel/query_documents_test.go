package kernel

import (
	"strings"
	"testing"
)

func TestDocumentLaneEnabled(t *testing.T) {
	cases := []struct {
		name  string
		kinds []EventKind
		want  bool
	}{
		{"default (no filter) includes documents", nil, true},
		{"empty slice includes documents", []EventKind{}, true},
		{"explicit documentRevision includes", []EventKind{docEventKind}, true},
		{"mixed filter naming documents includes", []EventKind{EventKind("episode"), docEventKind}, true},
		{"episode-only filter excludes", []EventKind{EventKind("episode")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := documentLaneEnabled(tc.kinds); got != tc.want {
				t.Fatalf("documentLaneEnabled(%v) = %v, want %v", tc.kinds, got, tc.want)
			}
		})
	}
}

func TestSplitDocChunkID(t *testing.T) {
	cases := []struct {
		id      string
		wantUID string
		wantIdx int
		wantOK  bool
	}{
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV#0", "01ARZ3NDEKTSV4RRFFQ69G5FAV", 0, true},
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV#12", "01ARZ3NDEKTSV4RRFFQ69G5FAV", 12, true},
		{"no-separator", "", 0, false},
		{"#3", "", 0, false},   // empty uid
		{"uid#", "", 0, false}, // empty index
		{"uid#notanumber", "", 0, false},
		{"uid#-1", "", 0, false},
	}
	for _, tc := range cases {
		uid, idx, ok := splitDocChunkID(tc.id)
		if uid != tc.wantUID || idx != tc.wantIdx || ok != tc.wantOK {
			t.Errorf("splitDocChunkID(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tc.id, uid, idx, ok, tc.wantUID, tc.wantIdx, tc.wantOK)
		}
	}
}

func TestDocHitTextAndSource_ChunkSliceHeadingAndLine(t *testing.T) {
	body := "# Title\n\nintro text\n\n## Goals\n\ngoal text here\n"
	goalsOffset := strings.Index(body, "## Goals")
	row := docRow{path: "notes/foo.md", body: body}
	c := &docCandidate{chunkIdx: 1}
	meta := []docChunkMeta{
		{index: 0, heading: "# Title", byteOffset: 0},
		{index: 1, heading: "## Goals", byteOffset: goalsOffset},
	}

	text, src := docHitTextAndSource(&row, c, meta)
	if text != body[goalsOffset:] {
		t.Fatalf("text = %q, want the Goals chunk", text)
	}
	if src.Path != "notes/foo.md" || src.Heading != "## Goals" {
		t.Fatalf("source = %+v", src)
	}
	// The Goals chunk starts after 4 newlines → line 5.
	if src.LineStart != 5 {
		t.Fatalf("lineStart = %d, want 5", src.LineStart)
	}
}

func TestDocHitTextAndSource_MiddleChunkEndsAtSuccessor(t *testing.T) {
	body := "aaaa\nbbbb\ncccc\n"
	row := docRow{path: "a.md", body: body}
	c := &docCandidate{chunkIdx: 0}
	meta := []docChunkMeta{
		{index: 0, heading: "", byteOffset: 0},
		{index: 1, heading: "", byteOffset: 5},
	}
	text, src := docHitTextAndSource(&row, c, meta)
	if text != "aaaa\n" {
		t.Fatalf("text = %q, want the slice up to the next chunk", text)
	}
	if src.LineStart != 1 {
		t.Fatalf("lineStart = %d, want 1", src.LineStart)
	}
}

func TestDocHitTextAndSource_NoChunkFallsBackToCappedBody(t *testing.T) {
	row := docRow{path: "a.md", body: strings.Repeat("é", docBodyTextCap)} // 2 bytes per rune
	c := &docCandidate{chunkIdx: -1}

	text, src := docHitTextAndSource(&row, c, nil)
	if len(text) > docBodyTextCap {
		t.Fatalf("text length %d exceeds cap %d", len(text), docBodyTextCap)
	}
	// Cap must land on a rune boundary, never splitting the 2-byte é.
	if !strings.HasSuffix(text, "é") {
		t.Fatalf("text does not end on a rune boundary: %q", text[len(text)-4:])
	}
	if src.Path != "a.md" || src.Heading != "" || src.LineStart != 1 {
		t.Fatalf("source = %+v", src)
	}
}

func TestDocHitTextAndSource_CorruptOffsetsClamp(t *testing.T) {
	row := docRow{path: "a.md", body: "short"}
	c := &docCandidate{chunkIdx: 0}
	meta := []docChunkMeta{{index: 0, byteOffset: 999}}

	text, src := docHitTextAndSource(&row, c, meta)
	if text != "" {
		t.Fatalf("text = %q, want empty for an out-of-range offset", text)
	}
	if src.LineStart != 1 {
		t.Fatalf("lineStart = %d", src.LineStart)
	}
}

// TestScoreDocCandidates_SemanticGateSparesNonVecLanes proves the fix for the
// "FTS-only docs are dropped" review finding: the pre-fusion semantic gate must
// only judge candidates the chunk-vec lane actually surfaced (vecRank > 0). A
// document that won purely on FTS or the wikilink graph has a structurally-zero
// rawSem and must NOT be gated out — otherwise the graph lane's zero-overlap
// "note about Rosie" match (and every lexical-only hit) would silently vanish
// whenever an operator enables the gate.
func TestScoreDocCandidates_SemanticGateSparesNonVecLanes(t *testing.T) {
	h := &HybridQuery{
		fusionMode:            FusionModeWeighted,
		semanticGateThreshold: 0.5, // gate ON, above any zero rawSem
		rrfK:                  60,
	}
	cands := map[string]*docCandidate{
		// FTS-only: no vec rank, rawSem is the zero value.
		"DOC_FTS": {uid: "DOC_FTS", ftsRank: 1, rawBM25: 0.7, chunkIdx: -1},
		// graph-only: no vec rank, no rawSem.
		"DOC_GRAPH": {uid: "DOC_GRAPH", graphRank: 1, chunkIdx: -1},
		// vec-lane winner BELOW the gate: must be dropped.
		"DOC_VEC_LOW": {uid: "DOC_VEC_LOW", vecRank: 1, rawSem: 0.1, chunkIdx: 0},
		// vec-lane winner ABOVE the gate: must survive.
		"DOC_VEC_HIGH": {uid: "DOC_VEC_HIGH", vecRank: 2, rawSem: 0.9, chunkIdx: 0},
	}
	rows := map[string]docRow{
		"DOC_FTS":      {uid: "DOC_FTS", path: "fts.md", body: "fts body"},
		"DOC_GRAPH":    {uid: "DOC_GRAPH", path: "graph.md", body: "graph body"},
		"DOC_VEC_LOW":  {uid: "DOC_VEC_LOW", path: "low.md", body: "low body"},
		"DOC_VEC_HIGH": {uid: "DOC_VEC_HIGH", path: "high.md", body: "high body"},
	}

	hits := h.scoreDocCandidates(cands, rows, map[string][]docChunkMeta{})

	got := make(map[string]bool, len(hits))
	for _, hit := range hits {
		got[hit.UID] = true
	}
	for _, want := range []string{"DOC_FTS", "DOC_GRAPH", "DOC_VEC_HIGH"} {
		if !got[want] {
			t.Errorf("candidate %s was dropped; non-vec and above-gate hits must survive", want)
		}
	}
	if got["DOC_VEC_LOW"] {
		t.Errorf("DOC_VEC_LOW (vec-lane, rawSem below gate) must be dropped by the semantic gate")
	}
}

func TestDocHitSnippet(t *testing.T) {
	if got := docHitSnippet("<b>match</b>", "full text", 100); got != "<b>match</b>" {
		t.Fatalf("snippet = %q, want the FTS excerpt to win", got)
	}
	if got := docHitSnippet("", "short", 100); got != "short" {
		t.Fatalf("snippet = %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := docHitSnippet("", long, 200); len([]rune(got)) != 200 {
		t.Fatalf("snippet rune length = %d, want 200", len([]rune(got)))
	}
}
