package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// seedEpisodes opens (and migrates) an index.db under dir and inserts the given
// (uid, text) episodes so the read-side `maz list` commands have rows to render.
func seedEpisodes(t *testing.T, dir string, rows map[string]string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	for uid, text := range rows {
		if _, err := store.Writer().ExecContext(ctx,
			`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
			 VALUES (?, '2026-06-08T00:00:00Z', 'episode', '{}', ?, 'test')`,
			uid, text); err != nil {
			t.Fatalf("insert episode %s: %v", uid, err)
		}
	}
}

// seedRelations inserts relation rows with nullable valid_from / valid_until
// so the relations list path can be exercised.
func seedRelations(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	// Insert one relation with NULL valid_from / valid_until.
	// All 10 columns are bound via ? so the query is unambiguous.
	var nullStr *string
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO relations`+
			` (uid, subject_uid, predicate, object_uid, confidence,`+
			`  source_event_uid, create_time, valid_from, valid_until, provenance)`+
			` VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"01ARZ3NDEKTSV4RRFFQ69G5RR1",
		"01ARZ3NDEKTSV4RRFFQ69G5SUB",
		"knows",
		"01ARZ3NDEKTSV4RRFFQ69G5OBJ",
		0.9,
		"01ARZ3NDEKTSV4RRFFQ69G5EVT",
		"2026-06-08T00:00:00Z",
		nullStr,
		nullStr,
		"test",
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}
}

// TestListRelations_JSONNoNullStringInternals is the failing test for B14.1.
// Before the fix, maz list relations --output json emits objects like:
//
//	{"validFrom":{"String":"","Valid":false}}
//
// After the fix, null fields must appear as JSON null (or be omitted), never
// as a serialised sql.NullString struct.
func TestListRelations_JSONNoNullStringInternals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedRelations(t, dir)

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "relations", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list relations: %v (stderr: %s)", err, errBuf.String())
	}

	raw := out.String()
	if strings.Contains(raw, `"Valid"`) {
		t.Errorf("output must not contain sql.NullString internals (\"Valid\" key): %s", raw)
	}
	if strings.Contains(raw, `"String"`) {
		t.Errorf("output must not contain sql.NullString internals (\"String\" key): %s", raw)
	}

	// Parse the JSON array.
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &rows); err != nil {
		t.Fatalf("output is not a valid JSON array: %v\nraw: %s", err, raw)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one relation row in the array")
	}
	// validFrom must be absent or JSON null, never an object.
	if vf, ok := rows[0]["validFrom"]; ok {
		if _, isMap := vf.(map[string]any); isMap {
			t.Errorf("validFrom must be null or absent, got object: %v", vf)
		}
	}
}

// TestListEpisodes_JSONEmitsArray is the failing test for B8.1.
// Before the fix, --output json emits NDJSON (one object per line).
// After the fix it must emit a single JSON array.
func TestListEpisodes_JSONEmitsArray(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEpisodes(t, dir, map[string]string{
		"01ARZ3NDEKTSV4RRFFQ69G5FA1": "first episode",
		"01ARZ3NDEKTSV4RRFFQ69G5FA2": "second episode",
	})

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "episodes", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list episodes: %v (stderr: %s)", err, errBuf.String())
	}

	raw := strings.TrimSpace(out.String())
	if !strings.HasPrefix(raw, "[") {
		preview := raw
		if len(preview) > 40 {
			preview = preview[:40]
		}
		t.Fatalf("--output json must start with '[', got: %q", preview)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("output is not a valid JSON array: %v\nraw: %s", err, raw)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows in array, got %d", len(rows))
	}
	for i, row := range rows {
		if _, ok := row["uid"]; !ok {
			t.Errorf("row %d missing uid field", i)
		}
	}
}

// TestListEpisodes_ExcludesDraftScope guards the draft-staging security
// boundary: draft-scoped episodes are speculative staging that must stay
// invisible to default navigation (reachable only via `maz scope draft list`).
// The raw `list episodes` query once omitted the scope predicate entirely and
// leaked draft rows; this pins that list spans {project,personal,team} but
// never draft.
func TestListEpisodes_ExcludesDraftScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEpisodes(t, dir, map[string]string{
		"01ARZ3NDEKTSV4RRFFQ69G5FA1": "personal visible episode",
	})
	// seedEpisodes defaults to scope=personal; insert one draft-scoped row.
	func() {
		ctx := context.Background()
		store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		defer func() { _ = store.Close() }()
		if _, err := store.Writer().ExecContext(ctx,
			`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source, scope)
			 VALUES (?, '2026-06-08T00:00:00Z', 'episode', '{}', ?, 'test', 'draft')`,
			"01ARZ3NDEKTSV4RRFFQ69G5FA2", "draft hidden episode"); err != nil {
			t.Fatalf("insert draft episode: %v", err)
		}
	}()

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "episodes", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list episodes: %v (stderr: %s)", err, errBuf.String())
	}

	raw := strings.TrimSpace(out.String())
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("output is not a valid JSON array: %v\nraw: %s", err, raw)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (personal only), got %d: %s", len(rows), raw)
	}
	if got := rows[0]["text"]; got != "personal visible episode" {
		t.Errorf("unexpected row surfaced: text = %v", got)
	}
	if strings.Contains(raw, "draft hidden") {
		t.Errorf("draft-scoped episode leaked into `list episodes`: %s", raw)
	}
}

// TestListEpisodes_JSONEmptyIsArray is the failing test for F2.2.
// Before the fix, --output json with zero rows emits zero bytes.
// After the fix an empty result must emit "[]".
func TestListEpisodes_JSONEmptyIsArray(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Seed the store (runs migrations) but insert no episodes.
	seedEpisodes(t, dir, map[string]string{})

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "episodes", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list episodes empty: %v (stderr: %s)", err, errBuf.String())
	}

	raw := strings.TrimSpace(out.String())
	if raw != "[]" {
		t.Fatalf("empty --output json must yield '[]', got: %q", raw)
	}
}

// TestListEpisodes_NDJSONStaysLineDelimited confirms --output ndjson continues
// to emit one object per line (not an array) after the json-array fix.
func TestListEpisodes_NDJSONStaysLineDelimited(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEpisodes(t, dir, map[string]string{"01ARZ3NDEKTSV4RRFFQ69G5FA1": "only episode"})

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "episodes", "--output", "ndjson"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list episodes ndjson: %v (stderr: %s)", err, errBuf.String())
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), out.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("ndjson line not parseable: %v", err)
	}
}

// TestGroupCommand_UnknownSubcommandErrors pins the fix for groups that printed
// help and exited 0 on an unknown subcommand (e.g. `maz fact list`). Each group
// must now return an error so scripts and agents see a non-zero exit.
func TestGroupCommand_UnknownSubcommandErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"fact", "list"},
		{"doc", "bogus"},
		{"context", "nope"},
		{"list", "wat"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			root := newRootCmd()
			root.SetOut(&out)
			root.SetErr(&errBuf)
			root.SetArgs(args)
			if err := root.Execute(); err == nil {
				t.Errorf("maz %s: want an error for the unknown subcommand, got nil", strings.Join(args, " "))
			}
		})
	}
}

// TestListRelations_JSONHasStatus is the failing test for B7.
// Before the fix, maz list relations --output json rows omit the "status"
// field that the text output renders via relationStateGlyph. After the fix
// each row must contain "status": "open" or "status": "closed".
func TestListRelations_JSONHasStatus(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedRelations(t, dir)

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "relations", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list relations json: %v (stderr: %s)", err, errBuf.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &rows); err != nil {
		t.Fatalf("output is not valid JSON array: %v\nraw: %s", err, out.String())
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one relation row")
	}
	status, ok := rows[0]["status"]
	if !ok {
		t.Fatal("relation JSON row must contain a \"status\" field")
	}
	statusStr, isStr := status.(string)
	if !isStr {
		t.Fatalf("status must be a string, got %T: %v", status, status)
	}
	if statusStr != "open" && statusStr != "closed" {
		t.Fatalf("status must be \"open\" or \"closed\", got %q", statusStr)
	}
}

// TestListRelations_JSONStatusClosed verifies that a closed relation (non-NULL
// valid_until) emits "status":"closed" in JSON mode.
func TestListRelations_JSONStatusClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Seed a relation with valid_until set so it is considered closed.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	until := "2026-01-01T00:00:00Z"
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO relations`+
			` (uid, subject_uid, predicate, object_uid, confidence,`+
			`  source_event_uid, create_time, valid_from, valid_until, provenance)`+
			` VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"01ARZ3NDEKTSV4RRFFQ69G5CLO",
		"01ARZ3NDEKTSV4RRFFQ69G5SUB",
		"knows",
		"01ARZ3NDEKTSV4RRFFQ69G5OBJ",
		0.8,
		"01ARZ3NDEKTSV4RRFFQ69G5EVT",
		"2026-06-08T00:00:00Z",
		(*string)(nil),
		&until,
		"test",
	); err != nil {
		t.Fatalf("insert closed relation: %v", err)
	}
	if cerr := store.Close(); cerr != nil {
		t.Fatalf("close sqlite: %v", cerr)
	}

	var out, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--data-dir", dir, "list", "relations", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list relations json closed: %v (stderr: %s)", err, errBuf.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &rows); err != nil {
		t.Fatalf("output is not valid JSON array: %v\nraw: %s", err, out.String())
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one row")
	}
	if rows[0]["status"] != "closed" {
		t.Fatalf("closed relation must have status=\"closed\", got %v", rows[0]["status"])
	}
}

// TestListRelations_LimitZeroReturnsAll confirms --limit 0 (and the --all
// alias) mean "no limit": every seeded relation is returned. This unifies the
// convention with navigate/traverse/query, where a zero limit already means
// "no explicit cap" rather than the old list-only "return no rows".
func TestListRelations_LimitZeroReturnsAll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedRelations(t, dir)

	run := func(args ...string) []any {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errBuf)
		root.SetArgs(append([]string{"--data-dir", dir, "list", "relations", "--output", "json"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("list relations %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		var rows []any
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("list relations %v: not a JSON array: %v (%q)", args, err, out.String())
		}
		return rows
	}

	zero := run("--limit", "0")
	if len(zero) == 0 {
		t.Fatalf("--limit 0 must return all seeded relations, got an empty array")
	}
	if all := run("--all"); len(all) != len(zero) {
		t.Fatalf("--all returned %d rows, --limit 0 returned %d; they must match", len(all), len(zero))
	}
}

// nonEmptyLines splits s on newlines and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}
