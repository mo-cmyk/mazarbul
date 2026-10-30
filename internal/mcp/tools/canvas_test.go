package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// newCanvasNav seeds a small graph for the exportCanvas tests:
//
//	Merry(alice) --works_at(0.8)--> Mithril(org)
//	Merry(alice) --mentions------> doc1 (a document at notes/sarah.md)
//	doc1         --mentions------> Mithril(org)  (sibling cross-link)
func newCanvasNav(t *testing.T) *navigate.Navigator {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "alice", "person", "Merry", 1.0, "confirmed", now)
	exec(ent, "mithril", "organization", "Mithril", 1.0, "confirmed", now)
	exec(ent, "doc1", "document", "Sarah Notes", 1.0, "confirmed", now)

	exec(`INSERT INTO documents (document_uid, path, title, body, content_hash, revision_uid, create_time, update_time)
	      VALUES (?,?,?,?,?,?,?,?)`,
		"doc1", "notes/sarah.md", "Sarah Notes", "Body about Merry and Mithril.", "h1", "rev1", now, now)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes) VALUES (?,?,?,?,?,?,?,?)`
	exec(rel, "r_works", "alice", "works_at", "mithril", 0.8, "ep1", now, `{}`)
	exec(rel, "r_mentions", "alice", "mentions", "doc1", 1.0, "ep1", now, `{}`)
	// Sibling cross-link: doc1 and mithril are both hop-1 neighbors of alice,
	// so this edge between them exercises Subgraph's edge-union (it is not on
	// the BFS tree from the seed, only surfaces by unioning edges among the set).
	exec(rel, "r_doc_org", "doc1", "mentions", "mithril", 1.0, "ep1", now, `{}`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

// newScopedCanvasNav seeds a graph that straddles a scope boundary:
//
//	Merry(alice, personal) --member_of(team)--> Mithril(secret, team)
//
// The seed is in the default read set; the neighbor and the edge are team-scoped,
// so a default-scope export must not surface them, and widening to team must.
func newScopedCanvasNav(t *testing.T) *navigate.Navigator {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time, scope) VALUES (?,?,?,?,?,?,?)`
	exec(ent, "alice", "person", "Merry", 1.0, "confirmed", now, "personal")
	exec(ent, "secret", "organization", "Mithril", 1.0, "confirmed", now, "team")

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, attributes, scope) VALUES (?,?,?,?,?,?,?,?,?)`
	exec(rel, "r_secret", "alice", "member_of", "secret", 1.0, "ep1", now, `{}`, "team")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

// canvasResp is the decoded exportCanvas response shape.
type canvasResp struct {
	Canvas struct {
		Nodes []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Color string `json:"color"`
			File  string `json:"file"`
			Text  string `json:"text"`
		} `json:"nodes"`
		Edges []struct {
			ID       string `json:"id"`
			FromNode string `json:"fromNode"`
			ToNode   string `json:"toNode"`
			Label    string `json:"label"`
		} `json:"edges"`
	} `json:"canvas"`
	Documents []string `json:"documents"`
}

func TestExportCanvas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("exports a seed centered subgraph with file and text nodes", func(t *testing.T) {
		t.Parallel()
		tool := tools.NewExportCanvas(newCanvasNav(t))
		raw, err := tool.Invoke(ctx, json.RawMessage(`{"seed":"entities/alice","hops":1}`))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var resp canvasResp
		if uerr := json.Unmarshal(raw, &resp); uerr != nil {
			t.Fatalf("decode: %v", uerr)
		}
		if len(resp.Canvas.Nodes) != 3 || len(resp.Canvas.Edges) != 3 {
			t.Fatalf("canvas = %d nodes / %d edges, want 3 / 3", len(resp.Canvas.Nodes), len(resp.Canvas.Edges))
		}
		nodes := map[string]struct {
			ID, Type, Color, File, Text string
		}{}
		for _, n := range resp.Canvas.Nodes {
			nodes[n.ID] = struct{ ID, Type, Color, File, Text string }{n.ID, n.Type, n.Color, n.File, n.Text}
		}
		if seed := nodes["alice"]; seed.Type != "text" || seed.Color != "4" {
			t.Fatalf("seed alice = %+v, want text node colored 4", seed)
		}
		if org := nodes["mithril"]; org.Type != "text" || org.Color != "" {
			t.Fatalf("mithril = %+v, want default-color text node", org)
		}
		if doc := nodes["doc1"]; doc.Type != "file" || doc.File != "notes/sarah.md" || doc.Color != "5" {
			t.Fatalf("doc1 = %+v, want file node -> notes/sarah.md, color 5", doc)
		}
		if len(resp.Documents) != 1 || resp.Documents[0] != "notes/sarah.md" {
			t.Fatalf("documents = %v, want [notes/sarah.md]", resp.Documents)
		}
		// Confidence < 1 annotates the works_at label.
		var worksLabel string
		for _, e := range resp.Canvas.Edges {
			if e.ID == "r_works" {
				worksLabel = e.Label
			}
		}
		if worksLabel != "works_at (0.80)" {
			t.Fatalf("works_at edge label = %q, want %q", worksLabel, "works_at (0.80)")
		}
		// The sibling cross-link doc1 -> mithril is off the BFS tree; it surfaces
		// only because Subgraph unions every edge among the reached node set.
		sibling := false
		for _, e := range resp.Canvas.Edges {
			if e.ID == "r_doc_org" && e.FromNode == "doc1" && e.ToNode == "mithril" {
				sibling = true
			}
		}
		if !sibling {
			t.Fatalf("missing sibling cross-link doc1 -> mithril edge: %+v", resp.Canvas.Edges)
		}
	})

	t.Run("document seed renders the document as the focal node", func(t *testing.T) {
		t.Parallel()
		tool := tools.NewExportCanvas(newCanvasNav(t))
		raw, err := tool.Invoke(ctx, json.RawMessage(`{"seed":"entities/doc1","hops":1}`))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var resp canvasResp
		if uerr := json.Unmarshal(raw, &resp); uerr != nil {
			t.Fatalf("decode: %v", uerr)
		}
		var seedNode struct {
			ID, Type, Color string
		}
		for _, n := range resp.Canvas.Nodes {
			if n.ID == "doc1" {
				seedNode.ID, seedNode.Type, seedNode.Color = n.ID, n.Type, n.Color
			}
		}
		// A document seed is still a file node, but colored as the focal seed.
		if seedNode.Type != "file" || seedNode.Color != "4" {
			t.Fatalf("document seed = %+v, want file node colored 4 (seed)", seedNode)
		}
	})

	t.Run("missing seed is not found", func(t *testing.T) {
		t.Parallel()
		tool := tools.NewExportCanvas(newCanvasNav(t))
		_, err := tool.Invoke(ctx, json.RawMessage(`{"seed":"entities/ghost"}`))
		assertToolErrorCode(t, err, mcp.CodeNotFound)
	})

	t.Run("rejects an unknown scope", func(t *testing.T) {
		t.Parallel()
		tool := tools.NewExportCanvas(newCanvasNav(t))
		_, err := tool.Invoke(ctx, json.RawMessage(`{"seed":"entities/alice","scopes":["bogus"]}`))
		assertToolErrorCode(t, err, mcp.CodeValidation)
	})

	t.Run("scope gates which neighbors the canvas surfaces", func(t *testing.T) {
		t.Parallel()
		tool := tools.NewExportCanvas(newScopedCanvasNav(t))

		// Default scope {project, personal}: the team-scoped neighbor and edge
		// are invisible, so the canvas is the lone in-scope seed node.
		raw, err := tool.Invoke(ctx, json.RawMessage(`{"seed":"entities/alice","hops":1}`))
		if err != nil {
			t.Fatalf("Invoke (default scope): %v", err)
		}
		var narrow canvasResp
		if uerr := json.Unmarshal(raw, &narrow); uerr != nil {
			t.Fatalf("decode: %v", uerr)
		}
		if len(narrow.Canvas.Nodes) != 1 || len(narrow.Canvas.Edges) != 0 {
			t.Fatalf("default scope = %d nodes / %d edges, want 1 / 0 (team neighbor hidden)",
				len(narrow.Canvas.Nodes), len(narrow.Canvas.Edges))
		}
		if narrow.Canvas.Nodes[0].ID != "alice" {
			t.Fatalf("default-scope node = %q, want the in-scope seed alice", narrow.Canvas.Nodes[0].ID)
		}

		// Widening the request to include team surfaces both the neighbor and
		// the edge: it is the scope predicate, not absence, that hid them.
		raw2, err := tool.Invoke(ctx, json.RawMessage(`{"seed":"entities/alice","hops":1,"scopes":["personal","team"]}`))
		if err != nil {
			t.Fatalf("Invoke (team scope): %v", err)
		}
		var wide canvasResp
		if uerr := json.Unmarshal(raw2, &wide); uerr != nil {
			t.Fatalf("decode: %v", uerr)
		}
		if len(wide.Canvas.Nodes) != 2 || len(wide.Canvas.Edges) != 1 {
			t.Fatalf("team scope = %d nodes / %d edges, want 2 / 1 (team neighbor visible)",
				len(wide.Canvas.Nodes), len(wide.Canvas.Edges))
		}
	})

	t.Run("hops schema maximum matches MaxHops", func(t *testing.T) {
		t.Parallel()
		var schema struct {
			Properties struct {
				Hops struct {
					Maximum int `json:"maximum"`
				} `json:"hops"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tools.NewExportCanvas(nil).InputSchema(), &schema); err != nil {
			t.Fatalf("decode input schema: %v", err)
		}
		if schema.Properties.Hops.Maximum != navigate.MaxHops {
			t.Fatalf("canvas schema hops.maximum = %d, want navigate.MaxHops = %d",
				schema.Properties.Hops.Maximum, navigate.MaxHops)
		}
	})
}

// assertToolErrorCode fails unless err is an *mcp.ToolError carrying want.
func assertToolErrorCode(t *testing.T, err error, want mcp.ToolErrorCode) {
	t.Helper()
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *mcp.ToolError", err)
	}
	if te.Code != want {
		t.Fatalf("error code = %q, want %q", te.Code, want)
	}
}
