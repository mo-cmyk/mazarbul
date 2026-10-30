//go:build !windows

package daemon

import (
	"context"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// maybeStartOllamaSupervisor reads the user config and, when
// providers.ollama.managed == true, constructs and starts an
// OllamaSupervisor goroutine bound to runCtx. The goroutine terminates
// the ollama child and removes the sidecar PID file when runCtx is
// cancelled. Config-load failures are silent so a missing or unreadable
// config.toml never blocks the main daemon from starting.
//
// This function is compiled only on non-Windows targets because
// OllamaSupervisor uses POSIX process signals. The Windows stub in
// ollama_supervisor_hook_windows.go is a no-op.
func maybeStartOllamaSupervisor(runCtx context.Context, opts *SupervisorOptions) {
	cfg, err := config.LoadUserConfig(opts.DataDir)
	if err != nil {
		// Missing config (first-run) or unreadable file: no managed Ollama.
		return
	}
	ollamaTable, ok := cfg.Providers["ollama"]
	if !ok || !ollamaTable.Managed {
		return
	}
	// Thread the user-configured base_url into the supervisor so the
	// pre-spawn probe, the wedged-takeover probe, AND the spawned child
	// all target the same endpoint the embedder/chat client uses.
	// Without this, a managed provider on :11500 with something else
	// answering on :11434 would enter delegated mode against the wrong
	// process — the supervisor would report healthy while the embedder
	// keeps failing.
	sup := NewOllamaSupervisor(OllamaSupervisorOptions{
		DataDir:     opts.DataDir,
		Logger:      opts.Logger,
		NumParallel: ollamaTable.NumParallel,
		OllamaURL:   ollamaTable.BaseURL,
	})
	if serr := sup.Start(runCtx); serr != nil {
		opts.Logger.Warn("daemon: start ollama supervisor", zap.Error(serr))
	}
}
