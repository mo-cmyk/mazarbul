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

// makeRelateRoot wires a minimal cobra root for relate + entity + relations
// tests. It matches the persistent-flags shape the commands read via
// cmd.Root().PersistentFlags().
func makeRelateRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newRelateCmd())
	root.AddCommand(newEntityCmd())
	root.AddCommand(newRelationsCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// seedConfig writes embed=none to the data dir so OpenWiring boots without
// an Ollama probe in the test environment.
// skipIfShortLiveBackend skips a test under `go test -short`. Tests that seed a
// data dir do so to boot the full in-process backend (28 migrations + ladybug
// graph + sqlite-vec) and drive real `maz` commands end-to-end — each boot
// costs ~3-12s, so the cli `-short` suite would otherwise run ~167s and trip the
// 30s Stop-gate. CI runs the package WITHOUT `-short` (`go test -timeout 5m
// ./...`), so these still execute there; `-short` is the fast local subset.
func skipIfShortLiveBackend(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("live in-process backend e2e; runs in CI without -short")
	}
}

// TestRebindSelfToPrincipal_CLIWritePath guards the #209 Part-F gap that live
// CLI testing caught: `maz relate` / `maz fact` have their own native write path
// (buildRelationCLI / buildFact, appending straight to the log) separate from the
// MCP relate/fact tools, and originally did NOT rebind the "self" sentinel — so
// `maz relate self knows X` persisted the local seeded "self" row instead of the
// bound principal. rebindSelfToPrincipal is the shared fix; this pins its cases.
// No live backend needed (pure state.toml read), so it runs under -short.
func TestRebindSelfToPrincipal_CLIWritePath(t *testing.T) {
	t.Parallel()
	const principal = "01PRINCIPAL000000000000001"
	bound := t.TempDir()
	if err := config.BindSelfPrincipal(bound, principal, "01DEVICE0000000000000000001", "kid:test"); err != nil {
		t.Fatalf("bind self: %v", err)
	}
	if got := rebindSelfToPrincipal(bound, "self"); got != principal {
		t.Errorf("bound self rebind = %q, want principal %q", got, principal)
	}
	// A concrete ULID is never rebound.
	const concrete = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if got := rebindSelfToPrincipal(bound, concrete); got != concrete {
		t.Errorf("non-self uid rebind = %q, want unchanged %q", got, concrete)
	}
	// Unbound device: "self" stays verbatim (pre-identity behaviour preserved).
	if got := rebindSelfToPrincipal(t.TempDir(), "self"); got != "self" {
		t.Errorf("unbound self rebind = %q, want \"self\"", got)
	}
}

func seedConfig(t *testing.T, dataDir string) {
	t.Helper()
	skipIfShortLiveBackend(t)
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// runEntityCreateCmd calls `maz entity create <kind> <displayName>` with
// --output text and returns the printed entities/{uid} name (trimmed) or fatals.
func runEntityCreateCmd(t *testing.T, dataDir, kind, displayName string, extraArgs ...string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeRelateRoot(&outBuf, &errBuf)
	// Force --output text: bytes.Buffer stdout is not a TTY, so without an
	// explicit format the framework defaults to JSON. The test helper only
	// needs the resource name line.
	args := append(
		[]string{"--data-dir", dataDir, "--output", "text", "entity", "create", kind, displayName},
		extraArgs...,
	)
	root.SetArgs(args)
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity create: %v (stderr=%q)", err, errBuf.String())
	}
	return strings.TrimSpace(outBuf.String())
}

// runRelateCmd calls `maz relate <subjectUID> <predicate> <objectUID>` with
// --output text and returns the printed relations/{uid} name or fatals.
func runRelateCmd(t *testing.T, dataDir, subjectUID, predicate, objectUID string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeRelateRoot(&outBuf, &errBuf)
	// Force --output text: bytes.Buffer stdout is not a TTY so without an
	// explicit format the framework defaults to JSON.
	root.SetArgs([]string{"--output", "text", "--data-dir", dataDir, "relate", subjectUID, predicate, objectUID})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("relate: %v (stderr=%q)", err, errBuf.String())
	}
	return strings.TrimSpace(outBuf.String())
}

// runRelateExpectFail calls `maz relate …` and expects a non-zero exit.
// It returns the stderr text for assertion.
func runRelateExpectFail(t *testing.T, dataDir, subjectUID, predicate, objectUID string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeRelateRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "relate", subjectUID, predicate, objectUID})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("relate expected to fail but exited 0")
	}
	return errBuf.String()
}

// TestRelateRejectsUnknownScope pins the #213 write-scope param on relate: an
// out-of-enum --scope is clean bad input (exit 1) before any storage work.
func TestRelateRejectsUnknownScope(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	root := makeRelateRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", t.TempDir(), "relate", "self", "knows", "self", "--scope", "bogus"})
	root.SetContext(context.Background())
	if err := root.Execute(); err == nil {
		t.Fatal("relate --scope bogus expected to fail but exited 0")
	}
	if !strings.Contains(errBuf.String(), `scope "bogus" is not in`) {
		t.Errorf("unknown scope should be rejected, got stderr=%q", errBuf.String())
	}
}

// TestRelateRejectsTeamScopeSelf pins the team-self guard on relate: a
// team-scope edge referencing the unresolved "self" sentinel is refused.
func TestRelateRejectsTeamScopeSelf(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	root := makeRelateRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", t.TempDir(), "relate", "self", "knows", "self", "--scope", "team"})
	root.SetContext(context.Background())
	if err := root.Execute(); err == nil {
		t.Fatal("relate --scope team self expected to fail but exited 0")
	}
	if !strings.Contains(errBuf.String(), `"self" sentinel`) {
		t.Errorf("team-scope self edge should be rejected, got stderr=%q", errBuf.String())
	}
}

// runRelationsQuery calls `maz relations <entity>` and returns stdout trimmed.
func runRelationsQuery(t *testing.T, dataDir, entity string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeRelateRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "relations", entity, "--output", "json"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("relations: %v (stderr=%q)", err, errBuf.String())
	}
	return strings.TrimSpace(outBuf.String())
}

// TestRelateHelp: --help output is non-empty and includes Examples.
func TestRelateHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeRelateRoot(&buf, nil)
	root.SetArgs([]string{"relate", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"Examples:", "maz relate", "--valid-from"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRelateMaterialisesWithoutRebuild is the regression test for finding B:
// `maz relate` used to append via kernelboot.OpenLog (durable but unmaterialised),
// so the relation was invisible to `maz relations` until the operator ran
// `maz rebuild --yes`. The fix routes the write through openMaterializingLog,
// so the relation is queryable the instant relate returns.
func TestRelateMaterialisesWithoutRebuild(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	// Mint two real entities so the existence check passes.
	subjectName := runEntityCreateCmd(t, dir, "person", "Merry Tester")
	objectName := runEntityCreateCmd(t, dir, "organization", "Mithril Corp")

	subjectUID := strings.TrimPrefix(subjectName, "entities/")
	objectUID := strings.TrimPrefix(objectName, "entities/")

	relName := runRelateCmd(t, dir, subjectUID, "works_at", objectUID)
	if !strings.HasPrefix(relName, "relations/") {
		t.Fatalf("relate printed %q, want relations/{uid}", relName)
	}

	// Query without any rebuild — the fix means the relation is already
	// materialised into v_relations.
	raw := runRelationsQuery(t, dir, subjectUID)
	if !strings.Contains(raw, "works_at") {
		t.Errorf("relation not queryable without rebuild (silent-until-rebuild regression)\nrelations output: %s", raw)
	}
}

// TestRelateRejectsNonexistentUIDs is the regression test for finding E:
// `maz relate` used to accept random UIDs and persist dangling rows with NULL
// subject_name/object_name. The fix validates each UID against the entities
// projection before appending and fails with exit non-zero + an actionable
// message.
func TestRelateRejectsNonexistentUIDs(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	const fakeUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	stderr := runRelateExpectFail(t, dir, fakeUID, "works_at", fakeUID)
	if !strings.Contains(stderr, fakeUID) {
		t.Errorf("expected stderr to name the missing UID %q\ngot: %s", fakeUID, stderr)
	}
	// The routed relate tool surfaces an E_NOT_FOUND whose actionable hint
	// points at creating the entity first (remember / a known entities/{uid}).
	if !strings.Contains(stderr, "create it first") && !strings.Contains(stderr, "entities/{uid}") {
		t.Errorf("expected actionable create-it-first hint\ngot: %s", stderr)
	}
}

// TestRelateRejectsNonexistentObjectUID checks that a valid subject but missing
// object also fails with an actionable message naming the object UID.
func TestRelateRejectsNonexistentObjectUID(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	subjectName := runEntityCreateCmd(t, dir, "person", "Sam Tester")
	subjectUID := strings.TrimPrefix(subjectName, "entities/")

	const fakeObjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FBW"
	stderr := runRelateExpectFail(t, dir, subjectUID, "works_at", fakeObjectUID)
	if !strings.Contains(stderr, fakeObjectUID) {
		t.Errorf("expected stderr to name the missing object UID %q\ngot: %s", fakeObjectUID, stderr)
	}
}

// TestRelateJSON: --output json produces parseable JSON with a "name" field.
func TestRelateJSON(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	subjectName := runEntityCreateCmd(t, dir, "person", "Pippin Test")
	objectName := runEntityCreateCmd(t, dir, "organization", "Test Corp")
	subjectUID := strings.TrimPrefix(subjectName, "entities/")
	objectUID := strings.TrimPrefix(objectName, "entities/")

	var outBuf2, errBuf2 bytes.Buffer
	root := makeRelateRoot(&outBuf2, &errBuf2)
	root.SetArgs([]string{
		"--data-dir", dir, "relate", subjectUID, "works_at", objectUID,
		"--output", "json",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("relate --output json: %v (stderr=%q)", err, errBuf2.String())
	}
	var resp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf2.String())), &resp); err != nil {
		t.Fatalf("parse json: %v (out=%q)", err, outBuf2.String())
	}
	if !strings.HasPrefix(resp.Name, "relations/") {
		t.Errorf("json name = %q, want relations/{uid}", resp.Name)
	}
}

// TestRelateRegistersNovelPredicate pins Finding 4: when `maz relate` uses a
// predicate that is not already in the predicates table, that predicate must
// be auto-registered so it appears in `maz schema predicates`. Before the fix
// the relation was created but the predicate vocabulary stayed silent about
// it, making `maz introspect` lie about the navigable edge vocabulary.
func TestRelateRegistersNovelPredicate(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	// Mint two entities to satisfy the existence check.
	subjName := runEntityCreateCmd(t, dir, "person", "Merry Novel")
	objName := runEntityCreateCmd(t, dir, "person", "Sam Novel")
	subjectUID := strings.TrimPrefix(subjName, "entities/")
	objectUID := strings.TrimPrefix(objName, "entities/")

	// Use a predicate that is definitely not in the seed vocabulary.
	const novelPredicate = "novel_test_predicate_xyz"

	relName := runRelateCmd(t, dir, subjectUID, novelPredicate, objectUID)
	if !strings.HasPrefix(relName, "relations/") {
		t.Fatalf("relate printed %q, want relations/{uid}", relName)
	}

	// Now query schema predicates — the novel predicate must be present.
	predicatesJSON := runSchemaPredicatesCmd(t, dir)
	if !strings.Contains(predicatesJSON, novelPredicate) {
		t.Errorf("novel predicate %q not found in schema predicates after relate\npredicates: %s", novelPredicate, predicatesJSON)
	}
}

// runSchemaPredicatesCmd calls `maz schema predicates --json` and returns
// the stdout (the JSON array of predicates) for assertion.
func runSchemaPredicatesCmd(t *testing.T, dataDir string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeSchemaCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "schema", "predicates", "--json"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("schema predicates: %v (stderr=%q)", err, errBuf.String())
	}
	return strings.TrimSpace(outBuf.String())
}

// TestRelateNameResolution covers display-name resolution for subject and object
// arguments: passing a human display name, a mixed (UID + name) pair, the "self"
// sentinel alongside a name, and an unknown name that must produce a clear error.
// All sub-cases require a live in-process backend, guarded by skipIfShortLiveBackend.
func TestRelateNameResolution(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	// Pre-create two named entities used across sub-cases.
	subjName := runEntityCreateCmd(t, dir, "person", "Peregrin Took")
	objName := runEntityCreateCmd(t, dir, "organization", "The Shire Council")
	subjUID := strings.TrimPrefix(subjName, "entities/")
	objUID := strings.TrimPrefix(objName, "entities/")

	t.Run("name subject and name object", func(t *testing.T) {
		got := runRelateCmd(t, dir, "Peregrin Took", "member_of", "The Shire Council")
		if !strings.HasPrefix(got, "relations/") {
			t.Fatalf("relate by name → want relations/{uid}, got %q", got)
		}
	})

	t.Run("ULID subject and name object", func(t *testing.T) {
		got := runRelateCmd(t, dir, subjUID, "member_of", "The Shire Council")
		if !strings.HasPrefix(got, "relations/") {
			t.Fatalf("relate uid+name → want relations/{uid}, got %q", got)
		}
	})

	t.Run("name subject and ULID object", func(t *testing.T) {
		got := runRelateCmd(t, dir, "Peregrin Took", "member_of", objUID)
		if !strings.HasPrefix(got, "relations/") {
			t.Fatalf("relate name+uid → want relations/{uid}, got %q", got)
		}
	})

	t.Run("self subject and name object", func(t *testing.T) {
		// "self" is the unbound sentinel when no principal is set; it is
		// accepted as a valid endpoint (relation is written referencing "self").
		got := runRelateCmd(t, dir, "self", "knows", "Peregrin Took")
		if !strings.HasPrefix(got, "relations/") {
			t.Fatalf("relate self+name → want relations/{uid}, got %q", got)
		}
	})

	t.Run("unknown name gives clear error", func(t *testing.T) {
		stderr := runRelateExpectFail(t, dir, "No Such Entity Ever", "knows", "Peregrin Took")
		if !strings.Contains(stderr, "No Such Entity Ever") &&
			!strings.Contains(stderr, "not found") &&
			!strings.Contains(stderr, "no entity") {
			t.Errorf("expected clear not-found error for unknown name, got stderr=%q", stderr)
		}
	})
}

// TestRelateFlagNames asserts that --valid-from and --valid-until are the
// registered flag names (kebab-case, consistent with every other CLI flag) and
// that the old camelCase spellings --validFrom / --validUntil are NOT
// registered (pre-prod clean cut, no alias window).
func TestRelateFlagNames(t *testing.T) {
	t.Parallel()
	cmd := newRelateCmd()
	flags := cmd.Flags()

	// New kebab-case flags must be present.
	if flags.Lookup("valid-from") == nil {
		t.Error("flag --valid-from not registered on relate")
	}
	if flags.Lookup("valid-until") == nil {
		t.Error("flag --valid-until not registered on relate")
	}

	// Old camelCase spellings must NOT be registered (clean rename, no alias).
	if flags.Lookup("validFrom") != nil {
		t.Error("flag --validFrom is still registered; expected clean rename to --valid-from")
	}
	if flags.Lookup("validUntil") != nil {
		t.Error("flag --validUntil is still registered; expected clean rename to --valid-until")
	}
}

// TestJSONFlagVisible asserts that --json is a discoverable (non-hidden)
// persistent flag on the root command so it appears in `maz --help` output.
func TestJSONFlagVisible(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	pf := root.PersistentFlags()

	f := pf.Lookup("json")
	if f == nil {
		t.Fatal("persistent flag --json not registered on root command")
	}
	if f.Hidden {
		t.Error("persistent flag --json is hidden; it must be visible so it appears in --help")
	}
}

// TestRelateRejectsInvalidPredicateName pins the Greptile finding: `maz relate`
// must reject predicate names that do not match the snake_case pattern that
// `schema predicates add` enforces. Without this guard a relation can be created
// with a predicate name (e.g. "Works At") that no other CLI surface will accept,
// leaving the vocabulary inconsistent. Expected exit non-zero with an actionable
// message.
func TestRelateRejectsInvalidPredicateName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedConfig(t, dir)

	for _, bad := range []string{"Works At", "has-role", "SHOUTING", "1starts_with_digit"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			t.Parallel()
			var outBuf, errBuf bytes.Buffer
			root := makeRelateRoot(&outBuf, &errBuf)
			root.SetArgs([]string{
				"--data-dir", dir, "relate",
				"01ARZ3NDEKTSV4RRFFQ69G5FAV", bad, "01ARZ3NDEKTSV4RRFFQ69G5FBW",
			})
			root.SetContext(context.Background())
			err := root.Execute()
			if err == nil {
				t.Fatalf("relate with invalid predicate %q expected to fail but exited 0", bad)
			}
			stderr := errBuf.String()
			if !strings.Contains(stderr, "snake_case") && !strings.Contains(stderr, "predicate") {
				t.Errorf("expected actionable message mentioning snake_case; got: %s", stderr)
			}
		})
	}
}

// TestRelateAcceptsEntitiesPrefix pins the newcomer fix: `maz entity create`
// prints the entities/{uid} resource name, and the relate help advertises both
// the bare ULID and that resource-name form. Pasting the printed name straight
// into relate must succeed — the prefix is stripped before the existence check
// rather than failing with `entity "entities/…" does not exist`.
func TestRelateAcceptsEntitiesPrefix(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)
	// entity create prints the full entities/{uid} resource name.
	subjName := runEntityCreateCmd(t, dir, "person", "Merry Prefix")
	objName := runEntityCreateCmd(t, dir, "organization", "Mithril Prefix")
	if !strings.HasPrefix(subjName, "entities/") || !strings.HasPrefix(objName, "entities/") {
		t.Fatalf("precondition: entity create should print entities/{uid}, got %q / %q", subjName, objName)
	}
	// Feed those resource names verbatim (with the entities/ prefix) into relate.
	got := runRelateCmd(t, dir, subjName, "works_at", objName)
	if !strings.HasPrefix(got, "relations/") {
		t.Fatalf("relate with entities/ prefix → want relations/{uid}, got %q", got)
	}
}

// TestRelateRoutesRequestToTool pins Change Set 3b: `maz relate` builds the
// relate-tool request {subject, predicate, object, source, scope} (with the
// entities/ prefix stripped) and routes it through the daemon-or-in-process
// path. The stubbed tool registers under "relate" — NOT ontology.KindRelation
// ("relation") — and captures the request. No t.Parallel — stubs seams.
func TestRelateRoutesRequestToTool(t *testing.T) {
	var got json.RawMessage
	tool := fakeTeamToolFunc{
		name: "relate",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			got = req
			return json.RawMessage(`{"name":"relations/01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"asserted"}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeRelateRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(), "relate",
		"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV", "works_at", "01ARZ3NDEKTSV4RRFFQ69G5FBW",
		"--source", "cli:relate", "--scope", "project",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("relate: %v (stderr=%q)", err, errBuf.String())
	}

	var in map[string]any
	if err := json.Unmarshal(got, &in); err != nil {
		t.Fatalf("request not valid JSON: %v (%s)", err, got)
	}
	if in["subject"] != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("request subject = %v, want the entities/ prefix stripped", in["subject"])
	}
	if in["predicate"] != "works_at" || in["object"] != "01ARZ3NDEKTSV4RRFFQ69G5FBW" {
		t.Errorf("request edge = %v %v", in["predicate"], in["object"])
	}
	if in["source"] != "cli:relate" {
		t.Errorf("request source = %v, want cli:relate", in["source"])
	}
	if in["scope"] != "project" {
		t.Errorf("request scope = %v, want project", in["scope"])
	}
	if got := strings.TrimSpace(outBuf.String()); got != "relations/01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("printed name = %q, want the tool's relations/{uid}", got)
	}
}

// TestRelateTeamPermissionDeniedExitsOne pins the Change Set 3 exit-code
// mapping: an E_PERMISSION from the routed relate tool (a non-member team
// write) is a bad-input class failure → exit 1. No t.Parallel — stubs seams.
func TestRelateTeamPermissionDeniedExitsOne(t *testing.T) {
	tool := fakeTeamTool{
		name: "relate",
		err:  &mcp.ToolError{Code: mcp.CodePermission, Message: "not an admitted team member"},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeRelateRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(), "relate",
		"01ARZ3NDEKTSV4RRFFQ69G5FAV", "works_at", "01ARZ3NDEKTSV4RRFFQ69G5FBW", "--scope", "team",
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
