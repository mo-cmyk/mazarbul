package kernel_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func sampleEventWithScope(s kernel.Scope) kernel.Event {
	return kernel.Event{
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      s,
		Payload:    json.RawMessage(`{"text":"hello"}`),
	}
}

func TestFileEventLog_DefaultsEmptyScopeToPersonal(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	receipts, err := w.Append(ctx, []kernel.Event{sampleEvent()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := w.At(ctx, receipts[0].UID)
	if err != nil {
		t.Fatalf("At: %v", err)
	}
	if got.Scope != kernel.ScopePersonal {
		t.Errorf("Scope: got %q, want %q (empty must default to personal)",
			got.Scope, kernel.ScopePersonal)
	}
}

func TestFileEventLog_PreservesExplicitScope(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	// Use a provisioned (non-local) signer: team-scope writes reject the
	// local-default sentinel, but this test exercises scope preservation, not
	// the identity gate.
	priv, _ := fixedTestKey(t)
	w := newTestWriter(t, kernel.WithSigner(
		kernel.NewEd25519Signer(priv, "01PRINCIPAL000000000000001", "01DEVICE0000000000000000001", "kid:test")))

	cases := []struct {
		name  string
		scope kernel.Scope
	}{
		{"project scope", kernel.ScopeProject},
		{"personal scope", kernel.ScopePersonal},
		{"team scope", kernel.ScopeTeam},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipts, err := w.Append(ctx, []kernel.Event{sampleEventWithScope(tc.scope)})
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
			got, err := w.At(ctx, receipts[0].UID)
			if err != nil {
				t.Fatalf("At: %v", err)
			}
			if got.Scope != tc.scope {
				t.Errorf("Scope: got %q, want %q", got.Scope, tc.scope)
			}
		})
	}
}

func TestFileEventLog_RejectsInvalidScope(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	w := newTestWriter(t)

	ev := sampleEventWithScope("organization") // not one of {project,personal,team}
	_, err := w.Append(ctx, []kernel.Event{ev})
	if err == nil {
		t.Fatalf("Append must reject scope=organization")
	}
	if !errors.Is(err, kernel.ErrInvalidEvent) {
		t.Errorf("err = %v, want errors.Is ErrInvalidEvent", err)
	}
}

func TestScope_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    kernel.Scope
		want bool
	}{
		{kernel.ScopeProject, true},
		{kernel.ScopePersonal, true},
		{kernel.ScopeTeam, true},
		{"", false},
		{"organization", false},
		{"PROJECT", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.s), func(t *testing.T) {
			if got := tc.s.Valid(); got != tc.want {
				t.Errorf("Scope(%q).Valid() = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}
