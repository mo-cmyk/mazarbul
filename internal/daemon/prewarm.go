package daemon

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// prewarmModelTimeout caps a single warm-up call. A cold model load is exactly
// the slow case being hidden, so the bound is generous (a large chat model on a
// constrained machine), while still keeping a wedged Ollama from leaking the
// goroutine indefinitely.
const prewarmModelTimeout = 4 * time.Minute

// prewarmModels loads the local Ollama model(s) off the request path so the
// first recall/getEntityContext embed and the first extraction completion do
// not pay a multi-minute cold model-load — the cause of the first-call hangs in
// feedback #3 when Ollama starts with no model resident. Best-effort and fully
// detached: it never blocks serving and only logs. Callers pass a context that
// outlives the boot call (context.WithoutCancel) so a short startup deadline
// does not abort the load mid-flight. Only a LOCAL chat provider is warmed;
// warming Anthropic would issue a billable call for no latency win.
func prewarmModels(ctx context.Context, wired *Wired, logger *zap.Logger) {
	if wired == nil {
		return
	}
	emb, chat, chatModel := warmTargets(wired)
	if emb == nil && chat == nil {
		return
	}
	go warmModels(ctx, emb, chat, chatModel, logger)
}

// warmTargets selects what to pre-warm from a Wired: the embedder always (when
// present), and the chat provider only when it is local Ollama — a remote
// provider is omitted so pre-warm never issues a billable call. Pure so the
// gating is unit-testable without the fire-and-forget goroutine.
func warmTargets(wired *Wired) (kernel.Embedder, kernel.ChatProvider, string) {
	if wired == nil {
		return nil, nil, ""
	}
	if wired.ChatLocal {
		return wired.Embedder, wired.Chat, wired.ChatModel
	}
	return wired.Embedder, nil, ""
}

// warmModels performs the warm-up calls synchronously — embed first, then chat,
// so the two model loads do not contend for a single Ollama inference slot
// (OLLAMA_NUM_PARALLEL defaults to 1). Split from prewarmModels so tests can
// drive it without a goroutine. Every failure is swallowed to a Warn: the
// daemon must serve even when a model cannot be pre-loaded.
func warmModels(ctx context.Context, emb kernel.Embedder, chat kernel.ChatProvider, chatModel string, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if emb != nil {
		warmEmbedModel(ctx, emb, logger)
	}
	if chat != nil {
		warmChatModel(ctx, chat, chatModel, logger)
	}
}

// warmEmbedModel issues one bounded warm-up embed. Factored out so cancel() can
// fire via defer at return (the idiomatic lostcancel-safe form) rather than a
// trailing explicit call. Best-effort: a failure is logged, never propagated.
func warmEmbedModel(ctx context.Context, emb kernel.Embedder, logger *zap.Logger) {
	ec, cancel := context.WithTimeout(ctx, prewarmModelTimeout)
	defer cancel()
	if _, err := emb.Embed(ec, "warmup"); err != nil {
		logger.Warn("daemon: embed model pre-warm failed; first recall will cold-load", zap.Error(err))
		return
	}
	logger.Info("daemon: embed model pre-warmed")
}

// warmChatModel issues one bounded one-token completion to load the chat model.
// Best-effort, like warmEmbedModel.
func warmChatModel(ctx context.Context, chat kernel.ChatProvider, chatModel string, logger *zap.Logger) {
	cc, cancel := context.WithTimeout(ctx, prewarmModelTimeout)
	defer cancel()
	if _, err := chat.Complete(cc, kernel.CompleteRequest{
		Model:     chatModel,
		Messages:  []kernel.Message{{Role: "user", Content: "warmup"}},
		MaxTokens: 1,
	}); err != nil {
		logger.Warn("daemon: chat model pre-warm failed; first extraction will cold-load",
			zap.String("model", chatModel), zap.Error(err))
		return
	}
	logger.Info("daemon: chat model pre-warmed", zap.String("model", chatModel))
}
