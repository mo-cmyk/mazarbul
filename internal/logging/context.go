package logging

import (
	"context"

	"go.uber.org/zap"
)

type loggerKey struct{}

// WithLogger returns ctx carrying logger. Retrieve it with FromContext.
func WithLogger(ctx context.Context, logger *zap.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey{}, logger)
}

// FromContext returns the logger carried by ctx, or a no-op logger if none.
func FromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*zap.Logger); ok && l != nil {
		return l
	}
	return zap.NewNop()
}
