package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	clischemas "github.com/mo-cmyk/mazarbul/internal/cli/schemas"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// outputFormatMCP is the --output flag value that emits an MCP-compatible
// JSON tool definition instead of a human or standard JSON schema view.
const outputFormatMCP = "mcp"

// jsonNull is the literal string json.Encoder emits for a nil slice.
const jsonNull = "null"

// predicateRow is the in-memory representation of one predicates table
// row used by both the CLI and JSON renderers.
type predicateRow struct {
	Predicate      string          `json:"predicate"`
	Kind           string          `json:"kind"`
	Description    string          `json:"description"`
	AddedAt        time.Time       `json:"addedAt"`
	AddedBy        string          `json:"addedBy"`
	SubjectKinds   []string        `json:"subjectKinds,omitempty"`   // domain constraint; empty = any kind
	ObjectKinds    []string        `json:"objectKinds,omitempty"`    // range constraint; empty = any kind
	SameAs         []string        `json:"sameAs,omitempty"`         // external vocabulary IRIs
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"` // JSON Schema for the relation's attributes
}

// commandSchema is the JSON shape returned by maz schema show <path>.
type commandSchema struct {
	Command     string          `json:"command"`
	Description string          `json:"description"`
	Request     json.RawMessage `json:"request,omitempty"`
	Response    json.RawMessage `json:"response,omitempty"`
	ExitCodes   []exitCodeEntry `json:"exitCodes,omitempty"`
}

// exitCodeEntry is one exit-code documentation entry.
type exitCodeEntry struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

// mcpToolDefinition is the MCP tool definition shape for --output mcp.
type mcpToolDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// commandSummary is one entry in the maz schema (command list) output.
type commandSummary struct {
	Path        string `json:"path"`
	Description string `json:"description"`
}

func newSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Inspect the Mazarbul schema vocabulary and command structure",
		Long: `Commands for inspecting the schema vocabulary and the CLI command tree.

Use "maz schema predicates" to list every relation predicate the kernel
knows about, along with its kind, provenance, and description.

Use "maz schema show <command-path>" to get the JSON Schema for a
specific command's request and response shapes.

Use "maz schema" (no subcommand) to list all registered commands with
their brief descriptions.

Examples:
  maz schema
  maz schema predicates
  maz schema show entities/list
  maz schema show entities/list --output mcp`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSchemaList(cmd)
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/list_envelope.json",
	}
	cmd.AddCommand(newSchemaPredicatesCmd())
	cmd.AddCommand(newSchemaEntityKindsCmd())
	cmd.AddCommand(newSchemaShowCmd())
	cmd.AddCommand(newSchemaKindsCmd())
	cmd.AddCommand(newSchemaAliasCmd())
	cmd.AddCommand(newSchemaImportCmd())
	cmd.AddCommand(newSchemaReviewCmd())
	cmd.AddCommand(newSchemaLintCmd())
	cmd.AddCommand(newSchemaPreviewCmd())
	return cmd
}

func newSchemaPredicatesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "predicates",
		Short: "List every predicate in the vocabulary table",
		Long: `List every relation predicate registered in the predicates table,
ordered alphabetically. Columns are:

  Predicate   the relation name, e.g. works_at
  Kind        functional or non-functional
  Added By    seed, llm, or user
  Added At    RFC 3339 UTC timestamp
  Description human-readable explanation

Exit codes:
  0   success
  2   unexpected failure: wiring or query error

Examples:
  # Human-readable table
  maz schema predicates

  # Machine-readable JSON array
  maz schema predicates --json

  # Pipe into jq
  maz schema predicates --json | jq '[.[] | select(.kind=="functional")]'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSchemaPredicates(cmd)
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/schema_predicates_response.json",
	}
	cmd.AddCommand(newSchemaPredicatesAddCmd())
	return cmd
}

func newSchemaShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <command-path>",
		Short: "Show the JSON Schema for a command's request and response",
		Long: `Show the JSON Schema for the named command path. The path uses
forward slashes as separators (e.g. "schema/predicates", "config/list").

In the default output mode, prints the request and response schemas as
indented JSON. With --output mcp, emits the MCP tool definition shape
{name, description, inputSchema, outputSchema}.

Exit codes:
  0   success
  1   command path not found or no schema registered
  2   unexpected failure

Examples:
  maz schema show schema/predicates
  maz schema show schema/predicates --output json
  maz schema show schema/predicates --output mcp`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaShow(cmd, args[0])
		},
	}
	return cmd
}

// runSchemaList walks the cobra command tree from the root and emits a
// flat list of all registered command paths with their short descriptions.
func runSchemaList(cmd *cobra.Command) error {
	root := cmd.Root()
	var summaries []commandSummary
	walkCommands(root, "", &summaries)

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{"items": summaries})
	}
	c := cmdColorizer(cmd)
	tbl := output.NewTable(wr, c, []output.Column{
		{Title: "command"},
		{Title: "description"},
	})
	for _, s := range summaries {
		tbl.Row(s.Path, s.Description)
	}
	return tbl.Render()
}

// walkCommands recursively collects command paths and descriptions.
func walkCommands(cmd *cobra.Command, prefix string, out *[]commandSummary) {
	if cmd.Hidden {
		return
	}
	path := cmd.Name()
	if prefix != "" {
		path = prefix + "/" + cmd.Name()
	}
	if cmd.Runnable() {
		*out = append(*out, commandSummary{Path: path, Description: cmd.Short})
	}
	for _, sub := range cmd.Commands() {
		walkCommands(sub, path, out)
	}
}

// runSchemaShow resolves the command at commandPath and returns its registered
// JSON Schema. The commandPath uses "/" separators (e.g. "schema/predicates").
func runSchemaShow(cmd *cobra.Command, commandPath string) error {
	root := cmd.Root()
	parts := strings.Split(commandPath, "/")
	target, _, err := root.Find(parts)
	if err != nil || target == nil || target == root {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintf(cmd.ErrOrStderr(), "schema show: command %q not found\n", commandPath)
		return silentError{code: 1, err: fmt.Errorf("command not found: %s", commandPath)}
	}

	reqSchemaRef := target.Annotations["schema.request"]
	respSchemaRef := target.Annotations["schema.response"]

	var reqSchema, respSchema json.RawMessage
	if reqSchemaRef != "" {
		reqSchema = loadEmbeddedSchema(reqSchemaRef)
	}
	if respSchemaRef != "" {
		respSchema = loadEmbeddedSchema(respSchemaRef)
	}

	cs := commandSchema{
		Command:     commandPath,
		Description: target.Short,
		Request:     reqSchema,
		Response:    respSchema,
		ExitCodes:   standardExitCodes(),
	}

	w := cmd.OutOrStdout()

	// Check for --output mcp to emit an MCP tool definition.
	outputFlag, _ := cmd.Root().PersistentFlags().GetString("output")
	if outputFlag == outputFormatMCP {
		toolName := strings.ReplaceAll(commandPath, "/", "_")
		tool := mcpToolDefinition{
			Name:         toolName,
			Description:  target.Short,
			InputSchema:  reqSchema,
			OutputSchema: respSchema,
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(tool)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(cs)
}

// loadEmbeddedSchema reads a schema from the embedded FS. Returns nil when
// the file is absent (the command has no schema registered yet).
// The ref parameter is a basename, e.g. "schemas/cli/error.json" or just "error.json".
func loadEmbeddedSchema(ref string) json.RawMessage {
	// Normalize to basename only; the embed FS in clischemas uses bare filenames.
	base := ref
	if idx := strings.LastIndex(ref, "/"); idx >= 0 {
		base = ref[idx+1:]
	}
	data, err := clischemas.FS.ReadFile(base)
	if err != nil {
		return nil
	}
	return json.RawMessage(data)
}

// standardExitCodes returns the common exit codes documented for all commands.
func standardExitCodes() []exitCodeEntry {
	return []exitCodeEntry{
		{Code: 0, Meaning: "success"},
		{Code: 1, Meaning: "expected failure (bad input, not found)"},
		{Code: 2, Meaning: "unexpected failure (bug, I/O error)"},
		{Code: 130, Meaning: "user cancelled (Ctrl-C)"},
	}
}

// ListRegisteredSchemas returns all command paths that have a registered
// schema annotation. Used by compile-time tests.
func ListRegisteredSchemas(root *cobra.Command) []string {
	var out []string
	walkForSchemas(root, "", &out)
	return out
}

func walkForSchemas(cmd *cobra.Command, prefix string, out *[]string) {
	path := cmd.Name()
	if prefix != "" {
		path = prefix + "/" + cmd.Name()
	}
	if _, ok := cmd.Annotations["schema.response"]; ok {
		*out = append(*out, path)
	}
	if _, ok := cmd.Annotations["schema.request"]; ok {
		if !contains(*out, path) {
			*out = append(*out, path)
		}
	}
	for _, sub := range cmd.Commands() {
		walkForSchemas(sub, path, out)
	}
}

func contains(sl []string, s string) bool {
	for _, v := range sl {
		if v == s {
			return true
		}
	}
	return false
}

// runSchemaPredicates wires the daemon, opens SQLite, queries the
// predicates table, and renders the result in the active output mode.
func runSchemaPredicates(cmd *cobra.Command) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	db, closeDB, err := openSchemaReadDB(ctx, cmd, dataDir, logger)
	if err != nil {
		return wrapSchemaFatal(cmd, err)
	}
	defer closeDB()

	predicates, err := queryPredicates(ctx, db)
	if err != nil {
		return wrapSchemaFatal(cmd, err)
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return renderPredicatesJSON(w, predicates)
	}
	return renderPredicatesCLI(cmd, predicates)
}

// queryPredicates runs the SELECT against the predicates table and
// returns all rows ordered by predicate name.
func queryPredicates(ctx context.Context, db *sql.DB) ([]predicateRow, error) {
	const q = `SELECT predicate, kind, description, added_at, added_by,
	                  COALESCE(subject_kinds,''), COALESCE(object_kinds,''),
	                  COALESCE(same_as,''), COALESCE(property_schema,'')
	             FROM predicates
	            ORDER BY predicate ASC`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query predicates: %w", err)
	}
	defer rows.Close()

	var out []predicateRow
	for rows.Next() {
		r, serr := scanPredicateRow(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan row: %w", serr)
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate predicates: %w", err)
	}
	return out, nil
}

// scanPredicateRow scans one row from the predicates query result set. The
// domain/range (subject_kinds/object_kinds) and same_as columns are stored as
// JSON array text; property_schema as a JSON object — each is parsed when
// non-empty so the wire shape matches the introspect tool's predicate entry.
func scanPredicateRow(rows *sql.Rows) (predicateRow, error) {
	var r predicateRow
	var addedAt, subjectKinds, objectKinds, sameAs, propertySchema string
	if err := rows.Scan(&r.Predicate, &r.Kind, &r.Description, &addedAt, &r.AddedBy,
		&subjectKinds, &objectKinds, &sameAs, &propertySchema); err != nil {
		return predicateRow{}, err
	}
	t, err := time.Parse(time.RFC3339, addedAt)
	if err != nil {
		// Stored value is not RFC 3339: use zero time rather than failing.
		t = time.Time{}
	}
	r.AddedAt = t.UTC()
	r.SubjectKinds = parseJSONStringArray(subjectKinds)
	r.ObjectKinds = parseJSONStringArray(objectKinds)
	r.SameAs = parseJSONStringArray(sameAs)
	if propertySchema != "" && json.Valid([]byte(propertySchema)) {
		r.PropertySchema = json.RawMessage(propertySchema)
	}
	return r, nil
}

// parseJSONStringArray decodes a JSON string-array column into a Go slice,
// returning nil for empty/NULL/garbage so the field is omitted on the wire.
func parseJSONStringArray(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// renderPredicatesJSON encodes the predicates slice as an indented JSON
// array to w.
func renderPredicatesJSON(w io.Writer, predicates []predicateRow) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(predicates)
}

// renderPredicatesCLI prints a themed table of predicates to cmd's stdout.
func renderPredicatesCLI(cmd *cobra.Command, predicates []predicateRow) error {
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "predicate"},
		{Title: "kind"},
		{Title: "domain → range"},
		{Title: "attrs"},
		{Title: "added by"},
		{Title: "added at"},
		{Title: "description"},
	})
	for i := range predicates {
		r := &predicates[i] // pointer: predicateRow is 184 bytes, avoid rangeValCopy
		attrs := ""
		if len(r.PropertySchema) > 0 {
			attrs = "yes"
		}
		tbl.Row(
			r.Predicate,
			r.Kind,
			predicateDomainRange(r.SubjectKinds, r.ObjectKinds),
			attrs,
			r.AddedBy,
			r.AddedAt.Format(time.RFC3339),
			r.Description,
		)
	}
	return tbl.Render()
}

// predicateDomainRange renders the subject→object kind constraints for the CLI
// table; an unconstrained (open-world) side shows "*". Multiple kinds are
// comma-joined. Both unconstrained collapses to "* → *".
func predicateDomainRange(subjectKinds, objectKinds []string) string {
	side := func(ks []string) string {
		if len(ks) == 0 {
			return "*"
		}
		return strings.Join(ks, ",")
	}
	return side(subjectKinds) + " → " + side(objectKinds)
}

// wrapSchemaFatal prints the error to stderr and returns a silentError
// with code 2 so Execute exits with the right code without printing a
// duplicate message.
func wrapSchemaFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "schema", err)
}

// ---- entity-kinds subcommand ----

// entityKindRow is the in-memory representation of one entity_kinds table row
// joined to the live count from the entities table.
type entityKindRow struct {
	Kind        string    `json:"kind"`
	DisplayName string    `json:"displayName"`
	Provenance  string    `json:"provenance"`
	Extends     string    `json:"extends"`
	SameAs      []string  `json:"sameAs"`
	Count       int       `json:"count"`
	Description string    `json:"description"`
	AddedAt     time.Time `json:"addedAt"`
	AddedBy     string    `json:"addedBy"`
}

func newSchemaEntityKindsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entity-kinds",
		Short: "List every entity kind in the taxonomy with live counts",
		Long: `List every entity kind registered in the entity_kinds table, joined to
the live entity count from the entities table, ordered alphabetically.
Columns are:

  Kind         the kind identifier, e.g. person
  DisplayName  human-readable label
  Provenance   namespace: core, user, or extension:<name>
  Extends      parent kind in the subtype hierarchy (empty for root kinds)
  SameAs       comma-separated external ontology URIs (e.g. schema.org/Person)
  Count        number of entities of this kind currently in the store
  Description  one-sentence explanation

Exit codes:
  0   success
  2   unexpected failure: wiring or query error

Examples:
  # Human-readable table
  maz schema entity-kinds

  # Machine-readable JSON array
  maz schema entity-kinds --json

  # Pipe into jq
  maz schema entity-kinds --json | jq '[.[] | select(.provenance=="core")]'`,
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

// runSchemaEntityKinds opens SQLite, queries the entity_kinds and entities
// tables, and renders the result in the active output mode.
func runSchemaEntityKinds(cmd *cobra.Command) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	db, closeDB, err := openSchemaReadDB(ctx, cmd, dataDir, logger)
	if err != nil {
		return wrapSchemaFatal(cmd, err)
	}
	defer closeDB()

	kinds, err := queryEntityKinds(ctx, db)
	if err != nil {
		return wrapSchemaFatal(cmd, err)
	}
	// Guarantee a non-nil slice so JSON output emits [] on an empty registry
	// rather than null, which would fail clients validating against the
	// schema_entity_kinds_response.json array schema.
	if kinds == nil {
		kinds = []entityKindRow{}
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return renderEntityKindsJSON(w, kinds)
	}
	return renderEntityKindsCLI(cmd, kinds)
}

// queryEntityKinds queries the entity_kinds table LEFT JOINed to the live
// entity counts, returning active rows ordered by kind name. Proposed and
// rejected kinds are excluded from this listing; use `maz schema review list`
// to inspect proposed kinds.
func queryEntityKinds(ctx context.Context, db *sql.DB) ([]entityKindRow, error) {
	const q = `
		SELECT ek.name, ek.display_name, ek.provenance,
		       COALESCE(ek.parent,''), COALESCE(ek.same_as,''),
		       COALESCE(ek.description,''), ek.added_at, ek.added_by,
		       COALESCE(cnt.n, 0)
		  FROM entity_kinds ek
		  LEFT JOIN (
		        SELECT kind, COUNT(*) AS n FROM entities GROUP BY kind
		       ) cnt ON cnt.kind = ek.name
		 WHERE COALESCE(ek.status,'active') = 'active'
		 ORDER BY ek.name ASC`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query entity kinds: %w", err)
	}
	defer rows.Close()

	var out []entityKindRow
	for rows.Next() {
		r, serr := scanEntityKindRow(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan row: %w", serr)
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entity kinds: %w", err)
	}
	return out, nil
}

// scanEntityKindRow scans one row from the entity_kinds query result set.
func scanEntityKindRow(rows *sql.Rows) (entityKindRow, error) {
	var r entityKindRow
	var addedAt, sameAsRaw string
	if err := rows.Scan(
		&r.Kind, &r.DisplayName, &r.Provenance,
		&r.Extends, &sameAsRaw,
		&r.Description, &addedAt, &r.AddedBy,
		&r.Count,
	); err != nil {
		return entityKindRow{}, err
	}
	t, err := time.Parse(time.RFC3339, addedAt)
	if err != nil {
		t = time.Time{}
	}
	r.AddedAt = t.UTC()
	// same_as is stored as a JSON array text; parse it if non-empty.
	if sameAsRaw != "" && sameAsRaw != jsonNull && sameAsRaw != "[]" {
		var uris []string
		if jerr := json.Unmarshal([]byte(sameAsRaw), &uris); jerr == nil {
			r.SameAs = uris
		}
	}
	return r, nil
}

// renderEntityKindsJSON encodes the entity kinds slice as an indented JSON
// array to w.
func renderEntityKindsJSON(w io.Writer, kinds []entityKindRow) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(kinds)
}

// renderEntityKindsCLI prints a themed table of entity kinds to cmd's stdout.
// The SameAs column is joined with commas.
func renderEntityKindsCLI(cmd *cobra.Command, kinds []entityKindRow) error {
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "kind"},
		{Title: "display name"},
		{Title: "provenance"},
		{Title: "extends"},
		{Title: "same as"},
		{Title: "count", Align: output.AlignRight},
		{Title: "description"},
	})
	for i := range kinds {
		r := &kinds[i]
		tbl.Row(r.Kind, r.DisplayName, r.Provenance, r.Extends, strings.Join(r.SameAs, ","), r.Count, r.Description)
	}
	return tbl.Render()
}
