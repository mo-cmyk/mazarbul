package output

import (
	"errors"
	"fmt"
)

// Semantic exit codes (clig.dev-aligned).
const (
	ExitOK                int = 0
	ExitGenericFailure    int = 1
	ExitUsage             int = 2
	ExitDataDirMissing    int = 3
	ExitDaemonUnreachable int = 4
	ExitNotFound          int = 5
	ExitConflict          int = 6
	ExitPermission        int = 7
	ExitValidation        int = 8
	ExitTimeout           int = 124
	ExitInterrupted       int = 130
)

// Error codes (AIP-193). Stable strings used in JSON error envelopes.
const (
	CodeInvalidArgument    = "INVALID_ARGUMENT"
	CodeNotFound           = "NOT_FOUND"
	CodeAlreadyExists      = "ALREADY_EXISTS"
	CodePermissionDenied   = "PERMISSION_DENIED"
	CodeFailedPrecondition = "FAILED_PRECONDITION"
	CodeUnavailable        = "UNAVAILABLE"
	CodeDeadlineExceeded   = "DEADLINE_EXCEEDED"
	CodeAborted            = "ABORTED"
	CodeInternal           = "INTERNAL"
	CodeDataLoss           = "DATA_LOSS"
)

// CodedError carries an exit code + AIP-193 error code alongside the message.
// Returned by [Errorf]. Wraps any cause; unwrappable.
type CodedError struct {
	Exit    int
	Code    string
	Message string
	Cause   error
	Details map[string]any
}

// Error returns the human message.
func (e *CodedError) Error() string {
	if e.Cause != nil && e.Message != "" {
		return e.Message + ": " + e.Cause.Error()
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Message
}

// Unwrap returns the wrapped cause for errors.As / errors.Is.
func (e *CodedError) Unwrap() error { return e.Cause }

// Envelope returns the AIP-193 error envelope shape for JSON output.
func (e *CodedError) Envelope() map[string]any {
	env := map[string]any{
		"code":    e.Code,
		"message": e.Message,
	}
	if e.Cause != nil && e.Message == "" {
		env["message"] = e.Cause.Error()
	}
	if e.Details != nil {
		env["details"] = e.Details
	}
	return map[string]any{"error": env}
}

// Errorf constructs a CodedError with a formatted message.
func Errorf(exit int, code, format string, args ...any) *CodedError {
	return &CodedError{
		Exit:    exit,
		Code:    code,
		Message: fmt.Sprintf(format, args...),
	}
}

// Wrap returns a CodedError wrapping cause. If cause is already a CodedError
// the existing exit/code wins unless overridden by non-zero values.
func Wrap(cause error, exit int, code, message string) *CodedError {
	if cause == nil {
		return nil
	}
	var ce *CodedError
	if errors.As(cause, &ce) {
		out := *ce
		if exit != 0 {
			out.Exit = exit
		}
		if code != "" {
			out.Code = code
		}
		if message != "" {
			out.Message = message
		}
		return &out
	}
	return &CodedError{Exit: exit, Code: code, Message: message, Cause: cause}
}

// ExitFromError returns the exit code embedded in err, or a default of
// ExitGenericFailure when err is non-nil but uncoded, or ExitOK when nil.
func ExitFromError(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *CodedError
	if errors.As(err, &ce) && ce.Exit != 0 {
		return ce.Exit
	}
	return ExitGenericFailure
}
