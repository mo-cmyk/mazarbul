package cli

// reference.go implements the "maz reference <uri>" command — the CLI surface
// for the external-reference primitive (issue #208):
//
//	maz reference <uri>   mint-or-return an external_ref entity for a URI
//
// It routes through invokeNavTool, so a running daemon serves it over its MCP
// transport and a daemon-less invocation falls back to an in-process wiring —
// the same dispatch every navigation verb uses. Mazarbul does NOT dereference
// the URI; this records a first-class local pointer you can relate to via
// `maz relate <local> has_primary_source <name>`.

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// newReferenceCmd returns "maz reference <uri>".
func newReferenceCmd() *cobra.Command {
	var (
		label string
		scope string
	)
	cmd := &cobra.Command{
		Use:   "reference <uri>",
		Short: "Mint-or-return an external_ref entity for a URI",
		Long: `Record a first-class local pointer to information held in another system
(a GitHub issue, a Drive doc, a Granola transcript, a repo file). Prints the
external_ref's entities/{uid} resource name; pass it to relate to assert that a
local record is grounded in it:

  maz relate entities/<local> has_primary_source entities/<ref>

The call is idempotent: the same URI in the same scope returns the same entity.
Defaults to personal scope (local, not rendered to the shared project git tree);
pass --scope project to share. Mazarbul does NOT fetch the URI — to make the
referenced content searchable, fetch it with your own tools and remember() it.

Examples:
  maz reference https://github.com/org/repo/issues/42
  maz reference https://github.com/org/repo/issues/42 --scope project --label "issue #42"
  maz reference mailto:alice@example.com --output json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{"uri": args[0]}
			putIfSet(in, "label", label)
			putIfSet(in, "scope", scope)

			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "reference", err)
			}
			raw, err := invokeNavTool(cmd, "reference", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "reference", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderReferenceCLI(cmd, raw)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human-readable display name (defaults to the URI)")
	cmd.Flags().StringVar(&scope, "scope", "", "scope to write into: personal, project, team (defaults to personal)")
	return cmd
}

// renderReferenceCLI prints a reference response. NDJSON mode emits the raw
// object on one line; text mode prints the external_ref's resource name.
func renderReferenceCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "reference", fmt.Errorf("parse response: %w", err))
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), v.Name)
	return err
}
