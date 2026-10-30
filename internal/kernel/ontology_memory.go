package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/mod/semver"
)

// MemoryOntology is the in-process implementation of Ontology used by the
// M1 walking skeleton. Registrations live in a map keyed by kind; compiled
// JSON Schema validators are cached per kind and invalidated on a successful
// version-bump replacement. Disk persistence lands in M2.
//
// Safe for concurrent use. The zero value is not usable; call NewMemoryOntology.
type MemoryOntology struct {
	mu       sync.RWMutex
	schemas  map[string]Schema
	compiled map[string]*compiledEntry
}

// compiledEntry caches a lazily compiled JSON Schema validator. The
// sync.Once guarantees a single compile per (Kind, Definition) pair across
// goroutines; Register publishes a fresh entry under the write lock when
// a schema is replaced, so an inflight Validate that already holds an old
// pointer keeps using the old (still-valid) compiled validator.
type compiledEntry struct {
	once   sync.Once
	schema *jsonschema.Schema
	err    error
}

// NewMemoryOntology returns an empty registry. Callers (the kernel
// orchestrator in #55) seed it by calling Register for every built-in
// schema during startup.
func NewMemoryOntology() *MemoryOntology {
	return &MemoryOntology{
		schemas:  make(map[string]Schema),
		compiled: make(map[string]*compiledEntry),
	}
}

// Schema implements Ontology. Returns the registered schema for kind, or
// ErrNotFound if none. The returned Schema's Definition is a fresh copy;
// callers may mutate it without affecting the registry.
func (o *MemoryOntology) Schema(_ context.Context, kind string) (Schema, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	s, ok := o.schemas[kind]
	if !ok {
		return Schema{}, ErrNotFound
	}
	return cloneSchema(s), nil
}

// Register implements Ontology. See the interface doc comment for the
// idempotence and conflict rules. A non-semver Version, empty Kind, or
// empty Definition returns a wrapped error.
func (o *MemoryOntology) Register(_ context.Context, schema Schema) error {
	if err := validateSchemaInput(schema); err != nil {
		return err
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	existing, present := o.schemas[schema.Kind]
	if present {
		switch cmp := semver.Compare(schema.Version, existing.Version); {
		case cmp == 0 && bytes.Equal(schema.Definition, existing.Definition):
			return nil
		case cmp > 0:
			// Higher version: replace and invalidate the compile cache.
		default:
			return ErrSchemaConflict
		}
	}

	o.schemas[schema.Kind] = cloneSchema(schema)
	o.compiled[schema.Kind] = &compiledEntry{}
	return nil
}

// List implements Ontology. Returns every registered schema sorted by
// (Kind, Version) for determinism. Each entry's Definition is a fresh copy.
func (o *MemoryOntology) List(_ context.Context) ([]Schema, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	out := make([]Schema, 0, len(o.schemas))
	for _, s := range o.schemas {
		out = append(out, cloneSchema(s))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return semver.Compare(out[i].Version, out[j].Version) < 0
	})
	return out, nil
}

// Validate implements Ontology. Returns ErrSchemaNotRegistered when no
// schema is registered for kind, the underlying compile error wrapped with
// kernel: prefix when the schema fails to compile, or the validator's
// rich *jsonschema.ValidationError when the payload does not satisfy the
// schema. The compiled validator is cached per kind under sync.Once.
func (o *MemoryOntology) Validate(_ context.Context, kind string, payload json.RawMessage) error {
	o.mu.RLock()
	schema, ok := o.schemas[kind]
	entry := o.compiled[kind]
	o.mu.RUnlock()

	if !ok {
		return ErrSchemaNotRegistered
	}

	entry.once.Do(func() {
		var doc any
		if err := json.Unmarshal(schema.Definition, &doc); err != nil {
			entry.err = fmt.Errorf("kernel: parse schema %q: %w", kind, err)
			return
		}
		// resource is a fixed sentinel URL, not derived from kind. Each
		// compiledEntry owns its own *Compiler holding exactly one schema,
		// so the URL is purely internal scaffolding for the jsonschema
		// library. Embedding kind here would let URL-syntactic characters
		// (`#`, `?`, `/`) be reinterpreted by the compiler as fragments,
		// query strings, or path segments, breaking validation for kinds
		// that registered successfully.
		const resource = "mem://kernel/schema.json"
		c := jsonschema.NewCompiler()
		// AssertFormat upgrades the format keyword from annotation to
		// assertion for Draft 2020-12. Without it, a payload with
		// "createTime":"not-a-timestamp" would pass validation against a
		// schema declaring format=date-time, because the library follows
		// the spec's default (annotation-only) behavior. Mazarbul schemas
		// declare format=date-time as a hard contract on the wire, so the
		// compiler must enforce it; downstream consumers (event-log
		// readers, graph writer) validate raw payload JSON via this path
		// without unmarshalling into a Go struct, so the format check is
		// the only line of defence.
		c.AssertFormat()
		if err := c.AddResource(resource, doc); err != nil {
			entry.err = fmt.Errorf("kernel: add schema resource %q: %w", kind, err)
			return
		}
		compiled, err := c.Compile(resource)
		if err != nil {
			entry.err = fmt.Errorf("kernel: compile schema %q: %w", kind, err)
			return
		}
		entry.schema = compiled
	})
	if entry.err != nil {
		return entry.err
	}

	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Errorf("kernel: parse payload for %q: %w", kind, err)
	}
	if err := entry.schema.Validate(decoded); err != nil {
		return fmt.Errorf("kernel: validate %q: %w", kind, err)
	}
	return nil
}

// validateSchemaInput checks the fields a caller must provide. Provenance
// is intentionally permitted to be empty; the registry does not police it.
func validateSchemaInput(s Schema) error {
	if s.Kind == "" {
		return errors.New("kernel: register: empty Kind")
	}
	if !semver.IsValid(s.Version) {
		return fmt.Errorf("kernel: register %q: invalid semver Version %q", s.Kind, s.Version)
	}
	if len(s.Definition) == 0 {
		return fmt.Errorf("kernel: register %q: empty Definition", s.Kind)
	}
	return nil
}

// cloneSchema returns s with Definition copied into fresh backing memory,
// so callers cannot mutate the registry's internal state through the
// returned json.RawMessage.
func cloneSchema(s Schema) Schema {
	def := make(json.RawMessage, len(s.Definition))
	copy(def, s.Definition)
	s.Definition = def
	return s
}
