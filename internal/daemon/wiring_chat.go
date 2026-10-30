package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/provider/anthropic"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
	"github.com/mo-cmyk/mazarbul/internal/provider/resilience"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// Circuit-breaker defaults for the chat provider. After this many consecutive
// failures the breaker opens and calls fail fast with resilience.ErrOpen
// instead of hammering a dead backend; after the cooldown a single half-open
// probe tests recovery. Tuned for LLM latency: a few consecutive transport/5xx
// failures (each already retried inside the client) is a strong "backend down"
// signal, and 30s is long enough to let a restarting provider come back.
const (
	chatBreakerMaxFailures = 5
	chatBreakerCooldown    = 30 * time.Second
)

// chatExtractionTimeout is the per-request HTTP deadline for the Ollama chat
// client. Extraction of a single multi-KB episode (prompt-eval of the body plus
// constrained tool-use generation) routinely runs well past the 60s embed
// default on a local model — measured at 70-200s for a ~16KB transcript on a 7B
// model — so the embed-tuned default would cut every extraction call off
// mid-flight and silently produce no entities/relations. This budget lets a
// slow extraction finish; the daemon runs it in the background, so a long call
// never blocks a user request, and the embed client keeps the short default so
// recall still fails fast when Ollama is down.
const chatExtractionTimeout = 5 * time.Minute

// chatProviderOllama is the provider key the wizard writes when the user
// selects Ollama as the chat backend in `maz init`.
const chatProviderOllama = "ollama"

// chatProviderAnthropic is the provider key for the Anthropic backend.
const chatProviderAnthropic = "anthropic"

// chatProviderNone is the explicit-disabled sentinel.
const chatProviderNone = "none"

// errUnsupportedChatProvider sentinels a configuration error in the chat
// provider name. The daemon distinguishes this class from a probe failure:
// a misnamed provider is the operator's fault, while an unreachable backend
// downgrades to extraction-disabled mode (remember + recall still work).
var errUnsupportedChatProvider = errors.New("daemon: unsupported chat provider")

// buildChatProvider constructs a kernel.ChatProvider from user config. The
// return contract is three-valued:
//   - (provider, nil): provider is ready; extractor + resolver + composer
//     can wire it.
//   - (nil, err): fatal configuration error; the daemon refuses to start.
//   - (nil, nil): defaults.chat is empty or "none"; the daemon proceeds in
//     extraction-disabled mode.
//
// The secrets chain is required for Anthropic (the SDK fetches the API key
// lazily on first request) and ignored for Ollama (local, no auth).
//
//nolint:nilnil,gocritic // (nil, nil) is the documented disabled signal; user passed by value at startup-only callsite. ireturn is excluded for internal/daemon/ in .golangci.yml.
func buildChatProvider(ctx context.Context, user config.UserConfig, secrets secret.Provider, logger *zap.Logger) (kernel.ChatProvider, error) {
	key := user.Defaults.Chat
	if key == chatProviderNone {
		logger.Info("daemon: chat provider explicitly disabled (defaults.chat = \"none\"); extractor + resolver + ask skipped")
		return nil, nil
	}
	if key == "" {
		// Empty defaults.chat is almost always misconfiguration: the user ran
		// `maz init` but never picked a chat backend, and the memory layer is
		// silently no-op'd. Logged at Debug, not Warn: the in-process CLI path
		// re-runs wiring per command, so a Warn fired on every invocation for a
		// chat-less store. `maz doctor`'s provider check is the surface that
		// flags it. Set defaults.chat to "none" to opt out explicitly.
		logger.Debug("daemon: defaults.chat is empty; extractor + resolver + ask will be no-op. Set defaults.chat to 'ollama', 'anthropic', or 'none' to configure it")
		return nil, nil
	}

	prov := user.Providers[key]

	switch key {
	case chatProviderOllama:
		opts := ollama.Options{
			BaseURL:        prov.BaseURL,
			ChatModel:      prov.ChatModel,
			RequestTimeout: chatExtractionTimeout,
		}
		client, err := ollama.New(ctx, opts)
		if err != nil {
			// Mirror the embedder's classifyEmbedderErr (wiring.go): a stopped
			// Ollama yields a dial error (connection refused), which is none of
			// the fatal classes, so the daemon degrades to extraction-disabled
			// mode and still boots — recall (FTS-only) + remember keep working
			// on an offline box (#236). Without this, the chat probe's hard
			// failure aborts the whole wiring before recall is ever registered,
			// defeating the offline-robustness guarantee. Operator-fixable
			// faults (malformed config, missing chat model, cancellation) stay
			// fatal so they surface loudly.
			if fatal := classifyChatProbeErr(err); fatal != nil {
				return nil, fmt.Errorf("daemon: build ollama chat client: %w", fatal)
			}
			logger.Warn("daemon: ollama chat provider unreachable; extractor + resolver + ask disabled (recall + remember still work). Extraction resumes after a daemon restart once Ollama is back",
				zap.Error(err))
			return nil, nil //nolint:nilnil // documented extraction-disabled signal; matches the (nil, nil) contract above.
		}
		// Bound concurrent /api/chat to the daemon's OLLAMA_NUM_PARALLEL so a
		// burst of extractions does not queue past the per-request HTTP
		// timeout against a single-slot Ollama. prov.NumParallel == 0 (unset)
		// falls back to the safe single-slot default inside NewChatProvider.
		//
		// Note: prov.NumParallel is what we ASK the supervisor to set. For an
		// unmanaged `ollama serve`, this may understate the daemon's real slot
		// count (we then under-utilise but never time out). Log the chosen
		// bound so an operator can spot a mismatch.
		cp, err := ollama.NewChatProvider(client, prov.NumParallel)
		if err != nil {
			return nil, fmt.Errorf("daemon: wrap ollama chat provider: %w", err)
		}
		logger.Info("daemon: ollama chat provider ready",
			zap.Int("num_parallel", prov.NumParallel),
			zap.Int("chat_concurrency", cp.MaxConcurrent()))
		return guardChatProvider(cp, logger)
	case chatProviderAnthropic:
		if secrets == nil {
			return nil, errors.New("daemon: anthropic chat provider requires a non-nil secrets chain")
		}
		opts := &anthropic.Options{
			BaseURL:        prov.BaseURL,
			SecretProvider: secrets,
		}
		client, err := anthropic.New(opts)
		if err != nil {
			return nil, fmt.Errorf("daemon: build anthropic client: %w", err)
		}
		cp, err := anthropic.NewChatProvider(client)
		if err != nil {
			return nil, fmt.Errorf("daemon: wrap anthropic chat provider: %w", err)
		}
		return guardChatProvider(cp, logger)
	default:
		return nil, fmt.Errorf(
			"%w: chat provider %q (M1 understands %q, %q, %q)",
			errUnsupportedChatProvider, key,
			chatProviderOllama, chatProviderAnthropic, chatProviderNone,
		)
	}
}

// classifyChatProbeErr decides whether an ollama.New failure when building the
// chat provider must fail the daemon or downgrade to extraction-disabled mode.
// It mirrors classifyEmbedderErr (wiring.go) and draws the same line at "can
// this ever work as configured?": an availability problem degrades so the
// daemon still boots (recall + remember keep working; extraction resumes after
// a restart once Ollama is back), while a genuine config typo or a cancelled
// boot stays fatal.
//
//   - Availability (degrade → nil chat provider): a stopped Ollama (dial error
//     / connection refused) and ollama.ErrModelMissing (the chat model is not
//     pulled — `ollama pull <chat-model>` fixes it). Neither should wall an
//     offline box's recall just because extraction is unavailable (#236).
//   - Fatal (return the error): ollama.ErrInvalidConfig (malformed BaseURL,
//     returned before any probe) and context.Canceled / DeadlineExceeded
//     (caller asked us to stop).
//
// Returns nil for an availability failure (caller degrades); the original error
// for a fatal class.
func classifyChatProbeErr(err error) error {
	switch {
	case errors.Is(err, ollama.ErrInvalidConfig),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return nil
	}
}

// guardChatProvider wraps a constructed chat provider with a circuit breaker so
// autonomous jobs fail fast against a dead backend instead of hammering it
// (#236). The wrapped provider satisfies kernel.ChatProvider, so callers are
// unaffected; only sustained provider failures change behaviour.
func guardChatProvider(cp kernel.ChatProvider, logger *zap.Logger) (kernel.ChatProvider, error) {
	breaker := &resilience.CircuitBreaker{
		MaxConsecutiveFailures: chatBreakerMaxFailures,
		Cooldown:               chatBreakerCooldown,
	}
	guarded, err := resilience.NewChatProvider(cp, breaker)
	if err != nil {
		return nil, fmt.Errorf("daemon: guard chat provider with circuit breaker: %w", err)
	}
	logger.Debug("daemon: chat provider guarded by circuit breaker",
		zap.Int("max_consecutive_failures", chatBreakerMaxFailures),
		zap.Duration("cooldown", chatBreakerCooldown),
	)
	return guarded, nil
}
