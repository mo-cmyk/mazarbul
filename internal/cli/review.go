package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

func newReviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Review queues: tentative entity merges and connector-proposed relations",
		Long: `Subcommands for two distinct review queues under the resolver:

  list / approve / reject   — entity-merge queue: entity RECORDS whose
                              resolution_status is tentative or split_candidate
                              (gray-zone merges flagged by the resolver).
  proposed                  — relation trust queue: connector-extracted RELATION
                              edges whose trust status is proposed, awaiting
                              human verification before being treated as asserted.

The three "approve" paths in maz are intentionally distinct:
  maz review approve <entity-uid>    — confirms a tentative ENTITY record (here)
  maz schema review approve <kind>   — approves a proposed entity KIND (taxonomy)
  maz promote relations/{uid}        — verifies a proposed RELATION edge

Examples:
  maz review list
  maz review approve 01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz review proposed
  maz review proposed --approve-all`,
	}
	cmd.AddCommand(newReviewListCmd())
	cmd.AddCommand(newReviewApproveCmd())
	cmd.AddCommand(newReviewRejectCmd())
	cmd.AddCommand(newReviewConflictsCmd())
	cmd.AddCommand(newReviewProposedCmd())
	cmd.AddCommand(newReviewDupsCmd())
	return cmd
}

func newReviewListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List entities flagged for review",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReviewList(cmd)
		},
	}
}

func newReviewApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <entity-uid>",
		Short: "Confirm a tentative entity record in the resolver queue",
		Long: `Set resolution_status to "confirmed" for an ENTITY record the resolver
flagged as tentative (a gray-zone merge or low-confidence match). This
operates on the entity-merge queue — not on relation edges and not on
entity kinds in the taxonomy.

The three "approve" paths in maz are intentionally distinct:
  maz review approve <entity-uid>    — confirms a tentative ENTITY record (this command)
  maz schema review approve <kind>   — approves a proposed entity KIND (taxonomy)
  maz promote relations/{uid}        — verifies a proposed RELATION edge (trust status)

Related:
  maz review list                    entities awaiting disposition
  maz review proposed                proposed-relation trust queue
  maz promote relations/{uid}        promote a single proposed relation

Exit codes:
  0   success
  1   entity not found
  2   unexpected failure

Examples:
  maz review approve 01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz review list | maz review approve -`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReviewDisposition(cmd, args[0], "confirmed")
		},
	}
}

func newReviewRejectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reject <entity-uid>",
		Short: "Mark a tentative entity as split_candidate (human review)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReviewDisposition(cmd, args[0], "split_candidate")
		},
	}
}

type reviewRow struct {
	UID         string  `json:"uid"`
	Kind        string  `json:"kind"`
	DisplayName string  `json:"displayName"`
	Confidence  float64 `json:"confidence"`
	Status      string  `json:"resolutionStatus"`
}

func runReviewList(cmd *cobra.Command) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return wrapReviewFatal(cmd, err)
	}
	defer closeStoreQuietly(store)

	rows, err := store.Reader().QueryContext(ctx,
		`SELECT uid, kind, display_name, confidence, resolution_status
		 FROM entities
		 WHERE resolution_status IN ('tentative', 'split_candidate')
		 ORDER BY confidence ASC, create_time DESC
		 LIMIT 200`)
	if err != nil {
		return wrapReviewFatal(cmd, fmt.Errorf("query: %w", err))
	}
	defer func() { _ = rows.Close() }()

	var out []reviewRow
	for rows.Next() {
		var r reviewRow
		if scanErr := rows.Scan(&r.UID, &r.Kind, &r.DisplayName, &r.Confidence, &r.Status); scanErr != nil {
			return wrapReviewFatal(cmd, fmt.Errorf("scan: %w", scanErr))
		}
		out = append(out, r)
	}
	if iterErr := rows.Err(); iterErr != nil {
		return wrapReviewFatal(cmd, fmt.Errorf("iterate: %w", iterErr))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(out)
	}
	if len(out) == 0 {
		_, err := fmt.Fprintln(w, "no entities pending review")
		return err
	}
	for _, r := range out {
		if _, err := fmt.Fprintf(w, "%-26s  %-12s  %-30s  conf=%.2f  %s\n",
			r.UID, r.Kind, r.DisplayName, r.Confidence, r.Status); err != nil {
			return err
		}
	}
	return nil
}

func runReviewDisposition(cmd *cobra.Command, entityUID, status string) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return wrapReviewFatal(cmd, err)
	}
	defer closeStoreQuietly(store)

	res, err := store.Writer().ExecContext(ctx,
		`UPDATE entities SET resolution_status = ? WHERE uid = ?`,
		status, entityUID,
	)
	if err != nil {
		return wrapReviewFatal(cmd, fmt.Errorf("update: %w", err))
	}
	affected, raErr := res.RowsAffected()
	if raErr != nil {
		return wrapReviewFatal(cmd, fmt.Errorf("rows affected: %w", raErr))
	}
	if affected == 0 {
		//nolint:errcheck // best-effort stderr on validation path
		fmt.Fprintf(cmd.ErrOrStderr(), "review: entity %s not found\n", entityUID)
		return silentError{code: 1, err: fmt.Errorf("entity %s not found", entityUID)}
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{
			"name":             "entities/" + entityUID,
			"resolutionStatus": status,
		})
	}
	_, err = fmt.Fprintf(w, "entities/%s set to %s\n", entityUID, status)
	return err
}

func wrapReviewFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "review", err)
}
