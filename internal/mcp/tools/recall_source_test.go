package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// fakePrimarySourceQuery is a kernel.Query that also implements
// kernel.PrimarySourceQuery, so recall's has-primary-source filter can be
// exercised without a real composer. It records the UIDs + scopes the filter
// hands it and reports a hit grounded when its UID is in `primary`.
type fakePrimarySourceQuery struct {
	hits      []kernel.Hit
	primary   map[string]bool
	gotUIDs   []string
	gotScopes []string
}

func (f *fakePrimarySourceQuery) Recall(_ context.Context, _ kernel.RecallRequest) iter.Seq2[kernel.Hit, error] {
	return func(yield func(kernel.Hit, error) bool) {
		for _, h := range f.hits {
			if !yield(h, nil) {
				return
			}
		}
	}
}

func (f *fakePrimarySourceQuery) Traverse(_ context.Context, _ kernel.TraverseRequest) iter.Seq2[kernel.Node, error] {
	return func(yield func(kernel.Node, error) bool) {
		yield(kernel.Node{}, errors.New("tools: fakePrimarySourceQuery: traverse unused"))
	}
}

func (f *fakePrimarySourceQuery) SubjectsWithPrimarySource(_ context.Context, uids, scopes []string) (map[string]bool, error) {
	f.gotUIDs = uids
	f.gotScopes = scopes
	out := make(map[string]bool, len(uids))
	for _, u := range uids {
		if f.primary[u] {
			out[u] = true
		}
	}
	return out, nil
}

// epHit is a tiny constructor for an episode hit with a source tag and score.
func epHit(uid, source string, score float32) kernel.Hit {
	return kernel.Hit{
		HitType:     kernel.HitTypeEpisode,
		UID:         uid,
		Kind:        "remember",
		Text:        "body of " + uid,
		Snippet:     "body of " + uid,
		FusedScore:  score,
		EventSource: source,
		Entities:    []kernel.EntityRef{},
	}
}

type recalledHit struct {
	Name        string  `json:"name"`
	EventSource string  `json:"eventSource"`
	FusedScore  float32 `json:"fusedScore"`
}

func decodeRecalledHits(t *testing.T, resp []byte) []recalledHit {
	t.Helper()
	var got struct {
		Hits []recalledHit `json:"hits"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return got.Hits
}

// TestRecall_SourceFilter_KeepsMatchingOnly verifies that --source keeps only
// hits whose EventSource matches (connector-prefix-insensitive), preserves the
// kernel's rank order among survivors, annotates each with eventSource, and
// over-fetches from the kernel so a full page can survive the post-filter.
func TestRecall_SourceFilter_KeepsMatchingOnly(t *testing.T) {
	t.Parallel()
	fake := &fakeQuery{yields: []fakeYield{
		{hit: epHit("g1", "connector:granola", 0.9)},
		{hit: epHit("r1", "remember", 0.8)},
		{hit: epHit("g2", "connector:granola", 0.7)},
	}}
	r := tools.NewRecall(fake)
	resp, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"x","source":["granola"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	hits := decodeRecalledHits(t, resp)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (the two granola hits): %s", len(hits), resp)
	}
	// Rank order preserved: g1 (0.9) before g2 (0.7); r1 dropped.
	if hits[0].Name != "episodes/g1" || hits[1].Name != "episodes/g2" {
		t.Errorf("names = [%q,%q], want [episodes/g1, episodes/g2]", hits[0].Name, hits[1].Name)
	}
	for _, h := range hits {
		if h.EventSource != "connector:granola" {
			t.Errorf("hit %q eventSource = %q, want connector:granola (annotated)", h.Name, h.EventSource)
		}
	}
	// Over-fetch: the kernel limit was bumped above the default page limit of 10.
	if fake.got.Limit <= 10 {
		t.Errorf("kernel request Limit = %d, want over-fetch (> 10) when a source filter is active", fake.got.Limit)
	}
}

// TestRecall_SourceFilter_RankingUnchangedVsUnfiltered proves the post-rank
// filter does not reorder survivors: the granola hits keep the exact relative
// order they had in the unfiltered result.
func TestRecall_SourceFilter_RankingUnchangedVsUnfiltered(t *testing.T) {
	t.Parallel()
	seed := []fakeYield{
		{hit: epHit("g1", "connector:granola", 0.9)},
		{hit: epHit("r1", "remember", 0.8)},
		{hit: epHit("g2", "connector:granola", 0.7)},
		{hit: epHit("r2", "remember", 0.6)},
		{hit: epHit("g3", "connector:granola", 0.5)},
	}
	unfiltered := decodeRecalledHits(t,
		mustInvokeRecall(t, &fakeQuery{yields: seed}, `{"query":"x"}`))
	var wantOrder []string
	for _, h := range unfiltered {
		if h.EventSource == "connector:granola" {
			wantOrder = append(wantOrder, h.Name)
		}
	}
	filtered := decodeRecalledHits(t,
		mustInvokeRecall(t, &fakeQuery{yields: seed}, `{"query":"x","source":["granola"]}`))
	if len(filtered) != len(wantOrder) {
		t.Fatalf("filtered len = %d, want %d", len(filtered), len(wantOrder))
	}
	for i := range filtered {
		if filtered[i].Name != wantOrder[i] {
			t.Errorf("filtered[%d] = %q, want %q (rank order must match unfiltered)", i, filtered[i].Name, wantOrder[i])
		}
	}
}

// TestRecall_SourceFilter_UnknownSourceEmptyNotError pins the contract: an
// unknown source yields an empty hits array, not an error.
func TestRecall_SourceFilter_UnknownSourceEmptyNotError(t *testing.T) {
	t.Parallel()
	fake := &fakeQuery{yields: []fakeYield{
		{hit: epHit("r1", "remember", 0.8)},
	}}
	resp := mustInvokeRecall(t, fake, `{"query":"x","source":["nope-unknown"]}`)
	if hits := decodeRecalledHits(t, resp); len(hits) != 0 {
		t.Errorf("unknown source: hits = %d, want 0; %s", len(hits), resp)
	}
}

// TestRecall_NoSourceFilter_NoOverFetch confirms the kernel limit is left at the
// page limit when no source/has-primary-source filter is active.
func TestRecall_NoSourceFilter_NoOverFetch(t *testing.T) {
	t.Parallel()
	fake := &fakeQuery{}
	if _, err := tools.NewRecall(fake).Invoke(context.Background(), json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.got.Limit != 10 {
		t.Errorf("kernel request Limit = %d, want 10 (default, no over-fetch without a filter)", fake.got.Limit)
	}
}

// TestRecall_HasPrimarySource_KeepsGroundedOnly verifies the has-primary-source
// filter keeps a hit when its own UID OR one of its entity anchors is grounded,
// and drops a hit grounded in neither. It also confirms the request scopes flow
// through to the kernel checker.
func TestRecall_HasPrimarySource_KeepsGroundedOnly(t *testing.T) {
	t.Parallel()
	direct := epHit("a1", "remember", 0.9) // a1 itself grounded
	viaAnchor := kernel.Hit{               // b1 not grounded, but anchor e1 is
		HitType:    kernel.HitTypeEpisode,
		UID:        "b1",
		Kind:       "remember",
		Text:       "anchored",
		Snippet:    "anchored",
		FusedScore: 0.8,
		Entities:   []kernel.EntityRef{{UID: "e1", Kind: "person", DisplayName: "Erik"}},
	}
	ungrounded := epHit("c1", "remember", 0.7)

	fake := &fakePrimarySourceQuery{
		hits:    []kernel.Hit{direct, viaAnchor, ungrounded},
		primary: map[string]bool{"a1": true, "e1": true},
	}
	resp := mustInvokeRecall(t, fake, `{"query":"x","hasPrimarySource":true,"scopes":["project","personal"]}`)
	hits := decodeRecalledHits(t, resp)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (a1 direct + b1 via anchor): %s", len(hits), resp)
	}
	if hits[0].Name != "episodes/a1" || hits[1].Name != "episodes/b1" {
		t.Errorf("names = [%q,%q], want [episodes/a1, episodes/b1]", hits[0].Name, hits[1].Name)
	}
	// Scopes flowed through, and both hit + anchor UIDs were checked.
	if len(fake.gotScopes) != 2 {
		t.Errorf("checker scopes = %v, want [project personal]", fake.gotScopes)
	}
	wantUID := map[string]bool{"a1": true, "b1": true, "e1": true, "c1": true}
	for _, u := range fake.gotUIDs {
		delete(wantUID, u)
	}
	if len(wantUID) != 0 {
		t.Errorf("checker missing candidate UIDs %v (got %v)", wantUID, fake.gotUIDs)
	}
}

// TestRecall_HasPrimarySource_NoCheckerErrors proves that when the wired Query
// does not implement PrimarySourceQuery (a degraded / fake path), a
// has-primary-source request fails loud with E_INTERNAL rather than silently
// returning zero hits — which a caller would misread as "nothing is grounded".
// Production always wires the HybridQuery, so this only fires on misconfiguration.
func TestRecall_HasPrimarySource_NoCheckerErrors(t *testing.T) {
	t.Parallel()
	fake := &fakeQuery{yields: []fakeYield{
		{hit: epHit("r1", "remember", 0.8)},
	}}
	_, err := tools.NewRecall(fake).Invoke(context.Background(),
		json.RawMessage(`{"query":"x","hasPrimarySource":true}`))
	if err == nil {
		t.Fatal("expected an error when the wired Query lacks PrimarySourceQuery, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeInternal {
		t.Errorf("code = %q, want %q (capability gap is a misconfiguration)", te.Code, mcp.CodeInternal)
	}
}

// TestRecall_SourceSchema pins the input schema for the two new flags: source
// must be an array of non-empty strings, hasPrimarySource a boolean.
func TestRecall_SourceSchema(t *testing.T) {
	t.Parallel()
	compiled := compileTestSchema(t, tools.NewRecall(&fakeQuery{}).InputSchema())
	accept := []string{
		`{"query":"x","source":["granola"]}`,
		`{"query":"x","source":["a","connector:b"]}`,
		`{"query":"x","hasPrimarySource":true}`,
		`{"query":"x","source":["a"],"hasPrimarySource":false}`,
	}
	reject := []string{
		`{"query":"x","source":"granola"}`,
		`{"query":"x","source":[""]}`,
		`{"query":"x","source":[1]}`,
		`{"query":"x","hasPrimarySource":"yes"}`,
	}
	for _, body := range accept {
		t.Run("accept "+body, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("body unparseable: %v", err)
			}
			if err := compiled.Validate(doc); err != nil {
				t.Errorf("schema rejected %q: %v", body, err)
			}
		})
	}
	for _, body := range reject {
		t.Run("reject "+body, func(t *testing.T) {
			t.Parallel()
			var doc any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("body unparseable: %v", err)
			}
			if err := compiled.Validate(doc); err == nil {
				t.Errorf("schema accepted %q, expected rejection", body)
			}
		})
	}
}

// mustInvokeRecall invokes recall over q and fails the test on error.
func mustInvokeRecall(t *testing.T, q kernel.Query, body string) []byte {
	t.Helper()
	resp, err := tools.NewRecall(q).Invoke(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("Invoke(%s): %v", body, err)
	}
	return resp
}
