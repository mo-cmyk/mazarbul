package prompts

import (
	"context"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindRendered is the event kind emitted to the kernel event log each time
// a template is successfully rendered. It lands in the JSONL event log only;
// the dispatcher does not route it to any derived store (SQLite, graph, vec,
// FTS). This is an audit-only event kind.
const KindRendered = kernel.EventKind("prompt.rendered")

// VersionRendered is the semver-shaped schema version for RenderedPayload.
const VersionRendered = "v0.1.0"

// RenderedPayload is the event payload appended to the event log on each
// successful Render call. It deliberately omits the rendered text and the
// raw variable values to protect privacy; only the SHA-256 hash of the
// canonical-JSON-serialised input vars and the byte length of the output
// are recorded.
type RenderedPayload struct {
	// TemplateName is the name of the template that was rendered,
	// e.g. "recall.query".
	TemplateName string `json:"templateName"`

	// TemplateVersion is the version of the template that was rendered.
	TemplateVersion Version `json:"templateVersion"`

	// InputHash is the hex-encoded SHA-256 of the canonical JSON
	// serialisation of the RenderVars map (keys sorted lexicographically).
	// Never contains raw variable values.
	InputHash string `json:"inputHash"`

	// OutputLen is the byte length of the rendered output string. Never
	// contains the rendered text itself.
	OutputLen int `json:"outputLen"`
}

// renderedSchema is the inline JSON Schema for RenderedPayload. Registered
// via RegisterKind and used by the ontology for validation.
const renderedSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "RenderedPayload",
  "description": "Audit payload for a prompt.rendered event. Contains no rendered text or raw variable values.",
  "type": "object",
  "required": ["templateName", "templateVersion", "inputHash", "outputLen"],
  "additionalProperties": false,
  "properties": {
    "templateName": {
      "type": "string",
      "minLength": 1,
      "description": "Name of the template that was rendered."
    },
    "templateVersion": {
      "type": "integer",
      "minimum": 1,
      "description": "Monotonic version of the template that was rendered."
    },
    "inputHash": {
      "type": "string",
      "pattern": "^[0-9a-f]{64}$",
      "description": "Hex-encoded SHA-256 of the canonical JSON-serialised RenderVars. Keys sorted lexicographically."
    },
    "outputLen": {
      "type": "integer",
      "minimum": 0,
      "description": "Byte length of the rendered output string."
    }
  }
}`

// RegisterKind installs the prompt.rendered JSON Schema with the kernel
// Ontology. Call once during process startup (or once per test). Idempotent
// when the same (Kind, Version, Definition) triple is presented; returns
// kernel.ErrSchemaConflict on a version conflict.
//
// Explicit registration (rather than auto-registration inside New) keeps
// the coupling observable and testable: callers opt in to ontology
// registration and can verify it without constructing a full MemoryRegistry.
func RegisterKind(ctx context.Context, ont kernel.Ontology) error {
	return ont.Register(ctx, kernel.Schema{
		Kind:       string(KindRendered),
		Version:    VersionRendered,
		Definition: []byte(renderedSchema),
		Provenance: "core",
	})
}
