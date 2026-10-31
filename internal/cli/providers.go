package cli

// Chat and embed provider identifiers used across init, config, and the
// provider table. Stable strings — they get persisted into config.toml
// and referenced by [providers.<id>] sections.
const (
	// The "none" chat id is the explicit "no server-side chat model"
	// sentinel and the init default: the calling agent does the reasoning,
	// Mazarbul stores and recalls. The daemon reads it as chat disabled.
	chatProviderNone      = "none"
	chatProviderAnthropic = "anthropic"
	chatProviderOllama    = "ollama"

	embedProviderOllama = "ollama"
	embedProviderNone   = "none"
)

// Re-exported under their historical wizard.* names so existing tests and
// callers that have not yet been swept keep compiling. New code should use
// the unexported constants above.
const (
	// ChatProviderAnthropic is the canonical id for the Anthropic chat slot.
	ChatProviderAnthropic = chatProviderAnthropic
	// ChatProviderOllama is the canonical id for the Ollama chat slot.
	ChatProviderOllama = chatProviderOllama
	// ChatProviderNone is the sentinel written to defaults.chat when no
	// server-side chat model is configured (the calling agent reasons).
	ChatProviderNone = chatProviderNone
	// EmbedProviderOllama is the canonical id for the Ollama embedding slot.
	EmbedProviderOllama = embedProviderOllama
	// EmbedProviderNone is the sentinel written to defaults.embed when the
	// user opts out of embeddings (disables recall until re-enabled).
	EmbedProviderNone = embedProviderNone
)
