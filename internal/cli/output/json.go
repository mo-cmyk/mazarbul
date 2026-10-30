package output

import (
	"fmt"
	"io"
)

// jsonWriter buffers a single JSON document per command. Header/Footer
// route to stderr so stdout stays parseable.
//
// Emit may be called multiple times — each call writes a top-level
// JSON document followed by a newline. This matches existing Mazarbul
// commands that emit a single object (e.g. `maz remember`) and also
// works for commands that already structure their own envelope (e.g.
// `maz recall` returning a wrapper with a hits array).
type jsonWriter struct {
	stdout io.Writer
	stderr io.Writer
	isTTY  bool
}

func (j *jsonWriter) Emit(record any) error {
	if record == nil {
		return nil
	}
	b, err := marshalJSON(record)
	if err != nil {
		return err
	}
	_, werr := j.stdout.Write(b)
	return werr
}

// EmitStream promotes JSON-mode streams to NDJSON so consumers can
// process the feed line-by-line without buffering a JSON array.
func (j *jsonWriter) EmitStream(record any) error {
	if record == nil {
		return nil
	}
	b, err := marshalNDJSON(record)
	if err != nil {
		return err
	}
	_, werr := j.stdout.Write(b)
	return werr
}

func (j *jsonWriter) Header(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(j.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (j *jsonWriter) Footer(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(j.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (j *jsonWriter) Format() Format { return FormatJSON }
func (j *jsonWriter) IsTTY() bool    { return j.isTTY }
func (j *jsonWriter) Flush() error   { return nil }
