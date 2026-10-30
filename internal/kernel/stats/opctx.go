package stats

import "context"

// operationContextKey is the unexported key type for operation labels stored
// in a context. Using a distinct type prevents collisions with other packages
// that store string-keyed values in the same context.
type operationContextKey struct{}

// WithOpContext returns a new context that carries op as the current operation
// label. Downstream code reads the label via OpFromContext to annotate token
// counters with the operation that caused the cost (e.g. "chat",
// "embed-ingest", "extract-entity").
func WithOpContext(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, operationContextKey{}, op)
}

// OpFromContext reads the operation label previously stored by WithOpContext.
// Returns an empty string when no label is present.
func OpFromContext(ctx context.Context) string {
	v, ok := ctx.Value(operationContextKey{}).(string)
	if !ok {
		return ""
	}
	return v
}
