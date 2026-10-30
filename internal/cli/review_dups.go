package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

func newReviewDupsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dups",
		Short: "List cross-device duplicate entity candidates",
		Long: `List entity pairs that the post-consume dedup sweep flagged as likely
duplicates after cross-device team-scope sync. The sweep runs automatically
after each graft pass and populates entity_dup_candidates.

Each row shows the two entity UIDs, the detection reason, and the similarity
score. Use maz entity merge to consolidate a pair:

  maz entity merge <source-uid> <target-uid>

Lane meanings:
  exact_name   Same entity kind + case-insensitive display name. High
               confidence; may be a homonym (two "John Smith" people).
  name_emb     Name embedding cosine similarity above the advisory
               threshold. Softer signal; review before merging.

Cross-device auto-merges (strong-identifier equality such as shared email or
username) are applied automatically by the sweep and do not appear here.

Examples:
  maz review dups
  maz review dups --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReviewDups(cmd)
		},
	}
}

// dupCandidateRow is one row from entity_dup_candidates.
type dupCandidateRow struct {
	EntityUIDa string  `json:"entityUidA"`
	EntityUIDb string  `json:"entityUidB"`
	Reason     string  `json:"reason"`
	Score      float64 `json:"score"`
	DetectedAt string  `json:"detectedAt"`
}

func runReviewDups(cmd *cobra.Command) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return wrapReviewFatal(cmd, err)
	}
	defer closeStoreQuietly(store)

	rows, err := store.Reader().QueryContext(ctx,
		`SELECT entity_uid_a, entity_uid_b, reason, score, detected_at
		   FROM entity_dup_candidates
		  ORDER BY score DESC, detected_at DESC
		  LIMIT 200`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			w := cmd.OutOrStdout()
			_, werr := fmt.Fprintln(w, "no duplicate candidates (dedup sweep has not run yet)")
			return werr
		}
		return wrapReviewFatal(cmd, fmt.Errorf("query: %w", err))
	}
	defer func() { _ = rows.Close() }()

	out := []dupCandidateRow{}
	for rows.Next() {
		var r dupCandidateRow
		if serr := rows.Scan(&r.EntityUIDa, &r.EntityUIDb, &r.Reason, &r.Score, &r.DetectedAt); serr != nil {
			return wrapReviewFatal(cmd, fmt.Errorf("scan: %w", serr))
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
		_, err := fmt.Fprintln(w, "no duplicate candidates")
		return err
	}
	for _, r := range out {
		if _, ferr := fmt.Fprintf(w, "%-26s  vs  %-26s  reason=%-10s  score=%.2f  detected=%s\n",
			r.EntityUIDa, r.EntityUIDb, r.Reason, r.Score, r.DetectedAt); ferr != nil {
			return ferr
		}
	}
	return nil
}
