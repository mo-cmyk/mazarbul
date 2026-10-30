// Package anthropic is the Mazarbul chat provider for Anthropic's Messages
// API. It is a hand-rolled HTTP client (no vendor SDK) with the small
// surface M1 requires: non-streaming completions, retry on 5xx and 429
// (honoring retry-after), fast-fail on every other 4xx, prompt-cache
// plumbing on content blocks. The Client reads its API key through a
// secret.Provider, so the init wizard, the daemon, and tests all share one
// path to the credential.
//
// Streaming responses, tool/function-calling, and a multi-provider chat
// abstraction are out of scope for M1; see agent-docs/designs/chat-
// provider.md for the design rationale.
package anthropic
