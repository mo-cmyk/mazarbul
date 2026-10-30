// Package output owns CLI rendering and exit-code semantics for maz.
//
// Every command resolves a [Writer] from [From](cmd) and emits records
// through it. Format resolution honors --output, MAZ_OUTPUT, NO_COLOR,
// and TTY state. Records that satisfy [TextRenderer] render natively in
// text mode; in JSON / NDJSON mode any json.Marshalable value works.
//
// Streams (--follow, --watch) use [Writer.EmitStream]: text mode appends
// a line, JSON mode promotes to NDJSON automatically.
//
// Exit codes follow a semantic taxonomy (see exit.go). Use [Errorf] to
// construct an error that carries an exit code and AIP-193 error envelope.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Format identifies the active output mode.
type Format int

const (
	// FormatText is human-readable text on a TTY.
	FormatText Format = iota
	// FormatJSON is a single, pretty-printed JSON document per command.
	FormatJSON
	// FormatNDJSON streams one JSON object per line.
	FormatNDJSON
	// FormatCSV emits a comma-separated header + one row per record. Columns
	// are the record's sorted top-level keys; nested values become compact
	// JSON cells. For spreadsheets and quick `cut`/`awk` pipelines.
	FormatCSV
	// FormatKV emits "key: value" lines per record, a blank line between
	// records. The most grep-friendly shape for eyeballing one record.
	FormatKV
)

// Canonical --output flag values, kept in one place so [Format.String] and
// [ParseFormat] agree on the wire names.
const (
	formatNameText   = "text"
	formatNameJSON   = "json"
	formatNameNDJSON = "ndjson"
	formatNameCSV    = "csv"
	formatNameKV     = "kv"
)

// String returns the canonical flag value for f.
func (f Format) String() string {
	switch f {
	case FormatJSON:
		return formatNameJSON
	case FormatNDJSON:
		return formatNameNDJSON
	case FormatCSV:
		return formatNameCSV
	case FormatKV:
		return formatNameKV
	default:
		return formatNameText
	}
}

// ParseFormat parses a canonical format name. "table" is an alias for text (the
// human table view), "jsonl" an alias for ndjson.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", formatNameText, "auto", "table":
		return FormatText, nil
	case formatNameJSON:
		return FormatJSON, nil
	case formatNameNDJSON, "jsonl":
		return FormatNDJSON, nil
	case formatNameCSV:
		return FormatCSV, nil
	case formatNameKV:
		return FormatKV, nil
	default:
		return FormatText, fmt.Errorf("unknown output format %q (want table|text|json|jsonl|ndjson|csv|kv)", s)
	}
}

// configDefaultFormat is the format injected from the [output] config table
// via SetDefaultFormat. It sits between MAZ_OUTPUT and the TTY heuristic in
// the precedence chain: flag > env > config > TTY. A zero value means "use the
// TTY heuristic".
//
//nolint:gochecknoglobals // guarded process-level default; set once in PersistentPreRun before any command reads it
var configDefaultFormat Format

// configDefaultFormatSet tracks whether SetDefaultFormat has been called with
// a non-text value so resolveFormat can distinguish "not set" from "set to text".
//
//nolint:gochecknoglobals // companion bool for configDefaultFormat; same lifecycle
var configDefaultFormatSet bool

// SetDefaultFormat injects the [output].default_format value from the resolved
// config. Call once from root.go's PersistentPreRun after config load; it is
// safe for concurrent reads after that point because PersistentPreRun always
// completes before the command's RunE executes.
func SetDefaultFormat(f Format) {
	configDefaultFormat = f
	configDefaultFormatSet = true
}

// TextRenderer is implemented by records that know how to print themselves
// as human text. Renderers should write to w without trailing newlines —
// the [Writer] appends a newline between records as appropriate.
type TextRenderer interface {
	RenderText(w io.Writer) error
}

// Writer is the rendering surface every command writes through.
type Writer interface {
	// Emit prints a single record. In text mode the record must
	// satisfy TextRenderer or fmt.Stringer; otherwise it is rendered
	// via %+v. In JSON modes the record is json.Marshal'd.
	Emit(record any) error
	// EmitStream prints a streaming record. In text mode this writes
	// a line per call; in JSON mode it emits NDJSON regardless of the
	// configured format so that long-running streams are agent-safe.
	EmitStream(record any) error
	// Header prints a human header line. In JSON modes it routes to
	// stderr so JSON stdout stays parseable.
	Header(text string)
	// Footer prints a closing line (e.g. "12 episodes, --limit reached").
	// Same stderr-in-JSON-mode rule as Header.
	Footer(text string)
	// Format reports the configured format.
	Format() Format
	// IsTTY reports whether stdout is connected to a terminal.
	IsTTY() bool
	// Flush flushes any buffered output.
	Flush() error
}

// From resolves a Writer for cmd. Resolution order:
//
//  1. --output flag (table|text|json|jsonl|ndjson|csv|kv)
//  2. MAZ_OUTPUT env var
//  3. legacy --json flag => FormatJSON (kept while sweeping)
//  4. stdout TTY => FormatText; pipe => FormatJSON
//
//nolint:ireturn // returns the Writer interface; concrete type is format-dependent and callers must not care
func From(cmd *cobra.Command) Writer {
	w, _ := FromCmd(cmd) //nolint:errcheck // soft-fail variant; callers wanting validation use FromCmd directly
	return w
}

// FromCmd is the error-returning variant of From. Commands that want
// to fail-fast on an invalid --output / MAZ_OUTPUT should call this
// and propagate the error. From keeps the soft-fail signature for
// callers that compose Writer construction into existing flows.
//
//nolint:ireturn // returns the Writer interface; concrete type is format-dependent and callers must not care
func FromCmd(cmd *cobra.Command) (Writer, error) {
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()
	isTTY := stdoutIsTTY(stdout)
	format, err := resolveFormat(cmd, isTTY)
	return newWriter(format, isTTY, stdout, stderr), err
}

//nolint:ireturn // returns the Writer interface; concrete type selected at runtime by format
func newWriter(format Format, isTTY bool, stdout, stderr io.Writer) Writer {
	switch format {
	case FormatJSON:
		return &jsonWriter{stdout: stdout, stderr: stderr, isTTY: isTTY}
	case FormatNDJSON:
		return &ndjsonWriter{stdout: stdout, stderr: stderr, isTTY: isTTY}
	case FormatCSV:
		return &csvWriter{stdout: stdout, stderr: stderr, isTTY: isTTY}
	case FormatKV:
		return &kvWriter{stdout: stdout, stderr: stderr, isTTY: isTTY}
	default:
		return &textWriter{stdout: stdout, stderr: stderr, isTTY: isTTY}
	}
}

// New returns a Writer using explicit options (mostly for tests).
//
//nolint:ireturn // returns the Writer interface; concrete type selected at runtime by format
func New(format Format, stdout, stderr io.Writer) Writer {
	return newWriter(format, false, stdout, stderr)
}

// resolveFormat picks the Format from --output, MAZ_OUTPUT, config default,
// and TTY state. Precedence (highest first):
//
//  1. --output flag
//  2. MAZ_OUTPUT env var
//  3. [output].default_format from config (injected via SetDefaultFormat)
//  4. TTY heuristic: text on TTY, json off TTY
//
// An explicit value that fails to parse is reported via the returned error —
// silently falling back to text/json hid script typos like `--output jsn`.
func resolveFormat(cmd *cobra.Command, isTTY bool) (Format, error) {
	if cmd != nil {
		// Walk through this command AND any inherited persistent
		// flags so `--output ndjson` works whether placed before or
		// after the subcommand path.
		if explicit, _ := cmd.Flags().GetString("output"); explicit != "" {
			f, err := ParseFormat(explicit)
			if err != nil {
				return FormatText, fmt.Errorf("--output %q: %w", explicit, err)
			}
			return f, nil
		}
		// Legacy --json flag — kept transitionally during the sweep.
		if legacy, _ := cmd.Flags().GetBool("json"); legacy {
			return FormatJSON, nil
		}
	}
	if env := strings.TrimSpace(os.Getenv("MAZ_OUTPUT")); env != "" {
		f, err := ParseFormat(env)
		if err != nil {
			return FormatText, fmt.Errorf("MAZ_OUTPUT=%q: %w", env, err)
		}
		return f, nil
	}
	// Config default (injected from [output].default_format via SetDefaultFormat).
	if configDefaultFormatSet {
		return configDefaultFormat, nil
	}
	if !isTTY {
		return FormatJSON, nil
	}
	return FormatText, nil
}

func stdoutIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// renderTextRecord writes record to w using TextRenderer or a best-effort fallback.
func renderTextRecord(w io.Writer, record any) error {
	if record == nil {
		return nil
	}
	switch r := record.(type) {
	case TextRenderer:
		return r.RenderText(w)
	case fmt.Stringer:
		_, err := fmt.Fprintln(w, r.String())
		return err
	case string:
		_, err := fmt.Fprintln(w, r)
		return err
	case []byte:
		_, err := w.Write(r)
		return err
	default:
		_, err := fmt.Fprintf(w, "%+v\n", record)
		return err
	}
}

// marshalJSON returns the indented JSON encoding for record.
func marshalJSON(record any) ([]byte, error) {
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal json: %w", err)
	}
	return append(b, '\n'), nil
}

// marshalNDJSON returns a single-line JSON encoding for record + trailing newline.
func marshalNDJSON(record any) ([]byte, error) {
	b, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("marshal ndjson: %w", err)
	}
	return append(b, '\n'), nil
}
