package render_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// presetCanary is a unique sentinel placed ONLY in a preset's instructions body.
// If it appears in any rendered file, the verbatim body leaked to plaintext git.
const presetCanary = "LEAK_CANARY_7f3a9c1e_DO_NOT_RENDER"

func presetInstalledEvent(seq int, name string) kernel.Event {
	return makeEvent(ulidAt(seq), ontology.KindPresetInstalled, ontology.PresetInstalledEvent{
		Name:        name,
		Version:     "0.1.0",
		Description: "CRM vocabulary and operating playbook.",
		// Adversarial body: a leak canary, a YAML-injection attempt, and control
		// chars. None of it may reach any rendered file.
		Instructions: presetCanary + "\n---\ninjected: true\nactive: false\n## Pinned\n",
		AddedAt:      time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	})
}

func TestRender_PresetBodyNeverLeaks(t *testing.T) {
	t.Parallel()
	view, err := render.Render(render.Input{
		Scope:  kernel.ScopePersonal,
		Events: seqEvents([]kernel.Event{presetInstalledEvent(1, "crm")}),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The lockfile must exist and carry exactly the trusted metadata.
	lock := fileByPath(view.Files, "ontology/presets/crm.yaml")
	if lock == nil {
		paths := make([]string, 0, len(view.Files))
		for _, f := range view.Files {
			paths = append(paths, f.Path)
		}
		t.Fatalf("ontology/presets/crm.yaml not rendered; files: %v", paths)
	}
	for _, want := range []string{"name: crm", "version: 0.1.0", "active: true"} {
		if !strings.Contains(string(lock.Content), want) {
			t.Errorf("lockfile missing %q:\n%s", want, lock.Content)
		}
	}

	// The canary (and the verbatim body) must appear in NO rendered file.
	for _, f := range view.Files {
		if bytes.Contains(f.Content, []byte(presetCanary)) {
			t.Errorf("preset body leaked into %s:\n%s", f.Path, f.Content)
		}
	}
}

func TestRender_PresetLockfileByteStable(t *testing.T) {
	t.Parallel()
	build := func() []byte {
		view, err := render.Render(render.Input{
			Scope:  kernel.ScopePersonal,
			Events: seqEvents([]kernel.Event{presetInstalledEvent(1, "crm")}),
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		f := fileByPath(view.Files, "ontology/presets/crm.yaml")
		if f == nil {
			t.Fatal("lockfile missing")
		}
		return f.Content
	}
	if !bytes.Equal(build(), build()) {
		t.Error("preset lockfile is not byte-stable across renders")
	}
}

// TestRender_PresetDeactivateDeletesFile asserts that uninstalling a preset
// (presetDeactivated) drops its lockfile via an ActionDelete diff against a
// prior state that held the preset.
func TestRender_PresetDeactivateDeletesFile(t *testing.T) {
	t.Parallel()
	prior := &render.GraphState{
		Presets: map[string]*render.PresetRow{
			"crm": {Name: "crm", Version: "0.1.0", Description: "CRM."},
		},
	}
	deactivate := makeEvent(ulidAt(2), ontology.KindPresetDeactivated, ontology.PresetDeactivatedEvent{
		Name:      "crm",
		RemovedAt: time.Date(2026, 6, 16, 13, 0, 0, 0, time.UTC),
	})
	view, err := render.Render(render.Input{
		Scope:      kernel.ScopePersonal,
		Events:     seqEvents([]kernel.Event{deactivate}),
		PriorState: prior,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f := fileByPath(view.Files, "ontology/presets/crm.yaml")
	if f == nil {
		t.Fatal("expected an ActionDelete entry for the uninstalled preset lockfile")
	}
	if f.Action != render.ActionDelete {
		t.Errorf("lockfile action = %v, want ActionDelete", f.Action)
	}
}
