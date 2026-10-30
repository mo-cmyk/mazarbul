package dispatcher_test

// dispatcher_convergence_property_test.go — proof layer for the full-projection
// convergence guarantee and for the position-restricted fold that powers the
// two-ordering equivalence tests.
//
// D1d.1: TestFullProjection_PermutationConverges
//   50 random permutations (6 under -short) of a mixed event set applied to
//   SEPARATE fresh dispatchers all produce byte-identical canonical SQL dumps.
//   This is the end-to-end proof that entities + facts + team_membership_acl
//   converge independently of arrival order.
//
// D1d.2: TestGraftBatch_EquivalentToCanonicalRebuild
//   Two interleaved Apply batches (simulating two pulls from different devices)
//   produce the same canonical dump as a single canonical-order apply.
//   Idempotency (replay of batch1 after batch2) must not perturb the result.
//
// D1c.3: TestTeamScope_ReaderDemoteAfterWrite_TwoOrderings
//   Membership fold + AuthorizeTeamWrite: a pre-demote content event authored by
//   A is accepted by graft in BOTH orderings (demotion arrives before or after
//   the event). The live-append gate blocks post-demote writes in both orderings.
//   Canonical dumps are identical across both orderings.
//
// D1c.4: TestTeamScope_RevocationTwoOrderings
//   Revoke event + content events: post-revocation live writes are denied in
//   both orderings; pre-revocation writes are retained; canonical dumps identical.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fullProjectionEventSet builds a stable mixed event set for permutation tests:
// genesis + member grant (vouched, to project entity rows), two fact slots with
// competing HLC writes, and a role change. UIDs are strictly increasing.
func fullProjectionEventSet(t *testing.T) []kernel.Event {
	t.Helper()
	t0 := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	return []kernel.Event{
		// Membership
		grantEventVouched(t, "01FPE0000000000000000001", "G", "G", "Genesis"),
		grantEventVouched(t, "01FPE0000000000000000002", "G", "A", "Member A"),
		roleChangeEvent(t, "01FPE0000000000000000005", "G", "A", ontology.RoleMember),
		// Fact slot 1: A writes alpha (HLC-1), then beta (HLC-2) → beta wins
		makeHLCFactEvent(t, "01FPE0000000000000000003", "pref.theme", "alpha", t0, "000001a00000cc100000"),
		makeHLCFactEvent(t, "01FPE0000000000000000004", "pref.theme", "beta", t0.Add(time.Second), "000001a00000cc200000"),
		// Fact slot 2: independent single write
		makeHLCFactEvent(t, "01FPE0000000000000000006", "state.mode", "active", t0.Add(2*time.Second), "000001a00000cc300000"),
	}
}

// canonicalProjectionDump returns a stable string of the derived tables in d:
// team_membership_acl (by principal_uid), entities (by uid), and open facts
// (by subject+key). This is intentionally narrower than the daemon-layer
// canonicalDump so dispatcher tests are not coupled to migration details.
func canonicalProjectionDump(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher) string {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var sb strings.Builder
	dumpProjectionSection(t, ctx, &sb, reader, "=acl\n",
		`SELECT principal_uid, is_genesis, role FROM team_membership_acl ORDER BY principal_uid`)
	dumpProjectionSection(t, ctx, &sb, reader, "=entities\n",
		`SELECT uid, scope, display_name FROM entities ORDER BY uid`)
	dumpProjectionSection(t, ctx, &sb, reader, "=facts_open\n",
		`SELECT subject_uid, state_key, value FROM facts
		  WHERE COALESCE(event_end,'')='' AND COALESCE(retracted_at,'')=''
		  ORDER BY subject_uid, state_key`)
	return sb.String()
}

// dumpProjectionSection appends one named table section to sb.
func dumpProjectionSection(t *testing.T, ctx context.Context, sb *strings.Builder, db *sql.DB, header, query string) {
	t.Helper()
	sb.WriteString(header)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("projection dump query: %v\nSQL: %s", err, query)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("projection dump columns: %v", err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if serr := rows.Scan(ptrs...); serr != nil {
			t.Fatalf("projection dump scan: %v", serr)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%v", v)
		}
		fmt.Fprintln(sb, strings.Join(parts, "\t"))
	}
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("projection dump rows: %v", rerr)
	}
}

// shuffledCopy returns a new slice holding the elements of src in a
// deterministically random order drawn from rng. It is safe to call
// sequentially; rng must not be shared with any concurrent caller.
func shuffledCopy(src []kernel.Event, rng *rand.Rand) []kernel.Event {
	out := make([]kernel.Event, len(src))
	copy(out, src)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestFullProjection_PermutationConverges is D1d.1: 50 random permutations (6
// under -short) of the mixed event set applied to separate fresh dispatchers
// all produce byte-identical canonical dumps.
func TestFullProjection_PermutationConverges(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	events := fullProjectionEventSet(t)

	trials := 50
	if testing.Short() {
		trials = 6
	}

	// Canonical reference: events in definition order.
	refDisp := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	if err := refDisp.Apply(ctx, events); err != nil {
		t.Fatalf("ref Apply: %v", err)
	}
	wantDump := canonicalProjectionDump(t, ctx, refDisp)

	// Pre-compute all shuffles in the parent goroutine (sequential, no race).
	rng := rand.New(rand.NewPCG(0xBA5EBA11_DEADBEEF, 0xCA11AB1E_F00DCAFE))
	type trialData struct {
		n       int
		shuffle []kernel.Event
	}
	tds := make([]trialData, trials)
	for i := range trials {
		tds[i] = trialData{n: i, shuffle: shuffledCopy(events, rng)}
	}

	for _, td := range tds {
		t.Run(fmt.Sprintf("trial %d", td.n), func(t *testing.T) {
			t.Parallel()
			d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
			if err := d.Apply(ctx, td.shuffle); err != nil {
				t.Fatalf("trial %d Apply: %v", td.n, err)
			}
			got := canonicalProjectionDump(t, ctx, d)
			if got != wantDump {
				t.Errorf("trial %d: dump diverged\nWANT:\n%s\nGOT:\n%s", td.n, wantDump, got)
			}
		})
	}
}

// TestGraftBatch_EquivalentToCanonicalRebuild is D1d.2: two interleaved Apply
// batches applied to a second dispatcher produce the same canonical dump as
// canonical single-pass apply. Idempotency (replay of batch1 after batch2) must
// not perturb the result.
func TestGraftBatch_EquivalentToCanonicalRebuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: two dispatchers for batch-vs-rebuild comparison")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	events := fullProjectionEventSet(t)

	// Canonical reference: single Apply of all events.
	dRef := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	if err := dRef.Apply(ctx, events); err != nil {
		t.Fatalf("canonical Apply: %v", err)
	}
	want := canonicalProjectionDump(t, ctx, dRef)

	// Two-batch apply: first half, then second half, then replay first half.
	mid := len(events) / 2
	batch1, batch2 := events[:mid], events[mid:]

	dBatch := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	if err := dBatch.Apply(ctx, batch1); err != nil {
		t.Fatalf("batch1 Apply: %v", err)
	}
	if err := dBatch.Apply(ctx, batch2); err != nil {
		t.Fatalf("batch2 Apply: %v", err)
	}
	// Replay batch1: must be idempotent — same UIDs, no state change.
	if err := dBatch.Apply(ctx, batch1); err != nil {
		t.Fatalf("batch1 replay Apply: %v", err)
	}
	got := canonicalProjectionDump(t, ctx, dBatch)
	if got != want {
		t.Errorf("batch dump diverged from canonical\nWANT:\n%s\nGOT:\n%s", want, got)
	}
}

// TestTeamScope_ReaderDemoteAfterWrite_TwoOrderings is D1c.3.
// A fact authored by A at position U5 (before A is demoted to reader at U7)
// must be present in the derived tables in BOTH orderings (fact-then-demote and
// demote-then-fact). AuthorizeTeamWrite must deny A's future writes in both.
// Canonical dumps are byte-identical.
func TestTeamScope_ReaderDemoteAfterWrite_TwoOrderings(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	genesisGrant := grantEvent(t, "01DM0000000000000000000001", "G", "G", 0)
	admitA := grantEvent(t, "01DM0000000000000000000003", "G", "A", 0)
	// A's fact at U5: authored while A is a full member — before the demotion at U7.
	aFact := makeHLCFactEvent(t, "01DM0000000000000000000005", "state.owner", "A", time.Now(), "000001a00000dc100000")
	aFact.Scope = kernel.ScopeTeam
	aFact.Author = kernel.EventAuthor{PrincipalUID: "A", DeviceID: "A-dev", SigKeyID: "kid:A"}
	demoteA := roleChangeEvent(t, "01DM0000000000000000000007", "G", "A", ontology.RoleReader)

	type ordering struct {
		name   string
		events []kernel.Event
	}
	orderings := []ordering{
		{"fact then demote", []kernel.Event{genesisGrant, admitA, aFact, demoteA}},
		{"demote then fact", []kernel.Event{genesisGrant, admitA, demoteA, aFact}},
	}

	// Compute canonical reference from ordering 0 before subtests start.
	refD := newDispatcher(t, ctx, nil)
	if err := refD.Apply(ctx, orderings[0].events); err != nil {
		t.Fatalf("reference Apply: %v", err)
	}
	wantDump := canonicalProjectionDump(t, ctx, refD)

	for _, ord := range orderings {
		t.Run(ord.name, func(t *testing.T) {
			t.Parallel()
			d := newDispatcher(t, ctx, nil)
			if err := d.Apply(ctx, ord.events); err != nil {
				t.Fatalf("%s Apply: %v", ord.name, err)
			}

			// Canonical dump must match regardless of ordering.
			if got := canonicalProjectionDump(t, ctx, d); got != wantDump {
				t.Errorf("ordering %q: dump diverged\nWANT:\n%s\nGOT:\n%s", ord.name, wantDump, got)
			}

			// A's pre-demote fact at U5 must survive in both orderings.
			reader, err := d.SQLiteReader()
			if err != nil {
				t.Fatalf("SQLiteReader: %v", err)
			}
			var factCount int
			if qerr := reader.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM facts
				  WHERE state_key='state.owner' AND value='A'
				  AND COALESCE(event_end,'')='' AND COALESCE(retracted_at,'')=''`).Scan(&factCount); qerr != nil {
				t.Fatalf("%s: query pre-demote fact: %v", ord.name, qerr)
			}
			if factCount != 1 {
				t.Errorf("ordering %q: pre-demote fact count = %d, want 1", ord.name, factCount)
			}

			// Live-append gate: A (now reader) must be denied a new team write.
			postWrite := makeHLCFactEvent(t, "01DM0000000000000000000009", "state.owner", "A2", time.Now(), "000001a00000dc200000")
			postWrite.Scope = kernel.ScopeTeam
			postWrite.Author = kernel.EventAuthor{PrincipalUID: "A", DeviceID: "A-dev", SigKeyID: "kid:A"}
			if err := d.AuthorizeTeamWrite(ctx, &postWrite, "A"); !errors.Is(err, kernel.ErrPermission) {
				t.Errorf("ordering %q: A (reader) post-demote write: %v, want ErrPermission", ord.name, err)
			}

			// Genesis owner G is unaffected.
			if err := d.AuthorizeTeamWrite(ctx, &postWrite, "G"); err != nil {
				t.Errorf("ordering %q: G (genesis) write denied: %v", ord.name, err)
			}
		})
	}
}

// TestTeamScope_RevocationTwoOrderings is D1c.4.
// Applying a revoke event with pre- and post-revocation content events in two
// orderings produces identical ACL state. A is absent from the ACL in both.
// TeamMemberRoleAt at U5 (before revoke at U7) returns "member" in both,
// proving GraftSealed would accept A's pre-revoke fact. Post-revoke live write
// is denied in both orderings.
func TestTeamScope_RevocationTwoOrderings(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	genesisGrant := grantEvent(t, "01RV0000000000000000000001", "G", "G", 0)
	admitA := grantEvent(t, "01RV0000000000000000000003", "G", "A", 0)
	// Pre-revoke fact from A at U5 — authored while A was a member.
	preRevokeFact := makeHLCFactEvent(t, "01RV0000000000000000000005", "state.phase", "active", time.Now(), "000001a00000ec100000")
	preRevokeFact.Scope = kernel.ScopeTeam
	preRevokeFact.Author = kernel.EventAuthor{PrincipalUID: "A", DeviceID: "A-dev", SigKeyID: "kid:A"}
	revokeA := revokeEvent(t, "01RV0000000000000000000007", "G", "A")

	type ordering struct {
		name   string
		events []kernel.Event
	}
	orderings := []ordering{
		{"canonical order", []kernel.Event{genesisGrant, admitA, preRevokeFact, revokeA}},
		{"revoke before pre-revoke fact", []kernel.Event{genesisGrant, admitA, revokeA, preRevokeFact}},
	}

	// Canonical reference from ordering 0.
	refD := newDispatcher(t, ctx, nil)
	if err := refD.Apply(ctx, orderings[0].events); err != nil {
		t.Fatalf("reference Apply: %v", err)
	}
	wantDump := canonicalProjectionDump(t, ctx, refD)

	for _, ord := range orderings {
		t.Run(ord.name, func(t *testing.T) {
			t.Parallel()
			d := newDispatcher(t, ctx, nil)
			if err := d.Apply(ctx, ord.events); err != nil {
				t.Fatalf("%s Apply: %v", ord.name, err)
			}

			// Dump must match.
			if got := canonicalProjectionDump(t, ctx, d); got != wantDump {
				t.Errorf("ordering %q: dump diverged\nWANT:\n%s\nGOT:\n%s", ord.name, wantDump, got)
			}

			// A must NOT be in the ACL (revoked in both orderings).
			for _, p := range aclMembers(t, ctx, d) {
				if p == "A" {
					t.Errorf("ordering %q: revoked A still in ACL", ord.name)
				}
			}

			// Pre-revoke fact at U5 IS present in both orderings.
			// (Apply does not filter facts by membership; that is GraftSealed's job.)
			reader, err := d.SQLiteReader()
			if err != nil {
				t.Fatalf("SQLiteReader: %v", err)
			}
			var factCount int
			if qerr := reader.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM facts
				  WHERE state_key='state.phase' AND value='active'
				  AND COALESCE(event_end,'')='' AND COALESCE(retracted_at,'')=''`).Scan(&factCount); qerr != nil {
				t.Fatalf("%s: query pre-revoke fact: %v", ord.name, qerr)
			}
			if factCount != 1 {
				t.Errorf("ordering %q: pre-revoke fact count = %d, want 1", ord.name, factCount)
			}

			// TeamMemberRoleAt at U6 (between admit at U3 and revoke at U7)
			// says A was a member — this is what GraftSealed uses to accept A's U5 fact.
			role, ok, err := d.TeamMemberRoleAt(ctx, "A", "01RV0000000000000000000006")
			if err != nil {
				t.Fatalf("%s: TeamMemberRoleAt: %v", ord.name, err)
			}
			if !ok || role != ontology.RoleMember {
				t.Errorf("ordering %q: TeamMemberRoleAt(A, before=U6) = (%q, %v), want (member, true)",
					ord.name, role, ok)
			}

			// Live-append gate: revoked A is denied a new team write.
			postRevoke := makeHLCFactEvent(t, "01RV0000000000000000000009", "state.phase", "gone", time.Now(), "000001a00000ec200000")
			postRevoke.Scope = kernel.ScopeTeam
			postRevoke.Author = kernel.EventAuthor{PrincipalUID: "A", DeviceID: "A-dev", SigKeyID: "kid:A"}
			if err := d.AuthorizeTeamWrite(ctx, &postRevoke, "A"); !errors.Is(err, kernel.ErrPermission) {
				t.Errorf("ordering %q: post-revoke write by A: %v, want ErrPermission", ord.name, err)
			}
		})
	}
}
