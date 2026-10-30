package output

import (
	"fmt"
	"io"
)

// textWriter renders human-readable text on stdout. Header and Footer
// go to stdout as well (they are human chrome).
type textWriter struct {
	stdout io.Writer
	stderr io.Writer
	isTTY  bool
}

func (t *textWriter) Emit(record any) error {
	return renderTextRecord(t.stdout, record)
}

func (t *textWriter) EmitStream(record any) error {
	return renderTextRecord(t.stdout, record)
}

func (t *textWriter) Header(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(t.stdout, text) //nolint:errcheck // best-effort stdout write
}

func (t *textWriter) Footer(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(t.stdout, text) //nolint:errcheck // best-effort stdout write
}

func (t *textWriter) Format() Format { return FormatText }
func (t *textWriter) IsTTY() bool    { return t.isTTY }
func (t *textWriter) Flush() error   { return nil }
