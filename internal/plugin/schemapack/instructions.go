package schemapack

import (
	"errors"
	"fmt"
	"io/fs"
)

// InstructionsFile is the optional filename inside a pack whose presence
// promotes the pack to a preset (#210). It holds the verbatim agent-steering
// module that the getContext brief surfaces as an "Operating instructions"
// section.
const InstructionsFile = "instructions.md"

// MaxInstructionsBytes is the HARD parse-time cap on instructions.md, measured
// in raw UTF-8 bytes. It is roughly half the getContext default brief budget
// (4000 tokens ≈ 16 KiB at the brief's 4-bytes/token heuristic): large enough
// for a real operating playbook, small enough that one preset can never
// monopolize the brief alongside the pinned/recent/entity sections. The cap is
// counted in bytes — not tokens — so it is deterministic and needs no tokenizer
// at parse time.
const MaxInstructionsBytes = 8192

// ErrInstructionsTooLarge is returned (loudly, at parse time) when
// instructions.md exceeds MaxInstructionsBytes. Rejecting at parse — rather than
// truncating, or letting the brief evict the overflow later — keeps the failure
// visible to the author at install rather than silently degrading agent steering.
var ErrInstructionsTooLarge = errors.New("schemapack: instructions.md exceeds size cap")

// ErrInstructionsControlChars is returned when instructions.md contains a C0
// control byte other than tab/newline/carriage-return, or DEL. A markdown
// steering body has no use for raw control bytes, and they are an injection
// vector once the body is remotely sourced (#224): a NUL can truncate the body
// at a C-string boundary downstream, and an ESC (0x1B) opens an ANSI escape
// sequence that could rewrite the terminal during the interactive install review
// and spoof the human-confirm gate. Rejecting at parse keeps the bytes that reach
// the review block, the getContext "Operating instructions" section, and the
// untrusted-data fence as plain text.
var ErrInstructionsControlChars = errors.New("schemapack: instructions.md contains control characters")

// parseInstructions reads instructions.md when present, enforcing the hard byte
// cap. A missing file yields "" (the pack is a plain schema-pack, not a preset).
// An over-cap file is rejected with ErrInstructionsTooLarge naming the byte
// count and cap so the author can trim it.
func parseInstructions(fsys fs.FS, instrPath string) (string, error) {
	data, err := fs.ReadFile(fsys, instrPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("schemapack: read instructions.md: %w", err)
	}
	if len(data) > MaxInstructionsBytes {
		return "", fmt.Errorf("%w: %d bytes (cap %d); split or trim the preset's instructions.md",
			ErrInstructionsTooLarge, len(data), MaxInstructionsBytes)
	}
	if off, b, bad := firstControlByte(data); bad {
		return "", fmt.Errorf("%w: byte 0x%02x at offset %d", ErrInstructionsControlChars, b, off)
	}
	return string(data), nil
}

// firstControlByte reports the offset and value of the first disallowed control
// byte in data. Tab (0x09), newline (0x0A), and carriage return (0x0D) are
// permitted; every other byte below 0x20, plus DEL (0x7F), is disallowed. The
// scan is over raw bytes, so multi-byte UTF-8 runes (lead/continuation bytes are
// all ≥0x80) are never misflagged.
func firstControlByte(data []byte) (int, byte, bool) {
	for i, b := range data {
		switch b {
		case '\t', '\n', '\r':
			continue
		}
		if b < 0x20 || b == 0x7f {
			return i, b, true
		}
	}
	return 0, 0, false
}
