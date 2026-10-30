package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// makeSchemaCmd wires a cobra command tree in the same shape as
// production so PersistentFlags (--data-dir, --json, --no-tui) are
// accessible.
func makeSchemaCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	schema := newSchemaCmd()
	root.AddCommand(schema)
	if outBuf != nil {
		root.SetOut(outBuf)
		schema.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		schema.SetErr(errBuf)
	}
	return root
}

// TestSchemaPredicatesHelp: --help output is non-empty and includes the
// examples section.
func TestSchemaPredicatesHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSchemaCmd(&buf, nil)
	root.SetArgs([]string{"schema", "predicates", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz schema predicates",
		"--json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRenderPredicatesCLI: plain-text table includes header, separator,
// and all predicate rows in order.
func TestRenderPredicatesCLI(t *testing.T) {
	t.Parallel()
	rows := []predicateRow{
		{
			Predicate:   "lives_in",
			Kind:        "functional",
			Description: "Subject resides in the object location.",
			AddedAt:     time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
			AddedBy:     "seed",
		},
		{
			Predicate:   "works_at",
			Kind:        "functional",
			Description: "Subject is employed at the object organisation.",
			AddedAt:     time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
			AddedBy:     "seed",
		},
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.Flags().String("output", "text", "output format")
	cmd.SetOut(&buf)
	if err := renderPredicatesCLI(cmd, rows); err != nil {
		t.Fatalf("renderPredicatesCLI: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"predicate",
		"kind",
		"domain → range",
		"attrs",
		"added by",
		"added at",
		"description",
		"lives_in",
		"works_at",
		"functional",
		"seed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}

	// lives_in must appear before works_at (caller controls order).
	if strings.Index(out, "lives_in") >= strings.Index(out, "works_at") {
		t.Errorf("ordering violated: lives_in should precede works_at\nfull:\n%s", out)
	}
}

// TestRenderPredicatesJSON: JSON mode emits a parseable array with the
// correct field names per AIP camelCase convention.
func TestRenderPredicatesJSON(t *testing.T) {
	t.Parallel()

	rows := []predicateRow{
		{
			Predicate:   "works_at",
			Kind:        "functional",
			Description: "Subject is employed at the object organisation.",
			AddedAt:     time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
			AddedBy:     "seed",
		},
	}

	var buf bytes.Buffer
	if err := renderPredicatesJSON(&buf, rows); err != nil {
		t.Fatalf("renderPredicatesJSON: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("parse JSON: %v (%q)", err, buf.String())
	}
	if len(parsed) != 1 {
		t.Fatalf("len(parsed) = %d, want 1", len(parsed))
	}
	for _, key := range []string{"predicate", "kind", "description", "addedAt", "addedBy"} {
		if _, ok := parsed[0][key]; !ok {
			t.Errorf("JSON record missing key %q", key)
		}
	}
	if got := parsed[0]["predicate"]; got != "works_at" {
		t.Errorf("predicate = %v, want works_at", got)
	}
	if got := parsed[0]["kind"]; got != "functional" {
		t.Errorf("kind = %v, want functional", got)
	}
}

// TestRenderPredicatesJSON_Empty: an empty table produces a JSON empty
// array, not null.
func TestRenderPredicatesJSON_Empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderPredicatesJSON(&buf, nil); err != nil {
		t.Fatalf("renderPredicatesJSON: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out != jsonNull && out != "[]" {
		// json.Encode(nil slice) emits "null"; both are valid JSON but we
		// document the shape as an array. Accept both to avoid a fragile
		// assertion on json package internals.
		t.Errorf("unexpected empty output: %q (want null or [])", out)
	}
}

// TestSchemaPredicatesCmd_RegisteredInRoot: the schema subcommand is
// reachable from the root command tree.
func TestSchemaPredicatesCmd_RegisteredInRoot(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	schema, _, err := root.Find([]string{"schema", "predicates"})
	if err != nil {
		t.Fatalf("root.Find(schema predicates): %v", err)
	}
	if schema == nil || schema.Name() != "predicates" {
		t.Fatalf("found cmd = %v, want predicates", schema)
	}
}

// TestSchemaPredicatesAdd_BadPropertySchema verifies the add path rejects a
// --property-schema that is valid JSON but does not compile as a JSON Schema
// (e.g. {"type":12}) before appending any event. Catching it here matters
// because strict relation validation later warn-accepts a compile failure
// (treats it as infrastructure), so an un-compilable schema would silently
// disable enforcement.
func TestSchemaPredicatesAdd_BadPropertySchema(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{
		"schema", "predicates", "add", "employed_by",
		"--kind", "non-functional",
		"--description", "Subject is employed by the object.",
		"--property-schema", `{"type":12}`, // valid JSON, invalid JSON Schema
		"--data-dir", t.TempDir(),
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for un-compilable --property-schema, got nil")
	}
	if !strings.Contains(errBuf.String(), "valid JSON Schema") {
		t.Errorf("stderr = %q, want mention of valid JSON Schema", errBuf.String())
	}
}

// TestSchemaPredicatesAdd_BooleanPropertySchema verifies the add path rejects a
// boolean (or other non-object) --property-schema before appending any event.
// `true`/`false` are valid JSON Schema documents that compile cleanly, but a
// `true` schema accepts every attributes map — the relation would look typed
// while no constraint could ever fire. The object-shape gate must catch it.
func TestSchemaPredicatesAdd_BooleanPropertySchema(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{`true`, `false`, `42`, `[1,2,3]`, `"x"`} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			root := makeSchemaCmd(&out, &errBuf)
			root.SetArgs([]string{
				"schema", "predicates", "add", "employed_by",
				"--kind", "non-functional",
				"--description", "Subject is employed by the object.",
				"--property-schema", bad,
				"--data-dir", t.TempDir(),
			})
			if err := root.Execute(); err == nil {
				t.Fatalf("expected error for non-object --property-schema %q, got nil", bad)
			}
			if !strings.Contains(errBuf.String(), "JSON Schema object") {
				t.Errorf("stderr = %q, want mention of JSON Schema object", errBuf.String())
			}
		})
	}
}

// ---- alias relation tests ----

// TestSchemaAlias_RejectedRelations verifies that the hierarchical/associative
// SKOS relations (which have no typed storage) are rejected with exit 1 and a
// message pointing at the storage limitation — the relation guard runs before
// any wiring, so no data dir is needed.
func TestSchemaAlias_RejectedRelations(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"closeMatch", "broadMatch", "narrowMatch", "relatedMatch"} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			root := makeSchemaCmd(&out, &errBuf)
			root.SetArgs([]string{"schema", "alias", "user:vehicle", "https://schema.org/Vehicle", rel})
			if err := root.Execute(); err == nil {
				t.Fatalf("expected error for relation %q, got nil", rel)
			}
			if !strings.Contains(errBuf.String(), "no typed storage") {
				t.Errorf("stderr = %q, want no-typed-storage message", errBuf.String())
			}
		})
	}
}

// TestSchemaAlias_UnknownRelation verifies a relation that is not a SKOS
// alignment term is rejected with the supported-relations hint.
func TestSchemaAlias_UnknownRelation(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{"schema", "alias", "user:vehicle", "https://schema.org/Vehicle", "isLike"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown relation, got nil")
	}
	if !strings.Contains(errBuf.String(), "unknown SKOS relation") {
		t.Errorf("stderr = %q, want unknown-relation message", errBuf.String())
	}
}

// TestSchemaAlias_EquivalenceRelationsPassGuard verifies the equivalence
// relations (and the default) pass the relation guard: a core-kind target then
// trips the core-kind guard (exit 1, distinct message), proving the relation
// itself was accepted without needing a data dir.
func TestSchemaAlias_EquivalenceRelationsPassGuard(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"schema", "alias", "person", "https://schema.org/Person"},               // default sameAs
		{"schema", "alias", "person", "https://schema.org/Person", "sameAs"},     // explicit sameAs
		{"schema", "alias", "person", "https://schema.org/Person", "exactMatch"}, // exactMatch
	} {
		t.Run(strings.Join(args[3:], " "), func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			root := makeSchemaCmd(&out, &errBuf)
			root.SetArgs(args)
			if err := root.Execute(); err == nil {
				t.Fatal("expected core-kind refusal, got nil")
			}
			// Must reach the core-kind guard, not the relation guard.
			if !strings.Contains(errBuf.String(), "core kind") {
				t.Errorf("stderr = %q, want core-kind refusal (relation should have passed)", errBuf.String())
			}
			if strings.Contains(errBuf.String(), "SKOS relation") || strings.Contains(errBuf.String(), "typed storage") {
				t.Errorf("stderr = %q, relation was wrongly rejected", errBuf.String())
			}
		})
	}
}

// TestSchemaAliasHelp verifies the alias help documents the relation argument
// and the supported/unsupported relations.
func TestSchemaAliasHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSchemaCmd(&buf, nil)
	root.SetArgs([]string{"schema", "alias", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"relation", "sameAs", "exactMatch", "closeMatch"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// ---- entity-kinds subcommand tests ----

// TestSchemaEntityKindsHelp: --help output is non-empty and includes the
// examples section.
func TestSchemaEntityKindsHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSchemaCmd(&buf, nil)
	root.SetArgs([]string{"schema", "entity-kinds", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz schema entity-kinds",
		"--json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRenderEntityKindsCLI: plain-text table includes header, separator, and
// all entity-kind rows in order.
func TestRenderEntityKindsCLI(t *testing.T) {
	t.Parallel()
	rows := []entityKindRow{
		{
			Kind:        "organisation",
			DisplayName: "Organisation",
			Provenance:  "core",
			Extends:     "",
			SameAs:      []string{"https://schema.org/Organization"},
			Count:       7,
			Description: "A legal entity.",
			AddedAt:     time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
			AddedBy:     "seed",
		},
		{
			Kind:        "person",
			DisplayName: "Person",
			Provenance:  "core",
			Extends:     "",
			SameAs:      nil,
			Count:       42,
			Description: "A human individual.",
			AddedAt:     time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
			AddedBy:     "seed",
		},
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.Flags().String("output", "text", "output format")
	cmd.SetOut(&buf)
	if err := renderEntityKindsCLI(cmd, rows); err != nil {
		t.Fatalf("renderEntityKindsCLI: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"kind", "display name", "provenance", "extends", "same as", "count", "description",
		"organisation", "Organisation", "core",
		"https://schema.org/Organization",
		"person", "Person",
		"7", "42",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}

	// organisation must appear before person (alphabetical order from caller).
	if strings.Index(out, "organisation") >= strings.Index(out, "person") {
		t.Errorf("ordering violated: organisation should precede person\nfull:\n%s", out)
	}
}

// TestRenderEntityKindsJSON: JSON mode emits a parseable array with the
// correct field names per AIP camelCase convention.
func TestRenderEntityKindsJSON(t *testing.T) {
	t.Parallel()

	rows := []entityKindRow{
		{
			Kind:        "person",
			DisplayName: "Person",
			Provenance:  "core",
			Extends:     "",
			SameAs:      []string{"https://schema.org/Person"},
			Count:       3,
			Description: "A human individual.",
			AddedAt:     time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
			AddedBy:     "seed",
		},
	}

	var buf bytes.Buffer
	if err := renderEntityKindsJSON(&buf, rows); err != nil {
		t.Fatalf("renderEntityKindsJSON: %v", err)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("parse JSON: %v (%q)", err, buf.String())
	}
	if len(parsed) != 1 {
		t.Fatalf("len(parsed) = %d, want 1", len(parsed))
	}
	for _, key := range []string{"kind", "displayName", "provenance", "extends", "sameAs", "count", "description", "addedAt", "addedBy"} {
		if _, ok := parsed[0][key]; !ok {
			t.Errorf("JSON record missing key %q", key)
		}
	}
	if got := parsed[0]["kind"]; got != "person" {
		t.Errorf("kind = %v, want person", got)
	}
	if got := parsed[0]["provenance"]; got != "core" {
		t.Errorf("provenance = %v, want core", got)
	}
	sameAs, ok := parsed[0]["sameAs"].([]interface{})
	if !ok || len(sameAs) != 1 || sameAs[0] != "https://schema.org/Person" {
		t.Errorf("sameAs = %v, want [https://schema.org/Person]", parsed[0]["sameAs"])
	}
}

// TestRenderEntityKindsJSON_Empty: an empty table produces a JSON null (or []),
// not an error.
func TestRenderEntityKindsJSON_Empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderEntityKindsJSON(&buf, nil); err != nil {
		t.Fatalf("renderEntityKindsJSON: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if out != jsonNull && out != "[]" {
		t.Errorf("unexpected empty output: %q (want null or [])", out)
	}
}

// TestSchemaEntityKindsCmd_RegisteredInRoot: the entity-kinds subcommand is
// reachable from the root command tree.
func TestSchemaEntityKindsCmd_RegisteredInRoot(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"schema", "entity-kinds"})
	if err != nil {
		t.Fatalf("root.Find(schema entity-kinds): %v", err)
	}
	if cmd == nil || cmd.Name() != "entity-kinds" {
		t.Fatalf("found cmd = %v, want entity-kinds", cmd)
	}
}

// TestSchemaEntityKindsCmd_HasSchemaAnnotation: the entity-kinds command carries
// the schema.response annotation so maz schema show can introspect it.
func TestSchemaEntityKindsCmd_HasSchemaAnnotation(t *testing.T) {
	t.Parallel()
	cmd := newSchemaEntityKindsCmd()
	ref, ok := cmd.Annotations["schema.response"]
	if !ok || ref == "" {
		t.Fatalf("entity-kinds command must carry schema.response annotation, got %v", cmd.Annotations)
	}
}

// ---- standalone materialization (silent-until-rebuild fix) ----

// TestSchemaKindsAdd_MaterialisesWithoutRebuild is the regression test for the
// silent-until-rebuild bug: a daemon-less `maz schema kinds add` used to append
// an entityKindRegistered event via kernelboot.OpenLog (durable but NOT
// materialised), so the freshly-minted kind was invisible to `maz schema kinds
// list` until the operator separately ran `maz rebuild --yes`. The fix routes
// the write through openMaterializingLog (the dispatcher-backed FileEventLog the
// daemon itself uses), so the add materialises synchronously and the kind is
// queryable the instant the command returns.
//
// The test asserts the round-trip end to end against one data dir, no rebuild
// in between. Before the fix the second command's output omits the kind.
func TestSchemaKindsAdd_MaterialisesWithoutRebuild(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: each command opens the data dir's exclusive store lock
	// in sequence; running siblings against the same dir would contend.
	dir := t.TempDir()
	// embed=none lets OpenWiring boot without an Ollama probe; the registry,
	// dispatcher, and SQLite projection all open regardless.
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Mint a non-core kind.
	var addOut, addErr bytes.Buffer
	addCmd := makeSchemaCmd(&addOut, &addErr)
	addCmd.SetArgs([]string{
		"schema", "kinds", "add", "user:vehicle",
		"--display-name", "Vehicle",
		"--description", "A motorised transport.",
		"--data-dir", dir,
	})
	if err := addCmd.Execute(); err != nil {
		t.Fatalf("schema kinds add: %v\nstderr: %s", err, addErr.String())
	}

	// List WITHOUT any intervening rebuild. The fix means the kind is already
	// materialised into entity_kinds, so the read sees it.
	var listOut, listErr bytes.Buffer
	listCmd := makeSchemaCmd(&listOut, &listErr)
	listCmd.SetArgs([]string{"schema", "kinds", "list", "--json", "--data-dir", dir})
	if err := listCmd.Execute(); err != nil {
		t.Fatalf("schema kinds list: %v\nstderr: %s", err, listErr.String())
	}

	if !strings.Contains(listOut.String(), "user:vehicle") {
		t.Errorf("kind not queryable without rebuild — silent-until-rebuild regression\nlist output:\n%s", listOut.String())
	}
}

// TestSchemaAlias_MaterialisesExactIRI pins the Greptile P1 follow-up on the
// alias read-back: verification uses exact json_each membership, not a LIKE
// probe, so an IRI containing a SQL-LIKE wildcard ("%") must still round-trip
// and pass — and land verbatim in same_as. A LIKE probe would either choke or
// mask a swallowed Apply failure by matching a different alias.
func TestSchemaAlias_MaterialisesExactIRI(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: sequential commands share the data dir's exclusive lock.
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	addCmd := makeSchemaCmd(nil, nil)
	addCmd.SetArgs([]string{
		"schema", "kinds", "add", "user:vehicle",
		"--display-name", "Vehicle", "--description", "A motorised transport.",
		"--data-dir", dir,
	})
	if err := addCmd.Execute(); err != nil {
		t.Fatalf("schema kinds add: %v", err)
	}

	const iri = "https://example.com/a%b" // contains a SQL-LIKE wildcard
	var aliasErr bytes.Buffer
	aliasCmd := makeSchemaCmd(nil, &aliasErr)
	aliasCmd.SetArgs([]string{"schema", "alias", "user:vehicle", iri, "--data-dir", dir})
	if err := aliasCmd.Execute(); err != nil {
		t.Fatalf("schema alias with wildcard IRI: %v\nstderr: %s", err, aliasErr.String())
	}

	var showOut bytes.Buffer
	showCmd := makeSchemaCmd(&showOut, nil)
	showCmd.SetArgs([]string{"schema", "kinds", "show", "user:vehicle", "--json", "--data-dir", dir})
	if err := showCmd.Execute(); err != nil {
		t.Fatalf("schema kinds show: %v", err)
	}
	if !strings.Contains(showOut.String(), iri) {
		t.Errorf("exact IRI %q not materialised into same_as\nshow output:\n%s", iri, showOut.String())
	}
}
