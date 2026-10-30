package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// makeEntityRoot wires a minimal cobra root for entity + fact + get tests.
func makeEntityRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newEntityCmd())
	root.AddCommand(newFactCmd())
	root.AddCommand(newGetCmd())
	root.AddCommand(newRelateCmd())
	root.AddCommand(newRelationsCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// seedEntityConfig writes embed=none so OpenWiring boots without Ollama in tests.
func seedEntityConfig(t *testing.T, dataDir string) {
	t.Helper()
	skipIfShortLiveBackend(t)
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("write entity config: %v", err)
	}
}

// TestEntityCreateRoutesRequestToTool pins Change Set 3c: `maz entity create`
// builds the createEntity-tool request and routes it through the
// daemon-or-in-process path, mapping --identifier, --attributes, and --scope
// onto the tool's input fields. The stubbed tool captures the request and
// returns a canned entity. No t.Parallel — stubs package-level seams.
func TestEntityCreateRoutesRequestToTool(t *testing.T) {
	var got json.RawMessage
	tool := fakeTeamToolFunc{
		name: "createEntity",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			got = req
			return json.RawMessage(
				`{"name":"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV","uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"person","status":"created"}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(),
		"entity", "create", "person", "Rosie Cotton",
		"--identifier", "email:rosie@bree.example",
		"--attributes", `{"team":"ops"}`,
		"--scope", "team",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity create: %v (stderr=%q)", err, errBuf.String())
	}

	var in map[string]any
	if err := json.Unmarshal(got, &in); err != nil {
		t.Fatalf("request not valid JSON: %v (%s)", err, got)
	}
	if in["kind"] != "person" || in["displayName"] != "Rosie Cotton" {
		t.Errorf("request kind/displayName = %v / %v", in["kind"], in["displayName"])
	}
	if in["scope"] != "team" {
		t.Errorf("request scope = %v, want team", in["scope"])
	}
	props, _ := in["properties"].(map[string]any)
	if props["team"] != "ops" {
		t.Errorf("request properties = %v, want team=ops", in["properties"])
	}
	ids, _ := in["identifiers"].([]any)
	if len(ids) != 1 {
		t.Fatalf("request identifiers = %v, want one element", in["identifiers"])
	}
	id0, _ := ids[0].(map[string]any)
	if id0["type"] != "email" || id0["value"] != "rosie@bree.example" {
		t.Errorf("identifier[0] = %v, want email:rosie@bree.example", ids[0])
	}
	if got := strings.TrimSpace(outBuf.String()); got != "entities/01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("printed name = %q, want the tool's entities/{uid}", got)
	}
}

// TestEntityCreateTeamPermissionDeniedExitsOne pins the Change Set 3 exit-code
// mapping: an E_PERMISSION from the routed createEntity tool (a non-member team
// mint) is a bad-input class failure → exit 1. No t.Parallel — stubs seams.
func TestEntityCreateTeamPermissionDeniedExitsOne(t *testing.T) {
	tool := fakeTeamTool{
		name: "createEntity",
		err:  &mcp.ToolError{Code: mcp.CodePermission, Message: "not an admitted team member"},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeEntityRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(),
		"entity", "create", "person", "Rosie Cotton", "--scope", "team",
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected a non-zero exit on a team permission denial")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("want silentError code 1 (bad-input class), got %v", err)
	}
	if !strings.Contains(errBuf.String(), "E_PERMISSION") {
		t.Errorf("stderr should surface the permission error: %q", errBuf.String())
	}
}

// TestEntityCreateHelp: --help is non-empty and includes Examples.
func TestEntityCreateHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeEntityRoot(&buf, nil)
	root.SetArgs([]string{"entity", "create", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"Examples:", "maz entity create", "--attributes"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestBatchSummaryExitCodes pins the chosen --batch exit-code contract: a bulk
// load whose only non-created rows are duplicate-skips (errors:0) exits 0 — the
// dedup-skips are a reported outcome, not a failure — so a script loading a list
// with internal repeats reads success. A row that genuinely errors exits 1.
// (A single `maz entity create` still exits 1 on a duplicate; that is a separate
// path, unchanged.)
func TestBatchSummaryExitCodes(t *testing.T) {
	t.Parallel()
	mk := func() *cobra.Command {
		var out bytes.Buffer
		return makeEntityRoot(&out, nil)
	}

	// errors:0 with only dedup-skips → exit 0.
	if err := printBatchSummary(mk(), &batchCreateSummary{Created: 2, DuplicateCandidates: 1}); err != nil {
		t.Errorf("batch with errors:0 (only dedup-skips) must exit 0, got %v", err)
	}

	// A genuinely errored row → exit 1, even with other rows created.
	err := printBatchSummary(mk(), &batchCreateSummary{Created: 1, Errors: 1})
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("batch with an errored row must exit 1, got %v", err)
	}
}

// TestEntityCreatePrintsResourceName: `maz entity create` prints entities/{uid}
// on stdout and the uid is a valid ULID (26 uppercase Crockford base32 chars).
func TestEntityCreatePrintsResourceName(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	// --output text: bytes.Buffer stdout is not a TTY so without an explicit
	// format the framework defaults to JSON; we want the plain resource name.
	root.SetArgs([]string{"--output", "text", "--data-dir", dir, "entity", "create", "person", "Merry Tester"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity create: %v (stderr=%q)", err, errBuf.String())
	}
	got := strings.TrimSpace(outBuf.String())
	if !strings.HasPrefix(got, "entities/") {
		t.Fatalf("entity create printed %q, want entities/{uid}", got)
	}
	uid := strings.TrimPrefix(got, "entities/")
	if len(uid) != 26 {
		t.Errorf("uid %q has length %d, want 26", uid, len(uid))
	}
}

// TestEntityCreateJSON: --output json produces parseable JSON with name, uid,
// kind, and status fields.
func TestEntityCreateJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--data-dir", dir, "entity", "create", "person", "Sam Test",
		"--output", "json",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity create --output json: %v (stderr=%q)", err, errBuf.String())
	}
	var resp struct {
		Name   string `json:"name"`
		UID    string `json:"uid"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &resp); err != nil {
		t.Fatalf("parse json: %v (out=%q)", err, outBuf.String())
	}
	if !strings.HasPrefix(resp.Name, "entities/") {
		t.Errorf("json name = %q, want entities/{uid}", resp.Name)
	}
	if resp.UID == "" {
		t.Error("json uid is empty")
	}
	if resp.Kind != "person" {
		t.Errorf("json kind = %q, want person", resp.Kind)
	}
	if resp.Status != "created" {
		t.Errorf("json status = %q, want created", resp.Status)
	}
}

// TestEntityCreateMaterialisesWithoutRebuild is the regression test for finding F:
// the entity must be queryable via `maz get` immediately after `entity create`
// with no intervening rebuild.
func TestEntityCreateMaterialisesWithoutRebuild(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	// Use runEntityCreateCmd which forces --output text so we get the bare
	// resource name rather than the full JSON object that the non-TTY default
	// produces.
	name := runEntityCreateCmd(t, dir, "person", "Pippin Queryable")

	// Query via `maz get` without rebuild — the entity must already be in SQLite.
	var getOut, getErr bytes.Buffer
	getRoot := makeEntityRoot(&getOut, &getErr)
	getRoot.SetArgs([]string{"--data-dir", dir, "get", name, "--output", "json"})
	getRoot.SetContext(context.Background())
	if err := getRoot.Execute(); err != nil {
		t.Fatalf("get: %v (stderr=%q)", err, getErr.String())
	}
	if !strings.Contains(getOut.String(), "Pippin Queryable") {
		t.Errorf("entity not queryable without rebuild\nget output: %s", getOut.String())
	}
}

// TestGetHelpIncludesTemporalFlags pins the bi-temporal read plumbing on `maz
// get` (feature #2): --help advertises the new --at / --as-of /
// --include-inactive flags and keeps the worked examples. Pure (no backend).
func TestGetHelpIncludesTemporalFlags(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeEntityRoot(&buf, nil)
	root.SetArgs([]string{"get", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("get help output is empty")
	}
	for _, want := range []string{"Examples:", "maz get", "--at", "--as-of", "--include-inactive"} {
		if !strings.Contains(out, want) {
			t.Errorf("get help missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestGetRejectsBadTimeAnchor pins the exit-code contract: a malformed --at is an
// expected failure (E_VALIDATION) that exits 1, surfaced through the same nav
// error path relations/neighbors use. The anchor is validated before the entity
// lookup, so this holds even for an unknown name. JSON mode still emits the
// shared {error,code} envelope.
func TestGetRejectsBadTimeAnchor(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)
	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "get", "Whoever", "--at", "not-a-time"})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("get --at not-a-time: expected a non-zero exit")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Fatalf("get --at not-a-time exit = %v, want silentError code 1", err)
	}
	var env errorEnvelope
	if jerr := json.Unmarshal(outBuf.Bytes(), &env); jerr != nil {
		t.Fatalf("stdout %q is not a JSON error envelope: %v", outBuf.String(), jerr)
	}
	if env.Code != "E_VALIDATION" {
		t.Errorf("envelope code = %q, want E_VALIDATION", env.Code)
	}
}

// TestGetEntityWithAnchorJSON pins the JSON-mode contract for the threaded
// anchor: `get <entity> --at <t> --output json` returns the same entity record
// (identity row unchanged) as parseable JSON. The entity has no facts, so the
// anchor narrows an empty fact set without disturbing the node.
func TestGetEntityWithAnchorJSON(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)
	name := runEntityCreateCmd(t, dir, "person", "Anchored Tester")

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "get", name, "--at", "2025-01-01T00:00:00Z"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("get --at: %v (stderr=%q)", err, errBuf.String())
	}
	var rec struct {
		Type   string `json:"type"`
		Entity struct {
			DisplayName string `json:"displayName"`
		} `json:"entity"`
	}
	if jerr := json.Unmarshal(outBuf.Bytes(), &rec); jerr != nil {
		t.Fatalf("get --at output is not parseable JSON: %v\n%s", jerr, outBuf.String())
	}
	if rec.Type != "entity" || rec.Entity.DisplayName != "Anchored Tester" {
		t.Fatalf("get --at = %s, want entity Anchored Tester (identity row unchanged)", outBuf.String())
	}
}

// TestEntityCreateRejectsProposedKind pins the Greptile finding: a kind minted
// under governed mode lands status="proposed", and entity create must not mint
// entities under a kind that has not been approved — registry reads only expose
// active kinds.
func TestEntityCreateRejectsProposedKind(t *testing.T) {
	skipIfShortLiveBackend(t)
	dir := t.TempDir()
	// Governed mode parks an explicit `schema kinds add` as proposed.
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
		Ontology:      config.OntologyTable{Mode: "governed"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Register user:vehicle — under governed mode it lands proposed, not active.
	var addOut, addErr bytes.Buffer
	addCmd := makeSchemaCmd(&addOut, &addErr)
	addCmd.SetArgs([]string{
		"schema", "kinds", "add", "user:vehicle",
		"--display-name", "Vehicle",
		"--description", "A motorised transport.",
		"--data-dir", dir,
	})
	if err := addCmd.Execute(); err != nil {
		t.Fatalf("schema kinds add (governed): %v\nstderr: %s", err, addErr.String())
	}

	// entity create against the proposed (not-yet-active) kind must be rejected.
	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "entity", "create", "user:vehicle", "My Car"})
	root.SetContext(context.Background())
	if err := root.Execute(); err == nil {
		t.Fatalf("entity create under a proposed kind expected to fail but exited 0 (stdout=%q)", outBuf.String())
	}
	if stderr := errBuf.String(); !strings.Contains(stderr, "vehicle") && !strings.Contains(stderr, "not registered") {
		t.Errorf("expected rejection naming the unapproved kind, got stderr=%q", stderr)
	}
}

// TestEntityCreateRejectsUnregisteredKind: an unregistered extension kind fails
// with exit non-zero and an actionable message.
func TestEntityCreateRejectsUnregisteredKind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "entity", "create", "user:nosuchthing", "Test Name"})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("entity create with unregistered kind expected to fail but exited 0")
	}
	stderr := errBuf.String()
	if !strings.Contains(stderr, "nosuchthing") && !strings.Contains(stderr, "not registered") {
		t.Errorf("expected actionable error mentioning the unregistered kind\ngot: %s", stderr)
	}
}

// TestEntityCreateCoreKindAccepted: core kinds (person, organization, etc.)
// are always accepted without a registry lookup.
func TestEntityCreateCoreKindAccepted(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	got := runEntityCreateCmd(t, dir, "organization", "TestCorp")
	if !strings.HasPrefix(got, "entities/") {
		t.Fatalf("entity create organization printed %q, want entities/{uid}", got)
	}
}

// TestEntityCreateWithDescription: --description flag is accepted and the
// entity still materialises.
func TestEntityCreateWithDescription(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	got := runEntityCreateCmd(t, dir, "person", "Dave Described", "--description", "Test person.")
	if !strings.HasPrefix(got, "entities/") {
		t.Fatalf("entity create with --description printed %q, want entities/{uid}", got)
	}
}

// TestBuildCreateEntityRequest_DescriptionPersists pins the silent-data-loss
// fix: --description must reach the createEntity tool (folded into properties),
// not be dropped on the floor. It also confirms --description merges with an
// existing --attributes object rather than clobbering it.
func TestBuildCreateEntityRequest_DescriptionPersists(t *testing.T) {
	t.Run("description alone lands in properties", func(t *testing.T) {
		raw, err := buildCreateEntityToolRequest("person", "Dave", "", &entityCreateFlags{description: "a fictional hobbit"})
		if err != nil {
			t.Fatalf("buildCreateEntityToolRequest: %v", err)
		}
		var in struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got := in.Properties["description"]; got != "a fictional hobbit" {
			t.Errorf("properties.description = %v, want %q (the flag must persist, not be dropped)", got, "a fictional hobbit")
		}
	})

	t.Run("description merges with attributes", func(t *testing.T) {
		raw, err := buildCreateEntityToolRequest("person", "Dave", "",
			&entityCreateFlags{attributes: `{"age":42}`, description: "a fictional hobbit"})
		if err != nil {
			t.Fatalf("buildCreateEntityToolRequest: %v", err)
		}
		var in struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if in.Properties["description"] != "a fictional hobbit" || in.Properties["age"] == nil {
			t.Errorf("properties = %v, want both description and age present", in.Properties)
		}
	})

	t.Run("malformed attributes is a clean error", func(t *testing.T) {
		if _, err := buildCreateEntityToolRequest("person", "Dave", "",
			&entityCreateFlags{attributes: `not json`}); err == nil {
			t.Error("expected an error for malformed --attributes, got nil")
		}
	})
}

// TestEntityCreateBareKindShowsUserNamespace pins Finding 2: when a bare
// extension kind (e.g. "vehicle") is canonicalised to "user:vehicle", the
// text output must expose the stored kind so the user knows what to query.
// Before the fix the text output printed only the resource name with no
// mention of the stored kind, leaving the user unable to find the entity.
func TestEntityCreateBareKindShowsUserNamespace(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	// Register user:vehicle so the bare name resolves.
	registerUserKind(t, dir, "user:vehicle", "Vehicle", "A motorised transport.")

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", dir,
		"entity", "create", "vehicle", "My Car",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity create vehicle: %v (stderr=%q)", err, errBuf.String())
	}
	out := outBuf.String()
	// The text output must contain "user:vehicle" so the user knows the
	// actual stored kind and can query it correctly.
	if !strings.Contains(out, "user:vehicle") {
		t.Errorf("text output does not mention stored kind user:vehicle\nout=%q\nstderr=%q", out, errBuf.String())
	}
}

// TestEntityCreateBareKindJSONKindField pins that the JSON output kind field
// is "user:vehicle" (not the bare "vehicle" the user typed).
func TestEntityCreateBareKindJSONKindField(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	registerUserKind(t, dir, "user:vehicle", "Vehicle", "A motorised transport.")

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "json", "--data-dir", dir,
		"entity", "create", "vehicle", "My Car",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity create vehicle --output json: %v (stderr=%q)", err, errBuf.String())
	}
	var resp struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &resp); err != nil {
		t.Fatalf("parse json: %v (out=%q)", err, outBuf.String())
	}
	if resp.Kind != "user:vehicle" {
		t.Errorf("json kind = %q, want user:vehicle", resp.Kind)
	}
}

// TestEntityCreateHelpMentionsUserNamespace pins that --help explains the
// bare -> user: rewrite so users know what namespace their custom kinds live
// in before they create entities.
func TestEntityCreateHelpMentionsUserNamespace(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeEntityRoot(&buf, nil)
	root.SetArgs([]string{"entity", "create", "--help"})
	_ = root.Execute()
	out := buf.String()
	if !strings.Contains(out, "user:") {
		t.Errorf("--help does not mention the user: namespace; users will not know how to query bare custom kinds\nhelp:\n%s", out)
	}
}

// registerUserKind is a test helper that registers an extension kind via
// `maz schema kinds add` so entity create tests can use bare names that
// resolve to user:<kind>.
func registerUserKind(t *testing.T, dataDir, kind, displayName, description string) {
	t.Helper()
	var addOut, addErr bytes.Buffer
	addCmd := makeSchemaCmd(&addOut, &addErr)
	addCmd.SetArgs([]string{
		"schema", "kinds", "add", kind,
		"--display-name", displayName,
		"--description", description,
		"--data-dir", dataDir,
	})
	if err := addCmd.Execute(); err != nil {
		t.Fatalf("schema kinds add %q: %v\nstderr: %s", kind, err, addErr.String())
	}
}

// TestFactSetMaterialisesWithoutRebuild is the regression test for finding B
// on the fact side: `maz fact set` used to append via kernelboot.OpenLog
// (durable but unmaterialised), so a freshly-set fact was invisible to
// `maz fact get` until the operator ran `maz rebuild --yes`.
func TestFactSetMaterialisesWithoutRebuild(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir)

	// Mint a real entity whose facts we will set.
	entityName := runEntityCreateCmd(t, dir, "person", "Fact Tester")
	entityUID := strings.TrimPrefix(entityName, "entities/")

	// Set a fact; force --output text so we get the bare facts/{uid} name.
	var setOut, setErr bytes.Buffer
	setRoot := makeEntityRoot(&setOut, &setErr)
	setRoot.SetArgs([]string{
		"--output", "text", "--data-dir", dir,
		"fact", "set", entityUID, "state.task.status", "pending",
	})
	setRoot.SetContext(context.Background())
	if err := setRoot.Execute(); err != nil {
		t.Fatalf("fact set: %v (stderr=%q)", err, setErr.String())
	}
	factName := strings.TrimSpace(setOut.String())
	if !strings.HasPrefix(factName, "facts/") {
		t.Fatalf("fact set printed %q, want facts/{uid}", factName)
	}

	// Query without any rebuild — the fix means the fact is already
	// materialised.
	var getOut, getErr bytes.Buffer
	getRoot := makeEntityRoot(&getOut, &getErr)
	getRoot.SetArgs([]string{
		"--data-dir", dir, "fact", "get", entityUID, "--output", "json",
	})
	getRoot.SetContext(context.Background())
	if err := getRoot.Execute(); err != nil {
		t.Fatalf("fact get: %v (stderr=%q)", err, getErr.String())
	}
	raw := getOut.String()
	if !strings.Contains(raw, "pending") {
		t.Errorf("fact not queryable without rebuild (silent-until-rebuild regression)\nfact get output: %s", raw)
	}
}
