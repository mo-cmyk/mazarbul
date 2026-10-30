package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
)

// TestClassifyChatProbeErr asserts the chat-provider build draws the same
// availability-vs-config line as the embedder (#236): an unreachable Ollama or
// a not-yet-pulled chat model degrades to extraction-disabled (the daemon still
// boots; recall + remember keep working), while a malformed config or a
// cancelled boot stays fatal.
func TestClassifyChatProbeErr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      error
		wantNil bool // nil result == degrade to extraction-disabled mode
	}{
		{name: "nil", in: nil, wantNil: true},
		{
			// A stopped Ollama surfaces as a dial error; it must degrade so the
			// offline binary still serves recall.
			name:    "unreachable",
			in:      errors.New("dial tcp 127.0.0.1:11434: connect: connection refused"),
			wantNil: true,
		},
		{
			// A not-yet-pulled chat model only blocks extraction, never recall.
			name:    "model missing",
			in:      ollama.ErrModelMissing,
			wantNil: true,
		},
		{
			name:    "wrapped model missing",
			in:      &wrappedError{inner: ollama.ErrModelMissing},
			wantNil: true,
		},
		{
			// A malformed BaseURL can never work as configured: stay fatal so
			// the operator sees the typo instead of a silently crippled daemon.
			name:    "invalid config",
			in:      ollama.ErrInvalidConfig,
			wantNil: false,
		},
		{
			name:    "wrapped invalid config",
			in:      &wrappedError{inner: ollama.ErrInvalidConfig},
			wantNil: false,
		},
		{name: "context canceled", in: context.Canceled, wantNil: false},
		{name: "context deadline", in: context.DeadlineExceeded, wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyChatProbeErr(tc.in)
			if tc.wantNil && got != nil {
				t.Errorf("classifyChatProbeErr(%v) = %v, want nil (degrade)", tc.in, got)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("classifyChatProbeErr(%v) = nil (degrade), want fatal", tc.in)
			}
			if !tc.wantNil && got != nil && !errors.Is(got, tc.in) {
				t.Errorf("classifyChatProbeErr(%v) = %v, must wrap or equal original", tc.in, got)
			}
		})
	}
}
