// Package storage holds storage-layer constants and helpers shared across the
// kernel and provider packages. The package contents stay minimal on purpose:
// it is imported by both the kernel (event log writers, vector index) and by
// providers (the embedding client), so anything that depends on a richer
// kernel surface lives in the parent internal/kernel package instead.
package storage

// VectorDim is the dense embedding dimension used by the M1 default embed
// provider (BGE-M3 via Ollama). The vector index (sqlite-vec) and every
// embedding call must produce vectors of this exact length; embeddings of any
// other size are rejected at the boundary so a model swap cannot silently
// corrupt the index.
//
// BGE-M3 emits 1024-float dense vectors. If a future M3 swap brings in a
// different default model, this constant moves with it and every consumer
// recompiles against the new length, surfacing the change at build time.
const VectorDim = 1024
