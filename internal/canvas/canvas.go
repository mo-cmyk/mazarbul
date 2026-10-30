// Package canvas serializes a seed-rooted subgraph of the Mazarbul knowledge
// graph into the JSON Canvas 1.0 format (jsoncanvas.org), the open file format
// Obsidian and other canvas viewers read. It is a pure presentation layer: it
// takes an already-resolved, already-scoped Subgraph (nodes + edges) and a
// deterministic radial layout and emits a stable, git-diffable .canvas document.
// It performs no I/O, no database access, and no time/random calls, so the same
// input always yields byte-identical output.
package canvas

import (
	"encoding/json"
	"fmt"
	"sort"
)

// JSON Canvas node colors are either a preset "1".."6" or a "#hex" string. The
// preset values map (in Obsidian) to red/orange/yellow/green/cyan/purple. We
// give the focal seed and document nodes distinct presets and leave plain
// entities the default (no color) so the three node classes read at a glance.
const (
	colorSeed     = "4" // green: the focal node
	colorDocument = "5" // cyan: a vault document
)

// Canvas is a JSON Canvas 1.0 document: a flat set of nodes and edges. Both
// fields are always emitted as arrays (never null) so a viewer reads an empty
// graph as "[]" rather than a missing key.
type Canvas struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node is one JSON Canvas node. Geometry is integer-only per the spec. Only the
// fields relevant to the node's type carry values; the rest are omitted. We emit
// "text" nodes (markdown card) and "file" nodes (a live vault file reference).
type Node struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Color  string `json:"color,omitempty"`
	Text   string `json:"text,omitempty"` // text node: markdown body
	File   string `json:"file,omitempty"` // file node: vault-relative path
}

// Edge is one JSON Canvas edge. ToEnd defaults to "arrow" in the spec, so a
// directed arrow into the object node needs no explicit end fields; we emit only
// the label (the predicate) when present.
type Edge struct {
	ID       string `json:"id"`
	FromNode string `json:"fromNode"`
	ToNode   string `json:"toNode"`
	Label    string `json:"label,omitempty"`
}

// DocRef carries the document facets a node renders. When Path is non-empty the
// node becomes a file node pointing at that vault-relative path (Obsidian renders
// the live document); otherwise Title + Excerpt render a self-contained text card.
type DocRef struct {
	Path    string
	Title   string
	Excerpt string
}

// GraphNode is one node to render: a graph entity, optionally a document. Hops
// is its distance from the seed (0 = the seed) and drives the radial ring it
// lands on.
type GraphNode struct {
	UID         string
	Kind        string
	DisplayName string
	Hops        int
	Doc         *DocRef
}

// GraphEdge is one relation to render between two nodes.
type GraphEdge struct {
	UID        string
	FromUID    string
	ToUID      string
	Predicate  string
	Confidence float64
}

// Subgraph is the decoupled input to Build: the seed UID plus the nodes and
// edges to render. It deliberately does not depend on the navigate package so
// the serializer stays a pure, independently testable presentation unit.
type Subgraph struct {
	SeedUID string
	Nodes   []GraphNode
	Edges   []GraphEdge
}

// Build serializes sg into a JSON Canvas document. It is the single source of
// ordering and layout: it sorts nodes by (hops, uid) and edges by uid, lays
// nodes out on concentric radial rings by hop, and drops edges that are
// self-loops or reference a node outside the set (deduping by UID). Because the
// ordering, layout, and IDs are all derived deterministically from the input,
// Build is referentially transparent: shuffled input yields identical output.
func Build(sg Subgraph) Canvas {
	nodes := append([]GraphNode(nil), sg.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Hops != nodes[j].Hops {
			return nodes[i].Hops < nodes[j].Hops
		}
		return nodes[i].UID < nodes[j].UID
	})

	ringCount := map[int]int{}
	inSet := make(map[string]bool, len(nodes))
	for i := range nodes {
		ringCount[nodes[i].Hops]++
		inSet[nodes[i].UID] = true
	}

	out := Canvas{
		Nodes: make([]Node, 0, len(nodes)),
		Edges: make([]Edge, 0, len(sg.Edges)),
	}
	ringIndex := map[int]int{}
	for i := range nodes {
		gn := nodes[i]
		idx := ringIndex[gn.Hops]
		ringIndex[gn.Hops]++
		x, y := position(gn.Hops, idx, ringCount[gn.Hops])
		out.Nodes = append(out.Nodes, buildNode(gn, sg.SeedUID, x, y))
	}

	// Build owns the edge invariants itself rather than trusting the input:
	// Subgraph is a public type a caller can hand-construct (and the tests do),
	// so drop self-loops and edges to a node outside the set, and dedup by UID.
	edges := append([]GraphEdge(nil), sg.Edges...)
	sort.Slice(edges, func(i, j int) bool { return edges[i].UID < edges[j].UID })
	seen := make(map[string]bool, len(edges))
	for i := range edges {
		ge := edges[i]
		if ge.FromUID == ge.ToUID || !inSet[ge.FromUID] || !inSet[ge.ToUID] || seen[ge.UID] {
			continue
		}
		seen[ge.UID] = true
		out.Edges = append(out.Edges, Edge{
			ID:       ge.UID,
			FromNode: ge.FromUID,
			ToNode:   ge.ToUID,
			Label:    edgeLabel(ge),
		})
	}
	return out
}

// buildNode renders one graph node. A document with a resolvable vault path
// becomes a file node (live Obsidian render); everything else is a text card.
func buildNode(gn GraphNode, seedUID string, x, y int) Node {
	n := Node{ID: gn.UID, X: x, Y: y, Width: nodeWidth, Height: nodeHeight, Color: nodeColor(gn, seedUID)}
	if gn.Doc != nil && gn.Doc.Path != "" {
		n.Type = "file"
		n.File = gn.Doc.Path
		return n
	}
	n.Type = "text"
	n.Text = nodeText(gn)
	return n
}

// nodeColor classes a node: seed (focal) > document > plain entity (default).
func nodeColor(gn GraphNode, seedUID string) string {
	switch {
	case gn.UID == seedUID:
		return colorSeed
	case gn.Doc != nil:
		return colorDocument
	default:
		return ""
	}
}

// nodeText renders a text-card body: a document falls back to its title +
// excerpt; an entity shows its name and kind.
func nodeText(gn GraphNode) string {
	if gn.Doc != nil {
		title := gn.Doc.Title
		if title == "" {
			title = gn.DisplayName
		}
		if gn.Doc.Excerpt != "" {
			return fmt.Sprintf("# %s\n\n%s", title, gn.Doc.Excerpt)
		}
		return "# " + title
	}
	return fmt.Sprintf("# %s\n\n_%s_", gn.DisplayName, gn.Kind)
}

// edgeLabel renders an edge label: the predicate, annotated with the confidence
// when it is a genuine partial belief (strictly between 0 and 1).
func edgeLabel(ge GraphEdge) string {
	if ge.Confidence > 0 && ge.Confidence < 1 {
		return fmt.Sprintf("%s (%.2f)", ge.Predicate, ge.Confidence)
	}
	return ge.Predicate
}

// MarshalIndent renders the canvas as tab-indented JSON with a trailing newline,
// the human- and git-friendly shape for a .canvas file committed to the vault.
func (c Canvas) MarshalIndent() ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "\t")
	if err != nil {
		return nil, fmt.Errorf("marshal canvas: %w", err)
	}
	return append(b, '\n'), nil
}
