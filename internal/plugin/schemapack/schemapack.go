// Package schemapack implements the schema-pack extension slot for Mazarbul.
//
// A schema-pack is a directory (or an embedded FS) containing:
//
//   - manifest.toml  — name, version, description, category, kind
//   - kinds/         — one YAML file per entity kind definition
//   - predicates.yaml — list of predicate definitions
//
// Installing a schema-pack registers every kind and predicate it declares by
// appending [ontology.EntityKindRegisteredEvent] and
// [ontology.PredicateRegisteredEvent] events to the event log with
// Provenance="extension:<name>", EntityKind AddedBy="extension", and
// Predicate AddedBy=[ontology.PredicateAddedByExtension]. The UPSERT semantics
// of the dispatcher make installs idempotent and maz-rebuild-safe.
//
// Two packs are embedded in the binary under packs/:
//
//   - schema-org-core: ~8 common schema.org classes mapped onto core kinds
//   - business-de: German business/legal types (company, shareholder,
//     managing_director) plus typed predicates (registered_with, manages_company,
//     owns_share_of)
//
// Use [ParseDir] to parse a pack from disk, [ParseFS] to parse from an
// [embed.FS], and [Installer.Install] to register the pack's vocabulary.
package schemapack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/plugin"
)

// KindSchemaPack is the extension Kind string that identifies a schema-pack.
const KindSchemaPack = "schema-pack"

// ManifestFile is the expected filename of the pack manifest inside a pack directory.
const ManifestFile = "manifest.toml"

// PredicatesFile is the expected filename of the predicates definition file inside a pack directory.
const PredicatesFile = "predicates.yaml"

// KindsDir is the expected subdirectory name containing entity-kind YAML files.
const KindsDir = "kinds"

// SeedFile is the optional JSONL file containing seed events to append
// alongside the ontology events during preset install. Each line is a
// kernel.Event JSON object (the same format that "maz export" emits).
// Missing or empty SeedFile is not an error.
const SeedFile = "seed-data.jsonl"

// ErrInvalidManifest is returned when the manifest.toml is missing required fields or has
// an invalid slot/category combination.
var ErrInvalidManifest = errors.New("schemapack: invalid manifest")

// ErrInvalidKind is returned when a kind definition file fails to parse or validate.
var ErrInvalidKind = errors.New("schemapack: invalid kind definition")

// ErrInvalidPredicate is returned when a predicate definition fails to parse or validate.
var ErrInvalidPredicate = errors.New("schemapack: invalid predicate definition")

// rePackName matches valid pack names: lowercase letter or digit, followed by
// lowercase letters, digits, hyphens, or underscores.  Mirrors the extension
// provenance pattern enforced by the ontology event schema.
var rePackName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// reKindName matches the kind-name format enforced by EntityKindRegisteredEvent:
// bare snake_case for core kinds (e.g. "person") or fully namespaced extension
// kinds where the namespace segments start lowercase but the tail may carry
// source casing (e.g. "extension:schema_org:Person", "user:deal").
// Pattern mirrors the JSON Schema constraint on the Name field.
var reKindName = regexp.MustCompile(`^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$`)

// rePredicateName matches valid predicate identifiers: snake_case, optionally
// namespaced with colons (e.g. "works_at" or "extension:biz:hired_at").
var rePredicateName = regexp.MustCompile(`^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)*$`)

// PackManifest is the parsed form of a schema-pack's manifest.toml.
type PackManifest struct {
	// Name is the pack identifier. Must be non-empty and contain only [a-z0-9_-].
	Name string `toml:"name"`

	// Version is semver-shaped, e.g. "0.1.0".
	Version string `toml:"version"`

	// Description is one sentence shown in install output.
	Description string `toml:"description"`

	// Category must be "extension" for schema packs.
	Category plugin.Category `toml:"category"`

	// Kind must be KindSchemaPack ("schema-pack").
	Kind string `toml:"kind"`

	// APIVersion is the host plugin-API version this pack was built against,
	// semver-shaped (e.g. "v1.0.0"). OPTIONAL: empty opts out of the install-time
	// compatibility gate, so packs authored before this field existed install
	// exactly as before. Validated by the install gate via
	// plugin.CheckAPICompatibility, not here, so a refusal carries the precise
	// host-vs-required message and the E_VALIDATION code rather than a parse error.
	APIVersion string `toml:"api_version"`

	// Scopes lists the read scopes this pack requests at install time. Each must
	// be a recognised kernel scope (project|personal|team). OPTIONAL: an empty
	// list requests no scopes and installs without a consent prompt. Validated +
	// consented at install time, not here.
	Scopes []string `toml:"scopes"`
}

// Validate returns ErrInvalidManifest when required fields are missing or
// Category/Kind have unexpected values, or when the name does not match the
// required pattern ^[a-z][a-z0-9_-]*$ (so that "extension:<name>" is a valid
// provenance token and dry-run + real install agree).
func (m *PackManifest) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidManifest)
	}
	if !rePackName.MatchString(m.Name) {
		return fmt.Errorf("%w: name %q must match ^[a-z][a-z0-9_-]*$ (lowercase only)", ErrInvalidManifest, m.Name)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("%w: version is required", ErrInvalidManifest)
	}
	if m.Category != plugin.CategoryExtension {
		return fmt.Errorf("%w: category must be %q, got %q", ErrInvalidManifest, plugin.CategoryExtension, m.Category)
	}
	if m.Kind != KindSchemaPack {
		return fmt.Errorf("%w: kind must be %q, got %q", ErrInvalidManifest, KindSchemaPack, m.Kind)
	}
	return nil
}

// KindDef is the in-memory representation of one YAML entity-kind definition.
type KindDef struct {
	// Name is the kind's full identifier, e.g. "extension:business_de:company".
	// Required. Must match EntityKindRegisteredEvent.Name pattern.
	Name string `yaml:"name"`

	// DisplayName is the human-readable label. Required.
	DisplayName string `yaml:"displayName"`

	// Description is a one-sentence explanation. Required.
	Description string `yaml:"description"`

	// Extends is the parent kind name for single-inheritance subtyping. Optional.
	Extends string `yaml:"extends,omitempty"`

	// SameAs lists alias/sameAs IRIs. Optional.
	SameAs []string `yaml:"sameAs,omitempty"`

	// PropertySchema is the optional JSON Schema for typed attributes.
	// In YAML it is written as a nested mapping; the loader converts it to
	// json.RawMessage via a yaml→json round-trip.
	PropertySchema any `yaml:"propertySchema,omitempty"`

	// propertySchemaRaw holds the compiled JSON Schema bytes after Parse.
	propertySchemaRaw json.RawMessage
}

// Validate returns ErrInvalidKind when required fields are absent, the name
// or extends value does not match the kind-name pattern, or subjectKinds /
// objectKinds contain invalid identifiers.  These checks mirror the rules the
// ontology event schema enforces so that dry-run and real install agree.
func (k *KindDef) Validate() error {
	if strings.TrimSpace(k.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidKind)
	}
	if !reKindName.MatchString(k.Name) {
		return fmt.Errorf("%w: name %q does not match kind-name pattern (lowercase snake_case, optionally namespaced)", ErrInvalidKind, k.Name)
	}
	if strings.TrimSpace(k.DisplayName) == "" {
		return fmt.Errorf("%w: displayName is required for kind %q", ErrInvalidKind, k.Name)
	}
	if strings.TrimSpace(k.Description) == "" {
		return fmt.Errorf("%w: description is required for kind %q", ErrInvalidKind, k.Name)
	}
	if k.Extends != "" && !reKindName.MatchString(k.Extends) {
		return fmt.Errorf("%w: extends %q does not match kind-name pattern for kind %q", ErrInvalidKind, k.Extends, k.Name)
	}
	return nil
}

// PredicateDef is the in-memory representation of one predicate from predicates.yaml.
type PredicateDef struct {
	// Predicate is the predicate identifier (snake_case). Required.
	Predicate string `yaml:"predicate"`

	// Kind is "functional" or "non-functional". Required.
	Kind string `yaml:"kind"`

	// Description is a one-sentence explanation. Required.
	Description string `yaml:"description"`

	// SubjectKinds lists valid subject kind names. Optional.
	SubjectKinds []string `yaml:"subjectKinds,omitempty"`

	// ObjectKinds lists valid object kind names. Optional.
	ObjectKinds []string `yaml:"objectKinds,omitempty"`

	// SameAs lists external vocabulary IRIs. Optional.
	SameAs []string `yaml:"sameAs,omitempty"`

	// PropertySchema is the optional JSON Schema for the relation's attributes.
	// In YAML written as a nested mapping; loaded as json.RawMessage.
	PropertySchema any `yaml:"propertySchema,omitempty"`

	// propertySchemaRaw holds the compiled JSON Schema bytes after Parse.
	propertySchemaRaw json.RawMessage
}

// Validate returns ErrInvalidPredicate when required fields are absent, the
// predicate name does not match the snake_case pattern, the kind value is not
// recognised, or subjectKinds/objectKinds contain invalid kind identifiers.
// These checks mirror the ontology event schema rules so dry-run and real
// install agree.
func (p *PredicateDef) Validate() error {
	if strings.TrimSpace(p.Predicate) == "" {
		return fmt.Errorf("%w: predicate name is required", ErrInvalidPredicate)
	}
	if !rePredicateName.MatchString(p.Predicate) {
		return fmt.Errorf("%w: predicate %q does not match predicate-name pattern (snake_case)", ErrInvalidPredicate, p.Predicate)
	}
	if p.Kind != "functional" && p.Kind != "non-functional" {
		return fmt.Errorf("%w: kind must be 'functional' or 'non-functional' for predicate %q, got %q",
			ErrInvalidPredicate, p.Predicate, p.Kind)
	}
	if strings.TrimSpace(p.Description) == "" {
		return fmt.Errorf("%w: description is required for predicate %q", ErrInvalidPredicate, p.Predicate)
	}
	for _, sk := range p.SubjectKinds {
		if !reKindName.MatchString(sk) {
			return fmt.Errorf("%w: subjectKind %q does not match kind-name pattern for predicate %q", ErrInvalidPredicate, sk, p.Predicate)
		}
	}
	for _, ok := range p.ObjectKinds {
		if !reKindName.MatchString(ok) {
			return fmt.Errorf("%w: objectKind %q does not match kind-name pattern for predicate %q", ErrInvalidPredicate, ok, p.Predicate)
		}
	}
	return nil
}

// predicatesDoc is the top-level YAML structure of predicates.yaml.
type predicatesDoc struct {
	Predicates []PredicateDef `yaml:"predicates"`
}

// Pack is a fully parsed and validated schema pack ready for installation.
type Pack struct {
	// Manifest is the parsed manifest.toml.
	Manifest PackManifest

	// Kinds holds the parsed entity-kind definitions in declaration order.
	Kinds []KindDef

	// Predicates holds the parsed predicate definitions in declaration order.
	Predicates []PredicateDef

	// Instructions is the verbatim body of instructions.md when present, the
	// empty string otherwise. A non-empty Instructions promotes this pack to a
	// preset (#210): installing it also appends a presetInstalled event so the
	// body steers agents via the getContext brief. The body is byte-capped at
	// parse time (see MaxInstructionsBytes) — never silently truncated.
	Instructions string

	// SeedEvents holds the parsed contents of seed-data.jsonl when present.
	// Each entry is a kernel.Event that will be appended alongside the
	// ontology and presetInstalled events during install, making the seed
	// atomic with the schema registration. Empty when the file is absent.
	SeedEvents []kernel.Event
}

// IsPreset reports whether this pack carries an instructions.md body and is
// therefore a preset: a schema-pack bundled with an agent-steering module.
func (p *Pack) IsPreset() bool { return p.Instructions != "" }

// ParseFS parses a schema pack from fsys rooted at root. The root directory
// must contain manifest.toml; kinds/ and predicates.yaml are optional.
// Any propertySchema fields are converted from YAML to JSON and compiled;
// an invalid schema is rejected immediately (fail-fast at install time).
func ParseFS(fsys fs.FS, root string) (*Pack, error) {
	// ---- manifest ----
	manifestData, err := fs.ReadFile(fsys, path.Join(root, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("schemapack: read manifest: %w", err)
	}
	var m PackManifest
	if _, err := toml.Decode(string(manifestData), &m); err != nil {
		return nil, fmt.Errorf("schemapack: parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}

	pack := &Pack{Manifest: m}

	kinds, err := parseKindDefs(fsys, path.Join(root, KindsDir))
	if err != nil {
		return nil, err
	}
	pack.Kinds = kinds

	predicates, err := parsePredicateDefs(fsys, path.Join(root, PredicatesFile))
	if err != nil {
		return nil, err
	}
	pack.Predicates = predicates

	// instructions.md is optional; its presence promotes the pack to a preset.
	// The byte cap is enforced here (parse time) so an over-cap body is rejected
	// loudly before any install, never silently dropped from the brief later.
	instructions, err := parseInstructions(fsys, path.Join(root, InstructionsFile))
	if err != nil {
		return nil, err
	}
	pack.Instructions = instructions

	// seed-data.jsonl is optional: it pre-seeds the knowledge graph with a
	// curated life-graph during preset install, making the seed atomic with the
	// ontology registration. Missing file is not an error.
	seedEvents, err := parseSeedData(fsys, path.Join(root, SeedFile))
	if err != nil {
		return nil, err
	}
	pack.SeedEvents = seedEvents

	return pack, nil
}

// parseKindDefs reads and validates every YAML file in kindsPath. It returns
// ErrInvalidKind when any definition is malformed or propertySchema is
// uncompilable.
func parseKindDefs(fsys fs.FS, kindsPath string) ([]KindDef, error) {
	entries, err := fs.ReadDir(fsys, kindsPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("schemapack: read kinds dir: %w", err)
	}
	kinds := make([]KindDef, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, rerr := fs.ReadFile(fsys, path.Join(kindsPath, name))
		if rerr != nil {
			return nil, fmt.Errorf("schemapack: read kind file %q: %w", name, rerr)
		}
		var kd KindDef
		if yerr := yaml.Unmarshal(data, &kd); yerr != nil {
			return nil, fmt.Errorf("schemapack: parse kind file %q: %w", name, yerr)
		}
		if verr := kd.Validate(); verr != nil {
			return nil, verr
		}
		if kd.PropertySchema != nil {
			raw, cerr := marshalAndCompileSchema(kd.PropertySchema)
			if cerr != nil {
				return nil, fmt.Errorf("schemapack: kind %q: invalid propertySchema: %w", kd.Name, cerr)
			}
			kd.propertySchemaRaw = raw
		}
		kinds = append(kinds, kd)
	}
	return kinds, nil
}

// parsePredicateDefs reads and validates predicates.yaml. It returns
// ErrInvalidPredicate when any definition is malformed or propertySchema is
// uncompilable. Returns nil when the file does not exist.
func parsePredicateDefs(fsys fs.FS, predPath string) ([]PredicateDef, error) {
	predData, err := fs.ReadFile(fsys, predPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("schemapack: read predicates.yaml: %w", err)
	}
	if predData == nil {
		return nil, nil
	}
	var doc predicatesDoc
	if yerr := yaml.Unmarshal(predData, &doc); yerr != nil {
		return nil, fmt.Errorf("schemapack: parse predicates.yaml: %w", yerr)
	}
	for i := range doc.Predicates {
		pd := &doc.Predicates[i]
		if verr := pd.Validate(); verr != nil {
			return nil, verr
		}
		if pd.PropertySchema != nil {
			raw, cerr := marshalAndCompileSchema(pd.PropertySchema)
			if cerr != nil {
				return nil, fmt.Errorf("schemapack: predicate %q: invalid propertySchema: %w", pd.Predicate, cerr)
			}
			pd.propertySchemaRaw = raw
		}
	}
	return doc.Predicates, nil
}

// parseSeedData reads and parses the optional seed-data.jsonl file. Each line
// must be a valid kernel.Event JSON object (same format as "maz export"). Lines
// that are empty or cannot be decoded are rejected with an explicit error so a
// malformed seed file fails fast at parse time, not silently at install time.
// Returns nil when the file does not exist.
func parseSeedData(fsys fs.FS, seedPath string) ([]kernel.Event, error) {
	data, err := fs.ReadFile(fsys, seedPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("schemapack: read seed-data.jsonl: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var events []kernel.Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev kernel.Event
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			return nil, fmt.Errorf("schemapack: seed-data.jsonl line %d: %w", lineNum, jerr)
		}
		if ev.Kind == "" {
			return nil, fmt.Errorf("schemapack: seed-data.jsonl line %d: missing kind", lineNum)
		}
		if len(ev.Payload) == 0 {
			return nil, fmt.Errorf("schemapack: seed-data.jsonl line %d: missing payload", lineNum)
		}
		events = append(events, ev)
	}
	if serr := sc.Err(); serr != nil {
		return nil, fmt.Errorf("schemapack: read seed-data.jsonl: %w", serr)
	}
	return events, nil
}

// ParseDir parses a schema pack from a directory on disk. It wraps ParseFS
// with os.DirFS.
func ParseDir(dir string) (*Pack, error) {
	// Use fs.Sub so paths are relative to the dir root.
	parent, base := path.Split(path.Clean(dir))
	if parent == "" {
		parent = "."
	}
	dirFS := mustOSDirFS(parent)
	return ParseFS(dirFS, base)
}

// marshalAndCompileSchema converts a YAML-decoded value (map[string]any) to
// JSON bytes and validates it using the santhosh-tekuri compiler.
func marshalAndCompileSchema(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal to JSON: %w", err)
	}
	if err := compileJSONSchema(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Installer drives the install pipeline for one pack. Create one per
// pack; it is not safe for concurrent use.
type Installer struct {
	// Log is the kernel event log to append events to.
	Log kernel.EventLog
}

// InstallResult summarises what the installer registered.
type InstallResult struct {
	// PackName is the name field from the manifest.
	PackName string

	// KindsRegistered lists the kind names that were appended as events.
	KindsRegistered []string

	// PredicatesRegistered lists the predicate names that were appended.
	PredicatesRegistered []string

	// DryRun is true when no events were actually appended.
	DryRun bool
}

// BuildEvents builds the entityKindRegistered and predicateRegistered events
// for every definition in pack WITHOUT appending them, and returns them
// alongside an InstallResult summarising what would be registered. It exists so
// a caller (the preset installer) can append the seeding events in ONE batch
// together with a presetInstalled event, making the seed-and-mark atomic.
//
// Provenance is set to "extension:<pack.Manifest.Name>"; AddedBy is set to
// "extension". Now stamps AddedAt on every event so the caller controls the
// install instant. Install is implemented in terms of BuildEvents, so the
// `maz install` path is byte-identical.
func (inst *Installer) BuildEvents(pack *Pack, now time.Time) ([]kernel.Event, *InstallResult, error) {
	provenance := "extension:" + pack.Manifest.Name

	result := &InstallResult{PackName: pack.Manifest.Name}
	events := make([]kernel.Event, 0, len(pack.Kinds)+len(pack.Predicates))

	// ---- entity kinds ----
	for i := range pack.Kinds {
		kd := &pack.Kinds[i]
		ev := &ontology.EntityKindRegisteredEvent{
			Name:           kd.Name,
			DisplayName:    kd.DisplayName,
			Description:    kd.Description,
			Extends:        kd.Extends,
			SameAs:         kd.SameAs,
			PropertySchema: kd.propertySchemaRaw,
			Provenance:     provenance,
			AddedBy:        ontology.EntityKindAddedByExtension,
			AddedAt:        now,
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return nil, nil, fmt.Errorf("schemapack: marshal kind %q: %w", kd.Name, err)
		}
		events = append(events, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
			Source:     "schemapack:" + pack.Manifest.Name,
			Confidence: 1.0,
			Payload:    payload,
		})
		result.KindsRegistered = append(result.KindsRegistered, kd.Name)
	}

	// ---- predicates ----
	for i := range pack.Predicates {
		pd := &pack.Predicates[i]
		pkind := ontology.PredicateKindNonFunctional
		if pd.Kind == "functional" {
			pkind = ontology.PredicateKindFunctional
		}
		ev := &ontology.PredicateRegisteredEvent{
			Predicate:      pd.Predicate,
			Kind:           pkind,
			Description:    pd.Description,
			AddedBy:        ontology.PredicateAddedByExtension,
			AddedAt:        now,
			SubjectKinds:   pd.SubjectKinds,
			ObjectKinds:    pd.ObjectKinds,
			SameAs:         pd.SameAs,
			PropertySchema: pd.propertySchemaRaw,
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return nil, nil, fmt.Errorf("schemapack: marshal predicate %q: %w", pd.Predicate, err)
		}
		events = append(events, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindPredicateRegistered),
			Source:     "schemapack:" + pack.Manifest.Name,
			Confidence: 1.0,
			Payload:    payload,
		})
		result.PredicatesRegistered = append(result.PredicatesRegistered, pd.Predicate)
	}

	return events, result, nil
}

// Install appends entityKindRegistered and predicateRegistered events for every
// definition in pack. When dryRun is true the events are built and validated but
// not appended — the result still lists what would be registered.
//
// Provenance is set to "extension:<pack.Manifest.Name>"; AddedBy is set to
// "extension". The UPSERT path in the dispatcher makes this idempotent; calling
// Install twice for the same pack is safe.
func (inst *Installer) Install(ctx context.Context, pack *Pack, dryRun bool) (*InstallResult, error) {
	events, result, err := inst.BuildEvents(pack, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	result.DryRun = dryRun

	if dryRun || len(events) == 0 {
		return result, nil
	}

	if _, err := inst.Log.Append(ctx, events); err != nil {
		return nil, fmt.Errorf("schemapack: append events: %w", err)
	}
	return result, nil
}

// Extension implements the [plugin.Extension] interface for a loaded and
// installed schema pack. It is a thin wrapper around [Pack] that satisfies
// the plugin contract so schema packs can be registered with the plugin host.
type Extension struct {
	pack     *Pack
	manifest plugin.Manifest
}

// NewExtension wraps a parsed pack in the plugin.Extension interface.
func NewExtension(p *Pack) *Extension {
	return &Extension{
		pack: p,
		manifest: plugin.Manifest{
			Name:        p.Manifest.Name,
			Version:     p.Manifest.Version,
			Description: p.Manifest.Description,
			Category:    plugin.CategoryExtension,
			APIVersion:  p.Manifest.APIVersion,
			Scopes:      p.Manifest.Scopes,
		},
	}
}

// Manifest implements [plugin.Plugin].
func (e *Extension) Manifest() plugin.Manifest { return e.manifest }

// Init implements [plugin.Plugin]. Schema packs have no runtime state; Init
// is a no-op.
func (e *Extension) Init(_ context.Context, _ plugin.Host) error { return nil }

// Shutdown implements [plugin.Plugin]. Schema packs have no runtime state;
// Shutdown is a no-op.
func (e *Extension) Shutdown(_ context.Context) error { return nil }

// Kind implements [plugin.Extension]. Returns KindSchemaPack ("schema-pack").
func (e *Extension) Kind() string { return KindSchemaPack }
