package render_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// dashboardRenderedAt is a fixed injected timestamp for the dashboard banner;
// the renderer never reads a clock, so the test supplies one.
var dashboardRenderedAt = time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

// renderDashboard renders events with the given injected dashboard inputs and
// returns the DASHBOARD.md content. Fails if the renderer omitted it.
func renderDashboard(t *testing.T, events []kernel.Event, devices []string) string {
	t.Helper()
	view, err := render.Render(render.Input{
		Scope:      kernel.ScopePersonal,
		Events:     seqEvents(events),
		HeadULID:   ulidAt(999999),
		Device:     "dev-a",
		Devices:    devices,
		RenderedAt: dashboardRenderedAt,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, f := range view.Files {
		if f.Path == "DASHBOARD.md" {
			return string(f.Content)
		}
	}
	t.Fatalf("DASHBOARD.md not emitted; files = %d", len(view.Files))
	return ""
}

// connectedGraphEvents builds a predicate plus n people where person i relates
// to person i+1, so every person but the last has at least one live edge.
func connectedGraphEvents(n int) []kernel.Event {
	events := []kernel.Event{
		makeEvent(ulidAt(0), ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate:   "knows",
			Kind:        ontology.PredicateKindNonFunctional,
			Description: "Person knows Person.",
		}),
	}
	for i := 1; i <= n; i++ {
		ent := ulidAt(i * 10)
		events = append(events, makeEvent(ent, ontology.KindEntity,
			entityPayload(ent, "Person "+itoa(i), ontology.EntityKindPerson)))
	}
	for i := 1; i < n; i++ {
		rel := ulidAt(i*10 + 2)
		events = append(events, makeEvent(rel, ontology.KindRelation,
			relationPayload(rel, ulidAt(i*10), "knows", ulidAt((i+1)*10))))
	}
	return events
}

func TestDashboard_LandingSections(t *testing.T) {
	t.Parallel()
	// dev-a appears twice and out of order: the banner must dedupe + sort.
	body := renderDashboard(t, connectedGraphEvents(4), []string{"dev-b", "dev-a", "dev-a"})

	for _, want := range []string{
		"# Your Mazarbul memory — `personal` scope",
		"do not edit",
		"_Last synced 2026-06-03 12:00 UTC from 2 device(s): dev-a, dev-b._",
		"## At a glance",
		"👤 person",
		"| **Entities** | **4** |",
		"🔗 relations (live)",
		"## Activity",
		"## Graph",
		"```mermaid",
		"graph LR",
		"maz doc canvas",
		"[`README.md`](README.md)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("DASHBOARD.md missing %q\n---\n%s", want, body)
		}
	}
}

func TestDashboard_MermaidEscapesLabels(t *testing.T) {
	t.Parallel()
	// Two entities with adversarial display names, related so both rank into
	// the graph.
	a, b := ulidAt(10), ulidAt(20)
	events := []kernel.Event{
		makeEvent(ulidAt(0), ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate: "knows", Kind: ontology.PredicateKindNonFunctional, Description: "x",
		}),
		makeEvent(a, ontology.KindEntity, entityPayload(a, `Alice "The Boss" 100%`, ontology.EntityKindPerson)),
		makeEvent(b, ontology.KindEntity, entityPayload(b, "Bob", ontology.EntityKindPerson)),
		makeEvent(ulidAt(12), ontology.KindRelation, relationPayload(ulidAt(12), a, "knows", b)),
	}
	body := renderDashboard(t, events, nil)

	if !strings.Contains(body, "#quot;The Boss#quot;") {
		t.Errorf("double-quote not escaped to #quot;\n%s", body)
	}
	if !strings.Contains(body, "100#37;") {
		t.Errorf("percent not escaped to #37;\n%s", body)
	}
	// A raw quoted name inside the label would break the Mermaid line.
	if strings.Contains(body, `["Alice "The Boss"`) {
		t.Errorf("raw unescaped quote leaked into a Mermaid label\n%s", body)
	}
}

func TestDashboard_GraphCapsNodes(t *testing.T) {
	t.Parallel()
	// 26 connected people; the graph must cap at 20 nodes.
	body := renderDashboard(t, connectedGraphEvents(26), nil)
	nodes := strings.Count(body, `["`) // node declarations are the only `["` token
	if nodes != 20 {
		t.Errorf("Mermaid node count = %d, want 20 (capped)", nodes)
	}
}

func TestDashboard_EmptyGraph(t *testing.T) {
	t.Parallel()
	// A predicate registration but no entities: counts are zero and the graph
	// degrades to a friendly placeholder rather than an empty Mermaid block.
	events := []kernel.Event{
		makeEvent(ulidAt(0), ontology.KindPredicateRegistered, ontology.PredicateRegisteredEvent{
			Predicate: "knows", Kind: ontology.PredicateKindNonFunctional, Description: "x",
		}),
	}
	body := renderDashboard(t, events, nil)
	if !strings.Contains(body, "| **Entities** | **0** |") {
		t.Errorf("empty graph must report 0 entities\n%s", body)
	}
	if !strings.Contains(body, "No connected entities yet") {
		t.Errorf("empty graph must show the placeholder, not a Mermaid block\n%s", body)
	}
	if strings.Contains(body, "```mermaid") {
		t.Errorf("empty graph must not emit a Mermaid block\n%s", body)
	}
}

// TestDashboard_DeterministicWithInjectedValues asserts the dashboard is
// byte-identical across runs once RenderedAt + Devices are fixed — the same
// guarantee the rest of the renderer makes, extended to the two injected
// values that would otherwise be a non-determinism vector.
func TestDashboard_DeterministicWithInjectedValues(t *testing.T) {
	t.Parallel()
	events := connectedGraphEvents(8)
	devices := []string{"dev-b", "dev-a"}

	reference := sha256.Sum256([]byte(renderDashboard(t, events, devices)))
	for run := range 30 {
		got := sha256.Sum256([]byte(renderDashboard(t, events, devices)))
		if hex.EncodeToString(got[:]) != hex.EncodeToString(reference[:]) {
			t.Fatalf("DASHBOARD.md diverged on run %d/30", run+1)
		}
	}
}
