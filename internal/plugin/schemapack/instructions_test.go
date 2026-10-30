package schemapack_test

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// withInstructions returns validPackFS plus an instructions.md of the given body.
func withInstructions(body string) fstest.MapFS {
	fsys := validPackFS()
	fsys["pack/"+schemapack.InstructionsFile] = &fstest.MapFile{Data: []byte(body)}
	return fsys
}

func TestParseFS_WithInstructions_IsPreset(t *testing.T) {
	t.Parallel()
	body := "# CRM operating guide\nAlways file deals under crm:Account.\n"
	pack, err := schemapack.ParseFS(withInstructions(body), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	if !pack.IsPreset() {
		t.Fatal("IsPreset() = false, want true for a pack carrying instructions.md")
	}
	if pack.Instructions != body {
		t.Errorf("Instructions = %q, want verbatim %q", pack.Instructions, body)
	}
}

func TestParseFS_NoInstructions_NotPreset(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.ParseFS(validPackFS(), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	if pack.IsPreset() {
		t.Error("IsPreset() = true, want false for a pack with no instructions.md")
	}
	if pack.Instructions != "" {
		t.Errorf("Instructions = %q, want empty", pack.Instructions)
	}
}

func TestParseFS_InstructionsAtCap_Accepted(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("a", schemapack.MaxInstructionsBytes)
	pack, err := schemapack.ParseFS(withInstructions(body), "pack")
	if err != nil {
		t.Fatalf("ParseFS at exactly the cap: %v", err)
	}
	if len(pack.Instructions) != schemapack.MaxInstructionsBytes {
		t.Errorf("Instructions length = %d, want %d", len(pack.Instructions), schemapack.MaxInstructionsBytes)
	}
}

func TestParseFS_InstructionsOverCap_Rejected(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("a", schemapack.MaxInstructionsBytes+1)
	_, err := schemapack.ParseFS(withInstructions(body), "pack")
	if !errors.Is(err, schemapack.ErrInstructionsTooLarge) {
		t.Fatalf("ParseFS over cap: err = %v, want ErrInstructionsTooLarge", err)
	}
	// The error must name the byte count and the cap so the author can trim.
	if !strings.Contains(err.Error(), "8193") || !strings.Contains(err.Error(), "8192") {
		t.Errorf("error %q should name the actual size (8193) and cap (8192)", err.Error())
	}
}

// TestParseFS_InstructionsCapCountsBytesNotRunes guards that a body just over
// the byte cap built from multibyte runes is still rejected — the cap is on raw
// bytes, deterministically, not on rune count.
func TestParseFS_InstructionsCapCountsBytesNotRunes(t *testing.T) {
	t.Parallel()
	// "世" is 3 bytes in UTF-8. ceil(8193/3) runes exceeds the byte cap.
	runes := (schemapack.MaxInstructionsBytes + 3) / 3
	body := strings.Repeat("世", runes)
	if len(body) <= schemapack.MaxInstructionsBytes {
		t.Fatalf("test setup: body is %d bytes, want > %d", len(body), schemapack.MaxInstructionsBytes)
	}
	if _, err := schemapack.ParseFS(withInstructions(body), "pack"); !errors.Is(err, schemapack.ErrInstructionsTooLarge) {
		t.Fatalf("multibyte over-cap body: err = %v, want ErrInstructionsTooLarge", err)
	}
}

// TestParseFS_InstructionsControlChars_Rejected guards that a steering body
// carrying a raw C0 control byte is refused at parse — a NUL (truncation vector)
// and an ESC (ANSI-escape / terminal-spoofing vector) must never reach the store,
// the install review block, or the getContext operating-instructions section.
func TestParseFS_InstructionsControlChars_Rejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"NUL byte", "# guide\nlegit text\x00hidden"},
		{"ESC byte", "# guide\n\x1b[2J\x1b[Hspoofed prompt"},
		{"bell", "ring\x07"},
		{"DEL", "trailing\x7f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := schemapack.ParseFS(withInstructions(tc.body), "pack")
			if !errors.Is(err, schemapack.ErrInstructionsControlChars) {
				t.Fatalf("ParseFS(%q): err = %v, want ErrInstructionsControlChars", tc.name, err)
			}
		})
	}
}

// TestParseFS_InstructionsAllowsTabsAndNewlines confirms the control-byte guard
// does not reject the whitespace a real markdown body legitimately contains.
func TestParseFS_InstructionsAllowsTabsAndNewlines(t *testing.T) {
	t.Parallel()
	body := "# guide\r\n\t- bullet one\n\t- bullet two\n"
	pack, err := schemapack.ParseFS(withInstructions(body), "pack")
	if err != nil {
		t.Fatalf("ParseFS with tabs/CRLF: %v", err)
	}
	if pack.Instructions != body {
		t.Errorf("Instructions = %q, want verbatim %q", pack.Instructions, body)
	}
}

// TestBuildEvents_NoAppend confirms BuildEvents returns the seeding events and a
// result without appending, so a caller can batch them with a presetInstalled
// event atomically.
func TestBuildEvents_NoAppend(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.ParseFS(validPackFS(), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	log := &recordingLog{}
	inst := &schemapack.Installer{Log: log}
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

	events, result, err := inst.BuildEvents(pack, now)
	if err != nil {
		t.Fatalf("BuildEvents: %v", err)
	}
	if len(log.appended) != 0 {
		t.Errorf("BuildEvents appended %d events, want 0 (build only)", len(log.appended))
	}
	if len(events) != len(pack.Kinds)+len(pack.Predicates) {
		t.Errorf("events = %d, want %d (kinds+predicates)", len(events), len(pack.Kinds)+len(pack.Predicates))
	}
	if len(result.KindsRegistered) != 1 || len(result.PredicatesRegistered) != 1 {
		t.Errorf("result = %+v, want 1 kind + 1 predicate", result)
	}
	if events[0].Kind != kernel.EventKind(ontology.KindEntityKindRegistered) {
		t.Errorf("first event kind = %q, want %q", events[0].Kind, ontology.KindEntityKindRegistered)
	}
}
