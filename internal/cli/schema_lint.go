package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
)

// lintNearDupLexicalThreshold is the word-level LCS similarity floor used by
// lintNearDuplicateKinds when no embedder is available. It is intentionally
// separate from dedup_medium_threshold, which is calibrated for cosine
// similarity of dense embedding vectors (typical values 0.75–0.90) and fires
// only when scores are very high. Jaccard or character-level LCS over free
// prose text almost never reaches those cosine-calibrated values, so reusing
// 0.82 as a lexical floor makes the check effectively dead.
//
// 0.50 is chosen because word-level LCS similarity over "displayName
// description" text reaches ~0.50 for semantically close pairs (e.g. "vehicle
// A road vehicle." vs "auto A road automobile vehicle." scores 0.60) while
// unrelated pairs score below 0.30. The 0.50 floor gives a meaningful
// precision/recall trade-off in the lexical-only fallback path.
const lintNearDupLexicalThreshold = 0.50

// newSchemaLintCmd returns the `maz schema lint` command, a read-only hygiene
// report over the ontology registry.
func newSchemaLintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Run ontology hygiene checks: near-duplicate kinds, unused predicates, missing sameAs",
		Long: `Read-only ontology hygiene report. Checks:

  near-duplicate kinds   — pairs of active kinds whose names are lexically
                           similar above the dedup medium threshold; a human
                           should review whether they should be merged.
  unused predicates      — predicates in the vocabulary with zero live
                           relations referencing them (relations table).
  missing sameAs         — non-core active kinds with no sameAs IRI, which
                           makes cross-vocabulary alignment impossible.

Exit codes:
  0   success (findings may still exist; use --strict for a non-zero exit)
  2   unexpected failure: wiring or query error

Flags:
  --strict   exit 1 when any finding exists

Examples:
  maz schema lint
  maz schema lint --json
  maz schema lint --strict`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSchemaLint(cmd)
		},
	}
	cmd.Flags().Bool("strict", false, "Exit 1 when any finding exists")
	return cmd
}

// lintFinding is one hygiene finding returned by a lint check.
type lintFinding struct {
	// Check is the lint rule that fired, e.g. "near-duplicate-kinds".
	Check string `json:"check"`
	// Name is the primary subject of the finding, e.g. the kind name or
	// predicate name. Empty for findings that span multiple subjects.
	Name string `json:"name,omitempty"`
	// Message is a human-readable description of the finding.
	Message string `json:"message"`
}

// lintReport is the full output of `maz schema lint`.
type lintReport struct {
	// Findings contains every lint finding, grouped by check name.
	Findings []lintFinding `json:"findings"`
	// TotalFindings is the total count across all checks.
	TotalFindings int `json:"totalFindings"`
}

// runSchemaLint opens the SQLite store and runs all three lint checks.
func runSchemaLint(cmd *cobra.Command) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	nearDupThreshold := lintNearDupThreshold(dataDir, logger)

	db, embedder, closeDB, err := openSchemaLintDB(ctx, cmd, dataDir, logger)
	if err != nil {
		return wrapSchemaFatal(cmd, err)
	}
	defer closeDB()

	findings, err := runAllLintChecks(ctx, db, embedder, nearDupThreshold)
	if err != nil {
		return wrapSchemaFatal(cmd, err)
	}

	report := lintReport{
		Findings:      findings,
		TotalFindings: len(findings),
	}
	if report.Findings == nil {
		report.Findings = []lintFinding{}
	}

	if err := writeLintReport(cmd, report); err != nil {
		return wrapSchemaFatal(cmd, err)
	}

	// --strict: exit 1 when any finding exists.
	strict, _ := cmd.Flags().GetBool("strict")
	if strict && len(findings) > 0 {
		return silentError{code: 1, err: fmt.Errorf("lint: %d finding(s)", len(findings))}
	}
	return nil
}

// lintNearDupThreshold loads the user config and returns the embedding-based
// near-duplicate similarity floor used when an embedder is available. Returns
// dedup.MintAliasThreshold when the config is absent or the field is zero.
// This value is a cosine-similarity threshold (0..1) and must NOT be used as
// a lexical threshold; use lintNearDupLexicalThreshold for the lexical path.
func lintNearDupThreshold(dataDir string, logger *zap.Logger) float64 {
	userCfg, cfgErr := config.LoadUserConfig(dataDir)
	if cfgErr != nil {
		logger.Debug("schema lint: user config unavailable; using default dedup threshold",
			zap.Error(cfgErr))
	}
	if userCfg.Ontology.DedupMediumThreshold != 0 {
		return userCfg.Ontology.DedupMediumThreshold
	}
	return dedup.MintAliasThreshold
}

// runAllLintChecks runs the three lint checks and returns the combined findings
// slice. The embedder may be nil; when nil the near-duplicate check falls back
// to lexical similarity. Returns the first non-nil error encountered.
func runAllLintChecks(ctx context.Context, db *sql.DB, embedder kernel.Embedder, nearDupThreshold float64) ([]lintFinding, error) {
	nearDups, err := lintNearDuplicateKinds(ctx, db, embedder, nearDupThreshold)
	if err != nil {
		return nil, fmt.Errorf("near-duplicate check: %w", err)
	}
	unusedPreds, err := lintUnusedPredicates(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("unused predicates check: %w", err)
	}
	missingSameAs, err := lintMissingSameAs(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("missing sameAs check: %w", err)
	}
	findings := make([]lintFinding, 0, len(nearDups)+len(unusedPreds)+len(missingSameAs))
	findings = append(findings, nearDups...)
	findings = append(findings, unusedPreds...)
	findings = append(findings, missingSameAs...)
	return findings, nil
}

// writeLintReport writes the lint report to cmd's stdout in the format
// requested by the --json / --ndjson flag.
func writeLintReport(cmd *cobra.Command, report lintReport) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	switch wr.Format() {
	case output.FormatNDJSON:
		// NDJSON: emit one JSON object per line, each representing a single
		// finding. An empty findings list produces no output lines.
		enc := json.NewEncoder(w)
		for i := range report.Findings {
			if err := enc.Encode(report.Findings[i]); err != nil {
				return err
			}
		}
	case output.FormatJSON:
		// JSON: emit the aggregate report as a single indented JSON object.
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	default:
		return renderLintReportCLI(w, report)
	}
	return nil
}

// lintNearDuplicateKinds returns near-duplicate kind pairs above threshold.
//
// The ADR (dynamic-ontology.md §Lint checks) specifies detection via
// "displayName — description" embeddings exceeding dedup_medium_threshold
// (a cosine similarity floor). This function implements that ADR-specified
// behaviour when an embedder is available, and degrades gracefully to a
// calibrated lexical fallback when none is wired (e.g. offline / no Ollama).
//
// When embedder != nil: embeds each kind's "displayName — description" text
// and reports pairs whose cosine similarity >= nearDupThreshold (the
// embedding-calibrated floor, typically ~0.75–0.82).
//
// When embedder == nil: computes word-level LCS similarity over the
// concatenated "displayName description" text and reports pairs above
// lintNearDupLexicalThreshold (0.50). This is intentionally separate from
// nearDupThreshold because cosine-calibrated thresholds (0.75–0.82) are far
// too high for lexical similarity — prose text rarely reaches Jaccard or LCS
// scores above 0.50 even for semantically identical descriptions.
//
// Per the ADR, comparison is always over displayName + description text, not
// the kind name identifier, so kinds with different names but near-identical
// semantics (e.g. "user:automobile" vs "user:vehicle" both described as
// "A road vehicle") are still caught.
func lintNearDuplicateKinds(ctx context.Context, db *sql.DB, embedder kernel.Embedder, nearDupThreshold float64) ([]lintFinding, error) {
	const q = `SELECT name, COALESCE(display_name,''), COALESCE(description,'')
	             FROM entity_kinds
	            WHERE COALESCE(status,'active') = 'active'
	            ORDER BY name ASC`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query kinds for dedup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var kinds []kindEntry
	for rows.Next() {
		var e kindEntry
		if serr := rows.Scan(&e.name, &e.displayName, &e.desc); serr != nil {
			return nil, fmt.Errorf("scan kind: %w", serr)
		}
		kinds = append(kinds, e)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate kinds: %w", rerr)
	}

	if embedder != nil {
		return lintNearDuplicateKindsEmbedding(ctx, kinds, embedder, nearDupThreshold)
	}
	return lintNearDuplicateKindsLexical(kinds)
}

// lintNearDuplicateKindsEmbedding is the embedding path: embeds each kind's
// "displayName — description" text (matching the dedup package's convention)
// and reports a pair when EITHER the embedding cosine >= threshold OR the
// word-level lexical similarity >= lintNearDupLexicalThreshold.
//
// Both lanes run because the cosine threshold (reused from the dedup package's
// MintAliasThreshold, ~0.75) is calibrated for that package's *combined*
// lexical+embedding score, not for raw prose cosine. The production embedder
// (snowflake-arctic-embed2) compresses short-prose cosine into a narrow band —
// two kinds with *identical* descriptions score only ~0.70, below 0.75 — so a
// cosine-only check never fires in practice (measured against the live model
// during validation). The lexical lane catches those same-words pairs
// regardless of the embedding model; the cosine lane still catches semantic
// near-dups whose wording differs. Union > either alone. Lint is advisory
// (findings are reviewed, never auto-merged), so a false positive is cheap.
//
// If any embed call fails (e.g. a configured Ollama endpoint is unreachable) it
// degrades to the pure lexical path rather than silently skipping kinds —
// skipping would leave `embedded` short and let `--strict` pass with duplicate
// kinds unreported.
func lintNearDuplicateKindsEmbedding(ctx context.Context, kinds []kindEntry, embedder kernel.Embedder, threshold float64) ([]lintFinding, error) {
	type embeddedKind struct {
		name    string
		lexText string // "displayName description" — the lexical-lane comparison text
		vec     []float32
	}
	embedded := make([]embeddedKind, 0, len(kinds))
	for _, k := range kinds {
		text := k.displayName + " — " + k.desc // em dash matches dedup package convention
		vec, err := embedder.Embed(ctx, text)
		if err != nil {
			// Caller cancellation / deadline must propagate, not be masked as a
			// normal lexical run: an interrupted `maz schema lint` would
			// otherwise keep working and exit with a clean report.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			// A configured-but-unreachable embedder must not silently disable
			// the check: fall back to the lexical path so duplicates still surface.
			return lintNearDuplicateKindsLexical(kinds)
		}
		embedded = append(embedded, embeddedKind{name: k.name, lexText: k.displayName + " " + k.desc, vec: vec})
	}

	var findings []lintFinding
	for i := range embedded {
		for j := i + 1; j < len(embedded); j++ {
			cos := lintCosineSimilarity(embedded[i].vec, embedded[j].vec)
			lex := lintTextSimilarity(embedded[i].lexText, embedded[j].lexText)
			if cos >= threshold || lex >= lintNearDupLexicalThreshold {
				findings = append(findings, lintFinding{
					Check:   "near-duplicate-kinds",
					Message: fmt.Sprintf("kinds %q and %q have near-identical displayName+description (cosine %.2f / lexical %.2f; flagged on cosine>=%.2f or lexical>=%.2f); consider merging", embedded[i].name, embedded[j].name, cos, lex, threshold, lintNearDupLexicalThreshold),
				})
			}
		}
	}
	return findings, nil
}

// lintNearDuplicateKindsLexical is the offline fallback: computes word-level
// LCS similarity over "displayName description" and reports pairs above
// lintNearDupLexicalThreshold. This is the path taken in CI and offline
// environments where no embedder is wired.
func lintNearDuplicateKindsLexical(kinds []kindEntry) ([]lintFinding, error) {
	var findings []lintFinding
	for i := range kinds {
		textI := kinds[i].displayName + " " + kinds[i].desc
		for j := i + 1; j < len(kinds); j++ {
			textJ := kinds[j].displayName + " " + kinds[j].desc
			sim := lintTextSimilarity(textI, textJ)
			if sim >= lintNearDupLexicalThreshold {
				findings = append(findings, lintFinding{
					Check:   "near-duplicate-kinds",
					Message: fmt.Sprintf("kinds %q and %q have near-identical displayName+description (lexical similarity %.2f >= %.2f threshold); consider merging", kinds[i].name, kinds[j].name, sim, lintNearDupLexicalThreshold),
				})
			}
		}
	}
	return findings, nil
}

// kindEntry holds the fields of one entity kind row used by the near-duplicate
// checks. Shared by lintNearDuplicateKinds, lintNearDuplicateKindsEmbedding,
// and lintNearDuplicateKindsLexical.
type kindEntry struct {
	name        string
	displayName string
	desc        string
}

// lintTextSimilarity computes a word-level LCS (longest common subsequence)
// similarity between two free-form text strings. Returns a value in [0.0, 1.0].
//
// Word-level LCS is chosen over character-level Jaccard for free prose because
// it respects word boundaries and shared subsequences ("a road vehicle" in two
// descriptions of different lengths) rather than just set overlap. The score is
// the LCS length divided by the maximum word count of the two inputs — the same
// formula used by dedup.nameSimilarity for the LCS component.
func lintTextSimilarity(a, b string) float64 {
	wa := lintWordTokenize(a)
	wb := lintWordTokenize(b)
	if len(wa) == 0 && len(wb) == 0 {
		return 1.0
	}
	if len(wa) == 0 || len(wb) == 0 {
		return 0.0
	}
	lcs := lintWordLCS(wa, wb)
	maxLen := len(wa)
	if len(wb) > maxLen {
		maxLen = len(wb)
	}
	return float64(lcs) / float64(maxLen)
}

// lintWordTokenize splits s into lowercase words, discarding punctuation.
func lintWordTokenize(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
}

// lintWordLCS returns the length of the longest common subsequence of two word
// slices. O(|a|*|b|) but kinds number in the dozens, so this is fine.
func lintWordLCS(a, b []string) int {
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			switch {
			case a[i-1] == b[j-1]:
				dp[i][j] = dp[i-1][j-1] + 1
			case dp[i-1][j] > dp[i][j-1]:
				dp[i][j] = dp[i-1][j]
			default:
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	return dp[m][n]
}

// lintCosineSimilarity returns the cosine similarity between two float32
// vectors. Returns 0 when either vector is empty or either norm is zero.
// Mirrors dedup.cosineSimilarity (unexported there) so the embedding path
// does not need to import a larger dependency.
func lintCosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// lintTokenize splits text on ":", "_", and whitespace then lowercases each
// token. Used by existing tests that call it directly; kept for compatibility.
func lintTokenize(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ':' || r == '_' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// lintLexicalSimilarity is kept for backward compatibility with existing tests
// that call it directly. New code should use lintTextSimilarity instead.
// It computes Jaccard token-overlap (splitting on colon, underscore, and
// whitespace) and is NOT suitable as a near-dup threshold for free prose; see
// lintNearDupLexicalThreshold for the calibrated replacement.
func lintLexicalSimilarity(a, b string) float64 {
	tokensA := lintTokenize(a)
	tokensB := lintTokenize(b)
	if len(tokensA) == 0 && len(tokensB) == 0 {
		return 1.0
	}
	// Jaccard = |A ∩ B| / |A ∪ B|
	setA := make(map[string]struct{}, len(tokensA))
	for _, t := range tokensA {
		setA[t] = struct{}{}
	}
	var intersection int
	setB := make(map[string]struct{}, len(tokensB))
	for _, t := range tokensB {
		if _, ok := setA[t]; ok {
			intersection++
		}
		setB[t] = struct{}{}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0.0
	}
	return float64(intersection) / float64(union)
}

// lintUnusedPredicates returns predicates with zero live relations referencing
// them in the relations table.
func lintUnusedPredicates(ctx context.Context, db *sql.DB) ([]lintFinding, error) {
	const q = `
		SELECT p.predicate
		  FROM predicates p
		  LEFT JOIN (
		        SELECT predicate, COUNT(*) AS n FROM relations GROUP BY predicate
		       ) r ON r.predicate = p.predicate
		 WHERE COALESCE(r.n, 0) = 0
		 ORDER BY p.predicate ASC`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query unused predicates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var findings []lintFinding
	for rows.Next() {
		var pred string
		if serr := rows.Scan(&pred); serr != nil {
			return nil, fmt.Errorf("scan predicate: %w", serr)
		}
		findings = append(findings, lintFinding{
			Check:   "unused-predicate",
			Name:    pred,
			Message: fmt.Sprintf("predicate %q has zero relations referencing it", pred),
		})
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate unused predicates: %w", rerr)
	}
	return findings, nil
}

// lintMissingSameAs returns non-core active kinds that have an empty same_as
// column. Core kinds are excluded because they are not expected to carry
// cross-vocabulary alignment (they predate the sameAs field).
func lintMissingSameAs(ctx context.Context, db *sql.DB) ([]lintFinding, error) {
	const q = `
		SELECT name
		  FROM entity_kinds
		 WHERE COALESCE(status,'active') = 'active'
		   AND provenance != 'core'
		   AND (same_as IS NULL OR same_as = '[]' OR same_as = '')
		 ORDER BY name ASC`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query kinds missing sameAs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var findings []lintFinding
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			return nil, fmt.Errorf("scan kind name: %w", serr)
		}
		findings = append(findings, lintFinding{
			Check:   "missing-sameAs",
			Name:    name,
			Message: fmt.Sprintf("kind %q has no sameAs IRIs; add schema.org or Wikidata alignment for cross-vocabulary interoperability", name),
		})
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate kinds missing sameAs: %w", rerr)
	}
	return findings, nil
}

// renderLintReportCLI prints a human-readable lint report to w.
func renderLintReportCLI(w io.Writer, report lintReport) error {
	if report.TotalFindings == 0 {
		_, err := fmt.Fprintln(w, "schema lint: no findings")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "Check\tMessage"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(tw, "-----\t-------"); err != nil {
		return err
	}
	for _, f := range report.Findings {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", f.Check, f.Message); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n%d finding(s) total\n", report.TotalFindings)
	return err
}
