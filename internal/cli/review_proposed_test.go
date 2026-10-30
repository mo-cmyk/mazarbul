package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// makeReviewRoot wires a minimal cobra root carrying the review command group
// and the global output flags the command resolves through.
func makeReviewRoot(dataDir string, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", dataDir, "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newReviewCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	root.SetContext(context.Background())
	return root
}

// seedProposedIndexDB writes a projection at <dir>/index.db carrying a mix of
// proposed and asserted edges across scopes, each with a connector source
// recorded in provenance. It mirrors the rows `maz review proposed` reads.
//
//	(personal) Frodo --met--> Gandalf      proposed, connector:granola
//	(personal) Frodo --gave--> Sam         proposed, connector:claude-chats
//	(personal) Frodo --owns--> Sting       asserted (must never surface)
//	(team)     Aragorn --rules--> Gondor   proposed, connector:granola (team-only)
func seedProposedIndexDB(t *testing.T, dir string) {
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

	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, "frodo", "person", "Frodo", 1.0, "confirmed", now, "personal")
	exec(ent, "gandalf", "person", "Gandalf", 1.0, "confirmed", now, "personal")
	exec(ent, "sam", "person", "Sam", 1.0, "confirmed", now, "personal")
	exec(ent, "sting", "object", "Sting", 1.0, "confirmed", now, "personal")
	exec(ent, "aragorn", "person", "Aragorn", 1.0, "confirmed", now, "team")
	exec(ent, "gondor", "place", "Gondor", 1.0, "confirmed", now, "team")

	ep := `INSERT INTO episodes (uid, create_time, kind, payload_json, scope) VALUES (?,?,?,?,?)`
	exec(ep, "ev_granola_1", now, "episode", `{"text":"t","source":"connector:granola","confidence":0.7}`, "personal")
	exec(ep, "ev_claude_1", now, "episode", `{"text":"t","source":"connector:claude-chats","confidence":0.6}`, "personal")
	exec(ep, "ev_user_1", now, "episode", `{"text":"t","source":"cli:remember","confidence":1.0}`, "personal")
	exec(ep, "ev_granola_team", now, "episode", `{"text":"t","source":"connector:granola","confidence":0.7}`, "team")

	prov := `INSERT INTO provenance (event_uid, source, confidence, decided_at) VALUES (?,?,?,?)`
	exec(prov, "ev_granola_1", "connector:granola", 0.7, now)
	exec(prov, "ev_claude_1", "connector:claude-chats", 0.6, now)
	exec(prov, "ev_user_1", "cli:remember", 1.0, now)
	exec(prov, "ev_granola_team", "connector:granola", 0.7, now)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes, scope, status)
	        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
	exec(rel, "r_met", "frodo", "met", "gandalf", 0.7, "ev_granola_1", now, nil, nil, nil, `{}`, "personal", "proposed")
	exec(rel, "r_gave", "frodo", "gave", "sam", 0.6, "ev_claude_1", now, nil, nil, nil, `{}`, "personal", "proposed")
	exec(rel, "r_owns", "frodo", "owns", "sting", 1.0, "ev_user_1", now, nil, nil, nil, `{}`, "personal", "asserted")
	exec(rel, "r_rules", "aragorn", "rules", "gondor", 0.7, "ev_granola_team", now, nil, nil, nil, `{}`, "team", "proposed")
}

// TestReviewProposedHelpHasExamples pins requirement 1: --help is non-empty and
// teaches with worked example invocations.
func TestReviewProposedHelpHasExamples(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeReviewRoot("", &buf, nil)
	root.SetArgs([]string{"review", "proposed", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("review proposed --help: %v", err)
	}
	out := buf.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("review proposed --help produced empty output")
	}
	for _, want := range []string{"Examples:", "maz review proposed", "--approve-all", "--connector", "Exit codes:"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n%s", want, out)
		}
	}
}

// TestReviewProposedJSONListsQueue pins requirement 3: --json emits a parseable,
// stable shape carrying the proposed edges, the connector summary, and the
// count — and proves scope isolation (the team edge never appears).
func TestReviewProposedJSONListsQueue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedProposedIndexDB(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "review", "proposed"})
	if err := root.Execute(); err != nil {
		t.Fatalf("review proposed --json: %v (stderr=%q)", err, errBuf.String())
	}

	var res struct {
		Proposed []struct {
			Name, Subject, Predicate, Object, Connector, Scope string
			Confidence                                         float64
		} `json:"proposed"`
		Connectors []struct {
			Connector string
			Count     int
		} `json:"connectors"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(outBuf.Bytes(), &res); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, outBuf.String())
	}
	if res.Count != 2 || len(res.Proposed) != 2 {
		t.Fatalf("count=%d proposed=%d, want 2 personal edges (team excluded)\n%s", res.Count, len(res.Proposed), outBuf.String())
	}
	for _, r := range res.Proposed {
		if r.Scope == "team" {
			t.Errorf("team-scope edge leaked into a personal review: %+v", r)
		}
		if !strings.HasPrefix(r.Name, "relations/") {
			t.Errorf("name %q is not a relations/{uid} resource name", r.Name)
		}
	}
	// Connector summary present and counted.
	if len(res.Connectors) != 2 {
		t.Errorf("connectors summary = %+v, want two connectors", res.Connectors)
	}
}

// TestReviewProposedConnectorFilterJSON pins --connector narrowing in JSON mode.
func TestReviewProposedConnectorFilterJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedProposedIndexDB(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "review", "proposed", "--connector", "connector:claude-chats"})
	if err := root.Execute(); err != nil {
		t.Fatalf("review proposed --connector: %v (stderr=%q)", err, errBuf.String())
	}
	var res struct {
		Proposed []struct{ Name, Connector string } `json:"proposed"`
		Count    int                                `json:"count"`
	}
	if err := json.Unmarshal(outBuf.Bytes(), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, outBuf.String())
	}
	if res.Count != 1 || len(res.Proposed) != 1 || res.Proposed[0].Connector != "connector:claude-chats" {
		t.Fatalf("connector filter = %+v, want exactly the claude-chats edge", res)
	}
}

// TestReviewProposedTextMode pins requirement 2: forced text mode (non-TTY)
// produces stable, grouped output with a footer nudging --approve-all.
func TestReviewProposedTextMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedProposedIndexDB(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "review", "proposed"})
	if err := root.Execute(); err != nil {
		t.Fatalf("review proposed text: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	for _, want := range []string{"connector:granola", "connector:claude-chats", "conf=", "maz review proposed --approve-all"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "Aragorn") {
		t.Errorf("team-scope edge leaked into the personal text review\n%s", out)
	}
}

// TestReviewProposedEmptyQueueExitsZero pins requirement 4: an empty queue is a
// success (exit 0), not an error.
func TestReviewProposedEmptyQueueExitsZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A migrated but empty projection.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_ = store.Close()

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "text", "review", "proposed"})
	if err := root.Execute(); err != nil {
		t.Fatalf("empty queue should exit 0, got: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "no proposed relations") {
		t.Errorf("empty queue output = %q, want a 'no proposed relations' notice", outBuf.String())
	}
}

// TestReviewProposedBadScopeExitsOne pins the validation exit code: an
// unrecognised --scope is bad input → exit 1.
func TestReviewProposedBadScopeExitsOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedProposedIndexDB(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "review", "proposed", "--scope", "bogus"})
	err := root.Execute()
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("bad scope error = %T %v, want silentError", err, err)
	}
	if se.code != 1 {
		t.Errorf("bad scope exit code = %d, want 1", se.code)
	}
}

// TestReviewProposedApproveAllPromotesScopedEdges pins the acceptance criterion:
// --approve-all promotes every listed (in-scope) proposed edge via
// promoteRelation, each in its own scope, and a follow-up read shows an empty
// queue. Scope isolation holds: the team edge is neither listed nor promoted
// under the personal default. No t.Parallel — stubs package-level seams.
func TestReviewProposedApproveAllPromotesScopedEdges(t *testing.T) {
	dir := t.TempDir()
	seedProposedIndexDB(t, dir)

	var calls []map[string]any
	promote := fakeTeamToolFunc{
		name: "promoteRelation",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			var in map[string]any
			if err := json.Unmarshal(req, &in); err != nil {
				return nil, err
			}
			calls = append(calls, in)
			// Flip the edge to asserted in the seeded projection so the
			// follow-up read shows the queue drained (the real dispatcher does
			// this via the relationPromoted projection).
			name, _ := in["name"].(string)
			uid := strings.TrimPrefix(name, "relations/")
			ctx := context.Background()
			store, oerr := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
			if oerr != nil {
				return nil, oerr
			}
			defer func() { _ = store.Close() }()
			if _, uerr := store.Writer().ExecContext(ctx,
				`UPDATE relations SET status='asserted' WHERE uid=?`, uid); uerr != nil {
				return nil, uerr
			}
			return json.RawMessage(`{"name":"` + name + `","status":"asserted","promoted":true}`), nil
		},
	}
	stubTeamWiring(t, promote)

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "review", "proposed", "--approve-all"})
	if err := root.Execute(); err != nil {
		t.Fatalf("approve-all: %v (stderr=%q)", err, errBuf.String())
	}

	// Two personal proposed edges promoted; the team edge is untouched.
	if len(calls) != 2 {
		t.Fatalf("promoteRelation called %d times, want 2 (personal edges only)\ncalls=%+v", len(calls), calls)
	}
	for _, c := range calls {
		if c["scope"] != "personal" {
			t.Errorf("promote scope = %v, want personal (each edge promoted in its own scope)", c["scope"])
		}
		name, _ := c["name"].(string)
		if name == "relations/r_rules" {
			t.Error("team-scope edge r_rules was promoted under the personal default (scope leak)")
		}
	}

	// Follow-up read: the queue is now drained.
	var out2, err2 bytes.Buffer
	root2 := makeReviewRoot(dir, &out2, &err2)
	root2.SetArgs([]string{"--data-dir", dir, "--output", "json", "review", "proposed"})
	if err := root2.Execute(); err != nil {
		t.Fatalf("follow-up read: %v (stderr=%q)", err, err2.String())
	}
	var res struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(out2.Bytes(), &res); err != nil {
		t.Fatalf("follow-up not JSON: %v\n%s", err, out2.String())
	}
	if res.Count != 0 {
		t.Errorf("after approve-all the personal queue count = %d, want 0\n%s", res.Count, out2.String())
	}
}

// TestReviewProposedApproveAllTeamScope pins that --scope team reviews and
// promotes ONLY the team edge, in team scope.
func TestReviewProposedApproveAllTeamScope(t *testing.T) {
	dir := t.TempDir()
	seedProposedIndexDB(t, dir)

	var calls []map[string]any
	promote := fakeTeamToolFunc{
		name: "promoteRelation",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			var in map[string]any
			_ = json.Unmarshal(req, &in)
			calls = append(calls, in)
			return json.RawMessage(`{"name":"relations/r_rules","status":"asserted","promoted":true}`), nil
		},
	}
	stubTeamWiring(t, promote)

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "review", "proposed", "--approve-all", "--scope", "team"})
	if err := root.Execute(); err != nil {
		t.Fatalf("approve-all --scope team: %v (stderr=%q)", err, errBuf.String())
	}
	if len(calls) != 1 {
		t.Fatalf("promoteRelation called %d times, want 1 (the team edge)\ncalls=%+v", len(calls), calls)
	}
	if calls[0]["name"] != "relations/r_rules" || calls[0]["scope"] != "team" {
		t.Errorf("team promote = %+v, want relations/r_rules in team scope", calls[0])
	}
}

// TestReviewProposedApprovePermissionExitsOne pins the exit-code contract on the
// approve path: an E_PERMISSION from promoteRelation (a non-member team
// promotion) is expected-class → exit 1. No t.Parallel — stubs seams.
func TestReviewProposedApprovePermissionExitsOne(t *testing.T) {
	dir := t.TempDir()
	seedProposedIndexDB(t, dir)

	promote := fakeTeamToolFunc{
		name: "promoteRelation",
		invoke: func(json.RawMessage) (json.RawMessage, error) {
			return nil, &mcp.ToolError{Code: mcp.CodePermission, Message: "not a team member"}
		},
	}
	stubTeamWiring(t, promote)

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "review", "proposed", "--approve-all", "--scope", "team"})
	err := root.Execute()
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("permission error = %T %v, want silentError", err, err)
	}
	if se.code != 1 {
		t.Errorf("E_PERMISSION exit code = %d, want 1", se.code)
	}
	if !strings.Contains(outBuf.String(), "E_PERMISSION") {
		t.Errorf("JSON envelope missing E_PERMISSION code\n%s", outBuf.String())
	}
}
