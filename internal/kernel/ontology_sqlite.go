package kernel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/mod/semver"
)

// ErrEntityKindNotRegistered is returned by SQLiteOntology.ValidateEntityKind
// when the queried kind has no row in the entity_kinds projection table.
// Defined as a sentinel so call sites can branch on errors.Is without
// coupling to the error string. The write-path membership check (P2) will
// call this; it is defined here so P2 can import it without a circular
// dependency.
var ErrEntityKindNotRegistered = errors.New("kernel: entity kind not registered")

// ErrPropertyValidation is returned by SQLiteOntology.ValidateEntityProperties
// when the supplied properties JSON fails to validate against the kind's
// composed allOf property schema. Callers can branch on errors.Is; the
// wrapping message names the kind and the first constraint violation.
var ErrPropertyValidation = errors.New("kernel: entity properties validation failed")

// ErrRelationValidation is returned by SQLiteOntology.ValidateRelation when a
// relation instance violates its predicate's declared domain/range (the
// subject or object entity kind is outside subjectKinds/objectKinds) or its
// Attributes fail the predicate's propertySchema (P5 typed relations). Callers
// branch on errors.Is; the wrapping message names the predicate and the
// constraint that failed.
var ErrRelationValidation = errors.New("kernel: relation domain/range validation failed")

// maxExtendsDepth is the maximum number of parent hops
// ValidateEntityProperties will follow before treating the chain as a
// cycle or an unreasonably deep hierarchy. Keeps a misconfigured
// registry from spinning indefinitely.
const maxExtendsDepth = 16

// EntityKindDef is the full projection of one row in the entity_kinds table.
// Returned by SQLiteOntology.EntityKindDef for the describeType MCP tool
// (wired in P2).
type EntityKindDef struct {
	// Name is the kind identifier, e.g. "person" or "user:vehicle".
	Name string `json:"name"`
	// DisplayName is the human-readable label.
	DisplayName string `json:"displayName"`
	// Description is a one-sentence explanation of the kind.
	Description string `json:"description"`
	// Parent is the parent kind for the subtype hierarchy (RDFS subClassOf).
	// Empty for root kinds.
	Parent string `json:"parent,omitempty"`
	// SameAs lists alias/sameAs IRIs to canonical or external vocabularies.
	SameAs []string `json:"sameAs,omitempty"`
	// PropertySchema is the optional JSON Schema for the kind's typed
	// attributes. Nil when absent.
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"`
	// Provenance is the namespace lane: "core", "user", or "extension:<name>".
	Provenance string `json:"provenance"`
	// AddedBy identifies the agent that registered the kind.
	AddedBy string `json:"addedBy"`
	// AddedAt is when the kind was registered, in UTC.
	AddedAt time.Time `json:"addedAt"`
	// Status is the governance status of the kind. Empty and "active" are
	// equivalent. "proposed" means the kind is pending review in governed
	// mode; "rejected" means it was rejected. Only active kinds are included
	// in the membership/registration view by default.
	Status string `json:"status,omitempty"`
}

// EntityKindRow is an alias for EntityKindDef used by the proposal-listing
// surface. Both types represent one row from entity_kinds.
type EntityKindRow = EntityKindDef

// predicateDef is the internal read-projection of one predicates-table row's
// P5 domain/range columns, used by ValidateRelation; the found return is false
// when the predicate has no row yet (open-world: validation passes).
type predicateDef struct {
	// subjectKinds lists entity-kind names valid as subjects (domain). Nil
	// means any kind is accepted.
	subjectKinds []string
	// objectKinds lists entity-kind names valid as objects (range). Nil
	// means any kind is accepted.
	objectKinds []string
	// propertySchema is the optional JSON Schema for the relation's Attributes
	// map. Nil means the bag is untyped.
	propertySchema json.RawMessage
}

// SQLiteOntology implements Ontology with a two-layer read strategy:
//
//  1. An in-process MemoryOntology overlay holds the core schemas registered
//     at boot via Register calls (Episode, Entity, Mention, Relation, …).
//  2. On Schema / Validate / List miss in the overlay, SQLiteOntology queries
//     the ontology_schemas projection table built by the dispatcher. Dynamic
//     schemas are compiled on first read and cached (cache key = schema_kind +
//     version, so a version bump invalidates the old entry).
//
// Register delegates to the in-memory overlay; it is only called at boot time
// for core schemas. Dynamic schemas flow through the event log (schemaRegistered
// events → dispatcher UPSERT → SQLiteOntology read-through).
//
// ValidateEntityKind and EntityKindDef query the entity_kinds projection table.
// The write-path membership check that calls ValidateEntityKind lands in P2;
// the method is defined here so P2 can use it without a code change.
//
// ValidateEntityProperties composes a kind's propertySchema with all ancestor
// schemas via JSON Schema allOf (P3 typed properties) and validates the
// supplied properties JSON. The composed validator is cached keyed by
// (kind, schemaHash) so a kind update invalidates the old entry automatically.
//
// Safe for concurrent use.
type SQLiteOntology struct {
	core   *MemoryOntology
	reader *sql.DB

	// dynMu guards the dynamic compiled-validator cache.
	dynMu       sync.RWMutex
	dynCompiled map[dynamicKey]*compiledEntry

	// kindMu guards the entity-kind read-through cache. Only successful
	// lookups are cached: a miss is never stored, so a kind registered after a
	// failed lookup (e.g. an event applied or pulled later) is picked up on the
	// next query rather than being negatively cached for the process lifetime.
	kindMu    sync.RWMutex
	kindCache map[string]*EntityKindDef

	// propMu guards the composed property-validator cache.
	propMu       sync.RWMutex
	propCompiled map[propKey]*compiledEntry

	// relMu guards the compiled relation-attribute-validator cache (P5 typed
	// relations). Keyed by (predicate, schemaHash) so a predicate re-registered
	// with a different propertySchema invalidates the old entry automatically.
	relMu       sync.RWMutex
	relCompiled map[propKey]*compiledEntry
}

// propKey is the cache key for a composed property validator.
// SchemaHash is the SHA-256 hex of the assembled allOf JSON, so any
// change to the kind or any ancestor invalidates the old entry.
type propKey struct {
	kind       string
	schemaHash string
}

// dynamicKey is the cache key for a dynamic schema entry. Version is included
// so a version bump invalidates the cached validator.
type dynamicKey struct {
	kind    string
	version string
}

// NewSQLiteOntology constructs a SQLiteOntology backed by reader. Reader must
// be non-nil and must point at the SQLite projection's read-only pool (the
// same pool the Navigator uses). Core schemas are registered via Register
// after construction.
func NewSQLiteOntology(reader *sql.DB) (*SQLiteOntology, error) {
	if reader == nil {
		return nil, errors.New("kernel: NewSQLiteOntology requires a non-nil reader")
	}
	return &SQLiteOntology{
		core:         NewMemoryOntology(),
		reader:       reader,
		dynCompiled:  make(map[dynamicKey]*compiledEntry),
		kindCache:    make(map[string]*EntityKindDef),
		propCompiled: make(map[propKey]*compiledEntry),
		relCompiled:  make(map[propKey]*compiledEntry),
	}, nil
}

// NewSQLiteOntologyUnattached constructs a SQLiteOntology without a reader
// pool. This is the late-binding constructor for callers (the daemon, init
// wizard) that need to build the ontology before the SQLite pool is open.
// Core schemas are registered via Register in the normal way. Dynamic
// schema and entity-kind lookups are silently skipped until AttachStore is
// called with the live pool. Specifically, ValidateEntityKind returns nil
// (no-op, not ErrEntityKindNotRegistered) while the store is detached so
// the write-path membership check does not fire false positives at boot.
func NewSQLiteOntologyUnattached() *SQLiteOntology {
	return &SQLiteOntology{
		core:         NewMemoryOntology(),
		dynCompiled:  make(map[dynamicKey]*compiledEntry),
		kindCache:    make(map[string]*EntityKindDef),
		propCompiled: make(map[propKey]*compiledEntry),
		relCompiled:  make(map[propKey]*compiledEntry),
	}
}

// AttachStore installs the read-only SQLite pool. After this call dynamic
// schema lookups and entity-kind membership checks become live. It is safe
// to call AttachStore exactly once after construction via
// NewSQLiteOntologyUnattached; a second call replaces the previous reader
// (the caller is responsible for not doing that after serving has started).
func (o *SQLiteOntology) AttachStore(reader *sql.DB) error {
	if reader == nil {
		return errors.New("kernel: AttachStore requires a non-nil reader")
	}
	o.kindMu.Lock()
	o.dynMu.Lock()
	o.propMu.Lock()
	o.reader = reader
	// Invalidate caches populated before the store was attached (unlikely
	// but defensive: a Register call during boot may have touched kindCache).
	o.kindCache = make(map[string]*EntityKindDef)
	o.dynCompiled = make(map[dynamicKey]*compiledEntry)
	o.propCompiled = make(map[propKey]*compiledEntry)
	o.propMu.Unlock()
	o.dynMu.Unlock()
	o.kindMu.Unlock()
	return nil
}

// Register implements Ontology. Delegates to the in-memory overlay. Use only
// for core boot-time registrations; dynamic schemas flow through events.
func (o *SQLiteOntology) Register(ctx context.Context, schema Schema) error {
	return o.core.Register(ctx, schema)
}

// Schema implements Ontology. Checks the in-memory overlay first; on
// ErrNotFound falls through to the ontology_schemas table.
func (o *SQLiteOntology) Schema(ctx context.Context, kind string) (Schema, error) {
	s, err := o.core.Schema(ctx, kind)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Schema{}, err
	}
	return o.dynamicSchema(ctx, kind)
}

// List implements Ontology. Returns the union of the in-memory overlay and the
// ontology_schemas table, deterministically sorted by (Kind, Version).
func (o *SQLiteOntology) List(ctx context.Context) ([]Schema, error) {
	core, err := o.core.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("kernel: list core schemas: %w", err)
	}

	dynamic, err := o.listDynamic(ctx)
	if err != nil {
		return nil, fmt.Errorf("kernel: list dynamic schemas: %w", err)
	}

	// Build a union map keyed by kind; dynamic rows that duplicate a core
	// registration are silently dropped (core wins).
	byKind := make(map[string]Schema, len(core)+len(dynamic))
	for _, s := range dynamic {
		byKind[s.Kind] = s
	}
	for _, s := range core {
		byKind[s.Kind] = s // core overwrites dynamic
	}

	out := make([]Schema, 0, len(byKind))
	for _, s := range byKind {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return semver.Compare(out[i].Version, out[j].Version) < 0
	})
	return out, nil
}

// Validate implements Ontology. Checks the in-memory overlay first; on
// ErrSchemaNotRegistered falls through to the ontology_schemas table.
func (o *SQLiteOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
	err := o.core.Validate(ctx, kind, payload)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrSchemaNotRegistered) {
		return err
	}
	return o.validateDynamic(ctx, kind, payload)
}

// ValidateEntityKind returns nil when kind exists in the entity_kinds
// projection table with status 'active', or ErrEntityKindNotRegistered when
// absent or when the kind is proposed/rejected. This method always queries
// SQLite to reflect the current status: an entityKindReviewed rejection
// applied after an earlier validation would otherwise be served stale from a
// process-lifetime cache. Results are NOT cached here — use EntityKindDef for
// read-heavy paths that do not need current-status guarantees.
//
// When the ontology was constructed via NewSQLiteOntologyUnattached and
// AttachStore has not yet been called, this method returns nil (no-op) so the
// write-path membership check does not fire false positives during the early
// boot window before the SQLite pool is live.
func (o *SQLiteOntology) ValidateEntityKind(ctx context.Context, kind string) error {
	o.kindMu.RLock()
	noReader := o.reader == nil
	o.kindMu.RUnlock()
	if noReader {
		return nil // store not yet attached; treat as no-op
	}

	// Always query SQLite: the caller may have applied an entityKindReviewed
	// rejection event since the last call, so a cached active entry could be
	// stale. This is a write-path check (called once per entity event), so
	// the SQLite round-trip cost is acceptable.
	def, err := o.queryEntityKindDef(ctx, kind)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrEntityKindNotRegistered
		}
		return fmt.Errorf("validate entity kind %q: %w", kind, err)
	}

	// Proposed and rejected kinds are not "active" for membership purposes.
	if def.Status == "proposed" || def.Status == "rejected" {
		return ErrEntityKindNotRegistered
	}
	return nil
}

// EntityKindDef returns the full definition row for name from the entity_kinds
// table. Returns ErrNotFound when the kind is not registered or the store is
// not yet attached. Successful reads are cached; misses are not (a kind
// registered later is picked up on the next query rather than being
// rejected until restart).
func (o *SQLiteOntology) EntityKindDef(ctx context.Context, name string) (EntityKindDef, error) {
	o.kindMu.RLock()
	noReader := o.reader == nil
	entry, hit := o.kindCache[name]
	o.kindMu.RUnlock()
	if noReader {
		return EntityKindDef{}, ErrNotFound
	}
	if hit && entry != nil {
		return *entry, nil
	}

	def, err := o.queryEntityKindDef(ctx, name)
	if err != nil {
		return EntityKindDef{}, err
	}

	o.kindMu.Lock()
	o.kindCache[name] = def
	o.kindMu.Unlock()
	return *def, nil
}

// --- private helpers ---

// dynamicSchema fetches a schema from ontology_schemas by kind, compiles and
// caches the validator, and returns the Schema. Returns ErrNotFound when no
// row exists or the store is not yet attached.
func (o *SQLiteOntology) dynamicSchema(ctx context.Context, kind string) (Schema, error) {
	o.kindMu.RLock()
	r := o.reader
	o.kindMu.RUnlock()
	if r == nil {
		return Schema{}, ErrNotFound
	}
	const q = `SELECT schema_kind, version, definition, provenance FROM ontology_schemas WHERE schema_kind = ?`
	row := r.QueryRowContext(ctx, q, kind)

	var s Schema
	var definitionText string
	if err := row.Scan(&s.Kind, &s.Version, &definitionText, &s.Provenance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Schema{}, ErrNotFound
		}
		return Schema{}, fmt.Errorf("kernel: query dynamic schema %q: %w", kind, err)
	}
	s.Definition = json.RawMessage(definitionText)
	return cloneSchema(s), nil
}

// listDynamic returns all rows from ontology_schemas as Schema values.
// Returns an empty slice when the store is not yet attached.
func (o *SQLiteOntology) listDynamic(ctx context.Context) ([]Schema, error) {
	o.kindMu.RLock()
	r := o.reader
	o.kindMu.RUnlock()
	if r == nil {
		return nil, nil
	}
	const q = `SELECT schema_kind, version, definition, provenance FROM ontology_schemas ORDER BY schema_kind, version`
	rows, err := r.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("kernel: list dynamic schemas: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Schema
	for rows.Next() {
		var s Schema
		var defText string
		if serr := rows.Scan(&s.Kind, &s.Version, &defText, &s.Provenance); serr != nil {
			return nil, fmt.Errorf("kernel: scan dynamic schema: %w", serr)
		}
		s.Definition = json.RawMessage(defText)
		out = append(out, cloneSchema(s))
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("kernel: iterate dynamic schemas: %w", rerr)
	}
	return out, nil
}

// validateDynamic fetches the schema from ontology_schemas, compiles (with
// caching), and validates payload. Returns ErrSchemaNotRegistered on miss.
func (o *SQLiteOntology) validateDynamic(ctx context.Context, kind string, payload json.RawMessage) error {
	s, err := o.dynamicSchema(ctx, kind)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrSchemaNotRegistered
		}
		return err
	}

	key := dynamicKey{kind: s.Kind, version: s.Version}
	o.dynMu.RLock()
	entry := o.dynCompiled[key]
	o.dynMu.RUnlock()

	if entry == nil {
		newEntry := &compiledEntry{}
		o.dynMu.Lock()
		// Double-check after acquiring the write lock.
		if existing := o.dynCompiled[key]; existing != nil {
			entry = existing
		} else {
			o.dynCompiled[key] = newEntry
			entry = newEntry
		}
		o.dynMu.Unlock()
	}

	entry.once.Do(func() {
		entry.schema, entry.err = compileSchemaDefinition(s.Kind, s.Definition)
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

// queryEntityKindDef runs the SELECT against entity_kinds. Returns ErrNotFound
// on sql.ErrNoRows. Thread-safe with respect to the database; the caller holds
// the kindMu or runs before populating the cache.
func (o *SQLiteOntology) queryEntityKindDef(ctx context.Context, name string) (*EntityKindDef, error) {
	// COALESCE(status,'active') normalises rows created before migration
	// 0019 (they have no status column until the migration runs) to active.
	const q = `SELECT name, display_name, description, COALESCE(parent,''),
	                  COALESCE(attr_schema,''), COALESCE(same_as,''),
	                  provenance, added_by, added_at,
	                  COALESCE(status,'active')
	           FROM entity_kinds WHERE name = ?`
	row := o.reader.QueryRowContext(ctx, q, name)

	var def EntityKindDef
	var attrSchemaText, sameAsText, addedAtText string
	if err := row.Scan(
		&def.Name, &def.DisplayName, &def.Description, &def.Parent,
		&attrSchemaText, &sameAsText,
		&def.Provenance, &def.AddedBy, &addedAtText,
		&def.Status,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("kernel: query entity_kind %q: %w", name, err)
	}

	if attrSchemaText != "" {
		def.PropertySchema = json.RawMessage(attrSchemaText)
	}
	if sameAsText != "" {
		if err := json.Unmarshal([]byte(sameAsText), &def.SameAs); err != nil {
			return nil, fmt.Errorf("kernel: unmarshal same_as for %q: %w", name, err)
		}
	}
	t, err := time.Parse(time.RFC3339Nano, addedAtText)
	if err != nil {
		// Fall back to RFC3339 without nanoseconds.
		t, err = time.Parse(time.RFC3339, addedAtText)
		if err != nil {
			return nil, fmt.Errorf("kernel: parse added_at for %q: %w", name, err)
		}
	}
	def.AddedAt = t.UTC()
	return &def, nil
}

// ProposedEntityKinds returns all entity_kinds rows whose status is 'proposed'.
// These are kinds that have been minted in governed mode and are awaiting human
// review via `maz schema review approve`/`reject`. Returns an empty slice (not
// error) when no proposals exist or the store is not yet attached.
func (o *SQLiteOntology) ProposedEntityKinds(ctx context.Context) ([]EntityKindDef, error) {
	o.kindMu.RLock()
	r := o.reader
	o.kindMu.RUnlock()
	if r == nil {
		return nil, nil // store not yet attached
	}
	const q = `SELECT name, display_name, description, COALESCE(parent,''),
	                  COALESCE(attr_schema,''), COALESCE(same_as,''),
	                  provenance, added_by, added_at,
	                  COALESCE(status,'active')
	           FROM entity_kinds
	           WHERE COALESCE(status,'active') = 'proposed'
	           ORDER BY name ASC`
	rows, err := r.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("kernel: list proposed entity_kinds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EntityKindDef
	for rows.Next() {
		var def EntityKindDef
		var attrSchemaText, sameAsText, addedAtText string
		if serr := rows.Scan(
			&def.Name, &def.DisplayName, &def.Description, &def.Parent,
			&attrSchemaText, &sameAsText,
			&def.Provenance, &def.AddedBy, &addedAtText,
			&def.Status,
		); serr != nil {
			return nil, fmt.Errorf("kernel: scan proposed entity_kind: %w", serr)
		}
		if attrSchemaText != "" {
			def.PropertySchema = json.RawMessage(attrSchemaText)
		}
		if sameAsText != "" {
			if uerr := json.Unmarshal([]byte(sameAsText), &def.SameAs); uerr != nil {
				return nil, fmt.Errorf("kernel: unmarshal same_as for proposed kind %q: %w", def.Name, uerr)
			}
		}
		t, perr := time.Parse(time.RFC3339Nano, addedAtText)
		if perr != nil {
			t, perr = time.Parse(time.RFC3339, addedAtText)
			if perr != nil {
				return nil, fmt.Errorf("kernel: parse added_at for proposed kind %q: %w", def.Name, perr)
			}
		}
		def.AddedAt = t.UTC()
		out = append(out, def)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("kernel: iterate proposed entity_kinds: %w", rerr)
	}
	return out, nil
}

// compileSchemaDefinition compiles a JSON Schema definition using the
// santhosh-tekuri compiler (same settings as MemoryOntology.Validate).
// Factored out so both MemoryOntology and SQLiteOntology share the same
// compile path; the dispatcher's pre-materialize guard (compileJSONSchema in
// dispatcher.go) is a thin wrapper over the same logic.
func compileSchemaDefinition(kind string, definition json.RawMessage) (*jsonschema.Schema, error) {
	var doc any
	if err := json.Unmarshal(definition, &doc); err != nil {
		return nil, fmt.Errorf("kernel: parse schema %q: %w", kind, err)
	}
	const resource = "mem://kernel/schema.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("kernel: add schema resource %q: %w", kind, err)
	}
	compiled, err := c.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("kernel: compile schema %q: %w", kind, err)
	}
	return compiled, nil
}

// CompileSchemaDocument reports whether definition is a well-formed,
// compilable JSON Schema (Draft 2020-12, AssertFormat on — the same compiler
// and settings the ontology validators use). It returns nil on success and a
// descriptive error otherwise. Exposed so the MCP/CLI authoring tools can
// reject an un-compilable schema with a validation error up front, instead of
// appending a registration event the dispatcher would later skip — which would
// otherwise report "registered" for a schema that never materializes.
func CompileSchemaDocument(definition json.RawMessage) error {
	_, err := compileSchemaDefinition("authoring", definition)
	return err
}

// ValidateEntityProperties validates properties against the composed allOf
// schema for kind. It walks the parent chain (via the entity_kinds.parent
// column) up to maxExtendsDepth hops, collecting each ancestor's attr_schema,
// then wraps them all in a JSON Schema allOf and compiles the result. The
// composed validator is cached keyed by (kind, sha256(allOf JSON)) so any
// change to the kind or an ancestor's schema automatically invalidates the
// cached entry.
//
// Returns nil when:
//   - The ontology store is not yet attached (boot window; no false positives).
//   - No schema is defined on kind or any ancestor (untyped kind).
//   - properties is empty.
//
// Returns ErrPropertyValidation (wrapping the first constraint violation) when
// validation fails. Returns a plain error for infrastructure failures (DB
// query, JSON compile).
//
// Cycle guard: if the parent chain visits the same kind name twice, or exceeds
// maxExtendsDepth hops, the method aborts composition with an error naming the
// offending kind. This mirrors the dispatcher's compile-before-materialize
// precedent: the dispatcher already rejects a bad propertySchema at write time
// so a cycle reaching here is a data-integrity bug, not normal operation.
func (o *SQLiteOntology) ValidateEntityProperties(ctx context.Context, kind string, properties json.RawMessage) error {
	if len(properties) == 0 {
		return nil
	}

	o.kindMu.RLock()
	noReader := o.reader == nil
	o.kindMu.RUnlock()
	if noReader {
		return nil // store not yet attached; treat as no-op
	}

	// Walk the parent chain collecting attr_schema values from leaf to root.
	schemas, err := o.collectAncestorSchemas(ctx, kind)
	if err != nil {
		return err
	}
	if len(schemas) == 0 {
		return nil // no schema defined on this kind or any ancestor
	}

	// Build the allOf JSON and derive a cache key from its hash.
	allOfJSON, err := buildAllOfJSON(schemas)
	if err != nil {
		return fmt.Errorf("kernel: build allOf for kind %q: %w", kind, err)
	}
	h := sha256.Sum256(allOfJSON)
	key := propKey{kind: kind, schemaHash: hex.EncodeToString(h[:])}

	o.propMu.RLock()
	entry := o.propCompiled[key]
	o.propMu.RUnlock()

	if entry == nil {
		newEntry := &compiledEntry{}
		o.propMu.Lock()
		if existing := o.propCompiled[key]; existing != nil {
			entry = existing
		} else {
			o.propCompiled[key] = newEntry
			entry = newEntry
		}
		o.propMu.Unlock()
	}

	entry.once.Do(func() {
		entry.schema, entry.err = compileSchemaDefinition("allOf:"+kind, allOfJSON)
	})
	if entry.err != nil {
		return fmt.Errorf("kernel: compile property schema for kind %q: %w", kind, entry.err)
	}

	var decoded any
	if err := json.Unmarshal(properties, &decoded); err != nil {
		return fmt.Errorf("kernel: parse properties for kind %q: %w", kind, err)
	}
	if err := entry.schema.Validate(decoded); err != nil {
		return fmt.Errorf("%w: kind %q: %w", ErrPropertyValidation, kind, err)
	}
	return nil
}

// collectAncestorSchemas walks the entity_kinds parent chain starting at kind,
// returning each non-empty attr_schema from leaf to root (leaf first so that
// child constraints appear before parent constraints in the allOf). Stops when
// it reaches a kind with no parent, exceeds maxExtendsDepth, or encounters a
// cycle.
func (o *SQLiteOntology) collectAncestorSchemas(ctx context.Context, startKind string) ([]json.RawMessage, error) {
	o.kindMu.RLock()
	r := o.reader
	o.kindMu.RUnlock()
	if r == nil {
		return nil, nil
	}

	var schemas []json.RawMessage
	visited := make(map[string]struct{}, maxExtendsDepth+1)
	current := startKind

	for i := range maxExtendsDepth + 2 {
		if _, seen := visited[current]; seen {
			return nil, fmt.Errorf("kernel: extends cycle detected at kind %q", current)
		}
		// Guard: if we have consumed more than maxExtendsDepth hops the
		// chain is pathologically deep; abort rather than walking forever.
		if i > maxExtendsDepth {
			return nil, fmt.Errorf("kernel: extends chain for kind %q exceeds maximum depth %d",
				startKind, maxExtendsDepth)
		}
		visited[current] = struct{}{}

		const q = `SELECT COALESCE(attr_schema,''), COALESCE(parent,'') FROM entity_kinds WHERE name = ?`
		var attrSchema, parent string
		err := r.QueryRowContext(ctx, q, current).Scan(&attrSchema, &parent)
		if errors.Is(err, sql.ErrNoRows) {
			break // kind not in registry; stop walking
		}
		if err != nil {
			return nil, fmt.Errorf("kernel: query kind %q for property schema: %w", current, err)
		}
		if attrSchema != "" {
			schemas = append(schemas, json.RawMessage(attrSchema))
		}
		if parent == "" {
			break
		}
		current = parent
	}
	return schemas, nil
}

// buildAllOfJSON wraps one or more JSON Schema objects in an allOf array.
// When only one schema is supplied it is returned as-is (no unnecessary
// wrapping). The result is always valid JSON Schema.
func buildAllOfJSON(schemas []json.RawMessage) (json.RawMessage, error) {
	if len(schemas) == 1 {
		return schemas[0], nil
	}
	type allOfDoc struct {
		AllOf []json.RawMessage `json:"allOf"`
	}
	raw, err := json.Marshal(allOfDoc{AllOf: schemas})
	if err != nil {
		return nil, fmt.Errorf("kernel: marshal allOf schema: %w", err)
	}
	return raw, nil
}

// relationSelfSentinel is the literal subject UID for relations whose subject is
// the seeded canonical self entity (see ontology.SelfSubjectSentinel). Copied
// here rather than imported to keep the kernel → ontology boundary the rest of
// this package observes; the value is fixed by the Relation JSON Schema.
const relationSelfSentinel = "self"

// ValidateRelation enforces a predicate's declared domain/range and attribute
// schema against a single relation instance (P5 typed relations). Policy:
//
//   - A predicate with no row in the predicates table, or one declaring neither
//     subjectKinds/objectKinds nor a propertySchema, passes (open-world default).
//   - subjectUid/objectUid are resolved to their entity kinds via the entities
//     projection. An endpoint whose kind cannot be resolved — not yet
//     materialized, an Episode rather than an Entity, or the "self" sentinel —
//     skips its domain/range check (eventually-consistent sync tolerance); it is
//     never a hard failure.
//   - A resolved kind outside the declared set, or attributes failing the
//     propertySchema, returns ErrRelationValidation.
//
// Store-not-attached (reader == nil) is a no-op. Infrastructure failures
// (query/compile errors) are returned wrapped but NOT as ErrRelationValidation,
// so the write path warn-accepts them rather than rejecting on a transient.
func (o *SQLiteOntology) ValidateRelation(ctx context.Context, predicate, subjectUID, objectUID string, attributes json.RawMessage) error {
	o.kindMu.RLock()
	noReader := o.reader == nil
	o.kindMu.RUnlock()
	if noReader {
		return nil
	}

	def, found, err := o.predicateDomainRange(ctx, predicate)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}

	if len(def.subjectKinds) > 0 {
		if cerr := o.checkEndpointKind(ctx, "subject", subjectUID, def.subjectKinds, predicate); cerr != nil {
			return cerr
		}
	}
	if len(def.objectKinds) > 0 {
		if cerr := o.checkEndpointKind(ctx, "object", objectUID, def.objectKinds, predicate); cerr != nil {
			return cerr
		}
	}
	if len(def.propertySchema) > 0 {
		// Validate an empty object when the relation carries no attributes, so a
		// schema with required fields or minProperties can reject an incomplete
		// relation rather than being bypassed by the len==0 short-circuit.
		attrs := attributes
		if len(attrs) == 0 {
			attrs = json.RawMessage(`{}`)
		}
		if aerr := o.validateRelationAttributes(predicate, def.propertySchema, attrs); aerr != nil {
			return aerr
		}
	}
	return nil
}

// checkEndpointKind resolves uid to its entity kind and verifies membership in
// allowed. An unresolvable endpoint (self sentinel, not-yet-materialized, or an
// Episode) skips the check; role ("subject"/"object") shapes the error message.
func (o *SQLiteOntology) checkEndpointKind(ctx context.Context, role, uid string, allowed []string, predicate string) error {
	kind, ok, err := o.KindForEntity(ctx, uid)
	if err != nil {
		return err
	}
	if !ok {
		return nil // unresolved endpoint: skip (sync tolerance)
	}
	for _, k := range allowed {
		if k == kind {
			return nil
		}
	}
	return fmt.Errorf("%w: predicate %q %s kind %q not in declared %s kinds %v",
		ErrRelationValidation, predicate, role, kind, role, allowed)
}

// KindForEntity resolves an entity UID to its kind via the entities projection.
// Returns ("", false, nil) when uid is empty, the self sentinel, or has no
// entity row (not yet materialized, or the UID names an Episode). A query
// failure is surfaced as the error value so the caller can warn-accept it.
func (o *SQLiteOntology) KindForEntity(ctx context.Context, uid string) (string, bool, error) {
	if uid == "" || uid == relationSelfSentinel {
		return "", false, nil
	}
	o.kindMu.RLock()
	r := o.reader
	o.kindMu.RUnlock()
	if r == nil {
		return "", false, nil
	}
	const q = `SELECT kind FROM entities WHERE uid = ?`
	var kind string
	err := r.QueryRowContext(ctx, q, uid).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("kernel: query entity kind for %q: %w", uid, err)
	}
	return kind, true, nil
}

// predicateDomainRange reads the P5 domain/range columns for predicate from the
// predicates table; the found return is false when no row exists yet
// (open-world). Malformed JSON in a column degrades that column to nil rather
// than failing the read, so a single corrupt row never blocks writes.
func (o *SQLiteOntology) predicateDomainRange(ctx context.Context, predicate string) (predicateDef, bool, error) {
	o.kindMu.RLock()
	r := o.reader
	o.kindMu.RUnlock()
	if r == nil {
		return predicateDef{}, false, nil
	}
	const q = `SELECT COALESCE(subject_kinds,''), COALESCE(object_kinds,''), COALESCE(property_schema,'')
	           FROM predicates WHERE predicate = ?`
	var subjRaw, objRaw, schemaRaw string
	err := r.QueryRowContext(ctx, q, predicate).Scan(&subjRaw, &objRaw, &schemaRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return predicateDef{}, false, nil
	}
	if err != nil {
		return predicateDef{}, false, fmt.Errorf("kernel: query predicate def %q: %w", predicate, err)
	}

	var def predicateDef
	if subjRaw != "" {
		if uerr := json.Unmarshal([]byte(subjRaw), &def.subjectKinds); uerr != nil {
			def.subjectKinds = nil
		}
	}
	if objRaw != "" {
		if uerr := json.Unmarshal([]byte(objRaw), &def.objectKinds); uerr != nil {
			def.objectKinds = nil
		}
	}
	if schemaRaw != "" {
		def.propertySchema = json.RawMessage(schemaRaw)
	}
	return def, true, nil
}

// validateRelationAttributes compiles (with caching) the predicate's
// propertySchema and validates the relation's attributes JSON against it.
// Returns ErrRelationValidation on a constraint violation; a malformed schema
// (compile error) is returned wrapped but NOT as ErrRelationValidation, so the
// write path warn-accepts it rather than rejecting the relation on a bad schema.
func (o *SQLiteOntology) validateRelationAttributes(predicate string, schema, attributes json.RawMessage) error {
	h := sha256.Sum256(schema)
	key := propKey{kind: predicate, schemaHash: hex.EncodeToString(h[:])}

	o.relMu.RLock()
	entry := o.relCompiled[key]
	o.relMu.RUnlock()

	if entry == nil {
		newEntry := &compiledEntry{}
		o.relMu.Lock()
		if existing := o.relCompiled[key]; existing != nil {
			entry = existing
		} else {
			o.relCompiled[key] = newEntry
			entry = newEntry
		}
		o.relMu.Unlock()
	}

	entry.once.Do(func() {
		entry.schema, entry.err = compileSchemaDefinition("relationAttributes:"+predicate, schema)
	})
	if entry.err != nil {
		return fmt.Errorf("kernel: compile relation attribute schema for %q: %w", predicate, entry.err)
	}

	var decoded any
	if err := json.Unmarshal(attributes, &decoded); err != nil {
		return fmt.Errorf("kernel: parse relation attributes for %q: %w", predicate, err)
	}
	if err := entry.schema.Validate(decoded); err != nil {
		return fmt.Errorf("%w: predicate %q attributes: %w", ErrRelationValidation, predicate, err)
	}
	return nil
}
