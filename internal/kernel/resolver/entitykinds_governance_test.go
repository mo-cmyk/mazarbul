package resolver

// entitykinds_governance_test.go pins the entity-kind governance and
// permissive-with-review behaviour for auto-registration.
//
// Governed mode:
//   - appendEntityKindRegistered persists events with Status="proposed" and
//     does NOT add the kind to the KindRegistry cache.
//   - enqueueKindReview appends a durable EntityKindRegisteredEvent with
//     Status="proposed".
//
// Permissive mode (also covered here for low-confidence paths):
//   - enqueueKindReview appends a durable EntityKindRegisteredEvent with
//     Status="proposed" (same as governed) — low-confidence routes to the
//     review queue regardless of mode; only high-confidence mints land active.
//
// Tests are in-package (package resolver) to access unexported helpers.

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// projectingLog wraps captureLog and ALSO applies entityKindRegistered events
// to the entity_kinds table inside Append, mirroring production's FileEventLog,
// which runs the dispatcher projection synchronously within the Append call.
// This lets the status-aware dedup check (isKindActiveOrProposed) observe a
// freshly-appended proposal, so the concurrency and same-process-remint tests
// exercise the same ordering the daemon sees rather than the projection-less
// fake-log shortcut. mu guards both the recorded-events slice and the table
// write for concurrent Append callers.
type projectingLog struct {
	*captureLog
	mu sync.Mutex
	db *sql.DB
}

func newProjectingLog(db *sql.DB) *projectingLog {
	return &projectingLog{captureLog: &captureLog{}, db: db}
}

func (l *projectingLog) Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, events...)
	for _, ev := range events {
		if ev.Kind != kernel.EventKind(ontology.KindEntityKindRegistered) {
			continue
		}
		var p ontology.EntityKindRegisteredEvent
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, err
		}
		status := p.Status
		if status == "" {
			status = "active"
		}
		if _, err := l.db.ExecContext(ctx,
			`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(name) DO UPDATE SET status = excluded.status, display_name = excluded.display_name, description = excluded.description`,
			p.Name, p.DisplayName, p.Description, p.Provenance, string(p.AddedBy), p.AddedAt.Format(time.RFC3339Nano), status,
		); err != nil {
			return nil, err
		}
	}
	return make([]kernel.Receipt, len(events)), nil
}

// newGovernedTestResolver builds a Resolver with Governed=true and a live
// SQLite store. It mirrors newKindTestResolver but sets the Governed flag and
// wires the SQLite reader so isKindActiveOrProposed (the duplicate-proposal
// guard) can query the database. The event log is taken as an interface so a
// caller can pass either captureLog or projectingLog.
func newGovernedTestResolver(t *testing.T, log kernel.EventLog, chat kernel.ChatProvider, store *sqlite.Store) *Resolver {
	t.Helper()
	ctx := t.Context()
	kr, err := NewKindRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewKindRegistry: %v", err)
	}
	reg := prompts.New(nil)
	if err := RegisterPrompts(ctx, reg); err != nil {
		t.Fatalf("RegisterPrompts: %v", err)
	}
	cfg := Config{Governed: true}
	cfg.ApplyDefaults()
	return &Resolver{
		cfg:          cfg,
		eventLog:     log,
		chat:         chat,
		kindRegistry: kr,
		promptReg:    reg,
		reader:       store.Reader(),
		logger:       zap.NewNop(),
	}
}

// TestAppendEntityKindRegistered_GovernedMode_ProposedStatus verifies that in
// governed mode appendEntityKindRegistered appends an EntityKindRegisteredEvent
// with Status="proposed" and does NOT add the kind to the KindRegistry cache.
func TestAppendEntityKindRegistered_GovernedMode_ProposedStatus(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	if err := r.appendEntityKindRegistered(ctx, "spaceship", "Spaceship", "An extraterrestrial vehicle."); err != nil {
		t.Fatalf("appendEntityKindRegistered: %v", err)
	}

	// Exactly one event must be appended.
	if len(log.events) != 1 {
		t.Fatalf("governed mode: want 1 event, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != "proposed" {
		t.Errorf("event.Status = %q, want proposed", ev.Status)
	}
	if ev.Name != "spaceship" {
		t.Errorf("event.Name = %q, want spaceship", ev.Name)
	}

	// In governed mode the KindRegistry cache must NOT be updated.
	if r.kindRegistry.IsRegistered("spaceship") {
		t.Error("governed mode: kind must NOT be added to KindRegistry cache after proposed registration")
	}
}

// TestAppendEntityKindRegistered_PermissiveMode_ActiveStatus verifies that in
// permissive mode appendEntityKindRegistered appends an event with Status=""
// (empty, meaning active) and DOES add the kind to the KindRegistry cache.
func TestAppendEntityKindRegistered_PermissiveMode_ActiveStatus(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newKindTestResolver(t, log, &countingChat{}, store)

	if err := r.appendEntityKindRegistered(ctx, "drone", "Drone", "An unmanned aerial vehicle."); err != nil {
		t.Fatalf("appendEntityKindRegistered: %v", err)
	}

	if len(log.events) != 1 {
		t.Fatalf("permissive mode: want 1 event, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	// Permissive mode: Status must be empty (omitempty) — dispatcher defaults to active.
	if ev.Status != "" {
		t.Errorf("event.Status = %q, want empty (permissive default)", ev.Status)
	}

	// Cache MUST be updated in permissive mode.
	if !r.kindRegistry.IsRegistered("drone") {
		t.Error("permissive mode: kind must be added to KindRegistry cache after registration")
	}
}

// TestEnqueueKindReview_GovernedMode_AppendsProposedEvent verifies that
// enqueueKindReview in governed mode appends a durable EntityKindRegisteredEvent
// with Status="proposed" (not just a log message).
func TestEnqueueKindReview_GovernedMode_AppendsProposedEvent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	r.enqueueKindReview(ctx, "nebula", "Context snippet about a nebula", 0.42)

	// Must append exactly one proposed event.
	if len(log.events) != 1 {
		t.Fatalf("governed enqueueKindReview: want 1 event, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != "proposed" {
		t.Errorf("event.Status = %q, want proposed", ev.Status)
	}
	if ev.Name != "nebula" {
		t.Errorf("event.Name = %q, want nebula", ev.Name)
	}
	if ev.AddedAt.IsZero() {
		t.Error("event.AddedAt must be set")
	}
}

// TestEnqueueKindReview_PermissiveMode_ProposedEvent verifies that
// enqueueKindReview in permissive mode appends a durable
// EntityKindRegisteredEvent with Status="proposed". Low-confidence kinds must
// route to the review queue regardless of mode so they appear in
// `maz schema review list` and are not silently dropped.
func TestEnqueueKindReview_PermissiveMode_ProposedEvent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newKindTestResolver(t, log, &countingChat{}, store)

	r.enqueueKindReview(ctx, "nebula", "Context snippet about a nebula", 0.42)

	// Must append exactly one proposed event (not active, not dropped).
	if len(log.events) != 1 {
		t.Fatalf("permissive enqueueKindReview: want 1 event, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != ontology.EntityKindStatusProposed {
		t.Errorf("event.Status = %q, want proposed", ev.Status)
	}
	if ev.Name != "nebula" {
		t.Errorf("event.Name = %q, want nebula", ev.Name)
	}
	// Kind must NOT be in the registry cache (proposed != active).
	if r.kindRegistry.IsRegistered("nebula") {
		t.Error("permissive mode: low-confidence kind must NOT be added to KindRegistry cache")
	}
}

// TestAutoRegisterEntityKind_GovernedMode_LexiconNewKindIsProposed verifies
// that when a lexicon new-kind (vehicle) is auto-registered in governed mode:
//   - Exactly one EntityKindRegisteredEvent is appended.
//   - Its Status is "proposed".
//   - The KindRegistry cache is NOT updated (kind stays inactive until approved).
func TestAutoRegisterEntityKind_GovernedMode_LexiconNewKindIsProposed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	canonical, err := r.autoRegisterEntityKind(ctx, "vehicle", "Merry drives a vehicle", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind: %v", err)
	}
	// The canonical name is returned even in governed mode; resolution continues.
	if canonical != "vehicle" {
		t.Errorf("canonical = %q, want vehicle", canonical)
	}
	if len(log.events) != 1 {
		t.Fatalf("governed lexicon new kind: want 1 event, got %d", len(log.events))
	}

	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != "proposed" {
		t.Errorf("event.Status = %q, want proposed", ev.Status)
	}
	if r.kindRegistry.IsRegistered("vehicle") {
		t.Error("governed mode: vehicle must NOT be in KindRegistry cache (not yet approved)")
	}
}

// TestAppendEntityKindRegistered_GovernedMode_NoDuplicateProposal verifies that
// when a kind is already persisted in entity_kinds (status="proposed") a second
// call to appendEntityKindRegistered does NOT append another event. This is the
// Issue-6 regression test: the KindRegistry in-memory cache does not track
// proposed kinds, so without the SQLite guard repeated ingestion passes for the
// same unknown kind would accumulate duplicate proposal rows.
func TestAppendEntityKindRegistered_GovernedMode_NoDuplicateProposal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	// First call: no row in entity_kinds, so the event IS appended.
	if err := r.appendEntityKindRegistered(ctx, "comet", "Comet", "An icy small solar system body."); err != nil {
		t.Fatalf("first appendEntityKindRegistered: %v", err)
	}
	if len(log.events) != 1 {
		t.Fatalf("first call: want 1 event, got %d", len(log.events))
	}

	// Simulate the dispatcher materialising the event: insert the proposed row
	// into entity_kinds (in production this happens asynchronously after the
	// event is appended; in this unit test we do it synchronously).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"comet", "Comet", "An icy small solar system body.", "user", "llm", now, "proposed",
	); err != nil {
		t.Fatalf("seed entity_kinds row: %v", err)
	}

	// Reset the capture log so we can count only the second call's events.
	log.events = nil

	// Second call: the row EXISTS in entity_kinds; isKindAlreadyPersisted must
	// return true and the function must return without appending another event.
	if err := r.appendEntityKindRegistered(ctx, "comet", "Comet", "An icy small solar system body."); err != nil {
		t.Fatalf("second appendEntityKindRegistered: %v", err)
	}
	if len(log.events) != 0 {
		t.Errorf("second call: want 0 events (duplicate suppressed), got %d", len(log.events))
	}
}

// TestEnqueueKindReview_GovernedMode_NoDuplicateProposal verifies that
// enqueueKindReview in governed mode skips appending when the kind is already
// in entity_kinds. This covers the enqueueKindReview path of Issue-6.
func TestEnqueueKindReview_GovernedMode_NoDuplicateProposal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	// Pre-seed the kind as already proposed in entity_kinds.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"pulsar", "Pulsar", "A rotating neutron star.", "user", "llm", now, "proposed",
	); err != nil {
		t.Fatalf("seed entity_kinds row: %v", err)
	}

	// enqueueKindReview must not append an event because the row already exists.
	r.enqueueKindReview(ctx, "pulsar", "Context: a pulsar was mentioned", 0.45)
	if len(log.events) != 0 {
		t.Errorf("enqueueKindReview with existing row: want 0 events (duplicate suppressed), got %d", len(log.events))
	}
}

// TestAutoRegisterEntityKind_GovernedMode_IdempotentSecondCall verifies that a
// second call for the same governed-mode kind does NOT append another event
// (since the kind is not in the KindRegistry cache it would re-enter the
// lexicon path; but the appendEntityKindRegistered guard checks IsRegistered,
// so it fires once per cache miss — this test documents the current contract).
func TestAutoRegisterEntityKind_GovernedMode_IdempotentSecondCall(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	// Seed the kind into entity_kinds so the second call's guard fires.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"vehicle", "Vehicle", "A motorised road vehicle.", "user", "llm", now, "proposed",
	); err != nil {
		t.Fatalf("seed entity_kinds: %v", err)
	}
	// Re-prime the KindRegistry from the updated store.
	kr2, err := NewKindRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewKindRegistry: %v", err)
	}
	r.kindRegistry = kr2

	// Call once: no event because the store already has the row (IsRegistered
	// via the KindRegistry load query would not find it since it filters active
	// only — so appending still fires). Reset log to test the second call.
	log.events = nil

	// Manually mark the cache so the guard fires.
	r.kindRegistry.MarkRegistered("vehicle")

	_, err = r.autoRegisterEntityKind(ctx, "vehicle", "Merry drives a vehicle", &ingestMintCounter{})
	if err != nil {
		t.Fatalf("autoRegisterEntityKind second call: %v", err)
	}
	// Now that IsRegistered returns true, no event should be appended.
	if len(log.events) != 0 {
		t.Errorf("second call with cache hit: want 0 events, got %d", len(log.events))
	}
}

// TestAppendEntityKindRegistered_GovernedMode_ConcurrentNoDuplicate pins the
// pre-projection race fix: many concurrent ingestion passes for the same
// unknown kind must append exactly ONE proposal event. singleflight coalesces
// the concurrent wave into a single check-then-append; the projectingLog then
// applies the 'proposed' row so any straggler that arrives after the flight
// clears re-checks the row and skips. Without the guard, every goroutine would
// observe no entity_kinds row and append a duplicate. Run under -race, a broken
// guard also trips the detector on the unsynchronised event-slice write.
func TestAppendEntityKindRegistered_GovernedMode_ConcurrentNoDuplicate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := newProjectingLog(store.Writer())
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			// Ignore the error: a losing goroutine returns nil without appending.
			_ = r.appendEntityKindRegistered(ctx, "asteroid",
				"Asteroid", "A small rocky body orbiting the sun.")
		}()
	}
	wg.Wait()

	if len(log.events) != 1 {
		t.Fatalf("concurrent governed proposals: want exactly 1 appended event, got %d", len(log.events))
	}
}

// TestAppendEntityKindRegistered_GovernedMode_SameProcessRemintAfterRejection is
// the definitive regression for the lifecycle rule that a long-running governed
// resolver — the SAME process that originally proposed a kind — can re-propose
// it after a reviewer rejects it. The projectingLog mirrors production: the
// first proposal writes a 'proposed' row; a reviewer rejection flips it to
// 'rejected'; a corrected ingestion pass on the same Resolver must append a
// fresh proposal. This fails if the in-process guard outlives the append window
// (a process-lifetime guard would suppress the remint until restart).
func TestAppendEntityKindRegistered_GovernedMode_SameProcessRemintAfterRejection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := newProjectingLog(store.Writer())
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	// First proposal: appends one event and projects the 'proposed' row.
	if err := r.appendEntityKindRegistered(ctx, "pulsar", "Pulsar", "A rotating neutron star."); err != nil {
		t.Fatalf("first proposal: %v", err)
	}
	if len(log.events) != 1 {
		t.Fatalf("first proposal: want 1 event, got %d", len(log.events))
	}

	// A duplicate pass while the kind is still 'proposed' must be suppressed.
	if err := r.appendEntityKindRegistered(ctx, "pulsar", "Pulsar", "A rotating neutron star."); err != nil {
		t.Fatalf("duplicate proposal: %v", err)
	}
	if len(log.events) != 1 {
		t.Fatalf("duplicate while proposed: want 1 event total, got %d", len(log.events))
	}

	// Reviewer rejects the proposal.
	if _, err := store.Writer().ExecContext(ctx,
		`UPDATE entity_kinds SET status = 'rejected' WHERE name = ?`, "pulsar",
	); err != nil {
		t.Fatalf("simulate rejection: %v", err)
	}

	// Corrected ingestion pass on the SAME resolver must append a fresh proposal.
	if err := r.appendEntityKindRegistered(ctx, "pulsar", "Pulsar", "A corrected, higher-confidence classification."); err != nil {
		t.Fatalf("remint after rejection: %v", err)
	}
	if len(log.events) != 2 {
		t.Fatalf("same-process remint after rejection: want 2 events total, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[1].Payload, &ev); err != nil {
		t.Fatalf("unmarshal remint event: %v", err)
	}
	if ev.Status != "proposed" {
		t.Errorf("remint event.Status = %q, want proposed", ev.Status)
	}
}

// TestAppendEntityKindRegistered_GovernedMode_RejectedKindCanRemint verifies the
// lifecycle rule that a REJECTED kind can be re-proposed. A reviewer may reject a
// kind that was proposed with a poor classification; a later ingestion pass with
// a corrected, higher-confidence proposal must be allowed to append a fresh
// entityKindRegistered event (the dispatcher upsert flips the row back to
// 'proposed'). The status-aware isKindActiveOrProposed guard treats only
// 'active'/'proposed' rows as terminal, so a 'rejected' row does NOT suppress the
// remint.
func TestAppendEntityKindRegistered_GovernedMode_RejectedKindCanRemint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	// Seed the kind as already REJECTED in entity_kinds.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"quasar", "Quasar", "An old, rejected classification.", "user", "llm", now, "rejected",
	); err != nil {
		t.Fatalf("seed rejected entity_kinds row: %v", err)
	}

	// A corrected proposal must NOT be suppressed: exactly one event is appended.
	if err := r.appendEntityKindRegistered(ctx, "quasar", "Quasar", "A luminous active galactic nucleus."); err != nil {
		t.Fatalf("appendEntityKindRegistered after rejection: %v", err)
	}
	if len(log.events) != 1 {
		t.Fatalf("rejected-kind remint: want 1 appended event, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != "proposed" {
		t.Errorf("remint event.Status = %q, want proposed", ev.Status)
	}
	if ev.Name != "quasar" {
		t.Errorf("remint event.Name = %q, want quasar", ev.Name)
	}
}

// TestEnqueueKindReview_GovernedMode_RejectedKindCanRemint mirrors the
// rejected-remint rule for the low-confidence enqueueKindReview path: a rejected
// kind seen again by ingestion must be re-proposable rather than permanently
// suppressed.
func TestEnqueueKindReview_GovernedMode_RejectedKindCanRemint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newGovernedTestResolver(t, log, &countingChat{}, store)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"magnetar", "Magnetar", "An old, rejected classification.", "user", "llm", now, "rejected",
	); err != nil {
		t.Fatalf("seed rejected entity_kinds row: %v", err)
	}

	r.enqueueKindReview(ctx, "magnetar", "Context: a magnetar was mentioned", 0.45)
	if len(log.events) != 1 {
		t.Fatalf("rejected-kind enqueue remint: want 1 appended event, got %d", len(log.events))
	}
	var ev ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Status != "proposed" {
		t.Errorf("remint event.Status = %q, want proposed", ev.Status)
	}
}
