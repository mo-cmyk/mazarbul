package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// makeInstallCmd builds a minimal root wiring the top-level install command,
// mirroring makeSchemaCmd's flag surface so output.From resolves cleanly.
func makeInstallCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.AddCommand(newInstallCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// TestInstallCmd_DryRunBuiltin installs the business-de builtin in dry-run mode
// (no event log touched) and asserts the flagship company kind is listed.
func TestInstallCmd_DryRunBuiltin(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", "business-de", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("install --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "business-de") {
		t.Errorf("output %q missing pack name business-de", got)
	}
	if !strings.Contains(got, "company") {
		t.Errorf("output %q missing the company kind", got)
	}
}

// TestInstallCmd_UnknownPack reports a friendly error and exits 1 for an
// argument that is neither a local dir nor a builtin pack.
func TestInstallCmd_UnknownPack(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", "no-such-pack-xyz", "--dry-run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown pack, got nil")
	}
	if !strings.Contains(errBuf.String(), "not found") {
		t.Errorf("stderr = %q, want mention of not found", errBuf.String())
	}
}

// TestSchemaImportCmd_DryRun imports schema.org in dry-run mode and asserts the
// generated kinds are namespaced extension kinds.
func TestSchemaImportCmd_DryRun(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "schema.org", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema import --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "extension:schema_org:") {
		t.Errorf("output %q missing namespaced schema.org kinds", out.String())
	}
}

// TestSchemaImportCmd_TypesFilter restricts the import to a single type.
func TestSchemaImportCmd_TypesFilter(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "schema.org", "--types", "Person", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schema import --types: %v (stderr=%q)", err, errBuf.String())
	}
	// Label is normalized to lowercase: "Person" → "person".
	if !strings.Contains(out.String(), "person") {
		t.Errorf("output %q missing normalized person kind", out.String())
	}
}

// (TestSchemaImportCmd_DeferredVocab removed: all five vocabularies are now
// bundled; per-vocab acceptance is covered in schema_import_test.go.)

// TestSchemaImportCmd_UnknownVocab rejects an unrecognised vocabulary.
func TestSchemaImportCmd_UnknownVocab(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "import", "nonsense"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown vocab, got nil")
	}
	if !strings.Contains(errBuf.String(), "unknown vocabulary") {
		t.Errorf("stderr = %q, want 'unknown vocabulary'", errBuf.String())
	}
}

// TestInstallCmd_NoArgExitsOne ensures bare "maz install" exits non-zero and
// prints a usage hint. Previously the command exited 0 (cobra allowed 0 args
// because Args was not set to ExactArgs(1) in the pre-fix code).
func TestInstallCmd_NoArgExitsOne(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for missing pack argument, got nil")
	}
	if !strings.Contains(errBuf.String(), "<pack>") && !strings.Contains(errBuf.String(), "argument required") {
		t.Errorf("stderr %q should mention missing argument", errBuf.String())
	}
}

// TestInstallCmd_ListPrintsBuiltins verifies that --list prints the two
// bundled pack names without requiring a pack argument or event log.
func TestInstallCmd_ListPrintsBuiltins(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", "--list", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("install --list: %v (stderr=%q)", err, errBuf.String())
	}
	got := out.String()
	for _, want := range []string{"schema-org-core", "business-de"} {
		if !strings.Contains(got, want) {
			t.Errorf("--list output %q missing %q", got, want)
		}
	}
}

// TestInstallCmd_TooManyArgsExitsOne ensures "maz install a b" exits non-zero.
// Silently ignoring extra positional arguments hides malformed scripts.
func TestInstallCmd_TooManyArgsExitsOne(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", "schema-org-core", "typo"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for too many arguments, got nil")
	}
}

// TestInstallCmd_ListWithArgExitsOne ensures "--list extra" exits non-zero.
// --list does not accept positional arguments.
func TestInstallCmd_ListWithArgExitsOne(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", "--list", "schema-org-core"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for --list with extra argument, got nil")
	}
}

// TestFilterPackAgainstDB_SkipsUserPredicates verifies the core data-safety
// invariant: a pack predicate whose name matches an existing user-authored
// predicate is skipped (not installed) when --force is false.
func TestFilterPackAgainstDB_SkipsUserPredicates(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a full daemon wiring with SQLite; running in parallel
	// with other wiring tests exhausts the race-detector address space on macOS.

	// Open an in-memory wiring so we can seed the predicates table without
	// touching the real data directory.
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}
	t.Cleanup(func() { _ = wired.Close(t.Context()) })

	// Seed a user-authored predicate named "manages_company".
	db, err := wired.Orchestrator.Storage.OpenSQLite(t.Context())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO predicates (predicate, kind, description, added_at, added_by)
		 VALUES ('manages_company','functional','user custom','2026-01-01T00:00:00Z','user')`); err != nil {
		t.Fatalf("seed predicate: %v", err)
	}

	// Build a minimal pack that contains manages_company.
	pack := &schemapack.Pack{
		Manifest: schemapack.PackManifest{
			Name: "test-pack", Version: "0.1.0",
			Category: "extension", Kind: "schema-pack",
		},
		Predicates: []schemapack.PredicateDef{
			{Predicate: "manages_company", Kind: "non-functional", Description: "pack version"},
			{Predicate: "reports_to", Kind: "non-functional", Description: "safe predicate"},
		},
	}

	cmd := &cobra.Command{}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	filtered, skippedKinds, skippedPreds, err := filterPackAgainstDB(t.Context(), cmd, db, pack, false)
	if err != nil {
		t.Fatalf("filterPackAgainstDB: %v", err)
	}

	// manages_company must be skipped; reports_to must pass through.
	if len(skippedPreds) != 1 || skippedPreds[0] != "manages_company" {
		t.Errorf("skippedPreds = %v, want [manages_company]", skippedPreds)
	}
	if len(skippedKinds) != 0 {
		t.Errorf("skippedKinds = %v, want []", skippedKinds)
	}
	if len(filtered.Predicates) != 1 || filtered.Predicates[0].Predicate != "reports_to" {
		t.Errorf("filtered predicates = %v, want [reports_to]", filtered.Predicates)
	}
	if !strings.Contains(errBuf.String(), "manages_company") {
		t.Errorf("stderr %q should mention skipped predicate", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "--force") {
		t.Errorf("stderr %q should mention --force", errBuf.String())
	}
}

// TestFilterPackAgainstDB_ForceOverwrites verifies that --force keeps ALL pack
// items (including user-owned collisions) and emits a WARNING rather than
// silently skipping.
func TestFilterPackAgainstDB_ForceOverwrites(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a full daemon wiring (see SkipsUserPredicates).

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}
	t.Cleanup(func() { _ = wired.Close(t.Context()) })

	db, err := wired.Orchestrator.Storage.OpenSQLite(t.Context())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO predicates (predicate, kind, description, added_at, added_by)
		 VALUES ('manages_company','functional','user custom','2026-01-01T00:00:00Z','user')`); err != nil {
		t.Fatalf("seed predicate: %v", err)
	}

	pack := &schemapack.Pack{
		Manifest: schemapack.PackManifest{
			Name: "test-pack", Version: "0.1.0",
			Category: "extension", Kind: "schema-pack",
		},
		Predicates: []schemapack.PredicateDef{
			{Predicate: "manages_company", Kind: "non-functional", Description: "pack version"},
		},
	}

	cmd := &cobra.Command{}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	filtered, _, skippedPreds, err := filterPackAgainstDB(t.Context(), cmd, db, pack, true /* force */)
	if err != nil {
		t.Fatalf("filterPackAgainstDB: %v", err)
	}

	// With --force: nothing skipped, item passes through, WARNING on stderr.
	if len(skippedPreds) != 0 {
		t.Errorf("with --force: skippedPreds = %v, want []", skippedPreds)
	}
	if len(filtered.Predicates) != 1 {
		t.Errorf("with --force: filtered predicates = %v, want 1", filtered.Predicates)
	}
	if !strings.Contains(errBuf.String(), "WARNING") {
		t.Errorf("with --force: stderr %q should contain WARNING", errBuf.String())
	}
}

// TestInstallCmd_BadArgJSONEmitsErrorEnvelope verifies that
// "install <bad> --output json" emits a JSON error envelope on stdout
// (not just plain text on stderr). B8: scripted consumers must get parseable
// errors on stdout when --output json is active.
func TestInstallCmd_BadArgJSONEmitsErrorEnvelope(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", "no-such-pack-xyz", "--output", "json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown pack, got nil")
	}

	rawOut := strings.TrimSpace(out.String())
	if rawOut == "" {
		t.Fatal("install --output json with bad arg: stdout empty; expected a JSON error envelope")
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(rawOut), &got); jerr != nil {
		t.Fatalf("install --output json: stdout not JSON: %v\nraw: %q", jerr, rawOut)
	}
	if _, ok := got["error"]; !ok {
		t.Errorf("install --output json: JSON missing 'error' key: %s", rawOut)
	}
	if _, ok := got["code"]; !ok {
		t.Errorf("install --output json: JSON missing 'code' key: %s", rawOut)
	}
}

// TestInstallCmd_ListNDJSONIsLineDelimited verifies that "install --list --output ndjson"
// emits one JSON object per line (NDJSON), not a pretty-printed JSON array.
// B9: ndjson must differ from json (one line per item, not a pretty array).
func TestInstallCmd_ListNDJSONIsLineDelimited(t *testing.T) {
	t.Parallel()
	// json mode.
	var jsonOut bytes.Buffer
	{
		root := makeInstallCmd(&jsonOut, nil)
		root.SetArgs([]string{"install", "--list", "--output", "json"})
		if err := root.Execute(); err != nil {
			t.Fatalf("install --list --output json: %v", err)
		}
	}

	// ndjson mode.
	var ndjsonOut bytes.Buffer
	{
		root := makeInstallCmd(&ndjsonOut, nil)
		root.SetArgs([]string{"install", "--list", "--output", "ndjson"})
		if err := root.Execute(); err != nil {
			t.Fatalf("install --list --output ndjson: %v", err)
		}
	}

	jsonRaw := strings.TrimSpace(jsonOut.String())
	ndjsonRaw := strings.TrimSpace(ndjsonOut.String())

	// json output must be an array.
	if !strings.HasPrefix(jsonRaw, "[") {
		t.Errorf("--output json: expected JSON array, got %q", jsonRaw)
	}

	// ndjson output must NOT be an array; each line must be a JSON object.
	if strings.HasPrefix(ndjsonRaw, "[") {
		t.Errorf("--output ndjson: got a JSON array (same as --output json), want one object per line; raw=%q", ndjsonRaw)
	}
	for i, line := range strings.Split(ndjsonRaw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("ndjson line %d not valid JSON: %v (line=%q)", i, err, line)
		}
	}

	// The two outputs must be byte-distinct.
	if jsonRaw == ndjsonRaw {
		t.Errorf("--output json and --output ndjson are byte-identical:\n%s", jsonRaw)
	}
}

// TestInstallCmd_ChecksumOnLocalDirErrors verifies that --checksum is refused
// when the source is a local directory, rather than silently ignoring it.
// N3: --checksum on a local-dir/file source must error with a clear message.
func TestInstallCmd_ChecksumOnLocalDirErrors(t *testing.T) {
	t.Parallel()
	// Use any local dir that is a valid pack dir — business-de is a builtin, so
	// use a temp fixture that exists on disk (writePresetFixture creates a real dir).
	// We can also pass a path that IS a directory but isn't a pack; the checksum
	// check should fire before the pack is parsed.
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", dir, "--checksum", "sha256:abc123", "--dry-run"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --checksum is given for a local dir, got nil")
	}
	stderrStr := errBuf.String()
	if !strings.Contains(stderrStr, "checksum") {
		t.Errorf("stderr %q should mention 'checksum'", stderrStr)
	}
	if !strings.Contains(stderrStr, "remote") {
		t.Errorf("stderr %q should explain checksum only applies to remote sources", stderrStr)
	}
}

// TestFilterPackAgainstDB_ExtensionPredicateNotProtected verifies that a pack
// predicate colliding with an existing extension-provenance predicate (not
// user-authored) is NOT skipped — pack-vs-pack collisions keep the idempotent
// UPSERT behaviour.
func TestFilterPackAgainstDB_ExtensionPredicateNotProtected(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a full daemon wiring (see SkipsUserPredicates).

	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wired, err := daemon.OpenWiring(t.Context(), daemon.WiringConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("OpenWiring: %v", err)
	}
	t.Cleanup(func() { _ = wired.Close(t.Context()) })

	db, err := wired.Orchestrator.Storage.OpenSQLite(t.Context())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	// Seed a predicate with added_by = 'extension' (not 'user').
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO predicates (predicate, kind, description, added_at, added_by)
		 VALUES ('manages_company','non-functional','ext version','2026-01-01T00:00:00Z','extension')`); err != nil {
		t.Fatalf("seed predicate: %v", err)
	}

	pack := &schemapack.Pack{
		Manifest: schemapack.PackManifest{
			Name: "test-pack", Version: "0.1.0",
			Category: "extension", Kind: "schema-pack",
		},
		Predicates: []schemapack.PredicateDef{
			{Predicate: "manages_company", Kind: "non-functional", Description: "pack version"},
		},
	}

	cmd := &cobra.Command{}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	filtered, _, skippedPreds, err := filterPackAgainstDB(t.Context(), cmd, db, pack, false)
	if err != nil {
		t.Fatalf("filterPackAgainstDB: %v", err)
	}

	// extension-provenance collision: NOT skipped (pack-vs-pack is idempotent).
	if len(skippedPreds) != 0 {
		t.Errorf("extension collision: skippedPreds = %v, want [] (not protected)", skippedPreds)
	}
	if len(filtered.Predicates) != 1 {
		t.Errorf("extension collision: filtered predicates = %v, want 1", filtered.Predicates)
	}
}

// writeInstallPackDir writes a minimal installable schema pack (manifest +
// one entity kind) into a fresh temp dir and returns the path. manifestBody is
// the full manifest.toml so a test can add api_version / scopes lines.
func writeInstallPackDir(t *testing.T, manifestBody string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir %s: %v", full, mkErr)
		}
		if wErr := os.WriteFile(full, []byte(content), 0o644); wErr != nil {
			t.Fatalf("write %s: %v", full, wErr)
		}
	}
	write("manifest.toml", manifestBody)
	write("kinds/widget.yaml", ""+
		"name: extension:scoped:widget\n"+
		"displayName: Widget\n"+
		"description: A widget tracked by the scoped pack.\n")
	return dir
}

// TestInstallCmd_IncompatibleAPIVersionRefused verifies that a manifest whose
// declared apiVersion is outside the host-supported range is refused at install
// (before any state is touched) with a clear host-vs-required message and the
// E_VALIDATION JSON code. The gate fires ahead of --dry-run, so no daemon boots.
func TestInstallCmd_IncompatibleAPIVersionRefused(t *testing.T) {
	t.Parallel()
	dir := writeInstallPackDir(t, ""+
		"name        = \"scoped\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"Pack built against a future plugin API.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n"+
		"api_version = \"v99.0.0\"\n")
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", dir, "--output", "json", "--dry-run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal for an incompatible apiVersion, got nil")
	}
	if !strings.Contains(errBuf.String(), "v99.0.0") {
		t.Errorf("stderr %q should name the required version v99.0.0", errBuf.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
		t.Fatalf("error envelope JSON: %v (raw=%q)", jerr, out.String())
	}
	if env.Code != "E_VALIDATION" {
		t.Errorf("code = %q, want E_VALIDATION", env.Code)
	}
}

// TestInstallCmd_UnknownScopeTokenRefused verifies that a manifest declaring a
// scope token outside the kernel enum (project|personal|team) is refused with
// the E_VALIDATION code, before any state is touched.
func TestInstallCmd_UnknownScopeTokenRefused(t *testing.T) {
	t.Parallel()
	dir := writeInstallPackDir(t, ""+
		"name        = \"scoped\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"Pack requesting an unknown scope.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n"+
		"scopes      = [\"bogus\"]\n")
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"install", dir, "--output", "json", "--dry-run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal for an unknown scope token, got nil")
	}
	if !strings.Contains(errBuf.String(), "scope") {
		t.Errorf("stderr %q should mention the invalid scope", errBuf.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
		t.Fatalf("error envelope JSON: %v (raw=%q)", jerr, out.String())
	}
	if env.Code != "E_VALIDATION" {
		t.Errorf("code = %q, want E_VALIDATION", env.Code)
	}
}

// TestInstallCmd_ScopesRequireConsentNonInteractive verifies that a manifest
// declaring valid scopes refuses a non-interactive install lacking --yes: scopes
// are never granted silently in a scripted run. The refusal precedes any daemon
// open, so the test needs no live backend.
func TestInstallCmd_ScopesRequireConsentNonInteractive(t *testing.T) {
	t.Parallel()
	dir := writeInstallPackDir(t, ""+
		"name        = \"scoped\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"Pack requesting scopes.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n"+
		"scopes      = [\"project\", \"personal\"]\n")
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	// Non-dry-run, no --yes: the scope-grant gate must refuse before opening a log.
	root.SetArgs([]string{"--data-dir", t.TempDir(), "install", dir})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal: scopes declared, no --yes, non-interactive")
	}
	if !strings.Contains(errBuf.String(), "--yes") {
		t.Errorf("stderr %q should tell the user to re-run with --yes", errBuf.String())
	}
}

// TestInstallCmd_ScopesGrantedWithYes verifies that a manifest with a compatible
// apiVersion and valid scopes installs end-to-end under --yes and records the
// granted scopes in the result envelope.
func TestInstallCmd_ScopesGrantedWithYes(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a full daemon wiring (see filterPackAgainstDB tests).
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	dir := writeInstallPackDir(t, ""+
		"name        = \"scoped\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"Pack with a compatible apiVersion and valid scopes.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n"+
		"api_version = \"v1.0.0\"\n"+
		"scopes      = [\"project\", \"personal\"]\n")
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "install", dir, "--yes", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("install --yes: %v (stderr=%q)", err, errBuf.String())
	}
	var got struct {
		KindsRegistered []string `json:"kindsRegistered"`
		GrantedScopes   []string `json:"grantedScopes"`
		DryRun          bool     `json:"dryRun"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("install JSON: %v (raw=%q)", jerr, out.String())
	}
	if got.DryRun {
		t.Errorf("a real install must not report dryRun=true")
	}
	if len(got.GrantedScopes) != 2 || got.GrantedScopes[0] != "project" || got.GrantedScopes[1] != "personal" {
		t.Errorf("grantedScopes = %v, want [project personal]", got.GrantedScopes)
	}
	if len(got.KindsRegistered) == 0 {
		t.Errorf("kindsRegistered should be non-empty after a real install: %q", out.String())
	}
}

// TestInstallCmd_NoScopeFieldsInstallsWithoutPrompt verifies that a pack
// declaring NEITHER apiVersion NOR scopes installs end-to-end with NO --yes and
// NO prompt (behaves exactly as before this feature), recording no scopes.
func TestInstallCmd_NoScopeFieldsInstallsWithoutPrompt(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a full daemon wiring.
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	dir := writeInstallPackDir(t, ""+
		"name        = \"plain\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"Pack with neither apiVersion nor scopes.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n")
	var out, errBuf bytes.Buffer
	root := makeInstallCmd(&out, &errBuf)
	// No --yes: must still succeed, because no scopes are declared (no prompt).
	root.SetArgs([]string{"--data-dir", dataDir, "install", dir, "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("install (no fields): %v (stderr=%q)", err, errBuf.String())
	}
	var got struct {
		KindsRegistered []string `json:"kindsRegistered"`
		GrantedScopes   []string `json:"grantedScopes"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("install JSON: %v (raw=%q)", jerr, out.String())
	}
	if len(got.GrantedScopes) != 0 {
		t.Errorf("grantedScopes = %v, want empty for a pack declaring no scopes", got.GrantedScopes)
	}
	if len(got.KindsRegistered) == 0 {
		t.Errorf("kindsRegistered should be non-empty: %q", out.String())
	}
}
