// Package tools holds the kernel-coupled MCP tool implementations the
// server hosts: remember, recall, forget, relate, traverse, review. Each
// tool implements mcp.Tool by wrapping a kernel surface (event log,
// query composer, ontology) so the mcp package itself stays free of
// kernel imports.
//
// Tools register through mcp.Server.Register from a wiring helper that
// the daemon supervisor and the foreground `maz mcp serve` CLI share.
// The wiring owns construction order; tools own their JSON Schema and
// the projection from kernel types to MCP response shape.
package tools
