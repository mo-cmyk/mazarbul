package privacy_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
)

// presetEvent builds a presetInstalled event at the given scope carrying a
// verbatim instructions body.
func presetEvent(scope kernel.Scope) kernel.Event {
	payload, _ := json.Marshal(ontology.PresetInstalledEvent{
		Name:         "crm",
		Version:      "0.1.0",
		Description:  "CRM vocab.",
		Instructions: "verbatim steering body",
		AddedAt:      time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	})
	return kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindPresetInstalled),
		Source:     "preset:crm",
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	}
}

func TestPresetInstructionsMarker_ScopeAware(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   kernel.Event
		want bool
	}{
		{"personal presetInstalled is not sensitive", presetEvent(kernel.ScopePersonal), false},
		{"project presetInstalled is sensitive", presetEvent(kernel.ScopeProject), true},
		{"team presetInstalled is sensitive", presetEvent(kernel.ScopeTeam), true},
		{"empty-scope presetInstalled is sensitive (fail-closed)", presetEvent(kernel.Scope("")), true},
		{"non-preset event is not sensitive", kernel.Event{
			UID: ulid.Make().String(), Kind: kernel.EventKind(ontology.KindEpisode),
			Scope: kernel.ScopeProject, Payload: []byte(`{}`),
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := privacy.IsSensitivePayload(tc.ev); got != tc.want {
				t.Errorf("IsSensitivePayload = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPresetInstructionsMarker_Registered(t *testing.T) {
	t.Parallel()
	if !slices.Contains(privacy.MarkerNames(), "preset.instructions") {
		t.Errorf("MarkerNames() = %v, want it to include preset.instructions", privacy.MarkerNames())
	}
}

// TestPresetSeal_PersonalPlaintextSeals confirms a presetInstalled event seals
// fine into a personal plaintext chunk — the scope-aware policy keeps default
// personal plaintext sync working.
func TestPresetSeal_PersonalPlaintextSeals(t *testing.T) {
	t.Parallel()
	_, _, err := chunk.EncodeWithOpts(
		[]kernel.Event{presetEvent(kernel.ScopePersonal)},
		chunk.EncodeOpts{Scope: kernel.ScopePersonal}, // nil ScopeKey → plaintext
	)
	if err != nil {
		t.Fatalf("personal plaintext seal of a preset: %v, want success", err)
	}
}

// TestPresetSeal_TeamPlaintextRefused confirms the fail-closed backstop on the
// shared-but-private team scope: a presetInstalled event bound for a team
// plaintext chunk is refused so the verbatim body never reaches a surface other
// members can read. (Project — the deliberately public, unencryptable in-repo
// scope — is exempt from the seal gate by design; presets are never
// project-scoped, and render-exclusion keeps the body out of the rendered tree.)
func TestPresetSeal_TeamPlaintextRefused(t *testing.T) {
	t.Parallel()
	_, _, err := chunk.EncodeWithOpts(
		[]kernel.Event{presetEvent(kernel.ScopeTeam)},
		chunk.EncodeOpts{Scope: kernel.ScopeTeam},
	)
	if !errors.Is(err, chunk.ErrSensitivePayloadPlaintext) {
		t.Fatalf("team plaintext seal of a preset: err = %v, want ErrSensitivePayloadPlaintext", err)
	}
}

// TestPresetSeal_TeamEncryptedSeals confirms the body may travel on a shared
// scope when encrypted — only the plaintext path is refused.
func TestPresetSeal_TeamEncryptedSeals(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _, err := chunk.EncodeWithOpts(
		[]kernel.Event{presetEvent(kernel.ScopeTeam)},
		chunk.EncodeOpts{Scope: kernel.ScopeTeam, ScopeKey: key},
	)
	if err != nil {
		t.Fatalf("team encrypted seal of a preset: %v, want success", err)
	}
}
