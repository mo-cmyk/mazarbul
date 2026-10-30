package mcp

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaValidator runs a tool's InputSchema against incoming arguments.
// The schema is compiled once at registration time; Validate is safe for
// concurrent use across many invocations.
type schemaValidator struct {
	schema *jsonschema.Schema
}

// compileSchema parses raw as a JSON Schema document and returns a
// validator ready to run against arguments. The name argument is used
// only in error messages to identify the offending tool. An empty raw
// schema yields a passthrough validator (Validate always returns nil),
// which lets tools that declare no schema stay functional without
// forcing every Tool to ship a non-empty body.
func compileSchema(name string, raw json.RawMessage) (*schemaValidator, error) {
	if len(raw) == 0 {
		return &schemaValidator{}, nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("mcp: parse input schema for %q: %w", name, err)
	}
	// resource is a fixed sentinel URL, not derived from name. Each
	// schemaValidator owns its own *Compiler holding exactly one
	// schema, so the URL is purely internal scaffolding for the
	// jsonschema library. Embedding name here would let URL-syntactic
	// characters (`#`, `?`, `/`) be reinterpreted as fragments, query
	// strings, or path segments, breaking validation for any tool with
	// such characters in its name. Mirrors internal/kernel ontology.
	const resource = "mem://mcp/input-schema.json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("mcp: add input schema for %q: %w", name, err)
	}
	compiled, err := c.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("mcp: compile input schema for %q: %w", name, err)
	}
	return &schemaValidator{schema: compiled}, nil
}

// Validate parses args as JSON and runs the compiled schema against it.
// Returns nil on success or a *ToolError with CodeValidation on schema
// failure or parse failure. A nil receiver or nil compiled schema
// returns nil so callers can skip validation transparently.
func (v *schemaValidator) Validate(args json.RawMessage) *ToolError {
	if v == nil || v.schema == nil {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(args, &decoded); err != nil {
		return NewValidationError(
			fmt.Sprintf("arguments are not valid JSON: %v", err),
			nil,
		)
	}
	if err := v.schema.Validate(decoded); err != nil {
		return NewValidationError(err.Error(), validationDetails(err))
	}
	return nil
}

// validationDetails renders a *jsonschema.ValidationError into a JSON
// blob carrying the structured failure tree. Returns nil when err is
// not a jsonschema validation error or when marshaling fails; callers
// treat empty Details as "no structured metadata available".
func validationDetails(err error) json.RawMessage {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	out := ve.BasicOutput()
	if out == nil {
		return nil
	}
	b, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		return nil
	}
	return b
}
