package kernel

import (
	"context"
	"encoding/json"
)

// Schema is a JSON Schema definition for an event kind, entity type, or
// any other named shape Mazarbul exchanges across process or sync
// boundaries. The Definition is the literal JSON Schema document; the
// Version supports forward-compat checks at read time.
//
// Schemas are written by users (YAML), generated from Go structs via
// invopop/jsonschema, or shipped with extensions. The registry's job is
// to keep one definition per Kind and reject conflicting registrations.
type Schema struct {
	// Kind is the lookup key. Must match an EventKind for event schemas, or
	// an EntityKind for entity schemas.
	Kind string

	// Version is a semver-shaped tag. v0.x means "expect breaking changes
	// before v1.0".
	Version string

	// Definition is the JSON Schema document. Parsed lazily by validators.
	Definition json.RawMessage

	// Provenance identifies who registered the schema: "core" for built-in,
	// "extension:<name>" for extension-registered, "user" for hand-written.
	Provenance string
}

// Ontology is the registry for every schema Mazarbul knows about. Real
// implementations persist registrations to disk so the registry survives
// restart. M1 ships with the core schemas for Episode, Entity, Mention,
// and Relation (see agent-docs/designs/ontology.md once it lands).
type Ontology interface {
	// Schema returns the registered schema for kind, or ErrNotFound if none.
	Schema(ctx context.Context, kind string) (Schema, error)

	// Register installs a schema. Same (Kind, Version, Definition) tuple is
	// idempotent. A subsequent registration for the same Kind with a Version
	// strictly greater than the existing one replaces it; same-or-lower
	// Version with a different Definition returns ErrSchemaConflict.
	Register(ctx context.Context, schema Schema) error

	// List returns every registered schema. Callers must not rely on a
	// specific iteration order; implementations should sort deterministically
	// (by Kind, then Version) so tests and diffs are stable.
	List(ctx context.Context) ([]Schema, error)

	// Validate checks a payload against the schema for kind. Returns nil
	// when the payload satisfies the schema, ErrSchemaNotRegistered when no
	// schema is registered for kind (distinct from ErrNotFound so callers
	// can warning-and-accept missing-schema writes), or a validation error
	// describing the first failure.
	Validate(ctx context.Context, kind string, payload json.RawMessage) error
}

// ErrSchemaConflict signals an attempt to register a conflicting schema
// for an existing kind without bumping the version.
var ErrSchemaConflict = errorf("kernel: schema conflict")

// ErrSchemaNotRegistered is returned by Validate when no schema is
// registered for kind. Distinct from ErrNotFound so callers (the event
// log writer in #20) can warning-and-accept missing-schema writes while
// a hard ErrNotFound stays fatal for explicit Schema lookups. Strict-mode
// validation in M2 turns this into a hard rejection.
var ErrSchemaNotRegistered = errorf("kernel: schema not registered")

// errorf is a tiny constructor used in this file to avoid the errors
// package round-trip when an error value is a static sentinel.
type sentinelError string

func (e sentinelError) Error() string { return string(e) }

func errorf(s string) error { return sentinelError(s) }

// NoopOntology is the placeholder that returns ErrNotImplemented for every
// operation. Replaced in M1 by the on-disk registry.
type NoopOntology struct{}

// Schema implements Ontology. It returns ErrNotImplemented.
func (NoopOntology) Schema(_ context.Context, _ string) (Schema, error) {
	return Schema{}, ErrNotImplemented
}

// Register implements Ontology. It returns ErrNotImplemented.
func (NoopOntology) Register(_ context.Context, _ Schema) error {
	return ErrNotImplemented
}

// List implements Ontology. It returns ErrNotImplemented so an unwired
// registry cannot be mistaken for a real empty registry by a caller that
// loads schemas during startup.
func (NoopOntology) List(_ context.Context) ([]Schema, error) {
	return nil, ErrNotImplemented
}

// Validate implements Ontology. It returns ErrNotImplemented.
func (NoopOntology) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	return ErrNotImplemented
}
