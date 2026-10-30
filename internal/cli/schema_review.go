package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// newSchemaReviewCmd returns the `maz schema review` command tree, which
// provides list / approve / reject subcommands for the P7 governance surface.
func newSchemaReviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Inspect and disposition proposed entity kinds in the governance queue",
		Long: `Commands for the P7 governed-mode entity-kind review queue.

In governed mode ([ontology] mode="governed" in config.toml) new entity kinds
minted by the resolver or via defineEntityKind are persisted as "proposed"
rather than immediately active. Use these subcommands to inspect and
disposition them.

  list     — list all proposed kinds awaiting review
  approve  — approve a proposed kind (makes it active)
  reject   — reject a proposed kind (marks it rejected)

Exit codes:
  0   success
  1   kind not found or already actioned (approve/reject)
  2   unexpected failure: wiring or query error

Examples:
  maz schema review list
  maz schema review list --json
  maz schema review approve user:vehicle
  maz schema review reject user:widget`,
	}
	cmd.AddCommand(newSchemaReviewListCmd())
	cmd.AddCommand(newSchemaReviewApproveCmd())
	cmd.AddCommand(newSchemaReviewRejectCmd())
	return cmd
}

// kindStatusProposed is the status string for entity kinds awaiting governance
// review. Extracted as a constant so the Go comparison path is kept DRY.
const kindStatusProposed = "proposed"

// proposedKindRow is the in-memory representation of one proposed entity_kinds
// table row for the review list output.
type proposedKindRow struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Description string    `json:"description"`
	Provenance  string    `json:"provenance"`
	AddedBy     string    `json:"addedBy"`
	AddedAt     time.Time `json:"addedAt"`
}

func newSchemaReviewListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List proposed entity kinds awaiting governance review",
		Long: `List every entity kind whose status is "proposed" in the entity_kinds
projection table. These are kinds queued for human review in governed mode
([ontology] mode="governed" in config.toml).

Columns are:
  Name         the kind identifier, e.g. user:vehicle
  DisplayName  human-readable label
  Provenance   namespace: user or extension:<name>
  AddedBy      agent that minted the kind: llm, user, or extension
  AddedAt      when the kind was proposed

Use "maz schema review approve <name>" or "reject <name>" to disposition.

Exit codes:
  0   success
  2   unexpected failure: wiring or query error`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSchemaReviewList(cmd)
		},
	}
	cmd.Annotations = map[string]string{
		"schema.response": "schemas/cli/schema_review_list_response.json",
	}
	return cmd
}

func newSchemaReviewApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve <name>",
		Short: "Approve a proposed entity KIND (taxonomy), making it active",
		Long: `Approve the named proposed entity KIND in the ontology taxonomy by appending
an entityKindReviewed event with decision="approved". The dispatcher
materialises the event by updating the kind's status to "active".

This command operates on the TAXONOMY (entity kinds such as user:vehicle),
NOT on entity records and NOT on relations. The three "approve" paths in maz
are distinct:
  maz schema review approve <kind>   — approves an entity KIND (this command)
  maz review approve <entity-uid>    — confirms a TENTATIVE entity record
  maz promote relations/{uid}        — verifies a PROPOSED relation edge

Related:
  maz review approve <entity-uid>    confirm a tentative entity (resolver queue)
  maz promote relations/{uid}        verify a proposed relation (trust promotion)

Exit codes:
  0   success
  1   kind not found or not in "proposed" status
  2   unexpected failure`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaReviewDecision(cmd, args[0], ontology.EntityKindReviewApproved)
		},
	}
	cmd.Flags().String("note", "", "Optional free-text note recorded in the entityKindReviewed event")
	return cmd
}

func newSchemaReviewRejectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reject <name>",
		Short: "Reject a proposed entity kind, excluding it from the taxonomy",
		Long: `Reject the named proposed entity kind by appending an
entityKindReviewed event with decision="rejected". The dispatcher materialises
the event by updating the kind's status to "rejected".

Exit codes:
  0   success
  1   kind not found or not in "proposed" status
  2   unexpected failure`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaReviewDecision(cmd, args[0], ontology.EntityKindReviewRejected)
		},
	}
	cmd.Flags().String("note", "", "Optional free-text note recorded in the entityKindReviewed event")
	return cmd
}

// runSchemaReviewList queries the entity_kinds table for proposed rows and
// renders the result.
func runSchemaReviewList(cmd *cobra.Command) error {
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

	rows, err := queryProposedKinds(ctx, db)
	if err != nil {
		return wrapSchemaFatal(cmd, err)
	}
	if rows == nil {
		rows = []proposedKindRow{}
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return renderProposedKindsJSON(w, rows)
	}
	return renderProposedKindsCLI(cmd, rows)
}

// runSchemaReviewDecision appends an entityKindReviewed event for the named
// kind and prints the result.
func runSchemaReviewDecision(cmd *cobra.Command, kindName string, decision ontology.EntityKindReviewDecision) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// No reviewEntityKind MCP tool exists, so when a daemon holds the store lock
	// the only safe path is to refuse cleanly rather than race its exclusive
	// writer (this is a WRITE: it appends an entityKindReviewed event).
	if refused, rerr := refuseSchemaWriteUnderDaemon(cmd, dataDir, "schema review"); refused {
		return rerr
	}

	wired, err := daemon.OpenWiring(ctx, daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("open wiring: %w", err))
	}
	defer func() {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("schema review: close wiring", zap.Error(cerr))
		}
	}()

	db, err := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if err != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("open sqlite: %w", err))
	}

	// Verify the kind exists and is proposed.
	status, found, qerr := queryKindStatus(ctx, db, kindName)
	if qerr != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("query kind status: %w", qerr))
	}
	if !found {
		//nolint:errcheck // best-effort stderr on not-found path
		fmt.Fprintf(cmd.ErrOrStderr(), "schema review: kind %q not found\n", kindName)
		return silentError{code: 1, err: fmt.Errorf("kind %q not found", kindName)}
	}
	if status != kindStatusProposed {
		//nolint:errcheck // best-effort stderr on validation path
		fmt.Fprintf(cmd.ErrOrStderr(), "schema review: kind %q has status %q (expected proposed)\n", kindName, status)
		return silentError{code: 1, err: fmt.Errorf("kind %q has status %q, expected proposed", kindName, status)}
	}

	// Append the entityKindReviewed event. The --note flag is optional; an
	// empty string is omitted from the event (the field is omitempty).
	note, _ := cmd.Flags().GetString("note")

	// Append the entityKindReviewed event.
	payload := ontology.EntityKindReviewedEvent{
		Name:       kindName,
		Decision:   decision,
		ReviewedBy: "cli",
		ReviewedAt: timeNowUTC(),
		Note:       note,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("marshal event: %w", err))
	}
	if _, err := wired.Orchestrator.Log.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityKindReviewed),
		Source:     "cli/schema-review",
		Confidence: 1.0,
		Payload:    raw,
	}}); err != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("append review event: %w", err))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	decisionStr := string(decision)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{
			"name":     kindName,
			"decision": decisionStr,
		})
	}
	_, err = fmt.Fprintf(w, "schema review: %s %s\n", kindName, decisionStr)
	return err
}

// timeNowUTC returns the current UTC time. Indirected so tests can verify the
// clock is live; the review commands call it inline.
func timeNowUTC() time.Time { return time.Now().UTC() }

// queryProposedKinds returns all entity_kinds rows with status='proposed'.
func queryProposedKinds(ctx context.Context, db *sql.DB) ([]proposedKindRow, error) {
	const q = `
		SELECT name, display_name, description, provenance, added_by, added_at
		  FROM entity_kinds
		 WHERE COALESCE(status,'active') = 'proposed'
		 ORDER BY name ASC`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query proposed kinds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []proposedKindRow
	for rows.Next() {
		r, serr := scanProposedKindRow(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan proposed kind: %w", serr)
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate proposed kinds: %w", err)
	}
	return out, nil
}

// scanProposedKindRow scans one row from the proposed-kinds query result.
func scanProposedKindRow(rows *sql.Rows) (proposedKindRow, error) {
	var r proposedKindRow
	var addedAt string
	if err := rows.Scan(&r.Name, &r.DisplayName, &r.Description, &r.Provenance, &r.AddedBy, &addedAt); err != nil {
		return proposedKindRow{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, addedAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, addedAt)
		if err != nil {
			t = time.Time{}
		}
	}
	r.AddedAt = t.UTC()
	return r, nil
}

// queryKindStatus returns the status of the named kind and whether the row
// exists.
func queryKindStatus(ctx context.Context, db *sql.DB, name string) (status string, found bool, err error) {
	qerr := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = ?`, name,
	).Scan(&status)
	if qerr == sql.ErrNoRows {
		return "", false, nil
	}
	if qerr != nil {
		return "", false, qerr
	}
	return status, true, nil
}

// renderProposedKindsJSON encodes the proposed-kinds slice as an indented JSON
// array to w.
func renderProposedKindsJSON(w io.Writer, rows []proposedKindRow) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// renderProposedKindsCLI prints a themed table of proposed kinds to cmd's stdout.
func renderProposedKindsCLI(cmd *cobra.Command, rows []proposedKindRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no entity kinds pending review")
		return err
	}
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "name"},
		{Title: "display name"},
		{Title: "provenance"},
		{Title: "added by"},
		{Title: "added at"},
	})
	for _, r := range rows {
		tbl.Row(r.Name, r.DisplayName, r.Provenance, r.AddedBy, r.AddedAt.Format(time.RFC3339))
	}
	return tbl.Render()
}
