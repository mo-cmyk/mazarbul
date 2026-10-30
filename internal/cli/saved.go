package cli

// saved.go — `maz saved` command group.
//
// Saved recalls let users bookmark a query + parameters under a name and replay
// them with `maz saved recall <name>`. The bookmarks live in the [saved_recalls]
// section of config.toml so they follow the data directory and survive daemon
// restarts.
//
// Sub-commands:
//   maz saved add    <name> --query <q> [--limit N] [--min-score F] [--scope s…]
//   maz saved list
//   maz saved show   <name>
//   maz saved remove <name>
//   maz saved recall <name> [recall-flags…]

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
)

func newSavedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "saved",
		Short: "Manage saved recall queries",
		Long: `Create, list, and replay named recall queries.

A saved recall stores a query string with its parameters (limit, min-score,
scopes) in the data-directory's config.toml under [saved_recalls]. Replaying a
saved recall runs the same query through the live recall pipeline, so results
evolve as new episodes are written.

Examples:
  # Save a query
  maz saved add standup --query "daily standup Berlin team" --limit 5

  # List all saved queries
  maz saved list

  # Inspect one saved query
  maz saved show standup

  # Replay a saved query (identical to maz recall with the stored params)
  maz saved recall standup

  # Remove a saved query
  maz saved remove standup`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newSavedAddCmd())
	cmd.AddCommand(newSavedListCmd())
	cmd.AddCommand(newSavedShowCmd())
	cmd.AddCommand(newSavedRemoveCmd())
	cmd.AddCommand(newSavedRecallCmd())
	return cmd
}

// savedDataDir resolves the --data-dir flag or falls back to the default.
func savedDataDir(cmd *cobra.Command) string {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	return dataDir
}

// loadSavedRecalls loads the SavedRecallsTable from the resolved config, or
// returns an empty map on first-run (no config.toml yet).
func loadSavedRecalls(dataDir string) (config.SavedRecallsTable, error) {
	cfg, err := config.LoadUserConfig(dataDir)
	if err != nil {
		if errors.Is(err, config.ErrUserConfigNotFound) {
			return config.SavedRecallsTable{}, nil
		}
		return nil, fmt.Errorf("saved: load config: %w", err)
	}
	if cfg.SavedRecalls == nil {
		return config.SavedRecallsTable{}, nil
	}
	return cfg.SavedRecalls, nil
}

// writeSavedRecalls persists the SavedRecallsTable back to config.toml,
// preserving all other config keys.
func writeSavedRecalls(dataDir string, saved config.SavedRecallsTable) error {
	cfg, err := config.LoadUserConfig(dataDir)
	if err != nil && !errors.Is(err, config.ErrUserConfigNotFound) {
		return fmt.Errorf("saved: load config: %w", err)
	}
	if errors.Is(err, config.ErrUserConfigNotFound) {
		cfg = config.UserConfig{SchemaVersion: config.CurrentUserConfigSchemaVersion}
	}
	cfg.SavedRecalls = saved
	return config.WriteUserConfig(dataDir, cfg)
}

// savedRecallEntry is the JSON/text rendering shape for a saved recall.
type savedRecallEntry struct {
	Name     string   `json:"name"`
	Query    string   `json:"query"`
	Limit    int      `json:"limit,omitempty"`
	MinScore float64  `json:"minScore,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

// newSavedAddCmd wires `maz saved add <name> --query <q> [params]`.
func newSavedAddCmd() *cobra.Command {
	var (
		query    string
		limit    int
		minScore float64
		scopes   []string
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "add <name> [query]",
		Short: "Save a recall query under a name",
		Long: `Add a named recall bookmark to config.toml.

Give the query as a positional argument (like "maz recall <query>") or with the
--query flag. The stored parameters (query, --limit, --min-score, --scope) are
replayed verbatim by "maz saved recall <name>".

Examples:
  maz saved add standup "daily standup" --limit 5
  maz saved add standup --query "daily standup" --limit 5
  maz saved add team-news "team updates" --scope team --min-score 0.1`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if len(args) == 2 {
				if query != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), "saved add: pass the query either positionally or with --query, not both") //nolint:errcheck // best-effort stderr
					return silentError{code: 1, err: errors.New("query given both positionally and via --query")}
				}
				query = args[1]
			}
			if query == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), `saved add: a query is required — pass it positionally (maz saved add NAME "query") or with --query`) //nolint:errcheck // best-effort stderr
				return silentError{code: 1, err: errors.New("query is required")}
			}
			dataDir := savedDataDir(cmd)
			saved, err := loadSavedRecalls(dataDir)
			if err != nil {
				return silentError{code: 2, err: err}
			}
			if _, exists := saved[name]; exists && !force {
				fmt.Fprintf(cmd.ErrOrStderr(), "saved add: %q already exists; pass --force to overwrite it\n", name) //nolint:errcheck // best-effort stderr
				return silentError{code: 1, err: fmt.Errorf("saved recall %q already exists", name)}
			}
			saved[name] = config.SavedRecall{
				Query:    query,
				Limit:    limit,
				MinScore: minScore,
				Scopes:   scopes,
			}
			if err := writeSavedRecalls(dataDir, saved); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "saved add: write: %v\n", err) //nolint:errcheck // best-effort stderr
				return silentError{code: 2, err: err}
			}
			wr := output.From(cmd)
			if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
					"ok": true, "name": name, "query": query,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved %q (query: %q)\n", name, query) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "recall query string (or pass it as the positional [query] argument)")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum hits to return (0 = recall default)")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "fused-score floor (0 = recall default)")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "scope to read from (repeatable): project, personal, team")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing saved recall of the same name")
	return cmd
}

// newSavedListCmd wires `maz saved list`.
func newSavedListCmd() *cobra.Command { //nolint:gocognit,gocyclo // cobra command constructor; flag/filter branches are inherently enumerated here
	return &cobra.Command{
		Use:   "list",
		Short: "List all saved recall queries",
		Long: `List the names and queries stored under [saved_recalls] in config.toml.

In JSON mode the output is an array of {name, query, limit, minScore, scopes}.

Examples:
  maz saved list
  maz saved list --output json | jq '.[].name'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir := savedDataDir(cmd)
			saved, err := loadSavedRecalls(dataDir)
			if err != nil {
				return silentError{code: 2, err: err}
			}
			names := make([]string, 0, len(saved))
			for k := range saved {
				names = append(names, k)
			}
			sort.Strings(names)

			entries := make([]savedRecallEntry, 0, len(names))
			for _, n := range names {
				r := saved[n]
				entries = append(entries, savedRecallEntry{
					Name:     n,
					Query:    r.Query,
					Limit:    r.Limit,
					MinScore: r.MinScore,
					Scopes:   r.Scopes,
				})
			}

			wr := output.From(cmd)
			switch wr.Format() {
			case output.FormatNDJSON:
				// True NDJSON: one JSON object per line.
				if len(entries) == 0 {
					return wr.Flush()
				}
				for _, e := range entries {
					if err := wr.EmitStream(e); err != nil {
						return err
					}
				}
				return wr.Flush()
			case output.FormatJSON:
				if entries == nil {
					entries = []savedRecallEntry{}
				}
				return wr.Emit(entries)
			case output.FormatCSV, output.FormatKV:
				// Structured tabular: emit each entry as a record. For CSV,
				// this always produces a header row even when the list is empty
				// so parsers do not need to special-case an empty result.
				if len(entries) == 0 {
					// Emit a sentinel empty record so csvWriter writes the header,
					// then flush to ensure the buffered CSV bytes reach stdout.
					if err := wr.Emit(savedRecallEntry{}); err != nil {
						return err
					}
					return wr.Flush()
				}
				for _, e := range entries {
					if err := wr.Emit(e); err != nil {
						return err
					}
				}
				return wr.Flush()
			default:
				if len(entries) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no saved recalls") //nolint:errcheck // best-effort stdout
					return nil
				}
				for _, e := range entries {
					fmt.Fprintf(cmd.OutOrStdout(), "%-20s  %q\n", e.Name, e.Query) //nolint:errcheck // best-effort stdout
				}
				return nil
			}
		},
	}
}

// newSavedShowCmd wires `maz saved show <name>`.
func newSavedShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show the stored parameters for a saved recall",
		Long: `Print all stored parameters for a saved recall bookmark.

Examples:
  maz saved show standup
  maz saved show standup --output json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dataDir := savedDataDir(cmd)
			saved, err := loadSavedRecalls(dataDir)
			if err != nil {
				return silentError{code: 2, err: err}
			}
			r, ok := saved[name]
			if !ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "saved: %q not found\n", name) //nolint:errcheck // best-effort stderr
				return silentError{code: 1, err: fmt.Errorf("saved recall %q not found", name)}
			}
			entry := savedRecallEntry{
				Name:     name,
				Query:    r.Query,
				Limit:    r.Limit,
				MinScore: r.MinScore,
				Scopes:   r.Scopes,
			}
			wr := output.From(cmd)
			switch wr.Format() {
			case output.FormatJSON, output.FormatNDJSON, output.FormatCSV, output.FormatKV:
				// All structured formats: emit the single entry as a record.
				// csv produces header+one-row; kv produces a key:value block.
				if err := wr.Emit(entry); err != nil {
					return err
				}
				return wr.Flush()
			default:
				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "name:      %s\n", entry.Name)  //nolint:errcheck // best-effort stdout
				fmt.Fprintf(w, "query:     %s\n", entry.Query) //nolint:errcheck // best-effort stdout
				if entry.Limit > 0 {
					fmt.Fprintf(w, "limit:     %d\n", entry.Limit) //nolint:errcheck // best-effort stdout
				}
				if entry.MinScore > 0 {
					fmt.Fprintf(w, "min-score: %.4f\n", entry.MinScore) //nolint:errcheck // best-effort stdout
				}
				if len(entry.Scopes) > 0 {
					fmt.Fprintf(w, "scopes:    %v\n", entry.Scopes) //nolint:errcheck // best-effort stdout
				}
				return nil
			}
		},
	}
}

// newSavedRemoveCmd wires `maz saved remove <name>`.
func newSavedRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a saved recall by name",
		Long: `Delete a named recall bookmark from config.toml.

Examples:
  maz saved remove standup`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dataDir := savedDataDir(cmd)
			saved, err := loadSavedRecalls(dataDir)
			if err != nil {
				return silentError{code: 2, err: err}
			}
			if _, ok := saved[name]; !ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "saved: %q not found\n", name) //nolint:errcheck // best-effort stderr
				return silentError{code: 1, err: fmt.Errorf("saved recall %q not found", name)}
			}
			delete(saved, name)
			if err := writeSavedRecalls(dataDir, saved); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "saved remove: write: %v\n", err) //nolint:errcheck // best-effort stderr
				return silentError{code: 2, err: err}
			}
			wr := output.From(cmd)
			if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
					"ok": true, "removed": name,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %q\n", name) //nolint:errcheck // best-effort stdout
			return nil
		},
	}
}

// newSavedRecallCmd wires `maz saved recall <name>`.
//
// It resolves the stored SavedRecall entry and invokes runRecall via the shared
// recall tool path, exactly as if the user had typed `maz recall <query>`
// with the stored parameters. Any flags specified on the command line override
// the stored defaults.
func newSavedRecallCmd() *cobra.Command {
	flags := &recallFlags{}
	cmd := &cobra.Command{
		Use:     "recall <name>",
		Aliases: []string{"run", "replay"},
		Short:   "Run a saved recall query (aliases: run, replay)",
		Long: `Replay a saved recall bookmark through the live recall pipeline.

The stored query and parameters are loaded from config.toml and forwarded to the
recall engine. Any flags specified on this invocation override the stored values
(e.g. --limit 20 overrides a stored limit of 5).

Exit codes follow maz recall: 0 success, 1 bad input/disabled, 2 unexpected.

Examples:
  maz saved recall standup
  maz saved recall standup --output json
  maz saved recall standup --limit 20 --output csv`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dataDir := savedDataDir(cmd)
			saved, err := loadSavedRecalls(dataDir)
			if err != nil {
				return silentError{code: 2, err: err}
			}
			r, ok := saved[name]
			if !ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "saved: %q not found\n", name) //nolint:errcheck // best-effort stderr
				return silentError{code: 1, err: fmt.Errorf("saved recall %q not found", name)}
			}

			// Overlay stored params with any flag overrides. The cobra Changed()
			// method reports whether the user set a flag explicitly on this
			// invocation; only those overrides win.
			if !cmd.Flags().Changed("limit") && r.Limit > 0 {
				flags.limit = r.Limit
			}
			if !cmd.Flags().Changed("min-score") && r.MinScore > 0 {
				flags.minScore = r.MinScore
			}
			if !cmd.Flags().Changed("scope") && len(r.Scopes) > 0 {
				flags.scopes = r.Scopes
			}

			return runRecall(cmd, r.Query, flags)
		},
	}
	// Expose the same flags as `maz recall` so the caller can override stored
	// params without having to edit config.toml.
	cmd.Flags().IntVar(&flags.limit, "limit", 10, "override stored limit (0 = use stored value)")
	cmd.Flags().Float64Var(&flags.minScore, "min-score", 0, "override stored min-score floor (0 = use stored value)")
	cmd.Flags().StringArrayVar(&flags.scopes, "scope", nil, "override stored scopes (repeatable: project, personal, team)")
	cmd.Flags().StringVar(&flags.at, "at", "", "RFC 3339 UTC instant for the bi-temporal valid-time filter")
	cmd.Flags().BoolVar(&flags.noGraph, "no-graph", false, "skip graph traversal lane")
	cmd.Flags().BoolVar(&flags.explain, "explain", false, "show per-lane score breakdown for each hit")
	cmd.Flags().BoolVar(&flags.explain, "why", false, "alias for --explain")
	return cmd
}
