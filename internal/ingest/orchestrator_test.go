package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// --- test doubles ---------------------------------------------------------

// fakeAppender records every Append batch verbatim and can be made to error.
type fakeAppender struct {
	mu      sync.Mutex
	batches [][]kernel.Event
	err     error
}

func (f *fakeAppender) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]kernel.Event, len(events))
	copy(cp, events)
	f.batches = append(f.batches, cp)
	return make([]kernel.Receipt, len(events)), nil
}

// episodeEvents flattens every appended episode event across all batches.
func (f *fakeAppender) eventsOfKind(kind string) []kernel.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []kernel.Event
	for bi := range f.batches {
		b := f.batches[bi]
		for i := range b {
			if string(b[i].Kind) == kind {
				out = append(out, b[i])
			}
		}
	}
	return out
}

// fakeReader is an in-memory Reader: dedupKey->hash and (connector|stream)->wm.
type fakeReader struct {
	hashes map[string]string
	wms    map[string]Watermark
}

func newFakeReader() *fakeReader {
	return &fakeReader{hashes: map[string]string{}, wms: map[string]Watermark{}}
}

func (r *fakeReader) SourceRefHash(_ context.Context, dedupKey string) (string, bool, error) {
	h, ok := r.hashes[dedupKey]
	return h, ok, nil
}

func (r *fakeReader) Watermark(_ context.Context, connector, stream string) (Watermark, error) {
	if wm, ok := r.wms[connector+"|"+stream]; ok {
		return wm, nil
	}
	return Watermark{Connector: connector, Stream: stream}, nil
}

// fakeSource yields a fixed queue of batches per stream.
type fakeSource struct {
	name        string
	streams     []string
	batches     map[string][]Batch
	pullErr     map[string]error
	validateErr error
	pulls       map[string]int
}

func (s *fakeSource) Name() string      { return s.name }
func (s *fakeSource) Streams() []string { return s.streams }
func (s *fakeSource) Validate(context.Context) error {
	return s.validateErr
}

func (s *fakeSource) Pull(_ context.Context, stream string, wm Watermark, _ int) (Batch, error) {
	if err := s.pullErr[stream]; err != nil {
		return Batch{}, err
	}
	if s.pulls == nil {
		s.pulls = map[string]int{}
	}
	i := s.pulls[stream]
	s.pulls[stream]++
	q := s.batches[stream]
	if i >= len(q) {
		// Drained: a terminal no-advance batch the orchestrator accepts as Done.
		return Batch{NextCursor: json.RawMessage(`{}`), NextSequence: wm.Sequence, Done: true}, nil
	}
	return q[i], nil
}

// --- helpers --------------------------------------------------------------

const testConnector = "local-git"

// newTestOrchestrator wires an orchestrator with deterministic UID/time so
// appended payloads are stable. Returns the orchestrator and the appender.
func newTestOrchestrator(t *testing.T, reader Reader) (*Orchestrator, *fakeAppender) {
	t.Helper()
	app := &fakeAppender{}
	o := NewOrchestrator(app, reader, jobs.NewRegistry(), nil)
	var n int
	o.newUID = func() (string, error) {
		n++
		// Valid 26-char Crockford ULID-shaped ids, distinct per call.
		return "00000000000000000000000A" + string(rune('A'+n/10)) + string(rune('A'+n%10)), nil
	}
	o.now = func() time.Time { return time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC) }
	return o, app
}

func item(dedup, hash, body string) Item {
	return Item{
		DedupKey:    dedup,
		ExternalID:  dedup,
		SourceType:  "git_commit",
		ContentHash: hash,
		Body:        body,
		FetchTime:   time.Date(2026, 6, 17, 11, 0, 0, 0, time.UTC),
	}
}

func oneBatch(items ...Item) Batch {
	return Batch{
		Items:        items,
		NextCursor:   json.RawMessage(`{"n":1}`),
		NextSequence: uint64(len(items)),
		Done:         true,
	}
}

// --- tests ----------------------------------------------------------------

func TestRun_AppliesItemsAsEpisodeAndSourceRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(
			item("git-commit:a", "hasha", "first commit"),
			item("git-commit:b", "hashb", "second commit"),
		)}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())

	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != 2 || report.ItemsSkipped != 0 {
		t.Fatalf("report applied=%d skipped=%d, want 2/0", report.ItemsApplied, report.ItemsSkipped)
	}

	eps := app.eventsOfKind(ontology.KindEpisode)
	if len(eps) != 2 {
		t.Fatalf("episodes appended = %d, want 2", len(eps))
	}
	refs := app.eventsOfKind(ontology.KindSourceRefRecorded)
	if len(refs) != 2 {
		t.Fatalf("sourceRefs appended = %d, want 2", len(refs))
	}

	// Episode + SourceRef for one item must share an Append batch (atomic emit).
	app.mu.Lock()
	defer app.mu.Unlock()
	var sawAtomicPair bool
	for _, b := range app.batches {
		if len(b) == 2 &&
			string(b[0].Kind) == ontology.KindEpisode &&
			string(b[1].Kind) == ontology.KindSourceRefRecorded {
			sawAtomicPair = true
		}
	}
	if !sawAtomicPair {
		t.Fatalf("expected an atomic [episode, sourceRef] batch; batches=%v", app.batches)
	}
}

func TestRun_TagsEpisodeUntrustedAndCapsConfidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(item("git-commit:a", "h", "hello world"))}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())

	if _, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	eps := app.eventsOfKind(ontology.KindEpisode)
	if len(eps) != 1 {
		t.Fatalf("episodes = %d, want 1", len(eps))
	}
	ev := eps[0]
	if want := ontology.ConnectorSourceColonPrefix + testConnector; ev.Source != want {
		t.Errorf("event Source = %q, want %q", ev.Source, want)
	}
	if ev.Confidence != 1.0 {
		t.Errorf("event Confidence = %v, want 1.0 (trust via connector source prefix, not a capped score)", ev.Confidence)
	}
	if ev.Scope != kernel.ScopePersonal {
		t.Errorf("event Scope = %q, want personal", ev.Scope)
	}
	var ep ontology.Episode
	if err := json.Unmarshal(ev.Payload, &ep); err != nil {
		t.Fatalf("unmarshal episode: %v", err)
	}
	if !strings.Contains(ep.Text, "UNTRUSTED") || !strings.Contains(ep.Text, "hello world") {
		t.Errorf("episode text not fenced: %q", ep.Text)
	}
	if ep.Confidence != 1.0 {
		t.Errorf("episode payload confidence = %v, want 1.0 (trust via source prefix, not a cap)", ep.Confidence)
	}
	if ep.Actor != ontology.ActorSystem {
		t.Errorf("episode actor = %q, want system", ep.Actor)
	}
}

func TestRun_ScrubsSecretsBeforeAppend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(
			item("git-commit:a", "h", "token is "+secret+" do not leak"))}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())
	if _, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var ep ontology.Episode
	if err := json.Unmarshal(app.eventsOfKind(ontology.KindEpisode)[0].Payload, &ep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.Contains(ep.Text, secret) {
		t.Fatalf("secret survived into episode text: %q", ep.Text)
	}
	if !strings.Contains(ep.Text, secretRedaction) {
		t.Errorf("expected redaction marker in %q", ep.Text)
	}
}

func TestRun_DedupSkipsUnchangedItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reader := newFakeReader()
	reader.hashes["git-commit:a"] = "same" // already ingested, identical hash
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(item("git-commit:a", "same", "body"))}},
	}
	o, app := newTestOrchestrator(t, reader)
	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != 0 || report.ItemsSkipped != 1 {
		t.Fatalf("applied=%d skipped=%d, want 0/1", report.ItemsApplied, report.ItemsSkipped)
	}
	if got := len(app.eventsOfKind(ontology.KindEpisode)); got != 0 {
		t.Fatalf("episodes appended = %d, want 0 (deduped)", got)
	}
}

func TestRun_ReingestsChangedContentHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reader := newFakeReader()
	reader.hashes["git-commit:a"] = "old" // present but DIFFERENT hash -> revision
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(item("git-commit:a", "new", "amended body"))}},
	}
	o, app := newTestOrchestrator(t, reader)
	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != 1 {
		t.Fatalf("applied=%d, want 1 (changed hash re-ingests)", report.ItemsApplied)
	}
	if got := len(app.eventsOfKind(ontology.KindEpisode)); got != 1 {
		t.Fatalf("episodes = %d, want 1", got)
	}
}

func TestRun_DryRunAppendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(
			item("git-commit:a", "h1", "one"), item("git-commit:b", "h2", "two"))}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())
	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.DryRun || report.ItemsApplied != 2 {
		t.Fatalf("dryRun=%v applied=%d, want true/2 (preview counts)", report.DryRun, report.ItemsApplied)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.batches) != 0 {
		t.Fatalf("dry run appended %d batches, want 0", len(app.batches))
	}
}

func TestRun_RefusesProjectScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{name: testConnector, streams: []string{"commits"}}
	o, app := newTestOrchestrator(t, newFakeReader())
	_, err := o.Run(ctx, src, kernel.ScopeProject, Bounds{}, false)
	if err == nil {
		t.Fatal("Run with project scope: want error, got nil")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("error = %q, want mention of project scope", err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.batches) != 0 {
		t.Fatalf("project-scope run appended %d batches, want 0", len(app.batches))
	}
}

func TestRun_FailsJobOnNonMonotonicCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {{
			Items:        []Item{item("git-commit:a", "h", "body")},
			NextCursor:   json.RawMessage(`{"n":0}`),
			NextSequence: 0, // not > prev (0) and Done=false -> non-monotonic
			Done:         false,
		}}},
	}
	o, _ := newTestOrchestrator(t, newFakeReader())
	_, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err == nil {
		t.Fatal("Run with non-monotonic cursor: want fatal error, got nil")
	}
	if !strings.Contains(err.Error(), "cursor") {
		t.Errorf("error = %q, want cursor validation failure", err)
	}
}

func TestRun_FailsJobOnOversizedCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	big := `{"x":"` + strings.Repeat("a", ontology.MaxCursorBytes+10) + `"}`
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {{
			Items:        []Item{item("git-commit:a", "h", "body")},
			NextCursor:   json.RawMessage(big),
			NextSequence: 1,
			Done:         true,
		}}},
	}
	o, _ := newTestOrchestrator(t, newFakeReader())
	if _, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false); err == nil {
		t.Fatal("Run with oversized cursor: want fatal error, got nil")
	}
}

func TestRun_HonorsMaxItemsBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {Batch{
			Items: []Item{
				item("git-commit:a", "1", "a"),
				item("git-commit:b", "2", "b"),
				item("git-commit:c", "3", "c"),
			},
			NextCursor:   json.RawMessage(`{"n":3}`),
			NextSequence: 3,
			Done:         true,
		}}},
	}
	o, _ := newTestOrchestrator(t, newFakeReader())
	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{MaxItems: 2}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != 2 {
		t.Fatalf("applied=%d, want 2 (MaxItems bound)", report.ItemsApplied)
	}
}

func TestRun_HonorsMaxExtractionCallsBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {Batch{
			Items: []Item{
				item("git-commit:a", "1", "a"),
				item("git-commit:b", "2", "b"),
				item("git-commit:c", "3", "c"),
			},
			NextCursor:   json.RawMessage(`{"n":3}`),
			NextSequence: 3,
			Done:         true,
		}}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())
	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{MaxExtractionCalls: 1}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != 1 {
		t.Fatalf("applied=%d, want 1 (MaxExtractionCalls bound)", report.ItemsApplied)
	}
	if got := len(app.eventsOfKind(ontology.KindEpisode)); got != 1 {
		t.Fatalf("episodes = %d, want 1", got)
	}
}

func TestRun_AdvancesWatermark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {Batch{
			Items:        []Item{item("git-commit:a", "h", "body")},
			NextCursor:   json.RawMessage(`{"lastSHA":"abc"}`),
			NextSequence: 1,
			Done:         true,
		}}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())
	if _, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wms := app.eventsOfKind(ontology.KindConnectorWatermarkAdvanced)
	if len(wms) != 1 {
		t.Fatalf("watermark events = %d, want 1", len(wms))
	}
	var adv ontology.ConnectorWatermarkAdvancedEvent
	if err := json.Unmarshal(wms[0].Payload, &adv); err != nil {
		t.Fatalf("unmarshal watermark: %v", err)
	}
	if adv.Connector != testConnector || adv.Stream != "commits" || adv.Sequence != 1 {
		t.Errorf("watermark = %+v, want connector/commits/seq1", adv)
	}
	if string(adv.Cursor) != `{"lastSHA":"abc"}` {
		t.Errorf("watermark cursor = %s, want advanced cursor", adv.Cursor)
	}
	// The watermark event must NOT be connector-tagged (it is internal metadata,
	// not external content) so it never trips the leak gate on its own.
	if ontology.IsConnectorSource(wms[0].Source) {
		t.Errorf("watermark event Source = %q, must not be connector-tagged", wms[0].Source)
	}
}

func TestRun_DryRunDoesNotAdvanceWatermark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(item("git-commit:a", "h", "body"))}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())
	if _, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, true); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(app.eventsOfKind(ontology.KindConnectorWatermarkAdvanced)); got != 0 {
		t.Fatalf("dry run wrote %d watermark events, want 0", got)
	}
}

func TestRun_SoftSkipsStreamOnPullError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"pull-requests", "commits"},
		pullErr: map[string]error{"pull-requests": errors.New("network down")},
		batches: map[string][]Batch{"commits": {oneBatch(item("git-commit:a", "h", "body"))}},
	}
	o, _ := newTestOrchestrator(t, newFakeReader())
	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("Run: want soft skip, got fatal %v", err)
	}
	if report.ItemsApplied != 1 {
		t.Fatalf("applied=%d, want 1 (commits still ingest when PRs fail)", report.ItemsApplied)
	}
	var prReport *StreamReport
	for i := range report.Streams {
		if report.Streams[i].Stream == "pull-requests" {
			prReport = &report.Streams[i]
		}
	}
	if prReport == nil || prReport.Err == "" {
		t.Fatalf("expected a recorded soft error for pull-requests, got %+v", report.Streams)
	}
}

func TestRun_PropagatesValidateError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &fakeSource{name: testConnector, streams: []string{"commits"}, validateErr: errors.New("not a repo")}
	o, _ := newTestOrchestrator(t, newFakeReader())
	if _, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false); err == nil {
		t.Fatal("Run: want validate error, got nil")
	}
}

func TestRun_CanceledContextEndsGracefully(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &fakeSource{
		name:    testConnector,
		streams: []string{"commits"},
		batches: map[string][]Batch{"commits": {oneBatch(item("git-commit:a", "h", "body"))}},
	}
	o, app := newTestOrchestrator(t, newFakeReader())
	report, err := o.Run(ctx, src, kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("canceled Run: want nil error (graceful), got %v", err)
	}
	if report.ItemsApplied != 0 {
		t.Fatalf("canceled Run applied=%d, want 0", report.ItemsApplied)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.batches) != 0 {
		t.Fatalf("canceled Run appended %d batches, want 0", len(app.batches))
	}
}
