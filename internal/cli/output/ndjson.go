package output

import (
	"fmt"
	"io"
)

// ndjsonWriter emits one JSON document per Emit / EmitStream call.
type ndjsonWriter struct {
	stdout io.Writer
	stderr io.Writer
	isTTY  bool
}

func (n *ndjsonWriter) Emit(record any) error {
	if record == nil {
		return nil
	}
	b, err := marshalNDJSON(record)
	if err != nil {
		return err
	}
	_, werr := n.stdout.Write(b)
	return werr
}

func (n *ndjsonWriter) EmitStream(record any) error {
	return n.Emit(record)
}

func (n *ndjsonWriter) Header(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(n.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (n *ndjsonWriter) Footer(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(n.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (n *ndjsonWriter) Format() Format { return FormatNDJSON }
func (n *ndjsonWriter) IsTTY() bool    { return n.isTTY }
func (n *ndjsonWriter) Flush() error   { return nil }
