package output_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

type sampleRow struct {
	Name string `json:"name"`
	Hits int    `json:"hits"`
}

func (s sampleRow) RenderText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s %d\n", s.Name, s.Hits)
	return err
}

func TestTextRendererUsedForCustomTypes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(output.FormatText, &stdout, &stderr)
	if err := w.Emit(sampleRow{Name: "abc", Hits: 3}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "abc 3") {
		t.Fatalf("stdout = %q, want abc 3", got)
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want output.Format
	}{
		{"", output.FormatText},
		{"text", output.FormatText},
		{"TEXT", output.FormatText},
		{"json", output.FormatJSON},
		{"ndjson", output.FormatNDJSON},
		{"jsonl", output.FormatNDJSON},
	}
	for _, c := range cases {
		got, err := output.ParseFormat(c.in)
		if err != nil {
			t.Fatalf("ParseFormat(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseFormat(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseFormatRejectsUnknown(t *testing.T) {
	if _, err := output.ParseFormat("yaml"); err == nil {
		t.Fatal("expected error for yaml")
	}
}

func TestTextEmit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(output.FormatText, &stdout, &stderr)
	if err := w.Emit("hello"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "hello") {
		t.Fatalf("stdout = %q, want contains hello", got)
	}
}

func TestJSONEmit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(output.FormatJSON, &stdout, &stderr)
	if err := w.Emit(map[string]string{"k": "v"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := stdout.String()
	if !strings.Contains(got, "\"k\": \"v\"") {
		t.Fatalf("stdout = %q, want JSON with k:v", got)
	}
	w.Header("informational")
	if !strings.Contains(stderr.String(), "informational") {
		t.Fatalf("stderr = %q, want header on stderr in JSON mode", stderr.String())
	}
	if strings.Contains(stdout.String(), "informational") {
		t.Fatalf("stdout = %q, want no header in JSON mode", stdout.String())
	}
}

func TestJSONStreamEscalatesToNDJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(output.FormatJSON, &stdout, &stderr)
	if err := w.EmitStream(map[string]int{"i": 1}); err != nil {
		t.Fatal(err)
	}
	if err := w.EmitStream(map[string]int{"i": 2}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), stdout.String())
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "{") {
			t.Fatalf("line %q not a JSON object", line)
		}
	}
}

func TestNDJSONEmit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(output.FormatNDJSON, &stdout, &stderr)
	if err := w.Emit(map[string]int{"i": 7}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "{") || !strings.HasSuffix(stdout.String(), "}\n") {
		t.Fatalf("stdout = %q, want NDJSON line", stdout.String())
	}
}

func TestExitFromError(t *testing.T) {
	if output.ExitFromError(nil) != output.ExitOK {
		t.Fatal("nil should return ExitOK")
	}
	if output.ExitFromError(errors.New("boom")) != output.ExitGenericFailure {
		t.Fatal("uncoded should return ExitGenericFailure")
	}
	ce := output.Errorf(output.ExitNotFound, output.CodeNotFound, "missing %s", "x")
	if output.ExitFromError(ce) != output.ExitNotFound {
		t.Fatalf("coded exit = %d, want %d", output.ExitFromError(ce), output.ExitNotFound)
	}
}

func TestCodedErrorEnvelope(t *testing.T) {
	ce := output.Errorf(output.ExitNotFound, output.CodeNotFound, "entity %s missing", "x")
	env := ce.Envelope()
	inner, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope shape: %+v", env)
	}
	if inner["code"] != output.CodeNotFound {
		t.Fatalf("code = %v", inner["code"])
	}
	if inner["message"] == "" {
		t.Fatalf("message empty: %+v", inner)
	}
}
