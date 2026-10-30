package contextbrief

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// scopedPresetReader mimics the daemon's presetBriefReader: it fails closed,
// returning its presets only when the read span includes the personal scope.
// It records the scope it was handed so a test can prove OperatingInstructions
// forwards the caller's scope verbatim (rather than defaulting it).
type scopedPresetReader struct {
	presets []ActivePreset
	gotPers bool
}

func (r *scopedPresetReader) ActivePresets(_ context.Context, scopes kernel.ScopeSet) ([]ActivePreset, error) {
	if !scopes.Contains(kernel.ScopePersonal) {
		return nil, nil
	}
	r.gotPers = true
	return r.presets, nil
}

func crmPreset() ActivePreset {
	return ActivePreset{Name: "crm", Version: "0.1.0", Instructions: "File deals under crm:Account."}
}

// TestOperatingInstructions_EmptyScopeFailsClosed asserts an empty/unwired
// ScopeSet yields an empty section — NOT the full set. This proves
// OperatingInstructions does not inherit Build's default-to-{project,personal}
// behavior: the empty set is forwarded to the fail-closed reader (#206/#214).
func TestOperatingInstructions_EmptyScopeFailsClosed(t *testing.T) {
	t.Parallel()
	rd := &scopedPresetReader{presets: []ActivePreset{crmPreset()}}
	a := New(presetTestReader(), nil).WithPresetReader(rd)

	got, err := a.OperatingInstructions(context.Background(), kernel.NewScopeSet())
	if err != nil {
		t.Fatalf("OperatingInstructions: %v", err)
	}
	if got != "" {
		t.Errorf("empty ScopeSet must fail closed (empty), got:\n%s", got)
	}
	if rd.gotPers {
		t.Errorf("reader saw the personal scope for an empty request — scope was defaulted, not forwarded")
	}
}

// TestOperatingInstructions_PersonalScopeRendersFenced asserts a personal-inclusive
// span renders the provenance-fenced preset body.
func TestOperatingInstructions_PersonalScopeRendersFenced(t *testing.T) {
	t.Parallel()
	a := New(presetTestReader(), nil).WithPresetReader(&scopedPresetReader{presets: []ActivePreset{crmPreset()}})

	got, err := a.OperatingInstructions(context.Background(), kernel.NewScopeSet(kernel.ScopePersonal))
	if err != nil {
		t.Fatalf("OperatingInstructions: %v", err)
	}
	if !strings.Contains(got, "From preset: crm") {
		t.Errorf("expected provenance label, got:\n%s", got)
	}
	if !strings.Contains(got, untrustedCaveat) {
		t.Errorf("expected untrusted-content caveat, got:\n%s", got)
	}
	if !strings.Contains(got, "> File deals under crm:Account.") {
		t.Errorf("expected blockquoted preset body, got:\n%s", got)
	}
}

// TestOperatingInstructions_NilReaderEmpty asserts the degraded path (no preset
// reader wired, e.g. #210 absent) yields "" with no error.
func TestOperatingInstructions_NilReaderEmpty(t *testing.T) {
	t.Parallel()
	a := New(presetTestReader(), nil) // no WithPresetReader
	got, err := a.OperatingInstructions(context.Background(), kernel.NewScopeSet(kernel.ScopePersonal))
	if err != nil {
		t.Fatalf("OperatingInstructions: %v", err)
	}
	if got != "" {
		t.Errorf("nil preset reader must yield empty, got:\n%s", got)
	}
}

// TestOperatingInstructions_BudgetClamp asserts the rendered section stays within
// the instructions-sink budget even when several oversized presets are active.
func TestOperatingInstructions_BudgetClamp(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("operating playbook line. ", 200) // ~5 KiB, exceeds maxPresetBodyBytes
	presets := []ActivePreset{
		{Name: "a-pack", Version: "1.0.0", Instructions: big},
		{Name: "b-pack", Version: "1.0.0", Instructions: big},
		{Name: "c-pack", Version: "1.0.0", Instructions: big},
	}
	a := New(presetTestReader(), nil).WithPresetReader(&scopedPresetReader{presets: presets})

	got, err := a.OperatingInstructions(context.Background(), kernel.NewScopeSet(kernel.ScopePersonal))
	if err != nil {
		t.Fatalf("OperatingInstructions: %v", err)
	}
	if tokensFor(got) > operatingInstructionsBudgetTokens {
		t.Errorf("section tokens %d exceed budget %d", tokensFor(got), operatingInstructionsBudgetTokens)
	}
	if got == "" {
		t.Errorf("at least the first preset should render within budget")
	}
}

// TestOperatingInstructions_DropLogged asserts that when the budget cap drops a
// lower-priority preset block the assembler emits a Debug observability line (so
// an operator who installed N presets but sees fewer can diagnose it). The common
// single-preset path must stay silent.
func TestOperatingInstructions_DropLogged(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	big := strings.Repeat("operating playbook line. ", 200) // ~5 KiB, exceeds maxPresetBodyBytes
	presets := []ActivePreset{
		{Name: "a-pack", Version: "1.0.0", Instructions: big},
		{Name: "b-pack", Version: "1.0.0", Instructions: big},
		{Name: "c-pack", Version: "1.0.0", Instructions: big},
	}
	a := New(presetTestReader(), nil).
		WithPresetReader(&scopedPresetReader{presets: presets}).
		WithLogger(logger)

	if _, err := a.OperatingInstructions(context.Background(), kernel.NewScopeSet(kernel.ScopePersonal)); err != nil {
		t.Fatalf("OperatingInstructions: %v", err)
	}
	dropLogs := logs.FilterMessageSnippet("preset block(s) dropped").All()
	if len(dropLogs) == 0 {
		t.Fatalf("expected a drop log when the cap cuts presets; got none of %d entries", logs.Len())
	}
	if dropped, ok := dropLogs[0].ContextMap()["dropped"].(int64); !ok || dropped < 1 {
		t.Errorf("drop log must carry a positive dropped count, got %v", dropLogs[0].ContextMap()["dropped"])
	}
}

// TestOperatingInstructions_NoDropNoLog asserts the single-preset path (nothing
// dropped) emits no observability noise.
func TestOperatingInstructions_NoDropNoLog(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	a := New(presetTestReader(), nil).
		WithPresetReader(&scopedPresetReader{presets: []ActivePreset{crmPreset()}}).
		WithLogger(logger)

	if _, err := a.OperatingInstructions(context.Background(), kernel.NewScopeSet(kernel.ScopePersonal)); err != nil {
		t.Fatalf("OperatingInstructions: %v", err)
	}
	if n := logs.FilterMessageSnippet("preset block(s) dropped").Len(); n != 0 {
		t.Errorf("single preset within budget must not log a drop; got %d", n)
	}
}
