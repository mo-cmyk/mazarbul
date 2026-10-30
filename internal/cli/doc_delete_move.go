package cli

// doc_delete_move.go: `maz doc rm` and `maz doc mv` CLI subcommands.
// Both route through the daemon-or-in-process MCP tool dispatch (invokeNavTool)
// and match the conventions of the rest of the doc sub-commands:
//   - --output json for machine-readable output
//   - wrapNavFatal / emitNavResult for error/output handling
//   - rm requires --yes to confirm the destructive operation

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// ---- doc rm ----

func newDocRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <path>",
		Short: "Delete a vault document",
		Long: `Soft-delete a vault document by its vault-relative path. Appends a tombstone
event to the event log (full history preserved), strips the document from
FTS5 and vector recall, forgets its outgoing wikilink edges, and removes
its vault file.

Backlinks from other documents pointing AT this document are left intact as
dead links; run 'maz doc lint' to surface them.

Requires --yes to confirm the destructive operation.

Examples:
  maz doc rm notes/old-idea.md --yes
  maz doc rm notes/old-idea     --yes    # .md appended automatically`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				//nolint:errcheck // best-effort advisory write to stderr
				fmt.Fprintln(cmd.ErrOrStderr(),
					"doc rm: destructive operation requires --yes to confirm")
				return silentError{code: 1, err: errors.New("confirmation required")}
			}
			reqBody, err := json.Marshal(map[string]any{"path": args[0]})
			if err != nil {
				return wrapNavFatal(cmd, "doc rm", err)
			}
			raw, err := invokeNavTool(cmd, "deleteDocument", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "doc rm", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDocRm(cmd, raw)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the destructive deletion")
	return cmd
}

func renderDocRm(cmd *cobra.Command, raw json.RawMessage) error {
	var res struct {
		Path        string `json:"path"`
		DisplayName string `json:"displayName"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("doc rm: parse response: %w", err)
	}
	name := res.DisplayName
	if name == "" {
		name = res.Path
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s deleted (%s)\n", res.Path, name)
	return err
}

// ---- doc mv ----

func newDocMvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mv <old-path> <new-path>",
		Short: "Rename (move) a vault document",
		Long: `Move a vault document from <old-path> to <new-path>, preserving its
document UID so all wikilink and mention edges survive the rename.

A new documentRevision event is appended at the new path with the same body
and a chained previousRevisionUid. The vault file at the old path is removed
and a new file appears at the new path.

Rejects if <new-path> is already occupied by a different document.

Examples:
  maz doc mv notes/scratch.md notes/archive/scratch.md
  maz doc mv notes/scratch    notes/archive/scratch     # .md added automatically`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			reqBody, err := json.Marshal(map[string]any{
				"path":    args[0],
				"newPath": args[1],
			})
			if err != nil {
				return wrapNavFatal(cmd, "doc mv", err)
			}
			raw, err := invokeNavTool(cmd, "moveDocument", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "doc mv", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderDocMv(cmd, raw)
		},
	}
}

func renderDocMv(cmd *cobra.Command, raw json.RawMessage) error {
	var res struct {
		OldPath     string `json:"oldPath"`
		NewPath     string `json:"newPath"`
		RevisionUID string `json:"revisionUid"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("doc mv: parse response: %w", err)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"%s → %s (revision %s)\n", res.OldPath, res.NewPath, res.RevisionUID)
	return err
}
