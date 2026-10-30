# Plugins

Mazarbul's provider system is the extension point for LLM and embedding
backends. Today two providers ship out of the box:

| Provider   | Chat | Embed | Notes                                      |
|------------|:----:|:-----:|--------------------------------------------|
| anthropic  |  ✓   |       | Requires `ANTHROPIC_API_KEY` or keychain.  |
| ollama     |  ✓   |   ✓   | Local inference; no API key required.      |

## Configuring providers

Set the active provider in `config.toml`:

```toml
[defaults]
chat  = "ollama"
embed = "ollama"

[providers.ollama]
base_url    = "http://localhost:11434"
chat_model  = "qwen2.5:7b"
embed_model = "snowflake-arctic-embed2"
```

## Adding a provider (for contributors)

1. Implement `ChatProvider` or `EmbedProvider` in `internal/provider/<name>/`.
2. Register it in `internal/kernelboot/providers.go`.
3. Add a `[providers.<name>]` section to the `UserConfig` TOML struct.
4. Update `maz init` to offer the new provider in the wizard.
5. Add an integration test under `internal/provider/<name>/`.

Provider implementations are plain Go — no plugin ABI, no separate process.
The provider interface is intentionally small:

```go
type ChatProvider interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
```
