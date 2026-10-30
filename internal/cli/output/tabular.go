package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// flattenRecord renders a record as an ordered list of (key, value) pairs by
// marshaling it to JSON and reading its top-level object keys in sorted order.
// Scalar string/number/bool/null values become plain strings; nested
// objects/arrays become compact JSON so a cell or line is never multi-line.
// A non-object record (a bare scalar or array) degrades to a single "value"
// column holding its compact JSON — robust rather than erroring.
func flattenRecord(record any) (keys []string, values map[string]string, err error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal record: %w", err)
	}
	// A bare scalar or array is not column-shaped: degrade to a single "value"
	// cell holding its compact form. Only a leading '{' is treated as an object.
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return []string{"value"}, map[string]string{"value": strings.TrimSpace(string(raw))}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("unmarshal record object: %w", err)
	}
	keys = make([]string, 0, len(obj))
	values = make(map[string]string, len(obj))
	for k, v := range obj {
		keys = append(keys, k)
		values[k] = stringifyJSONValue(v)
	}
	sort.Strings(keys)
	return keys, values, nil
}

// stringifyJSONValue turns one JSON value into a single-line cell string.
func stringifyJSONValue(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "" || s == "null":
		return ""
	case strings.HasPrefix(s, `"`):
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return str
		}
		return s
	default:
		// number, bool, object, or array — keep the compact JSON form.
		return s
	}
}

// csvWriter emits a CSV header established from the first record's columns,
// then one row per record. Columns are fixed by the first Emit; later records
// align to that header (missing keys blank, extra keys dropped) so the output
// stays rectangular and script-parseable.
type csvWriter struct {
	stdout io.Writer
	stderr io.Writer
	isTTY  bool
	w      *csv.Writer
	header []string
}

func (c *csvWriter) Emit(record any) error {
	if record == nil {
		return nil
	}
	keys, values, err := flattenRecord(record)
	if err != nil {
		return err
	}
	if c.w == nil {
		c.w = csv.NewWriter(c.stdout)
		c.header = keys
		if werr := c.w.Write(c.header); werr != nil {
			return fmt.Errorf("write csv header: %w", werr)
		}
	}
	row := make([]string, len(c.header))
	for i, k := range c.header {
		row[i] = values[k]
	}
	if werr := c.w.Write(row); werr != nil {
		return fmt.Errorf("write csv row: %w", werr)
	}
	return nil
}

func (c *csvWriter) EmitStream(record any) error { return c.Emit(record) }

func (c *csvWriter) Header(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(c.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (c *csvWriter) Footer(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(c.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (c *csvWriter) Format() Format { return FormatCSV }
func (c *csvWriter) IsTTY() bool    { return c.isTTY }

func (c *csvWriter) Flush() error {
	if c.w == nil {
		return nil
	}
	c.w.Flush()
	return c.w.Error()
}

// kvWriter emits "key: value" lines per record with a blank line between
// records — the most legible shape for inspecting a single record by eye.
type kvWriter struct {
	stdout io.Writer
	stderr io.Writer
	isTTY  bool
	wrote  bool
}

func (k *kvWriter) Emit(record any) error {
	if record == nil {
		return nil
	}
	keys, values, err := flattenRecord(record)
	if err != nil {
		return err
	}
	if k.wrote {
		if _, werr := fmt.Fprintln(k.stdout); werr != nil {
			return fmt.Errorf("write kv separator: %w", werr)
		}
	}
	for _, key := range keys {
		if _, werr := fmt.Fprintf(k.stdout, "%s: %s\n", key, values[key]); werr != nil {
			return fmt.Errorf("write kv line: %w", werr)
		}
	}
	k.wrote = true
	return nil
}

func (k *kvWriter) EmitStream(record any) error { return k.Emit(record) }

func (k *kvWriter) Header(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(k.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (k *kvWriter) Footer(text string) {
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(k.stderr, text) //nolint:errcheck // best-effort stderr write
}

func (k *kvWriter) Format() Format { return FormatKV }
func (k *kvWriter) IsTTY() bool    { return k.isTTY }
func (k *kvWriter) Flush() error   { return nil }
