// Package prompts provides a versioned prompt-template registry with
// audit logging. Templates are Go text/template bodies stored in
// memory; each Render emits a prompt.rendered event to the kernel event
// log for audit purposes. The registry is transport-agnostic and is
// safe for concurrent use.
package prompts
