package navigate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// newFilterNavigator seeds a small CRM-shaped graph for the filter compiler:
//
//	alice   (person, confirmed)   task_status=pending, firstName=Steven, works_at Mithril
//	bob     (person, confirmed)   task_status=active
//	carol   (person, tentative)   no fact slots
//	deleted (person, deleted)     no fact slots
//	mithril (organization, confirmed)
//
// The fact-slot key is task_status (NOT status) because status is a reserved
// column alias for resolution_status in the AIP-160 filter compiler; using
// task_status avoids shadowing and lets us test both code paths independently.
func newFilterNavigator(t *testing.T) *navigate.Navigator {
	t.Helper()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "alice", "person", "Merry", 1.0, "confirmed", now)
	exec(ent, "bob", "person", "Sam", 0.9, "confirmed", now)
	exec(ent, "carol", "person", "Pippin", 0.8, "tentative", now)
	exec(ent, "deleted_ent", "person", "Shadowfax", 0.7, "deleted", now)
	exec(ent, "mithril", "organization", "Mithril", 1.0, "confirmed", now)

	fact := `INSERT INTO facts (uid, subject_uid, state_key, value, event_start, event_end, create_time, source_event_uid, content_hash, superseded_by_uid, source_priority) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	exec(fact, "f_a_status", "alice", "task_status", "pending", nil, nil, now, "f_a_status", "h1", nil, 1.0)
	exec(fact, "f_a_name", "alice", "first_name", "Steven", nil, nil, now, "f_a_name", "h2", nil, 1.0)
	exec(fact, "f_b_status", "bob", "task_status", "active", nil, nil, now, "f_b_status", "h3", nil, 1.0)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	exec(rel, "r_works", "alice", "works_at", "mithril", 1.0, "ep1", now, nil, nil, nil, `{}`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

// uids collects the entity UIDs of a list result for set assertions.
func uids(es []navigate.Entity) map[string]bool {
	m := make(map[string]bool, len(es))
	for _, e := range es {
		m[e.UID] = true
	}
	return m
}

func TestListEntities_FilterByFactSlot(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `kind = "person" AND task_status = "pending"`,
	})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if m := uids(got); len(m) != 1 || !m["alice"] {
		t.Errorf("got %v, want only alice", m)
	}
}

func TestListEntities_FilterByRelated(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `related("works_at", "Mithril")`,
	})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if m := uids(got); len(m) != 1 || !m["alice"] {
		t.Errorf("got %v, want only alice (works_at Mithril)", m)
	}
}

func TestListEntities_FilterNotEquals(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `kind = "person" AND task_status != "pending"`,
	})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	// bob (active) qualifies; alice (pending) does not. Other persons with no
	// task_status fact also qualify under "!= pending", so we assert membership
	// rather than an exact set.
	if m := uids(got); m["alice"] || !m["bob"] {
		t.Errorf("got %v, want bob present and alice absent", m)
	}
}

func TestListEntities_FilterHasSubstring(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	// `:` is a case-insensitive substring match on a column.
	got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `displayName : "err"`,
	})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if m := uids(got); len(m) != 1 || !m["alice"] {
		t.Errorf("got %v, want only alice (Merry contains 'err')", m)
	}
}

func TestCountEntities(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	// Fact-slot counts are unambiguous (the seeded "self" person has no facts).
	n, err := nav.CountEntities(context.Background(), navigate.EntityFilter{Filter: `task_status = "pending"`})
	if err != nil {
		t.Fatalf("CountEntities(task_status): %v", err)
	}
	if n != 1 {
		t.Errorf("count task_status=pending = %d, want 1", n)
	}
	n, err = nav.CountEntities(context.Background(), navigate.EntityFilter{Filter: `first_name = "Steven"`})
	if err != nil {
		t.Fatalf("CountEntities(first_name): %v", err)
	}
	if n != 1 {
		t.Errorf("count first_name=Steven = %d, want 1", n)
	}
}

// TestListEntities_KindIsCaseInsensitive proves a capitalized --kind filter
// ("Person") matches the lowercase stored kind ("person"). An agent that
// capitalizes the kind otherwise gets an empty result and wrongly concludes no
// such entities exist.
func TestListEntities_KindIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	lower, err := nav.ListEntities(context.Background(), navigate.EntityFilter{Kind: "person"})
	if err != nil {
		t.Fatalf("ListEntities(person): %v", err)
	}
	upper, err := nav.ListEntities(context.Background(), navigate.EntityFilter{Kind: "Person"})
	if err != nil {
		t.Fatalf("ListEntities(Person): %v", err)
	}
	if len(upper) == 0 || len(upper) != len(lower) {
		t.Errorf("Kind %q returned %d entities, want same as %q (%d)", "Person", len(upper), "person", len(lower))
	}
}

func TestAggregateEntities_ByFactSlot(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	groups, err := nav.AggregateEntities(context.Background(), navigate.AggregateSpec{
		Kind:    "person",
		GroupBy: "task_status",
	})
	if err != nil {
		t.Fatalf("AggregateEntities: %v", err)
	}
	counts := map[string]int64{}
	for _, g := range groups {
		counts[g.Value] = g.Count
	}
	if counts["pending"] != 1 || counts["active"] != 1 {
		t.Errorf("task_status buckets = %+v, want pending=1 active=1", counts)
	}
}

func TestAggregateEntities_ByKind(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	groups, err := nav.AggregateEntities(context.Background(), navigate.AggregateSpec{GroupBy: "kind"})
	if err != nil {
		t.Fatalf("AggregateEntities: %v", err)
	}
	counts := map[string]int64{}
	for _, g := range groups {
		counts[g.Value] = g.Count
	}
	// person includes the seeded "self" plus alice + bob (>= 2); organization is
	// exactly Mithril.
	if counts["organization"] != 1 || counts["person"] < 2 {
		t.Errorf("kind buckets = %+v, want organization=1 and person>=2", counts)
	}
}

func TestListEntities_BadFilterErrors(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	if _, err := nav.ListEntities(context.Background(), navigate.EntityFilter{Filter: `kind =`}); err == nil {
		t.Error("expected error for malformed filter, got nil")
	}
}

// TestListEntities_FilterByConfidenceRange proves range operators still work on
// the confidence column (REAL), with a real numeric comparison rather than a
// lexical one: bob (0.9) is excluded by `> 0.95` while alice (1.0) is kept.
func TestListEntities_FilterByConfidenceRange(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `confidence > 0.95`,
	})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if m := uids(got); !m["alice"] || !m["mithril"] || m["bob"] {
		t.Errorf("got %v, want alice+mithril present and bob (0.9) absent", m)
	}
}

// TestListEntities_FilterByCreateTimeRange proves range operators still work on
// the createTime column (sortable RFC3339 text). A timestamp in the past keeps
// every seeded entity that carries a create_time.
func TestListEntities_FilterByCreateTimeRange(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `createTime > "2000-01-01T00:00:00Z"`,
	})
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if m := uids(got); !m["alice"] || !m["bob"] || !m["mithril"] {
		t.Errorf("got %v, want alice+bob+mithril present (all created after 2000)", m)
	}
}

// TestListEntities_RangeOnFactSlotRejected verifies that a range operator on a
// fact slot (stored as TEXT) is rejected with ErrInvalidFilter rather than
// emitting silently-wrong lexical SQL. The MCP/CLI surface maps this to
// E_VALIDATION.
func TestListEntities_RangeOnFactSlotRejected(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	for _, filter := range []string{
		`age > 9`,
		`age >= 9`,
		`age < 100`,
		`task_status <= "x"`,
	} {
		_, err := nav.ListEntities(context.Background(), navigate.EntityFilter{Filter: filter})
		if err == nil {
			t.Errorf("filter %q: expected error, got nil", filter)
			continue
		}
		if !errors.Is(err, navigate.ErrInvalidFilter) {
			t.Errorf("filter %q: got %v, want ErrInvalidFilter", filter, err)
		}
	}
}

// TestListEntities_FactSlotEqualityStillWorks guards that =, != and : on fact
// slots are unaffected by the range rejection.
func TestListEntities_FactSlotEqualityStillWorks(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)
	cases := []struct {
		name   string
		filter string
		want   string // single expected uid
	}{
		{"equals", `task_status = "pending"`, "alice"},
		{"substring", `task_status : "pend"`, "alice"},
		{"notEquals", `task_status != "pending"`, "bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{Filter: tc.filter})
			if err != nil {
				t.Fatalf("ListEntities(%q): %v", tc.filter, err)
			}
			m := uids(got)
			if !m[tc.want] {
				t.Errorf("filter %q: got %v, want %q present", tc.filter, m, tc.want)
			}
			// The opposite-task_status person must be excluded by =/:; for != the
			// equals subject (alice) must be excluded.
			if tc.name == "notEquals" && m["alice"] {
				t.Errorf("filter %q: alice should be absent, got %v", tc.filter, m)
			}
			if tc.name != "notEquals" && m["bob"] {
				t.Errorf("filter %q: bob should be absent, got %v", tc.filter, m)
			}
		})
	}
}

// TestListEntities_StatusEnumValidation verifies that a status/resolutionStatus
// filter with a value not in the legal set (resolved, confirmed, tentative,
// split_candidate, deleted) is rejected with ErrInvalidFilter rather than
// silently returning an empty or full result.
func TestListEntities_StatusEnumValidation(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)

	invalidValues := []string{
		`status = "pending"`,
		`status = "active"`,
		`status = "unknown"`,
		`status = "CONFIRMED"`, // wrong case
		`resolutionStatus = "open"`,
		`resolutionStatus = "closed"`,
	}
	for _, filter := range invalidValues {
		t.Run(filter, func(t *testing.T) {
			t.Parallel()
			_, err := nav.ListEntities(context.Background(), navigate.EntityFilter{Filter: filter})
			if err == nil {
				t.Errorf("filter %q: expected ErrInvalidFilter, got nil", filter)
				return
			}
			if !errors.Is(err, navigate.ErrInvalidFilter) {
				t.Errorf("filter %q: got %v, want ErrInvalidFilter", filter, err)
			}
		})
	}

	// Valid values must still work (resolved is the value seed entities use).
	validValues := []struct {
		filter  string
		wantAny bool // just check no error
	}{
		{`status = "resolved"`, true},
		{`status = "confirmed"`, true},
		{`status = "tentative"`, true},
		{`status = "split_candidate"`, true},
		{`status = "deleted"`, true},
		{`resolutionStatus = "confirmed"`, true},
		{`status != "deleted"`, true},
	}
	for _, tc := range validValues {
		t.Run(tc.filter, func(t *testing.T) {
			t.Parallel()
			_, err := nav.ListEntities(context.Background(), navigate.EntityFilter{Filter: tc.filter})
			if err != nil {
				t.Errorf("filter %q: unexpected error: %v", tc.filter, err)
			}
		})
	}
}

// TestListEntities_StatusAliasesResolutionStatus proves that the `status`
// filter field is an alias for the resolution_status entity column, not a
// fact-slot lookup. Bug: before the fix, `status = "confirmed"` routed to
// a fact EXISTS subquery (state_key="status") and returned [] even when
// entities had resolution_status="confirmed", while `status != "confirmed"`
// returned the full set (no entity had such a fact, so NOT EXISTS always
// matched).
func TestListEntities_StatusAliasesResolutionStatus(t *testing.T) {
	t.Parallel()
	nav := newFilterNavigator(t)

	// Two entities have resolution_status="confirmed" (alice, bob) in the
	// base person set; carol is "tentative" and deleted_ent is "deleted".
	// Filter by status = "confirmed": must include alice and bob, must exclude
	// carol (tentative) and deleted_ent (deleted).
	got, err := nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `kind = "person" AND status = "confirmed"`,
	})
	if err != nil {
		t.Fatalf("ListEntities(status=confirmed): %v", err)
	}
	m := uids(got)
	if !m["alice"] || !m["bob"] {
		t.Errorf("status=confirmed: got %v, want alice and bob present", m)
	}
	if m["carol"] || m["deleted_ent"] {
		t.Errorf("status=confirmed: got %v, want carol and deleted_ent absent", m)
	}

	// Filter by status != "confirmed": must include carol (tentative) and
	// deleted_ent (deleted), must exclude alice and bob (confirmed).
	got, err = nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `kind = "person" AND status != "confirmed"`,
	})
	if err != nil {
		t.Fatalf("ListEntities(status!=confirmed): %v", err)
	}
	m = uids(got)
	if !m["carol"] || !m["deleted_ent"] {
		t.Errorf("status!=confirmed: got %v, want carol and deleted_ent present", m)
	}
	if m["alice"] || m["bob"] {
		t.Errorf("status!=confirmed: got %v, want alice and bob absent", m)
	}

	// Filter by status != "deleted": must exclude deleted_ent, must include
	// alice, bob, carol (all non-deleted).
	got, err = nav.ListEntities(context.Background(), navigate.EntityFilter{
		Filter: `kind = "person" AND status != "deleted"`,
	})
	if err != nil {
		t.Fatalf("ListEntities(status!=deleted): %v", err)
	}
	m = uids(got)
	if m["deleted_ent"] {
		t.Errorf("status!=deleted: got %v, want deleted_ent absent", m)
	}
	if !m["alice"] || !m["bob"] || !m["carol"] {
		t.Errorf("status!=deleted: got %v, want alice, bob, carol present", m)
	}
}
