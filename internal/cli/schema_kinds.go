package cli

// schema_kinds.go implements the "maz schema kinds" authoring subcommand
// tree:
//
//   maz schema kinds list      — thin alias for maz schema entity-kinds
//   maz schema kinds add       — append an entityKindRegistered event
//   maz schema kinds show      — single-kind detail with parent chain + children
//
// Also implements:
//   maz schema alias <kind> <iri>   — add a sameAs IRI to any non-core kind
//   maz schema predicates add       — append a predicateRegistered event
//
// Write pathway: openMaterializingLog → dispatcher-backed FileEventLog. The
// entityKindRegistered / predicateRegistered append materialises synchronously,
// so the kind/predicate is queryable the instant the command returns — no
// `maz rebuild`. It refuses when a daemon already serves the data dir (the
// daemon holds the exclusive store lock); that write must go through the
// daemon's MCP authoring tool instead.
// Read pathway: daemon.OpenWiring → OpenSQLite (identical to schema.go list/show).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// coreKindNames lists the six built-in entity kinds whose provenance is
// "core". They are guarded at the dispatcher layer from non-core overwrites.
// The alias command refuses to emit a user-provenance event for these kinds
// because the dispatcher's core-provenance guard would silently discard it —
// governance of core kinds is out of scope for the human-authoring surface.
//
//nolint:gochecknoglobals // read-only lookup set
var coreKindNames = map[string]struct{}{
	"person":           {},
	"organization":     {},
	"place":            {},
	"project":          {},
	"event":            {},
	entityKindDocument: {},
}

// kindNamePattern matches the EntityKindRegisteredEvent.Name contract:
// bare snake_case (e.g. vehicle) or namespaced (e.g. user:research_paper).
var kindNamePattern = regexp.MustCompile(`^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$`)

// predicateNamePattern matches the predicateRegistered.Predicate contract:
// bare snake_case (e.g. works_at). Mirrors the MCP definePredicate tool so the
// CLI and agent surfaces reject the same malformed names.
var predicateNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// mintKindName maps a bare local name to the user: namespace for minting a NEW
// kind, consistent with the MCP defineEntityKind tool (which always prefixes
// user:) so the CLI never pollutes the bare/core namespace. A name that already
// carries a namespace (contains a colon) or names one of the six core kinds is
// returned unchanged. Use this ONLY on the write path (the kind being created);
// read/update paths must resolve against the registry via resolveRegisteredKind.
func mintKindName(name string) string {
	if strings.Contains(name, ":") {
		return name
	}
	if _, isCore := coreKindNames[name]; isCore {
		return name
	}
	return "user:" + name
}

// localKindSegment returns the local name of a (possibly namespaced) kind: the
// substring after the last colon, or the whole string when un-namespaced.
func localKindSegment(name string) string {
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// resolveRegisteredKind resolves arg to a single registered kind name using the
// same exact/local/ambiguous model as the MCP describeType tool, so read/update
// surfaces (show, alias, --extends) agree on which stored kind a bare name
// targets instead of blindly minting a user: default. An exact full-name match
// (person, user:vehicle) wins outright. Otherwise arg is treated as a bare local
// name and matched against each registered kind's local segment: exactly one
// local match resolves; zero returns found=false; more than one returns the
// candidate set so the caller can reject the ambiguous bare name.
func resolveRegisteredKind(registered []string, arg string) (name string, ambiguous []string, found bool) {
	for _, k := range registered {
		if k == arg {
			return k, nil, true
		}
	}
	var locals []string
	for _, k := range registered {
		if localKindSegment(k) == arg {
			locals = append(locals, k)
		}
	}
	switch len(locals) {
	case 1:
		return locals[0], nil, true
	case 0:
		return "", nil, false
	default:
		return "", locals, false
	}
}

// listKindNames returns every registered kind name from entity_kinds, sorted,
// regardless of governance status. Used by read/update-path resolution (show,
// alias): a proposed or rejected kind must stay addressable so it can be
// inspected and updated while it waits in (or after) the review queue.
func listKindNames(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryKindNames(ctx, db, `SELECT name FROM entity_kinds ORDER BY name ASC`)
}

// listActiveKindNames returns only the active kind names. Used by extends
// resolution: a new kind must not graft onto a proposed or rejected parent —
// the parent's propertySchema is not part of the active taxonomy yet, which
// mirrors the defineEntityKind MCP tool resolving extends against the
// active-only RegisteredEntityKinds listing.
func listActiveKindNames(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryKindNames(ctx, db,
		`SELECT name FROM entity_kinds WHERE COALESCE(status,'active') = 'active' ORDER BY name ASC`)
}

// queryKindNames runs a single-column kind-name query and collects the rows.
func queryKindNames(ctx context.Context, db *sql.DB, q string) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list kind names: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if serr := rows.Scan(&n); serr != nil {
			return nil, fmt.Errorf("scan kind name: %w", serr)
		}
		names = append(names, n)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate kind names: %w", rerr)
	}
	return names, nil
}

// ---- kinds sub-tree ----

// newSchemaKindsCmd returns the "maz schema kinds" parent command.
func newSchemaKindsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kinds",
		Short: "Manage entity kind taxonomy (list, show, add)",
		Long: `Commands for browsing and authoring the entity-kind taxonomy.

Prefer reusing an existing kind (see 'maz schema kinds list') before
minting a new one — duplicate kinds fragment the knowledge graph.

Subcommands:
  list   List all registered entity kinds (alias for maz schema entity-kinds)
  show   Show full detail for a single kind including parent chain and children
  add    Register a new custom entity kind by appending an entityKindRegistered event

Examples:
  maz schema kinds list
  maz schema kinds show person
  maz schema kinds add vehicle --display-name "Vehicle" --description "A motorised transport."`,
		// No RunE: cobra prints usage.
	}
	cmd.AddCommand(newSchemaKindsListCmd())
	cmd.AddCommand(newSchemaKindsAddCmd())
	cmd.AddCommand(newSchemaKindsShowCmd())
	return cmd
}

// newSchemaKindsListCmd is a thin alias that delegates to runSchemaEntityKinds.
func newSchemaKindsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every entity kind in the taxonomy with live counts",
		Long: `Prefer reusing an existing kind before minting a new one.

List every entity kind registered in the entity_kinds table, joined to
the live entity count from the entities table, ordered alphabetically.

This command is a thin alias for 'maz schema entity-kinds'. Both forms
emit the same output and accept the same flags.

Exit codes:
  0   success
  2   unexpected failure: wiring or query error

Examples:
  maz schema kinds list
  maz schema kinds list --json
  maz schema kinds list --json | jq '[.[] | select(.provenance=="user")]'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSchemaEntityKinds(cmd)
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/schema_entity_kinds_response.json",
	}
	return cmd
}

// kindsAddFlags holds the parsed flag values for "maz schema kinds add".
type kindsAddFlags struct {
	displayName    string
	description    string
	extends        string
	sameAs         []string
	propertySchema string // path to JSON Schema file, or "-" for stdin
}

// newSchemaKindsAddCmd returns the "maz schema kinds add <name>" command.
func newSchemaKindsAddCmd() *cobra.Command {
	flags := &kindsAddFlags{}
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a new custom entity kind",
		Long: `Append an entityKindRegistered event to the event log and materialise it
into the entity_kinds table in the same step, so the kind is queryable via
'maz schema kinds list' / 'show' the instant this command returns — no
'maz rebuild' needed. The event survives rebuild either way.

If a Mazarbul daemon already serves this data dir it holds the store's
single-writer lock, so this command refuses rather than racing it; stop the
daemon ('maz daemon stop') or use the defineEntityKind MCP tool instead.

Prefer reusing an existing kind (see 'maz schema kinds list') before
minting a new one.

Name format: bare snake_case (e.g. vehicle) or namespaced with a colon
separator (e.g. user:research_paper).  The six core kinds (person,
organization, place, project, event, document) cannot be overwritten from
this command.

Flags:
  --display-name   Human-readable label, e.g. "Vehicle" (required)
  --description    One-sentence explanation of what the kind models (required)
  --extends        Parent kind name for single-inheritance subtyping (optional)
  --same-as        Repeatable: external ontology IRI, e.g. schema.org/Vehicle
  --property-schema  Path to a JSON Schema file describing typed attributes,
                   or "-" to read from stdin.  Validated before the event is
                   appended; an invalid schema aborts with exit code 1.

Exit codes:
  0   kind registered (event appended)
  1   expected failure: bad input, validation error, core-kind collision
  2   unexpected failure: storage or wiring error

Examples:
  maz schema kinds add vehicle \
    --display-name "Vehicle" \
    --description "A motorised road transport." \
    --extends document \
    --same-as "https://schema.org/Vehicle" \
    --same-as "http://www.wikidata.org/entity/Q42889"

  maz schema kinds add user:research_paper \
    --display-name "Research Paper" \
    --description "A scholarly article or preprint." \
    --property-schema ./schemas/research_paper.json

  maz schema kinds add vehicle --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaKindsAdd(cmd, args[0], flags)
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/schema_kinds_add_response.json",
		"schema.request":  "schemas/cli/schema_kinds_add_request.json",
	}
	cmd.Flags().StringVar(&flags.displayName, "display-name", "", "Human-readable label for the kind (required)")
	cmd.Flags().StringVar(&flags.description, "description", "", "One-sentence description of the kind (required)")
	cmd.Flags().StringVar(&flags.extends, "extends", "", "Parent kind name for single-inheritance subtyping")
	cmd.Flags().StringArrayVar(&flags.sameAs, "same-as", nil, "External ontology IRI (repeatable)")
	cmd.Flags().StringVar(&flags.propertySchema, "property-schema", "", `Path to a JSON Schema file for typed attributes, or "-" for stdin`)
	return cmd
}

// validateKindAddFlags checks the client-side preconditions for kinds add.
// Returns a silentError(1) on any validation failure after printing to stderr.
func validateKindAddFlags(cmd *cobra.Command, name string, flags *kindsAddFlags) error {
	if !kindNamePattern.MatchString(name) {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema kinds add: name %q must match snake_case or namespaced pattern (e.g. vehicle, user:deal)\n", name)
		return silentError{code: 1, err: fmt.Errorf("invalid kind name: %s", name)}
	}
	if _, isCore := coreKindNames[name]; isCore {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema kinds add: %q is a core kind and cannot be redefined from the CLI; "+
				"use 'maz schema alias' to add sameAs IRIs to a core kind (subject to governance constraints)\n", name)
		return silentError{code: 1, err: fmt.Errorf("core kind collision: %s", name)}
	}
	if strings.TrimSpace(flags.displayName) == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "schema kinds add: --display-name is required")
		return silentError{code: 1, err: errors.New("--display-name required")}
	}
	if strings.TrimSpace(flags.description) == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "schema kinds add: --description is required")
		return silentError{code: 1, err: errors.New("--description required")}
	}
	return nil
}

// loadAndCompilePropertySchema reads the property schema from the path (or
// stdin) and validates it with the santhosh-tekuri compiler. Returns the raw
// JSON and nil on success, or a silentError(1) on any failure.
func loadAndCompilePropertySchema(cmd *cobra.Command, path string) (json.RawMessage, error) {
	raw, err := readPropertySchema(path)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "schema kinds add: --property-schema: %v\n", err)
		return nil, silentError{code: 1, err: fmt.Errorf("read property schema: %w", err)}
	}
	if err := compilePropertySchema(raw); err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "schema kinds add: --property-schema invalid: %v\n", err)
		return nil, silentError{code: 1, err: fmt.Errorf("invalid property schema: %w", err)}
	}
	return raw, nil
}

// kindMintStatus resolves the status an explicitly-minted entity kind lands in.
// Governed mode (config [ontology] mode="governed") parks the mint as "proposed"
// pending `maz schema review approve`; permissive mode (the default) mints
// active. This mirrors the defineEntityKind MCP tool. A config-load failure
// degrades to permissive rather than blocking the mint. It returns the status to
// persist in the event (empty == active) and the status to surface in the result.
func kindMintStatus(dataDir string, logger *zap.Logger) (eventStatus, displayStatus string) {
	governed := false
	if userCfg, cfgErr := config.LoadUserConfig(dataDir); cfgErr != nil {
		logger.Warn("schema kinds add: load user config for governance check; assuming permissive",
			zap.Error(cfgErr))
	} else {
		governed = userCfg.Ontology.Governed()
	}
	if governed {
		return ontology.EntityKindStatusProposed, ontology.EntityKindStatusProposed
	}
	return "", ontology.EntityKindStatusActive
}

// runSchemaKindsAdd validates flags, optionally loads and compiles a
// property schema, checks that the --extends target exists, and appends an
// entityKindRegistered event through the materialising event log.
func runSchemaKindsAdd(cmd *cobra.Command, name string, flags *kindsAddFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	if err := validateKindAddFlags(cmd, name, flags); err != nil {
		return err
	}
	// Keep the original argument (the user's bare or namespaced name) for the
	// daemon route, which feeds the defineEntityKind tool that namespaces and
	// resolves extends itself.
	rawName := name
	// Namespace a bare name to user: (matching the MCP defineEntityKind tool) so
	// the CLI never persists an un-namespaced kind that collides with the bare
	// core namespace. Explicitly namespaced names pass through unchanged.
	name = mintKindName(name)

	var propSchemaRaw json.RawMessage
	if flags.propertySchema != "" {
		raw, err := loadAndCompilePropertySchema(cmd, flags.propertySchema)
		if err != nil {
			return err
		}
		propSchemaRaw = raw
	}

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// A live daemon owns the store's single-writer lock, so the in-process
	// append below would die with the cryptic "ladybug ... status 1". Route the
	// authoring through the daemon's defineEntityKind MCP tool instead, which
	// namespaces the name to user:, runs the reuse-before-mint dedup check, and
	// resolves --extends against the live registry. The tool's bare-name pattern
	// cannot accept an explicitly-namespaced argument (e.g. user:research_paper),
	// so that case still falls through to the in-process path's clean refusal.
	if perr := preflightDaemon("", dataDir); perr != nil && !strings.Contains(rawName, ":") {
		return runSchemaKindsAddViaDaemon(cmd, rawName, propSchemaRaw, flags)
	}

	return runSchemaKindsAddInProcess(ctx, cmd, dataDir, logger, name, propSchemaRaw, flags)
}

// runSchemaKindsAddInProcess is the no-daemon authoring path for `schema kinds
// add`: it resolves --extends against the registry, opens the materialising
// event log (which refuses cleanly if a daemon races in), appends the
// entityKindRegistered event, confirms it materialised, and renders the result.
// The name argument is already namespaced (user:) by the caller.
func runSchemaKindsAddInProcess(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	name string,
	propSchemaRaw json.RawMessage,
	flags *kindsAddFlags,
) error {
	// Resolve --extends through the registry so a bare parent reference (e.g.
	// --extends vehicle) matches the same stored kind that `kinds add vehicle`
	// minted as user:vehicle, and the child persists the canonical parent name
	// rather than a bare reference that would dangle.
	if flags.extends != "" {
		resolved, err := resolveExtendsDB(ctx, cmd, dataDir, logger, flags.extends)
		if err != nil {
			return err
		}
		flags.extends = resolved
	}

	wired, closeFn, err := openMaterializingLog(ctx, cmd, dataDir, logger, "schema kinds add")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("schema kinds add: close wiring", zap.Error(cerr))
		}
	}()

	eventStatus, displayStatus := kindMintStatus(dataDir, logger)

	now := time.Now().UTC()
	ev := &ontology.EntityKindRegisteredEvent{
		Name:           name,
		DisplayName:    flags.displayName,
		Description:    flags.description,
		Extends:        flags.extends,
		SameAs:         flags.sameAs,
		PropertySchema: propSchemaRaw,
		Provenance:     "user",
		AddedBy:        ontology.EntityKindAddedByUser,
		AddedAt:        now,
		Status:         eventStatus,
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		return wrapSchemaKindsFatal(cmd, fmt.Errorf("marshal event: %w", err))
	}

	if _, err = wired.Orchestrator.Log.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
		Source:     "cli:schema:kinds:add",
		Confidence: 1.0,
		Payload:    payload,
	}}); err != nil {
		return wrapSchemaKindsFatal(cmd, fmt.Errorf("append: %w", err))
	}

	if cerr := confirmMaterialised(ctx, cmd, wired, "schema kinds add",
		matCheck{query: "SELECT COUNT(1) FROM entity_kinds WHERE name = ?", args: []any{name}, label: name}); cerr != nil {
		return cerr
	}

	result := kindsAddResult{
		Name:        name,
		DisplayName: flags.displayName,
		Provenance:  "user",
		AddedBy:     string(ontology.EntityKindAddedByUser),
		AddedAt:     now,
		Status:      displayStatus,
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	return renderKindsAddCLI(w, &result)
}

// runSchemaKindsAddViaDaemon routes `schema kinds add` through the running
// daemon's defineEntityKind MCP tool. It is used when a daemon holds the store
// lock so the command stays usable rather than dying on the in-process append's
// "ladybug ... Status 1". The tool namespaces the bare name to user:, runs the
// reuse-before-mint dedup check, and resolves --extends itself, so the CLI only
// forwards the user's inputs and renders the tool's response in the same shape
// as the in-process path.
func runSchemaKindsAddViaDaemon(cmd *cobra.Command, bareName string, propSchemaRaw json.RawMessage, flags *kindsAddFlags) error {
	in := map[string]any{
		"name":        bareName,
		"displayName": flags.displayName,
		"description": flags.description,
	}
	if flags.extends != "" {
		in["extends"] = flags.extends
	}
	if len(flags.sameAs) > 0 {
		in["sameAs"] = flags.sameAs
	}
	if len(propSchemaRaw) > 0 {
		in["propertySchema"] = propSchemaRaw
	}
	reqBody, merr := json.Marshal(in)
	if merr != nil {
		return wrapSchemaKindsFatal(cmd, fmt.Errorf("marshal defineEntityKind request: %w", merr))
	}

	raw, err := invokeNavTool(cmd, "defineEntityKind", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "schema kinds add", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}

	var resp struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapSchemaKindsFatal(cmd, fmt.Errorf("parse defineEntityKind response: %w", uerr))
	}

	result := kindsAddResult{
		Name:        resp.Name,
		DisplayName: flags.displayName,
		Provenance:  "user",
		AddedBy:     string(ontology.EntityKindAddedByUser),
		AddedAt:     time.Now().UTC(),
		// Map the tool's status to the CLI's: "registered" lands active.
		// "proposed" (governed mode) and "reused" pass through verbatim.
		Status: daemonKindStatusToDisplay(resp.Status),
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	return renderKindsAddCLI(w, &result)
}

// daemonKindStatusToDisplay maps the defineEntityKind tool's status enum
// ("registered"/"reused"/"proposed") onto the CLI's kindsAddResult status, so a
// daemon-routed add renders the same status string an in-process add would.
func daemonKindStatusToDisplay(toolStatus string) string {
	switch toolStatus {
	case "registered":
		return ontology.EntityKindStatusActive
	default:
		// "proposed" and "reused" already read sensibly as-is.
		return toolStatus
	}
}

// reportExtendsNotFound prints a friendly not-found message for --extends with
// nearest-name suggestions fetched from the DB, then returns silentError(1).
func reportExtendsNotFound(ctx context.Context, cmd *cobra.Command, db *sql.DB, logger *zap.Logger, kindName string) error {
	rows, qerr := db.QueryContext(ctx, `SELECT name FROM entity_kinds ORDER BY name ASC`)
	if qerr == nil {
		defer rows.Close()
		var all []string
		for rows.Next() {
			var k string
			if serr := rows.Scan(&k); serr == nil {
				all = append(all, k)
			}
		}
		if rerr := rows.Err(); rerr != nil {
			logger.Warn("schema kinds add: iterate kind names for suggestion", zap.Error(rerr))
		}
		suggestions := nearestKindNames(kindName, all)
		if len(suggestions) > 0 {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(),
				"schema kinds add: --extends %q not found; did you mean: %s\n",
				kindName, strings.Join(suggestions, ", "))
		} else {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(),
				"schema kinds add: --extends %q not found in entity_kinds\n", kindName)
		}
	} else {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema kinds add: --extends %q not found in entity_kinds\n", kindName)
	}
	return silentError{code: 1, err: fmt.Errorf("extends target not found: %s", kindName)}
}

// resolveExtendsDB resolves the --extends argument to a registered kind's
// canonical name using the same exact/local/ambiguous model as `describeType`,
// so `--extends vehicle` matches a parent minted as `user:vehicle`. It returns
// the canonical name to persist in EntityKindRegisteredEvent.Extends, or a
// silentError(1) when the parent is absent or the bare name is ambiguous.
func resolveExtendsDB(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, kindName string) (string, error) {
	wired, err := daemon.OpenWiring(ctx, daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		return "", wrapSchemaKindsFatal(cmd, fmt.Errorf("open wiring for extends check: %w", err))
	}
	defer func() {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("schema kinds add: close wiring", zap.Error(cerr))
		}
	}()

	db, err := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if err != nil {
		return "", wrapSchemaKindsFatal(cmd, fmt.Errorf("open sqlite for extends check: %w", err))
	}

	names, err := listActiveKindNames(ctx, db)
	if err != nil {
		return "", wrapSchemaKindsFatal(cmd, fmt.Errorf("query extends target: %w", err))
	}

	resolved, ambiguous, found := resolveRegisteredKind(names, kindName)
	if found {
		return resolved, nil
	}
	if len(ambiguous) > 1 {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema kinds add: --extends %q is ambiguous across namespaces: %s; pass the full kind name\n",
			kindName, strings.Join(ambiguous, ", "))
		return "", silentError{code: 1, err: fmt.Errorf("ambiguous extends target: %s", kindName)}
	}
	return "", reportExtendsNotFound(ctx, cmd, db, logger, kindName)
}

// resolveKindArg resolves a read/update-path kind argument (show, alias) to its
// canonical registered name over the already-open db, mirroring describeType's
// exact/local/ambiguous model. Label prefixes the diagnostic (e.g. "schema
// kinds show"). It returns the canonical name, or a silentError(1) when the bare
// name is ambiguous across namespaces. A name that resolves to nothing is
// returned unchanged so the caller's own not-found rendering (sql.ErrNoRows)
// still fires with did-you-mean suggestions.
func resolveKindArg(ctx context.Context, cmd *cobra.Command, db *sql.DB, label, arg string) (string, error) {
	names, err := listKindNames(ctx, db)
	if err != nil {
		return "", wrapSchemaKindsFatal(cmd, err)
	}
	resolved, ambiguous, found := resolveRegisteredKind(names, arg)
	if found {
		return resolved, nil
	}
	if len(ambiguous) > 1 {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"%s: %q is ambiguous across namespaces: %s; pass the full kind name\n",
			label, arg, strings.Join(ambiguous, ", "))
		return "", silentError{code: 1, err: fmt.Errorf("ambiguous kind name: %s", arg)}
	}
	return arg, nil
}

// kindsAddResult is the JSON shape returned by maz schema kinds add and
// maz schema alias.
type kindsAddResult struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Provenance  string    `json:"provenance"`
	AddedBy     string    `json:"addedBy"`
	AddedAt     time.Time `json:"addedAt"`
	// Status is the governance status the kind landed with: "active" in
	// permissive mode (the default), or "proposed" in governed mode, in which
	// case it stays out of the active taxonomy until `maz schema review
	// approve`.
	Status string `json:"status"`
}

// renderKindsAddCLI prints a confirmation key-value block to w.
func renderKindsAddCLI(w io.Writer, r *kindsAddResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "Kind\t%s\n", r.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Display name\t%s\n", r.DisplayName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Provenance\t%s\n", r.Provenance); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Added by\t%s\n", r.AddedBy); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Added at\t%s\n", r.AddedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Status\t%s\n", r.Status); err != nil {
		return err
	}
	if r.Status == ontology.EntityKindStatusProposed {
		if _, err := fmt.Fprintf(tw, "\t(governed mode: approve with `maz schema review approve %s`)\n", r.Name); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// readPropertySchema reads a JSON Schema document from a file path or stdin
// ("-"). Returns the raw bytes ready for json.RawMessage assignment.
func readPropertySchema(path string) (json.RawMessage, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	// Ensure the bytes are valid JSON.
	var probe any
	if jerr := json.Unmarshal(data, &probe); jerr != nil {
		return nil, fmt.Errorf("not valid JSON: %w", jerr)
	}
	return json.RawMessage(data), nil
}

// compilePropertySchema validates raw (a JSON Schema document) using the
// santhosh-tekuri compiler with format assertions enabled — the same
// settings as the dispatcher's compile-before-materialise guard. Returns
// nil on success or a descriptive error that the caller can surface.
func compilePropertySchema(raw json.RawMessage) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse JSON Schema document: %w", err)
	}
	const resource = "mem://cli/property-schema-check.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(resource, doc); err != nil {
		return fmt.Errorf("add schema resource: %w", err)
	}
	if _, err := c.Compile(resource); err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	return nil
}

// wrapSchemaKindsFatal prints the error to stderr and returns a silentError
// with code 2.
func wrapSchemaKindsFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "schema kinds", err)
}

// ---- kinds show ----

// newSchemaKindsShowCmd returns the "maz schema kinds show <name>" command.
func newSchemaKindsShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show full detail for a single entity kind",
		Long: `Show all columns for the named entity kind from the entity_kinds table,
including the pretty-printed propertySchema, the parent chain (ancestors),
and the direct children. Also reports the live entity count.

E_NOT_FOUND: if the kind is not registered, prints a friendly error with
nearest-name suggestions (prefix and contains matches) and exits 1.

Exit codes:
  0   success
  1   kind not found
  2   unexpected failure: wiring or query error

Examples:
  maz schema kinds show person
  maz schema kinds show user:vehicle
  maz schema kinds show vehicle --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaKindsShow(cmd, args[0])
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/schema_kinds_show_response.json",
	}
	return cmd
}

// kindDetailRow is the full-detail shape for one kind including parent chain,
// children, and live entity count.
type kindDetailRow struct {
	Kind           string          `json:"kind"`
	DisplayName    string          `json:"displayName"`
	Provenance     string          `json:"provenance"`
	Extends        string          `json:"extends"`
	SameAs         []string        `json:"sameAs"`
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"`
	Description    string          `json:"description"`
	AddedAt        time.Time       `json:"addedAt"`
	AddedBy        string          `json:"addedBy"`
	// Status is the governance status: "active", "proposed", or "rejected".
	// Carried so update paths (schema alias) re-append the kind with its
	// current status instead of silently promoting a proposed/rejected kind
	// to active through the dispatcher's empty-status-means-active default.
	Status      string   `json:"status"`
	Count       int      `json:"count"`
	ParentChain []string `json:"parentChain"`
	Children    []string `json:"children"`
}

// runSchemaKindsShow queries a single kind and renders it.
func runSchemaKindsShow(cmd *cobra.Command, name string) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	db, closeDB, err := openSchemaReadDB(ctx, cmd, dataDir, logger)
	if err != nil {
		return wrapSchemaKindsFatal(cmd, err)
	}
	defer closeDB()

	// Resolve a bare local name to its registered kind using the same
	// exact/local/ambiguous model as describeType, so `show vehicle` finds a
	// kind stored as user:vehicle, an ambiguous bare name is rejected, and a
	// non-existent kind is reported (rather than blindly minting a user: name).
	resolved, err := resolveKindArg(ctx, cmd, db, "schema kinds show", name)
	if err != nil {
		return err
	}
	name = resolved

	detail, err := queryKindDetail(ctx, db, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return renderKindNotFound(ctx, cmd, db, "schema kinds show", name)
		}
		return wrapSchemaKindsFatal(cmd, err)
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(detail)
	}
	return renderKindDetailCLI(w, detail)
}

// queryKindDetail fetches all columns for one kind plus its parent chain and
// direct children.
func queryKindDetail(ctx context.Context, db *sql.DB, name string) (*kindDetailRow, error) {
	const q = `
		SELECT ek.name, ek.display_name, ek.provenance,
		       COALESCE(ek.parent,''), COALESCE(ek.same_as,''),
		       COALESCE(ek.attr_schema,''), COALESCE(ek.description,''),
		       ek.added_at, ek.added_by, COALESCE(ek.status,'active'),
		       COALESCE(cnt.n, 0)
		  FROM entity_kinds ek
		  LEFT JOIN (
		        SELECT kind, COUNT(*) AS n FROM entities GROUP BY kind
		       ) cnt ON cnt.kind = ek.name
		 WHERE ek.name = ?`

	row := db.QueryRowContext(ctx, q, name)

	var r kindDetailRow
	var addedAt, sameAsRaw, attrSchemaRaw string
	if err := row.Scan(
		&r.Kind, &r.DisplayName, &r.Provenance,
		&r.Extends, &sameAsRaw,
		&attrSchemaRaw, &r.Description,
		&addedAt, &r.AddedBy, &r.Status,
		&r.Count,
	); err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339, addedAt)
	if err != nil {
		t = time.Time{}
	}
	r.AddedAt = t.UTC()

	if sameAsRaw != "" && sameAsRaw != jsonNull && sameAsRaw != "[]" {
		var uris []string
		if jerr := json.Unmarshal([]byte(sameAsRaw), &uris); jerr == nil {
			r.SameAs = uris
		}
	}
	if r.SameAs == nil {
		r.SameAs = []string{}
	}

	if attrSchemaRaw != "" {
		r.PropertySchema = json.RawMessage(attrSchemaRaw)
	}

	// parent chain
	r.ParentChain, err = queryParentChain(ctx, db, r.Extends)
	if err != nil {
		return nil, fmt.Errorf("query parent chain: %w", err)
	}

	// direct children
	r.Children, err = queryChildren(ctx, db, name)
	if err != nil {
		return nil, fmt.Errorf("query children: %w", err)
	}

	return &r, nil
}

// queryParentChain walks up the parent links from startKind and returns the
// ordered ancestor chain (nearest first). Returns an empty slice for root kinds.
func queryParentChain(ctx context.Context, db *sql.DB, startKind string) ([]string, error) {
	if startKind == "" {
		return []string{}, nil
	}
	chain := make([]string, 0, 8)
	current := startKind
	for range 32 { // safety cap matching dispatcherMaxExtendsDepth
		chain = append(chain, current)
		var parent string
		err := db.QueryRowContext(ctx,
			`SELECT COALESCE(parent,'') FROM entity_kinds WHERE name = ?`, current,
		).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) || parent == "" {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("scan parent of %q: %w", current, err)
		}
		current = parent
	}
	return chain, nil
}

// queryChildren returns the names of all entity kinds that directly extend name.
func queryChildren(ctx context.Context, db *sql.DB, name string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT ek.name FROM entity_kinds ek WHERE ek.parent = ? ORDER BY ek.name ASC`, name)
	if err != nil {
		return nil, fmt.Errorf("query children of %q: %w", name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var child string
		if serr := rows.Scan(&child); serr != nil {
			return nil, fmt.Errorf("scan child: %w", serr)
		}
		out = append(out, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate children: %w", err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// renderKindNotFound fetches all kind names from entity_kinds and prints a
// friendly E_NOT_FOUND message with nearest-name suggestions. The op argument
// names the invoking command (such as "schema kinds show" or "schema alias")
// so a caller's error is never mislabelled as a different subcommand's.
func renderKindNotFound(ctx context.Context, cmd *cobra.Command, db *sql.DB, op, name string) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM entity_kinds ORDER BY name ASC`)
	if err == nil {
		defer rows.Close()
		var candidates []string
		for rows.Next() {
			var k string
			if serr := rows.Scan(&k); serr == nil {
				candidates = append(candidates, k)
			}
		}
		if rerr := rows.Err(); rerr != nil {
			// best-effort: suggestion list may be incomplete; not fatal
			_ = rerr
		}
		suggestions := nearestKindNames(name, candidates)
		if len(suggestions) > 0 {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(),
				"%s: kind %q not found; did you mean: %s\n",
				op, name, strings.Join(suggestions, ", "))
		} else {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(),
				"%s: kind %q not found (no similar kinds registered)\n", op, name)
		}
	} else {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"%s: kind %q not found\n", op, name)
	}
	return silentError{code: 1, err: fmt.Errorf("kind not found: %s", name)}
}

// nearestKindNames returns up to three candidates from all that share a prefix
// with or contain name (case-insensitive).
func nearestKindNames(name string, all []string) []string {
	lower := strings.ToLower(name)
	var out []string
	// Prefix matches first.
	for _, c := range all {
		if strings.HasPrefix(strings.ToLower(c), lower) {
			out = append(out, c)
			if len(out) == 3 {
				return out
			}
		}
	}
	// Then contains matches.
	for _, c := range all {
		if strings.Contains(strings.ToLower(c), lower) {
			if !contains(out, c) {
				out = append(out, c)
				if len(out) == 3 {
					return out
				}
			}
		}
	}
	return out
}

// renderKindDetailCLI prints a key-value block plus parent chain and children.
func renderKindDetailCLI(w io.Writer, r *kindDetailRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	writef := func(key, val string) error {
		_, err := fmt.Fprintf(tw, "%s\t%s\n", key, val)
		return err
	}

	if err := writef("Kind", r.Kind); err != nil {
		return err
	}
	if err := writef("Display name", r.DisplayName); err != nil {
		return err
	}
	if err := writef("Provenance", r.Provenance); err != nil {
		return err
	}
	if err := writef("Extends", r.Extends); err != nil {
		return err
	}
	if err := writef("Same as", strings.Join(r.SameAs, ", ")); err != nil {
		return err
	}
	if err := writef("Description", r.Description); err != nil {
		return err
	}
	if err := writef("Status", r.Status); err != nil {
		return err
	}
	if err := writef("Added by", r.AddedBy); err != nil {
		return err
	}
	if err := writef("Added at", r.AddedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := writef("Entity count", strconv.Itoa(r.Count)); err != nil {
		return err
	}
	if err := writef("Parent chain", strings.Join(r.ParentChain, " -> ")); err != nil {
		return err
	}
	if err := writef("Children", strings.Join(r.Children, ", ")); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return renderPropertySchemaCLI(w, r.PropertySchema)
}

// renderPropertySchemaCLI pretty-prints a JSON Schema document to w with a
// header line. No-ops when raw is empty. Extracted to keep renderKindDetailCLI
// below the gocognit threshold.
func renderPropertySchemaCLI(w io.Writer, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var pretty any
	if err := json.Unmarshal(raw, &pretty); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "\nProperty schema:"); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(pretty)
}

// ---- schema alias ----

// aliasEquivalenceRelations are the SKOS alignment relations that map
// faithfully to the same_as IRI bag — the only alignment storage the
// entity_kinds projection has. Both sameAs (OWL equivalence) and exactMatch
// (SKOS equivalence) assert this kind IS the external concept, so the bare
// IRI belongs in same_as. The hierarchical/associative relations
// (closeMatch/broadMatch/narrowMatch/relatedMatch) have no typed storage and
// are rejected rather than silently flattened into same_as; closeMatch between
// two kinds is recorded automatically by the dedup layer (defineEntityKind).
//
//nolint:gochecknoglobals // read-only lookup, never mutated
var aliasEquivalenceRelations = map[string]struct{}{
	"sameAs":     {},
	"exactMatch": {},
}

// aliasRejectedRelations are valid SKOS alignment relations the CLI recognises
// but cannot persist with their relation type intact (no typed column exists).
//
//nolint:gochecknoglobals // read-only lookup, never mutated
var aliasRejectedRelations = map[string]struct{}{
	"closeMatch":   {},
	"broadMatch":   {},
	"narrowMatch":  {},
	"relatedMatch": {},
}

// newSchemaAliasCmd returns the "maz schema alias <kind> <iri> [relation]" command.
func newSchemaAliasCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias <kind> <iri> [relation]",
		Short: "Map an entity kind to an external vocabulary IRI",
		Long: `Re-append an entityKindRegistered event for <kind> with the given IRI
added to its existing sameAs list. The dispatcher's idempotent UPSERT
semantics treat this as an update — the row is replaced with the new
sameAs list.

The event is appended and materialised in the same step, so the updated
sameAs list is queryable via 'maz schema kinds show' the instant this command
returns — no 'maz rebuild' needed. The event survives rebuild either way. If a
daemon already serves this data dir, this command refuses (the daemon holds the
store lock); stop it or use an MCP authoring tool instead.

The optional <relation> is the SKOS alignment relation (default "sameAs").
Only the equivalence relations map faithfully to the same_as IRI bag, which
is the only alignment storage entity kinds have:
  sameAs       OWL equivalence — the bare IRI is added to same_as (default).
  exactMatch   SKOS equivalence — same storage as sameAs.

The hierarchical/associative SKOS relations (closeMatch, broadMatch,
narrowMatch, relatedMatch) have no typed storage and are rejected with exit 1
rather than silently flattened into same_as. closeMatch between two kinds is
recorded automatically by the dedup layer when you defineEntityKind a
near-duplicate, so it needs no CLI surface.

Core kinds (person, organization, place, project, event, document):
  The dispatcher's core-provenance guard silently discards any non-core
  event that tries to overwrite a core row.  Because this command emits
  events with provenance "user", it CANNOT modify core kinds — the event
  would be appended to the log but silently discarded by the dispatcher.
  To avoid confusing the user with a silent no-op, this command refuses
  to alias core kinds with exit code 1.

  Adding sameAs IRIs to core kinds is a governance-phase operation (Phase 6
  of the dynamic-ontology effort), not a human CLI authoring surface.

Exit codes:
  0   alias added (event appended)
  1   kind not found, core kind, IRI already present, or unsupported relation
  2   unexpected failure: wiring or storage error

Examples:
  maz schema alias vehicle "https://schema.org/Vehicle"
  maz schema alias vehicle "https://schema.org/Vehicle" exactMatch
  maz schema alias user:research_paper "http://www.wikidata.org/entity/Q13442814"
  maz schema alias user:deal "https://schema.org/LoanOrCredit" --json`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			relation := "sameAs"
			if len(args) == 3 {
				relation = args[2]
			}
			return runSchemaAlias(cmd, args[0], args[1], relation)
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/schema_kinds_add_response.json",
	}
	return cmd
}

// readKindForAlias reads the existing kind row through openSchemaReadDB, checks
// the IRI is not already present, and returns the existing row plus the new
// merged sameAs slice (or a silentError). OpenSchemaReadDB handles the
// daemon-or-in-process split: under a live daemon it opens a lock-tolerant
// read-only SQLite pool (the daemon holds the graph-DB write lock), and
// standalone it opens the in-process wiring. The read handle is released before
// returning — when standalone it holds the dataDir lock, and the caller opens
// the event log next, where only one holder may take the lock at a time.
func readKindForAlias(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, kindName, iri string) (*kindDetailRow, []string, error) {
	db, closeDB, err := openSchemaReadDB(ctx, cmd, dataDir, logger)
	if err != nil {
		return nil, nil, wrapSchemaKindsFatal(cmd, fmt.Errorf("open schema read db: %w", err))
	}
	defer closeDB()

	// Resolve a bare local name to the canonical registered kind (e.g. vehicle ->
	// user:vehicle) using describeType's exact/local/ambiguous model, so alias
	// targets the stored kind and rejects an ambiguous bare name rather than
	// silently mutating a guessed user: default.
	resolved, rerr := resolveKindArg(ctx, cmd, db, "schema alias", kindName)
	if rerr != nil {
		return nil, nil, rerr
	}
	kindName = resolved

	existing, err := queryKindDetail(ctx, db, kindName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, renderKindNotFound(ctx, cmd, db, "schema alias", kindName)
		}
		return nil, nil, wrapSchemaKindsFatal(cmd, fmt.Errorf("query kind: %w", err))
	}

	for _, s := range existing.SameAs {
		if s == iri {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(),
				"schema alias: IRI %q is already registered for kind %q\n", iri, kindName)
			return nil, nil, silentError{code: 1, err: fmt.Errorf("IRI already present: %s", iri)}
		}
	}

	newSameAs := make([]string, len(existing.SameAs)+1)
	copy(newSameAs, existing.SameAs)
	newSameAs[len(existing.SameAs)] = iri
	return existing, newSameAs, nil
}

// runSchemaAlias reads the current kind row, adds the IRI to sameAs, and
// re-appends an entityKindRegistered event. The relation argument is the SKOS
// alignment relation: only the equivalence relations (sameAs/exactMatch) are
// stored; the hierarchical/associative ones are rejected (see aliasEquivalenceRelations).
func runSchemaAlias(cmd *cobra.Command, kindName, iri, relation string) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	// Relation guard: only equivalence relations map to the same_as bag.
	if _, ok := aliasEquivalenceRelations[relation]; !ok {
		msg := fmt.Sprintf("schema alias: unknown SKOS relation %q; supported: sameAs, exactMatch\n", relation)
		if _, rejected := aliasRejectedRelations[relation]; rejected {
			msg = fmt.Sprintf("schema alias: relation %q has no typed storage in the entity-kind "+
				"registry; only the equivalence relations sameAs and exactMatch are persistable. "+
				"closeMatch between kinds is recorded automatically when you defineEntityKind a "+
				"near-duplicate.\n", relation)
		}
		//nolint:errcheck // best-effort stderr
		fmt.Fprint(cmd.ErrOrStderr(), msg)
		return silentError{code: 1, err: fmt.Errorf("unsupported alias relation: %s", relation)}
	}

	// Core-kind guard: refuse early to avoid a silent dispatcher no-op. Core
	// kinds are stored under their bare name, so guarding the raw argument here
	// catches `alias person ...` before any registry resolution. Non-core bare
	// names (e.g. vehicle) are resolved against the registry inside
	// readKindForAlias, which targets the stored user:/extension: kind.
	if _, isCore := coreKindNames[kindName]; isCore {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema alias: %q is a core kind; the dispatcher's core-provenance guard would "+
				"silently discard a user-provenance event. Adding sameAs to core kinds is a "+
				"governance-phase operation (Phase 6 of the dynamic-ontology effort).\n", kindName)
		return silentError{code: 1, err: fmt.Errorf("core kind alias refused: %s", kindName)}
	}

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	now := time.Now().UTC()

	// Read the existing kind (its displayName, description, extends,
	// propertySchema, status, and current sameAs) and merge in the new IRI, then
	// re-append a COMPLETE entityKindRegistered event. readKindForAlias routes
	// the read through openSchemaReadDB, so this works whether or not a daemon is
	// live; the append routes through the daemon's admin endpoint when one runs,
	// or in-process otherwise (appendBatchViaDaemonOrInProcess). A partial event
	// (name + sameAs only) would fail the ontology schema's required-field
	// validation (displayName/description), which is why both paths build the
	// full payload from the read row rather than relying on a dispatcher merge.
	existing, newSameAs, err := readKindForAlias(ctx, cmd, dataDir, logger, kindName, iri)
	if err != nil {
		return err
	}

	ev := &ontology.EntityKindRegisteredEvent{
		Name:           existing.Kind,
		DisplayName:    existing.DisplayName,
		Description:    existing.Description,
		Extends:        existing.Extends,
		SameAs:         newSameAs,
		PropertySchema: existing.PropertySchema,
		Provenance:     "user",
		AddedBy:        ontology.EntityKindAddedByUser,
		AddedAt:        now,
		// Carry the kind's current governance status: the dispatcher upsert
		// writes excluded.status (empty means active), so omitting it here
		// would silently promote a proposed/rejected kind to active and
		// bypass `maz schema review approve` in governed mode.
		Status: existing.Status,
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		return wrapSchemaKindsFatal(cmd, fmt.Errorf("marshal event: %w", err))
	}

	batch := []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
		Source:     "schema/alias",
		Scope:      kernel.ScopePersonal,
		Confidence: 1.0,
		Payload:    payload,
	}}
	// Verify the exact IRI landed in the projection's same_as JSON array — not
	// just that the row exists (it pre-dated this update). json_each gives exact
	// element membership; a LIKE probe would treat a `%`/`_` in the IRI as a
	// wildcard and could match an old alias, masking a swallowed Apply failure.
	checks := []matCheck{{
		query:   "SELECT COUNT(1) FROM entity_kinds, json_each(same_as) WHERE name = ? AND json_each.value = ?",
		args:    []any{existing.Kind, iri},
		label:   iri,
		confirm: "present",
	}}
	if aerr := appendBatchViaDaemonOrInProcess(ctx, cmd, dataDir, logger, "schema alias", batch, nil, checks); aerr != nil {
		return aerr
	}

	result := kindsAddResult{
		Name:        existing.Kind,
		DisplayName: existing.DisplayName,
		Provenance:  "user",
		AddedBy:     string(ontology.EntityKindAddedByUser),
		AddedAt:     now,
		// Carry the kind's current governance status so the alias response
		// does not misreport a proposed/rejected kind as active.
		Status: existing.Status,
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	_, err = fmt.Fprintf(w, "Added %q to %s of %q\n", iri, relation, existing.Kind)
	return err
}

// ---- predicates add ----

// predicatesAddFlags holds parsed flags for "maz schema predicates add".
type predicatesAddFlags struct {
	kind           string
	description    string
	sameAs         []string
	subjectKinds   []string
	objectKinds    []string
	propertySchema string
}

// newSchemaPredicatesAddCmd returns "maz schema predicates add <predicate>".
func newSchemaPredicatesAddCmd() *cobra.Command {
	flags := &predicatesAddFlags{}
	cmd := &cobra.Command{
		Use:   "add <predicate>",
		Short: "Register a new relation predicate",
		Long: `Append a predicateRegistered event to the event log and materialise it
into the predicates table in the same step, so the predicate is queryable via
'maz schema predicates' the instant this command returns — no 'maz rebuild'
needed. The event survives rebuild either way.

If a daemon already serves this data dir it holds the store's single-writer
lock, so this command refuses rather than racing it; stop the daemon or use
the definePredicate MCP tool instead.

The predicate name should be snake_case (e.g. funded_by, reports_to).

Flags:
  --kind           functional or non-functional (default non-functional)
                   functional: at most one current value per subject
                   non-functional: multiple current values permitted
  --description    One-sentence explanation of the predicate (required)
  --subject-kinds  Repeatable: entity kind allowed as the subject (domain).
                   Omit to accept any kind (open-world default).
  --object-kinds   Repeatable: entity kind allowed as the object (range).
  --same-as        Repeatable: external ontology IRI (e.g. https://schema.org/worksFor)
  --property-schema  JSON Schema (string) constraining the relation's attributes

Exit codes:
  0   predicate registered (event appended)
  1   expected failure: bad input, validation error
  2   unexpected failure: storage or wiring error

Examples:
  maz schema predicates add funded_by \
    --kind functional \
    --description "Subject organisation is funded by the object entity."

  maz schema predicates add reports_to \
    --description "Subject person reports to the object person in a hierarchy."

  maz schema predicates add knows \
    --kind non-functional \
    --description "Subject person knows the object person." \
    --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaPredicatesAdd(cmd, args[0], flags)
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/schema_predicates_add_response.json",
		"schema.request":  "schemas/cli/schema_predicates_add_request.json",
	}
	cmd.Flags().StringVar(&flags.kind, "kind", "non-functional",
		`Predicate kind: "functional" (one value per subject) or "non-functional" (many values)`)
	cmd.Flags().StringVar(&flags.description, "description", "", "One-sentence explanation of the predicate (required)")
	cmd.Flags().StringArrayVar(&flags.sameAs, "same-as", nil, "External ontology IRI (repeatable)")
	cmd.Flags().StringArrayVar(&flags.subjectKinds, "subject-kinds", nil, "Entity kind allowed as the subject/domain (repeatable; omit for any kind)")
	cmd.Flags().StringArrayVar(&flags.objectKinds, "object-kinds", nil, "Entity kind allowed as the object/range (repeatable; omit for any kind)")
	cmd.Flags().StringVar(&flags.propertySchema, "property-schema", "", "JSON Schema (string) constraining the relation's attributes")
	return cmd
}

// predicatesAddResult is the JSON shape returned by maz schema predicates add.
type predicatesAddResult struct {
	Predicate   string    `json:"predicate"`
	Kind        string    `json:"kind"`
	Description string    `json:"description"`
	AddedBy     string    `json:"addedBy"`
	AddedAt     time.Time `json:"addedAt"`
}

// validatePredicatePropertySchema validates the optional --property-schema flag
// for `predicates add`. An empty flag yields a nil schema. A non-empty flag must
// be an object-shaped, compilable JSON Schema: boolean/scalar schemas
// (`true`/`false`/`42`) compile cleanly but accept every attributes map, so a
// `true` schema would make a relation look typed while no constraint could ever
// fire — those are rejected up front, matching the MCP/entity-kind authoring
// contract that propertySchema is a JSON object. On failure it writes a friendly
// message to stderr and returns a silentError (exit 1).
func validatePredicatePropertySchema(cmd *cobra.Command, flag string) (json.RawMessage, error) {
	s := strings.TrimSpace(flag)
	if s == "" {
		return nil, nil
	}
	raw := json.RawMessage(s)
	var doc map[string]any
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema predicates add: --property-schema must be a JSON Schema object: %v\n", jerr)
		return nil, silentError{code: 1, err: fmt.Errorf("invalid --property-schema: %w", jerr)}
	}
	// Compile-check the schema (not just JSON-object shape): an uncompilable
	// schema like {"type":12} would otherwise be appended, then relation
	// validation would treat the compile failure as an infrastructure error and
	// warn-accept it even in strict mode. Reuse the same compiler the MCP and
	// entity-kind authoring paths use.
	if jerr := compilePropertySchema(raw); jerr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema predicates add: --property-schema must be a valid JSON Schema object: %v\n", jerr)
		return nil, silentError{code: 1, err: fmt.Errorf("invalid --property-schema: %w", jerr)}
	}
	return raw, nil
}

// runSchemaPredicatesAdd validates flags and appends a predicateRegistered event.
func runSchemaPredicatesAdd(cmd *cobra.Command, predicate string, flags *predicatesAddFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	// ---- client-side validations ----

	if strings.TrimSpace(predicate) == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "schema predicates add: predicate name is required")
		return silentError{code: 1, err: errors.New("predicate name required")}
	}
	if !predicateNamePattern.MatchString(predicate) {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema predicates add: predicate %q must be snake_case (lowercase, start with a letter, e.g. works_at)\n", predicate)
		return silentError{code: 1, err: fmt.Errorf("invalid predicate name: %s", predicate)}
	}

	var pkind ontology.PredicateKind
	switch flags.kind {
	case "functional":
		pkind = ontology.PredicateKindFunctional
	case "non-functional":
		pkind = ontology.PredicateKindNonFunctional
	default:
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema predicates add: --kind must be \"functional\" or \"non-functional\", got %q\n", flags.kind)
		return silentError{code: 1, err: fmt.Errorf("invalid --kind: %s", flags.kind)}
	}

	if strings.TrimSpace(flags.description) == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "schema predicates add: --description is required")
		return silentError{code: 1, err: errors.New("--description required")}
	}

	propertySchema, perr := validatePredicatePropertySchema(cmd, flags.propertySchema)
	if perr != nil {
		return perr
	}

	// ---- append event via the materialising event log ----

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// A live daemon owns the store's single-writer lock, so the in-process
	// append below would die with the cryptic "ladybug ... status 1". Route the
	// authoring through the daemon's definePredicate MCP tool instead so the
	// command stays usable while a daemon runs.
	if perr := preflightDaemon("", dataDir); perr != nil {
		return runSchemaPredicatesAddViaDaemon(cmd, predicate, string(pkind), propertySchema, flags)
	}

	return runSchemaPredicatesAddInProcess(ctx, cmd, dataDir, logger, predicate, pkind, propertySchema, flags)
}

// runSchemaPredicatesAddInProcess is the no-daemon authoring path for `schema
// predicates add`: it opens the materialising event log (which refuses cleanly
// if a daemon races in), appends the predicateRegistered event, confirms it
// materialised, and renders the result.
func runSchemaPredicatesAddInProcess(
	ctx context.Context,
	cmd *cobra.Command,
	dataDir string,
	logger *zap.Logger,
	predicate string,
	pkind ontology.PredicateKind,
	propertySchema json.RawMessage,
	flags *predicatesAddFlags,
) error {
	wired, closeFn, err := openMaterializingLog(ctx, cmd, dataDir, logger, "schema predicates add")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("schema predicates add: close wiring", zap.Error(cerr))
		}
	}()

	now := time.Now().UTC()
	ev := &ontology.PredicateRegisteredEvent{
		Predicate:      predicate,
		Kind:           pkind,
		Description:    flags.description,
		AddedBy:        ontology.PredicateAddedByUser,
		AddedAt:        now,
		SubjectKinds:   flags.subjectKinds,
		ObjectKinds:    flags.objectKinds,
		SameAs:         flags.sameAs,
		PropertySchema: propertySchema,
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		return wrapSchemaPredicatesAddFatal(cmd, fmt.Errorf("marshal event: %w", err))
	}

	_, err = wired.Orchestrator.Log.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindPredicateRegistered),
		Source:     "cli:schema:predicates:add",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		return wrapSchemaPredicatesAddFatal(cmd, fmt.Errorf("append: %w", err))
	}

	if cerr := confirmMaterialised(ctx, cmd, wired, "schema predicates add",
		matCheck{query: "SELECT COUNT(1) FROM predicates WHERE predicate = ?", args: []any{predicate}, label: predicate}); cerr != nil {
		return cerr
	}

	result := predicatesAddResult{
		Predicate:   predicate,
		Kind:        string(pkind),
		Description: flags.description,
		AddedBy:     string(ontology.PredicateAddedByUser),
		AddedAt:     now,
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	return renderPredicatesAddCLI(w, &result)
}

// runSchemaPredicatesAddViaDaemon routes `schema predicates add` through the
// running daemon's definePredicate MCP tool. Used when a daemon holds the store
// lock so the command stays usable rather than dying on the in-process append's
// "ladybug ... Status 1". The CLI forwards the already-validated inputs and
// renders the same confirmation block as the in-process path. An existing
// predicate returns status "updated" from the tool (kind/description/domain-range
// overwritten); either way the confirmation output is the same.
func runSchemaPredicatesAddViaDaemon(cmd *cobra.Command, predicate, kind string, propertySchema json.RawMessage, flags *predicatesAddFlags) error {
	in := map[string]any{
		"predicate":   predicate,
		"kind":        kind,
		"description": flags.description,
	}
	if len(flags.subjectKinds) > 0 {
		in["subjectKinds"] = flags.subjectKinds
	}
	if len(flags.objectKinds) > 0 {
		in["objectKinds"] = flags.objectKinds
	}
	if len(flags.sameAs) > 0 {
		in["sameAs"] = flags.sameAs
	}
	if len(propertySchema) > 0 {
		in["propertySchema"] = propertySchema
	}
	reqBody, merr := json.Marshal(in)
	if merr != nil {
		return wrapSchemaPredicatesAddFatal(cmd, fmt.Errorf("marshal definePredicate request: %w", merr))
	}

	raw, err := invokeNavTool(cmd, "definePredicate", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "schema predicates add", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}

	result := predicatesAddResult{
		Predicate:   predicate,
		Kind:        kind,
		Description: flags.description,
		AddedBy:     string(ontology.PredicateAddedByUser),
		AddedAt:     time.Now().UTC(),
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	return renderPredicatesAddCLI(w, &result)
}

// renderPredicatesAddCLI prints a confirmation key-value block to w.
func renderPredicatesAddCLI(w io.Writer, r *predicatesAddResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "Predicate\t%s\n", r.Predicate); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Kind\t%s\n", r.Kind); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Description\t%s\n", r.Description); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Added by\t%s\n", r.AddedBy); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tw, "Added at\t%s\n", r.AddedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	return tw.Flush()
}

// wrapSchemaPredicatesAddFatal prints the error to stderr and returns a
// silentError with code 2.
func wrapSchemaPredicatesAddFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "schema predicates add", err)
}
