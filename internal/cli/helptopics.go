package cli

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/tui/style/mdrender"
)

//go:embed topics/*.md
var topicsFS embed.FS

// knownTopics maps topic names (as used on the CLI) to their embedded
// markdown filenames (basename inside topics/, without .md extension).
var knownTopics = map[string]string{ //nolint:gochecknoglobals
	"concepts":           "concepts",
	"exit-codes":         "exit-codes",
	"four-layer-storage": "four-layer-storage",
	"mellon":             "mellon",
	"moria":              "moria",
	"plugins":            "plugins",
	"recall":             "recall",
	"scopes":             "scopes",
	"sync":               "sync",
	"watchword":          "watchword",
}

func newHelpTopicsCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "help-topics [topic]",
		Short:     "Read embedded help topics (concept guides)",
		ValidArgs: topicNames(),
		Long: `List or read the embedded Mazarbul concept guides.

Without a topic name, lists all available topics. With a topic name,
renders the guide in the terminal (markdown on a TTY; plain text when
NO_COLOR is set or stdout is piped).

Exit codes:
  0   success
  1   topic not found

Examples:
  maz help-topics
  maz help-topics exit-codes
  maz help-topics four-layer-storage
  maz help-topics recall | less`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runHelpTopicsList(cmd)
			}
			return runHelpTopicShow(cmd, args[0])
		},
	}
}

// runHelpTopicsList prints all available topic names.
func runHelpTopicsList(cmd *cobra.Command) error {
	names := topicNames()
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		type item struct {
			Topic string `json:"topic"`
		}
		items := make([]item, len(names))
		for i, n := range names {
			items[i] = item{Topic: n}
		}
		return json.NewEncoder(w).Encode(map[string]any{"items": items})
	}
	//nolint:errcheck // best-effort stdout write
	fmt.Fprintln(w, "Available topics:")
	for _, n := range names {
		//nolint:errcheck // best-effort stdout write
		fmt.Fprintf(w, "  maz help-topics %s\n", n)
	}
	return nil
}

// runHelpTopicShow renders the named topic to the command's stdout.
func runHelpTopicShow(cmd *cobra.Command, topic string) error {
	filename, ok := knownTopics[topic]
	if !ok {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "help-topics: unknown topic %q\n", topic)
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "Run 'maz help-topics' to list available topics.\n")
		return silentError{code: 1, err: fmt.Errorf("unknown topic: %s", topic)}
	}

	data, err := fs.ReadFile(topicsFS, "topics/"+filename+".md")
	if err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "help-topics: read %s: %v\n", filename, err)
		return silentError{code: 2, err: err}
	}

	w := cmd.OutOrStdout()
	src := string(data)
	// Glamour-render on a styled TTY; pipes, NO_COLOR, and --color=never get
	// the raw markdown so `maz help-topics recall | less` and agents stay clean.
	// ShouldStyle is the shared gate (honours --color/CLICOLOR), so this matches
	// every other coloured CLI surface rather than re-deciding TTY here.
	if interactive.ShouldStyle(cmd, w) {
		src = mdrender.RenderStyled(src)
	}
	_, werr := fmt.Fprint(w, src)
	return werr
}

// topicNames returns the sorted list of known topic keys.
func topicNames() []string {
	names := make([]string, 0, len(knownTopics))
	for k := range knownTopics {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
