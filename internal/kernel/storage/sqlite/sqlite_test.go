package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOpen_CreatesFileAndAppliesPragmas(t *testing.T) {
	store := openStore(t)

	cases := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"}, // NORMAL = 1
	}

	for _, tc := range cases {
		t.Run(tc.pragma, func(t *testing.T) {
			var got string
			row := store.Writer().QueryRowContext(t.Context(), "PRAGMA "+tc.pragma)
			if err := row.Scan(&got); err != nil {
				t.Fatalf("scan PRAGMA %s: %v", tc.pragma, err)
			}
			if strings.ToLower(got) != tc.want {
				t.Errorf("PRAGMA %s = %q, want %q", tc.pragma, got, tc.want)
			}
		})
	}
}

func TestOpen_RejectsEmptyPath(t *testing.T) {
	if _, err := sqlite.Open(t.Context(), ""); err == nil {
		t.Fatal("Open with empty path returned nil error")
	}
}

func TestOpen_CreatesMissingParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "child", "test.db")

	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open nested path: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if store.Path() != path {
		t.Errorf("Path() = %q, want %q", store.Path(), path)
	}
}

// TestOpen_PathWithURIMetacharacters guards against the DSN-splice bug
// surfaced by Greptile on PR #71: a path containing `?`, `#`, or `%`
// must be opened verbatim, not parsed as URI syntax. Writes against the
// returned Store land in the actual file, not in a sibling that SQLite
// would create from the truncated path.
func TestOpen_PathWithURIMetacharacters(t *testing.T) {
	cases := []struct {
		label   string
		dirName string
	}{
		{"question mark", "data?dir"},
		{"hash", "data#dir"},
		{"percent", "data%dir"},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), tc.dirName)
			path := filepath.Join(parent, "test.db")

			store, err := sqlite.Open(t.Context(), path)
			if err != nil {
				t.Fatalf("Open(%q): %v", path, err)
			}
			t.Cleanup(func() { _ = store.Close() })

			// Round-trip a row to confirm the writer is bound to the
			// intended file. A miswired DSN would either silently open a
			// different file or apply zero pragmas.
			uid := "01HX-URI-TEST"
			_, err = store.Writer().ExecContext(t.Context(),
				`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?, ?, ?, ?)`,
				uid, time.Now().UTC().Format(time.RFC3339Nano), "remember", `{}`,
			)
			if err != nil {
				t.Fatalf("insert into %q: %v", path, err)
			}

			// The file is exactly where we asked.
			if _, err := os.Stat(path); err != nil {
				t.Errorf("expected db at %q, stat err = %v", path, err)
			}

			// Pragmas are still applied (would silently default if the
			// DSN was malformed and the query string got dropped).
			var journal string
			if err := store.Writer().QueryRowContext(t.Context(),
				"PRAGMA journal_mode").Scan(&journal); err != nil {
				t.Fatalf("read journal_mode: %v", err)
			}
			if strings.ToLower(journal) != "wal" {
				t.Errorf("journal_mode = %q, want wal (DSN query was dropped?)", journal)
			}
		})
	}
}

func TestOpen_WriterPoolMaxOpenConns(t *testing.T) {
	store := openStore(t)

	// The Go database/sql package does not expose MaxOpenConns directly via
	// Stats() (only the live counts), so we verify the bound by holding one
	// conn and trying to acquire a second under a short deadline. The
	// second acquisition must time out because the writer pool is capped at
	// 1.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	first, err := store.Writer().Conn(ctx)
	if err != nil {
		t.Fatalf("acquire first conn: %v", err)
	}
	defer first.Close()

	blocked := make(chan error, 1)
	go func() {
		gctx, gcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer gcancel()
		c, err := store.Writer().Conn(gctx)
		if err == nil {
			_ = c.Close()
		}
		blocked <- err
	}()

	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("second writer conn acquired while first was held; pool is not single-writer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test timed out waiting for blocked conn")
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	first, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	var n1 int
	if err := first.Writer().QueryRowContext(t.Context(), "SELECT count(*) FROM _migrations").Scan(&n1); err != nil {
		t.Fatalf("count after first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	var n2 int
	if err := second.Writer().QueryRowContext(t.Context(), "SELECT count(*) FROM _migrations").Scan(&n2); err != nil {
		t.Fatalf("count after second open: %v", err)
	}

	if n1 != n2 {
		t.Errorf("_migrations row count changed across reopen: %d -> %d", n1, n2)
	}
	if n2 == 0 {
		t.Errorf("_migrations is empty after reopen; expected at least 1 row")
	}
}

func TestReader_IsReadOnly(t *testing.T) {
	store := openStore(t)

	_, err := store.Reader().ExecContext(t.Context(),
		`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?, ?, ?, ?)`,
		"01HX000000000000000000000A", time.Now().UTC().Format(time.RFC3339Nano), "remember", `{}`,
	)
	if err == nil {
		t.Fatal("INSERT via reader pool succeeded; expected read-only error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("error = %v, want substring 'readonly'", err)
	}
}

func TestSchema_ExpectedTablesPresent(t *testing.T) {
	store := openStore(t)

	rows, err := store.Reader().QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`,
	)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	wantSubset := []string{
		"_migrations", "entities", "episodes", "mentions", "provenance",
		// FTS5 virtual table + its shadow tables from migration 0002.
		"episodes_fts", "episodes_fts_data", "episodes_fts_idx",
		"episodes_fts_docsize", "episodes_fts_config",
	}
	sort.Strings(got)

	for _, table := range wantSubset {
		if !contains(got, table) {
			t.Errorf("table %q missing from schema; got %v", table, got)
		}
	}
}

func TestSchema_EpisodesColumns(t *testing.T) {
	store := openStore(t)
	cols := tableColumns(t, store, "episodes")
	want := []string{"uid", "create_time", "kind", "payload_json", "text", "source"}
	for _, c := range want {
		if _, ok := cols[c]; !ok {
			t.Errorf("episodes column %q missing; got %v", c, keys(cols))
		}
	}
}

func TestSchema_EntitiesColumns(t *testing.T) {
	store := openStore(t)
	cols := tableColumns(t, store, "entities")
	want := []string{"uid", "kind", "display_name", "confidence", "resolution_status"}
	for _, c := range want {
		if _, ok := cols[c]; !ok {
			t.Errorf("entities column %q missing; got %v", c, keys(cols))
		}
	}
}

func TestSchema_MentionsColumns(t *testing.T) {
	store := openStore(t)
	cols := tableColumns(t, store, "mentions")
	want := []string{"uid", "kind", "display_name", "source_event_uid", "create_time"}
	for _, c := range want {
		if _, ok := cols[c]; !ok {
			t.Errorf("mentions column %q missing; got %v", c, keys(cols))
		}
	}
}

func TestSchema_ProvenanceColumns(t *testing.T) {
	store := openStore(t)
	cols := tableColumns(t, store, "provenance")
	want := []string{"event_uid", "source", "confidence", "decided_at"}
	for _, c := range want {
		if _, ok := cols[c]; !ok {
			t.Errorf("provenance column %q missing; got %v", c, keys(cols))
		}
	}
}

func TestSchema_ForeignKeysEnforced(t *testing.T) {
	store := openStore(t)

	// mentions.source_event_uid -> episodes.uid; insert without a matching
	// episodes row should fail under foreign_keys=ON.
	_, err := store.Writer().ExecContext(t.Context(),
		`INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time)
         VALUES (?, ?, ?, ?, ?)`,
		"01HX0M0", "person", "Lobelia", "missing", time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err == nil {
		t.Fatal("mentions insert with missing FK target succeeded; foreign_keys=ON not effective")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("error = %v, want substring 'foreign key'", err)
	}
}

func TestSchema_RoundTripEpisodeInsertSelect(t *testing.T) {
	store := openStore(t)

	uid := "01HX-EPISODE-0001"
	created := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := store.Writer().ExecContext(t.Context(),
		`INSERT INTO episodes (uid, create_time, kind, payload_json) VALUES (?, ?, ?, ?)`,
		uid, created, "remember", `{"text":"hello"}`,
	)
	if err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	var gotUID, gotKind, gotPayload string
	err = store.Reader().QueryRowContext(t.Context(),
		`SELECT uid, kind, payload_json FROM episodes WHERE uid = ?`, uid,
	).Scan(&gotUID, &gotKind, &gotPayload)
	if err != nil {
		t.Fatalf("read back episode: %v", err)
	}
	if gotUID != uid || gotKind != "remember" || gotPayload != `{"text":"hello"}` {
		t.Errorf("round-trip mismatch: %q %q %q", gotUID, gotKind, gotPayload)
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	store := openStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestConcurrentReaders(t *testing.T) {
	store := openStore(t)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)

	for range n {
		go func() {
			defer wg.Done()
			var count int
			if err := store.Reader().QueryRowContext(t.Context(),
				"SELECT count(*) FROM episodes").Scan(&count); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent reader error: %v", err)
	}
}

// helpers

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func tableColumns(t *testing.T, store *sqlite.Store, table string) map[string]string {
	t.Helper()
	rows, err := store.Reader().QueryContext(t.Context(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()

	cols := map[string]string{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[name] = ctype
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return cols
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
