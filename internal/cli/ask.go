package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// errChatDisabledMsg is the message emitted when the ask tool is not
// found in the wired tool slice, indicating the chat provider is
// disabled. The wording matches the daemon log line so operators see a
// consistent message regardless of which surface they use.
const errChatDisabledMsg = `chat provider disabled (defaults.chat empty or none); set [defaults] chat = "ollama" in config.toml`

// askFlags holds the parsed flag values for "maz ask".
type askFlags struct {
	limit        int
	at           string
	currentDate  string
	noGraph      bool
	rewriteQuery bool
}

func newAskCmd() *cobra.Command {
	flags := &askFlags{}
	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Answer a question from the event log with grounded citations",
		Long: `Answer a natural-language question by running hybrid recall (vector, FTS,
graph) against the event log and composing a grounded response via the
configured chat provider.

The answer includes inline [N]-style citation markers and a citations
block listing the source episodes. When the chat provider is not
configured, the command exits with a clear error.

Exit codes:
  0   answer produced
  1   expected failure: chat disabled, bad input
  2   unexpected failure: storage or wiring error
  130 interrupted by Ctrl-C

Examples:
  # Interactive prose answer
  maz ask "Where does Merry work?"

  # Scripted JSON output
  maz ask --json "Who is Sam's manager?"

  # Limit recall hits and pin the temporal view
  maz ask --limit 5 --at 2026-01-01T00:00:00Z "What was the team structure?"

  # Skip graph traversal lane
  maz ask --no-graph "Summarize project Zephyr"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAsk(cmd, args[0], flags)
		},
	}
	cmd.Flags().IntVar(&flags.limit, "limit", 10, "maximum recall hits to feed the composer (1..50)")
	cmd.Flags().StringVar(&flags.at, "at", "", "RFC 3339 UTC instant to scope the answer (bi-temporal filter)")
	cmd.Flags().StringVar(&flags.currentDate, "current-date", "", `override the "now" anchor for relative-time questions (RFC 3339 UTC or YYYY-MM-DD); defaults to the daemon's wall clock`)
	cmd.Flags().BoolVar(&flags.noGraph, "no-graph", false, "skip graph traversal lane (vector + FTS only)")
	cmd.Flags().BoolVar(&flags.rewriteQuery, "rewrite-query", false, "expand the question via query-rewrite-v1 before retrieval")
	return cmd
}

// runAsk wires the daemon, finds the ask tool, invokes it, and renders
// the answer in the active output mode.
func runAsk(cmd *cobra.Command, question string, flags *askFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	if err := validateAskFlags(cmd, flags); err != nil {
		return err
	}

	// Build the JSON input for the ask tool.
	in := map[string]interface{}{
		"question": question,
		"limit":    flags.limit,
	}
	if flags.at != "" {
		in["at"] = flags.at
	}
	if flags.currentDate != "" {
		in["currentDate"] = flags.currentDate
	}
	if flags.noGraph {
		in["noGraph"] = true
	}
	if flags.rewriteQuery {
		in["rewriteQuery"] = true
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return wrapAskFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}

	rawAnswer, err := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger,
		"ask", reqBody,
		func(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, req json.RawMessage) (json.RawMessage, error) {
			return invokeNamedTool(ctx, wired, "ask", req)
		},
	)
	if err != nil {
		if isProviderAuthError(err) {
			msg := `ask: provider authentication failed (HTTP 401/403): run "maz secret set" to configure your API key`
			if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
				return werr
			}
			return silentError{code: 1, err: errors.New(msg)}
		}
		return wrapAskFatal(cmd, err)
	}

	if rawAnswer == nil {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errChatDisabledMsg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: fmt.Errorf("%s", errChatDisabledMsg)}
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, err = fmt.Fprintln(w, string(rawAnswer))
		return err
	}

	// CLI mode: render plain text prose + citations.
	return renderAskCLI(cmd, rawAnswer)
}

// validateAskFlags enforces --limit bounds and --at format, returning a
// silentError with code 1 on any violation.
func validateAskFlags(cmd *cobra.Command, flags *askFlags) error {
	if flags.limit < recallLimitMin || flags.limit > recallLimitMax {
		msg := fmt.Sprintf("ask: --limit %d out of range: must be between %d and %d", flags.limit, recallLimitMin, recallLimitMax)
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: errors.New(msg)}
	}
	if flags.at != "" {
		if _, perr := time.Parse(time.RFC3339, flags.at); perr != nil {
			msg := fmt.Sprintf("ask: --at %q: invalid RFC 3339 timestamp (example: 2006-01-02T15:04:05Z)", flags.at)
			if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
				return werr
			}
			return silentError{code: 1, err: errors.New(msg)}
		}
	}
	return nil
}

// isProviderAuthError reports whether err represents a provider-level
// authentication or authorisation failure (HTTP 401 or 403). It inspects the
// error message string because the anthropic and ollama provider packages are
// not imported from the cli layer; the error text is the stable surface.
func isProviderAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 401") ||
		strings.Contains(msg, "status 403") ||
		strings.Contains(msg, "HTTP 401") ||
		strings.Contains(msg, "HTTP 403") ||
		strings.Contains(msg, "authentication_error")
}

// renderAskCLI parses the answer JSON and prints prose + citations in
// plain text suitable for piped and TTY output.
func renderAskCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var answer composer.Answer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return fmt.Errorf("ask: parse answer: %w", err)
	}

	w := cmd.OutOrStdout()

	// Prose block.
	if _, err := fmt.Fprintln(w, answer.Answer); err != nil {
		return err
	}

	if len(answer.Citations) == 0 {
		return nil
	}

	// Citations block.
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Citations:"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range answer.Citations {
		snippet := stripHighlight(strings.ReplaceAll(c.Snippet, "\n", " "))
		if len([]rune(snippet)) > 80 {
			snippet = string([]rune(snippet)[:77]) + "..."
		}
		if _, err := fmt.Fprintf(tw, "[%d]\t%s\t%s\n", c.Index, c.EpisodeUID, snippet); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// wrapAskFatal prints the error to stderr and returns a silentError
// with code 2 so Execute exits with the right code without printing a
// duplicate message.
func wrapAskFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "ask", err)
}
