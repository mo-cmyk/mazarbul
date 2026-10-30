package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseFormat_CSVKVTableAliases(t *testing.T) {
	cases := map[string]Format{
		"csv":   FormatCSV,
		"kv":    FormatKV,
		"table": FormatText,
		"jsonl": FormatNDJSON,
		"JSON":  FormatJSON,
	}
	for in, want := range cases {
		got, err := ParseFormat(in)
		if err != nil {
			t.Errorf("ParseFormat(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFormat(%q) = %v, want %v", in, got, want)
		}
	}
	if got := FormatCSV.String(); got != "csv" {
		t.Errorf("FormatCSV.String() = %q, want csv", got)
	}
	if got := FormatKV.String(); got != "kv" {
		t.Errorf("FormatKV.String() = %q, want kv", got)
	}
}

func TestCSVWriter_HeaderThenRows(t *testing.T) {
	var out, errb bytes.Buffer
	w := &csvWriter{stdout: &out, stderr: &errb}
	type rec struct {
		Name  string   `json:"name"`
		Count int      `json:"count"`
		Tags  []string `json:"tags"`
	}
	if err := w.Emit(rec{Name: "alpha", Count: 3, Tags: []string{"a", "b"}}); err != nil {
		t.Fatalf("emit 1: %v", err)
	}
	if err := w.Emit(rec{Name: "beta", Count: 7}); err != nil {
		t.Fatalf("emit 2: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got := out.String()
	// Columns are sorted: count, name, tags.
	wantHeader := "count,name,tags\n"
	if !strings.HasPrefix(got, wantHeader) {
		t.Errorf("csv header = %q, want prefix %q", got, wantHeader)
	}
	if !strings.Contains(got, `3,alpha,"[""a"",""b""]"`) {
		t.Errorf("csv row 1 missing nested-JSON cell in:\n%s", got)
	}
	if !strings.Contains(got, "7,beta,\n") {
		t.Errorf("csv row 2 (empty tags) malformed in:\n%s", got)
	}
}

func TestKVWriter_LinesAndSeparator(t *testing.T) {
	var out, errb bytes.Buffer
	w := &kvWriter{stdout: &out, stderr: &errb}
	_ = w.Emit(map[string]any{"name": "alpha", "count": 3})
	_ = w.Emit(map[string]any{"name": "beta", "count": 7})
	got := out.String()
	if !strings.Contains(got, "name: alpha\n") || !strings.Contains(got, "count: 3\n") {
		t.Errorf("kv missing key:value lines:\n%s", got)
	}
	// A blank line separates the two records.
	if !strings.Contains(got, "\n\n") {
		t.Errorf("kv missing blank-line separator between records:\n%s", got)
	}
}
