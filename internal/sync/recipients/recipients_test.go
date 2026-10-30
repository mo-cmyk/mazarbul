package recipients

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleRows returns a small fixed set of rows spanning two epochs and
// out-of-order principals so ordering and round-trip assertions have
// something to bite on.
func sampleRows() []Row {
	ct := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	return []Row{
		{PrincipalUID: "01HZZZZ0000000000000000003", WrapPublicKey: "cHViLTM=", DEKEpoch: 2, WrappedDEK: "d3JhcC0z", CreateTime: ct},
		{PrincipalUID: "01HZZZZ0000000000000000001", WrapPublicKey: "cHViLTE=", DEKEpoch: 1, WrappedDEK: "d3JhcC0x", CreateTime: ct},
		{PrincipalUID: "01HZZZZ0000000000000000002", WrapPublicKey: "cHViLTI=", DEKEpoch: 1, WrappedDEK: "d3JhcC0y", CreateTime: ct},
	}
}

// TestWriteRead_RoundTrip writes a set of rows and reads them back,
// asserting the recovered rows match the deterministic (epoch, principal)
// order rather than the input order.
func TestWriteRead_RoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), Filename)
	if err := Write(path, sampleRows()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Row{
		{PrincipalUID: "01HZZZZ0000000000000000001", WrapPublicKey: "cHViLTE=", DEKEpoch: 1, WrappedDEK: "d3JhcC0x", CreateTime: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)},
		{PrincipalUID: "01HZZZZ0000000000000000002", WrapPublicKey: "cHViLTI=", DEKEpoch: 1, WrappedDEK: "d3JhcC0y", CreateTime: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)},
		{PrincipalUID: "01HZZZZ0000000000000000003", WrapPublicKey: "cHViLTM=", DEKEpoch: 2, WrappedDEK: "d3JhcC0z", CreateTime: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestWrite_DeterministicOrdering pins the stable-diff guarantee: two
// shuffled inputs that carry the same rows must produce byte-identical
// files.
func TestWrite_DeterministicOrdering(t *testing.T) {
	t.Parallel()
	rows := sampleRows()
	shuffled := []Row{rows[1], rows[0], rows[2]} // a different input order

	pathA := filepath.Join(t.TempDir(), Filename)
	pathB := filepath.Join(t.TempDir(), Filename)
	if err := Write(pathA, rows); err != nil {
		t.Fatalf("Write A: %v", err)
	}
	if err := Write(pathB, shuffled); err != nil {
		t.Fatalf("Write B: %v", err)
	}
	a, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	b, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("shuffled inputs produced different bytes\n A: %s\n B: %s", a, b)
	}
}

// TestRead_MissingFile confirms a missing recipients file is a best-effort
// no-op: (nil, nil), never an error.
func TestRead_MissingFile(t *testing.T) {
	t.Parallel()
	rows, err := Read(filepath.Join(t.TempDir(), "absent", Filename))
	if err != nil {
		t.Fatalf("missing file err = %v, want nil", err)
	}
	if rows != nil {
		t.Errorf("missing file rows = %v, want nil", rows)
	}
}

// TestRead_SkipsBlankLines confirms blank lines between records are
// ignored rather than parsed as malformed.
func TestRead_SkipsBlankLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), Filename)
	body := "\n" +
		`{"principalUid":"01HZZZZ0000000000000000001","wrapPublicKey":"cHViLTE=","dekEpoch":1,"wrappedDek":"d3JhcC0x","createTime":"2026-06-16T12:00:00Z"}` + "\n" +
		"\n   \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (blank lines skipped)", len(rows))
	}
}

// TestRead_MalformedLine confirms a malformed JSONL line surfaces a
// wrapped error rather than silently dropping the row.
func TestRead_MalformedLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), Filename)
	if err := os.WriteFile(path, []byte("{not json}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Fatalf("Read of malformed line returned nil error, want a parse error")
	}
}

// piiDenylist is the set of JSON-tag substrings that would betray a
// personally-identifying field on Row. recipients.jsonl is plaintext and
// published, so it must never carry any of these.
var piiDenylist = []string{"name", "displayname", "display_name", "github", "login", "email", "mail", "handle"}

// TestRow_NoPIIFields walks Row's struct fields via reflection and
// asserts the JSON tag set is EXACTLY the five non-identifying fields and
// that no tag matches the PII denylist. This is the privacy contract for
// the published bootstrap artifact: a future field addition that leaks a
// name/login/email fails here.
func TestRow_NoPIIFields(t *testing.T) {
	t.Parallel()
	want := map[string]struct{}{
		"principalUid":  {},
		"wrapPublicKey": {},
		"dekEpoch":      {},
		"wrappedDek":    {},
		"createTime":    {},
	}
	rt := reflect.TypeOf(Row{})
	got := make(map[string]struct{}, rt.NumField())
	for i := range rt.NumField() {
		field := rt.Field(i)
		tag := field.Tag.Get("json")
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		if tag == "" || tag == "-" {
			t.Errorf("field %s has no usable json tag (%q)", field.Name, field.Tag.Get("json"))
			continue
		}
		got[tag] = struct{}{}
		lower := strings.ToLower(tag)
		for _, bad := range piiDenylist {
			if strings.Contains(lower, bad) {
				t.Errorf("field %s json tag %q matches PII denylist term %q", field.Name, tag, bad)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Row json tag set mismatch\n got: %v\nwant: %v", keys(got), keys(want))
	}
}

// TestRow_MarshalCarriesNoPII marshals a realistic row and asserts the
// serialized bytes contain none of the denylist substrings, catching a
// stray value (not just a field name) that might leak identity.
func TestRow_MarshalCarriesNoPII(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), Filename)
	row := Row{
		PrincipalUID:  "01HZZZZ0000000000000000042",
		WrapPublicKey: "MCowBQYDK2VuAyEAabc123base64x25519pubkey00000000000000=",
		DEKEpoch:      7,
		WrappedDEK:    "c2VhbGVkLWJveC1ieXRlcy1nby1oZXJlLW9wYXF1ZQ==",
		CreateTime:    time.Date(2026, 6, 16, 9, 30, 0, 0, time.UTC),
	}
	if err := Write(path, []Row{row}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lower := strings.ToLower(string(raw))
	for _, bad := range []string{"@", "displayname", "github.com", "login"} {
		if strings.Contains(lower, bad) {
			t.Errorf("serialized recipients row contains PII-like substring %q: %s", bad, raw)
		}
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
