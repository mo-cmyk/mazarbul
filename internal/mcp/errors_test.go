package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestToolError_Error(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *ToolError
		want string
	}{
		{
			name: "code and message",
			err:  &ToolError{Code: CodeValidation, Message: "fact is required"},
			want: "mcp: E_VALIDATION: fact is required",
		},
		{
			name: "code only",
			err:  &ToolError{Code: CodeNotFound},
			want: "mcp: E_NOT_FOUND",
		},
		{
			name: "nil receiver",
			err:  nil,
			want: "<nil>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolError_Unwrap(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("underlying")
	te := &ToolError{Code: CodeInternal, Message: "boom", Cause: sentinel}
	if got := te.Unwrap(); !errors.Is(got, sentinel) {
		t.Fatalf("Unwrap() = %v, want sentinel", got)
	}
	if got := (*ToolError)(nil).Unwrap(); got != nil {
		t.Fatalf("nil receiver Unwrap() = %v, want nil", got)
	}
}

func TestToolError_ErrorsAs(t *testing.T) {
	t.Parallel()
	inner := NewNotFoundError("user 42 not found")
	wrapped := fmt.Errorf("recall: %w", inner)
	var got *ToolError
	if !errors.As(wrapped, &got) {
		t.Fatalf("errors.As did not match *ToolError in %v", wrapped)
	}
	if got.Code != CodeNotFound {
		t.Fatalf("recovered Code = %q, want %q", got.Code, CodeNotFound)
	}
}

func TestConstructors(t *testing.T) {
	t.Parallel()
	t.Run("NewValidationError", func(t *testing.T) {
		t.Parallel()
		details := json.RawMessage(`{"instancePath":"/fact"}`)
		te := NewValidationError("missing fact", details)
		if te.Code != CodeValidation {
			t.Fatalf("Code = %q, want %q", te.Code, CodeValidation)
		}
		if string(te.Details) != string(details) {
			t.Fatalf("Details = %s, want %s", te.Details, details)
		}
	})
	t.Run("NewNotFoundError", func(t *testing.T) {
		t.Parallel()
		te := NewNotFoundError("missing")
		if te.Code != CodeNotFound {
			t.Fatalf("Code = %q, want %q", te.Code, CodeNotFound)
		}
	})
	t.Run("NewInternalError wraps cause", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("disk full")
		te := NewInternalError(cause)
		if te.Code != CodeInternal {
			t.Fatalf("Code = %q, want %q", te.Code, CodeInternal)
		}
		if te.Message != cause.Error() {
			t.Fatalf("Message = %q, want %q", te.Message, cause.Error())
		}
		if !errors.Is(te, cause) {
			t.Fatalf("errors.Is did not match wrapped cause")
		}
	})
	t.Run("NewInternalError handles nil cause", func(t *testing.T) {
		t.Parallel()
		te := NewInternalError(nil)
		if te.Message != "internal error" {
			t.Fatalf("Message = %q, want 'internal error'", te.Message)
		}
	})
	t.Run("NewPermissionError", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("principal not a team member")
		te := NewPermissionError("team scope write denied", cause)
		if te.Code != CodePermission {
			t.Fatalf("Code = %q, want %q", te.Code, CodePermission)
		}
		if te.Error() != "mcp: E_PERMISSION: team scope write denied" {
			t.Fatalf("Error() = %q", te.Error())
		}
		if !errors.Is(te, cause) {
			t.Fatalf("NewPermissionError did not preserve cause via Unwrap")
		}
	})
}

func TestAsToolError(t *testing.T) {
	t.Parallel()
	t.Run("nil returns nil", func(t *testing.T) {
		t.Parallel()
		if got := asToolError(nil); got != nil {
			t.Fatalf("asToolError(nil) = %v, want nil", got)
		}
	})
	t.Run("preserves typed error", func(t *testing.T) {
		t.Parallel()
		original := NewNotFoundError("nope")
		got := asToolError(fmt.Errorf("wrap: %w", original))
		if got.Code != CodeNotFound {
			t.Fatalf("Code = %q, want %q", got.Code, CodeNotFound)
		}
	})
	t.Run("falls back to internal", func(t *testing.T) {
		t.Parallel()
		bare := errors.New("kaboom")
		got := asToolError(bare)
		if got.Code != CodeInternal {
			t.Fatalf("Code = %q, want %q", got.Code, CodeInternal)
		}
		if !errors.Is(got, bare) {
			t.Fatalf("fallback did not preserve cause via Unwrap")
		}
	})
	t.Run("deadline exceeded maps to timeout", func(t *testing.T) {
		t.Parallel()
		got := asToolError(fmt.Errorf("ran long: %w", context.DeadlineExceeded))
		if got.Code != CodeTimeout {
			t.Fatalf("Code = %q, want %q", got.Code, CodeTimeout)
		}
	})
	t.Run("cancellation maps to canceled not timeout", func(t *testing.T) {
		t.Parallel()
		got := asToolError(fmt.Errorf("client went away: %w", context.Canceled))
		if got.Code != CodeCanceled {
			t.Fatalf("Code = %q, want %q (a cancellation is not a timeout)", got.Code, CodeCanceled)
		}
	})
	t.Run("database is locked maps to unavailable", func(t *testing.T) {
		t.Parallel()
		got := asToolError(errors.New("database is locked"))
		if got.Code != CodeUnavailable {
			t.Fatalf("Code = %q, want %q (lock contention is transient)", got.Code, CodeUnavailable)
		}
		if !errors.Is(got, got.Cause) {
			t.Fatal("cause not preserved via Unwrap")
		}
	})
	t.Run("sqlite busy token maps to unavailable", func(t *testing.T) {
		t.Parallel()
		got := asToolError(errors.New("SQLITE_BUSY: cannot obtain lock"))
		if got.Code != CodeUnavailable {
			t.Fatalf("Code = %q, want %q", got.Code, CodeUnavailable)
		}
	})
	t.Run("failed to open database with status 1 maps to unavailable", func(t *testing.T) {
		t.Parallel()
		got := asToolError(fmt.Errorf("ladybug: %w", errors.New("failed to open database with status 1")))
		if got.Code != CodeUnavailable {
			t.Fatalf("Code = %q, want %q", got.Code, CodeUnavailable)
		}
	})
	t.Run("generic error still maps to internal", func(t *testing.T) {
		t.Parallel()
		got := asToolError(errors.New("some unrelated failure"))
		if got.Code != CodeInternal {
			t.Fatalf("Code = %q, want %q", got.Code, CodeInternal)
		}
	})
}

func TestIsTransientLockErr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"nil error", "", false},
		{"database is locked", "database is locked", true},
		{"database is locked mixed case", "Database Is Locked", true},
		{"sqlite_busy lowercase", "sqlite_busy: cannot obtain lock", true},
		{"SQLITE_BUSY uppercase", "SQLITE_BUSY", true},
		{"database table is locked", "database table is locked", true},
		{"failed to open database with status 1", "failed to open database with status 1", true},
		{"wrapped failed to open", "ladybug open: failed to open database with status 1", true},
		{"bare status 1 does not match", "exit status 1", false},
		{"generic error", "disk full", false},
		{"connection refused", "connection refused", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.msg != "" {
				err = errors.New(tc.msg)
			}
			if got := IsTransientLockErr(err); got != tc.want {
				t.Fatalf("IsTransientLockErr(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestNewUnavailableError(t *testing.T) {
	t.Parallel()
	t.Run("default message when empty", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("database is locked")
		te := NewUnavailableError("", cause)
		if te.Code != CodeUnavailable {
			t.Fatalf("Code = %q, want %q", te.Code, CodeUnavailable)
		}
		if te.Message == "" {
			t.Fatal("Message should not be empty when msg arg is empty")
		}
		if !errors.Is(te, cause) {
			t.Fatal("cause not preserved via Unwrap")
		}
	})
	t.Run("custom message is preserved", func(t *testing.T) {
		t.Parallel()
		te := NewUnavailableError("append relation: database is locked", errors.New("database is locked"))
		if te.Message != "append relation: database is locked" {
			t.Fatalf("Message = %q, want custom message", te.Message)
		}
	})
}
