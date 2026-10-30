package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

const definePredicateInputSchema = `{
  "type": "object",
  "properties": {
    "predicate": {
      "type": "string",
      "pattern": "^[a-z][a-z0-9_]*$",
      "description": "Snake_case predicate identifier, e.g. works_at or member_of. Must be unique in the predicates vocabulary."
    },
    "kind": {
      "type": "string",
      "enum": ["functional", "non-functional"],
      "description": "functional = at most one live value per subject (e.g. works_at). non-functional = multiple concurrent values allowed (e.g. member_of)."
    },
    "description": {
      "type": "string",
      "minLength": 1,
      "description": "One-sentence human-readable explanation of what the predicate means."
    },
    "subjectKinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional domain constraint: entity kinds allowed as the subject of this relation, e.g. [\"person\"] for works_at. Omit to accept any kind (open-world default)."
    },
    "objectKinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional range constraint: entity kinds allowed as the object of this relation, e.g. [\"organization\"] for works_at. Omit to accept any kind."
    },
    "sameAs": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional alias IRIs mapping this predicate to external vocabularies, e.g. [\"https://schema.org/worksFor\", \"wikidata:P108\"]."
    },
    "propertySchema": {
      "type": "object",
      "description": "Optional JSON Schema (Draft 2020-12) constraining the relation's attributes map, e.g. require a string role. Validated against relation instances only when [ontology] strict_domain_range is enabled."
    }
  },
  "required": ["predicate", "kind", "description"],
  "additionalProperties": false
}`

const definePredicateOutputSchema = `{
  "type": "object",
  "properties": {
    "predicate": {
      "type": "string",
      "description": "The registered predicate identifier."
    },
    "status": {
      "type": "string",
      "enum": ["registered", "updated"],
      "description": "registered = new predicate written to the vocabulary; updated = existing predicate's kind, description, domain/range, sameAs, and propertySchema overwritten."
    }
  },
  "required": ["predicate", "status"],
  "additionalProperties": false
}`

// predicateNamePattern validates snake_case predicate names.
var predicateNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// predicateDomainKindPattern validates a subject/object (domain/range) kind:
// either a bare snake_case core kind (person) or a namespaced kind
// (user:vehicle, extension:schema_org:Person). Mirrors the entityKindRegistered
// name pattern so domain/range entries match the kinds the registry can hold.
var predicateDomainKindPattern = regexp.MustCompile(`^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$`)

// DefinePredicate registers a new predicate in the predicates vocabulary table.
//
// Before minting a predicate, check whether an existing predicate already
// expresses the relationship: call introspect → predicates first. Predicates
// are shared across all entity kinds; a duplicate predicate with a different
// description would produce ambiguous graph edges.
type DefinePredicate struct {
	appender Appender
	writer   *sql.DB
	registry *resolver.Registry
	logger   *zap.Logger
}

// NewDefinePredicate constructs a DefinePredicate tool. Writer is the SQLite
// write-pool; registry is the in-memory predicate cache; logger may be nil
// (a no-op logger is substituted).
func NewDefinePredicate(a Appender, writer *sql.DB, registry *resolver.Registry, logger *zap.Logger) *DefinePredicate {
	return &DefinePredicate{appender: a, writer: writer, registry: registry, logger: logger}
}

// Name implements mcp.Tool.
func (*DefinePredicate) Name() string { return "definePredicate" }

// Description implements mcp.Tool.
func (*DefinePredicate) Description() string {
	return "Register a new predicate in the shared predicates vocabulary — but first call introspect to check existing predicates (works_at, member_of, etc.) so you don't mint a synonym. Inputs: predicate (snake_case), kind (functional for at-most-one-live-value, non-functional for multiple concurrent values), description, and optional typing: subjectKinds/objectKinds (domain/range — the entity kinds allowed on each end), sameAs (external-vocabulary IRIs), and propertySchema (a JSON Schema for the relation's attributes). Domain/range and attribute constraints are advisory by default and enforced only when [ontology] strict_domain_range is enabled. The predicate is immediately queryable via introspect and usable in relate calls. Re-registering an existing predicate updates its kind, description, domain/range, sameAs, and propertySchema and returns status=updated. Returns E_VALIDATION if the predicate name or propertySchema is malformed."
}

// InputSchema implements mcp.Tool.
func (*DefinePredicate) InputSchema() json.RawMessage {
	return json.RawMessage(definePredicateInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DefinePredicate) OutputSchema() json.RawMessage {
	return json.RawMessage(definePredicateOutputSchema)
}

type definePredicateInput struct {
	Predicate      string          `json:"predicate"`
	Kind           string          `json:"kind"`
	Description    string          `json:"description"`
	SubjectKinds   []string        `json:"subjectKinds,omitempty"`
	ObjectKinds    []string        `json:"objectKinds,omitempty"`
	SameAs         []string        `json:"sameAs,omitempty"`
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"`
}

type definePredicateOutput struct {
	Predicate string `json:"predicate"`
	Status    string `json:"status"`
}

// Invoke implements mcp.Tool.
func (d *DefinePredicate) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in definePredicateInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if !predicateNamePattern.MatchString(in.Predicate) {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("predicate %q must be snake_case (a-z, 0-9, _), e.g. works_at", in.Predicate), nil)
	}

	var pk resolver.PredicateKind
	switch in.Kind {
	case "functional":
		pk = resolver.Functional
	case "non-functional":
		pk = resolver.NonFunctional
	default:
		return nil, mcp.NewValidationError(
			fmt.Sprintf("kind %q must be functional or non-functional", in.Kind), nil)
	}

	// Validate the optional propertySchema (P5 typed relations) before touching
	// the registry, so a malformed schema returns E_VALIDATION even on an
	// unwired tool (the validation-path tests build it with nil deps).
	if len(in.PropertySchema) > 0 {
		if err := compilePropertySchema(in.PropertySchema); err != nil {
			return nil, mcp.NewValidationError(
				fmt.Sprintf("propertySchema is not a valid JSON Schema: %v", err), nil)
		}
	}

	// Validate the shape of any domain/range kinds. ValidateRelation later
	// compares endpoint kinds by exact string, so a malformed value (wrong
	// case, stray punctuation) would register fine yet make the constraint
	// silently ineffective — or, under strict_domain_range, reject the very
	// relations it was meant to permit. We validate the namespaced snake_case
	// shape here (bare core kind, or namespaced like user:vehicle) but do not
	// require the kind to be registered yet: a predicate may legitimately be
	// defined before its domain kind exists (define-before-use).
	for _, k := range append(append([]string{}, in.SubjectKinds...), in.ObjectKinds...) {
		if !predicateDomainKindPattern.MatchString(k) {
			return nil, mcp.NewValidationError(
				fmt.Sprintf("domain/range kind %q must be a bare snake_case core kind (person) or a namespaced kind (user:vehicle)", k), nil)
		}
	}

	// Guard the write-side dependencies: the constructor accepts nil deps (the
	// validation-path tests build the tool that way), and definePredicate is only
	// wired with a live registry+writer in the memory layer. If a valid request
	// reaches it while that layer is unwired, return a structured error rather
	// than panicking on the nil registry dereference below.
	if d.registry == nil || d.writer == nil {
		return nil, mcp.NewInternalError(errors.New("predicate registry unavailable; the memory layer is not wired"))
	}

	// Probe existence BEFORE writing so we can report the correct status.
	_, existed := d.registry.Kind(in.Predicate)

	p := resolver.Predicate{
		Name:           in.Predicate,
		Kind:           pk,
		Description:    in.Description,
		AddedAt:        time.Now().UTC(),
		AddedBy:        "user",
		SubjectKinds:   in.SubjectKinds,
		ObjectKinds:    in.ObjectKinds,
		SameAs:         in.SameAs,
		PropertySchema: in.PropertySchema,
	}
	if err := resolver.WritePredicate(ctx, d.writer, d.appender, d.registry, d.logger, p, "mcp/definePredicate"); err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("write predicate: %w", err))
	}

	status := "registered"
	if existed {
		status = "updated"
	}
	out := definePredicateOutput{Predicate: in.Predicate, Status: status}
	return marshalOutput(out)
}
