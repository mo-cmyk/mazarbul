package config_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// TestOntologyTable_Governed_Permissive verifies Governed returns false when
// Mode is empty (the default permissive state).
func TestOntologyTable_Governed_Permissive(t *testing.T) {
	t.Parallel()
	o := config.OntologyTable{}
	if o.Governed() {
		t.Error("Governed() = true, want false for empty mode")
	}
}

// TestOntologyTable_Governed_GovernedMode verifies Governed returns true when
// Mode is exactly "governed".
func TestOntologyTable_Governed_GovernedMode(t *testing.T) {
	t.Parallel()
	o := config.OntologyTable{Mode: "governed"}
	if !o.Governed() {
		t.Error("Governed() = false, want true for mode=\"governed\"")
	}
}

// TestOntologyTable_Governed_OtherMode verifies Governed returns false for any
// Mode string that is not "governed", e.g. a hypothetical future mode.
func TestOntologyTable_Governed_OtherMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"permissive", "strict", "Governed", "GOVERNED"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			o := config.OntologyTable{Mode: mode}
			if o.Governed() {
				t.Errorf("Governed() = true for mode=%q, want false", mode)
			}
		})
	}
}

// TestOntologyTable_ModeRoundtrip verifies that a UserConfig carrying ontology
// Mode + dedup thresholds survives a Write/Load round-trip with all three
// fields intact.
func TestOntologyTable_ModeRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg := sampleConfig()
	cfg.Ontology = config.OntologyTable{
		Mode:                 "governed",
		DedupHighThreshold:   0.92,
		DedupMediumThreshold: 0.75,
	}

	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}

	if got.Ontology.Mode != "governed" {
		t.Errorf("Mode = %q, want governed", got.Ontology.Mode)
	}
	if got.Ontology.DedupHighThreshold != 0.92 {
		t.Errorf("DedupHighThreshold = %v, want 0.92", got.Ontology.DedupHighThreshold)
	}
	if got.Ontology.DedupMediumThreshold != 0.75 {
		t.Errorf("DedupMediumThreshold = %v, want 0.75", got.Ontology.DedupMediumThreshold)
	}
	if !got.Ontology.Governed() {
		t.Error("Governed() = false after roundtrip, want true")
	}
}

// TestOntologyTable_ZeroValuePermissive verifies that an OntologyTable with
// zero thresholds and empty Mode round-trips cleanly (all zero/false) and that
// Governed returns false.
func TestOntologyTable_ZeroValuePermissive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cfg := sampleConfig()
	// Ontology field is zero — omitempty must omit it cleanly.
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}

	got, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.Ontology.Mode != "" {
		t.Errorf("Mode = %q, want empty", got.Ontology.Mode)
	}
	if got.Ontology.Governed() {
		t.Error("Governed() = true for default config, want false")
	}
}
