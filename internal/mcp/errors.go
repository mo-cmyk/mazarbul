package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ToolErrorCode is the stable, machine-readable code attached to a tool
// failure. Codes are E_UPPERCASE_SNAKE per AGENTS.md rule 6, stable
// across versions, and additive: new codes may appear, existing codes
// keep their semantics.
type ToolErrorCode string

const (
	// CodeValidation indicates the request did not satisfy the tool's
	// declared InputSchema. The adapter returns this code before the
	// handler runs.
	CodeValidation ToolErrorCode = "E_VALIDATION"

	// CodeNotFound indicates the handler executed but the target
	// resource does not exist. Typical for recall lookups by id.
	CodeNotFound ToolErrorCode = "E_NOT_FOUND"

	// CodeInternal indicates an unexpected handler failure. The adapter
	// falls back to this code when Tool.Invoke returns a bare error that
	// does not unwrap to a *ToolError.
	CodeInternal ToolErrorCode = "E_INTERNAL"

	// CodePermission indicates the authenticated principal is not
	// authorized for the requested operation — today, a team-scope write
	// by a principal who is not an admitted, non-revoked team member
	// (kernel.ErrPermission). Distinct from CodeValidation: the request is
	// well-formed, the actor simply lacks authority.
	CodePermission ToolErrorCode = "E_PERMISSION"

	// CodeTimeout indicates the handler did not complete within its
	// deadline — the adapter's per-invocation backstop or the caller's own
	// context. The request was well-formed and may succeed on retry; it
	// simply ran too long (a stalled embedder, a saturated chat slot, a
	// stuck lock). Distinct from CodeInternal: nothing is known to be
	// broken, the work just did not finish in time.
	CodeTimeout ToolErrorCode = "E_TIMEOUT"

	// CodeCanceled indicates the request was explicitly cancelled before it
	// completed — the client disconnected, the daemon is shutting down, or a
	// caller invoked cancel(). Unlike CodeTimeout, no deadline elapsed and a
	// blind retry is not the right response: the work was deliberately
	// abandoned, not merely slow.
	CodeCanceled ToolErrorCode = "E_CANCELED"

	// CodeUnavailable indicates the resource is temporarily unavailable due to
	// database lock or busy contention. The request is well-formed and the
	// operation is likely to succeed on retry with backoff. Distinct from
	// CodeInternal (something is broken) and CodeTimeout (a deadline elapsed
	// without lock contention being the cause).
	CodeUnavailable ToolErrorCode = "E_UNAVAILABLE"
)

// ToolError is a tool-returned error carrying a stable code. Tools wrap
// the underlying cause via Cause; callers route on Code. The adapter
// renders ToolError as a CallToolResult{IsError: true} with a structured
// content block {code, message, details}.
//
// Bare errors returned from Tool.Invoke fall back to CodeInternal.
type ToolError struct {
	// Code is the stable error code surfaced to the caller.
	Code ToolErrorCode

	// Message is a human-readable description of the failure. Safe to
	// surface to the calling agent.
	Message string

	// Details optionally carries structured failure metadata, typically
	// an AIP-193 errorInfo-style body. May be nil.
	Details json.RawMessage

	// Cause is the underlying error. Exposed via Unwrap.
	Cause error
}

// Error implements the error interface. The format is
// "mcp: <code>: <message>"; an empty Message collapses to "mcp: <code>".
func (e *ToolError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return fmt.Sprintf("mcp: %s", e.Code)
	}
	return fmt.Sprintf("mcp: %s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause so errors.Is and errors.As can
// reach wrapped sentinels.
func (e *ToolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewValidationError builds a ToolError with CodeValidation. The details
// argument may be nil when no structured metadata is available.
func NewValidationError(msg string, details json.RawMessage) *ToolError {
	return &ToolError{Code: CodeValidation, Message: msg, Details: details}
}

// NewNotFoundError builds a ToolError with CodeNotFound.
func NewNotFoundError(msg string) *ToolError {
	return &ToolError{Code: CodeNotFound, Message: msg}
}

// NewPermissionError builds a ToolError with CodePermission. Cause may be
// nil; when non-nil it is exposed via Unwrap so callers can errors.Is the
// underlying sentinel (e.g. kernel.ErrPermission).
func NewPermissionError(msg string, cause error) *ToolError {
	return &ToolError{Code: CodePermission, Message: msg, Cause: cause}
}

// NewTimeoutError builds a ToolError with CodeTimeout wrapping cause
// (typically context.DeadlineExceeded). Message defaults to a generic
// "tool invocation timed out" when empty.
func NewTimeoutError(msg string, cause error) *ToolError {
	if msg == "" {
		msg = "tool invocation timed out"
	}
	return &ToolError{Code: CodeTimeout, Message: msg, Cause: cause}
}

// NewCanceledError builds a ToolError with CodeCanceled wrapping cause
// (typically context.Canceled). Message defaults to a generic
// "tool invocation canceled" when empty.
func NewCanceledError(msg string, cause error) *ToolError {
	if msg == "" {
		msg = "tool invocation canceled"
	}
	return &ToolError{Code: CodeCanceled, Message: msg, Cause: cause}
}

// NewUnavailableError builds a ToolError with CodeUnavailable wrapping cause
// (typically a SQLite lock/busy contention error). Message defaults to a
// generic "resource temporarily unavailable (locked/busy); retry with backoff"
// when empty.
func NewUnavailableError(msg string, cause error) *ToolError {
	if msg == "" {
		msg = "resource temporarily unavailable (locked/busy); retry with backoff"
	}
	return &ToolError{Code: CodeUnavailable, Message: msg, Cause: cause}
}

// IsTransientLockErr reports whether err is a transient database lock or busy
// contention error that a caller may retry with backoff. It matches the opaque
// messages emitted by LadybugDB ("failed to open database with status 1") and
// standard SQLite busy/locked signals ("database is locked", "SQLITE_BUSY",
// "database table is locked"). The check is conservative: only unambiguous
// lock/busy signals match; generic "status 1" alone does not.
func IsTransientLockErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "failed to open database with status 1")
}

// NewInternalError builds a ToolError with CodeInternal wrapping cause.
// The Message defaults to cause.Error() when cause is non-nil; a nil
// cause produces the literal message "internal error".
func NewInternalError(cause error) *ToolError {
	msg := "internal error"
	if cause != nil {
		msg = cause.Error()
	}
	return &ToolError{Code: CodeInternal, Message: msg, Cause: cause}
}

// asToolError converts err into a *ToolError. If err already wraps a
// *ToolError it is returned unchanged; otherwise a CodeInternal fallback
// is constructed so every handler error has a stable code on the wire.
func asToolError(err error) *ToolError {
	if err == nil {
		return nil
	}
	var te *ToolError
	if errors.As(err, &te) {
		return te
	}
	// A bare deadline expiry is a timeout, not an internal fault: the tool
	// was well-formed and simply ran past its deadline (the adapter's
	// backstop or the caller's own context). Map it to E_TIMEOUT so callers
	// can distinguish "ran too long, retry may help" from "something broke".
	if errors.Is(err, context.DeadlineExceeded) {
		return NewTimeoutError(err.Error(), err)
	}
	// An explicit cancellation (client disconnect, daemon shutdown, cancel())
	// is neither a timeout nor a broken handler: no deadline elapsed and a
	// blind retry is not the right response. Surface it as E_CANCELED.
	if errors.Is(err, context.Canceled) {
		return NewCanceledError(err.Error(), err)
	}
	// A database lock or busy contention error is transient, not a broken
	// handler. Surface it as E_UNAVAILABLE so callers know retry-with-backoff
	// is the right response rather than treating it as a hard failure.
	if IsTransientLockErr(err) {
		return NewUnavailableError("", err)
	}
	return NewInternalError(err)
}
