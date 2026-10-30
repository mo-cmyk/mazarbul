package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// recordingAppender is a tools.Appender stub that captures every event
// appended. It is distinct from fakeAppender (defined in remember_test.go)
// to avoid redeclaration; callers only need the event slice and a receipt
// to pass the "no receipt" guard.
type recordingAppender struct {
	events []kernel.Event
}

func (r *recordingAppender) Append(_ context.Context, evs []kernel.Event) ([]kernel.Receipt, error) {
	r.events = append(r.events, evs...)
	return []kernel.Receipt{{}}, nil
}

// openDedupStore opens a real SQLite store with all migrations applied and
// seeds a single person entity so the dedup tests have something to collide with.
// It returns the store, the seeded entity UID, and a cleanup function.
func openDedupStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const pippinUID = "01HZZZZZZZZZZZZZZZZPPPNTK1"
	const now = "2026-01-01T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	exec(
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		pippinUID, "person", "Pippin", 1.0, "confirmed", now,
	)
	return store, pippinUID
}

// TestCreateEntityDedup_ExactNameRefusesWithCandidates verifies that creating a
// person named "Pippin" when one already exists returns status
// duplicate_candidates with the existing uid and similarity 1.0 (exact_name
// reason), and that NO event was appended.
func TestCreateEntityDedup_ExactNameRefusesWithCandidates(t *testing.T) {
	t.Parallel()
	store, pippinUID := openDedupStore(t)
	matcher := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)
	rec := &recordingAppender{}
	tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Pippin",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status     string `json:"status"`
		Candidates []struct {
			UID        string  `json:"uid"`
			Similarity float64 `json:"similarity"`
			Reason     string  `json:"reason"`
		} `json:"candidates"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "duplicate_candidates" {
		t.Errorf("status = %q, want duplicate_candidates", out.Status)
	}
	if len(out.Candidates) == 0 {
		t.Fatal("candidates is empty, want at least one")
	}
	found := false
	for _, c := range out.Candidates {
		if c.UID == pippinUID {
			found = true
			if c.Similarity != 1.0 {
				t.Errorf("candidate similarity = %v, want 1.0", c.Similarity)
			}
			if c.Reason != "exact_name" {
				t.Errorf("candidate reason = %q, want exact_name", c.Reason)
			}
		}
	}
	if !found {
		t.Errorf("existing uid %q not in candidates %+v", pippinUID, out.Candidates)
	}
	if len(rec.events) != 0 {
		t.Errorf("appended %d events, want 0 (create must be refused)", len(rec.events))
	}
}

// TestCreateEntityDedup_IdentifierMatchReuses verifies that when an identifier
// matches an existing entity, Invoke returns status "reused" with the existing
// uid and appends no create event.
func TestCreateEntityDedup_IdentifierMatchReuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, pippinUID := openDedupStore(t)

	// Seed an email identifier for Pippin.
	const now = "2026-01-01T00:00:00Z"
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_identifiers (entity_uid, identifier, kind, source, added_at)
		 VALUES (?, ?, ?, ?, ?)`,
		pippinUID, "pippin@example.com", "email", "test", now,
	); err != nil {
		t.Fatalf("seed identifier: %v", err)
	}

	matcher := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)
	rec := &recordingAppender{}
	tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Chris",
		"identifiers": []map[string]string{
			{"type": "email", "value": "pippin@example.com"},
		},
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status string `json:"status"`
		UID    string `json:"uid"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "reused" {
		t.Errorf("status = %q, want reused", out.Status)
	}
	if out.UID != pippinUID {
		t.Errorf("uid = %q, want %q (existing entity)", out.UID, pippinUID)
	}
	if len(rec.events) != 0 {
		t.Errorf("appended %d events, want 0 on reuse", len(rec.events))
	}
}

// TestCreateEntityDedup_ForceMintsDespiteNameCollision verifies that when
// force=true is set, the create proceeds even when a same-name entity exists,
// returning status "created" and appending exactly one entity event.
func TestCreateEntityDedup_ForceMintsDespiteNameCollision(t *testing.T) {
	t.Parallel()
	store, _ := openDedupStore(t)
	matcher := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)
	rec := &recordingAppender{}
	tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Pippin",
		"force":       true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke with force=true: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "created" {
		t.Errorf("status = %q, want created (force bypasses dedup)", out.Status)
	}
	if len(rec.events) == 0 {
		t.Error("no events appended, want at least one entity event")
	}
	if string(rec.events[0].Kind) != "entity" {
		t.Errorf("event[0] kind = %q, want entity", rec.events[0].Kind)
	}
}

// TestCreateEntityDedup_ForceStillSurfacesCandidates verifies that a forced
// mint over a name collision still mints (status "created") AND reports the
// near-match it is knowingly forking from in candidates — only the refuse
// decision is suppressed by force, not the advisory.
func TestCreateEntityDedup_ForceStillSurfacesCandidates(t *testing.T) {
	t.Parallel()
	store, pippinUID := openDedupStore(t)
	matcher := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)
	rec := &recordingAppender{}
	tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Pippin",
		"force":       true,
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke with force=true: %v", err)
	}
	var out struct {
		Status     string `json:"status"`
		UID        string `json:"uid"`
		Candidates []struct {
			UID    string `json:"uid"`
			Reason string `json:"reason"`
		} `json:"candidates"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "created" {
		t.Errorf("status = %q, want created (force mints)", out.Status)
	}
	if out.UID == "" || out.UID == pippinUID {
		t.Errorf("forced mint uid = %q, want a fresh uid distinct from %q", out.UID, pippinUID)
	}
	if len(out.Candidates) == 0 {
		t.Fatal("force suppressed candidates; want the exact-name near-match surfaced as advisory")
	}
	found := false
	for _, c := range out.Candidates {
		if c.UID == pippinUID && c.Reason == "exact_name" {
			found = true
		}
	}
	if !found {
		t.Errorf("existing %q (exact_name) not in advisory candidates %+v", pippinUID, out.Candidates)
	}
	if len(rec.events) == 0 {
		t.Error("no events appended, want at least one entity event")
	}
}

// TestCreateEntityDedup_NoMatchCreates verifies that a brand-new display name
// with no existing match proceeds to mint normally (status "created").
func TestCreateEntityDedup_NoMatchCreates(t *testing.T) {
	t.Parallel()
	store, _ := openDedupStore(t)
	matcher := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)
	rec := &recordingAppender{}
	tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher)

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Definitely Not Pippin",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "created" {
		t.Errorf("status = %q, want created", out.Status)
	}
	if len(rec.events) == 0 {
		t.Error("no events appended, want entity event")
	}
}

// TestCreateEntityDedup_NilMatcherDegradesToCreate verifies that when no
// matcher is wired (WithDedup never called), the dedup gate is skipped entirely
// and the create succeeds even for a name that exists in the store.
func TestCreateEntityDedup_NilMatcherDegradesToCreate(t *testing.T) {
	t.Parallel()
	// Store is opened and "Pippin" is seeded, but we wire no matcher.
	store, _ := openDedupStore(t)
	_ = store // present in DB, but tool doesn't know about it
	rec := &recordingAppender{}
	tool := tools.NewCreateEntity(rec, nil) // no WithDedup

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Pippin",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke with nil matcher: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "created" {
		t.Errorf("status = %q, want created (nil matcher skips gate)", out.Status)
	}
}

// TestCreateEntityDedup_OutputValidatesAgainstSchema verifies that both the
// "created" and "duplicate_candidates" output envelopes carry all required
// fields as declared in OutputSchema: status is always present; created also
// carries name, uid, and kind; duplicate_candidates carries candidates with
// the required sub-fields.
func TestCreateEntityDedup_OutputValidatesAgainstSchema(t *testing.T) {
	t.Parallel()
	store, _ := openDedupStore(t)
	matcher := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)

	t.Run("created output has required fields", func(t *testing.T) {
		t.Parallel()
		rec := &recordingAppender{}
		tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher)
		req, _ := json.Marshal(map[string]any{
			"kind":        "person",
			"displayName": "Brandnew Person",
		})
		raw, err := tool.Invoke(context.Background(), req)
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var out struct {
			Name   string `json:"name"`
			UID    string `json:"uid"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		}
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			t.Fatalf("unmarshal: %v", uerr)
		}
		if out.Status != "created" {
			t.Errorf("status = %q, want created", out.Status)
		}
		if out.UID == "" {
			t.Error("uid is empty, want non-empty ULID")
		}
		if out.Name != "entities/"+out.UID {
			t.Errorf("name = %q, want entities/%s", out.Name, out.UID)
		}
		if out.Kind != "person" {
			t.Errorf("kind = %q, want person", out.Kind)
		}
	})

	t.Run("duplicate candidates output has required candidate fields", func(t *testing.T) {
		t.Parallel()
		rec := &recordingAppender{}
		tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher)
		req, _ := json.Marshal(map[string]any{
			"kind":        "person",
			"displayName": "Pippin",
		})
		raw, err := tool.Invoke(context.Background(), req)
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var out struct {
			Status     string `json:"status"`
			Candidates []struct {
				UID         string  `json:"uid"`
				DisplayName string  `json:"displayName"`
				Kind        string  `json:"kind"`
				Similarity  float64 `json:"similarity"`
				Reason      string  `json:"reason"`
			} `json:"candidates"`
		}
		if uerr := json.Unmarshal(raw, &out); uerr != nil {
			t.Fatalf("unmarshal: %v", uerr)
		}
		if out.Status != "duplicate_candidates" {
			t.Errorf("status = %q, want duplicate_candidates", out.Status)
		}
		if len(out.Candidates) == 0 {
			t.Fatal("candidates is empty")
		}
		c := out.Candidates[0]
		if c.UID == "" {
			t.Error("candidate.uid is empty")
		}
		if c.DisplayName == "" {
			t.Error("candidate.displayName is empty")
		}
		if c.Kind == "" {
			t.Error("candidate.kind is empty")
		}
		if c.Similarity == 0 {
			t.Error("candidate.similarity is 0")
		}
		if c.Reason == "" {
			t.Error("candidate.reason is empty")
		}
	})
}

// TestCreateEntityDedup_MatcherErrorFailsOpen verifies the fail-open contract:
// when the dedup matcher errors (an infra fault), createEntity does NOT block
// the write — it mints the entity anyway and records the degradation at debug so
// the silent fallback is observable (a spike there means dedup is effectively off
// and duplicates can slip back in). The store is closed before Invoke so every
// matcher query errors, forcing the fail-open branch.
func TestCreateEntityDedup_MatcherErrorFailsOpen(t *testing.T) {
	t.Parallel()
	store, _ := openDedupStore(t)
	matcher := entitymatch.NewLookup(store.Reader(), nil, nil, 0, 0)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)
	rec := &recordingAppender{}
	tool := tools.NewCreateEntity(rec, nil).WithDedup(matcher).WithLogger(zap.New(core))

	req, _ := json.Marshal(map[string]any{
		"kind":        "person",
		"displayName": "Pippin",
	})
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("unmarshal output: %v", uerr)
	}
	if out.Status != "created" {
		t.Errorf("status = %q, want created (a matcher error must fail open, never block the write)", out.Status)
	}
	if len(rec.events) != 1 {
		t.Errorf("appended %d events, want 1 (the write must still proceed)", len(rec.events))
	}
	if got := logs.FilterMessageSnippet("dedup matcher error").Len(); got == 0 {
		t.Error("expected a debug log recording the fail-open dedup degradation, got none")
	}
}

// Ensure mcp.CodeNotFound is referenced so the import is used if needed;
// this line confirms the test file is aware of the full code surface.
var _ = mcp.CodeNotFound
