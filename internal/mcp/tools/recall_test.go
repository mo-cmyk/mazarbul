package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// fakeQuery is a kernel.Query stub that yields a pre-seeded slice of
// (Hit, error) tuples. The captured RecallRequest lets tests assert
// that limit / minConfidence flow through unchanged.
type fakeQuery struct {
	yields []fakeYield
	got    kernel.RecallRequest
	called bool
}

// fakeYield is one (Hit, err) the fake will emit. A non-nil err
// terminates the iterator on that step, matching the real composer's
// semantics.
type fakeYield struct {
	hit kernel.Hit
	err error
}

func (f *fakeQuery) Recall(_ context.Context, req kernel.RecallRequest) iter.Seq2[kernel.Hit, error] {
	f.got = req
	f.called = true
	yields := f.yields
	return func(yield func(kernel.Hit, error) bool) {
		for _, y := range yields {
			if !yield(y.hit, y.err) {
				return
			}
			if y.err != nil {
				return
			}
		}
	}
}

func (f *fakeQuery) Traverse(_ context.Context, _ kernel.TraverseRequest) iter.Seq2[kernel.Node, error] {
	return func(yield func(kernel.Node, error) bool) {
		yield(kernel.Node{}, errors.New("tools: fakeQuery: traverse unused in recall tests"))
	}
}

// fakeReportingQuery implements kernel.ReportingQuery so recall-tool tests can
// exercise the degraded markers (degraded + lanes) without a real composer.
type fakeReportingQuery struct {
	hits   []kernel.Hit
	report kernel.RecallReport
}

func (f *fakeReportingQuery) Recall(_ context.Context, _ kernel.RecallRequest) iter.Seq2[kernel.Hit, error] {
	return func(yield func(kernel.Hit, error) bool) {
		for _, h := range f.hits {
			if !yield(h, nil) {
				return
			}
		}
	}
}

func (f *fakeReportingQuery) Traverse(_ context.Context, _ kernel.TraverseRequest) iter.Seq2[kernel.Node, error] {
	return func(yield func(kernel.Node, error) bool) {
		yield(kernel.Node{}, errors.New("tools: fakeReportingQuery: traverse unused"))
	}
}

func (f *fakeReportingQuery) RecallWithReport(_ context.Context, _ kernel.RecallRequest) ([]kernel.Hit, kernel.RecallReport, error) {
	return f.hits, f.report, nil
}

// TestRecall_Metadata pins the tool's surface metadata. Name and
// Description appear in tools/list, so they are part of the agent-facing
// contract; the schemas drive validation, so a typo breaks discovery
// silently if not asserted.
func TestRecall_Metadata(t *testing.T) {
	t.Parallel()
	r := tools.NewRecall(&fakeQuery{})
	if got, want := r.Name(), "recall"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if r.Description() == "" {
		t.Error("Description() is empty")
	}
	var in map[string]any
	if err := json.Unmarshal(r.InputSchema(), &in); err != nil {
		t.Fatalf("InputSchema unparseable: %v", err)
	}
	if in["additionalProperties"] != false {
		t.Error("InputSchema does not forbid additionalProperties")
	}
	required, _ := in["required"].([]any)
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("InputSchema.required = %v, want [\"query\"]", required)
	}
	var out map[string]any
	if err := json.Unmarshal(r.OutputSchema(), &out); err != nil {
		t.Fatalf("OutputSchema unparseable: %v", err)
	}
}

// TestRecall_HappyPath exercises the projection layer: every kernel.Hit
// field surfaces on the wire under its AIP-named JSON key, with the
// right type and RFC3339Nano timestamp formatting.
func TestRecall_HappyPath(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 5, 24, 12, 0, 0, 123456789, time.UTC)
	fake := &fakeQuery{yields: []fakeYield{
		{hit: kernel.Hit{
			UID:        "01HX-CAT-0000000000000001",
			Kind:       "remember",
			Snippet:    "<b>cat</b> on the rug",
			Confidence: 0.9,
			CreateTime: created,
			FusedScore: 0.42,
			Components: kernel.ScoreComponents{Vector: 0.30, FTS: 0.12, Graph: 0, Recency: 0},
			Citations:  []string{"events/01HX-CAT-0000000000000001"},
		}},
		{hit: kernel.Hit{
			UID:        "01HX-CAT-0000000000000002",
			Snippet:    "purring cat",
			Confidence: 0.8,
			CreateTime: created.Add(time.Second),
			FusedScore: 0.30,
			Components: kernel.ScoreComponents{Vector: 0.20, FTS: 0.10},
			Citations:  []string{"events/01HX-CAT-0000000000000002"},
		}},
	}}

	r := tools.NewRecall(fake)
	resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"cat"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var got struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("hits len = %d, want 2", len(got.Hits))
	}
	first := got.Hits[0]
	if first["name"] != "episodes/01HX-CAT-0000000000000001" {
		t.Errorf("hits[0].name = %v", first["name"])
	}
	if first["snippet"] != "<b>cat</b> on the rug" {
		t.Errorf("hits[0].snippet = %v", first["snippet"])
	}
	if first["createTime"] != "2026-05-24T12:00:00.123456789Z" {
		t.Errorf("hits[0].createTime = %v", first["createTime"])
	}
	comps, _ := first["components"].(map[string]any)
	if comps == nil {
		t.Fatal("hits[0].components missing (explain:true was requested)")
	}
	for _, key := range []string{"vector", "fts", "graph", "recency"} {
		if _, ok := comps[key]; !ok {
			t.Errorf("hits[0].components.%s missing", key)
		}
	}
	citations, _ := first["citations"].([]any)
	if len(citations) != 1 || citations[0] != "events/01HX-CAT-0000000000000001" {
		t.Errorf("hits[0].citations = %v", citations)
	}
	// Citations stay as `events/{uid}` (the underlying event-log
	// reference); only the primary identifier moved to AIP `name`.
}

// TestRecall_DraftScopeReturnsValidationError is the F1 security gate at the
// recall boundary: recall builds its scope set directly (not via scopesFromParam),
// so it carries its own draft rejection. A scopes:["draft"] request must surface
// E_VALIDATION before any query runs — draft staging is reachable only via the
// dedicated draftStage tool, never recall.
func TestRecall_DraftScopeReturnsValidationError(t *testing.T) {
	t.Parallel()
	r := tools.NewRecall(&fakeQuery{})
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"x","scopes":["draft"]}`))
	if err == nil {
		t.Fatal("Invoke succeeded, want E_VALIDATION for scopes:[\"draft\"]")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
	}
}

// TestRecall_DisabledReturnsValidationError pins the issue-#105
// contract: when the daemon was wired with embed=none, the recall tool
// is registered as the disabled variant and every Invoke surfaces an
// E_VALIDATION with the exact recovery hint that names config.toml and
// the re-init command. The wording is part of the wire surface — agents
// match on it to route the user to the fix without further round-trips.
func TestRecall_DisabledReturnsValidationError(t *testing.T) {
	t.Parallel()
	r := tools.NewRecallDisabled()
	if got, want := r.Name(), "recall"; got != want {
		t.Errorf("Name() = %q, want %q (disabled variant must keep the wire name)", got, want)
	}
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err == nil {
		t.Fatal("Invoke succeeded, want E_VALIDATION")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
	}
	// The message must name the cause and the actionable recovery flag
	// (`--embed`, not `--provider`), plus an embedder-free fallback so a stuck
	// agent has a next step. Substring checks keep wording free to evolve.
	for _, want := range []string{"embeddings disabled in config.toml", "maz init --embed=ollama", "maz list episodes"} {
		if !strings.Contains(te.Message, want) {
			t.Errorf("err.Message = %q; missing %q", te.Message, want)
		}
	}
}

// TestRecall_DisabledIgnoresMalformedInput documents that the disabled
// tool short-circuits before JSON decoding: a bad request still surfaces
// the configuration error rather than a confusing decode failure that
// would mislead the user into "fixing" their (perfectly fine) query.
func TestRecall_DisabledIgnoresMalformedInput(t *testing.T) {
	t.Parallel()
	r := tools.NewRecallDisabled()
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"limit":}`))
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
	}
	if !strings.Contains(te.Message, "embeddings disabled") {
		t.Errorf("err.Message = %q; should be the disabled-config message, not a decode error", te.Message)
	}
}

// TestRecall_AgentCompatibleText pins the agent-compatibility contract:
// every hit must expose a clean `text` field with the full episode text,
// stripped of FTS5 HTML highlight markup. Without it, an agent receives
// only the `snippet` (which may carry <b>...</b> tags from FTS5 or be
// truncated to snippetRunes runes), gets confused by the markup, and
// can't reliably ground its answer in the retrieved memory.
//
// Pre-fix behaviour: the response had only `snippet` (HTML-marked or
// truncated). This test fails until kernel.Hit carries the full text
// and projectHit surfaces it on the wire.
func TestRecall_AgentCompatibleText(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeQuery{yields: []fakeYield{
		{hit: kernel.Hit{
			UID:        "01HX-COFFEE-AGENT0000000001",
			Kind:       "remember",
			Text:       "I prefer black coffee in the morning",
			Snippet:    "I prefer black <b>coffee</b> in the morning",
			Confidence: 1.0,
			CreateTime: created,
			FusedScore: 0.42,
			Components: kernel.ScoreComponents{Vector: 0.30, FTS: 0.12},
			Citations:  []string{"events/01HX-COFFEE-AGENT0000000001"},
		}},
	}}

	r := tools.NewRecall(fake)
	resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"coffee"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	var got struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("hits len = %d, want 1", len(got.Hits))
	}
	hit := got.Hits[0]
	text, ok := hit["text"].(string)
	if !ok {
		t.Fatalf("hits[0].text missing or not a string; full hit = %v", hit)
	}
	if want := "I prefer black coffee in the morning"; text != want {
		t.Errorf("hits[0].text = %q, want %q (full episode text, no HTML markup)", text, want)
	}
	if strings.Contains(text, "<b>") || strings.Contains(text, "</b>") {
		t.Errorf("hits[0].text contains FTS markup: %q (agents cannot parse HTML)", text)
	}
}

// TestRecall_EmptyResults guards the agent contract that hits and lanes are
// always arrays (never null) so every client can skip the empty special-case.
// A non-reporting fakeQuery leaves degraded=false and lanes=[].
func TestRecall_EmptyResults(t *testing.T) {
	t.Parallel()
	fake := &fakeQuery{}
	r := tools.NewRecall(fake)
	resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"nothing matches"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := string(resp); got != `{"hits":[],"degraded":false,"lanes":[]}` {
		t.Errorf("response = %q, want %q", got, `{"hits":[],"degraded":false,"lanes":[]}`)
	}
}

// TestRecall_DegradedAdvertisesLanes pins the #236 contract: when the kernel
// reports a degraded (embedder-unavailable) recall, the tool surfaces
// degraded:true and the active lanes alongside the FTS-ranked hits — NOT the
// disabled variant's E_VALIDATION.
func TestRecall_DegradedAdvertisesLanes(t *testing.T) {
	t.Parallel()
	fake := &fakeReportingQuery{
		hits: []kernel.Hit{{
			UID:        "01HX-DOG-0000000000000001",
			Kind:       "remember",
			Text:       "the dog barked",
			Confidence: 0.7,
			FusedScore: 0.1,
			Components: kernel.ScoreComponents{FTS: 0.1},
			Citations:  []string{"events/01HX-DOG-0000000000000001"},
		}},
		report: kernel.RecallReport{Degraded: true, Lanes: []string{kernel.LaneFTS}},
	}
	r := tools.NewRecall(fake)
	resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"dog"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		Hits     []map[string]any `json:"hits"`
		Degraded bool             `json:"degraded"`
		Lanes    []string         `json:"lanes"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !got.Degraded {
		t.Error("degraded = false, want true (embedder unavailable)")
	}
	if len(got.Lanes) != 1 || got.Lanes[0] != "fts" {
		t.Errorf("lanes = %v, want [fts]", got.Lanes)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("hits len = %d, want 1 (FTS hit survives degradation)", len(got.Hits))
	}
}

// TestRecall_SurfacesWarningCodes pins the seam an earlier audit found broken:
// the kernel builds RecallReport.Warnings with stable codes (MAZ-RECALL-001
// degraded, MAZ-RECALL-002 fuzzy fallback), and the recall tool must forward
// them on the wire so an agent can branch on a code, not just the `degraded`
// bool. Covers both the verbose and compact response shapes.
func TestRecall_SurfacesWarningCodes(t *testing.T) {
	t.Parallel()
	mkFake := func() *fakeReportingQuery {
		return &fakeReportingQuery{
			hits: []kernel.Hit{{
				UID:        "01HX-CAT-0000000000000001",
				Kind:       "remember",
				Text:       "the cat purred",
				Confidence: 0.7,
				FusedScore: 0.1,
				Components: kernel.ScoreComponents{FTS: 0.1},
				Citations:  []string{"events/01HX-CAT-0000000000000001"},
			}},
			report: kernel.RecallReport{
				Degraded: true,
				Lanes:    []string{kernel.LaneFTS},
				Warnings: []kernel.RecallWarning{
					{Code: "MAZ-RECALL-001", Message: "embedder timed out"},
					{Code: "MAZ-RECALL-002", Message: "prefix fallback widened the query"},
				},
			},
		}
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"verbose response", `{"query":"cat"}`},
		{"compact response", `{"query":"cat","compact":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tools.NewRecall(mkFake())
			resp, err := r.Invoke(context.Background(), json.RawMessage(tc.body))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			var got struct {
				Warnings []struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"warnings"`
			}
			if err := json.Unmarshal(resp, &got); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			seen := map[string]bool{}
			for _, w := range got.Warnings {
				if w.Message == "" {
					t.Errorf("warning %q has empty message", w.Code)
				}
				seen[w.Code] = true
			}
			for _, want := range []string{"MAZ-RECALL-001", "MAZ-RECALL-002"} {
				if !seen[want] {
					t.Errorf("missing warning code %q on the wire; response = %s", want, resp)
				}
			}
		})
	}
}

// TestRecall_IteratorError pins the failure path: the kernel yielding a
// mid-stream error surfaces as an mcp.ToolError with CodeInternal, and
// the underlying cause stays reachable via errors.Is.
func TestRecall_IteratorError(t *testing.T) {
	t.Parallel()
	boom := errors.New("composer: vector backend down")
	fake := &fakeQuery{yields: []fakeYield{
		{hit: kernel.Hit{UID: "01HX-x"}},
		{err: boom},
	}}
	r := tools.NewRecall(fake)
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err == nil {
		t.Fatal("Invoke succeeded, want error")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeInternal {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeInternal)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err does not wrap the iterator's boom: %v", err)
	}
}

// TestRecall_LimitAndMinConfidencePassThrough pins that the handler
// forwards the caller's limit / minConfidence into the RecallRequest
// verbatim, and that the default of 10 fills in when limit is absent.
func TestRecall_LimitAndMinConfidencePassThrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		body              string
		wantLimit         int
		wantMinConfidence float64
	}{
		{name: "defaults", body: `{"query":"q"}`, wantLimit: 10, wantMinConfidence: 0},
		{name: "explicit limit", body: `{"query":"q","limit":7}`, wantLimit: 7, wantMinConfidence: 0},
		{name: "explicit minConfidence", body: `{"query":"q","minConfidence":0.4}`, wantLimit: 10, wantMinConfidence: 0.4},
		{name: "explicit zero minConfidence", body: `{"query":"q","minConfidence":0}`, wantLimit: 10, wantMinConfidence: 0},
		{name: "both fields", body: `{"query":"q","limit":3,"minConfidence":0.25}`, wantLimit: 3, wantMinConfidence: 0.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeQuery{}
			r := tools.NewRecall(fake)
			if _, err := r.Invoke(context.Background(), json.RawMessage(tc.body)); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !fake.called {
				t.Fatal("kernel.Query.Recall was never called")
			}
			if fake.got.Query != "q" {
				t.Errorf("RecallRequest.Query = %q, want %q", fake.got.Query, "q")
			}
			if fake.got.Limit != tc.wantLimit {
				t.Errorf("RecallRequest.Limit = %d, want %d", fake.got.Limit, tc.wantLimit)
			}
			if fake.got.MinConfidence != tc.wantMinConfidence {
				t.Errorf("RecallRequest.MinConfidence = %v, want %v", fake.got.MinConfidence, tc.wantMinConfidence)
			}
		})
	}
}

// TestRecall_KindsPassThrough proves the public recall tool forwards the
// `kinds` argument into RecallRequest.Kinds, so callers can scope a recall to
// documents only (["documentRevision"]) or exclude them (["episode"]). Without
// this wiring the kernel's document-gating contract is unreachable from MCP.
func TestRecall_KindsPassThrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want []kernel.EventKind
	}{
		{name: "absent → no restriction", body: `{"query":"q"}`, want: nil},
		{name: "document-only", body: `{"query":"q","kinds":["documentRevision"]}`, want: []kernel.EventKind{"documentRevision"}},
		{name: "episode-only", body: `{"query":"q","kinds":["episode"]}`, want: []kernel.EventKind{"episode"}},
		{name: "mixed", body: `{"query":"q","kinds":["episode","documentRevision"]}`, want: []kernel.EventKind{"episode", "documentRevision"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeQuery{}
			r := tools.NewRecall(fake)
			if _, err := r.Invoke(context.Background(), json.RawMessage(tc.body)); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if len(fake.got.Kinds) != len(tc.want) {
				t.Fatalf("RecallRequest.Kinds = %v, want %v", fake.got.Kinds, tc.want)
			}
			for i := range tc.want {
				if fake.got.Kinds[i] != tc.want[i] {
					t.Errorf("RecallRequest.Kinds[%d] = %q, want %q", i, fake.got.Kinds[i], tc.want[i])
				}
			}
		})
	}
}

// TestRecall_SchemaRejection proves the tool's declared input schema is
// well-formed and rejects every malformed argument shape the adapter
// would forward to Invoke. The mcp package's own server tests cover the
// adapter -> validator -> handler wiring; this test pins the schema
// content itself so a typo can't silently disable validation.
func TestRecall_SchemaRejection(t *testing.T) {
	t.Parallel()
	r := tools.NewRecall(&fakeQuery{})
	compiled := compileTestSchema(t, r.InputSchema())

	cases := []struct {
		name string
		body string
	}{
		{name: "missing query", body: `{}`},
		{name: "empty query", body: `{"query":""}`},
		{name: "wrong type", body: `{"query":42}`},
		{name: "limit too large", body: `{"query":"x","limit":101}`},
		{name: "limit zero", body: `{"query":"x","limit":0}`},
		{name: "minConfidence out of range", body: `{"query":"x","minConfidence":1.5}`},
		{name: "maxTokens zero", body: `{"query":"x","maxTokens":0}`},
		{name: "maxTokens negative", body: `{"query":"x","maxTokens":-5}`},
		{name: "maxTokens wrong type", body: `{"query":"x","maxTokens":"big"}`},
		{name: "compact wrong type", body: `{"query":"x","compact":"yes"}`},
		{name: "extra property", body: `{"query":"x","unknown":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(tc.body), &doc); err != nil {
				t.Fatalf("test body unparseable: %v", err)
			}
			if err := compiled.Validate(doc); err == nil {
				t.Errorf("schema accepted %q, expected rejection", tc.body)
			}
		})
	}
}

// TestRecall_SchemaAccepts pairs with SchemaRejection: the schema must
// admit the canonical valid bodies, including the all-defaults case the
// agent will most often send.
func TestRecall_SchemaAccepts(t *testing.T) {
	t.Parallel()
	r := tools.NewRecall(&fakeQuery{})
	compiled := compileTestSchema(t, r.InputSchema())

	cases := []string{
		`{"query":"hello"}`,
		`{"query":"hello","limit":1}`,
		`{"query":"hello","limit":50,"minConfidence":1}`,
		`{"query":"hello","minConfidence":0}`,
		`{"query":"hello","compact":true}`,
		`{"query":"hello","compact":false,"maxTokens":500}`,
		`{"query":"hello","compact":true,"maxTokens":1}`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("test body unparseable: %v", err)
			}
			if err := compiled.Validate(doc); err != nil {
				t.Errorf("schema rejected %q: %v", body, err)
			}
		})
	}
}

// TestRecall_ProjectHitDefaultsHitType verifies that a kernel.Hit with an
// empty HitType (the zero value, from a Query implementation that predates the
// discriminator field) is projected as hitType="episode" on the wire rather
// than the empty string, which would violate the output schema enum.
// Regression test for Greptile finding id=3372597914.
func TestRecall_ProjectHitDefaultsHitType(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// A Hit with zero-value HitType (no field set) — as would be returned
	// by a kernel.Query implementation that predates the discriminator field.
	fake := &fakeQuery{yields: []fakeYield{
		{hit: kernel.Hit{
			// HitType intentionally left as zero value ("").
			UID:        "01ZERO00000000000000000000",
			Text:       "some episode",
			Snippet:    "some episode",
			Confidence: 1.0,
			CreateTime: created,
			FusedScore: 0.5,
			Citations:  []string{"events/01ZERO00000000000000000000"},
			Entities:   []kernel.EntityRef{},
		}},
	}}
	r := tools.NewRecall(fake)
	resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"episode"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		Hits []struct {
			HitType string `json:"hitType"`
			Name    string `json:"name"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("hits len = %d, want 1", len(got.Hits))
	}
	if got.Hits[0].HitType != "episode" {
		t.Errorf("hitType = %q, want %q (default for zero-value HitType)", got.Hits[0].HitType, "episode")
	}
	if want := "episodes/01ZERO00000000000000000000"; got.Hits[0].Name != want {
		t.Errorf("name = %q, want %q", got.Hits[0].Name, want)
	}
}

// TestRecall_ProjectsDerivedHitTypes pins issue #231's recall surfacing: an
// insight hit projects to hitType "insight" / name "insights/{uid}" / kind
// "insightSynthesized"; a consolidated memory to "consolidated" /
// "consolidatedMemories/{uid}" / "memoryConsolidated"; and BOTH carry the
// passive-reinforcement Strength component on the wire. The regressions this
// guards: a derived memory mislabeled as an episode (empty Kind defaulting to
// "episode", name defaulting to "episodes/{uid}"), and the cognitive ranking
// signal silently dropped from the score breakdown so an agent cannot see how
// recall frequency moved the rank.
func TestRecall_ProjectsDerivedHitTypes(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insight := kernel.Hit{
		HitType:    kernel.HitTypeInsight,
		Kind:       kernel.EventKind("insightSynthesized"),
		UID:        "01KVAV11VGX3KYXG96S4J8FKSV",
		Text:       "Aragorn leads Mithril and prefers Rust.",
		Snippet:    "Aragorn leads Mithril and prefers Rust.",
		FusedScore: 0.7,
		Components: kernel.ScoreComponents{Vector: 0.6, RawSemantic: 0.6, Strength: 0.55},
		Citations:  []string{"01KVATEFW32DAB5K04KVH2M60E", "01KVATEG5MG474CD4NCQWK0WYE"},
		CreateTime: created,
	}
	consolidated := kernel.Hit{
		HitType:    kernel.HitTypeConsolidated,
		Kind:       kernel.EventKind("memoryConsolidated"),
		UID:        "01KVAVEWENZKZ1AP9FANVP3SKQ",
		Text:       "The Mithril project migrated Postgres to an event log.",
		Snippet:    "The Mithril project migrated Postgres to an event log.",
		FusedScore: 0.65,
		Components: kernel.ScoreComponents{Vector: 0.5, RawSemantic: 0.5},
		Citations:  []string{"01KVATEGF9GMASAGK75B3DKZWW"},
		CreateTime: created,
	}
	resp := invokeRecallHits(t, `{"query":"mithril"}`, insight, consolidated)
	var got struct {
		Hits []struct {
			HitType    string   `json:"hitType"`
			Name       string   `json:"name"`
			Kind       string   `json:"kind"`
			Citations  []string `json:"citations"`
			Components struct {
				Strength float32 `json:"strength"`
			} `json:"components"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("hits len = %d, want 2", len(got.Hits))
	}
	ins := got.Hits[0]
	if ins.HitType != "insight" || ins.Name != "insights/01KVAV11VGX3KYXG96S4J8FKSV" || ins.Kind != "insightSynthesized" {
		t.Errorf("insight projection = {hitType:%q name:%q kind:%q}, want {insight, insights/01KVAV11VGX3KYXG96S4J8FKSV, insightSynthesized}", ins.HitType, ins.Name, ins.Kind)
	}
	if ins.Components.Strength != 0.55 {
		t.Errorf("insight wire strength = %v, want 0.55 (reinforcement signal must be observable)", ins.Components.Strength)
	}
	if len(ins.Citations) != 2 {
		t.Errorf("insight citations = %v, want 2 source uids", ins.Citations)
	}
	con := got.Hits[1]
	if con.HitType != "consolidated" || con.Name != "consolidatedMemories/01KVAVEWENZKZ1AP9FANVP3SKQ" || con.Kind != "memoryConsolidated" {
		t.Errorf("consolidated projection = {hitType:%q name:%q kind:%q}, want {consolidated, consolidatedMemories/01KVAVEWENZKZ1AP9FANVP3SKQ, memoryConsolidated}", con.HitType, con.Name, con.Kind)
	}
}

// compileTestSchema compiles raw with the same jsonschema/v6 library the
// mcp package uses for production validation, so test assertions track
// production semantics exactly.
func compileTestSchema(t *testing.T, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema unparseable: %v", err)
	}
	const resource = "mem://recall-test/input.json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(resource, doc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	compiled, err := c.Compile(resource)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return compiled
}

// invokeRecallHits runs the recall tool over a fixed set of hits and returns
// the raw JSON response. The fake yields the hits in slice order, and the tool
// preserves that order (it trusts the composer's ranking), so a test can seed
// hits in descending-score order and assert on the wire order directly.
func invokeRecallHits(t *testing.T, body string, hits ...kernel.Hit) []byte {
	t.Helper()
	yields := make([]fakeYield, len(hits))
	for i, h := range hits {
		yields[i] = fakeYield{hit: h}
	}
	r := tools.NewRecall(&fakeQuery{yields: yields})
	resp, err := r.Invoke(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("Invoke(%s): %v", body, err)
	}
	return resp
}

// decodeFirstHit unmarshals the first hit of a recall response into a generic
// map so a test can assert on the presence/absence of individual wire keys.
func decodeFirstHit(t *testing.T, resp []byte) map[string]any {
	t.Helper()
	var got struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Hits) == 0 {
		t.Fatalf("response had no hits: %s", resp)
	}
	return got.Hits[0]
}

// decodeHitNames pulls just the ordered `name` field from each hit, the unit a
// trimming test asserts on.
func decodeHitNames(t *testing.T, resp []byte) []string {
	t.Helper()
	var got struct {
		Hits []struct {
			Name string `json:"name"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	names := make([]string, len(got.Hits))
	for i, h := range got.Hits {
		names[i] = h.Name
	}
	return names
}

// TestRecall_CompactModeReducesPayload pins acceptance criterion 1: compact
// mode yields a measurable byte/token reduction while preserving the entity
// anchors that drive seed-then-traverse. It also logs the verbose-vs-compact
// token delta — the citable token-efficiency metric.
func TestRecall_CompactModeReducesPayload(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	longText := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 40)
	hit := kernel.Hit{
		UID:        "01HXCOMPACT0000000000000001",
		Kind:       "remember",
		Text:       longText,
		Snippet:    "the quick <b>brown</b> fox",
		Confidence: 0.9,
		CreateTime: created,
		FusedScore: 0.42,
		Components: kernel.ScoreComponents{
			Vector: 0.3, FTS: 0.12, Graph: 0.05, NameEmb: 0.01,
			Recency: 0.2, RawSemantic: 0.8, RawBM25: 0.6,
		},
		Citations: []string{"events/01HXCOMPACT0000000000000001"},
		Entities: []kernel.EntityRef{
			{UID: "01HXENT00000000000000000A1", Kind: "person", DisplayName: "Ada"},
		},
	}

	verbose := invokeRecallHits(t, `{"query":"fox"}`, hit)
	compact := invokeRecallHits(t, `{"query":"fox","compact":true}`, hit)

	if len(compact) >= len(verbose) {
		t.Fatalf("compact payload (%d bytes) not smaller than verbose (%d bytes)", len(compact), len(verbose))
	}
	vTok, cTok := (len(verbose)+3)/4, (len(compact)+3)/4
	t.Logf("recall token delta: verbose=%d compact=%d (%.0f%% reduction)",
		vTok, cTok, 100*float64(vTok-cTok)/float64(vTok))

	cHit := decodeFirstHit(t, compact)
	// Heavy verbose-only fields are dropped.
	for _, k := range []string{"text", "components", "confidence", "citations", "createTime", "hitType"} {
		if _, ok := cHit[k]; ok {
			t.Errorf("compact hit unexpectedly carries %q", k)
		}
	}
	// The minimum-to-traverse subset survives.
	for _, k := range []string{"name", "kind", "snippet", "fusedScore", "entities"} {
		if _, ok := cHit[k]; !ok {
			t.Errorf("compact hit missing %q", k)
		}
	}
	if cHit["name"] != "episodes/01HXCOMPACT0000000000000001" {
		t.Errorf("compact name = %v", cHit["name"])
	}
	// Compact snippet is markup-free (derived from clean Text, not the FTS snippet).
	if snip, _ := cHit["snippet"].(string); strings.Contains(snip, "<b>") || strings.Contains(snip, "</b>") {
		t.Errorf("compact snippet carries FTS markup: %q", snip)
	}
	// Entity anchors are byte-identical across modes.
	vEnt, _ := json.Marshal(decodeFirstHit(t, verbose)["entities"])
	cEnt, _ := json.Marshal(cHit["entities"])
	if string(vEnt) != string(cEnt) {
		t.Errorf("entities differ across modes: verbose=%s compact=%s", vEnt, cEnt)
	}
}

// TestRecall_BudgetTrimDropsLowestScoreFirst pins acceptance criterion 2: a
// maxTokens cap trims the lowest-fusedScore hits first, keeps the survivors in
// their original order, and always returns at least the top-ranked hit.
func TestRecall_BudgetTrimDropsLowestScoreFirst(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("padding text to give each hit a real token cost. ", 6)
	scores := []float32{0.91, 0.72, 0.53, 0.34, 0.15}
	hits := make([]kernel.Hit, len(scores))
	for i := range scores {
		uid := fmt.Sprintf("01HXBKT%019d", i) // 26 chars, equal length so per-hit cost is uniform
		hits[i] = kernel.Hit{
			UID: uid, Kind: "remember", Text: pad, Snippet: pad,
			Confidence: 1, CreateTime: time.Unix(0, 0).UTC(), FusedScore: scores[i],
			Citations: []string{"events/" + uid},
		}
	}

	// Baseline: no cap returns all five in descending-score order.
	fullResp := invokeRecallHits(t, `{"query":"q"}`, hits...)
	full := decodeHitNames(t, fullResp)
	if len(full) != 5 {
		t.Fatalf("uncapped hits = %d, want 5", len(full))
	}

	// Cap at roughly half the uncapped payload's estimated tokens.
	half := ((len(fullResp) + 3) / 4) / 2
	capped := decodeHitNames(t, invokeRecallHits(t, fmt.Sprintf(`{"query":"q","maxTokens":%d}`, half), hits...))
	if len(capped) < 1 || len(capped) >= len(full) {
		t.Fatalf("capped hits = %d, want between 1 and %d (trimming should drop the lowest scores)", len(capped), len(full)-1)
	}
	// Prefix property: survivors are exactly the top-K by score, in original order.
	for i := range capped {
		if capped[i] != full[i] {
			t.Errorf("capped[%d] = %q, want %q (lowest-score-first must keep the top prefix in order)", i, capped[i], full[i])
		}
	}

	// The top hit is always returned even when it alone blows the budget.
	one := decodeHitNames(t, invokeRecallHits(t, `{"query":"q","maxTokens":1}`, hits...))
	if len(one) != 1 || one[0] != full[0] {
		t.Fatalf("maxTokens=1 returned %v, want exactly the top hit %q", one, full[0])
	}
}

// TestRecall_CompactProjectionVariants covers the compact projection of the
// non-episode hit shapes: a bare entity hit (name pivots via entities/), a
// document-revision hit (kind + section-anchored source survive compaction),
// and the snippet fallback for a hit with no clean Text (FTS markup stripped).
func TestRecall_CompactProjectionVariants(t *testing.T) {
	t.Parallel()

	t.Run("bare entity hit pivots via entities", func(t *testing.T) {
		t.Parallel()
		hit := kernel.Hit{
			HitType: kernel.HitTypeEntity,
			UID:     "01HXENTITY0000000000000001",
			Kind:    "person",
			Text:    "Ada Lovelace",
			Entities: []kernel.EntityRef{
				{UID: "01HXENTITY0000000000000001", Kind: "person", DisplayName: "Ada Lovelace"},
			},
		}
		cHit := decodeFirstHit(t, invokeRecallHits(t, `{"query":"ada","compact":true}`, hit))
		if cHit["name"] != "entities/01HXENTITY0000000000000001" {
			t.Errorf("name = %v, want entities/{uid}", cHit["name"])
		}
		if cHit["kind"] != "person" {
			t.Errorf("kind = %v, want person", cHit["kind"])
		}
		if cHit["snippet"] != "Ada Lovelace" {
			t.Errorf("snippet = %v, want the entity display name", cHit["snippet"])
		}
	})

	t.Run("document hit keeps section source", func(t *testing.T) {
		t.Parallel()
		hit := kernel.Hit{
			UID:    "01HXDOCUMENT00000000000001",
			Kind:   "documentRevision",
			Text:   "the project goals are ambitious",
			Source: &kernel.SourceLocation{Path: "notes/project.md", Heading: "Goals", LineStart: 12},
		}
		cHit := decodeFirstHit(t, invokeRecallHits(t, `{"query":"goals","compact":true}`, hit))
		if cHit["name"] != "entities/01HXDOCUMENT00000000000001" {
			t.Errorf("name = %v, want entities/{uid} (a document IS an entity)", cHit["name"])
		}
		if cHit["kind"] != "documentRevision" {
			t.Errorf("kind = %v, want documentRevision", cHit["kind"])
		}
		src, ok := cHit["source"].(map[string]any)
		if !ok {
			t.Fatalf("compact document hit dropped source: %v", cHit)
		}
		if src["path"] != "notes/project.md" || src["heading"] != "Goals" {
			t.Errorf("source = %v, want path/heading preserved", src)
		}
		if ls, _ := src["lineStart"].(float64); ls != 12 {
			t.Errorf("source.lineStart = %v, want 12", src["lineStart"])
		}
	})

	t.Run("snippet falls back to stripped FTS markup when text is empty", func(t *testing.T) {
		t.Parallel()
		hit := kernel.Hit{
			UID:     "01HXFALLBACK00000000000001",
			Kind:    "remember",
			Text:    "",
			Snippet: "the quick <b>brown</b> fox",
		}
		cHit := decodeFirstHit(t, invokeRecallHits(t, `{"query":"fox","compact":true}`, hit))
		snip, _ := cHit["snippet"].(string)
		if snip == "" {
			t.Fatal("compact snippet empty; should fall back to the (stripped) FTS snippet")
		}
		if strings.Contains(snip, "<b>") || strings.Contains(snip, "</b>") {
			t.Errorf("compact snippet retained FTS markup: %q", snip)
		}
		if snip != "the quick brown fox" {
			t.Errorf("compact snippet = %q, want the markup-stripped snippet", snip)
		}
	})
}

// TestRecall_CompactWithBudgetCombined pins the canonical token-efficiency
// workflow: compact AND maxTokens together return a strict subset of the
// compact full set, in top-prefix order, with the top hit retained.
func TestRecall_CompactWithBudgetCombined(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("compact budget combined padding text. ", 6)
	scores := []float32{0.9, 0.7, 0.5, 0.3}
	hits := make([]kernel.Hit, len(scores))
	for i := range scores {
		uid := fmt.Sprintf("01HXCMB%019d", i)
		hits[i] = kernel.Hit{
			UID: uid, Kind: "remember", Text: pad, FusedScore: scores[i],
			Entities: []kernel.EntityRef{{UID: uid + "E", Kind: "topic", DisplayName: "t"}},
		}
	}

	fullResp := invokeRecallHits(t, `{"query":"q","compact":true}`, hits...)
	full := decodeHitNames(t, fullResp)
	if len(full) != 4 {
		t.Fatalf("uncapped compact hits = %d, want 4", len(full))
	}

	half := ((len(fullResp) + 3) / 4) / 2
	capped := decodeHitNames(t, invokeRecallHits(t, fmt.Sprintf(`{"query":"q","compact":true,"maxTokens":%d}`, half), hits...))
	if len(capped) < 1 || len(capped) >= len(full) {
		t.Fatalf("compact+budget hits = %d, want between 1 and %d", len(capped), len(full)-1)
	}
	for i := range capped {
		if capped[i] != full[i] {
			t.Errorf("capped[%d] = %q, want %q (top prefix preserved)", i, capped[i], full[i])
		}
	}
}

// TestRecall_BudgetTrimPrefixNotKnapsack pins the score-prefix guarantee under
// HETEROGENEOUS hit costs (the uniform-cost case is covered by
// TestRecall_BudgetTrimDropsLowestScoreFirst). Once a higher-scored hit
// overflows the budget, every lower-scored hit is dropped — even a small one
// that would individually fit in the remaining budget. A knapsack/packing
// trimmer would instead skip the big hit and admit the small tail; this test
// fails for any such future regression.
func TestRecall_BudgetTrimPrefixNotKnapsack(t *testing.T) {
	t.Parallel()
	const tiny = "tiny"
	huge := strings.Repeat("huge middle hit body that blows the token budget. ", 20)
	mk := func(uid string, score float32, text string) kernel.Hit {
		return kernel.Hit{
			UID: uid, Kind: "remember", Text: text, Snippet: text,
			Confidence: 1, CreateTime: time.Unix(0, 0).UTC(), FusedScore: score,
			Citations: []string{"events/" + uid},
		}
	}
	const topName = "episodes/01HXPFX0000000000000000001"
	top := mk("01HXPFX0000000000000000001", 0.9, tiny) // small, top-ranked
	mid := mk("01HXPFX0000000000000000002", 0.5, huge) // large, overflows the budget
	low := mk("01HXPFX0000000000000000003", 0.1, tiny) // small, would fit but must be dropped

	// Budget generous enough to hold the top hit AND the small low-scored tail
	// together (measured from a response containing exactly those two), yet far
	// too small for the huge middle hit. So a knapsack packer could admit
	// {top, low}; the score-prefix rule admits {top} only.
	budget := (len(invokeRecallHits(t, `{"query":"q"}`, top, low)) + 3) / 4

	names := decodeHitNames(t, invokeRecallHits(t,
		fmt.Sprintf(`{"query":"q","maxTokens":%d}`, budget), top, mid, low))
	if len(names) != 1 || names[0] != topName {
		t.Fatalf("budget trim returned %v; want only %q. The low-scored tail hit "+
			"fits the budget but must be dropped once the higher-scored middle hit "+
			"overflows (score-prefix, not knapsack packing)", names, topName)
	}
}

// TestRecall_ExplainMetaShape pins the R2 telemetry contract: when explain=true
// the top-level response carries an optional "_meta" key with took_ms and a
// lanes array of {name, candidates} objects. When explain is absent or false,
// "_meta" must be omitted entirely to preserve the compact envelope.
func TestRecall_ExplainMetaShape(t *testing.T) {
	t.Parallel()

	meta := kernel.RecallMeta{
		TookMs:          42,
		FTSCandidates:   10,
		VecCandidates:   8,
		GraphCandidates: 3,
	}
	fake := &fakeReportingQuery{
		hits: []kernel.Hit{{
			UID: "01HXMETA00000000000000001A", Kind: "remember",
			Text: "some text", FusedScore: 0.5,
		}},
		report: kernel.RecallReport{
			Degraded: false,
			Lanes:    []string{kernel.LaneFTS, kernel.LaneVector, kernel.LaneGraph},
			Meta:     &meta,
		},
	}
	r := tools.NewRecall(fake)

	t.Run("explain true attaches _meta", func(t *testing.T) {
		t.Parallel()
		resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q","explain":true}`))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var got struct {
			Meta *struct {
				TookMs int64 `json:"took_ms"`
				Lanes  []struct {
					Name       string `json:"name"`
					Candidates int    `json:"candidates"`
				} `json:"lanes"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(resp, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Meta == nil {
			t.Fatal("_meta absent; explain:true must attach telemetry")
		}
		if got.Meta.TookMs != 42 {
			t.Errorf("_meta.took_ms = %d, want 42", got.Meta.TookMs)
		}
		// Only lanes with candidates > 0 appear; nameEmb had 0 so only 3.
		if len(got.Meta.Lanes) != 3 {
			t.Errorf("_meta.lanes len = %d, want 3 (fts, vector, graph)", len(got.Meta.Lanes))
		}
		laneNames := make(map[string]int, len(got.Meta.Lanes))
		for _, l := range got.Meta.Lanes {
			laneNames[l.Name] = l.Candidates
		}
		if laneNames["fts"] != 10 {
			t.Errorf("_meta.lanes fts candidates = %d, want 10", laneNames["fts"])
		}
		if laneNames["vector"] != 8 {
			t.Errorf("_meta.lanes vector candidates = %d, want 8", laneNames["vector"])
		}
	})

	t.Run("explain false omits _meta", func(t *testing.T) {
		t.Parallel()
		resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(resp, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, present := got["_meta"]; present {
			t.Error("_meta must be absent when explain is not set")
		}
	})
}

// TestRecall_AsOfEventUID_ValidationErrors pins Fix 3: a malformed ULID or a
// valid-but-nonexistent ULID in asOfEventUID must return E_VALIDATION (not
// E_INTERNAL) with a clean message that does not leak raw parse internals.
func TestRecall_AsOfEventUID_ValidationErrors(t *testing.T) {
	t.Parallel()

	assertValidation := func(t *testing.T, err error, wantSubstring string) {
		t.Helper()
		if err == nil {
			t.Fatal("Invoke succeeded, want E_VALIDATION")
		}
		var te *mcp.ToolError
		if !errors.As(err, &te) {
			t.Fatalf("err type = %T, want *mcp.ToolError", err)
		}
		if te.Code != mcp.CodeValidation {
			t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeValidation)
		}
		if !strings.Contains(te.Message, wantSubstring) {
			t.Errorf("err.Message = %q; want substring %q", te.Message, wantSubstring)
		}
		// Must not leak raw parse internals.
		for _, leak := range []string{"bad data size", "ulid:"} {
			if strings.Contains(te.Message, leak) {
				t.Errorf("err.Message leaks internal error text %q: full message = %q", leak, te.Message)
			}
		}
	}

	t.Run("malformed ULID returns validation error", func(t *testing.T) {
		t.Parallel()
		r := tools.NewRecall(&fakeQuery{}).WithEventLookup(
			&fakeAtLookup{err: fmt.Errorf("%w: uid %q: ulid: bad data size when parsing", kernel.ErrInvalidEvent, "not-a-ulid")},
		)
		_, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q","asOfEventUID":"not-a-ulid"}`))
		assertValidation(t, err, "not a valid event ULID")
	})

	t.Run("nonexistent ULID returns validation error", func(t *testing.T) {
		t.Parallel()
		r := tools.NewRecall(&fakeQuery{}).WithEventLookup(
			&fakeAtLookup{err: kernel.ErrNotFound},
		)
		_, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q","asOfEventUID":"01HXMISSING0000000000000001"}`))
		assertValidation(t, err, "not found")
	})
}

// fakeAtLookup is a minimal EventLookup stub that returns a pre-seeded error or
// a zero Event. Used by TestRecall_AsOfEventUID_ValidationErrors.
type fakeAtLookup struct{ err error }

func (f *fakeAtLookup) At(_ context.Context, _ string) (kernel.Event, error) {
	if f.err != nil {
		return kernel.Event{}, f.err
	}
	return kernel.Event{}, nil
}

// TestRecall_AsOfEventUID_Inclusive pins Fix 2: the named event is included in
// the result set (boundary is inclusive). The kernel's Until filter is exclusive
// (createTime < Until), so applyAsOfEventUID must add 1ns to the event's
// CreateTime before setting kreq.AsOf, making the event's own state visible.
func TestRecall_AsOfEventUID_Inclusive(t *testing.T) {
	t.Parallel()
	eventTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	fake := &fakeQuery{}
	r := tools.NewRecall(fake).WithEventLookup(&fakeTimeLookup{createTime: eventTime})
	_, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q","asOfEventUID":"01HXINCLUSIVE00000000000001"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !fake.called {
		t.Fatal("kernel.Query.Recall was not called")
	}
	// AsOf must be set to eventTime + 1ns (inclusive boundary).
	wantAsOf := eventTime.Add(time.Nanosecond)
	if fake.got.AsOf == nil {
		t.Fatal("RecallRequest.AsOf is nil; expected inclusive boundary")
	}
	if !fake.got.AsOf.Equal(wantAsOf) {
		t.Errorf("RecallRequest.AsOf = %v, want %v (+1ns for inclusive boundary)", fake.got.AsOf, wantAsOf)
	}
}

// fakeTimeLookup is an EventLookup stub that returns an Event with a fixed
// CreateTime. Used by TestRecall_AsOfEventUID_Inclusive.
type fakeTimeLookup struct{ createTime time.Time }

func (f *fakeTimeLookup) At(_ context.Context, _ string) (kernel.Event, error) {
	return kernel.Event{CreateTime: f.createTime}, nil
}

// TestRecall_SurfacesBothDocumentAndDomainEntityHits confirms that when a name
// is shared by a document entity (kind="document") and a non-document entity
// (e.g. kind="product"), recall surfaces BOTH in the result with distinct kind
// labels. This is the observable counterpart of the nameCollisions lint
// category: the collision does not cause either hit to be suppressed — both
// are visible, and the agent can distinguish them by their kind field.
func TestRecall_SurfacesBothDocumentAndDomainEntityHits(t *testing.T) {
	t.Parallel()
	const (
		prodUID = "01HXTRI0000000000000PROD1"
		docUID  = "01HXTRI0000000000000DOCU1"
	)
	productHit := kernel.Hit{
		HitType:    kernel.HitTypeEntity,
		UID:        prodUID,
		Kind:       "product",
		Text:       "Mithrilmail",
		FusedScore: 0.9,
	}
	documentHit := kernel.Hit{
		HitType:    kernel.HitTypeEntity,
		UID:        docUID,
		Kind:       "document",
		Text:       "Mithrilmail",
		FusedScore: 0.8,
	}
	resp := invokeRecallHits(t, `{"query":"Mithrilmail"}`, productHit, documentHit)

	var out struct {
		Hits []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			HitType string `json:"hitType"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Hits) != 2 {
		t.Fatalf("hits = %d, want 2 (both document and product entity must surface)", len(out.Hits))
	}

	// Both are entity-resource hits (entities/ prefix + hitType="entity").
	for i, h := range out.Hits {
		if !strings.HasPrefix(h.Name, "entities/") {
			t.Errorf("hits[%d].name = %q, want entities/ prefix", i, h.Name)
		}
		if h.HitType != "entity" {
			t.Errorf("hits[%d].hitType = %q, want 'entity'", i, h.HitType)
		}
	}

	// The two hits carry distinct kind labels so the agent can tell them apart.
	kinds := map[string]bool{out.Hits[0].Kind: true, out.Hits[1].Kind: true}
	if !kinds["product"] {
		t.Errorf("no hit with kind='product' in output; hits = %+v", out.Hits)
	}
	if !kinds["document"] {
		t.Errorf("no hit with kind='document' in output; hits = %+v", out.Hits)
	}
}
