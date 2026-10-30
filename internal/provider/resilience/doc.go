// Package resilience provides circuit-breaker and ordered-fallback primitives
// for guarding calls to external providers against cascading failures.
//
// CircuitBreaker tracks consecutive failures and trips open after a
// configurable threshold, then probes for recovery after a cooldown period.
// Chain composes multiple breaker-guarded attempts into a single call that
// falls back transparently within the same invocation.
package resilience
