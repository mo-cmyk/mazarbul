# ChatProvider abstraction

`kernel.ChatProvider` is the unified LLM interface the kernel uses for all eight
structured-output leverage points (extraction, composition, disambiguation,
contradiction detection, entity summary, predicate canonicalisation, query
rewriting, naming canonicalisation).

## Interface

```go
type ChatProvider interface {
    Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
    ToolUse(ctx context.Context, req ToolUseRequest) (ToolUseResponse, error)
}
```

`Complete` is the free-form prose path (composition, query rewriting).
`ToolUse` is the structured-output path: the caller supplies one `ToolSchema`
with a JSON Schema `InputSchema`; the provider returns a `ToolInvocation` whose
`Input` field is a `json.RawMessage` already validated against that schema.

## Implementations

### `anthropic.ChatProvider` (`internal/provider/anthropic/chatprovider.go`)

Wraps `*anthropic.Client`. Routes `ToolUse` to the native Anthropic tool-use API
(`POST /v1/messages` with `tools` array and `tool_choice:{type:"any"}`).

- **`ErrNoToolUse`**: returned when `stop_reason` is not `"tool_use"` (model
  produced prose instead of a tool call). Wrap-check with `errors.Is`.
- The retry loop and API-key cache from `*Client` are reused transparently.

### `ollama.ChatProvider` (`internal/provider/ollama/chatprovider.go`)

Wraps `*ollama.Client`. Dispatches to one of two paths on first `ToolUse` call:

**Native path (Ollama >= 0.4)**: `POST /api/chat` with a `tools` array. Probed
lazily on first call via `GET /api/version`; result cached in a `sync.Once`.
Returns `ErrNoToolCall` when `tool_calls` is absent in the response.

**JSON-mode fallback (Ollama < 0.4 or probe failure)**: `POST /api/chat` with
`format:"json"` and the tool schema + description injected into the system prompt.
The raw JSON response is validated against `ToolSchema.InputSchema` using
`santhosh-tekuri/jsonschema/v6`. Returns `ErrSchemaValidation` when the model
output does not conform.

- **`ErrNoToolCall`**: native path, no tool call in response.
- **`ErrSchemaValidation`**: fallback path, response fails schema validation.
- Version probe failure is treated as "no native support" - the fallback runs
  transparently without surfacing a probe error to the caller.

## `ToolUseRequest` sizing

`kernel.ToolUseRequest` is 128 bytes. Public `ToolUse` methods carry a value
receiver to satisfy the interface; private helpers (`toolUseNative`,
`toolUseJSONFallback`) take `*kernel.ToolUseRequest` to avoid the `hugeParam`
lint finding. The `//nolint:gocritic // hugeParam: value signature forced by
kernel.ChatProvider interface.` directive lives on the public method line only.

## Wiring

`OpenWiring` in `internal/daemon/wiring.go` constructs both providers and
threads a `secretProviderAdapter` (env -> keychain chain) through
`mcp.Options.Secrets`. The `kernel.ChatProvider` implementations are constructed
per-provider in the Phase 1 memory pipeline; the P0 PR ships the adapters and
tests only - the kernel does not yet call `ChatProvider` directly.

## Extending

To add a new provider:
1. Implement `kernel.ChatProvider` in a new `internal/provider/<name>/` package.
2. Add a compile-time assertion: `var _ kernel.ChatProvider = (*ChatProvider)(nil)`.
3. Use `*kernel.ToolUseRequest` for private helpers; value for the public method.
4. Name sentinel errors `Err<Condition>` in the package and document them in godoc.
