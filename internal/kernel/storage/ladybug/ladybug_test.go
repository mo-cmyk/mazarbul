package ladybug_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/ladybug"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func TestOpenLadybug_FreshOpenAppliesMigrations(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	path := filepath.Join(t.TempDir(), "graph.ladybug")

	c, err := ladybug.OpenLadybug(ctx, path)
	if err != nil {
		t.Fatalf("OpenLadybug: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	t.Run("schema present", func(t *testing.T) {
		// Exactly one Entity: the seeded canonical self node (migration 0004).
		assertScalarInt64(t, c, "MATCH (e:Entity) RETURN count(e)", 1)
		assertScalarInt64(t, c, "MATCH (e:Episode) RETURN count(e)", 0)
	})

	t.Run("rel tables registered", func(t *testing.T) {
		seen := tableNames(t, c)
		for _, want := range []string{"Entity", "Episode", "MENTIONS", "RELATES_TO", "_ladybug_migrations"} {
			if _, ok := seen[want]; !ok {
				t.Errorf("table %q not registered; got %v", want, mapKeys(seen))
			}
		}
	})

	t.Run("self entity seeded", func(t *testing.T) {
		// Migration 0004 seeds the canonical self node so self-anchored edges
		// (self --has_name--> X, document --mentions--> self) are graph-
		// traversable. Values mirror SQLite migration 0012 (issue #175).
		r, err := c.Execute(ctx,
			`MATCH (e:Entity {uid: 'self'}) RETURN e.kind, e.displayName, e.confidence, e.resolutionStatus`, nil)
		if err != nil {
			t.Fatalf("query self: %v", err)
		}
		defer func() { _ = r.Close() }()
		if !r.Next() {
			if err := r.Err(); err != nil {
				t.Fatalf("self row err: %v", err)
			}
			t.Fatal("no self entity node seeded in graph")
		}
		var kind, name, res string
		var conf float64
		if err := r.Scan(&kind, &name, &conf, &res); err != nil {
			t.Fatalf("scan self: %v", err)
		}
		if kind != "person" || name != "Me" || conf != 1.0 || res != "resolved" {
			t.Errorf("self node = (kind=%q name=%q conf=%v status=%q); want (person, Me, 1, resolved)",
				kind, name, conf, res)
		}
	})

	t.Run("migration row recorded", func(t *testing.T) {
		// Five migrations ship in this build: 0001 (schema), 0002 (RELATES_TO
		// validity columns added for bi-temporal recall), 0003 (RELATES_TO
		// attributes column added for role/title qualifiers), 0004 (self
		// entity node seed), and 0005 (scope columns for team-scope
		// defense-in-depth). All fire on a fresh graph DB; the count and
		// max(version) track that.
		assertScalarInt64(t, c, "MATCH (m:_ladybug_migrations) RETURN max(m.version)", 5)
		assertScalarInt64(t, c, "MATCH (m:_ladybug_migrations) RETURN count(m)", 5)
	})
}

func TestOpenLadybug_ReopenIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	path := filepath.Join(t.TempDir(), "graph.ladybug")

	c1, err := ladybug.OpenLadybug(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	c2, err := ladybug.OpenLadybug(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	// Schema and exactly the five recorded migrations, even after the second
	// migrate pass; reopen is idempotent (no duplicate migration rows). The
	// self seed (0004) is MERGE-based, so reopening leaves exactly one self
	// node rather than a duplicate.
	assertScalarInt64(t, c2, "MATCH (m:_ladybug_migrations) RETURN count(m)", 5)
	assertScalarInt64(t, c2, "MATCH (e:Entity) RETURN count(e)", 1)
	assertScalarInt64(t, c2, "MATCH (e:Entity {uid: 'self'}) RETURN count(e)", 1)
}

func TestOpenLadybug_ErrorPaths(t *testing.T) {
	t.Parallel()

	t.Run("rejects empty path", func(t *testing.T) {
		t.Parallel()
		if _, err := ladybug.OpenLadybug(testutil.TestContext(t), ""); err == nil {
			t.Fatal("expected error for empty path, got nil")
		}
	})

	t.Run("rejects canceled context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(testutil.TestContext(t))
		cancel()
		_, err := ladybug.OpenLadybug(ctx, filepath.Join(t.TempDir(), "graph.ladybug"))
		if err == nil {
			t.Fatal("expected error for canceled context, got nil")
		}
	})
}

func TestConn_ExecuteAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	c, err := ladybug.OpenLadybug(ctx, filepath.Join(t.TempDir(), "graph.ladybug"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := c.Execute(ctx, "MATCH (e:Entity) RETURN count(e)", nil); !errors.Is(err, ladybug.ErrClosed) {
		t.Fatalf("Execute after Close = %v, want ErrClosed", err)
	}
	// Close is idempotent.
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestConn_CloseDrainsInflightResults(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	c, err := ladybug.OpenLadybug(ctx, filepath.Join(t.TempDir(), "graph.ladybug"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Hold an open result; Close must block until we release it.
	r, err := c.Execute(ctx, "MATCH (e:Entity) RETURN count(e)", nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	closeDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if cerr := c.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
		close(closeDone)
	}()

	// Give Close a moment to start; it should still be blocked on the
	// in-flight result.
	select {
	case <-closeDone:
		t.Fatal("Close returned before in-flight result was closed")
	case <-time.After(50 * time.Millisecond):
	}

	if err := r.Close(); err != nil {
		t.Fatalf("result close: %v", err)
	}

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after result drained")
	}
	wg.Wait()
}

func TestConn_ExecuteWithParams(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	c, err := ladybug.OpenLadybug(ctx, filepath.Join(t.TempDir(), "graph.ladybug"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Insert an Entity with parameterized values.
	insert := `CREATE (:Entity {
		uid: $uid,
		kind: $kind,
		displayName: $name,
		confidence: $conf,
		resolutionStatus: $res,
		createTime: timestamp('2026-05-24T00:00:00Z')
	})`
	r, err := c.Execute(ctx, insert, map[string]any{
		"uid":  "ent_test_1",
		"kind": "person",
		"name": "Test Person",
		"conf": 0.9,
		"res":  "canonical",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close insert: %v", err)
	}

	// Read it back via a parameterized MATCH.
	read, err := c.Execute(ctx, `MATCH (e:Entity {uid: $uid}) RETURN e.displayName, e.confidence`, map[string]any{
		"uid": "ent_test_1",
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	t.Cleanup(func() { _ = read.Close() })

	if !read.Next() {
		if err := read.Err(); err != nil {
			t.Fatalf("read.Err: %v", err)
		}
		t.Fatal("expected one row, got zero")
	}
	var name string
	var conf float64
	if err := read.Scan(&name, &conf); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "Test Person" || conf != 0.9 {
		t.Errorf("read back wrong values: name=%q conf=%v", name, conf)
	}
}

// assertScalarInt64 runs a query that returns one row with one int64 column
// and asserts the scalar value.
func assertScalarInt64(t *testing.T, c kernel.GraphConn, cypher string, want int64) {
	t.Helper()
	r, err := c.Execute(testutil.TestContext(t), cypher, nil)
	if err != nil {
		t.Fatalf("execute %q: %v", cypher, err)
	}
	defer func() { _ = r.Close() }()
	if !r.Next() {
		if err := r.Err(); err != nil {
			t.Fatalf("query %q err: %v", cypher, err)
		}
		t.Fatalf("query %q returned zero rows", cypher)
	}
	var got int64
	if err := r.Scan(&got); err != nil {
		t.Fatalf("scan %q: %v", cypher, err)
	}
	if got != want {
		t.Errorf("query %q = %d, want %d", cypher, got, want)
	}
}

// tableNames returns the set of table names currently registered in the
// database. Lbug exposes `CALL show_tables() RETURN *` and the columns are
// (id, name, type, database_name, comment).
func tableNames(t *testing.T, c kernel.GraphConn) map[string]struct{} {
	t.Helper()
	r, err := c.Execute(testutil.TestContext(t), "CALL show_tables() RETURN *", nil)
	if err != nil {
		t.Fatalf("show_tables: %v", err)
	}
	defer func() { _ = r.Close() }()
	out := make(map[string]struct{})
	for r.Next() {
		var (
			id      int64
			name    string
			ttype   string
			dbName  string
			comment string
		)
		if err := r.Scan(&id, &name, &ttype, &dbName, &comment); err != nil {
			t.Fatalf("scan show_tables: %v", err)
		}
		out[name] = struct{}{}
	}
	if err := r.Err(); err != nil {
		t.Fatalf("show_tables iter: %v", err)
	}
	return out
}

func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
