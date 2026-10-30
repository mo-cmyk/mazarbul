// Package observability will host tracing and metrics setup. OpenTelemetry
// wiring lands once the kernel is real; placeholder for now.
package observability

import "context"

// Init is a no-op until OTel SDK setup lands.
func Init(_ context.Context) error { return nil }

// Shutdown is a no-op until OTel SDK setup lands.
func Shutdown(_ context.Context) error { return nil }
