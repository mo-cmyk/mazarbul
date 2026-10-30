package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// writeOntologyKindYAML produces the YAML content for one entity kind file at
// ontology/kinds/<name>.yaml. The output is deterministic: fields appear in a
// fixed order and the sameAs list is pre-sorted.
func writeOntologyKindYAML(row *EntityKindRow) []byte {
	var buf strings.Builder
	buf.WriteString("---\n")
	buf.WriteString("name: ")
	buf.WriteString(renderYAMLString(row.Name))
	buf.WriteString("\n")
	buf.WriteString("display_name: ")
	buf.WriteString(renderYAMLString(row.DisplayName))
	buf.WriteString("\n")
	buf.WriteString("description: ")
	buf.WriteString(renderYAMLString(row.Description))
	buf.WriteString("\n")
	if row.Extends != "" {
		buf.WriteString("extends: ")
		buf.WriteString(renderYAMLString(row.Extends))
		buf.WriteString("\n")
	}
	if len(row.SameAs) > 0 {
		sorted := make([]string, len(row.SameAs))
		copy(sorted, row.SameAs)
		sort.Strings(sorted)
		buf.WriteString("same_as: ")
		buf.WriteString(renderYAMLStringSlice(sorted))
		buf.WriteString("\n")
	}
	if len(row.PropertySchema) > 0 {
		// Compact the JSON so the YAML scalar stays on one line and is
		// byte-stable regardless of source whitespace.
		var compact bytes.Buffer
		if err := json.Compact(&compact, row.PropertySchema); err == nil {
			buf.WriteString("property_schema: ")
			buf.WriteString(renderYAMLString(compact.String()))
			buf.WriteString("\n")
		}
	}
	// Emit status always for clarity; active is the common case but proposals
	// and rejections must travel through the git transport so peers can see them.
	status := row.Status
	if status == "" {
		status = ontology.EntityKindStatusActive
	}
	buf.WriteString("status: ")
	buf.WriteString(renderYAMLString(status))
	buf.WriteString("\n")
	buf.WriteString("provenance: ")
	buf.WriteString(renderYAMLString(row.Provenance))
	buf.WriteString("\n")
	buf.WriteString("added_by: ")
	buf.WriteString(renderYAMLString(row.AddedBy))
	buf.WriteString("\n")
	buf.WriteString("added_at: ")
	buf.WriteString(row.AddedAt.UTC().Format(time.RFC3339Nano))
	buf.WriteString("\n")
	return []byte(buf.String())
}

// writePredicatesYAML produces the YAML content for ontology/predicates.yaml,
// one YAML document with a top-level list of all registered predicates sorted
// by predicate name. The v0.2 typed-relation metadata (domain/range, sameAs,
// attribute propertySchema) is emitted when present so the git-rendered
// vocabulary carries the same typing the runtime registry enforces; subject/
// object kinds keep the author's event order (domain/range intent), while
// same_as is sorted like the kind YAML's.
func writePredicatesYAML(state *GraphState) []byte {
	names := sortedKeys(state.Predicates)
	if len(names) == 0 {
		return nil
	}

	var buf strings.Builder
	buf.WriteString("predicates:\n")
	for _, name := range names {
		row := state.Predicates[name]
		buf.WriteString("  - predicate: ")
		buf.WriteString(renderYAMLString(row.Predicate))
		buf.WriteString("\n")
		buf.WriteString("    kind: ")
		buf.WriteString(string(row.Kind))
		buf.WriteString("\n")
		if row.Description != "" {
			buf.WriteString("    description: ")
			buf.WriteString(renderYAMLString(row.Description))
			buf.WriteString("\n")
		}
		if len(row.SubjectKinds) > 0 {
			buf.WriteString("    subject_kinds: ")
			buf.WriteString(renderYAMLStringSlice(row.SubjectKinds))
			buf.WriteString("\n")
		}
		if len(row.ObjectKinds) > 0 {
			buf.WriteString("    object_kinds: ")
			buf.WriteString(renderYAMLStringSlice(row.ObjectKinds))
			buf.WriteString("\n")
		}
		if len(row.SameAs) > 0 {
			sorted := make([]string, len(row.SameAs))
			copy(sorted, row.SameAs)
			sort.Strings(sorted)
			buf.WriteString("    same_as: ")
			buf.WriteString(renderYAMLStringSlice(sorted))
			buf.WriteString("\n")
		}
		if len(row.PropertySchema) > 0 {
			// Compact the JSON so the YAML scalar stays on one line and is
			// byte-stable regardless of source whitespace.
			var compact bytes.Buffer
			if err := json.Compact(&compact, row.PropertySchema); err == nil {
				buf.WriteString("    property_schema: ")
				buf.WriteString(renderYAMLString(compact.String()))
				buf.WriteString("\n")
			}
		}
		buf.WriteString("    added_by: ")
		buf.WriteString(string(row.AddedBy))
		buf.WriteString("\n")
		buf.WriteString("    added_at: ")
		buf.WriteString(row.AddedAt.UTC().Format(time.RFC3339Nano))
		buf.WriteString("\n")
	}
	return []byte(buf.String())
}

// writeOntologySchemaJSON produces the deterministic JSON content for one
// runtime-registered schema at ontology/schemas/<schemaKind>.json. The
// definition is re-marshaled through a canonical map so byte output is stable
// regardless of the source key ordering or whitespace.
func writeOntologySchemaJSON(row *SchemaRow) ([]byte, error) {
	var definition any
	if len(row.Definition) > 0 {
		if err := json.Unmarshal(row.Definition, &definition); err != nil {
			return nil, fmt.Errorf("render: unmarshal schema definition %q: %w", row.SchemaKind, err)
		}
	}
	doc := struct {
		SchemaKind string `json:"schemaKind"`
		Version    string `json:"version"`
		Provenance string `json:"provenance"`
		AddedBy    string `json:"addedBy"`
		AddedAt    string `json:"addedAt"`
		Definition any    `json:"definition"`
	}{
		SchemaKind: row.SchemaKind,
		Version:    row.Version,
		Provenance: row.Provenance,
		AddedBy:    row.AddedBy,
		AddedAt:    row.AddedAt.UTC().Format(time.RFC3339Nano),
		Definition: definition,
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render: marshal schema %q: %w", row.SchemaKind, err)
	}
	return append(out, '\n'), nil
}

// ontologyKindPath returns the rendered path for one entity kind YAML file.
func ontologyKindPath(name string) string {
	return "ontology/kinds/" + name + ".yaml"
}

// ontologySchemaPath returns the rendered path for one runtime schema file.
// It delegates to ontology.SchemaKindRenderFilename for the frozen,
// injective render-path contract (the namespace colon maps to a hyphen so the
// filename is checkout/sync-safe on every platform and two distinct keys can
// never collide).
func ontologySchemaPath(schemaKind string) string {
	return "ontology/schemas/" + ontology.SchemaKindRenderFilename(schemaKind)
}

// ontologyPredicatesPath is the path of the consolidated predicates file.
const ontologyPredicatesPath = "ontology/predicates.yaml"

// presetPath returns the rendered path for one active preset's lockfile.
func presetPath(name string) string {
	return "ontology/presets/" + name + ".yaml"
}

// writePresetYAML produces the lockfile content for one active preset at
// ontology/presets/<name>.yaml. It emits ONLY name/version/description plus a
// constant `active: true`; the verbatim instructions body is never rendered
// (PresetRow has no body field), so this output is determined entirely by the
// trusted metadata and an adversarial body can never influence it.
func writePresetYAML(row *PresetRow) []byte {
	var buf strings.Builder
	buf.WriteString("---\n")
	buf.WriteString("name: ")
	buf.WriteString(renderYAMLString(row.Name))
	buf.WriteString("\n")
	buf.WriteString("version: ")
	buf.WriteString(renderYAMLString(row.Version))
	buf.WriteString("\n")
	buf.WriteString("description: ")
	buf.WriteString(renderYAMLString(row.Description))
	buf.WriteString("\n")
	buf.WriteString("active: true\n")
	return []byte(buf.String())
}

// collectOntologyFiles populates current with ontology/kinds/,
// ontology/schemas/, and ontology/predicates.yaml outputs. Called from
// assembleFiles.
func collectOntologyFiles(current map[string][]byte, state *GraphState) error {
	for _, name := range sortedKeys(state.EntityKinds) {
		row := state.EntityKinds[name]
		current[ontologyKindPath(name)] = writeOntologyKindYAML(row)
	}
	for _, kind := range sortedKeys(state.Schemas) {
		out, err := writeOntologySchemaJSON(state.Schemas[kind])
		if err != nil {
			return err
		}
		current[ontologySchemaPath(kind)] = out
	}
	if pred := writePredicatesYAML(state); len(pred) > 0 {
		current[ontologyPredicatesPath] = pred
	}
	for _, name := range sortedKeys(state.Presets) {
		current[presetPath(name)] = writePresetYAML(state.Presets[name])
	}
	return nil
}

// buildPriorOntologyEmission populates em with the prior-state ontology file
// set so diffFiles can detect adds/updates/deletes. Called from
// buildPriorEmission.
func buildPriorOntologyEmission(em priorEmission, priorState *GraphState) error {
	for _, name := range sortedKeys(priorState.EntityKinds) {
		row := priorState.EntityKinds[name]
		em[ontologyKindPath(name)] = writeOntologyKindYAML(row)
	}
	for _, kind := range sortedKeys(priorState.Schemas) {
		out, err := writeOntologySchemaJSON(priorState.Schemas[kind])
		if err != nil {
			return err
		}
		em[ontologySchemaPath(kind)] = out
	}
	if pred := writePredicatesYAML(priorState); len(pred) > 0 {
		em[ontologyPredicatesPath] = pred
	}
	for _, name := range sortedKeys(priorState.Presets) {
		em[presetPath(name)] = writePresetYAML(priorState.Presets[name])
	}
	return nil
}
