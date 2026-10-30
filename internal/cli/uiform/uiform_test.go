package uiform_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
)

func TestTheme(t *testing.T) {
	t.Run("theme is non-nil", func(t *testing.T) {
		th := uiform.Theme()
		if th == nil {
			t.Fatal("Theme() returned nil")
		}
	})

	// huh renders a text input's prompt as promptStyle.Render(textinput.Prompt),
	// where the bubbles textinput already carries the "> " sigil. A theme that
	// ALSO SetString("> ") on Focused.TextInput.Prompt renders the sigil twice
	// ("> > "), which is the doubled prompt users saw on `maz init`. The prompt
	// style must only colour the sigil, never inject a second one.
	t.Run("text-input prompt sigil is not doubled", func(t *testing.T) {
		styles := uiform.Theme().Theme(true) // dark profile
		got := ansi.Strip(styles.Focused.TextInput.Prompt.Render("> "))
		if n := strings.Count(got, ">"); n != 1 {
			t.Fatalf("text-input prompt sigil should appear once, got %q (count=%d)", got, n)
		}
	})
}

func TestHandleFormCancellation(t *testing.T) {
	t.Run("abort error maps to clean cancellation", func(t *testing.T) {
		var buf bytes.Buffer
		err := uiform.HandleFormCancellation(&buf, "delete", huh.ErrUserAborted)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if !strings.Contains(buf.String(), "delete cancelled.") {
			t.Fatalf("expected 'delete cancelled.' in output, got %q", buf.String())
		}
	})

	t.Run("context cancel maps to clean cancellation", func(t *testing.T) {
		var buf bytes.Buffer
		err := uiform.HandleFormCancellation(&buf, "delete", context.Canceled)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if !strings.Contains(buf.String(), "delete cancelled.") {
			t.Fatalf("expected 'delete cancelled.' in output, got %q", buf.String())
		}
	})

	t.Run("other error is wrapped with action", func(t *testing.T) {
		var buf bytes.Buffer
		err := uiform.HandleFormCancellation(&buf, "delete", errors.New("boom"))
		if err == nil {
			t.Fatal("expected non-nil error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "delete") {
			t.Errorf("error message %q does not contain 'delete'", msg)
		}
		if !strings.Contains(msg, "boom") {
			t.Errorf("error message %q does not contain 'boom'", msg)
		}
	})
}

func TestPromptYN(t *testing.T) {
	t.Run("prompt yn returns default when non-interactive", func(t *testing.T) {
		var buf bytes.Buffer

		got, err := uiform.PromptYN(&buf, "ok?", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Error("expected true (def=true), got false")
		}

		got, err = uiform.PromptYN(&buf, "ok?", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Error("expected false (def=false), got true")
		}
	})
}

func TestNew(t *testing.T) {
	t.Run("accessible env builds a form", func(t *testing.T) {
		t.Setenv("ACCESSIBLE", "1")
		f := uiform.New(huh.NewGroup(huh.NewConfirm()))
		if f == nil {
			t.Fatal("New() returned nil")
		}
	})
}
