package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaImportCmd_FoafDryRun imports the FOAF vocabulary in dry-run mode
// and asserts the generated kinds are namespaced under extension:foaf:.
func TestSchemaImportCmd_FoafDryRun(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "foaf", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema import foaf --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "extension:foaf:") {
		t.Errorf("output %q missing namespaced foaf kinds", out.String())
	}
}

// TestSchemaImportCmd_DublinCoreDryRun imports dublincore in dry-run mode and
// asserts the output contains extension:dublincore: namespaced kinds.
func TestSchemaImportCmd_DublinCoreDryRun(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "dublincore", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema import dublincore --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "extension:dublincore:") {
		t.Errorf("output %q missing namespaced dublincore kinds", out.String())
	}
}

// TestSchemaImportCmd_SkosDryRun imports skos in dry-run mode and asserts
// the output contains extension:skos: namespaced kinds.
func TestSchemaImportCmd_SkosDryRun(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "skos", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema import skos --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "extension:skos:") {
		t.Errorf("output %q missing namespaced skos kinds", out.String())
	}
}

// TestSchemaImportCmd_WikidataDryRun imports wikidata in dry-run mode and
// asserts the output contains extension:wikidata: namespaced kinds.
func TestSchemaImportCmd_WikidataDryRun(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "wikidata", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema import wikidata --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "extension:wikidata:") {
		t.Errorf("output %q missing namespaced wikidata kinds", out.String())
	}
}

// TestSchemaImportCmd_FoafPersonGraft verifies that importing foaf with a
// filter on "Person" produces a kind that maps to the core person kind
// (extension:foaf:person) in the dry-run output.
func TestSchemaImportCmd_FoafPersonGraft(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "foaf", "--types", "Person", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema import foaf --types Person --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	// The normalized kind name for FOAF Person is extension:foaf:person.
	if !strings.Contains(out.String(), "extension:foaf:person") {
		t.Errorf("output %q missing extension:foaf:person", out.String())
	}
}

// TestSchemaImportCmd_FromFileAllVocabs verifies that --from-file works for
// every supported vocabulary: the supplied types.json entries are imported
// under the vocabulary's own namespace.
func TestSchemaImportCmd_FromFileAllVocabs(t *testing.T) {
	t.Parallel()
	snapshot := filepath.Join(t.TempDir(), "types.json")
	if err := os.WriteFile(snapshot, []byte(
		`[{"id":"https://example.org/Spacecraft","label":"Spacecraft","comment":"A crewed or uncrewed vehicle designed for spaceflight."}]`,
	), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	// Expected kind namespace per vocab (dots normalize to underscores).
	wantNS := map[string]string{
		"schema.org": "schema_org",
		"foaf":       "foaf",
		"dublincore": "dublincore",
		"skos":       "skos",
		"wikidata":   "wikidata",
	}
	for _, v := range []string{"schema.org", "foaf", "dublincore", "skos", "wikidata"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			root := makeSchemaCmd(&out, &errBuf)
			root.SetArgs([]string{"schema", "import", v, "--from-file", snapshot, "--dry-run"})
			if err := root.Execute(); err != nil {
				t.Fatalf("schema import %s --from-file --dry-run: %v (stderr=%q)", v, err, errBuf.String())
			}
			want := "extension:" + wantNS[v] + ":spacecraft"
			if !strings.Contains(out.String(), want) {
				t.Errorf("output %q missing %s", out.String(), want)
			}
		})
	}
}

// TestSchemaImportCmd_FromFileNullRejected pins that a --from-file payload of
// JSON "null" (which parses to a nil entry slice) is rejected with a clear
// error instead of silently falling back to the embedded snapshot — a
// malformed replacement file must never turn into a full bundled import.
func TestSchemaImportCmd_FromFileNullRejected(t *testing.T) {
	t.Parallel()
	snapshot := filepath.Join(t.TempDir(), "null.json")
	if err := os.WriteFile(snapshot, []byte(`null`), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "wikidata", "--from-file", snapshot, "--dry-run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for JSON null --from-file payload, got nil")
	}
	if !strings.Contains(errBuf.String(), "contains no type entries") {
		t.Errorf("stderr = %q, want no-type-entries error", errBuf.String())
	}
	if strings.Contains(out.String(), "extension:wikidata:") {
		t.Errorf("output %q shows embedded wikidata kinds — null file fell back to the bundled snapshot", out.String())
	}
}

// TestSchemaImportCmd_FromFileUnreadable verifies that an unreadable
// --from-file path fails with exit code 1 and a clear error message.
func TestSchemaImportCmd_FromFileUnreadable(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "foaf", "--from-file", filepath.Join(t.TempDir(), "missing.json"), "--dry-run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for missing --from-file path, got nil")
	}
	if !strings.Contains(errBuf.String(), "read --from-file") {
		t.Errorf("stderr = %q, want read --from-file error", errBuf.String())
	}
}

// TestSchemaImportCmd_AllVocabsAccepted asserts every supported vocabulary
// name is accepted without an "unknown vocabulary" error in dry-run mode.
func TestSchemaImportCmd_AllVocabsAccepted(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"schema.org", "foaf", "dublincore", "skos", "wikidata"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			root := makeSchemaCmd(&out, &errBuf)
			root.SetArgs([]string{"schema", "import", v, "--dry-run"})
			if err := root.Execute(); err != nil {
				t.Fatalf("schema import %s --dry-run: %v (stderr=%q)", v, err, errBuf.String())
			}
			if strings.Contains(errBuf.String(), "unknown vocabulary") {
				t.Errorf("vocab %s reported as unknown: %q", v, errBuf.String())
			}
		})
	}
}
