package secret

import (
	"context"
	"errors"
	"testing"
)

func TestCanonicalEnvName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{NameAnthropicAPIKey, "MAZ_SECRET_ANTHROPIC_API_KEY"},
		{NameOpenAIAPIKey, "MAZ_SECRET_OPENAI_API_KEY"},
		{"custom.value", "MAZ_SECRET_CUSTOM_VALUE"},
		{"already_flat", "MAZ_SECRET_ALREADY_FLAT"},
	}
	for _, c := range cases {
		if got := CanonicalEnvName(c.in); got != c.want {
			t.Errorf("CanonicalEnvName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEnv_Get_Canonical exercises the primary MAZ_SECRET_* form.
func TestEnv_Get_Canonical(t *testing.T) {
	t.Setenv("MAZ_SECRET_ANTHROPIC_API_KEY", "sk-canonical")
	t.Setenv("ANTHROPIC_API_KEY", "sk-legacy")

	got, err := NewEnvProvider().Get(context.Background(), NameAnthropicAPIKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-canonical" {
		t.Errorf("got %q, want %q (canonical must beat legacy)", got, "sk-canonical")
	}
}

// TestEnv_Get_LegacyFallback verifies the legacy env var works when only it
// is set.
func TestEnv_Get_LegacyFallback(t *testing.T) {
	t.Setenv("MAZ_SECRET_ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-legacy")

	got, err := NewEnvProvider().Get(context.Background(), NameAnthropicAPIKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-legacy" {
		t.Errorf("got %q, want %q", got, "sk-legacy")
	}
}

// TestEnv_Get_NotFound verifies ErrNotFound when neither form is set.
func TestEnv_Get_NotFound(t *testing.T) {
	t.Setenv("MAZ_SECRET_ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := NewEnvProvider().Get(context.Background(), NameAnthropicAPIKey)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get returned %v, want ErrNotFound", err)
	}
}

// TestEnv_Get_UnknownNameNoLegacy verifies that a name with no legacy
// mapping falls straight through to ErrNotFound when the canonical form is
// unset.
func TestEnv_Get_UnknownNameNoLegacy(t *testing.T) {
	t.Setenv("MAZ_SECRET_NEW_THING", "")

	_, err := NewEnvProvider().Get(context.Background(), "new.thing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get returned %v, want ErrNotFound", err)
	}
}

// TestEnv_SetIsReadOnly verifies Set returns ErrReadOnly.
func TestEnv_SetIsReadOnly(t *testing.T) {
	err := NewEnvProvider().Set(context.Background(), "anything", "value")
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("Set returned %v, want ErrReadOnly", err)
	}
}

// TestEnv_DeleteIsReadOnly verifies Delete returns ErrReadOnly.
func TestEnv_DeleteIsReadOnly(t *testing.T) {
	err := NewEnvProvider().Delete(context.Background(), "anything")
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete returned %v, want ErrReadOnly", err)
	}
}
