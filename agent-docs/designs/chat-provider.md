# Chat provider design (Anthropic + Ollama, M1)

**Status**: Implemented (Anthropic #49, Ollama #86)
**Last updated**: 2026-05-24

## Context

M1's walking skeleton needs one chat provider live end-to-end: `maz init`,
collect API key, `maz daemon start`, `remember`, `recall`. Anthropic is
that provider. The M3 plugin loader makes the chat slot swappable; until
then, the Anthropic implementation ships inside the binary at
`internal/provider/anthropic`.

The chat client also needs to read its API key from a single place that
the init wizard, the daemon, and tests all share. That contract is the
`secret.Provider` interface at `internal/secret`, which ships with the
Anthropic client.

This document explains the shape we shipped and why, so the next provider
(Ollama embed, OpenAI chat in M3) can follow the same conventions without
re-deriving them.

## Surface

The package exports four kinds of names:

- **`Client`, `Options`, `New`, `Complete`**: the API entry points.
- **`CompleteRequest`, `CompleteResponse`, `Message`, `ContentBlock`,
  `CacheControl`, `Usage`, `Role`, `ContentType`, `TextBlock`,
  `EphemeralCache`**: the request/response types and a few convenience
  constructors.
- **`APIError`**: the structured non-2xx error, with `Type`,
  `StatusCode`, `Message`, `RequestID`. `errors.As`-friendly.
- Unexported helpers: `retryable`, `backoff`, `parseRetryAfter`,
  `wireRequest`, `wireError`. Tests live in the same package.

Signatures (no breaking changes from issue #49 except the by-pointer
request: gocritic flags `CompleteRequest` as a hugeParam at 112 bytes, so
the function takes `*CompleteRequest`):

```go
func New(opts *Options) (*Client, error)
func (c *Client) Complete(ctx context.Context, req *CompleteRequest) (CompleteResponse, error)
```

`Options` zero-value-defaults every field except `SecretProvider`, which
is required. Pinned defaults:

| Field | Default |
|---|---|
| `BaseURL` | `https://api.anthropic.com` |
| `AnthropicVersion` | `2023-06-01` |
| `Timeout` | `30 * time.Second` |
| `MaxRetries` | `3` (3 retries on top of the initial attempt = up to 4 attempts) |
| `UserAgent` | `mazarbul/0.x` |
| `SecretName` | `secret.NameAnthropicAPIKey` (`"anthropic.api_key"`) |

## Retry policy

Per issue #49: 3 retries with exponential backoff on 5xx and 429, honor
`Retry-After`, no retry on any other 4xx. The official Anthropic SDK also
retries on 408 and 409; we deliberately keep the surface tighter at M1
and document the divergence in `retry.go`.

Exponential backoff: `min(initialBackoff * 2^attempt, maxBackoff) * (1 -
jitterFrac * rand())`, where `initialBackoff = 1s`, `maxBackoff = 60s`,
`jitterFrac = 0.25`. When the server sends `Retry-After`, the client
honors it verbatim (capped at `maxBackoff`); no jitter is applied because
the server explicitly told us when to come back.

`parseRetryAfter` accepts the seconds form (integer or fractional) and
the HTTP-date form, per RFC 7231.

## Prompt-cache plumbing

`ContentBlock` carries an optional `*CacheControl` field. When set, the
block is serialized with the Anthropic `cache_control: {type: "ephemeral"}`
marker. The client does not decide where breakpoints go; the recall
composer (later M1 issue) places them on the system prompt and on
high-reuse user content.

`Usage` decodes `cache_creation_input_tokens` and
`cache_read_input_tokens` so the budget-and-costs subsystem can
distinguish cache writes (full rate) from cache reads (10% rate).

## SecretProvider integration

`secret.Provider` has three methods: `Get`, `Set`, `Delete`. Three
implementations:

- `secret.KeychainProvider` wraps `github.com/zalando/go-keyring`, which
  uses the macOS Keychain via `/usr/bin/security`, libsecret over D-Bus on
  Linux, and the Credential Manager on Windows. No CGO. Service identifier
  is the constant `mazarbul`; account name is the canonical secret name
  (e.g., `"anthropic.api_key"`).
- `secret.EnvProvider` is read-only. It looks up `MAZ_SECRET_<UPPER_DOT_TO_UNDERSCORE>`
  first, then a legacy form (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`).
- `secret.Chain` composes Providers. `Chain(env, keychain)` lets the env
  var win in CI without a branch at the call site; the keychain absorbs
  writes from `maz init` so the user's key persists.

The Anthropic client reads the key lazily on the first `Complete` call
and caches the result (success or failure). A missing key surfaces as
`secret.ErrNotFound` wrapped by the client.

## Error taxonomy

| Status | Behaviour |
|---|---|
| 2xx | Decode `CompleteResponse`, stamp `CreateTime` UTC, return. |
| 429 | Retry. Honor `Retry-After` when present. |
| 5xx | Retry with exponential backoff. |
| Every other 4xx (400, 401, 403, 404, 408, 409, 413, ...) | Fail fast with `*APIError`. |
| Transport error | Retry. Bail out when `ctx.Err()` is non-nil. |

`APIError.Error()` never prints request bodies or the API key. The
`Type`, `Message`, `StatusCode`, and `RequestID` fields are enough for
callers (and for support tickets via the `request-id` header).

## Concurrency

`Client` is safe for concurrent use. The API key is read once under a
`sync.Once`; the cached value is read under a `sync.Mutex` for the rare
case where `keyOnce.Do` and a reader race on the first call.

## What is out of scope for M1

- Streaming completions (server-sent events). M2 ships them as a separate
  `CompleteStream` method on `Client`.
- Tool / function-calling. M3 wires them in alongside the recall composer.
- Multi-provider abstraction. There is no `Chat` interface yet; the
  daemon talks directly to `anthropic.Client`. M3 introduces a `Chat`
  interface when OpenAI lands as the second built-in provider.
- Per-call zap structured logging. The client emits no logs from unit
  tests; the daemon will inject a logger when it wires up. This is open
  question 6 on the plan; defer.

## Tests

`go test ./internal/secret/... ./internal/provider/anthropic/... -race`
covers 86–89% of statements:

- `client_test.go`: success path, 429 + Retry-After retry, 5xx retry,
  exhausted retries (asserts exactly 1 initial + 3 retries = 4 attempts),
  401 / 400 fast-fail, context cancel during backoff, missing API key,
  request validation, API-key cache-once.
- `retry_test.go`: `retryable` table over status codes, `backoff` bounds
  and jitter floor, retry-after override and cap, header parsing of
  seconds / fractional / HTTP-date / malformed.
- `secret/`: keychain round-trip via `keyring.MockInit`, env-var
  canonical and legacy precedence, chain ordering, write-through past
  read-only members.

## References

- the original architecture notes, plugin slots.
- the original architecture notes,
  secret storage policy.
- the original architecture notes, cache
  token accounting.
- https://docs.anthropic.com/en/api/messages, Messages API.
- https://docs.anthropic.com/en/api/errors, error taxonomy.
- https://docs.anthropic.com/en/api/versioning, `2023-06-01` version pin.
- `github.com/zalando/go-keyring` v0.2.8.

---

# Ollama chat provider (M1, issue #86)

The local-first sibling to the Anthropic client. Lives at
`internal/provider/ollama` alongside the embedding code from #50 and
reuses its HTTP client, retry helper, and model-probe wiring. Same
package, single `Client` that dispatches both `Embed` and `Complete`.

## Surface

```go
const DefaultChatModel = "llama3.1:8b"

type Options struct {
    BaseURL        string
    EmbedModel     string
    ChatModel      string         // empty = chat disabled, no probe, Complete fails loud
    HTTPClient     *http.Client
    SecretProvider secret.Provider // accepted for parity with anthropic.Options; unused
}

func New(ctx context.Context, opts Options) (*Client, error)
func (c *Client) ChatModel() string
func (c *Client) Complete(ctx context.Context, req *CompleteRequest) (CompleteResponse, error)

// Mazarbul-side types (mirrored after anthropic.* without cross-importing).
type CompleteRequest struct {
    Model         string
    System        string    // single string; Ollama has no block array for system prompts
    Messages      []Message
    MaxTokens     int       // -> options.num_predict
    Temperature   *float64  // -> options.temperature, omitempty
    StopSequences []string  // -> options.stop, omitempty
}

type CompleteResponse struct {
    Model      string
    Role       Role           // always RoleAssistant on success
    Content    []ContentBlock // single TextBlock; shape parity with anthropic
    StopReason string         // Ollama done_reason verbatim
    Usage      Usage          // {InputTokens, OutputTokens}; ns durations deferred to M2
    CreateTime time.Time      // synthesized UTC; wire created_at is discarded
}
```

## Retry policy

Inherited from the embed `doJSON` helper at `client.go`. Tighter than
Anthropic because Ollama is local:

| Knob | Ollama | Anthropic |
|---|---|---|
| Max retries | 2 (3 attempts) | 3 (4 attempts) |
| Base backoff | 200 ms | 1 s |
| Max backoff | implicit (200ms << attempts) | 60 s |
| Jitter | ±25% | ±25% (25% reduction floor) |
| `Retry-After` honored | no (Ollama emits no rate-limit headers) | yes |
| Retry on 429 | n/a | yes |
| Retry on 5xx | yes | yes |
| Retry on 4xx | no | no |
| Per-request timeout | 60 s (DefaultTimeout) | 30 s |

The wider per-request timeout reflects local 8B-parameter inference on
cold KV caches; the shorter retry budget reflects that a local daemon
either answers fast or is genuinely down.

## Wire mapping (`POST /api/chat`, `stream:false`)

| Mazarbul field | Wire field | Notes |
|---|---|---|
| `req.Model` (or `c.chatModel`) | `model` | falls back to client default |
| n/a | `stream` | always `false` (M1 non-streaming only) |
| `req.System` (when non-empty) | prepended `{role:"system", content:System}` | |
| `req.Messages[i]` | `messages[i+offset]` | content blocks concatenated into a single string |
| `req.MaxTokens` | `options.num_predict` | |
| `req.Temperature` | `options.temperature` (omitempty) | |
| `req.StopSequences` | `options.stop` (omitempty) | |
| `resp.message.content` | `Content = []ContentBlock{TextBlock(...)}` | |
| `resp.done_reason` | `StopReason` | pass-through string |
| `resp.prompt_eval_count` | `Usage.InputTokens` | |
| `resp.eval_count` | `Usage.OutputTokens` | |
| `resp.total_duration`, `load_duration`, `prompt_eval_duration`, `eval_duration` | discarded | nanoseconds; M2 budget surface |
| `resp.created_at` | discarded | client-synthesized `CreateTime` (UTC) instead |

## Probe behavior

`New(ctx, Options{ChatModel: "llama3.1:8b"})` issues a single
`GET /api/tags` round trip. The response feeds both the embed-model
check (always) and the chat-model check (only when `ChatModel != ""`).
Either missing model wraps `ErrModelMissing` with the exact
`ollama pull <model>` command the user must run.

`Options.ChatModel == ""` disables the chat path entirely: no probe, and
`Complete` returns `"ollama: chat not configured; pass Options.ChatModel
to New"`. This is the default for callers that only need embeddings, so
the daemon stays bootable on hosts that have `bge-m3` but no llama.

## What is out of scope for M1

- Streaming responses. `stream:true` and SSE-ish chunk parsing land in M2.
- Tool / function calling. M3 wires them when the recall composer needs them.
- Per-call duration accounting. `Usage` carries only token counts; the
  wire `*_duration` ns fields are decoded then discarded.
- Pluggable Ollama hosts. `Options.BaseURL` works in tests but the
  default `127.0.0.1:11434` is what M1 contracts on; M3 plugin loader
  generalizes this.
- Prompt caching. Ollama has no equivalent to Anthropic ephemeral
  cache; `ContentBlock` omits `CacheControl`.

## Tests

`go test -race ./internal/provider/ollama/...` covers:

- `chat_test.go`: success (asserts wire fields, Content shape, Usage,
  UTC `CreateTime`), system-prompt prepending, options omit when not
  set, temperature + stop propagation, 5xx retry, retry exhaustion, 4xx
  fast-fail, context cancellation during backoff, request validation
  (nil request, empty messages, non-positive MaxTokens, missing role,
  non-text block), chat-disabled error, `flattenContent` cases.
- `chat_bench_test.go`: `BenchmarkComplete_HTTPRoundtrip` over an
  httptest server returning a constant 32-token response.

## Benchmark baseline (issue #86)

Apple M1 Pro, `httptest.NewServer` (loopback), `mise run bench` on the
implementing branch:

```
BenchmarkComplete_HTTPRoundtrip-8   ~117 µs/op   ~9.4 KB/op   116 allocs/op
```

The marshal / network / decode floor for the wrapper itself; real-Ollama
inference dominates this number by 3-4 orders of magnitude (8B model on
a warm cache: tens of ms per token).

## References (Ollama)

- https://github.com/ollama/ollama/blob/main/docs/api.md, `/api/chat`
  request and response schema (May 2026).
- the original architecture notes, three-category
  taxonomy and provider slot.
- the original architecture notes, where
  token + duration accounting will surface in M2.
- `internal/provider/ollama/client.go`, the shared HTTP / retry / probe
  scaffolding reused by both `Embed` and `Complete`.
