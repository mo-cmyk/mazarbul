// Package testutil provides shared test helpers: contexts, fixtures, builders.
package testutil

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/mo-cmyk/mazarbul/internal/correlation"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// TestContext returns a context with a per-test zap logger and a fresh
// correlation ID. Use this in unit and integration tests so log lines are
// attributable to the test that emitted them.
func TestContext(t *testing.T) context.Context {
	t.Helper()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))
	ctx := logging.WithLogger(context.Background(), logger)
	ctx = correlation.WithID(ctx, correlation.New())
	return ctx
}
