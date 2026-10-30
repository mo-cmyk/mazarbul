package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// errMemoryDisabledMsg is the message emitted when the remember tool is
// not found in the wired tool slice, indicating the memory layer is not
// available. Matches the daemon log wording for consistency.
const errMemoryDisabledMsg = `remember tool not available; run "maz init" to initialise the memory layer`

// rememberFlags holds the parsed flag values for "maz remember".
type rememberFlags struct {
	source           string
	coords           string
	coordsProvenance string
	confidence       float64
	scope            string
}

func newRememberCmd() *cobra.Command {
	flags := &rememberFlags{}
	cmd := &cobra.Command{
		Use:   "remember <text>",
		Short: "Append a natural-language episode to the memory layer",
		Long: `Append a natural-language episode to the event log. The episode text is
stored verbatim as a durable JSONL line; the four derived stores (SQLite,
vector index, FTS5, LadybugDB) are reconciled asynchronously.

On success the AIP resource name of the new episode is printed (CLI mode)
or returned as JSON (--json mode).

Exit codes:
  0   episode created
  1   expected failure: bad input, memory layer disabled
  2   unexpected failure: storage or wiring error
  130 interrupted by Ctrl-C

Examples:
  # Store a plain episode
  maz remember "Frodo works at Erebor Inc., met him in Berlin last week"

  # Supersede with a new fact
  maz remember "Frodo now works at Rivendell"

  # Attach coordinates to help the spatial filter
  maz remember "Frodo Baggins works at Erebor Inc." \
    --coords "52.52,13.40" --coords-provenance user_provided

  # Scripted JSON output
  maz remember "team standup at 09:00" --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("remember: text argument required\n\nUsage: maz remember <text>\nExample: maz remember \"Merry works at Mithril\"")
			}
			if len(args) > 1 {
				return fmt.Errorf("remember: accepts 1 argument, got %d (quote multi-word text)", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemember(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.source, "source", "cli",
		`episode source identifier (free-form, defaults to "cli")`)
	cmd.Flags().StringVar(&flags.coords, "coords", "",
		`geographic coordinates in "lat,lng" decimal-degree format, e.g. "52.52,13.40"`)
	cmd.Flags().StringVar(&flags.coordsProvenance, "coords-provenance", "user_provided",
		`origin of the coordinates (defaults to "user_provided" when --coords is set)`)
	cmd.Flags().Float64Var(&flags.confidence, "confidence", 1.0,
		"writer confidence in the episode, range 0..1 (default 1.0)")
	cmd.Flags().StringVar(&flags.scope, "scope", "",
		"sync/encryption `scope`: personal (default), project, team, or draft (draft = private staging, invisible to recall until promoted via 'maz scope draft promote')")
	return cmd
}

// runRemember wires the daemon, finds the remember tool, invokes it, and
// renders the new episode name in the active output mode.
func runRemember(cmd *cobra.Command, text string, flags *rememberFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	reqBody, err := buildRememberRequest(cmd, text, flags)
	if err != nil {
		return err
	}

	// If a daemon is running it holds the exclusive on-disk locks; in
	// that case we route this call through the daemon's MCP transport
	// instead of trying to OpenWiring (which would fail cryptically
	// deep in the storage stack with "ladybug status 1").
	rawResult, err := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger,
		"remember", reqBody, invokeRemember)
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, err = fmt.Fprintln(w, string(rawResult))
		return err
	}
	return renderRememberCLI(cmd, rawResult, flags.scope)
}

// buildRememberRequest validates flags and marshals the tool input JSON.
// Returns a silentError with code 1 on bad input.
func buildRememberRequest(cmd *cobra.Command, text string, flags *rememberFlags) (json.RawMessage, error) {
	coordsSet := flags.coords != ""
	provenanceChanged := cmd.Flags().Changed("coords-provenance")
	if !coordsSet && provenanceChanged {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
			"remember: --coords-provenance requires --coords"); werr != nil {
			return nil, werr
		}
		return nil, silentError{code: 1, err: errors.New("--coords-provenance requires --coords")}
	}

	in := map[string]interface{}{
		"text":   text,
		"source": flags.source,
	}
	if flags.scope != "" {
		in["scope"] = flags.scope
	}
	if cmd.Flags().Changed("confidence") {
		in["confidence"] = flags.confidence
	}

	if coordsSet {
		lat, lng, err := parseCoords(flags.coords)
		if err != nil {
			if _, werr := fmt.Fprintf(cmd.ErrOrStderr(),
				"remember: --coords: %v\n", err); werr != nil {
				return nil, werr
			}
			return nil, silentError{code: 1, err: fmt.Errorf("parse coords: %w", err)}
		}
		in["coordinates"] = map[string]interface{}{
			"latitude":  lat,
			"longitude": lng,
		}
		in["coordinatesProvenance"] = flags.coordsProvenance
	}

	body, err := json.Marshal(in)
	if err != nil {
		return nil, wrapRememberFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}
	return body, nil
}

// invokeRemember locates the "remember" tool in wired.Tools and calls it.
// Returns errMemoryDisabledMsg as a silentError(1) when the tool is absent.
func invokeRemember(ctx context.Context, cmd *cobra.Command, wired *daemon.Wired, reqBody json.RawMessage) (json.RawMessage, error) {
	for _, t := range wired.Tools {
		if t.Name() != "remember" {
			continue
		}
		raw, err := t.Invoke(ctx, reqBody)
		if err != nil {
			return nil, wrapRememberFatal(cmd, fmt.Errorf("invoke: %w", err))
		}
		return raw, nil
	}
	if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errMemoryDisabledMsg); werr != nil {
		return nil, werr
	}
	return nil, silentError{code: 1, err: fmt.Errorf("%s", errMemoryDisabledMsg)}
}

// renderRememberCLI parses the remember response and prints the new episode
// resource name to stdout. For a non-default scope it appends a one-line
// confirmation so the user can see the write landed where they intended —
// especially `draft`, where the staged note is intentionally invisible to
// recall and would otherwise return only a bare name with no sign it staged.
// The personal default keeps its bare-name output so existing text-mode
// scripting is unaffected.
func renderRememberCLI(cmd *cobra.Command, raw json.RawMessage, scope string) error {
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("remember: parse response: %w", err)
	}
	switch kernel.Scope(scope) {
	case kernel.ScopeDraft:
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"%s\nStaged in draft (private — hidden from recall/list until `maz scope draft promote`; drop with `maz scope draft discard`).\n",
			out.Name)
		return err
	case kernel.ScopeProject, kernel.ScopeTeam:
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\nSaved in %s scope.\n", out.Name, scope)
		return err
	default:
		_, err := fmt.Fprintln(cmd.OutOrStdout(), out.Name)
		return err
	}
}

// parseCoords parses a "lat,lng" string into float64 latitude and longitude.
// Returns an error if the string is malformed or the values are out of range.
func parseCoords(s string) (lat, lng float64, err error) {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected format \"lat,lng\", got %q", s)
	}
	lat, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("latitude %q: %w", parts[0], err)
	}
	lng, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("longitude %q: %w", parts[1], err)
	}
	if lat < -90 || lat > 90 {
		return 0, 0, fmt.Errorf("latitude %g out of range [-90, 90]", lat)
	}
	if lng < -180 || lng > 180 {
		return 0, 0, fmt.Errorf("longitude %g out of range [-180, 180]", lng)
	}
	return lat, lng, nil
}

// wrapRememberFatal prints the error to stderr and returns a silentError
// with code 2.
func wrapRememberFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "remember", err)
}
