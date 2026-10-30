//go:build windows

package daemon

import "context"

// maybeStartOllamaSupervisor is a no-op on Windows. The Ollama lifecycle
// supervisor relies on POSIX signals (SIGTERM, signal-0 liveness probe) which
// are not available on Windows. Users on Windows must manage the `ollama serve`
// process manually or via a system service.
func maybeStartOllamaSupervisor(_ context.Context, _ *SupervisorOptions) {}
