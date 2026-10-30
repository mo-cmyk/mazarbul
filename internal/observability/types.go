package observability

import (
	"context"
	"time"
)

// Logger is the thin facade over the structured logger (zap under the
// hood, see internal/logging). The kernel and plugins talk to Logger;
// the zap-specific surface stays inside internal/logging so swapping
// loggers later is a one-package change.
type Logger interface {
	Debug(ctx context.Context, msg string, fields ...Field)
	Info(ctx context.Context, msg string, fields ...Field)
	Warn(ctx context.Context, msg string, fields ...Field)
	Error(ctx context.Context, msg string, fields ...Field)
}

// Field is a structured log key-value pair. Keep the surface small: at
// pre-alpha the only field kinds we exercise are string, int, error,
// duration. Add more as concrete callers need them.
type Field struct {
	Key   string
	Value any
}

// String returns a string-valued log field.
func String(k, v string) Field { return Field{Key: k, Value: v} }

// Int returns an int-valued log field.
func Int(k string, v int) Field { return Field{Key: k, Value: v} }

// Err returns an error-valued log field. Conventionally keyed "error".
func Err(err error) Field { return Field{Key: "error", Value: err} }

// Duration returns a time.Duration-valued log field.
func Duration(k string, v time.Duration) Field { return Field{Key: k, Value: v} }

// Tracer is the thin facade over OpenTelemetry tracing. M1 ships a no-op
// implementation; real OTel SDK wiring lands when there's a configured
// exporter to ship spans to.
type Tracer interface {
	// Start begins a span. The returned Context carries the active span
	// so downstream code adds events and child spans to it; the returned
	// closer ends the span (call via defer).
	Start(ctx context.Context, name string, attrs ...Attribute) (context.Context, SpanCloser)
}

// SpanCloser ends an active span.
type SpanCloser interface {
	End()
	SetError(err error)
}

// Attribute is the tracing equivalent of Field. OTel calls them
// "attributes"; we mirror the term.
type Attribute struct {
	Key   string
	Value any
}

// Metrics is the thin facade over the metrics SDK. Same minimal-surface
// rule as Logger and Tracer: add only what callers need.
type Metrics interface {
	Counter(name string, attrs ...Attribute) Counter
	Histogram(name string, attrs ...Attribute) Histogram
}

// Counter is a monotonically increasing counter.
type Counter interface {
	Inc(ctx context.Context, by int64, attrs ...Attribute)
}

// Histogram records observations into bucketed distributions.
type Histogram interface {
	Observe(ctx context.Context, value float64, attrs ...Attribute)
}

// NoopLogger discards every record. Used when an explicit no-logger is
// preferred over passing nil.
type NoopLogger struct{}

// Debug implements Logger.
func (NoopLogger) Debug(_ context.Context, _ string, _ ...Field) {}

// Info implements Logger.
func (NoopLogger) Info(_ context.Context, _ string, _ ...Field) {}

// Warn implements Logger.
func (NoopLogger) Warn(_ context.Context, _ string, _ ...Field) {}

// Error implements Logger.
func (NoopLogger) Error(_ context.Context, _ string, _ ...Field) {}

// NoopTracer produces a NoopSpanCloser for every span.
type NoopTracer struct{}

// Start implements Tracer.
func (NoopTracer) Start(ctx context.Context, _ string, _ ...Attribute) (context.Context, SpanCloser) {
	return ctx, noopSpanCloser{}
}

type noopSpanCloser struct{}

func (noopSpanCloser) End()             {}
func (noopSpanCloser) SetError(_ error) {}

// NoopMetrics produces no-op counters and histograms.
type NoopMetrics struct{}

// Counter implements Metrics.
func (NoopMetrics) Counter(_ string, _ ...Attribute) Counter {
	return noopCounter{}
}

// Histogram implements Metrics.
func (NoopMetrics) Histogram(_ string, _ ...Attribute) Histogram {
	return noopHistogram{}
}

type noopCounter struct{}

func (noopCounter) Inc(_ context.Context, _ int64, _ ...Attribute) {}

type noopHistogram struct{}

func (noopHistogram) Observe(_ context.Context, _ float64, _ ...Attribute) {}
