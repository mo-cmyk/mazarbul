package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// selfTestPrincipalUID is the bound principal used across the self tests. Any
// stable string works - the binder reads it verbatim from state.toml.
const selfTestPrincipalUID = "PRINCIPAL00000000000000001"

// makeSelfRoot wires a minimal cobra root carrying just the self command and
// the global output flags it resolves through. The root-registration of
// newSelfCmd is lead-owned, so the test registers the command itself.
func makeSelfRoot(dataDir string, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", dataDir, "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newSelfCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	root.SetContext(context.Background())
	return root
}

// seedSelfIndexDB writes a projection at <dir>/index.db carrying the three
// surfaces `maz self` reads, and binds the local self anchor to a principal so
// the self<->principal pair is exercised:
//
//   - facts: two distinct facts on the principal, plus a DUPLICATE of one of
//     them on the "self" anchor (same stateKey+value) - proves dedup.
//   - memory_strength: the principal (5), the "self" anchor (2), and a separate
//     entity Mithril (3) - proves the self pair collapses to one summed row and
//     a non-self entity surfaces with its real count.
//   - insights: one seeded by the principal, one seeded by the "self" anchor.
func seedSelfIndexDB(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = store.Close() }()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// The "self" anchor entity is already seeded by migration 0012; add the
	// principal and a separate reinforced entity.
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, selfTestPrincipalUID, "person", "Frodo", 1.0, "confirmed", now, "personal")
	exec(ent, "mithril", "organization", "Mithril", 1.0, "confirmed", now, "personal")

	fact := `INSERT INTO facts (uid, subject_uid, state_key, value, create_time, source_event_uid, scope, source, confidence) VALUES (?,?,?,?,?,?,?,?,?)`
	exec(fact, "f_coffee", selfTestPrincipalUID, "preference.coffee", "flat white", now, "ev1", "personal", "cli:fact", 0.9)
	exec(fact, "f_role", selfTestPrincipalUID, "state.role", "founder", now, "ev2", "personal", "cli:fact", 1.0)
	// Same (stateKey, value) on the anchor - must collapse with the principal's.
	exec(fact, "f_coffee_anchor", "self", "preference.coffee", "flat white", now, "ev3", "personal", "extractor", 0.7)

	ms := `INSERT INTO memory_strength (target_uid, access_count, first_accessed_at, last_accessed_at, salience_base) VALUES (?,?,?,?,?)`
	exec(ms, selfTestPrincipalUID, 5, now, now, 0.5)
	exec(ms, "self", 2, now, now, 0.5)
	exec(ms, "mithril", 3, now, now, 0.5)

	ins := `INSERT INTO insights (uid, statement, insight_type, source_uids, seed_entity_uid, synthesized_confidence, current_confidence, state, source_event_uid, synthesized_at, create_time, scope)
	        VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	exec(ins, "ins_pref", "You prefer concise, direct answers.", "preference", `[]`, selfTestPrincipalUID, 0.85, 0.85, "hot", "ev4", now, now, "personal")
	exec(ins, "ins_pattern", "You ship features as one large PR.", "pattern", `[]`, "self", 0.75, 0.75, "hot", "ev5", now, now, "personal")

	if berr := config.BindSelfPrincipal(dir, selfTestPrincipalUID, "device-test", "key-test"); berr != nil {
		t.Fatalf("bind self principal: %v", berr)
	}
}

// TestSelfHelpHasExamples pins requirement 1: --help is non-empty and teaches
// with worked example invocations and the exit-code contract.
func TestSelfHelpHasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSelfRoot("", &buf, nil)
	root.SetArgs([]string{"self", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("self --help: %v", err)
	}
	out := buf.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("self --help produced empty output")
	}
	for _, want := range []string{"Examples:", "maz self", "--json", "Exit codes:", "Reinforced lately", "Insights about you"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n%s", want, out)
		}
	}
}

// TestSelfJSONShowsAllThreeSections pins requirement 3 and the acceptance
// criteria: --json emits a parseable, stable shape carrying all three sections,
// the reinforced entity has access_count >= 2, and the self/principal pair is
// collapsed (facts deduped, the self node shown once).
func TestSelfJSONShowsAllThreeSections(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedSelfIndexDB(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeSelfRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "self"})
	if err := root.Execute(); err != nil {
		t.Fatalf("self --json: %v (stderr=%q)", err, errBuf.String())
	}

	var view struct {
		Self struct {
			UID       string   `json:"uid"`
			Bound     bool     `json:"bound"`
			AliasUids []string `json:"aliasUids"`
		} `json:"self"`
		Facts []struct {
			StateKey string `json:"stateKey"`
			Value    string `json:"value"`
		} `json:"facts"`
		Reinforced []struct {
			UID         string `json:"uid"`
			DisplayName string `json:"displayName"`
			AccessCount int    `json:"accessCount"`
			Self        bool   `json:"self"`
		} `json:"reinforced"`
		Insights []struct {
			Statement string `json:"statement"`
			State     string `json:"state"`
		} `json:"insights"`
	}
	if err := json.Unmarshal(outBuf.Bytes(), &view); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, outBuf.String())
	}

	// Header: bound to the principal, both alias uids present.
	if view.Self.UID != selfTestPrincipalUID || !view.Self.Bound {
		t.Errorf("self header = %+v, want bound principal %q", view.Self, selfTestPrincipalUID)
	}
	if !slices.Contains(view.Self.AliasUids, selfTestPrincipalUID) || !slices.Contains(view.Self.AliasUids, "self") {
		t.Errorf("aliasUids = %v, want both principal and self anchor", view.Self.AliasUids)
	}

	// Facts: the duplicate (preference.coffee=flat white) collapses to one row.
	if len(view.Facts) != 2 {
		t.Fatalf("facts = %d, want 2 distinct (the duplicate must dedup)\n%+v", len(view.Facts), view.Facts)
	}
	coffee := 0
	for _, f := range view.Facts {
		if f.StateKey == "preference.coffee" {
			coffee++
		}
	}
	if coffee != 1 {
		t.Errorf("preference.coffee appears %d times, want exactly 1 (deduped across self+principal)", coffee)
	}

	// Reinforced: Mithril surfaces with its real count; the self pair is ONE row.
	var mithrilCount, selfRows int
	seenSelf := map[string]bool{selfTestPrincipalUID: true, "self": true}
	uidSeen := map[string]int{}
	for _, r := range view.Reinforced {
		uidSeen[r.UID]++
		if r.DisplayName == "Mithril" {
			mithrilCount = r.AccessCount
		}
		if r.Self || seenSelf[r.UID] {
			selfRows++
		}
	}
	if mithrilCount < 2 {
		t.Errorf("Mithril access_count = %d, want >= 2", mithrilCount)
	}
	if selfRows != 1 {
		t.Errorf("self appears in %d reinforced rows, want exactly 1 (pair collapsed)\n%+v", selfRows, view.Reinforced)
	}
	for uid, n := range uidSeen {
		if n > 1 {
			t.Errorf("uid %q appears %d times in reinforced; rows must be unique", uid, n)
		}
	}

	// Insights: both self-seeded insights surface, none duplicated.
	if len(view.Insights) != 2 {
		t.Fatalf("insights = %d, want 2 (principal-seeded + anchor-seeded)\n%+v", len(view.Insights), view.Insights)
	}
}

// TestSelfTextMode pins requirement 2: forced text mode (non-TTY) produces
// stable, titled output carrying the same data the JSON shape does.
func TestSelfTextMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedSelfIndexDB(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeSelfRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "self"})
	if err := root.Execute(); err != nil {
		t.Fatalf("self text: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	for _, want := range []string{
		"What I know about you", "Reinforced lately", "Insights about you",
		"preference.coffee", "flat white", "founder",
		"Mithril", "You prefer concise, direct answers.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\n%s", want, out)
		}
	}
}

// TestSelfBadScopeExitsOne pins the validation exit code: an unrecognised
// --scope is bad input → exit 1.
func TestSelfBadScopeExitsOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedSelfIndexDB(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeSelfRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "self", "--scope", "bogus"})
	err := root.Execute()
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("bad scope error = %T %v, want silentError", err, err)
	}
	if se.code != 1 {
		t.Errorf("bad scope exit code = %d, want 1", se.code)
	}
}

// TestSelfEmptyExitsZero pins requirement 4: an empty (migrated) projection is a
// success (exit 0) with friendly "nothing yet" notices, not an error.
func TestSelfEmptyExitsZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A migrated but otherwise empty projection (only the seeded self anchor).
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_ = store.Close()

	var outBuf, errBuf bytes.Buffer
	root := makeSelfRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "self"})
	if err := root.Execute(); err != nil {
		t.Fatalf("empty self should exit 0, got: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	for _, want := range []string{"What I know about you", "nothing recorded yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty self output missing %q\n%s", want, out)
		}
	}
}
