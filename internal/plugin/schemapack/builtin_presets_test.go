package schemapack_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// TestBuiltinPresets_ParseAsPresets guards that the two builtin presets parse,
// are recognised as presets (an instructions.md is present), and stay within
// the parse-time byte cap. A pack that loses its instructions.md silently
// degrades to a plain schema pack — this test fails loudly if that happens.
func TestBuiltinPresets_ParseAsPresets(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"crm", "notes-organizer", "decision-trace"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pack, err := schemapack.LoadBuiltin(name)
			if err != nil {
				t.Fatalf("LoadBuiltin(%q): %v", name, err)
			}
			if !pack.IsPreset() {
				t.Fatalf("builtin %q is not a preset (no instructions.md)", name)
			}
			if len(pack.Instructions) == 0 {
				t.Errorf("builtin %q has an empty instructions body", name)
			}
			if len(pack.Instructions) > schemapack.MaxInstructionsBytes {
				t.Errorf("builtin %q instructions = %d bytes, exceeds cap %d",
					name, len(pack.Instructions), schemapack.MaxInstructionsBytes)
			}
			if len(pack.Kinds) == 0 {
				t.Errorf("builtin preset %q seeds no entity kinds", name)
			}
			if len(pack.Predicates) == 0 {
				t.Errorf("builtin preset %q seeds no predicates", name)
			}
		})
	}
}

// TestAgentMemoryPreset_InstructionsOnly guards the steering preset the
// onboarding on-ramp installs. It is a preset (has instructions within the
// cap) but deliberately seeds NO ontology — its only job is to teach the agent
// when to call the memory tools.
func TestAgentMemoryPreset_InstructionsOnly(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.LoadBuiltin(schemapack.DefaultSteeringPreset)
	if err != nil {
		t.Fatalf("LoadBuiltin(%q): %v", schemapack.DefaultSteeringPreset, err)
	}
	if !pack.IsPreset() {
		t.Fatalf("%q is not a preset (no instructions.md)", schemapack.DefaultSteeringPreset)
	}
	if len(pack.Instructions) == 0 {
		t.Errorf("%q has an empty instructions body", schemapack.DefaultSteeringPreset)
	}
	if len(pack.Instructions) > schemapack.MaxInstructionsBytes {
		t.Errorf("%q instructions = %d bytes, exceeds cap %d",
			schemapack.DefaultSteeringPreset, len(pack.Instructions), schemapack.MaxInstructionsBytes)
	}
	if len(pack.Kinds) != 0 {
		t.Errorf("%q seeds %d entity kinds; want 0 (instructions-only)", schemapack.DefaultSteeringPreset, len(pack.Kinds))
	}
	if len(pack.Predicates) != 0 {
		t.Errorf("%q seeds %d predicates; want 0 (instructions-only)", schemapack.DefaultSteeringPreset, len(pack.Predicates))
	}
}

// TestBuiltinSchemaPacks_AreNotPresets guards the inverse: the two pure schema
// packs must NOT be presets, so `maz preset install schema-org-core` is
// correctly rejected and they keep flowing through `maz install`.
func TestBuiltinSchemaPacks_AreNotPresets(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"schema-org-core", "business-de"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pack, err := schemapack.LoadBuiltin(name)
			if err != nil {
				t.Fatalf("LoadBuiltin(%q): %v", name, err)
			}
			if pack.IsPreset() {
				t.Errorf("schema pack %q unexpectedly parsed as a preset", name)
			}
		})
	}
}
