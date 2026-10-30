package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// docLintDeadLink is one dead-link document in the lint report. It mirrors the
// lintDocuments tool's wire shape (documentUid/path/title/unresolved) so the
// report round-trips whether it came from the daemon transport or the
// in-process fallback. Unresolved names the exact [[wikilink]] targets that
// resolve to nothing, so the report says which link to fix, not just which file.
type docLintDeadLink struct {
	DocumentUID string   `json:"documentUid"`
	Path        string   `json:"path"`
	Title       string   `json:"title,omitempty"`
	Unresolved  []string `json:"unresolved"`
}

// docLintNameCollision is one name-collision finding: a document whose title
// (display name) shadows a non-document entity of the same name, so wikilink
// resolution silently favours the non-document entity. Mirrors the lintDocuments
// tool's nameCollisions wire shape. This is the category the doc<->entity
// coexistence wave added; surfacing it here keeps CLI/MCP lint parity.
type docLintNameCollision struct {
	DocumentUID  string `json:"documentUid"`
	Path         string `json:"path,omitempty"`
	Title        string `json:"title"`
	ShadowedKind string `json:"shadowedKind"`
	ShadowedUID  string `json:"shadowedUid"`
	Hint         string `json:"hint"`
}

// docLintReport aggregates the lint categories into a single output payload.
// The JSON shape is the contract with the lintDocuments MCP tool; all slices
// are non-nil so JSON consumers always see arrays.
type docLintReport struct {
	Orphans        []navigate.LintOrphanDoc        `json:"orphans"`
	DeadLinks      []docLintDeadLink               `json:"deadLinks"`
	Contradictions []navigate.LintContradictionDoc `json:"contradictions"`
	DanglingEdges  []navigate.LintDanglingEdge     `json:"danglingEdges"`
	NearDuplicates []navigate.LintNearDuplicateDoc `json:"nearDuplicates"`
	NameCollisions []docLintNameCollision          `json:"nameCollisions"`
}

// normalizeArrays replaces nil category slices with empty ones so JSON output
// always carries iterable arrays (never null), even when an older daemon's
// payload omitted a category this CLI knows about.
func (r *docLintReport) normalizeArrays() {
	if r.Orphans == nil {
		r.Orphans = []navigate.LintOrphanDoc{}
	}
	if r.DeadLinks == nil {
		r.DeadLinks = []docLintDeadLink{}
	}
	if r.Contradictions == nil {
		r.Contradictions = []navigate.LintContradictionDoc{}
	}
	if r.DanglingEdges == nil {
		r.DanglingEdges = []navigate.LintDanglingEdge{}
	}
	if r.NearDuplicates == nil {
		r.NearDuplicates = []navigate.LintNearDuplicateDoc{}
	}
	if r.NameCollisions == nil {
		r.NameCollisions = []docLintNameCollision{}
	}
}

// errDocLintIssues is the sentinel returned when lint finds at least one
// issue; the CLI maps it to exit code 1.
var errDocLintIssues = errors.New("doc lint: issues found")

// noTitlePlaceholder labels a document that has no title in lint output.
const noTitlePlaceholder = "(no title)"

// newDocLintCmd returns the `maz doc lint` subcommand. It is read-only: it runs
// the lintDocuments tool through the running daemon (or in-process when no
// daemon is up) and reports three categories of document health issues without
// writing any events. Routing through the daemon — like every other `doc` read
// command — is why this no longer dies with a cryptic "open database with
// status 1" while a daemon holds the graph lock (the normal case).
func newDocLintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint",
		Short: "Report document health issues (orphans, dead links, contradictions)",
		Long: `Run a read-only health check over the markdown vault and report:

  orphans         documents with no active inbound or outbound graph edges
  dead-links      documents that still carry unresolved [[wikilinks]]
  dangling-links  live documents that still link to a deleted document
  contradictions  document pairs connected by a "contradicts" edge
  near-duplicates pairs of documents whose chunk embeddings are suspiciously
                  similar (cosine >= 0.92); non-blocking lint signal only
  name-collisions documents whose title shadows a same-named non-document
                  entity, so wikilinks silently favour the entity

Exit codes: 0 = no issues, 1 = issues found, 2 = fatal error.
Use --output json|ndjson for machine-readable output.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := invokeNavTool(cmd, "lintDocuments", json.RawMessage(`{}`))
			if err != nil {
				//nolint:errcheck // best-effort stderr on fatal path
				fmt.Fprintf(cmd.ErrOrStderr(), "doc lint: %v\n", err)
				return silentError{code: 2, err: err}
			}
			return decodeAndRenderDocLint(cmd, raw)
		},
	}
}

// decodeAndRenderDocLint turns a lintDocuments tool result into rendered output
// and the lint exit code. A nil raw means the tool was not registered (older or
// minimal daemon); it is surfaced via the shared errNavUnavailableMsg handling
// like every other navigation read, not as a cryptic JSON parse error.
func decodeAndRenderDocLint(cmd *cobra.Command, raw json.RawMessage) error {
	if raw == nil {
		//nolint:errcheck // best-effort stderr on fatal path
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: errors.New(errNavUnavailableMsg)}
	}

	var report docLintReport
	if uErr := json.Unmarshal(raw, &report); uErr != nil {
		//nolint:errcheck // best-effort stderr on fatal path
		fmt.Fprintf(cmd.ErrOrStderr(), "doc lint: parse response: %v\n", uErr)
		return silentError{code: 2, err: uErr}
	}
	// The daemon's lintDocuments tool emits every category as an array, never
	// null. But an OLDER daemon may not carry a category this CLI knows (e.g.
	// danglingEdges), leaving its slice nil → marshaled as null and breaking the
	// "always iterable arrays" contract this CLI re-emits in JSON mode. Normalize
	// so the CLI's own output honours the contract regardless of the daemon age.
	report.normalizeArrays()

	if rErr := renderDocLintReport(cmd, &report); rErr != nil {
		return rErr
	}

	hasIssues := len(report.Orphans) > 0 ||
		len(report.DeadLinks) > 0 ||
		len(report.Contradictions) > 0 ||
		len(report.DanglingEdges) > 0 ||
		len(report.NearDuplicates) > 0 ||
		len(report.NameCollisions) > 0
	if hasIssues {
		return silentError{code: 1, err: errDocLintIssues}
	}
	return nil
}

// renderDocLintReport writes the lint report to cmd's stdout in the active
// output format (text, json, or ndjson).
func renderDocLintReport(cmd *cobra.Command, report *docLintReport) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		raw, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("doc lint: marshal report: %w", err)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	return renderDocLintText(cmd, report)
}

// renderDocLintText prints all lint categories as aligned tabwriter sections.
// An all-clear message is printed when no issues are found.
func renderDocLintText(cmd *cobra.Command, report *docLintReport) error {
	out := cmd.OutOrStdout()
	total := len(report.Orphans) + len(report.DeadLinks) +
		len(report.Contradictions) + len(report.DanglingEdges) +
		len(report.NearDuplicates) + len(report.NameCollisions)
	if total == 0 {
		_, err := fmt.Fprintln(out, "ok  no document issues found")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if err := writeLintOrphans(tw, report.Orphans); err != nil {
		return err
	}
	if err := writeLintDeadLinks(tw, report.DeadLinks); err != nil {
		return err
	}
	if err := writeLintDanglingEdges(tw, report.DanglingEdges); err != nil {
		return err
	}
	if err := writeLintContradictions(tw, report.Contradictions); err != nil {
		return err
	}
	if err := writeLintNearDuplicates(tw, report.NearDuplicates); err != nil {
		return err
	}
	if err := writeLintNameCollisions(tw, report.NameCollisions); err != nil {
		return err
	}
	return tw.Flush()
}

// writeLintNameCollisions appends the NAME COLLISIONS section to tw: documents
// whose title shadows a non-document entity of the same name. No-op when empty.
func writeLintNameCollisions(tw io.Writer, collisions []docLintNameCollision) error {
	if len(collisions) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(tw, "NAME COLLISIONS (%d) — document title shadows a same-named entity\n", len(collisions)); err != nil {
		return err
	}
	for _, c := range collisions {
		title := c.Title
		if title == "" {
			title = noTitlePlaceholder
		}
		if _, err := fmt.Fprintf(tw, "  %s\tshadows %s (%s)\t%s\n", title, c.ShadowedKind, c.ShadowedUID, c.Hint); err != nil {
			return err
		}
	}
	return nil
}

// writeLintOrphans appends the ORPHANS section to tw. No-op when the slice is empty.
func writeLintOrphans(tw io.Writer, orphans []navigate.LintOrphanDoc) error {
	if len(orphans) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(tw, "ORPHANS (%d) — no inbound or outbound edges\n", len(orphans)); err != nil {
		return err
	}
	for _, o := range orphans {
		title := o.Title
		if title == "" {
			title = noTitlePlaceholder
		}
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", o.Path, title); err != nil {
			return err
		}
	}
	return nil
}

// writeLintDeadLinks appends the DEAD LINKS section to tw. No-op when the slice is empty.
func writeLintDeadLinks(tw io.Writer, deadLinks []docLintDeadLink) error {
	if len(deadLinks) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(tw, "DEAD LINKS (%d) — unresolved [[wikilinks]]\n", len(deadLinks)); err != nil {
		return err
	}
	for _, d := range deadLinks {
		title := d.Title
		if title == "" {
			title = noTitlePlaceholder
		}
		targets := "(unknown)"
		if len(d.Unresolved) > 0 {
			targets = "[[" + strings.Join(d.Unresolved, "]], [[") + "]]"
		}
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\n", d.Path, title, targets); err != nil {
			return err
		}
	}
	return nil
}

// writeLintDanglingEdges appends the DANGLING LINKS section to tw: live
// documents that still link to a deleted document. No-op when the slice is empty.
func writeLintDanglingEdges(tw io.Writer, dangling []navigate.LintDanglingEdge) error {
	if len(dangling) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(tw, "DANGLING LINKS (%d) — link to a deleted document\n", len(dangling)); err != nil {
		return err
	}
	for _, d := range dangling {
		target := d.TargetTitle
		if target == "" {
			target = d.TargetPath
		}
		if _, err := fmt.Fprintf(tw, "  %s\t%s->\t[[%s]] (deleted: %s)\n", d.SubjectPath, d.Predicate, target, d.TargetPath); err != nil {
			return err
		}
	}
	return nil
}

// writeLintNearDuplicates appends the NEAR DUPLICATES section to tw.
// No-op when the slice is empty.
func writeLintNearDuplicates(tw io.Writer, pairs []navigate.LintNearDuplicateDoc) error {
	if len(pairs) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(tw, "NEAR DUPLICATES (%d) — chunk embeddings cosine >= %.2f (non-blocking)\n",
		len(pairs), navigate.NearDuplicateSimilarityThreshold); err != nil {
		return err
	}
	for _, p := range pairs {
		if _, err := fmt.Fprintf(tw, "  %s\t<~>\t%s\t(sim %.3f)\n",
			p.SubjectPath, p.ObjectPath, p.Similarity); err != nil {
			return err
		}
	}
	return nil
}

// writeLintContradictions appends the CONTRADICTIONS section to tw. No-op when the slice is empty.
func writeLintContradictions(tw io.Writer, contradictions []navigate.LintContradictionDoc) error {
	if len(contradictions) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(tw, "CONTRADICTIONS (%d) — linked by contradicts edge\n", len(contradictions)); err != nil {
		return err
	}
	for _, c := range contradictions {
		if _, err := fmt.Fprintf(tw, "  %s\t<->\t%s\n", c.SubjectPath, c.ObjectPath); err != nil {
			return err
		}
	}
	return nil
}
