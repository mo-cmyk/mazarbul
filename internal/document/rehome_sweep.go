package document

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// SweepStats holds the read-only counts returned by QuerySweepStats.
// It is used by `maz doc rehome --dry-run` to report what would happen.
type SweepStats struct {
	// FactsOnDocuments is the total number of observation facts currently homed
	// on a document entity (source = "document:import", subject is a document kind).
	FactsOnDocuments int
	// FactsToRehome is the number of those facts whose document has at least one
	// active about: subject (describes edge) and would therefore be moved.
	FactsToRehome int
	// DocumentsAffected is the number of distinct documents that have facts
	// eligible for rehoming (i.e., that have at least one describes edge and at
	// least one observation fact to move).
	DocumentsAffected int
}

// QuerySweepStats returns the counts of document-homed observation facts without
// mutating any state. It is the read-only counterpart to SweepObservationFacts,
// used by `maz doc rehome --dry-run` to report how many facts would be moved.
func QuerySweepStats(ctx context.Context, reader *sql.DB) (SweepStats, error) {
	docFacts, err := queryDocumentHomedFacts(ctx, reader)
	if err != nil {
		return SweepStats{}, fmt.Errorf("sweep stats: query document-homed facts: %w", err)
	}
	if len(docFacts) == 0 {
		return SweepStats{}, nil
	}

	aboutByDoc, err := queryAboutSubjects(ctx, reader)
	if err != nil {
		return SweepStats{}, fmt.Errorf("sweep stats: query about subjects: %w", err)
	}

	var stats SweepStats
	stats.FactsOnDocuments = len(docFacts)
	affected := make(map[string]struct{})
	for _, df := range docFacts {
		subjects := aboutByDoc[df.subjectUID]
		if len(subjects) == 0 {
			continue
		}
		if df.subjectUID == subjects[0] {
			continue // already converged
		}
		stats.FactsToRehome++
		affected[df.subjectUID] = struct{}{}
	}
	stats.DocumentsAffected = len(affected)
	return stats, nil
}

// SweepObservationFacts scans existing observation facts currently homed on a
// document entity (subject_uid is a document-kind entity, source =
// "document:import") and, for each such document that has about: subjects
// (active predicateDescribes edges in the relations table), emits factRehomed
// events to move those facts to the correct subject.
//
// Subject assignment follows the same rules as observationFactEvents:
//   - Exactly one about: subject: emit one factRehomed event moving the fact.
//   - Multiple about: subjects: emit one factRehomed for the first subject and
//     one new Fact event per additional subject.
//   - Zero about: subjects: nothing to do; the fact stays on the document.
//
// The sweep is idempotent: facts already on an about: subject are skipped.
// A second run after full convergence emits zero events.
//
// Reader is the SQLite read-only pool from Dispatcher.SQLiteReader(); appender
// is the kernel.FileEventLog. This function is NOT wired into any production
// path (not maz rebuild, not import, not the daemon): it has no callers outside
// tests and a deliberately-invoked controlled-rebuild step. It is the manual
// remediation for documents imported before Part-A subject assignment existed.
// It MUST NOT be called on live data outside of a controlled, sync-paused
// rebuild; it appends factRehomed events to the event log.
func SweepObservationFacts(
	ctx context.Context,
	reader *sql.DB,
	appender Appender,
	now func() time.Time,
	newUID func() (string, error),
	log *zap.Logger,
) error {
	if log == nil {
		log = zap.NewNop()
	}

	// 1. Collect observation facts that are currently homed on a document entity.
	docFacts, err := queryDocumentHomedFacts(ctx, reader)
	if err != nil {
		return fmt.Errorf("sweep: query document-homed facts: %w", err)
	}
	if len(docFacts) == 0 {
		return nil
	}

	// 2. Build a map from docUID to its about: subject UIDs (describes edges).
	aboutByDoc, err := queryAboutSubjects(ctx, reader)
	if err != nil {
		return fmt.Errorf("sweep: query about subjects: %w", err)
	}

	// 3. For each document-homed fact, emit the appropriate rehome events.
	ts := now()
	events := make([]kernel.Event, 0, len(docFacts))
	for _, df := range docFacts {
		evs, err := rehomeEventsForFact(df, aboutByDoc, ts, newUID)
		if err != nil {
			return fmt.Errorf("sweep: build rehome events for fact %s: %w", df.uid, err)
		}
		events = append(events, evs...)
	}

	if len(events) == 0 {
		return nil
	}
	if _, err := appender.Append(ctx, events); err != nil {
		return fmt.Errorf("sweep: append rehome events: %w", err)
	}
	log.Info("document: sweep observation facts",
		zap.Int("events", len(events)),
	)
	return nil
}

// rehomeEventsForFact builds the factRehomed (and any supplemental Fact)
// events needed to home df on the about: subjects for its document. Returns
// nil when no rehome is needed (no about: subjects, or already converged).
func rehomeEventsForFact(
	df docHomedFact,
	aboutByDoc map[string][]string,
	ts time.Time,
	newUID func() (string, error),
) ([]kernel.Event, error) {
	subjects, hasAbout := aboutByDoc[df.subjectUID]
	if !hasAbout || len(subjects) == 0 {
		return nil, nil // no about: — fact correctly lives on the document
	}
	if df.subjectUID == subjects[0] {
		return nil, nil // already converged
	}

	sourceUID, err := newUID()
	if err != nil {
		return nil, fmt.Errorf("mint source uid: %w", err)
	}

	rehomeUID, err := newUID()
	if err != nil {
		return nil, fmt.Errorf("mint rehome uid: %w", err)
	}
	rehome := ontology.FactRehomed{
		FactUID:        df.uid,
		FromSubjectUID: df.subjectUID,
		ToSubjectUID:   subjects[0],
		CreateTime:     ts,
		SourceEventUID: sourceUID,
	}
	payload, err := json.Marshal(rehome)
	if err != nil {
		return nil, fmt.Errorf("marshal factRehomed: %w", err)
	}
	events := []kernel.Event{{
		UID:        rehomeUID,
		Kind:       kernel.EventKind(ontology.KindFactRehomed),
		Source:     "document:sweep",
		Confidence: 1.0,
		Payload:    payload,
	}}

	// Emit a new Fact for each additional about: subject beyond the first.
	for _, extraSubject := range subjects[1:] {
		ev, err := extraFactEvent(df, extraSubject, sourceUID, ts, newUID)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// extraFactEvent builds a new Fact event for extraSubject carrying the same
// observation content as df. Used when a document has multiple about: subjects.
func extraFactEvent(
	df docHomedFact,
	extraSubject, sourceUID string,
	ts time.Time,
	newUID func() (string, error),
) (kernel.Event, error) {
	factUID, err := newUID()
	if err != nil {
		return kernel.Event{}, fmt.Errorf("mint extra fact uid: %w", err)
	}
	extraFact := ontology.Fact{
		Name:           "facts/" + factUID,
		UID:            factUID,
		SubjectUID:     extraSubject,
		StateKey:       df.stateKey,
		Value:          df.value,
		CreateTime:     ts,
		SourceEventUID: sourceUID,
		ContentHash:    observationContentHash(extraSubject, df.stateKey, df.value),
	}
	fp, err := json.Marshal(extraFact)
	if err != nil {
		return kernel.Event{}, fmt.Errorf("marshal extra fact: %w", err)
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindFact),
		Source:     "document:sweep",
		Confidence: 1.0,
		Payload:    fp,
	}, nil
}

// docHomedFact is one facts row whose subject is a document entity.
type docHomedFact struct {
	uid        string
	subjectUID string // the document entity UID
	stateKey   string
	value      string
}

// queryDocumentHomedFacts returns facts that have source "document:import"
// and whose subject_uid maps to an entity of kind "document". These are the
// facts eligible for rehoming.
func queryDocumentHomedFacts(ctx context.Context, reader *sql.DB) ([]docHomedFact, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT f.uid, f.subject_uid, f.state_key, f.value
		FROM facts f
		JOIN entities e ON e.uid = f.subject_uid
		WHERE f.source = 'document:import'
		  AND e.kind   = ?
		ORDER BY f.uid ASC`,
		ontology.EntityKindDocument,
	)
	if err != nil {
		return nil, fmt.Errorf("query document-homed facts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []docHomedFact
	for rows.Next() {
		var df docHomedFact
		if serr := rows.Scan(&df.uid, &df.subjectUID, &df.stateKey, &df.value); serr != nil {
			return nil, fmt.Errorf("scan document-homed fact: %w", serr)
		}
		out = append(out, df)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate document-homed facts: %w", rerr)
	}
	return out, nil
}

// queryAboutSubjects returns a map from document UID to the UIDs of all active
// about: subjects (predicateDescribes relations whose valid_until is NULL). The
// order of subjects within each slice is deterministic (ordered by relation UID
// ascending) so the first-subject assignment is stable across runs.
func queryAboutSubjects(ctx context.Context, reader *sql.DB) (map[string][]string, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT subject_uid, object_uid
		FROM relations
		WHERE predicate   = ?
		  AND valid_until IS NULL
		ORDER BY subject_uid ASC, uid ASC`,
		predicateDescribes,
	)
	if err != nil {
		return nil, fmt.Errorf("query about subjects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]string)
	for rows.Next() {
		var docUID, subjectUID string
		if serr := rows.Scan(&docUID, &subjectUID); serr != nil {
			return nil, fmt.Errorf("scan about subject: %w", serr)
		}
		out[docUID] = append(out[docUID], subjectUID)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate about subjects: %w", rerr)
	}
	return out, nil
}
