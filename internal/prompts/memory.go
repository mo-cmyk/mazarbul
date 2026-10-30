package prompts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"text/template"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// entryKey is the map key used inside MemoryRegistry. Using a struct key
// avoids string concatenation on every lookup; the Go runtime hashes struct
// keys more cheaply than formatted strings.
type entryKey struct {
	name    string
	version Version
}

// entry is a registered template plus its pre-compiled Go text/template.
// Compiled at Register time so Render never re-parses on the hot path.
type entry struct {
	tmpl     Template
	compiled *template.Template
}

// MemoryRegistry is the in-process implementation of Registry. All state
// lives in a sync.RWMutex-guarded map; Render holds only the read lock.
//
// Safe for concurrent use. The zero value is not usable; call New.
type MemoryRegistry struct {
	mu      sync.RWMutex
	entries map[entryKey]entry
	log     kernel.EventLog // nil means skip audit
}

// New returns a MemoryRegistry ready to use. Log may be nil; when nil, audit
// events are silently dropped and Render still returns the rendered string.
//
// The kernel does not import internal/prompts so there is no import cycle:
// the dependency arrow is prompts -> kernel, never kernel -> prompts.
func New(log kernel.EventLog) *MemoryRegistry {
	return &MemoryRegistry{
		entries: make(map[entryKey]entry),
		log:     log,
	}
}

// checkedIndex is a FuncMap replacement for Go text/template's built-in
// "index" function. It mirrors the built-in behaviour for arrays, slices,
// strings, and non-string map keys, but adds an error when a string key is
// absent from a map[string]string. This covers the {{index . "key"}} authoring
// style that missingkey=error does not protect (missingkey=error only applies
// to dot-notation lookups such as {{.key}}).
//
// Multi-index forms ({{index . "key" 0}}) and computed-key forms
// ({{$k := "name"}}{{index . $k}}) are also handled: the map lookup leg
// errors on any missing string key regardless of how the key value arrived.
//
// All non-map-string-key index paths fall through to standard reflect-based
// slice/array/string indexing so no existing template behaviour is broken.
//
//nolint:gocognit // mirrors the built-in index function; case complexity is structural.
func checkedIndex(item reflect.Value, indexes ...reflect.Value) (reflect.Value, error) {
	item = reflect.Indirect(item)
	if !item.IsValid() {
		return reflect.Value{}, errors.New("index of untyped nil")
	}
	for _, idx := range indexes {
		idx = reflect.Indirect(idx)
		switch item.Kind() {
		case reflect.Map:
			preparedIdx := idx
			if idx.Kind() != item.Type().Key().Kind() {
				converted := reflect.New(item.Type().Key()).Elem()
				if !idx.Type().ConvertibleTo(item.Type().Key()) {
					return reflect.Value{}, fmt.Errorf("index key type mismatch: got %s, want %s", idx.Type(), item.Type().Key())
				}
				converted.Set(idx.Convert(item.Type().Key()))
				preparedIdx = converted
			}
			v := item.MapIndex(preparedIdx)
			if !v.IsValid() {
				// For string keys return a descriptive error; for other key types
				// preserve the built-in zero-value behaviour to avoid surprises.
				if idx.Kind() == reflect.String {
					return reflect.Value{}, fmt.Errorf("prompts: missing template variable %q", idx.String())
				}
				return reflect.Zero(item.Type().Elem()), nil
			}
			item = v
		case reflect.Array, reflect.Slice, reflect.String:
			if idx.Kind() != reflect.Int && !idx.CanInt() {
				return reflect.Value{}, fmt.Errorf("index must be an integer, got %s", idx.Kind())
			}
			i := idx.Int()
			if i < 0 || i >= int64(item.Len()) {
				return reflect.Value{}, fmt.Errorf("index %d out of range (len %d)", i, item.Len())
			}
			item = item.Index(int(i))
		default:
			return reflect.Value{}, fmt.Errorf("can't index item of type %s", item.Type())
		}
	}
	return item, nil
}

// templateFuncs returns the FuncMap used when compiling prompt templates.
// The "index" entry is replaced with checkedIndex so missing map-key lookups
// produce a RenderError instead of silently expanding to the zero string.
func templateFuncs() template.FuncMap {
	return template.FuncMap{"index": checkedIndex}
}

// Register implements Registry. It compiles t.Body at call time so Render
// never re-parses. The same (Name, Version, Body) triple is idempotent;
// a different Body with an existing (Name, Version) returns ErrVersionConflict.
// Version zero is rejected.
func (r *MemoryRegistry) Register(_ context.Context, t Template) error { //nolint:gocritic // hugeParam: Template is value-typed per the Registry interface contract
	if t.Name == "" {
		return errors.New("prompts: register: empty Name")
	}
	if t.Version == 0 {
		return fmt.Errorf("prompts: register %q: version must be non-zero", t.Name)
	}

	// missingkey=error catches dot-notation lookups ({{.key}}) at execution
	// time. The "index" FuncMap override in templateFuncs() catches all index
	// function calls ({{index . "key"}}, {{index . $var}}, multi-index) so
	// both authoring styles fail fast on missing variables without breaking any
	// valid template behaviour.
	compiled, err := template.New(t.Name).Funcs(templateFuncs()).Option("missingkey=error").Parse(t.Body)
	if err != nil {
		return fmt.Errorf("prompts: register %q v%d: parse template: %w", t.Name, t.Version, err)
	}

	k := entryKey{name: t.Name, version: t.Version}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.entries[k]; ok {
		if existing.tmpl.Body == t.Body {
			return nil // idempotent
		}
		return fmt.Errorf("%w: %s@%d", ErrVersionConflict, t.Name, t.Version)
	}

	t.CreateTime = time.Now().UTC()
	r.entries[k] = entry{tmpl: t, compiled: compiled}
	return nil
}

// Get implements Registry. Returns ErrNotFound when no template matches
// (name, version).
func (r *MemoryRegistry) Get(_ context.Context, name string, version Version) (Template, error) {
	r.mu.RLock()
	e, ok := r.entries[entryKey{name: name, version: version}]
	r.mu.RUnlock()

	if !ok {
		return Template{}, fmt.Errorf("%w: %s@%d", ErrNotFound, name, version)
	}
	return e.tmpl, nil
}

// Render implements Registry. It executes the compiled template with vars
// and returns the rendered string. The audit event is emitted asynchronously
// in a goroutine so the hot path is not blocked by the event-log write.
//
// Audit durability: the audit goroutine is best-effort. It uses
// context.WithoutCancel + a 5-second deadline, so a cancelled caller context
// does not silently drop the record, but a process crash between Render
// returning and the goroutine completing will lose the event. Append errors
// are warn-logged. This is acceptable for prompt audit: the call itself is
// not durable state.
//
// Privacy: vars values are never written to the audit event. Only the
// SHA-256 hash of the canonical JSON serialisation of vars and the byte
// length of the output are recorded.
func (r *MemoryRegistry) Render(ctx context.Context, name string, version Version, vars RenderVars) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", &RenderError{Name: name, Version: version, Cause: err}
	}

	r.mu.RLock()
	e, ok := r.entries[entryKey{name: name, version: version}]
	r.mu.RUnlock()

	if !ok {
		return "", &RenderError{
			Name:    name,
			Version: version,
			Cause:   fmt.Errorf("%w: %s@%d", ErrNotFound, name, version),
		}
	}

	// Snapshot vars before template execution. The caller owns the original
	// RenderVars map and may reuse or mutate it concurrently. Copying before
	// Execute prevents a data race during template execution itself and ensures
	// the audit goroutine hashes the same values that were rendered.
	varsCopy := make(RenderVars, len(vars))
	for k, v := range vars {
		varsCopy[k] = v
	}

	var buf strings.Builder
	buf.Grow(len(e.tmpl.Body))
	if err := e.compiled.Execute(&buf, varsCopy); err != nil {
		return "", &RenderError{Name: name, Version: version, Cause: err}
	}
	out := buf.String()

	r.emitRendered(ctx, name, version, varsCopy, len(out))
	return out, nil
}

// List implements Registry. Returns a snapshot of all registered templates.
// Order is undefined; callers must not rely on it being stable across calls.
func (r *MemoryRegistry) List(_ context.Context) ([]Template, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Template, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.tmpl)
	}
	return out, nil
}

// emitRendered appends a prompt.rendered audit event to the kernel event log
// in a background goroutine. Durability is best-effort: context.WithoutCancel
// prevents a cancelled caller context from suppressing the write, and a 5-
// second deadline bounds the blocking time, but a process exit between Render
// returning and the goroutine completing will lose the event. Callers that
// require durable audit must persist the record themselves before calling
// Render. Append failures are warn-logged; they do not propagate to the
// caller because the rendered output has already been returned.
//
// The vars argument must be a caller-owned snapshot (not the original
// RenderVars map). Render performs the copy before calling emitRendered.
func (r *MemoryRegistry) emitRendered(ctx context.Context, name string, version Version, vars RenderVars, outputLen int) {
	if r.log == nil {
		return
	}

	payload := RenderedPayload{
		TemplateName:    name,
		TemplateVersion: version,
		InputHash:       inputHash(vars),
		OutputLen:       outputLen,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal on a plain struct with string/int fields cannot fail;
		// log defensively and bail rather than panic.
		logging.FromContext(ctx).Warn("prompts: marshal rendered payload",
			zap.String("template", name),
			zap.Uint32("version", uint32(version)),
			zap.Error(err),
		)
		return
	}

	ev := kernel.Event{
		Kind:       KindRendered,
		Source:     "prompts/registry",
		Confidence: 1.0,
		Payload:    raw,
	}

	// Detach from the caller's cancellation so a short-lived request context
	// does not silently drop the audit record. Bound with a 5-second deadline.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)

	go func() {
		defer cancel()
		logger := logging.FromContext(ctx)
		if _, appendErr := r.log.Append(auditCtx, []kernel.Event{ev}); appendErr != nil {
			logger.Warn("Prompts audit emit failed",
				zap.String("template", name),
				zap.Uint32("version", uint32(version)),
				zap.Error(appendErr),
			)
		}
	}()
}

var _ Registry = (*MemoryRegistry)(nil)
