package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// listFlags holds shared flags for the list subcommands.
type listFlags struct {
	limit int
	all   bool
	kind  string
}

// effectiveLimit resolves the SQL LIMIT value. --all or --limit 0 mean "no
// limit", which SQLite expresses as LIMIT -1 — matching how navigate/traverse/
// query already treat a zero limit (no explicit cap) so the convention is
// uniform across the read surface.
func (f *listFlags) effectiveLimit() int {
	if f.all || f.limit <= 0 {
		return -1
	}
	return f.limit
}

// closeStoreQuietly closes store and writes any error to stderr without
// failing the surrounding RunE. Used by deferred cleanup in the list
// subcommands where surfacing a close error would obscure the actual
// failure the function returned.
func closeStoreQuietly(store *sqlite.Store) {
	if err := store.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close sqlite: %v\n", err)
	}
}

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Browse derived state (entities, relations, episodes)",
		Long: `Read-side power tools for humans. Each subcommand queries the SQLite
projection directly so the output is whatever maz rebuild has already
projected (the JSONL log is the source of truth).`,
		// Bare `maz list` shows help; an unknown subcommand errors via NoArgs
		// instead of exiting 0. The RunE makes the command Runnable so cobra
		// validates Args (a group with only subcommands and no RunE returns
		// flag.ErrHelp before NoArgs ever runs).
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newListEntitiesCmd())
	cmd.AddCommand(newListRelationsCmd())
	cmd.AddCommand(newListEpisodesCmd())
	return cmd
}

func newListEntitiesCmd() *cobra.Command {
	flags := &listFlags{limit: 50}
	cmd := &cobra.Command{
		Use:   "entities",
		Short: "List resolved entities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runListEntities(cmd, flags)
		},
	}
	cmd.Flags().IntVar(&flags.limit, "limit", 50, "maximum rows to return (default 50; 0 or --all = no limit)")
	cmd.Flags().BoolVar(&flags.all, "all", false, "return all rows with no limit (same as --limit 0)")
	cmd.Flags().StringVar(&flags.kind, "kind", "", "filter by entity kind (person, organization, place, ...)")
	return cmd
}

func newListRelationsCmd() *cobra.Command {
	flags := &listFlags{limit: 50}
	cmd := &cobra.Command{
		Use:   "relations",
		Short: "List relations by temporal state (open, closed, superseded)",
		Long: `List relation edges from the SQLite projection, newest first. Includes both
currently-live (open) edges and ended (closed/superseded) ones.

TEMPORAL STATE vs TRUST STATUS — relations have two orthogonal axes:

  Temporal state (shown here):
    open        the edge has no valid_until; it is currently true
    closed      the edge reached its valid_until end date
    superseded  a newer edge replaced it (carries a supersededByUid)
    retracted   an explicit relationRetracted tombstone ended it
  Trust status (shown in "maz review proposed"):
    proposed    extracted by a connector, awaiting human verification
    asserted    verified by a human (or authored directly)

This command shows temporal state. Trust status is a separate concern:
use 'maz review proposed' to see the list of proposed (unverified) edges,
and 'maz promote relations/{uid}' to assert one. A relation can be asserted
yet temporally closed, or proposed and still open — these axes are independent.

See also 'maz timeline <entity>' for the full chronological history of one
entity's relations (opens, closes, supersessions in order).

Exit codes:
  0   success
  2   unexpected failure

Examples:
  maz list relations
  maz list relations --all --json
  maz list relations --limit 200`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runListRelations(cmd, flags)
		},
	}
	cmd.Flags().IntVar(&flags.limit, "limit", 50, "maximum rows to return (default 50; 0 or --all = no limit)")
	cmd.Flags().BoolVar(&flags.all, "all", false, "return all rows with no limit (same as --limit 0)")
	return cmd
}

func newListEpisodesCmd() *cobra.Command {
	flags := &listFlags{limit: 50}
	cmd := &cobra.Command{
		Use:   "episodes",
		Short: "List episodes (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runListEpisodes(cmd, flags)
		},
	}
	cmd.Flags().IntVar(&flags.limit, "limit", 50, "maximum rows to return (default 50; 0 or --all = no limit)")
	cmd.Flags().BoolVar(&flags.all, "all", false, "return all rows with no limit (same as --limit 0)")
	return cmd
}

func openListReader(cmd *cobra.Command) (*sqlite.Store, context.Context, error) {
	ctx := cmd.Context()
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "index.db"))
	if err != nil {
		return nil, ctx, fmt.Errorf("open sqlite: %w", err)
	}
	return store, ctx, nil
}

// entityStatusGlyph returns a glyph-prefixed resolution status string.
// The glyph appears in BOTH the plain and coloured paths (symbol-first
// principle) so meaning is never conveyed by colour alone.
func entityStatusGlyph(clr *colorizer.Colorizer, status string) string {
	switch status {
	case "confirmed":
		return clr.Success("✓") + " " + status
	case "split_candidate":
		return clr.Warn("⚠") + " " + status
	default: // tentative, unknown
		return clr.Dim("●") + " " + status
	}
}

// relationStateGlyph returns a glyph-prefixed open/closed label.
func relationStateGlyph(clr *colorizer.Colorizer, closed bool) string {
	if closed {
		return clr.Dim("✗") + " closed"
	}
	return clr.Success("●") + " open"
}

// entityRow is the per-row result from `maz list entities`.
type entityRow struct {
	UID         string  `json:"uid"`
	Kind        string  `json:"kind"`
	DisplayName string  `json:"displayName"`
	Confidence  float64 `json:"confidence"`
	Status      string  `json:"resolutionStatus"`
	// clr is unexported so it is invisible to json.Marshal.
	clr *colorizer.Colorizer
}

// RenderText implements output.TextRenderer for entityRow.
// Pre-pads UID and Kind before colorizing to keep column alignment intact.
//
//nolint:gocritic // hugeParam: value receiver required for TextRenderer interface; pointer would not satisfy it
func (r entityRow) RenderText(w io.Writer) error {
	clr := r.clr
	if clr == nil {
		clr = colorizer.New(false, w)
	}
	uidStr := fmt.Sprintf("%-26s", r.UID)
	kindStr := fmt.Sprintf("%-12s", r.Kind)
	statusStr := entityStatusGlyph(clr, r.Status)
	_, err := fmt.Fprintf(w, "%s  %s  %-30s  conf=%.2f  %s\n",
		clr.Ident(uidStr), clr.Meta(kindStr), r.DisplayName, r.Confidence, statusStr)
	return err
}

func runListEntities(cmd *cobra.Command, flags *listFlags) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return wrapListFatal(cmd, err)
	}
	defer closeStoreQuietly(store)

	enabled := interactive.ShouldStyle(cmd, cmd.OutOrStdout())
	clr := colorizer.New(enabled, cmd.OutOrStdout())

	// Local maintenance view: span every navigable user scope
	// {project,personal,team} but never draft. Draft is speculative staging
	// that must stay invisible to default navigation (reachable only via
	// `maz scope draft list`), so list mirrors the read spine's scope
	// predicate and cannot leak draft-scoped rows.
	scopePred, scopeArgs := kernel.AllScopes().Predicate("")
	q := `SELECT uid, kind, display_name, confidence, resolution_status
	      FROM entities WHERE ` + scopePred
	args := append([]any{}, scopeArgs...)
	if flags.kind != "" {
		q += " AND kind = ?"
		args = append(args, flags.kind)
	}
	q += " ORDER BY create_time DESC LIMIT ?"
	args = append(args, flags.effectiveLimit())

	rows, err := store.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return wrapListFatal(cmd, fmt.Errorf("query entities: %w", err))
	}
	defer func() { _ = rows.Close() }()

	w := output.From(cmd)
	jsonMode := w.Format() == output.FormatJSON
	var buf []entityRow
	for rows.Next() {
		var r entityRow
		if err := rows.Scan(&r.UID, &r.Kind, &r.DisplayName, &r.Confidence, &r.Status); err != nil {
			return wrapListFatal(cmd, fmt.Errorf("scan entity: %w", err))
		}
		r.clr = clr
		if jsonMode {
			buf = append(buf, r)
			continue
		}
		if err := w.EmitStream(r); err != nil {
			return wrapListFatal(cmd, err)
		}
	}
	if err := rows.Err(); err != nil {
		return wrapListFatal(cmd, fmt.Errorf("iterate entities: %w", err))
	}
	if jsonMode {
		if buf == nil {
			buf = []entityRow{}
		}
		if err := w.Emit(buf); err != nil {
			return wrapListFatal(cmd, err)
		}
	}
	return w.Flush()
}

// relationRow is the per-row result from `maz list relations`.
// ValidFrom and ValidUntil are nullable string pointers so that json.Marshal
// renders them as null rather than exposing sql.NullString internals.
// Status is the computed open/closed state: "open" when ValidUntil is nil,
// "closed" otherwise. It mirrors the text-mode relationStateGlyph output so
// both modes carry the same information.
type relationRow struct {
	UID        string  `json:"uid"`
	SubjectUID string  `json:"subjectUid"`
	Predicate  string  `json:"predicate"`
	ObjectUID  string  `json:"objectUid"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
	ValidFrom  *string `json:"validFrom"`
	ValidUntil *string `json:"validUntil"`
	// clr is unexported so it is invisible to json.Marshal.
	clr *colorizer.Colorizer
}

// RenderText implements output.TextRenderer for relationRow.
//
//nolint:gocritic // hugeParam: value receiver required for TextRenderer interface; pointer would not satisfy it
func (r relationRow) RenderText(w io.Writer) error {
	clr := r.clr
	if clr == nil {
		clr = colorizer.New(false, w)
	}
	stateStr := relationStateGlyph(clr, r.ValidUntil != nil)
	uidStr := fmt.Sprintf("%-26s", r.UID)
	_, err := fmt.Fprintf(w, "%s  %s --[%s]--> %s  conf=%.2f  %s\n",
		clr.Ident(uidStr), r.SubjectUID, clr.Meta(r.Predicate), r.ObjectUID, r.Confidence, stateStr)
	return err
}

func runListRelations(cmd *cobra.Command, flags *listFlags) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return wrapListFatal(cmd, err)
	}
	defer closeStoreQuietly(store)

	enabled := interactive.ShouldStyle(cmd, cmd.OutOrStdout())
	clr := colorizer.New(enabled, cmd.OutOrStdout())

	// Span all navigable user scopes but never draft (see runListEntities).
	scopePred, scopeArgs := kernel.AllScopes().Predicate("")
	relArgs := append([]any{}, scopeArgs...)
	relArgs = append(relArgs, flags.effectiveLimit())
	rows, err := store.Reader().QueryContext(ctx,
		`SELECT uid, subject_uid, predicate, object_uid, confidence, valid_from, valid_until
		 FROM relations WHERE `+scopePred+` ORDER BY create_time DESC LIMIT ?`,
		relArgs...,
	)
	if err != nil {
		return wrapListFatal(cmd, fmt.Errorf("query relations: %w", err))
	}
	defer func() { _ = rows.Close() }()

	w := output.From(cmd)
	jsonMode := w.Format() == output.FormatJSON
	var buf []relationRow
	for rows.Next() {
		var r relationRow
		if err := rows.Scan(&r.UID, &r.SubjectUID, &r.Predicate, &r.ObjectUID, &r.Confidence, &r.ValidFrom, &r.ValidUntil); err != nil {
			return wrapListFatal(cmd, fmt.Errorf("scan relation: %w", err))
		}
		if r.ValidUntil != nil {
			r.Status = relationStateClosed
		} else {
			r.Status = relationStateOpen
		}
		r.clr = clr
		if jsonMode {
			buf = append(buf, r)
			continue
		}
		if err := w.EmitStream(r); err != nil {
			return wrapListFatal(cmd, err)
		}
	}
	if err := rows.Err(); err != nil {
		return wrapListFatal(cmd, fmt.Errorf("iterate relations: %w", err))
	}
	if jsonMode {
		if buf == nil {
			buf = []relationRow{}
		}
		if err := w.Emit(buf); err != nil {
			return wrapListFatal(cmd, err)
		}
	}
	return w.Flush()
}

// episodeRow is the per-row result from `maz list episodes`.
type episodeRow struct {
	UID        string `json:"uid"`
	CreateTime string `json:"createTime"`
	Source     string `json:"source"`
	Text       string `json:"text"`
	// clr is unexported so it is invisible to json.Marshal.
	clr *colorizer.Colorizer
}

// RenderText implements output.TextRenderer for episodeRow.
func (r episodeRow) RenderText(w io.Writer) error {
	clr := r.clr
	if clr == nil {
		clr = colorizer.New(false, w)
	}
	uidStr := fmt.Sprintf("%-26s", r.UID)
	_, err := fmt.Fprintf(w, "%s  %s  [%s]  %s\n",
		clr.Ident(uidStr), clr.Dim(r.CreateTime), clr.Meta(r.Source), r.Text)
	return err
}

func runListEpisodes(cmd *cobra.Command, flags *listFlags) error {
	store, ctx, err := openListReader(cmd)
	if err != nil {
		return wrapListFatal(cmd, err)
	}
	defer closeStoreQuietly(store)

	enabled := interactive.ShouldStyle(cmd, cmd.OutOrStdout())
	clr := colorizer.New(enabled, cmd.OutOrStdout())

	// Span all navigable user scopes but never draft (see runListEntities).
	scopePred, scopeArgs := kernel.AllScopes().Predicate("")
	epArgs := append([]any{}, scopeArgs...)
	epArgs = append(epArgs, flags.effectiveLimit())
	rows, err := store.Reader().QueryContext(ctx,
		`SELECT uid, create_time, source, substr(text, 1, 160)
		 FROM episodes WHERE `+scopePred+` ORDER BY create_time DESC LIMIT ?`,
		epArgs...,
	)
	if err != nil {
		return wrapListFatal(cmd, fmt.Errorf("query episodes: %w", err))
	}
	defer func() { _ = rows.Close() }()

	w := output.From(cmd)
	jsonMode := w.Format() == output.FormatJSON
	var buf []episodeRow
	for rows.Next() {
		var r episodeRow
		if err := rows.Scan(&r.UID, &r.CreateTime, &r.Source, &r.Text); err != nil {
			return wrapListFatal(cmd, fmt.Errorf("scan episode: %w", err))
		}
		r.clr = clr
		if jsonMode {
			buf = append(buf, r)
			continue
		}
		if err := w.EmitStream(r); err != nil {
			return wrapListFatal(cmd, err)
		}
	}
	if err := rows.Err(); err != nil {
		return wrapListFatal(cmd, fmt.Errorf("iterate episodes: %w", err))
	}
	if jsonMode {
		if buf == nil {
			buf = []episodeRow{}
		}
		if err := w.Emit(buf); err != nil {
			return wrapListFatal(cmd, err)
		}
	}
	return w.Flush()
}

func wrapListFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "list", err)
}
