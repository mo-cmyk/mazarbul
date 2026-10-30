package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// captureNav is a configurable Navigator fake for captureExternal tests. It
// embeds the draft-test fakeNav (full interface, ErrNotFound everywhere) and
// overrides the two reads captureExternal actually makes: GetSourceRef (prior
// captures) and GetEntity (external_ref existence).
type captureNav struct {
	*fakeNav
	refs    map[string]navigate.SourceRef // keyed by sourceRef uid
	present map[string]bool               // entity uids that exist
}

func newCaptureNav() *captureNav {
	return &captureNav{fakeNav: &fakeNav{}, refs: map[string]navigate.SourceRef{}, present: map[string]bool{}}
}

func (n *captureNav) GetSourceRef(_ context.Context, uid string) (navigate.SourceRef, error) {
	if sr, ok := n.refs[uid]; ok {
		return sr, nil
	}
	return navigate.SourceRef{}, navigate.ErrNotFound
}

func (n *captureNav) GetEntity(_ context.Context, uid string) (navigate.Entity, error) {
	if n.present[uid] {
		return navigate.Entity{UID: uid}, nil
	}
	return navigate.Entity{}, navigate.ErrNotFound
}

// seedRef records a prior SourceRef under its derived (chunk-0) uid.
func (n *captureNav) seedRef(dedupKey string, sr navigate.SourceRef) {
	n.refs[string(ontology.SourceRefUID(dedupKey+"#0"))] = sr
}

// memSecret is an in-memory secret.Provider so the HMAC freshness token is
// generated and verifiable across calls.
type memSecret struct{ m map[string]string }

func newMemSecret() *memSecret { return &memSecret{m: map[string]string{}} }

func (s *memSecret) Get(_ context.Context, name string) (string, error) {
	if v, ok := s.m[name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}
func (s *memSecret) Set(_ context.Context, name, value string) error { s.m[name] = value; return nil }
func (s *memSecret) Delete(_ context.Context, name string) error     { delete(s.m, name); return nil }

func defaultFreshness() config.FreshnessPolicy {
	return config.ResolveFreshnessPolicy(config.ConnectorsTable{})
}

// eventKinds counts appended events by kind for terse assertions.
func eventKinds(events []kernel.Event) map[string]int {
	counts := map[string]int{}
	for _, e := range events {
		counts[string(e.Kind)]++
	}
	return counts
}

const captureValidBody = `{"uri":"https://notion.so/p/abc","connector":"notion","sourceType":"notion_page","summary":"Spec for the launch","externalId":"abc","contentHash":"` + sixtyFourHexA + `"}`

// sixtyFourHexA is a well-formed 64-char lower-hex contentHash for tests.
const (
	sixtyFourHexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sixtyFourHexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestCaptureExternal_Captured asserts a first capture mints the external_ref,
// the summary episode (tagged connector:<name> for untrusted lineage), the
// SourceRef (carrying a freshness token), and the has_primary_source edge.
func TestCaptureExternal_Captured(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, newCaptureNav(), newMemSecret(), defaultFreshness())

	out, err := c.Invoke(context.Background(), json.RawMessage(captureValidBody))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	counts := eventKinds(app.events)
	if counts[ontology.KindEntity] != 1 {
		t.Errorf("external_ref entity events = %d, want 1", counts[ontology.KindEntity])
	}
	if counts[ontology.KindEpisode] != 1 {
		t.Errorf("episode events = %d, want 1", counts[ontology.KindEpisode])
	}
	if counts[ontology.KindSourceRefRecorded] != 1 {
		t.Errorf("sourceRef events = %d, want 1", counts[ontology.KindSourceRefRecorded])
	}
	if counts[ontology.KindRelation] != 1 {
		t.Errorf("has_primary_source edge events = %d, want 1", counts[ontology.KindRelation])
	}

	var resp struct {
		Action          string `json:"action"`
		ExternalRefName string `json:"externalRefName"`
		Freshness       string `json:"freshness"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode response: %v", uerr)
	}
	if resp.Action != "captured" {
		t.Errorf("action = %q, want captured", resp.Action)
	}
	if resp.Freshness != config.FreshnessFresh {
		t.Errorf("freshness = %q, want fresh", resp.Freshness)
	}

	// The summary episode must carry the connector lineage so it lands untrusted.
	ep := findEvent(t, app.events, ontology.KindEpisode)
	if ep.Source != ontology.ConnectorSourceColonPrefix+"notion" {
		t.Errorf("episode source = %q, want connector:notion", ep.Source)
	}
	// The SourceRef must carry the HMAC freshness token.
	srEvt := findEvent(t, app.events, ontology.KindSourceRefRecorded)
	var sr ontology.SourceRefRecordedEvent
	if uerr := json.Unmarshal(srEvt.Payload, &sr); uerr != nil {
		t.Fatalf("decode sourceRef: %v", uerr)
	}
	if sr.FreshnessToken == "" {
		t.Error("sourceRef freshnessToken is empty, want an HMAC binding")
	}
	if sr.DedupKey != "notion:abc:personal#0" {
		t.Errorf("dedupKey = %q, want minted notion:abc:personal#0", sr.DedupKey)
	}
}

// TestCaptureExternal_ValidationErrors asserts each malformed request maps to the
// expected error code and never appends.
func TestCaptureExternal_ValidationErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body string
		code mcp.ToolErrorCode
	}{
		"missing uri":         {`{"connector":"notion","sourceType":"notion_page","summary":"x"}`, mcp.CodeValidation},
		"missing connector":   {`{"uri":"https://n.so/a","sourceType":"notion_page","summary":"x","externalId":"a"}`, mcp.CodeValidation},
		"missing sourceType":  {`{"uri":"https://n.so/a","connector":"notion","summary":"x","externalId":"a"}`, mcp.CodeValidation},
		"project scope":       {`{"uri":"https://n.so/a","connector":"notion","sourceType":"notion_page","summary":"x","externalId":"a","scope":"project"}`, mcp.CodeValidation},
		"ephemeral connector": {`{"uri":"https://m.so/a","connector":"gmail","sourceType":"email","summary":"x","externalId":"a"}`, mcp.CodeValidation},
		"missing summary":     {`{"uri":"https://n.so/a","connector":"notion","sourceType":"notion_page","externalId":"a"}`, mcp.CodeValidation},
		"no dedup anchor":     {`{"uri":"https://n.so/a","connector":"notion","sourceType":"notion_page","summary":"x"}`, mcp.CodeValidation},
		"bad status":          {`{"uri":"https://n.so/a","connector":"notion","sourceType":"notion_page","summary":"x","externalId":"a","status":"bogus"}`, mcp.CodeValidation},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := &captureRelateAppender{}
			c := tools.NewCaptureExternal(app, newCaptureNav(), newMemSecret(), defaultFreshness())
			_, err := c.Invoke(context.Background(), json.RawMessage(tc.body))
			wantRelateCode(t, err, tc.code)
			if len(app.events) != 0 {
				t.Errorf("appended %d events on a validation failure, want 0", len(app.events))
			}
		})
	}
}

// TestCaptureExternal_SkippedWhenUnchanged asserts a re-capture with the same
// contentHash within the connector TTL is an idempotent no-op (no append).
func TestCaptureExternal_SkippedWhenUnchanged(t *testing.T) {
	t.Parallel()
	nav := newCaptureNav()
	nav.seedRef("notion:abc:personal", navigate.SourceRef{
		SubjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ContentHash: sixtyFourHexA,
		FetchTime:   time.Now().UTC().Format(time.RFC3339Nano),
		Connector:   "notion",
	})
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, nav, newMemSecret(), defaultFreshness())

	out, err := c.Invoke(context.Background(), json.RawMessage(captureValidBody))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 0 {
		t.Errorf("appended %d events on an unchanged re-capture, want 0 (skipped)", len(app.events))
	}
	if action := decodeAction(t, out); action != "skipped" {
		t.Errorf("action = %q, want skipped", action)
	}
}

// TestCaptureExternal_RevisionOnContentChange asserts a changed contentHash mints
// a fresh episode and retracts the prior episode's primary-source edge so it
// supersedes.
func TestCaptureExternal_RevisionOnContentChange(t *testing.T) {
	t.Parallel()
	nav := newCaptureNav()
	nav.seedRef("notion:abc:personal", navigate.SourceRef{
		SubjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ContentHash: sixtyFourHexB, // different from the request's hash
		FetchTime:   time.Now().UTC().Format(time.RFC3339Nano),
		Connector:   "notion",
	})
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, nav, newMemSecret(), defaultFreshness())

	out, err := c.Invoke(context.Background(), json.RawMessage(captureValidBody))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	counts := eventKinds(app.events)
	if counts[ontology.KindEpisode] != 1 {
		t.Errorf("episode events = %d, want 1 (new revision)", counts[ontology.KindEpisode])
	}
	if counts[ontology.KindRelationRetracted] != 1 {
		t.Errorf("relationRetracted events = %d, want 1 (prior edge closed)", counts[ontology.KindRelationRetracted])
	}
	if action := decodeAction(t, out); action != "revision" {
		t.Errorf("action = %q, want revision", action)
	}
}

// TestCaptureExternal_RevisionOnTokenMismatch asserts that a re-capture whose
// contentHash matches the prior but whose HMAC freshness token does NOT (a
// replayed or forged hash) is treated as a revision, not an idempotent skip:
// this is the anti-replay guard. It mints a fresh episode and retracts the prior
// edge even though the hash and the fetch time would otherwise say skip.
func TestCaptureExternal_RevisionOnTokenMismatch(t *testing.T) {
	t.Parallel()
	nav := newCaptureNav()
	nav.seedRef("notion:abc:personal", navigate.SourceRef{
		SubjectUID:     "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ContentHash:    sixtyFourHexA,                             // same hash as the request
		FetchTime:      time.Now().UTC().Format(time.RFC3339Nano), // within TTL: isolates the token path
		Connector:      "notion",
		FreshnessToken: "forged-token-not-the-real-hmac", // mismatches the computed HMAC
	})
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, nav, newMemSecret(), defaultFreshness())

	out, err := c.Invoke(context.Background(), json.RawMessage(captureValidBody))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if action := decodeAction(t, out); action != "revision" {
		t.Errorf("action = %q, want revision (token mismatch despite matching hash and fresh fetch)", action)
	}
	counts := eventKinds(app.events)
	if counts[ontology.KindEpisode] != 1 {
		t.Errorf("episode events = %d, want 1 (fresh revision)", counts[ontology.KindEpisode])
	}
	if counts[ontology.KindRelationRetracted] != 1 {
		t.Errorf("relationRetracted events = %d, want 1 (prior edge closed)", counts[ontology.KindRelationRetracted])
	}
}

// TestCaptureExternal_ExpiredRestampsPriorEpisode asserts that a re-capture with
// the same contentHash but a prior fetch time past the connector TTL re-stamps the
// existing episode's SourceRef (new fetch time) WITHOUT minting a new episode or
// edge, reusing the prior episode uid.
func TestCaptureExternal_ExpiredRestampsPriorEpisode(t *testing.T) {
	t.Parallel()
	nav := newCaptureNav()
	nav.present[string(ontology.ExternalRefUID("personal", "https://notion.so/p/abc"))] = true
	nav.seedRef("notion:abc:personal", navigate.SourceRef{
		SubjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ContentHash: sixtyFourHexA,                                                  // same as the request -> not a revision
		FetchTime:   time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano), // past notion's 24h TTL
		Connector:   "notion",
	})
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, nav, newMemSecret(), defaultFreshness())

	out, err := c.Invoke(context.Background(), json.RawMessage(captureValidBody))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if action := decodeAction(t, out); action != "expired" {
		t.Errorf("action = %q, want expired", action)
	}
	counts := eventKinds(app.events)
	if counts[ontology.KindEpisode] != 0 {
		t.Errorf("episode events = %d, want 0 (expired re-stamps, mints no episode)", counts[ontology.KindEpisode])
	}
	if counts[ontology.KindRelation] != 0 {
		t.Errorf("edge events = %d, want 0 (expired re-stamps, mints no edge)", counts[ontology.KindRelation])
	}
	if counts[ontology.KindSourceRefRecorded] != 1 {
		t.Errorf("sourceRef events = %d, want 1 (the fetch-time re-stamp)", counts[ontology.KindSourceRefRecorded])
	}
	srEvt := findEvent(t, app.events, ontology.KindSourceRefRecorded)
	var sr ontology.SourceRefRecordedEvent
	if uerr := json.Unmarshal(srEvt.Payload, &sr); uerr != nil {
		t.Fatalf("decode sourceRef: %v", uerr)
	}
	if string(sr.SubjectUID) != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("re-stamped SourceRef SubjectUID = %q, want the reused prior episode uid", sr.SubjectUID)
	}
}

// TestCaptureExternal_LiveRecaptureClearsTombstone proves that re-capturing a
// previously tombstoned item live (same content, still within the TTL) does NOT
// skip: it re-stamps the SourceRef with a cleared tombstone so recall stops
// reporting access-unverified once the item is reachable again.
func TestCaptureExternal_LiveRecaptureClearsTombstone(t *testing.T) {
	t.Parallel()
	nav := newCaptureNav()
	nav.present[string(ontology.ExternalRefUID("personal", "https://notion.so/p/abc"))] = true
	nav.seedRef("notion:abc:personal", navigate.SourceRef{
		SubjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ContentHash: sixtyFourHexA,                                                 // same as the request -> not a revision
		FetchTime:   time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339Nano), // within notion's 24h TTL -> would skip if not tombstoned
		Connector:   "notion",
		Tombstone:   ontology.TombstoneAccessRevoked, // prior snapshot was 403'd
	})
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, nav, newMemSecret(), defaultFreshness())

	out, err := c.Invoke(context.Background(), json.RawMessage(captureValidBody))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// Same hash + within TTL would normally skip; the prior tombstone overrides that.
	if action := decodeAction(t, out); action != "expired" {
		t.Errorf("action = %q, want expired (tombstone forces a clearing re-stamp, not skip)", action)
	}
	counts := eventKinds(app.events)
	if counts[ontology.KindEpisode] != 0 {
		t.Errorf("episode events = %d, want 0 (re-stamp clears the tombstone, mints no episode)", counts[ontology.KindEpisode])
	}
	if counts[ontology.KindSourceRefRecorded] != 1 {
		t.Errorf("sourceRef events = %d, want 1 (the tombstone-clearing re-stamp)", counts[ontology.KindSourceRefRecorded])
	}
	srEvt := findEvent(t, app.events, ontology.KindSourceRefRecorded)
	var sr ontology.SourceRefRecordedEvent
	if uerr := json.Unmarshal(srEvt.Payload, &sr); uerr != nil {
		t.Fatalf("decode sourceRef: %v", uerr)
	}
	if sr.Tombstone != "" {
		t.Errorf("re-stamped SourceRef Tombstone = %q, want empty (cleared)", sr.Tombstone)
	}
	if string(sr.SubjectUID) != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("re-stamped SourceRef SubjectUID = %q, want the reused prior episode uid", sr.SubjectUID)
	}
}

// TestCaptureExternal_ExpiredGrownChunksMintMissing is the regression test for the
// dangling-SourceRef bug: when an expired re-capture supplies MORE chunks than the
// prior capture, each chunk index with no prior SourceRef must be minted in full
// (episode + SourceRef + edge), never as a SourceRef whose subject_uid points at an
// episode that does not exist.
func TestCaptureExternal_ExpiredGrownChunksMintMissing(t *testing.T) {
	t.Parallel()
	nav := newCaptureNav()
	nav.present[string(ontology.ExternalRefUID("personal", "https://notion.so/p/abc"))] = true
	// Prior capture recorded chunk #0 only, same hash but past the TTL.
	nav.seedRef("notion:abc:personal", navigate.SourceRef{
		SubjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ContentHash: sixtyFourHexA,
		FetchTime:   time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano),
		Connector:   "notion",
	})
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, nav, newMemSecret(), defaultFreshness())

	// Re-capture with TWO chunks (the prior had one), same hash, past the TTL -> expired.
	body := `{"uri":"https://notion.so/p/abc","connector":"notion","sourceType":"notion_page","externalId":"abc","contentHash":"` + sixtyFourHexA + `","summaries":["First chunk","Second chunk"]}`
	out, err := c.Invoke(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if action := decodeAction(t, out); action != "expired" {
		t.Errorf("action = %q, want expired", action)
	}
	counts := eventKinds(app.events)
	// Chunk #0 re-stamps (SourceRef only); chunk #1 has no prior, so it mints a full
	// episode + SourceRef + edge. Total: 1 episode, 2 sourceRefs, 1 edge.
	if counts[ontology.KindEpisode] != 1 {
		t.Errorf("episode events = %d, want 1 (the grown chunk gets a real episode, not a dangling SourceRef)", counts[ontology.KindEpisode])
	}
	if counts[ontology.KindRelation] != 1 {
		t.Errorf("edge events = %d, want 1 (the grown chunk's has_primary_source edge)", counts[ontology.KindRelation])
	}
	if counts[ontology.KindSourceRefRecorded] != 2 {
		t.Errorf("sourceRef events = %d, want 2 (re-stamp + new chunk)", counts[ontology.KindSourceRefRecorded])
	}
	// Prove no dangling: every NEW SourceRef (not the reused prior) points at an
	// episode uid that is actually present in this batch.
	episodeUIDs := map[string]bool{}
	for _, e := range app.events {
		if string(e.Kind) == ontology.KindEpisode {
			episodeUIDs[e.UID] = true
		}
	}
	for _, e := range app.events {
		if string(e.Kind) != ontology.KindSourceRefRecorded {
			continue
		}
		var sr ontology.SourceRefRecordedEvent
		if uerr := json.Unmarshal(e.Payload, &sr); uerr != nil {
			t.Fatalf("decode sourceRef: %v", uerr)
		}
		subj := string(sr.SubjectUID)
		if subj == "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
			continue // chunk #0 re-stamp reuses the prior episode (exists from before)
		}
		if !episodeUIDs[subj] {
			t.Errorf("SourceRef %q points at episode %q with no backing episode event in the batch (dangling)", sr.DedupKey, subj)
		}
	}
}

// TestCaptureExternal_Tombstone asserts status=deleted re-records the prior
// SourceRef with a tombstone (no new episode) and that tombstoning an
// uncaptured item is E_NOT_FOUND.
func TestCaptureExternal_Tombstone(t *testing.T) {
	t.Parallel()
	nav := newCaptureNav()
	nav.seedRef("notion:abc:personal", navigate.SourceRef{
		SubjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ContentHash: sixtyFourHexA,
		FetchTime:   time.Now().UTC().Format(time.RFC3339Nano),
		Connector:   "notion",
		SourceType:  "notion_page",
	})
	app := &captureRelateAppender{}
	c := tools.NewCaptureExternal(app, nav, newMemSecret(), defaultFreshness())

	body := `{"uri":"https://notion.so/p/abc","connector":"notion","sourceType":"notion_page","externalId":"abc","status":"deleted"}`
	out, err := c.Invoke(context.Background(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	counts := eventKinds(app.events)
	if counts[ontology.KindEpisode] != 0 {
		t.Errorf("episode events = %d on a tombstone, want 0 (no new episode)", counts[ontology.KindEpisode])
	}
	if counts[ontology.KindSourceRefRecorded] != 1 {
		t.Errorf("sourceRef events = %d, want 1 (re-recorded with tombstone)", counts[ontology.KindSourceRefRecorded])
	}
	srEvt := findEvent(t, app.events, ontology.KindSourceRefRecorded)
	var sr ontology.SourceRefRecordedEvent
	if uerr := json.Unmarshal(srEvt.Payload, &sr); uerr != nil {
		t.Fatalf("decode sourceRef: %v", uerr)
	}
	if sr.Tombstone != ontology.TombstoneDeleted {
		t.Errorf("tombstone = %q, want %q", sr.Tombstone, ontology.TombstoneDeleted)
	}
	if action := decodeAction(t, out); action != "tombstoned" {
		t.Errorf("action = %q, want tombstoned", action)
	}

	// Tombstoning an item that was never captured is E_NOT_FOUND.
	app2 := &captureRelateAppender{}
	c2 := tools.NewCaptureExternal(app2, newCaptureNav(), newMemSecret(), defaultFreshness())
	_, err2 := c2.Invoke(context.Background(), json.RawMessage(body))
	wantRelateCode(t, err2, mcp.CodeNotFound)
	if len(app2.events) != 0 {
		t.Errorf("appended %d events tombstoning an uncaptured item, want 0", len(app2.events))
	}
}

// findEvent returns the first appended event of the given kind, failing if none.
func findEvent(t *testing.T, events []kernel.Event, kind string) kernel.Event {
	t.Helper()
	for _, e := range events {
		if string(e.Kind) == kind {
			return e
		}
	}
	t.Fatalf("no %q event among %d appended", kind, len(events))
	return kernel.Event{}
}

// decodeAction extracts the action field from a captureExternal response.
func decodeAction(t *testing.T, out json.RawMessage) string {
	t.Helper()
	var resp struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.Action
}
