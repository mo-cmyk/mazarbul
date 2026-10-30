package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// connectorGranola is the GranolaSource name; it tags Event.Source as
// "connector:granola".
const connectorGranola = "granola"

// streamNotes is the single stream GranolaSource pulls.
const streamNotes = "notes"

// sourceTypeMeetingTranscript is the open-taxonomy SourceType stamped on every
// ingested note's SourceRef.
const sourceTypeMeetingTranscript = "meeting_transcript"

// granolaPageSize is the API maximum notes-per-page (the API caps at 30).
const granolaPageSize = 30

// GranolaSource ingests meeting notes + transcripts from the official Granola
// public REST API using a first-party API key (no OAuth). It pulls notes after a
// created_after high-water, fetches each note's transcript, and yields one Item
// per note. Minting an API key requires a Granola Business/Enterprise plan — a
// missing/forbidden key surfaces that precondition explicitly.
type GranolaSource struct {
	client    *granolaClient
	now       func() time.Time
	validated bool
}

// NewGranolaSource builds a GranolaSource over client. Now may be nil (defaults
// to time.Now).
func NewGranolaSource(client *granolaClient, now func() time.Time) *GranolaSource {
	if now == nil {
		now = time.Now
	}
	return &GranolaSource{client: client, now: now}
}

// Name implements Source.
func (s *GranolaSource) Name() string { return connectorGranola }

// Streams implements Source: a single "notes" stream.
func (s *GranolaSource) Streams() []string { return []string{streamNotes} }

// Validate implements Source: a cheap one-note probe that confirms the API key
// works WITHOUT ingesting. A 401/403 is mapped to the honest Business-plan
// precondition rather than a raw status.
func (s *GranolaSource) Validate(ctx context.Context) error {
	// The probe is memoized for the source instance's lifetime: the tool layer
	// pre-validates and Orchestrator.Run validates again, but one network
	// round-trip suffices (a source is built fresh per ingest invocation, and
	// calls within one invocation are sequential).
	if s.validated {
		return nil
	}
	_, err := s.client.listNotes(ctx, "", "", 1)
	if errors.Is(err, errGranolaUnauthorized) {
		return errors.New("ingest: granola API key rejected — a Granola API key requires a Business/Enterprise plan; run `maz source add granola` with a valid key")
	}
	if err != nil {
		return fmt.Errorf("ingest: granola validate: %w", err)
	}
	s.validated = true
	return nil
}

// granolaCursor is the opaque resume cursor for the notes stream. CreatedAfter is
// the query filter for the current run (the prior run's high-water); Page is the
// opaque within-run pagination cursor; HighWater accumulates the newest created_at
// seen this run and becomes the next run's CreatedAfter once the run drains.
// OldestPending records the oldest created_at of an un-transcribed (404) note
// seen this run; the drained high-water is held strictly below it so that note is
// re-fetched next run rather than skipped past forever (see Pull).
type granolaCursor struct {
	CreatedAfter  string `json:"createdAfter,omitempty"`
	Page          string `json:"page,omitempty"`
	HighWater     string `json:"highWater,omitempty"`
	OldestPending string `json:"oldestPending,omitempty"`
}

// Pull implements Source. It fetches one page of notes after the cursor, then a
// transcript per note, yielding one Item each. Within a run it paginates via the
// opaque page cursor; when the page set drains it advances CreatedAfter to the
// run's high-water so the next run is a cheap delta. SourceRef dedup (note id +
// content hash) makes any overlap idempotent.
func (s *GranolaSource) Pull(ctx context.Context, stream string, wm Watermark, limit int) (Batch, error) {
	if stream != streamNotes {
		return Batch{}, fmt.Errorf("ingest: unknown granola stream %q", stream)
	}
	cur, err := decodeGranolaCursor(wm.Cursor)
	if err != nil {
		return Batch{}, err
	}

	pageSize := granolaPageSize
	if limit > 0 && limit < pageSize {
		pageSize = limit
	}
	page, err := s.client.listNotes(ctx, cur.CreatedAfter, cur.Page, pageSize)
	if err != nil {
		return Batch{}, fmt.Errorf("ingest: granola list notes: %w", err)
	}

	startWater := cur.HighWater
	if startWater == "" {
		startWater = cur.CreatedAfter
	}
	items, highWater, oldestPending, err := s.collectPage(ctx, page.Notes, startWater, cur.OldestPending)
	if err != nil {
		return Batch{}, err
	}

	// An empty page cannot advance the sequence, so hasMore=true on an empty page
	// is treated as terminal: continuing would emit a not-done cursor whose
	// sequence equals the previous one, which ValidateCursor rejects (failing the
	// whole job). A transient/spurious empty page is retried on the next run from
	// the unchanged CreatedAfter.
	more := page.HasMore && len(page.Notes) > 0
	next := granolaCursor{HighWater: highWater}
	if more {
		next.CreatedAfter = cur.CreatedAfter // keep filtering the current set
		next.Page = page.Cursor
		next.OldestPending = oldestPending // carry the barrier across pages
	} else {
		// Run drained: resume after the high-water, but never at or past an
		// un-transcribed note. Holding the watermark just below the oldest pending
		// note re-opens its window so it is retried once transcribed; re-fetching
		// already-ingested notes in that window is idempotent (SourceRef dedup). A
		// note that is never transcribed pins the watermark there — the cost of
		// never dropping — and it self-heals the instant the note transcribes. The
		// barrier is consumed by this resume point, so it is cleared (not carried);
		// a still-pending note re-registers on the next run rather than ratcheting.
		next.CreatedAfter = highWater
		if oldestPending != "" && oldestPending <= next.CreatedAfter {
			next.CreatedAfter = granolaWatermarkBefore(oldestPending)
		}
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return Batch{}, fmt.Errorf("ingest: encode granola cursor: %w", err)
	}
	return Batch{
		Items:      items,
		NextCursor: raw,
		// Advance by NOTES consumed, not items emitted: a page of un-transcribed
		// notes (all 404-skipped) still makes progress, so the sequence moves and
		// ValidateCursor accepts a not-done page.
		NextSequence: wm.Sequence + uint64(len(page.Notes)),
		Done:         !more,
	}, nil
}

// collectPage fetches each note's transcript and returns the emitted items, the
// advanced high-water (the newest emitted note's created_at), and the oldest
// un-transcribed note's created_at (the watermark barrier). A note created BEFORE
// a newer transcribed note must still be re-fetched once Granola transcribes it,
// so a 404 records its created_at as a barrier rather than letting a newer note
// raise the high-water past it (which would exclude it from every future
// created_after window). StartWater/startPending seed the running values so the
// page contributes to a multi-page run's accumulators.
func (s *GranolaSource) collectPage(ctx context.Context, notes []granolaNote, startWater, startPending string) (items []Item, highWater, oldestPending string, err error) {
	highWater, oldestPending = startWater, startPending
	items = make([]Item, 0, len(notes))
	for i := range notes {
		item, ok, ierr := s.itemForNote(ctx, notes[i].ID)
		if ierr != nil {
			return nil, "", "", ierr
		}
		iso := ""
		if ca := notes[i].CreatedAt; !ca.IsZero() {
			iso = ca.UTC().Format(time.RFC3339)
		}
		if !ok {
			if iso != "" && (oldestPending == "" || iso < oldestPending) {
				oldestPending = iso
			}
			continue // un-transcribed note (404) — skipped, not an error
		}
		items = append(items, item)
		if iso != "" && iso > highWater {
			highWater = iso
		}
	}
	return items, highWater, oldestPending, nil
}

// itemForNote fetches a note's transcript and builds its Item. Ok is false when
// the note has no transcript yet (a 404), which the caller skips.
func (s *GranolaSource) itemForNote(ctx context.Context, id string) (Item, bool, error) {
	full, err := s.client.getNote(ctx, id)
	if errors.Is(err, errGranolaNotFound) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("ingest: granola get note %s: %w", id, err)
	}
	body := granolaBody(&full)
	if strings.TrimSpace(body) == "" {
		return Item{}, false, nil
	}
	fetch := s.now().UTC()
	return Item{
		DedupKey:    "granola:" + full.ID,
		ExternalID:  full.ID,
		ExternalURL: full.WebURL,
		SourceType:  sourceTypeMeetingTranscript,
		ContentHash: contentHash(body),
		Body:        body,
		FetchTime:   fetch,
	}, true, nil
}

// granolaBody renders a note into the verbatim body the orchestrator scrubs,
// fences, and turns into an Episode: title, then the AI summary (markdown
// preferred), then the diarised transcript. Nullable fields are tolerated.
func granolaBody(n *granolaNote) string {
	var b strings.Builder
	if n.Title != "" {
		b.WriteString("# " + n.Title + "\n\n")
	}
	switch {
	case n.SummaryMarkdown != "":
		b.WriteString(n.SummaryMarkdown + "\n\n")
	case n.SummaryText != "":
		b.WriteString(n.SummaryText + "\n\n")
	}
	if len(n.Transcript) > 0 {
		b.WriteString("## Transcript\n")
		for i := range n.Transcript {
			seg := n.Transcript[i]
			if strings.TrimSpace(seg.Text) == "" {
				continue
			}
			speaker := seg.Speaker.DiarizationLabel
			if speaker == "" {
				speaker = seg.Speaker.Source
			}
			if speaker == "" {
				speaker = "speaker"
			}
			b.WriteString(speaker + ": " + seg.Text + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// granolaWatermarkBefore returns an RFC3339 timestamp strictly less than iso, so
// a created_after filter set to it still RE-INCLUDES the note at iso. The
// watermark is second-granular (formatted with time.RFC3339), so one second is
// the smallest safe step back; the re-opened window re-fetches at most a few
// neighbouring notes, which SourceRef dedup absorbs. An unparseable iso (which
// cannot occur — it was produced by the same formatter) degrades to a full
// re-scan rather than risk dropping the pending note.
func granolaWatermarkBefore(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	return t.Add(-time.Second).UTC().Format(time.RFC3339)
}

func decodeGranolaCursor(raw json.RawMessage) (granolaCursor, error) {
	var c granolaCursor
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("ingest: decode granola cursor: %w", err)
	}
	return c, nil
}
