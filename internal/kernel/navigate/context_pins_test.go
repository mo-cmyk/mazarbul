package navigate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// seedContextPin inserts one context_pins row.
func seedContextPin(exec func(q string, args ...any), uid, pinType, text, targetRef string, active int, scope string) {
	exec(`INSERT INTO context_pins
		(uid, pin_type, text, target_ref, active, source_event_uid, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?)`,
		uid, pinType, text, targetRef, active, "ev_"+uid, "2026-06-24T09:00:00Z", scope,
	)
}

// TestContextPins exercises the live-pin listing: pins + corrections of the same
// scope surface, retired (active=0) rows are excluded, and the #206 scope
// boundary fails closed.
func TestContextPins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	seedContextPin(exec, "pin_live", "pin", "Do not suggest new Postgres features.", "", 1, "personal")
	seedContextPin(exec, "corr_live", "correction", "Frodo left in 3018.", "entities/frodo", 1, "personal")
	seedContextPin(exec, "pin_retired", "pin", "Old steering note.", "", 0, "personal")
	seedContextPin(exec, "pin_team", "pin", "Team-scope pin.", "", 1, "team")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	t.Run("returns live pins and corrections in scope", func(t *testing.T) {
		t.Parallel()
		personal := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))
		pins, err := personal.ContextPins(ctx)
		if err != nil {
			t.Fatalf("ContextPins: %v", err)
		}
		got := make(map[string]navigate.ContextPin, len(pins))
		for _, p := range pins {
			got[p.UID] = p
		}
		if _, ok := got["pin_live"]; !ok {
			t.Error("expected live pin to surface")
		}
		corr, ok := got["corr_live"]
		if !ok {
			t.Fatal("expected live correction to surface")
		}
		if corr.Type != "correction" {
			t.Errorf("Type = %q, want correction", corr.Type)
		}
		if corr.TargetRef != "entities/frodo" {
			t.Errorf("TargetRef = %q, want entities/frodo", corr.TargetRef)
		}
		if corr.Name != "contextPins/corr_live" {
			t.Errorf("Name = %q, want contextPins/corr_live", corr.Name)
		}
		if !corr.Active {
			t.Error("live correction should report Active=true")
		}
		if _, ok := got["pin_retired"]; ok {
			t.Error("retired pin (active=0) must be excluded from ContextPins")
		}
		if _, ok := got["pin_team"]; ok {
			t.Error("team-scope pin must not leak into a personal-scope read")
		}
	})

	t.Run("empty scope set fails closed", func(t *testing.T) {
		t.Parallel()
		empty := nav.WithScopes(kernel.ScopeSet{})
		pins, err := empty.ContextPins(ctx)
		if err != nil {
			t.Fatalf("ContextPins under empty scope: %v", err)
		}
		if len(pins) != 0 {
			t.Errorf("expected zero pins (fail-closed) under empty scope, got %d", len(pins))
		}
	})
}

// TestGetContextPin exercises single-row fetch: happy path, retired-still-
// fetchable, not-found, and scope fail-closed.
func TestGetContextPin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	seedContextPin(exec, "pin1", "pin", "A pinned note.", "entities/acme", 1, "personal")
	seedContextPin(exec, "pin_retired", "pin", "Retired note.", "", 0, "personal")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	t.Run("happy path fields are populated", func(t *testing.T) {
		t.Parallel()
		p, err := nav.GetContextPin(ctx, "pin1")
		if err != nil {
			t.Fatalf("GetContextPin: %v", err)
		}
		if p.Name != "contextPins/pin1" {
			t.Errorf("Name = %q, want contextPins/pin1", p.Name)
		}
		if p.Text != "A pinned note." {
			t.Errorf("Text = %q", p.Text)
		}
		if p.TargetRef != "entities/acme" {
			t.Errorf("TargetRef = %q, want entities/acme", p.TargetRef)
		}
		if !p.Active {
			t.Error("Active = false, want true")
		}
	})

	t.Run("retired pin is still fetchable", func(t *testing.T) {
		t.Parallel()
		p, err := nav.GetContextPin(ctx, "pin_retired")
		if err != nil {
			t.Fatalf("GetContextPin(retired): %v", err)
		}
		if p.Active {
			t.Error("retired pin should report Active=false")
		}
	})

	t.Run("unknown uid returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		if _, err := nav.GetContextPin(ctx, "missing"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetContextPin(missing): want ErrNotFound, got %v", err)
		}
	})

	t.Run("empty scope set fails closed", func(t *testing.T) {
		t.Parallel()
		empty := nav.WithScopes(kernel.ScopeSet{})
		if _, err := empty.GetContextPin(ctx, "pin1"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetContextPin under empty scope: want ErrNotFound (fail-closed), got %v", err)
		}
	})
}
