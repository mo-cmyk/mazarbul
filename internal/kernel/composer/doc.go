// Package composer assembles grounded answers from recall hits via
// ChatProvider. It is the last step in the memory-layer read pipeline:
//
//	HybridQuery.Recall -> Composer.Compose -> Answer
//
// The composer renders a numbered context block from the top-N hits,
// calls ChatProvider.Complete with the compose-v1 system prompt, and
// returns a structured Answer with inline [N]-style citations.
//
// The compose-v1 prompt template must be registered with the prompts.Registry
// before the first Compose call. Call RegisterComposePrompt at process startup
// to install it. The same (Name, Version, Body) triple is idempotent.
package composer
