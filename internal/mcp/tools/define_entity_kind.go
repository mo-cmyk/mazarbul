package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

const defineEntityKindInputSchema = `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "pattern": "^[a-z][a-z0-9_]*$",
      "description": "Bare snake_case kind identifier, e.g. vehicle. No namespace prefix — user: is added automatically. Must not start with core:."
    },
    "displayName": {
      "type": "string",
      "minLength": 1,
      "description": "Human-readable label, e.g. Vehicle."
    },
    "description": {
      "type": "string",
      "minLength": 1,
      "description": "One-sentence explanation of the kind. Used by the dedup check and agents."
    },
    "extends": {
      "type": "string",
      "pattern": "^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$",
      "description": "Parent kind name for single-inheritance subtype hierarchy (RDFS subClassOf). Must be a core kind or an already-registered active kind; a bare name (vehicle) resolves to its canonical registered form (user:vehicle) when unambiguous. E_VALIDATION when unknown or ambiguous. Optional."
    },
    "sameAs": {
      "type": "array",
      "items": {"type": "string", "minLength": 1},
      "description": "Alias/sameAs IRIs to canonical external vocabularies, e.g. [\"schema.org/Vehicle\", \"wikidata:Q42889\"]. Optional."
    },
    "propertySchema": {
      "type": "object",
      "description": "Optional JSON Schema (Draft 2020-12) for typed attributes. Compiled before registration; E_VALIDATION on compile failure."
    },
    "force": {
      "type": "boolean",
      "description": "Bypass the reuse-before-mint dedup check and register unconditionally. Default false."
    },
    "preview": {
      "type": "boolean",
      "description": "When true, compute and return a plan of what this call would do (collapse, supersede, or mint) WITHOUT appending any event. The event log is unchanged. Use this to inspect impact before committing. Default false."
    }
  },
  "required": ["name", "displayName", "description"],
  "additionalProperties": false
}`

const defineEntityKindOutputSchema = `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The registered kind name, e.g. user:vehicle."
    },
    "status": {
      "type": "string",
      "enum": ["registered", "reused", "proposed", "preview"],
      "description": "registered = kind written as active; reused = existing near-identical kind returned instead of minting; proposed = kind queued for review in governed mode; preview = plan computed without writing any event (preview=true was set)."
    },
    "reusedKind": {
      "type": "string",
      "description": "The existing kind name when status=reused. Absent otherwise."
    },
    "closeMatch": {
      "type": "string",
      "description": "The name of a closely related existing kind (skos:closeMatch semantics). Present when a medium-similarity near-duplicate was found during minting."
    },
    "similarity": {
      "type": "number",
      "minimum": 0,
      "maximum": 1,
      "description": "Dedup similarity score (0..1). Present when status=reused or when closeMatch is set."
    },
    "plan": {
      "type": "object",
      "description": "Present only when preview=true. Describes what the call would do without writing any event.",
      "properties": {
        "action": {
          "type": "string",
          "enum": ["mint", "supersede", "collapse"],
          "description": "mint = new kind would be created; supersede = existing kind's propertySchema would be replaced; collapse = candidate would merge into an existing kind."
        },
        "collapsesInto": {"type": "string", "description": "Fully-qualified name of the existing kind the candidate collapses into. Set when action=collapse."},
        "supersedesFrom": {"type": "string", "description": "Description of the schema being superseded. Set when action=supersede."},
        "orphanCount": {"type": "integer", "minimum": 0, "description": "Number of entities potentially affected by removed fields. Zero for backward-compatible changes."},
        "closeMatch": {"type": "string", "description": "Nearest existing kind when action=mint and its lexical similarity crosses the close-match threshold; the candidate would collapse into this if reused (skos:closeMatch). Accompanied by a MAZ-KIND-001 warning."},
        "nearestKinds": {
          "type": "array",
          "description": "Existing kinds ranked by lexical name-similarity to the candidate, highest first. Present on action=mint so you can judge whether the candidate is a synonym before writing.",
          "items": {
            "type": "object",
            "properties": {
              "kind": {"type": "string", "description": "Fully-qualified existing kind name, e.g. user:vehicle."},
              "similarity": {"type": "number", "minimum": 0, "maximum": 1, "description": "Lexical name-similarity score (0..1) against the candidate."}
            },
            "required": ["kind", "similarity"]
          }
        },
        "checksSkipped": {
          "type": "array",
          "items": {"type": "string"},
          "description": "Dedup layers that did NOT run, e.g. [\"semantic\"] when no embedder is wired — the mint verdict is then lexical-only. Accompanied by a MAZ-KIND-002 warning."
        }
      },
      "required": ["action"]
    },
    "warnings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "code":    {"type": "string", "description": "Stable machine-readable warning code, e.g. MAZ-KIND-001."},
          "message": {"type": "string", "description": "Human-readable explanation."}
        },
        "required": ["code", "message"]
      },
      "description": "Non-fatal advisory warnings. MAZ-KIND-001 = close match found; MAZ-KIND-002 = semantic dedup skipped; MAZ-SCHEMA-001 = supersede; MAZ-SCHEMA-002 = orphaned fields; MAZ-SCHEMA-003 = collapse."
    }
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// kindNamePattern validates a bare snake_case kind name (no namespace prefix).
var kindNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// isCoreKind reports whether name is one of the seven genesis kinds that must
// not be mutated by user-facing tools.
func isCoreKind(name string) bool {
	switch ontology.EntityKind(name) {
	case ontology.EntityKindPerson,
		ontology.EntityKindOrganization,
		ontology.EntityKindPlace,
		ontology.EntityKindProject,
		ontology.EntityKindEvent,
		ontology.EntityKindDocument,
		ontology.EntityKindExternalRef:
		return true
	}
	return false
}

// DefineEntityKind mints or reuses an entity kind in the dynamic-ontology registry.
//
// Before minting a new kind, always check whether an existing kind already
// covers the concept: call introspect or describeType first. The dedup check
// embedded in this tool runs automatically (lexical + semantic) and will
// return status="reused" with the matching kind when a near-identical entry
// exists. Pass force=true only after confirming the new kind is genuinely
// distinct.
//
// In governed mode (Governed=true) new kinds are persisted with
// status="proposed" and appear in `maz schema review list` awaiting approval.
// The tool response reflects status="proposed" in that case.
type DefineEntityKind struct {
	appender Appender
	deduper  *dedup.Checker
	kinds    kindLister // may be nil → extends-existence check skipped
	governed bool
}

// NewDefineEntityKind constructs a DefineEntityKind tool. Deduper may be nil,
// in which case only the force path and event append are wired (useful in
// minimal-boot mode where the navigator is unavailable). Kinds may be nil to
// skip the extends-target existence check (minimal boot); when wired it
// resolves a bare parent reference (vehicle → user:vehicle) against the
// active registry and rejects an unknown or ambiguous parent, so a typo like
// extends:"user:vehcile" cannot persist a dangling parent that never inherits
// the intended propertySchema. Governed enables the P7 governed-mode proposal
// path.
func NewDefineEntityKind(a Appender, d *dedup.Checker, kinds kindLister, governed bool) *DefineEntityKind {
	return &DefineEntityKind{appender: a, deduper: d, kinds: kinds, governed: governed}
}

// Name implements mcp.Tool.
func (*DefineEntityKind) Name() string { return "defineEntityKind" }

// Description implements mcp.Tool.
func (*DefineEntityKind) Description() string {
	return "Register a new entity kind in the dynamic-ontology registry — but first check whether an existing kind already covers the concept (introspect → entityKinds, or describeType). The built-in dedup check runs automatically: a near-identical kind returns status=\"reused\" with the existing name so you avoid minting a synonym; a medium-similarity near-duplicate mints but records a SKOS closeMatch alias (response field closeMatch + similarity, internal URN maz:kind:<name> added to sameAs). Because that dedup runs a semantic embedding pass over existing kinds, this call routinely takes a few seconds to return — that latency is expected, not a hang; do not abort or retry on a short timeout. Inputs: name (bare snake_case, no namespace), displayName, description (required for dedup), optional extends (parent kind — must be a core kind or an already-registered active kind; bare names resolve to their canonical registered form), sameAs (external vocabulary IRIs), propertySchema (JSON Schema for typed attributes). Appends an entityKindRegistered event; the dispatcher materialises it into entity_kinds and the render pipeline writes it to the git-synced ontology/ tree. Re-registering an existing user kind with force=true overwrites it. Attempting to modify a core kind, or extending an unknown/ambiguous parent, returns E_VALIDATION."
}

// InputSchema implements mcp.Tool.
func (*DefineEntityKind) InputSchema() json.RawMessage {
	return json.RawMessage(defineEntityKindInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DefineEntityKind) OutputSchema() json.RawMessage {
	return json.RawMessage(defineEntityKindOutputSchema)
}

type defineEntityKindInput struct {
	Name           string          `json:"name"`
	DisplayName    string          `json:"displayName"`
	Description    string          `json:"description"`
	Extends        string          `json:"extends,omitempty"`
	SameAs         []string        `json:"sameAs,omitempty"`
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"`
	Force          bool            `json:"force,omitempty"`
	// Preview, when true, computes the plan without appending any event.
	Preview bool `json:"preview,omitempty"`
}

// defineEntityKindPlan carries the preview plan (only when preview=true).
type defineEntityKindPlan struct {
	// Action describes the planned operation.
	Action string `json:"action"`
	// CollapsesInto is the kind this candidate would collapse into.
	CollapsesInto string `json:"collapsesInto,omitempty"`
	// SupersedesFrom describes the schema being superseded.
	SupersedesFrom string `json:"supersedesFrom,omitempty"`
	// OrphanCount is the number of entities potentially affected by removed fields.
	OrphanCount int `json:"orphanCount,omitempty"`
	// CloseMatch is the nearest existing kind when a "mint" verdict crosses the
	// close-match threshold (skos:closeMatch semantics). Empty otherwise.
	CloseMatch string `json:"closeMatch,omitempty"`
	// NearestKinds ranks existing kinds by lexical name-similarity to the
	// candidate, highest first. Present on a "mint" verdict so a caller can see
	// what the candidate is close to before writing. Absent otherwise.
	NearestKinds []dedup.NearestKind `json:"nearestKinds,omitempty"`
	// ChecksSkipped names dedup layers that did not run, e.g. ["semantic"] when
	// no embedder is wired (the mint verdict is then lexical-only). Absent when
	// every layer ran.
	ChecksSkipped []string `json:"checksSkipped,omitempty"`
}

type defineEntityKindOutput struct {
	// Name is the registered kind name, e.g. user:vehicle.
	Name string `json:"name"`
	// Status is "registered", "reused", "proposed", or "preview".
	Status string `json:"status"`
	// ReusedKind is the existing kind name when Status is "reused".
	ReusedKind string `json:"reusedKind,omitempty"`
	// CloseMatch is the name of a closely related kind (skos:closeMatch) when
	// the dedup check found a medium-similarity near-duplicate. The minted
	// kind's SameAs includes maz:kind:<CloseMatch> as an internal alias URN.
	CloseMatch string `json:"closeMatch,omitempty"`
	// Similarity is the dedup score (0..1) against ReusedKind or CloseMatch.
	Similarity *float64 `json:"similarity,omitempty"`
	// Plan carries the preview plan when Status is "preview". Absent otherwise.
	Plan *defineEntityKindPlan `json:"plan,omitempty"`
	// Warnings is a list of non-fatal advisory warnings. Each entry has a
	// stable Code (MAZ-KIND-NNN / MAZ-SCHEMA-NNN) and a human-readable Message.
	// Absent when empty so the wire shape stays clean for callers that do not
	// inspect it.
	Warnings []warncode.Warning `json:"warnings,omitempty"`
}

// Invoke implements mcp.Tool.
func (d *DefineEntityKind) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in defineEntityKindInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}

	// Validate name pattern.
	if !kindNamePattern.MatchString(in.Name) {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("name %q must be a bare snake_case identifier (a-z, 0-9, _), e.g. vehicle", in.Name), nil)
	}

	// Reject core namespace attempts.
	if isCoreKind(in.Name) {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("kind %q is a core kind and cannot be modified; choose a different name or use defineEntityKind with a custom name", in.Name), nil)
	}

	// Validate propertySchema compiles.
	if len(in.PropertySchema) > 0 {
		if err := compilePropertySchema(in.PropertySchema); err != nil {
			return nil, mcp.NewValidationError(
				fmt.Sprintf("propertySchema does not compile: %v", err), nil)
		}
	}

	// The canonical registered name gets the user: namespace prefix.
	registeredName := "user:" + in.Name

	// Preview path: compute the plan without touching the event log.
	if in.Preview {
		return d.invokePreview(ctx, &in, registeredName)
	}

	// Dedup check first (skipped when force=true or no deduper wired): when
	// the kind resolves to an existing one, nothing is minted and the input's
	// extends never persists, so reuse must not fail on an invalid parent.
	reuseOut, closeMatchName, closeMatchSim, dedupWarnings := d.applyDedup(ctx, &in)
	if reuseOut != nil {
		return marshalOutput(*reuseOut)
	}

	// Minting: resolve and validate the extends target against the active
	// registry (mirrors the CLI's resolveExtendsDB): a bare parent reference
	// resolves to its canonical registered name, an ambiguous bare name names
	// the candidates, and an unknown parent is rejected outright — the
	// dispatcher only cycle-checks extends, so a dangling parent would
	// otherwise persist silently and never inherit the intended
	// propertySchema.
	if in.Extends != "" {
		resolved, rerr := d.resolveExtends(ctx, in.Extends, registeredName)
		if rerr != nil {
			return nil, rerr
		}
		in.Extends = resolved
	}

	// In governed mode new kinds are proposed rather than active.
	eventStatus := ""
	responseStatus := "registered"
	if d.governed {
		eventStatus = ontology.EntityKindStatusProposed
		responseStatus = "proposed"
	}

	// Build and append the EntityKindRegisteredEvent.
	payload := ontology.EntityKindRegisteredEvent{
		Name:           registeredName,
		DisplayName:    in.DisplayName,
		Description:    in.Description,
		Extends:        in.Extends,
		SameAs:         in.SameAs,
		PropertySchema: in.PropertySchema,
		Provenance:     "user",
		AddedBy:        ontology.EntityKindAddedByUser,
		AddedAt:        time.Now().UTC(),
		Status:         eventStatus,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal entityKindRegistered: %w", err))
	}
	if _, err := d.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
		Source:     "mcp/defineEntityKind",
		Confidence: 1.0,
		Payload:    raw,
	}}); err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("append entityKindRegistered: %w", err))
	}

	out := defineEntityKindOutput{
		Name:       registeredName,
		Status:     responseStatus,
		CloseMatch: closeMatchName,
		Similarity: closeMatchSim,
		Warnings:   dedupWarnings,
	}
	return marshalOutput(out)
}

// kindCountSource is an optional extension of kindLister that also provides
// per-kind entity counts. *navigate.Navigator satisfies it when wired through
// the daemon. The preview path type-asserts against it to populate
// ExistingKind.EntityCount so orphan warnings carry the real count rather than
// the misleading "up to 0 entities affected" placeholder.
type kindCountSource interface {
	kindLister
	EntityKinds(ctx context.Context) ([]navigate.KindCount, error)
}

// invokePreview computes the plan for a preview=true call without appending
// any event. It uses the kinds lister to fetch the current registry, builds
// dedup.ExistingKind snapshots, and delegates to dedup.PlanKind. When no kinds
// lister is wired (minimal boot) the plan is always "mint" — a conservative
// safe answer that never blocks a preview call.
func (d *DefineEntityKind) invokePreview(ctx context.Context, in *defineEntityKindInput, registeredName string) (json.RawMessage, error) {
	var existing []dedup.ExistingKind
	if d.kinds != nil {
		regKinds, err := d.kinds.RegisteredEntityKinds(ctx)
		if err == nil {
			// Build a count index keyed by kind name when the lister also
			// implements kindCountSource. On an error the index is empty and
			// EntityCount stays zero — the plan degrades gracefully.
			counts := make(map[string]int)
			if cs, ok := d.kinds.(kindCountSource); ok {
				if kcs, cerr := cs.EntityKinds(ctx); cerr == nil {
					for _, kc := range kcs {
						counts[kc.Kind] = kc.Count
					}
				}
			}

			existing = make([]dedup.ExistingKind, 0, len(regKinds))
			for i := range regKinds {
				k := &regKinds[i]
				existing = append(existing, dedup.ExistingKind{
					Name:           k.Name,
					Description:    k.Description,
					PropertySchema: k.PropertySchema,
					EntityCount:    counts[k.Name],
				})
			}
		}
		// On lister error degrade to empty existing set (→ always mint).
	}

	// Surface the same lexical near-duplicate scan + close-match threshold the
	// live mint uses, and declare whether a semantic pass is even possible so a
	// lexical-only verdict is flagged (MAZ-KIND-002). The deduper may be nil in
	// minimal-boot wiring; treat that as no embedder and the default threshold.
	previewReq := dedup.KindPreviewRequest{
		Name:           in.Name,
		Description:    in.Description,
		PropertySchema: in.PropertySchema,
	}
	if d.deduper != nil {
		previewReq.SemanticChecked = d.deduper.HasEmbedder()
		previewReq.CloseMatchThreshold = d.deduper.MintAliasThreshold()
	}
	pr := dedup.PlanKind(previewReq, existing)

	plan := &defineEntityKindPlan{
		Action:         pr.Action,
		CollapsesInto:  pr.CollapsesInto,
		SupersedesFrom: pr.SupersedesFrom,
		OrphanCount:    pr.OrphanCount,
		CloseMatch:     pr.CloseMatch,
		NearestKinds:   pr.NearestKinds,
		ChecksSkipped:  pr.ChecksSkipped,
	}
	out := defineEntityKindOutput{
		Name:     registeredName,
		Status:   "preview",
		Plan:     plan,
		Warnings: pr.Warnings,
	}
	return marshalOutput(out)
}

// applyDedup runs the dedup check and mutates in.SameAs when a MintWithAlias
// result is produced. Returns a non-nil reuseOut when the caller should return
// immediately with the reused-kind response. Non-empty closeMatchName signals a
// medium-similarity near-duplicate: the mint proceeds with a SKOS closeMatch.
// The warnings return carries any MAZ-KIND-NNN advisory codes from the check.
// Skipped when in.Force is true or no deduper is wired.
func (d *DefineEntityKind) applyDedup(ctx context.Context, in *defineEntityKindInput) (reuseOut *defineEntityKindOutput, closeMatchName string, closeMatchSim *float64, warnings []warncode.Warning) {
	if in.Force || d.deduper == nil {
		return nil, "", nil, nil
	}
	result, err := d.deduper.Check(ctx, in.Name, in.Description)
	if err != nil {
		return nil, "", nil, nil // degrade gracefully: allow mint on check error
	}

	var warns []warncode.Warning

	// MAZ-KIND-002: semantic layer was skipped (no embedder or timed out).
	if result.Degraded {
		warns = append(warns, warncode.New(warncode.CodeKindDedupDegraded,
			"semantic dedup pass was skipped (no embedder wired or timed out); only lexical dedup ran — a semantic synonym may have slipped through"))
	}

	switch result.Decision {
	case dedup.Reuse:
		sim := result.Similarity
		out := defineEntityKindOutput{
			Name:       result.Match,
			Status:     "reused",
			ReusedKind: result.Match,
			Similarity: &sim,
			Warnings:   warns,
		}
		return &out, "", nil, nil
	case dedup.MintWithAlias:
		// Proceed with the mint but append the internal alias URN to SameAs
		// so the closeMatch travels in the event payload. Deduplicate to
		// avoid doubling a caller-supplied URN.
		aliasURN := "maz:kind:" + result.Match
		if !containsString(in.SameAs, aliasURN) {
			in.SameAs = append(in.SameAs, aliasURN)
		}
		sim := result.Similarity
		// MAZ-KIND-001: close match found; minted with a SKOS alias.
		warns = append(warns, warncode.New(warncode.CodeKindCloseMatch,
			"near-duplicate kind %q found (similarity %.2f); minted with SKOS closeMatch alias — consider reusing the existing kind instead",
			result.Match, result.Similarity))
		return nil, result.Match, &sim, warns
	case dedup.Mint:
		// No close match; proceed with a clean mint.
	}
	return nil, "", nil, warns
}

// compilePropertySchema validates that a JSON Schema object compiles with the
// santhosh-tekuri compiler (the same compile path the dispatcher applies before
// materializing the kind). A schema that parses as an object but fails to
// compile — e.g. {"type":12} — is rejected here as E_VALIDATION rather than
// being "registered" and then silently dropped by applyEntityKindRegistered.
func compilePropertySchema(schema json.RawMessage) error {
	// A non-object payload fails the compile too, but checking it first yields a
	// clearer "not a JSON object" message than the compiler's resource error.
	var doc map[string]any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return fmt.Errorf("not a JSON object: %w", err)
	}
	return kernel.CompileSchemaDocument(schema)
}

// resolveExtends resolves the extends argument to a canonical registered kind
// name using describeType's exact/local/ambiguous model. Core kinds pass
// through unchanged. When no registry lister is wired (minimal boot) the
// argument passes through unvalidated, and a registry read error degrades to
// allow so a transient DB fault never blocks a mint. The registeredName
// argument is the child's canonical name, used to reject direct
// self-extension early with an actionable message instead of the
// dispatcher's silent cycle-strip.
func (d *DefineEntityKind) resolveExtends(ctx context.Context, extends, registeredName string) (string, error) {
	if isCoreKind(extends) {
		return extends, nil
	}
	if d.kinds == nil {
		return extends, nil
	}
	kinds, err := d.kinds.RegisteredEntityKinds(ctx)
	if err != nil {
		return extends, nil //nolint:nilerr // documented degrade path: never block a mint on a registry read error
	}
	match, ambiguous := resolveKindByName(kinds, extends)
	if match.Name == "" {
		if len(ambiguous) > 1 {
			return "", mcp.NewValidationError(
				fmt.Sprintf("extends %q is ambiguous across namespaces: %s; pass the full kind name", extends, kindNames(ambiguous)), nil)
		}
		return "", mcp.NewValidationError(
			fmt.Sprintf("extends target %q is not a registered active kind; register it with defineEntityKind first, or check introspect/describeType for the exact name", extends), nil)
	}
	if match.Name == registeredName {
		return "", mcp.NewValidationError(
			fmt.Sprintf("kind %q cannot extend itself", registeredName), nil)
	}
	return match.Name, nil
}

// containsString reports whether s appears in slice.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// marshalOutput marshals v and wraps any error as E_INTERNAL.
func marshalOutput(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return b, nil
}
