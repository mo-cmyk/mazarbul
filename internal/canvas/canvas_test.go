package canvas_test

import (
	"bytes"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/canvas"
)

// sampleSubgraph is the fixture exercised across the rendering tests: a seed
// person, a document neighbor (file node), a plain entity neighbor (text node),
// a hop-2 document with no vault path (text fallback), plus a self-loop, a
// dangling edge, and a duplicate edge that Build must drop/dedup.
func sampleSubgraph() canvas.Subgraph {
	return canvas.Subgraph{
		SeedUID: "seed",
		Nodes: []canvas.GraphNode{
			{UID: "seed", Kind: "person", DisplayName: "Sarah", Hops: 0},
			{UID: "doc1", Kind: "document", DisplayName: "Notes", Hops: 1, Doc: &canvas.DocRef{Path: "notes/sarah.md", Title: "Sarah Notes"}},
			{UID: "ent2", Kind: "organization", DisplayName: "Acme", Hops: 1},
			{UID: "doc3", Kind: "document", DisplayName: "Plan", Hops: 2, Doc: &canvas.DocRef{Title: "Plan", Excerpt: "the body excerpt"}},
		},
		Edges: []canvas.GraphEdge{
			{UID: "e1", FromUID: "seed", ToUID: "doc1", Predicate: "mentions", Confidence: 1.0},
			{UID: "e2", FromUID: "seed", ToUID: "ent2", Predicate: "works_at", Confidence: 0.8},
			{UID: "e3", FromUID: "ent2", ToUID: "doc3", Predicate: "links_to", Confidence: 1.0},
			{UID: "e4", FromUID: "seed", ToUID: "seed", Predicate: "self"},     // self-loop -> dropped
			{UID: "e5", FromUID: "seed", ToUID: "ghost", Predicate: "gone"},    // dangling -> dropped
			{UID: "e1", FromUID: "seed", ToUID: "doc1", Predicate: "mentions"}, // duplicate UID -> deduped
		},
	}
}

// nodeByID indexes a canvas's nodes for assertions.
func nodeByID(c canvas.Canvas) map[string]canvas.Node {
	m := make(map[string]canvas.Node, len(c.Nodes))
	for _, n := range c.Nodes {
		m[n.ID] = n
	}
	return m
}

func TestBuild(t *testing.T) {
	t.Parallel()

	t.Run("renders entities documents and labeled edges", func(t *testing.T) {
		t.Parallel()
		c := canvas.Build(sampleSubgraph())
		if len(c.Nodes) != 4 {
			t.Fatalf("nodes = %d, want 4", len(c.Nodes))
		}
		if len(c.Edges) != 3 {
			t.Fatalf("edges = %d, want 3 (self-loop, dangling, dup dropped)", len(c.Edges))
		}
		nodes := nodeByID(c)

		seed := nodes["seed"]
		if seed.Type != "text" || seed.Color != "4" {
			t.Fatalf("seed = %+v, want text node colored 4", seed)
		}
		if seed.X != -nodeWidthHalf() || seed.Y != -nodeHeightHalf() {
			t.Fatalf("seed at (%d,%d), want centered at origin", seed.X, seed.Y)
		}

		doc1 := nodes["doc1"]
		if doc1.Type != "file" || doc1.File != "notes/sarah.md" || doc1.Text != "" || doc1.Color != "5" {
			t.Fatalf("doc1 = %+v, want file node -> notes/sarah.md, color 5, no text", doc1)
		}

		ent2 := nodes["ent2"]
		if ent2.Type != "text" || ent2.Color != "" || ent2.Text != "# Acme\n\n_organization_" {
			t.Fatalf("ent2 = %+v, want default-color text card", ent2)
		}

		doc3 := nodes["doc3"]
		if doc3.Type != "text" || doc3.Color != "5" || doc3.Text != "# Plan\n\nthe body excerpt" {
			t.Fatalf("doc3 = %+v, want text fallback card with excerpt", doc3)
		}

		// Confidence < 1 annotates the label; confidence 1 does not.
		var e1, e2 canvas.Edge
		for _, e := range c.Edges {
			switch e.ID {
			case "e1":
				e1 = e
			case "e2":
				e2 = e
			}
		}
		if e1.Label != "mentions" {
			t.Fatalf("e1 label = %q, want %q", e1.Label, "mentions")
		}
		if e2.Label != "works_at (0.80)" {
			t.Fatalf("e2 label = %q, want %q", e2.Label, "works_at (0.80)")
		}
		if e2.FromNode != "seed" || e2.ToNode != "ent2" {
			t.Fatalf("e2 endpoints = %s->%s, want seed->ent2", e2.FromNode, e2.ToNode)
		}
	})

	t.Run("orphan seed renders one node and no edges", func(t *testing.T) {
		t.Parallel()
		c := canvas.Build(canvas.Subgraph{
			SeedUID: "lonely",
			Nodes:   []canvas.GraphNode{{UID: "lonely", Kind: "person", DisplayName: "Solo", Hops: 0}},
		})
		if len(c.Nodes) != 1 || len(c.Edges) != 0 {
			t.Fatalf("orphan canvas = %d nodes / %d edges, want 1 / 0", len(c.Nodes), len(c.Edges))
		}
		b, err := c.MarshalIndent()
		if err != nil {
			t.Fatalf("MarshalIndent: %v", err)
		}
		// The edges key must serialize as [] (never null) so a viewer reads it.
		if !bytes.Contains(b, []byte("\"edges\": []")) {
			t.Fatalf("orphan canvas edges not emitted as []:\n%s", b)
		}
	})

	t.Run("is deterministic under shuffled input", func(t *testing.T) {
		t.Parallel()
		base := sampleSubgraph()
		shuffled := sampleSubgraph()
		// Reverse the node + edge slices so input order differs from base.
		for i, j := 0, len(shuffled.Nodes)-1; i < j; i, j = i+1, j-1 {
			shuffled.Nodes[i], shuffled.Nodes[j] = shuffled.Nodes[j], shuffled.Nodes[i]
		}
		for i, j := 0, len(shuffled.Edges)-1; i < j; i, j = i+1, j-1 {
			shuffled.Edges[i], shuffled.Edges[j] = shuffled.Edges[j], shuffled.Edges[i]
		}
		a, err := canvas.Build(base).MarshalIndent()
		if err != nil {
			t.Fatalf("marshal base: %v", err)
		}
		b, err := canvas.Build(shuffled).MarshalIndent()
		if err != nil {
			t.Fatalf("marshal shuffled: %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("Build is not order-independent:\n--- base ---\n%s\n--- shuffled ---\n%s", a, b)
		}
	})

	t.Run("coordinates are integers", func(t *testing.T) {
		t.Parallel()
		b, err := canvas.Build(sampleSubgraph()).MarshalIndent()
		if err != nil {
			t.Fatalf("MarshalIndent: %v", err)
		}
		// JSON Canvas requires integer geometry: no x/y/width/height may carry a
		// fractional part on the wire.
		fractional := regexp.MustCompile(`"(?:x|y|width|height)":\s*-?\d+\.\d`)
		if loc := fractional.Find(b); loc != nil {
			t.Fatalf("found fractional geometry %q in:\n%s", loc, b)
		}
		// And the document round-trips as valid JSON Canvas.
		var rt canvas.Canvas
		if err := json.Unmarshal(b, &rt); err != nil {
			t.Fatalf("rendered canvas does not round-trip: %v", err)
		}
	})
}

func nodeWidthHalf() int  { return 300 / 2 }
func nodeHeightHalf() int { return 180 / 2 }
