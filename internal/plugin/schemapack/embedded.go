package schemapack

import (
	"embed"
	"fmt"
)

// packsFS holds the bundled schema packs. Each pack lives in its own
// subdirectory under packs/ so ParseFS can address them by root. The crm,
// notes-organizer, and agent-memory packs additionally ship an instructions.md,
// so they are also installable as presets via `maz preset install`. The
// agent-memory pack is instructions-only (no kinds/predicates): its sole job is
// to steer agents on *when* to call the memory tools.
//
//go:embed packs/schema-org-core packs/business-de packs/crm packs/notes-organizer packs/agent-memory packs/decision-trace packs/second-brain
var packsFS embed.FS

// BuiltinPackNames lists the names of the bundled packs in the order they
// should be installed (schema-org-core first, so business-de can extend its
// kinds). Crm, notes-organizer, and agent-memory extend only core genesis kinds
// (agent-memory none at all), so their relative order is free.
//
//nolint:gochecknoglobals // read-only lookup, never mutated
var BuiltinPackNames = []string{"schema-org-core", "business-de", "crm", "notes-organizer", "agent-memory", "decision-trace", "second-brain"}

// DefaultSteeringPreset is the builtin preset the onboarding on-ramp installs to
// teach an agent when to call the memory tools. Instructions-only; seeds no
// ontology. Named here so `maz init` and docs reference one constant.
const DefaultSteeringPreset = "agent-memory"

// LoadBuiltin parses one of the embedded packs by name. Returns
// ErrInvalidManifest when name is not a recognised builtin.
func LoadBuiltin(name string) (*Pack, error) {
	for _, n := range BuiltinPackNames {
		if n == name {
			return ParseFS(packsFS, "packs/"+name)
		}
	}
	return nil, fmt.Errorf("%w: unknown builtin pack %q; known: %v", ErrInvalidManifest, name, BuiltinPackNames)
}

// LoadAllBuiltins parses all bundled packs and returns them in declaration
// order (schema-org-core first). An error in any pack aborts the load.
func LoadAllBuiltins() ([]*Pack, error) {
	packs := make([]*Pack, 0, len(BuiltinPackNames))
	for _, name := range BuiltinPackNames {
		p, err := LoadBuiltin(name)
		if err != nil {
			return nil, fmt.Errorf("load builtin %q: %w", name, err)
		}
		packs = append(packs, p)
	}
	return packs, nil
}
