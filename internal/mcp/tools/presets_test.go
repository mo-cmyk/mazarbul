package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

type fakePresetLister struct {
	presets []navigate.Preset
	err     error
}

func (f *fakePresetLister) ActivePresets(_ context.Context) ([]navigate.Preset, error) {
	return f.presets, f.err
}

func TestListPresets_ReturnsSummariesWithoutBody(t *testing.T) {
	t.Parallel()
	src := &fakePresetLister{presets: []navigate.Preset{
		{Name: "crm", Version: "0.1.0", Description: "CRM vocab.", Instructions: "SECRET STEERING BODY"},
		{Name: "notes-organizer", Version: "0.2.0", Description: "Notes vocab.", Instructions: "more steering"},
	}}
	tool := tools.NewListPresets(src)

	raw, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// The verbatim instructions body must never appear in the tool output.
	if strings.Contains(string(raw), "SECRET STEERING BODY") || strings.Contains(string(raw), "more steering") {
		t.Fatalf("instructions body leaked into listPresets output: %s", raw)
	}

	var out struct {
		Presets []struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			Description string `json:"description"`
		} `json:"presets"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Presets) != 2 {
		t.Fatalf("got %d presets, want 2", len(out.Presets))
	}
	if out.Presets[0].Name != "crm" || out.Presets[0].Version != "0.1.0" {
		t.Errorf("first preset = %+v, want crm/0.1.0", out.Presets[0])
	}
}

func TestListPresets_EmptyReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	tool := tools.NewListPresets(&fakePresetLister{presets: nil})
	raw, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(string(raw), `"presets":[]`) {
		t.Errorf("empty case should serialize an empty array, got %s", raw)
	}
}

func TestListPresets_Name(t *testing.T) {
	t.Parallel()
	if got := tools.NewListPresets(&fakePresetLister{}).Name(); got != "listPresets" {
		t.Errorf("Name() = %q, want listPresets", got)
	}
}

// TestListPresets_NoMutationVerb guards the #224 invariant: the only
// agent-facing preset tool is the read-only lister. Remote install is
// deliberately CLI-only and human-gated; no install/fetch/package tool may be
// exposed to agents.
func TestListPresets_NoMutationVerb(t *testing.T) {
	t.Parallel()
	name := strings.ToLower(tools.NewListPresets(&fakePresetLister{}).Name())
	for _, verb := range []string{"install", "fetch", "package"} {
		if strings.Contains(name, verb) {
			t.Errorf("preset tool name %q must not be a mutation verb (%q)", name, verb)
		}
	}
}

// TestListPresets_OutputSchemaHidesBodyAndProvenance keeps the agent surface
// from leaking the steering body or the remote provenance (#224): an agent is
// already steered via getContext and never needs the raw body, source, ref, or
// checksum.
func TestListPresets_OutputSchemaHidesBodyAndProvenance(t *testing.T) {
	t.Parallel()
	schema := string(tools.NewListPresets(&fakePresetLister{}).OutputSchema())
	for _, forbidden := range []string{"instructions", "sourceUrl", "pinnedRef", "checksum"} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("listPresets output schema must not expose %q: %s", forbidden, schema)
		}
	}
}
