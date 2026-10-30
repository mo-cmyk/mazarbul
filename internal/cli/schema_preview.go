package cli

// schema_preview.go implements "maz schema preview" — a read-only, side-effect-free
// surface that shows what defineEntityKind or defineSchema would do WITHOUT
// writing any event. It routes through invokeNavTool (daemon when running, in-
// process otherwise) and sets preview=true on the tool input so no events are
// appended. This is useful for humans and agents to plan changes before committing.
//
//   maz schema preview kind <name>
//   maz schema preview schema <schemaKind> <version> [--definition <file>]

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// nearestKindEntry is one existing kind ranked by lexical similarity to the
// previewed candidate, mirroring dedup.NearestKind on the wire.
type nearestKindEntry struct {
	Kind       string  `json:"kind"`
	Similarity float64 `json:"similarity"`
}

// kindPreviewPlan is the plan block returned for a defineEntityKind preview.
type kindPreviewPlan struct {
	Action         string             `json:"action"`
	CollapsesInto  string             `json:"collapsesInto,omitempty"`
	SupersedesFrom string             `json:"supersedesFrom,omitempty"`
	OrphanCount    int                `json:"orphanCount,omitempty"`
	CloseMatch     string             `json:"closeMatch,omitempty"`
	NearestKinds   []nearestKindEntry `json:"nearestKinds,omitempty"`
	ChecksSkipped  []string           `json:"checksSkipped,omitempty"`
}

// kindPreviewResponse is the defineEntityKind preview response shape the CLI
// parses to render the human-readable table.
type kindPreviewResponse struct {
	Name     string             `json:"name"`
	Status   string             `json:"status"`
	Plan     *kindPreviewPlan   `json:"plan,omitempty"`
	Warnings []warncode.Warning `json:"warnings,omitempty"`
}

// previewKindFlags holds the optional flags for "maz schema preview kind".
type previewKindFlags struct {
	displayName    string
	description    string
	propertySchema string // path or "-" for stdin
}

// previewSchemaFlags holds the optional flags for "maz schema preview schema".
type previewSchemaFlags struct {
	definition string // path or "-" for stdin (required)
}

// newSchemaPreviewCmd returns the "maz schema preview" parent command.
func newSchemaPreviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Preview what defineEntityKind or defineSchema would do without writing",
		Long: `Compute and display the plan for a defineEntityKind or defineSchema call,
WITHOUT appending any event to the event log. Use this to inspect impact
before committing a change.

Subcommands:
  kind    Preview what registering an entity kind would do
  schema  Preview what registering a JSON Schema version would do

Exit codes:
  0   preview computed successfully
  1   expected failure (bad input, validation error)
  2   unexpected failure (storage or wiring error)

Examples:
  maz schema preview kind vehicle --description "A motorised transport."
  maz schema preview schema entityKind:vehicle v0.2.0 --definition ./vehicle_v2.json`,
	}
	cmd.AddCommand(newSchemaPreviewKindCmd())
	cmd.AddCommand(newSchemaPreviewSchemaCmd())
	return cmd
}

// newSchemaPreviewKindCmd returns "maz schema preview kind <name>".
func newSchemaPreviewKindCmd() *cobra.Command {
	flags := &previewKindFlags{}
	cmd := &cobra.Command{
		Use:   "kind <name>",
		Short: "Preview what defineEntityKind would do for the given kind name",
		Long: `Calls defineEntityKind with preview=true and displays the plan
(mint / collapse / supersede) and any advisory warnings, without appending
any event to the event log.

The name is the bare local part (e.g. "vehicle") — the tool will apply the
user: namespace prefix automatically, exactly as it does for a live call.

--display-name is optional for a preview: when omitted it defaults to a
titleized form of the name (vehicle -> "Vehicle"), so the examples below
work as written. Pass --display-name to preview a specific label.

Exit codes:
  0   plan computed
  1   expected failure (validation error, core-kind collision)
  2   unexpected failure (wiring or storage error)

Examples:
  maz schema preview kind vehicle --description "A motorised transport."
  maz schema preview kind deal --display-name "Deal" --description "A sales deal." --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaPreviewKind(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.displayName, "display-name", "", "Human-readable label for the kind (defaults to a titleized name when omitted)")
	cmd.Flags().StringVar(&flags.description, "description", "", "One-sentence description of the kind")
	cmd.Flags().StringVar(&flags.propertySchema, "property-schema", "",
		`Path to a JSON Schema file for typed attributes, or "-" for stdin`)
	return cmd
}

// newSchemaPreviewSchemaCmd returns "maz schema preview schema <schemaKind> <version>".
func newSchemaPreviewSchemaCmd() *cobra.Command {
	flags := &previewSchemaFlags{}
	cmd := &cobra.Command{
		Use:   "schema <schemaKind> <version>",
		Short: "Preview what defineSchema would do for the given schemaKind and version",
		Long: `Calls defineSchema with preview=true and displays the plan
(register / supersede) and any advisory warnings, without appending any
event to the event log.

The definition is read from --definition (a file path or "-" for stdin).

Exit codes:
  0   plan computed
  1   expected failure (validation error, core-schema collision)
  2   unexpected failure (wiring or storage error)

Examples:
  maz schema preview schema entityKind:vehicle v0.2.0 --definition ./v2.json
  cat v2.json | maz schema preview schema entityKind:vehicle v0.2.0 --definition -`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaPreviewSchema(cmd, args[0], args[1], flags)
		},
	}
	cmd.Flags().StringVar(&flags.definition, "definition", "",
		`Path to a JSON Schema file (the definition object), or "-" for stdin. Required.`)
	return cmd
}

// defaultKindDisplayName derives a human-readable label from a bare kind name
// when --display-name is omitted, so the documented `schema preview kind <name>`
// example (which carries no --display-name) computes a plan instead of failing
// validation. "vehicle" -> "Vehicle"; "sales_deal" -> "Sales Deal".
func defaultKindDisplayName(name string) string {
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-' || r == ' '
	})
	for i, f := range fields {
		rs := []rune(f)
		rs[0] = unicode.ToUpper(rs[0])
		fields[i] = string(rs)
	}
	if len(fields) == 0 {
		return name
	}
	return strings.Join(fields, " ")
}

// runSchemaPreviewKind builds the defineEntityKind request with preview=true,
// invokes it through the daemon (or in-process), and renders the plan.
func runSchemaPreviewKind(cmd *cobra.Command, name string, flags *previewKindFlags) error {
	in := map[string]any{
		"name":    name,
		"preview": true,
	}
	// displayName is required by defineEntityKind, but the documented preview
	// example omits it (a dry-run only needs a label to compute the plan). When
	// omitted, derive one from the name so the copy-pasteable example succeeds
	// instead of failing E_VALIDATION on a missing property.
	if flags.displayName != "" {
		in["displayName"] = flags.displayName
	} else {
		in["displayName"] = defaultKindDisplayName(name)
	}
	if flags.description != "" {
		in["description"] = flags.description
	}
	if flags.propertySchema != "" {
		raw, err := readPropertySchema(flags.propertySchema)
		if err != nil {
			//nolint:errcheck // best-effort stderr write
			fmt.Fprintf(cmd.ErrOrStderr(), "schema preview kind: --property-schema: %v\n", err)
			return silentError{code: 1, err: fmt.Errorf("read property schema: %w", err)}
		}
		in["propertySchema"] = raw
	}

	reqBody, merr := json.Marshal(in)
	if merr != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("marshal defineEntityKind preview request: %w", merr))
	}

	raw, err := invokeNavTool(cmd, "defineEntityKind", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "schema preview kind", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}

	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return werr
	}

	var resp kindPreviewResponse
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("parse defineEntityKind response: %w", uerr))
	}
	return renderKindPreviewCLI(cmd.OutOrStdout(), &resp)
}

// runSchemaPreviewSchema reads the definition from --definition, builds the
// defineSchema request with preview=true, invokes it, and renders the plan.
func runSchemaPreviewSchema(cmd *cobra.Command, schemaKind, version string, flags *previewSchemaFlags) error {
	if flags.definition == "" {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintln(cmd.ErrOrStderr(), "schema preview schema: --definition is required")
		return silentError{code: 1, err: errors.New("--definition required")}
	}

	defRaw, err := readDefinitionFile(flags.definition)
	if err != nil {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintf(cmd.ErrOrStderr(), "schema preview schema: --definition: %v\n", err)
		return silentError{code: 1, err: fmt.Errorf("read definition: %w", err)}
	}

	in := map[string]any{
		"schemaKind": schemaKind,
		"version":    version,
		"definition": defRaw,
		"preview":    true,
	}

	reqBody, merr := json.Marshal(in)
	if merr != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("marshal defineSchema preview request: %w", merr))
	}

	raw, err := invokeNavTool(cmd, "defineSchema", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "schema preview schema", err)
	}
	if raw == nil {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg)
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}

	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return werr
	}

	var resp struct {
		SchemaKind string `json:"schemaKind"`
		Version    string `json:"version"`
		Status     string `json:"status"`
		Plan       *struct {
			Action         string `json:"action"`
			SupersedesFrom string `json:"supersedesFrom,omitempty"`
			OrphanCount    int    `json:"orphanCount,omitempty"`
		} `json:"plan,omitempty"`
		Warnings []warncode.Warning `json:"warnings,omitempty"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapSchemaFatal(cmd, fmt.Errorf("parse defineSchema response: %w", uerr))
	}
	return renderSchemaPreviewCLI(cmd.OutOrStdout(), &resp)
}

// renderKindPreviewCLI renders the kind preview plan as a human-readable table.
func renderKindPreviewCLI(w io.Writer, resp *kindPreviewResponse) (err error) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	p := func(format string, a ...any) {
		if err == nil {
			_, err = fmt.Fprintf(tw, format, a...)
		}
	}
	p("kind\t%s\n", resp.Name)
	if resp.Plan != nil {
		p("action\t%s\n", resp.Plan.Action)
		if resp.Plan.CollapsesInto != "" {
			p("collapses into\t%s\n", resp.Plan.CollapsesInto)
		}
		if resp.Plan.SupersedesFrom != "" {
			p("supersedes from\t%s\n", resp.Plan.SupersedesFrom)
		}
		if resp.Plan.OrphanCount > 0 {
			p("orphan count\t%d\n", resp.Plan.OrphanCount)
		}
		if resp.Plan.CloseMatch != "" {
			p("close match\t%s\n", resp.Plan.CloseMatch)
		}
		for _, nk := range resp.Plan.NearestKinds {
			p("nearest kind\t%s (%.2f)\n", nk.Kind, nk.Similarity)
		}
		if len(resp.Plan.ChecksSkipped) > 0 {
			p("checks skipped\t%s\n", strings.Join(resp.Plan.ChecksSkipped, ", "))
		}
	}
	if err != nil {
		return err
	}
	if ferr := tw.Flush(); ferr != nil {
		return ferr
	}
	return renderPreviewWarnings(w, resp.Warnings)
}

// renderSchemaPreviewCLI renders the schema preview plan as a human-readable table.
func renderSchemaPreviewCLI(w io.Writer, resp *struct {
	SchemaKind string `json:"schemaKind"`
	Version    string `json:"version"`
	Status     string `json:"status"`
	Plan       *struct {
		Action         string `json:"action"`
		SupersedesFrom string `json:"supersedesFrom,omitempty"`
		OrphanCount    int    `json:"orphanCount,omitempty"`
	} `json:"plan,omitempty"`
	Warnings []warncode.Warning `json:"warnings,omitempty"`
},
) (err error) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	p := func(format string, a ...any) {
		if err == nil {
			_, err = fmt.Fprintf(tw, format, a...)
		}
	}
	p("schemaKind\t%s\n", resp.SchemaKind)
	p("version\t%s\n", resp.Version)
	if resp.Plan != nil {
		p("action\t%s\n", resp.Plan.Action)
		if resp.Plan.SupersedesFrom != "" {
			p("supersedes from\t%s\n", resp.Plan.SupersedesFrom)
		}
		if resp.Plan.OrphanCount > 0 {
			p("orphan count\t%d\n", resp.Plan.OrphanCount)
		}
	}
	if err != nil {
		return err
	}
	if ferr := tw.Flush(); ferr != nil {
		return ferr
	}
	return renderPreviewWarnings(w, resp.Warnings)
}

// renderPreviewWarnings prints the optional MAZ-XXX advisory block shared by the
// kind and schema preview renderers. Returns the first write error, if any.
func renderPreviewWarnings(w io.Writer, warnings []warncode.Warning) error {
	if len(warnings) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nwarnings:"); err != nil {
		return err
	}
	for _, wn := range warnings {
		if _, err := fmt.Fprintf(w, "  [%s] %s\n", wn.Code, wn.Message); err != nil {
			return err
		}
	}
	return nil
}

// readDefinitionFile reads a JSON Schema definition from a file path or from
// stdin when path is "-". Returns the raw bytes, or an error.
func readDefinitionFile(path string) (json.RawMessage, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return json.RawMessage(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file %q: %w", path, err)
	}
	return json.RawMessage(data), nil
}
