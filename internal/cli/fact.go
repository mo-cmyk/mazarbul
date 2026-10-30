package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func newFactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fact",
		Short: "Record and read scalar facts (an entity's attributes, state, preferences)",
		Long: `Facts are scalar state slots about an entity — its attributes, current
state, or preferences — addressed by a hierarchical state_key, e.g.
state.task.status=pending or preference.coffee.milk=oat. They are the
slot-shaped counterpart to relations (typed edges between entities).

SEE ALSO:
  maz remember   record a free-text memory (extracts entities + relations)
  maz relate     assert a relationship between two entities (e.g. "Alice manages Phoenix")
Use 'fact' for an entity's own attributes/state, not for a relationship between two things.`,
		// Bare `maz fact` shows help; an unknown subcommand (e.g. `fact list`)
		// errors via NoArgs instead of printing help and exiting 0 — a silent
		// success that misleads scripts and agents. The RunE makes the command
		// Runnable so cobra validates Args (a group with only subcommands and no
		// RunE returns flag.ErrHelp before NoArgs ever runs).
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newFactSetCmd())
	cmd.AddCommand(newFactGetCmd())
	cmd.AddCommand(newFactRetractCmd())
	return cmd
}

func newFactRetractCmd() *cobra.Command {
	var scopes []string
	cmd := &cobra.Command{
		Use:   "retract <subject> <state-key>",
		Short: "Close the current value of a fact slot (preserves history)",
		Long: `Append a factRetracted tombstone closing the open value of a
(subject, state_key) slot — the scalar counterpart to 'maz unrelate'. The
value drops out of current reads (maz fact get no longer lists it) but its
history is PRESERVED: get(facts/{uid}) still walks it. The slot then reads as
EMPTY — retraction records that the value stopped being true, it does NOT fall
back to an earlier value.

To set a different value instead, just 'maz fact set' the new one — a later
write supersedes the prior. To hard-delete a fact row entirely, use 'maz forget
facts/{uid}'.

The subject accepts a human display name, a bare ULID, the entities/{uid}
resource name, or the literal "self" — same as 'maz fact set'.

Exit codes:
  0   fact retracted (resource name printed on stdout)
  1   expected failure: bad input or no open value in the slot
  2   unexpected failure: storage or wiring error

Examples:
  maz fact retract "Erebor renewal" stage
  maz fact retract self preference.coffee.milk`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactRetract(cmd, args[0], args[1], scopes)
		},
	}
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "scopes to search for the open slot: project|personal|team (default project+personal)")
	return cmd
}

func runFactRetract(cmd *cobra.Command, subject, stateKey string, scopes []string) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// Rebind the "self" sentinel to the concrete principal UID client-side so the
	// slot lookup matches CLI-authored facts written under the principal, matching
	// 'maz fact set'. The retractFact tool also rebinds server-side via its binder.
	subject = rebindSelfToPrincipal(dataDir, strings.TrimPrefix(subject, "entities/"))

	in := map[string]any{"subject": subject, "stateKey": stateKey}
	if len(scopes) > 0 {
		in["scopes"] = scopes
	}
	reqBody, berr := json.Marshal(in)
	if berr != nil {
		return wrapFactFatal(cmd, fmt.Errorf("marshal retractFact request: %w", berr))
	}

	raw, err := invokeNavTool(cmd, "retractFact", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "fact retract", err)
	}

	var resp struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapFactFatal(cmd, fmt.Errorf("parse retractFact response: %w", uerr))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{"name": resp.Name, "status": resp.Status})
	}
	_, err = fmt.Fprintf(w, "%s %s\n", resp.Status, resp.Name)
	return err
}

type factSetFlags struct {
	eventStart     string
	sourcePriority float64
	source         string
	scope          string
}

func newFactSetCmd() *cobra.Command {
	flags := &factSetFlags{sourcePriority: 1.0, source: "cli:fact"}
	cmd := &cobra.Command{
		Use:   "set <subject> <state-key> <value>",
		Short: "Assert a scalar fact about an entity (skips LLM extraction)",
		Long: `Append a Fact event recording a (subject, state_key, value) slot. A later
set to the same (subject, state_key) supersedes this value; the prior one is
retained as history.

The subject accepts a human display name (e.g. "Erebor renewal"), a bare ULID,
the entities/{uid} resource name, or the literal "self". Display names are
resolved case-insensitively; an unknown or ambiguous name returns a pointed
error.

The write is materialised synchronously via the dispatcher-backed event log,
so maz fact get <subject-uid> returns the new slot the instant this command
returns — no maz rebuild needed.

If a Mazarbul daemon already serves this data dir it holds the store lock; stop
it with maz daemon stop before using this command.

Exit codes:
  0   fact recorded (resource name printed on stdout)
  1   expected failure: bad input
  2   unexpected failure: storage or wiring error

Examples:
  maz fact set "Erebor renewal" stage negotiation
  maz fact set 01ARZ3NDEKTSV4RRFFQ69G5FAV state.task.status pending
  maz fact set self preference.coffee.milk oat --source-priority 0.8`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactSet(cmd, args[0], args[1], args[2], flags)
		},
	}
	cmd.Flags().StringVar(&flags.eventStart, "event-start", "", "RFC 3339 instant the fact became true (defaults to now)")
	cmd.Flags().Float64Var(&flags.sourcePriority, "source-priority", 1.0, "recall-lane priority, range 0..1")
	cmd.Flags().StringVar(&flags.source, "source", "cli:fact", `event source identifier (defaults to "cli:fact")`)
	cmd.Flags().StringVar(&flags.scope, "scope", "", `scope to write into (default personal): personal=private to you; project=this repo, shared with repo access; team=shared team brain (needs membership). See 'maz help-topics scopes'.`)
	return cmd
}

// parseWriteScope validates an optional single write scope. Empty yields "" so
// the event log backfills the default (personal); an unknown value is a clean
// bad-input error. Mirrors the MCP write tools' scope handling.
func parseWriteScope(raw string) (kernel.Scope, error) {
	if raw == "" {
		return "", nil
	}
	s := kernel.Scope(raw)
	if !s.Valid() {
		return "", fmt.Errorf("scope %q is not in {project|personal|team|draft}", raw)
	}
	return s, nil
}

func runFactSet(cmd *cobra.Command, subject, stateKey, value string, flags *factSetFlags) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// Client-side niceties for fast feedback BEFORE the tool call: scope
	// validation, self→principal rebind, and the team-self guard. The MCP fact
	// tool re-validates scope, subject existence, and the state-key on the
	// write path, so the routed call is authoritative either way.
	scope, serr := parseWriteScope(flags.scope)
	if serr != nil {
		return wrapFactBadInput(cmd, serr.Error(), serr)
	}

	// Rebind the "self" sentinel to the concrete principal UID (#209 Part F) so a
	// CLI-authored fact attaches to the shareable principal rather than the
	// device-local seeded "self" row, matching the MCP fact tool.
	subject = rebindSelfToPrincipal(dataDir, strings.TrimPrefix(subject, "entities/"))

	// A team-scope write must not persist the unresolved "self" sentinel — it
	// would leak a device-local literal into a shared slot. Reject until the
	// self→principal binding is provisioned (which rebinds it above).
	if scope == kernel.ScopeTeam && subject == selfEntityUID {
		return wrapFactBadInput(cmd,
			`team-scope write cannot reference the unresolved "self" sentinel; provision the self→principal binding (team genesis) first`, nil)
	}

	if flags.sourcePriority < 0 || flags.sourcePriority > 1 {
		return wrapFactBadInput(cmd,
			fmt.Sprintf("--source-priority %v is out of range; must be between 0.0 and 1.0 (e.g. --source-priority 0.8)", flags.sourcePriority),
			fmt.Errorf("--source-priority out of range: %v", flags.sourcePriority))
	}

	reqBody, berr := buildFactToolRequest(subject, stateKey, value, scope, flags)
	if berr != nil {
		return wrapFactFatal(cmd, berr)
	}

	// Route through the daemon-or-in-process tool path so a team write lands in
	// the running daemon's event log (where it can be sealed) — or in-process
	// when no daemon serves this data dir. Identical behaviour either way: the
	// in-process fallback invokes the same registered MCP fact tool.
	raw, err := invokeNavTool(cmd, ontology.KindFact, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "fact set", err)
	}

	var resp struct {
		Name string `json:"name"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapFactFatal(cmd, fmt.Errorf("parse fact response: %w", uerr))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{"name": resp.Name})
	}
	_, err = fmt.Fprintln(w, resp.Name)
	return err
}

// buildFactToolRequest assembles the JSON request body for the fact tool from
// the parsed CLI arguments. The subject and scope are already resolved/validated
// by the caller. SourcePriority is sent only when it differs from the default
// 1.0, and eventStart only when set, so the routed payload mirrors what the
// in-process write used to produce.
func buildFactToolRequest(subject, stateKey, value string, scope kernel.Scope, flags *factSetFlags) (json.RawMessage, error) {
	in := map[string]any{
		"subject":  subject,
		"stateKey": stateKey,
		"value":    value,
		"source":   flags.source,
	}
	if scope != "" {
		in["scope"] = string(scope)
	}
	if flags.sourcePriority != 1.0 {
		in["sourcePriority"] = flags.sourcePriority
	}
	if flags.eventStart != "" {
		in["eventStart"] = flags.eventStart
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal fact request: %w", err)
	}
	return raw, nil
}

func newFactGetCmd() *cobra.Command {
	var scopes []string
	cmd := &cobra.Command{
		Use:   "get <subject-uid>",
		Short: "List an entity's current (open) facts",
		Long: `Show the currently-open scalar facts for an entity — its attributes/state
right now, one row per state_key. Reads through the same get verb the agent
surface uses, so a running daemon and an in-process open both work.

The default read spans {project, personal}; pass --scope team (repeatable) to
include team-scope facts. "self" resolves to the bound principal entity, the
same target maz fact set self writes to, so set and get stay symmetric.

Examples:
  maz fact get 01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz fact get self --json
  maz fact get self --scope personal --scope team`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("fact get: subject-uid argument required\n\nUsage: maz fact get <subject-uid>\nExample: maz fact get self")
			}
			if len(args) > 1 {
				return fmt.Errorf("fact get: accepts 1 argument, got %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactGet(cmd, scopes, args[0])
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	return cmd
}

// runFactGet resolves the subject (rebinding "self" to the bound principal),
// reads its open facts through the get verb, and renders them. Split out of the
// RunE closure to keep cognitive complexity in check.
func runFactGet(cmd *cobra.Command, scopes []string, arg string) error {
	if err := validateScopeFlag(cmd, "fact get", scopes); err != nil {
		return err
	}
	// Resolve "self" to the bound principal UID before building the resource
	// name so fact get self reads the same entity fact set self writes to (#209
	// Part F); without this they target different rows ("self" sentinel vs
	// principal) and a set is invisible to a get.
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	name := rebindSelfToPrincipal(dataDir, strings.TrimPrefix(arg, "entities/"))
	if !strings.Contains(name, "/") {
		name = "entities/" + name
	}
	in := map[string]any{"name": name}
	if len(scopes) > 0 {
		in["scopes"] = scopes
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return wrapFactFatal(cmd, err)
	}
	raw, err := invokeNavTool(cmd, "get", reqBody)
	if err != nil {
		if isExpectedNavError(err) && isNotFoundLike(err) {
			return wrapFactGetNotFound(cmd, name)
		}
		return wrapFactFatal(cmd, err)
	}
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}
	return renderFactsCLI(cmd, raw)
}

// renderFactsCLI prints the facts[] array of a get(entities/{uid}) response as
// aligned state_key / value rows.
func renderFactsCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var rec struct {
		Facts []struct {
			StateKey string `json:"stateKey"`
			Value    string `json:"value"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("fact get: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if len(rec.Facts) == 0 {
		_, err := fmt.Fprintln(w, "(no facts)")
		return err
	}
	sort.Slice(rec.Facts, func(i, j int) bool { return rec.Facts[i].StateKey < rec.Facts[j].StateKey })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range rec.Facts {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", f.StateKey, f.Value); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// isNotFoundLike reports whether err carries an E_NOT_FOUND code or message.
// It is narrower than isExpectedNavError (which also matches E_VALIDATION and
// E_PERMISSION) so callers can branch on the specific not-found class.
func isNotFoundLike(err error) bool {
	if err == nil {
		return false
	}
	var te *mcp.ToolError
	if errors.As(err, &te) {
		return te.Code == mcp.CodeNotFound
	}
	return strings.Contains(err.Error(), string(mcp.CodeNotFound))
}

// wrapFactGetNotFound prints an actionable hint for `fact get` on a missing
// subject entity, names that the argument is an entity uid (the subject of a
// fact), and exits 1 (expected failure: bad input). The raw mcp E_NOT_FOUND
// message is suppressed in favour of a human-readable pointer to recovery.
func wrapFactGetNotFound(cmd *cobra.Command, name string) error {
	//nolint:errcheck // best-effort stderr on bad-input path
	fmt.Fprintf(cmd.ErrOrStderr(),
		"fact get: subject entity %s not found; the argument must be an entity uid (the subject of a fact) — list entities with `maz entities`, or create one with `maz remember` / `maz entity create`\n",
		name)
	return silentError{code: 1, err: fmt.Errorf("subject entity %s not found", name)}
}

// wrapFactFatal prints err to stderr under the fact command and returns exit code 2
// (unexpected/wiring failure). Use wrapFactBadInput for user-supplied bad values.
func wrapFactFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "fact", err)
}

// wrapFactBadInput prints msg to stderr under the fact command and returns exit
// code 1 (expected failure: bad user input). This is distinct from wrapFactFatal
// (code 2, unexpected/storage failure) so callers can distinguish "the user
// passed a bad value" from "the storage layer failed".
func wrapFactBadInput(cmd *cobra.Command, msg string, cause error) error {
	//nolint:errcheck // best-effort stderr on bad-input path
	fmt.Fprintf(cmd.ErrOrStderr(), "fact set: %s\n", msg)
	return silentError{code: 1, err: cause}
}
