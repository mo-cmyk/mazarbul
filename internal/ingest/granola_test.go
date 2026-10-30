package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// granolaTestNote is a note the fake server serves: id + created_at appear in the
// notes list; title/summary/transcript come back when the note is fetched by id.
// notFound makes the per-note GET return 404 (an un-transcribed note the source
// skips rather than erroring on).
type granolaTestNote struct {
	id        string
	createdAt time.Time
	title     string
	summary   string
	segments  []granolaSegment
	notFound  bool
}

func granolaSeg(speaker, text string) granolaSegment {
	return granolaSegment{Speaker: granolaSpeaker{DiarizationLabel: speaker}, Text: text}
}

// newGranolaServer emulates the public API: a notes list paged by an
// integer-offset cursor and filtered by created_after (strictly-after), plus a
// per-note fetch that includes the transcript.
func newGranolaServer(t *testing.T, notes []granolaTestNote) *httptest.Server {
	t.Helper()
	byID := make(map[string]granolaTestNote, len(notes))
	for _, n := range notes {
		byID[n.id] = n
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/notes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pageSize, _ := strconv.Atoi(q.Get("page_size"))
		if pageSize <= 0 {
			pageSize = granolaPageSize
		}
		createdAfter := q.Get("created_after")
		filtered := make([]granolaTestNote, 0, len(notes))
		for _, n := range notes {
			if createdAfter == "" || n.createdAt.UTC().Format(time.RFC3339) > createdAfter {
				filtered = append(filtered, n)
			}
		}
		offset, _ := strconv.Atoi(q.Get("cursor"))
		end := offset + pageSize
		if end > len(filtered) {
			end = len(filtered)
		}
		page := granolaNotesPage{HasMore: end < len(filtered), Cursor: strconv.Itoa(end)}
		for _, n := range filtered[offset:end] {
			page.Notes = append(page.Notes, granolaNote{ID: n.id, CreatedAt: n.createdAt})
		}
		writeGranolaJSON(t, w, page)
	})
	mux.HandleFunc("/notes/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/notes/")
		n, ok := byID[id]
		if !ok || n.notFound {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeGranolaJSON(t, w, granolaNote{
			ID:              n.id,
			Title:           n.title,
			SummaryMarkdown: n.summary,
			CreatedAt:       n.createdAt,
			Transcript:      n.segments,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeGranolaJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func newGranolaTestSource(srv *httptest.Server) *GranolaSource {
	return NewGranolaSource(newGranolaClient(srv.URL, "test-key", srv.Client()), fixedNow())
}

func TestGranolaSource_OrchestratorTagsUntrusted(t *testing.T) {
	t.Parallel()
	notes := []granolaTestNote{
		{
			id: "not_aaaaaaaaaaaaaa", createdAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
			title: "Standup", summary: "We shipped the connector.", segments: []granolaSegment{granolaSeg("Ada", "Status looks good.")},
		},
		// Null title/summary tolerated: only a transcript segment, still a body.
		{
			id: "not_bbbbbbbbbbbbbb", createdAt: time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC),
			segments: []granolaSegment{granolaSeg("Bob", "No update today.")},
		},
	}
	srv := newGranolaServer(t, notes)
	app := &fakeAppender{}
	o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
	report, err := o.Run(context.Background(), newGranolaTestSource(srv), kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != 2 {
		t.Fatalf("ItemsApplied = %d, want 2", report.ItemsApplied)
	}
	eps := app.eventsOfKind(ontology.KindEpisode)
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2", len(eps))
	}
	want := ontology.ConnectorSourceColonPrefix + connectorGranola
	for i := range eps {
		if eps[i].Source != want {
			t.Errorf("episode %d Source = %q, want %q", i, eps[i].Source, want)
		}
		if eps[i].Confidence != 1.0 {
			t.Errorf("episode %d Confidence = %v, want 1.0 (trust via source prefix, not a cap)", i, eps[i].Confidence)
		}
	}
}

func TestGranolaSource_PaginatesAndAdvancesWatermark(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)
	notes := []granolaTestNote{
		{id: "not_aaaaaaaaaaaaaa", createdAt: t1, summary: "one", segments: []granolaSegment{granolaSeg("x", "a")}},
		{id: "not_bbbbbbbbbbbbbb", createdAt: t2, summary: "two", segments: []granolaSegment{granolaSeg("x", "b")}},
		{id: "not_cccccccccccccc", createdAt: t3, summary: "three", segments: []granolaSegment{granolaSeg("x", "c")}},
	}
	src := newGranolaTestSource(newGranolaServer(t, notes))
	ctx := context.Background()

	b1, err := src.Pull(ctx, streamNotes, Watermark{}, 2)
	if err != nil {
		t.Fatalf("pull 1: %v", err)
	}
	if len(b1.Items) != 2 || b1.Done {
		t.Fatalf("page 1: items=%d done=%v, want 2 items, not done", len(b1.Items), b1.Done)
	}

	b2, err := src.Pull(ctx, streamNotes, Watermark{Cursor: b1.NextCursor, Sequence: b1.NextSequence}, 2)
	if err != nil {
		t.Fatalf("pull 2: %v", err)
	}
	if len(b2.Items) != 1 || !b2.Done {
		t.Fatalf("page 2: items=%d done=%v, want 1 item, done", len(b2.Items), b2.Done)
	}
	if b2.NextSequence <= b1.NextSequence {
		t.Errorf("sequence not monotonic: %d then %d", b1.NextSequence, b2.NextSequence)
	}

	// When the run drains, created_after advances to the newest note seen so the
	// next run is a cheap delta.
	var cur granolaCursor
	if uerr := json.Unmarshal(b2.NextCursor, &cur); uerr != nil {
		t.Fatalf("decode cursor: %v", uerr)
	}
	if want := t3.UTC().Format(time.RFC3339); cur.CreatedAfter != want {
		t.Errorf("CreatedAfter = %q, want high-water %q", cur.CreatedAfter, want)
	}
}

func TestGranolaSource_ReimportIsNoop(t *testing.T) {
	t.Parallel()
	notes := []granolaTestNote{
		{
			id: "not_aaaaaaaaaaaaaa", createdAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
			summary: "first", segments: []granolaSegment{granolaSeg("x", "a")},
		},
		{
			id: "not_bbbbbbbbbbbbbb", createdAt: time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC),
			summary: "second", segments: []granolaSegment{granolaSeg("x", "b")},
		},
	}
	srv := newGranolaServer(t, notes)

	app1 := &fakeAppender{}
	o1 := NewOrchestrator(app1, newFakeReader(), jobs.NewRegistry(), nil)
	r1, err := o1.Run(context.Background(), newGranolaTestSource(srv), kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Seed run 2's reader with the (dedupKey -> contentHash) the first run recorded.
	reader2 := newFakeReader()
	for _, ev := range app1.eventsOfKind(ontology.KindSourceRefRecorded) {
		var sr ontology.SourceRefRecordedEvent
		if uerr := json.Unmarshal(ev.Payload, &sr); uerr != nil {
			t.Fatalf("unmarshal sourceRef: %v", uerr)
		}
		reader2.hashes[sr.DedupKey] = sr.ContentHash
	}

	app2 := &fakeAppender{}
	o2 := NewOrchestrator(app2, reader2, jobs.NewRegistry(), nil)
	r2, err := o2.Run(context.Background(), newGranolaTestSource(srv), kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if r2.ItemsApplied != 0 || r2.ItemsSkipped != r1.ItemsApplied {
		t.Errorf("re-pull not a no-op: applied=%d skipped=%d (want applied=0 skipped=%d)",
			r2.ItemsApplied, r2.ItemsSkipped, r1.ItemsApplied)
	}
	if len(app2.eventsOfKind(ontology.KindEpisode)) != 0 {
		t.Error("re-pull appended episodes; expected none")
	}
}

func TestGranolaSource_SkipsUntranscribedNote(t *testing.T) {
	t.Parallel()
	notes := []granolaTestNote{
		{
			id: "not_aaaaaaaaaaaaaa", createdAt: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
			summary: "has transcript", segments: []granolaSegment{granolaSeg("x", "hi")},
		},
		{id: "not_bbbbbbbbbbbbbb", createdAt: time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC), notFound: true},
	}
	src := newGranolaTestSource(newGranolaServer(t, notes))
	batch, err := src.Pull(context.Background(), streamNotes, Watermark{}, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items = %d, want 1 (the 404 note is skipped, not an error)", len(batch.Items))
	}
	if batch.Items[0].DedupKey != "granola:not_aaaaaaaaaaaaaa" {
		t.Errorf("DedupKey = %q", batch.Items[0].DedupKey)
	}
	// The un-transcribed note still counts as a consumed note for the sequence.
	if batch.NextSequence != 2 {
		t.Errorf("NextSequence = %d, want 2 (both notes consumed)", batch.NextSequence)
	}
}

func TestGranolaSource_WatermarkStaysBelowOlderUntranscribedNote(t *testing.T) {
	t.Parallel()
	// The blocker case: an OLDER note has no transcript yet (404) while a NEWER
	// note is already transcribed. Advancing the high-water to the newer note
	// would push created_after past the older note and exclude it from every
	// future window — silently dropping it even after Granola transcribes it. The
	// watermark must stay strictly below the un-transcribed note so it is retried.
	tOld := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC) // un-transcribed (404)
	tNew := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC) // transcribed
	notes := []granolaTestNote{
		{id: "not_oooooooooooooo", createdAt: tOld, notFound: true},
		{id: "not_nnnnnnnnnnnnnn", createdAt: tNew, summary: "newer", segments: []granolaSegment{granolaSeg("x", "hi")}},
	}
	src := newGranolaTestSource(newGranolaServer(t, notes))
	batch, err := src.Pull(context.Background(), streamNotes, Watermark{}, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(batch.Items) != 1 || !batch.Done {
		t.Fatalf("items=%d done=%v, want 1 item + done", len(batch.Items), batch.Done)
	}
	var cur granolaCursor
	if uerr := json.Unmarshal(batch.NextCursor, &cur); uerr != nil {
		t.Fatalf("decode cursor: %v", uerr)
	}
	oldISO := tOld.UTC().Format(time.RFC3339)
	if cur.CreatedAfter >= oldISO {
		t.Errorf("CreatedAfter = %q, must be strictly < %q so the older un-transcribed note is retried", cur.CreatedAfter, oldISO)
	}
}

func TestGranolaSource_EmptyPageWithHasMoreTerminatesCleanly(t *testing.T) {
	t.Parallel()
	// A pathological page: hasMore=true with zero notes (an API race or server
	// bug). An empty page cannot advance the sequence, so the Source must report
	// Done rather than emit a not-done cursor whose sequence equals the previous
	// one — which ValidateCursor would reject, killing the job.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGranolaJSON(t, w, granolaNotesPage{Notes: nil, HasMore: true, Cursor: "page2"})
	}))
	t.Cleanup(srv.Close)
	src := NewGranolaSource(newGranolaClient(srv.URL, "k", srv.Client()), fixedNow())
	wm := Watermark{Sequence: 7}
	batch, err := src.Pull(context.Background(), streamNotes, wm, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(batch.Items) != 0 || !batch.Done {
		t.Fatalf("items=%d done=%v, want 0 items + done (empty page is terminal)", len(batch.Items), batch.Done)
	}
	if batch.NextSequence != wm.Sequence {
		t.Errorf("NextSequence = %d, want %d (no progress on an empty page)", batch.NextSequence, wm.Sequence)
	}
	if verr := ontology.ValidateCursor(wm.Sequence, batch.NextSequence, batch.NextCursor, batch.Done); verr != nil {
		t.Errorf("ValidateCursor rejected the terminal empty page: %v", verr)
	}
}

func TestGranolaSource_ValidateMapsUnauthorizedToPlanError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	src := newGranolaTestSource(srv)
	err := src.Validate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Business") {
		t.Fatalf("want Business-plan precondition error, got %v", err)
	}
}

func TestGranolaClient_RetriesThenSucceedsOn429(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.Header().Set("Retry-After", "0") // honored as a zero wait: fast, deterministic
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeGranolaJSON(t, w, granolaNotesPage{Notes: []granolaNote{{ID: "not_aaaaaaaaaaaaaa"}}})
	}))
	t.Cleanup(srv.Close)
	c := newGranolaClient(srv.URL, "k", srv.Client())
	page, err := c.listNotes(context.Background(), "", "", 30)
	if err != nil {
		t.Fatalf("listNotes after retries: %v", err)
	}
	if len(page.Notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(page.Notes))
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (2 x 429 then 200)", got)
	}
}

func TestGranolaClient_GivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := newGranolaClient(srv.URL, "k", srv.Client())
	_, err := c.listNotes(context.Background(), "", "", 30)
	if !errors.Is(err, errGranolaRateLimited) {
		t.Fatalf("err = %v, want errGranolaRateLimited", err)
	}
}

func TestGranolaClient_ToleratesNullFields(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// title and summary_markdown explicitly null — must decode to empty, not error.
		_, _ = w.Write([]byte(`{"id":"not_aaaaaaaaaaaaaa","title":null,"summary_markdown":null,` +
			`"transcript":[{"speaker":{"source":"me"},"text":"hello"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := newGranolaClient(srv.URL, "k", srv.Client())
	note, err := c.getNote(context.Background(), "not_aaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("getNote: %v", err)
	}
	if note.Title != "" || note.SummaryMarkdown != "" {
		t.Errorf("null fields not tolerated: title=%q summary=%q", note.Title, note.SummaryMarkdown)
	}
	if body := granolaBody(&note); !strings.Contains(body, "me: hello") {
		t.Errorf("body missing transcript: %q", body)
	}
}

func TestGranolaBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		attempt    int
		retryAfter string
		want       time.Duration
	}{
		{"numeric retry-after honored", 0, "2", 2 * time.Second},
		{"retry-after capped", 0, "999", granolaMaxBackoff},
		{"absent grows from base", 0, "", granolaBaseBackoff},
		{"absent doubles", 1, "", 2 * granolaBaseBackoff},
		{"absent capped", 10, "", granolaMaxBackoff},
		{"garbage retry-after falls back to exponential", 1, "soon", 2 * granolaBaseBackoff},
		// RFC 7231 also permits an HTTP-date. A far-future date is capped; a past
		// date means retry immediately.
		{"http-date future capped", 0, "Mon, 01 Jan 2114 00:00:00 GMT", granolaMaxBackoff},
		{"http-date past retries now", 0, "Mon, 01 Jan 2000 00:00:00 GMT", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := granolaBackoff(tc.attempt, tc.retryAfter); got != tc.want {
				t.Errorf("granolaBackoff(%d, %q) = %v, want %v", tc.attempt, tc.retryAfter, got, tc.want)
			}
		})
	}
}
