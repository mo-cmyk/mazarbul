package prompts

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Version is a monotonic, caller-assigned template version. Non-zero values
// only; the zero value is rejected by Register.
type Version uint32

// Template is the unit of registration in the Registry. The Body is a
// Go text/template string; Render executes it against RenderVars.
//
// Field naming follows Google AIP-142: createTime (not createdAt).
type Template struct {
	// Name identifies the template, e.g. "recall.query". Required.
	Name string `json:"name"`

	// Version is the caller-assigned monotonic version for this template.
	// Non-zero. A second Register call with the same (Name, Version) but
	// a different Body returns ErrVersionConflict; identical Body is
	// idempotent.
	Version Version `json:"version"`

	// Body is a Go text/template string. Compiled at Register time so
	// Render never re-parses on the hot path.
	Body string `json:"body"`

	// Description is an AIP-192 human-readable description of the template's
	// purpose and expected variables.
	Description string `json:"description"`

	// CreateTime is the instant the template was registered, in UTC.
	// Output-only; set by the Registry.
	CreateTime time.Time `json:"createTime"`
}

// RenderVars is the map of variable name to string value passed to Render.
// Keys must be valid Go text/template identifiers. Values are never stored
// in the audit event; only a SHA-256 hash of the canonical JSON serialisation
// is recorded.
type RenderVars map[string]string

// Registry is the versioned prompt-template registry. Implementations must
// be safe for concurrent use.
type Registry interface {
	// Register stores t in the registry. Same (Name, Version, Body) triple
	// is idempotent. Same (Name, Version) with a different Body returns
	// ErrVersionConflict. Version zero returns an error.
	Register(ctx context.Context, t Template) error

	// Get returns the template identified by (name, version), or ErrNotFound
	// if no matching template exists.
	Get(ctx context.Context, name string, version Version) (Template, error)

	// Render executes the template identified by (name, version) with vars,
	// returning the rendered string. Returns ErrNotFound (wrapped in
	// RenderError) if the template does not exist, or a RenderError wrapping
	// the template execution failure.
	Render(ctx context.Context, name string, version Version, vars RenderVars) (string, error)

	// List returns every registered template in undefined order. Callers
	// must not rely on iteration order across calls.
	List(ctx context.Context) ([]Template, error)
}

// ErrVersionConflict is returned by Register when a (Name, Version) pair
// already exists with a different Body.
var ErrVersionConflict = errors.New("prompts: version conflict")

// ErrNotFound is returned by Get and Render when no template matches the
// requested (name, version) pair.
var ErrNotFound = errors.New("prompts: not found")

// RenderError wraps a failure that occurred while executing a template.
// Unwrap returns the underlying cause so callers can use errors.Is / errors.As.
type RenderError struct {
	// Name is the template name that failed to render.
	Name string

	// Version is the template version that failed to render.
	Version Version

	// Cause is the underlying execution error.
	Cause error
}

// Error implements the error interface.
func (e *RenderError) Error() string {
	return fmt.Sprintf("prompts: render %s@%d: %v", e.Name, e.Version, e.Cause)
}

// Unwrap exposes Cause for errors.Is / errors.As unwrapping.
func (e *RenderError) Unwrap() error { return e.Cause }
