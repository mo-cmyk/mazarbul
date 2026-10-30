package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// errorEnvelope is the canonical JSON error shape every agent-facing command
// emits to stdout in --output json/ndjson mode. The code mirrors the MCP
// tool-error vocabulary (E_NOT_FOUND, E_VALIDATION, E_PERMISSION, E_INTERNAL)
// so an agent sees the same machine code whether it reached Mazarbul over MCP
// or over the CLI.
type errorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// toolErrorCode extracts the stable MCP error code for err. A typed
// *mcp.ToolError contributes its code directly; a plain-text transport error
// carries the code as a substring; anything else is an internal failure.
func toolErrorCode(err error) string {
	var te *mcp.ToolError
	if errors.As(err, &te) {
		return string(te.Code)
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, string(mcp.CodeNotFound)):
		return string(mcp.CodeNotFound)
	case strings.Contains(msg, string(mcp.CodeValidation)):
		return string(mcp.CodeValidation)
	case strings.Contains(msg, string(mcp.CodePermission)):
		return string(mcp.CodePermission)
	default:
		return string(mcp.CodeInternal)
	}
}

// emitErrorEnvelope writes the human-readable "<prefix>: <err>" line to stderr
// and, in JSON or NDJSON output mode, the structured {error,code} envelope to
// stdout so a piped consumer never has to parse stderr. Shared by respondError
// and respondFatal.
func emitErrorEnvelope(cmd *cobra.Command, prefix string, err error) {
	//nolint:errcheck // best-effort stderr on the error path
	fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", prefix, err)

	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		//nolint:errcheck // best-effort JSON: caller reads the exit code, not this write error
		_ = enc.Encode(errorEnvelope{Error: err.Error(), Code: toolErrorCode(err)})
	}
}

// respondError reports a tool-layer failure that may be expected or unexpected.
// It emits the shared {error,code} envelope and returns a silentError carrying
// exit code 1 for expected failures (E_NOT_FOUND / E_VALIDATION / E_PERMISSION)
// and 2 for everything else. The error is already user-rendered so Execute()
// exits quietly. A nil err returns nil unchanged, so callers can wrap a RunE
// tail as `return respondError(cmd, "get", runGet(cmd, args))`.
func respondError(cmd *cobra.Command, prefix string, err error) error {
	if err == nil {
		return nil
	}
	emitErrorEnvelope(cmd, prefix, err)
	if isExpectedNavError(err) {
		return silentError{code: 1, err: err}
	}
	return silentError{code: 2, err: err}
}

// respondFatal reports an unexpected command failure. It emits the same shared
// {error,code} envelope as respondError but always exits 2 — use it on the
// "fatal" path where the daemon call itself broke, as distinct from a validated
// or tool-classified failure. A nil err returns nil unchanged.
func respondFatal(cmd *cobra.Command, prefix string, err error) error {
	if err == nil {
		return nil
	}
	emitErrorEnvelope(cmd, prefix, err)
	return silentError{code: 2, err: err}
}
