package cli

// entity.go implements "maz entity create <kind> <displayName>" — the CLI-first
// way to mint a named entity deterministically, without an LLM extraction
// round-trip. It mirrors the MCP createEntity tool's semantics: validates the
// kind against the registry, accepts optional --identifier / --description /
// --attributes flags, writes through the materialising path so the entity is
// queryable immediately, and prints the new entities/{uid} resource name.
//
// The write pathway is identical to schema_kinds.go / relate.go:
// openMaterializingLog → dispatcher-backed FileEventLog. The dispatcher
// materialises the entity synchronously so it is queryable via `maz get` the
// instant this command returns — no `maz rebuild` needed.
//
// Before minting it runs the same duplicate check as the resolver (shared
// entitymatch lookup): an existing same-name/near-duplicate entity of the same
// kind refuses the create and returns candidates (status duplicate_candidates,
// exit 1) unless --force is set; a matching identifier reuses the existing
// entity (status reused, exit 0).
//
// Exit codes:
//   0   entity created or reused (resource name printed on stdout)
//   1   expected failure: bad input, unregistered kind, malformed --identifier,
//       or a same-name duplicate exists (re-run with --force to mint anyway)
//   2   unexpected failure: storage or wiring error

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// entityKindPatternCLI mirrors the MCP createEntity kind pattern so a malformed
// kind surfaces as a validation error here rather than an opaque storage failure.
var entityKindPatternCLI = regexp.MustCompile(`^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$`)

// createEntity result status values and the batch-internal error sentinel.
const (
	statusCreated             = "created"
	statusReused              = "reused"
	statusDuplicateCandidates = "duplicate_candidates"
	batchStatusError          = "error"
)

// entityCreateFlags holds the parsed flag values for "maz entity create".
type entityCreateFlags struct {
	description string
	identifier  string // single type:value pair, e.g. "email:merry@mithril.example"
	attributes  string // raw JSON object for entity Properties
	scope       string // project|personal|team write scope
	force       bool   // skip the duplicate gate and mint even on a name collision
	batch       string // path to a JSONL file of createEntity requests (bulk mint)
}

// newEntityCmd returns the "maz entity" parent command.
func newEntityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entity",
		Short: "Manage entities: create, update, merge, delete, inspect",
		Long: `Commands for minting, consolidating, and inspecting entities without an LLM
round-trip. (Note: "entity" is the singular write surface; "entities" is the
plural read/list surface.)

Subcommands:
  create          Mint a named entity of a registered kind (--batch bulk-mints from JSONL)
  update          Edit an existing entity's fields
  add-identifier  Attach an external identifier (anchors the resolver)
  merge           Fold a duplicate into the surviving entity (lossless)
  delete          Remove an unwanted/junk entity (refuses if it has active relations)
  context         Show an entity with its relations and facts

Examples:
  maz entity create person "Merry Brandybuck"
  maz entity create organization "Mithril Corp" --description "A fictional company."
  maz entity create --batch entities.jsonl    # bulk-mint, each row dedup-checked
  maz entity merge <duplicateUid> <survivingUid>`,
	}
	cmd.AddCommand(newEntityCreateCmd())
	cmd.AddCommand(newEntityUpdateCmd())
	cmd.AddCommand(newEntityAddIdentifierCmd())
	cmd.AddCommand(newEntityMergeCmd())
	cmd.AddCommand(newEntityDeleteCmd())
	cmd.AddCommand(newEntityContextCmd())
	return cmd
}

// newEntityCreateCmd returns "maz entity create <kind> <displayName>".
func newEntityCreateCmd() *cobra.Command {
	flags := &entityCreateFlags{}
	cmd := &cobra.Command{
		Use:   "create <kind> <displayName>",
		Short: "Mint a named entity of a registered kind (skips LLM extraction)",
		Long: `Mint a structured entity directly — the CLI-first alternative to maz remember
when you already hold the entity's kind and name. The entity is written through
the materialising event log, so it is queryable via maz get the instant this
command returns — no maz rebuild needed.

<kind> must be a core kind (person, organization, place, project, event,
document) or a registered extension kind (e.g. user:vehicle). A bare custom
kind name without a namespace prefix (e.g. "vehicle") is recorded under the
user: namespace (stored as "user:vehicle") — always use the full name when
querying or filtering. Register a new kind with maz schema kinds add first;
check what exists with maz introspect.

Flags:
  --description   One-sentence description of this entity (optional)
  --identifier    External identifier in type:value form, e.g. email:merry@mithril.example
                  (optional; anchors the resolver so future mentions attach here)
  --attributes    Raw JSON object of typed attributes for the kind, e.g.
                  '{"vin":"...","make":"..."}' (optional). Like the createEntity
                  MCP tool, these are validated against the kind's propertySchema
                  on write only when [ontology] strict_attributes is enabled;
                  with the default (off) a schema mismatch is warn-accepted.
  --force         Mint even when a same-name entity already exists (optional).
  --batch <file>  Bulk-mint from a JSONL file: one createEntity request object per
                  line, e.g.
                  {"kind":"person","displayName":"Sam","identifiers":[{"type":"email","value":"sam@x.io"}]}
                  Each identifiers element is an OBJECT {"type":..,"value":..} —
                  not the "type:value" string the --identifier flag takes.
                  Blank lines and #-comment lines are skipped. Each row runs
                  through the same duplicate check; --force applies to every row.
                  Positional <kind> <displayName> are omitted with --batch.
                  Exits 0 when no row errored — rows skipped as duplicates are
                  reported in the summary (duplicateCandidates + notes), not
                  treated as a failure — and exits 1 only if a row errors.

Duplicate check: before minting, this checks for an existing entity of the same
kind with the same name (or a near-identical one by embedding). If one exists,
NOTHING is created — the matching entities are printed and a single create exits
1. Reuse one of those uids, run maz entity merge, or re-run with --force to record
a genuinely distinct entity that happens to share a name (e.g. a second real
person named the same). A matching --identifier returns the existing entity (exit
0).

Exit codes (single create):
  0   entity created or reused (resource name printed on stdout)
  1   expected failure: bad input, unregistered kind, malformed --identifier, or
      a same-name duplicate exists (re-run with --force to mint anyway)
  2   unexpected failure: storage or wiring error
A --batch run exits 0 whenever no row errored (duplicate-skips are a reported
outcome, not a failure) and 1 only when a row errors.

Examples:
  maz entity create person "Merry Brandybuck"
  maz entity create organization "Mithril Corp" --description "A fictional company."
  maz entity create person "Sam Dev" --identifier email:sam@example.com
  maz entity create user:vehicle "Tesla Model 3" --attributes '{"make":"Tesla"}'
  maz entity create person "Pippin" --force
  maz entity create person "Pippin Test" --output json`,
		// --batch reads entities from a file, so the positional kind/displayName
		// are omitted; without it exactly two args are required.
		Args: func(cmd *cobra.Command, args []string) error {
			if flags.batch != "" {
				if len(args) != 0 {
					return errors.New("--batch reads entities from a file; do not also pass <kind> <displayName>")
				}
				return nil
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.batch != "" {
				return runEntityCreateBatch(cmd, flags)
			}
			return runEntityCreate(cmd, args[0], args[1], flags)
		},
	}
	cmd.Flags().StringVar(&flags.description, "description", "", "One-sentence description of this entity")
	cmd.Flags().StringVar(&flags.identifier, "identifier", "", "External identifier in type:value form, e.g. email:merry@mithril.example")
	cmd.Flags().StringVar(&flags.attributes, "attributes", "", "Raw JSON object of typed attributes, e.g. '{\"make\":\"Tesla\"}'")
	cmd.Flags().StringVar(&flags.scope, "scope", "", `scope to write into (default personal): personal=private to you; project=this repo, shared with repo access; team=shared team brain (needs membership). See 'maz help-topics scopes'.`)
	cmd.Flags().BoolVar(&flags.force, "force", false, "Mint even when a same-name entity already exists (skips the duplicate check)")
	cmd.Flags().StringVar(&flags.batch, "batch", "", "Path to a JSONL file of createEntity requests (one JSON object per line); bulk-mint each through the same duplicate check")
	return cmd
}

// runEntityCreate validates inputs and routes a createEntity tool call through
// the daemon-or-in-process path: the write lands in the running daemon's event
// log (where a team-scope mint can be sealed) or in-process when no daemon
// serves this data dir. The createEntity tool resolves the kind against the
// registry, mints the ULID, and materialises the entity; this command renders
// the new entities/{uid} resource name.
func runEntityCreate(cmd *cobra.Command, kind, displayName string, flags *entityCreateFlags) error {
	if err := validateEntityCreateInput(cmd, kind, displayName, flags); err != nil {
		return err
	}

	// Validate scope client-side for a fast, actionable exit 1; the createEntity
	// tool re-validates and enforces the team-membership boundary on write.
	scope, serr := parseWriteScope(flags.scope)
	if serr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "entity create: %s\n", serr.Error())
		return silentError{code: 1, err: serr}
	}

	reqBody, berr := buildCreateEntityToolRequest(kind, displayName, scope, flags)
	if berr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "entity create: %v\n", berr)
		return silentError{code: 1, err: berr}
	}

	raw, err := invokeNavTool(cmd, createEntityToolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "entity create", err)
	}

	var out entityCreateResult
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return wrapEntityFatal(cmd, fmt.Errorf("parse entity response: %w", uerr))
	}

	return printEntityCreateResult(cmd, kind, &out)
}

// batchCreateSummary tallies a --batch run.
type batchCreateSummary struct {
	Created             int             `json:"created"`
	Reused              int             `json:"reused"`
	DuplicateCandidates int             `json:"duplicateCandidates"`
	Errors              int             `json:"errors"`
	Notes               []batchLineNote `json:"notes,omitempty"`
}

// batchLineNote records a line that did not mint a fresh entity, with enough
// context to act on it.
type batchLineNote struct {
	Line        int    `json:"line"`
	DisplayName string `json:"displayName,omitempty"`
	Status      string `json:"status"` // "duplicate_candidates" | "error"
	Detail      string `json:"detail,omitempty"`
}

// runEntityCreateBatch bulk-mints entities from a JSONL file, routing each line
// through the createEntity tool so every row passes the same duplicate gate.
// Blank and #-comment lines are skipped. A per-line outcome (malformed JSON,
// tool error, or a duplicate refusal) is tallied and reported but never aborts
// the run. Exit 1 only when a line errored; a duplicate-skip is a reported
// outcome, not a failure, so a load whose only non-created rows are dedup-skips
// (errors:0) exits 0.
func runEntityCreateBatch(cmd *cobra.Command, flags *entityCreateFlags) error {
	f, err := os.Open(flags.batch)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "entity create --batch: %v\n", err)
		return silentError{code: 1, err: err}
	}
	defer func() { _ = f.Close() }()

	var sum batchCreateSummary
	sc := bufio.NewScanner(f)
	// Entities with long property blobs can exceed the default 64KiB line cap.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, status, detail := runBatchLine(cmd, line, flags.force)
		switch status {
		case statusCreated:
			sum.Created++
		case statusReused:
			sum.Reused++
		case statusDuplicateCandidates:
			sum.DuplicateCandidates++
			sum.Notes = append(sum.Notes, batchLineNote{Line: lineNo, DisplayName: name, Status: status, Detail: detail})
		default: // batchStatusError
			sum.Errors++
			sum.Notes = append(sum.Notes, batchLineNote{Line: lineNo, DisplayName: name, Status: batchStatusError, Detail: detail})
		}
	}
	if serr := sc.Err(); serr != nil {
		return respondFatal(cmd, "entity create --batch", fmt.Errorf("read %s: %w", flags.batch, serr))
	}

	return printBatchSummary(cmd, &sum)
}

// runBatchLine processes one JSONL line: validates it is an object with a
// non-empty kind and displayName, applies --force, invokes createEntity, and
// returns the display name, the result status ("created"|"reused"|
// "duplicate_candidates"), and a human-readable detail. A malformed line or a
// tool error returns status "error" with the message in detail.
func runBatchLine(cmd *cobra.Command, line string, force bool) (name, status, detail string) {
	var req map[string]any
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return "", batchStatusError, fmt.Sprintf("invalid JSON: %v", err)
	}
	kind, kok := req["kind"].(string)
	displayName, nok := req["displayName"].(string)
	if !kok || !nok || kind == "" || displayName == "" {
		return displayName, batchStatusError, "each line needs a non-empty string kind and displayName"
	}
	if force {
		req["force"] = true
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		return displayName, batchStatusError, fmt.Sprintf("marshal request: %v", err)
	}
	raw, err := invokeNavTool(cmd, createEntityToolName, reqBody)
	if err != nil {
		return displayName, batchStatusError, err.Error()
	}
	var out entityCreateResult
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return displayName, batchStatusError, fmt.Sprintf("parse response: %v", uerr)
	}
	if out.Status == statusDuplicateCandidates {
		return displayName, out.Status, fmt.Sprintf("%d existing %s match", len(out.Candidates), pluralizeEntities(len(out.Candidates)))
	}
	return displayName, out.Status, ""
}

// printBatchSummary renders the batch tally (JSON or text) and returns exit 1
// only when a line errored. Per-row duplicate-skips are reported in the summary
// (duplicateCandidates + notes), not treated as a failure.
func printBatchSummary(cmd *cobra.Command, sum *batchCreateSummary) error {
	wr := output.From(cmd)
	switch wr.Format() {
	case output.FormatJSON, output.FormatNDJSON:
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(sum); err != nil {
			return err
		}
	default:
		w := cmd.OutOrStdout()
		//nolint:errcheck // best-effort stdout
		fmt.Fprintf(w, "batch: %d created, %d reused, %d duplicate (skipped), %d error(s)\n",
			sum.Created, sum.Reused, sum.DuplicateCandidates, sum.Errors)
		for _, n := range sum.Notes {
			//nolint:errcheck // best-effort stdout
			fmt.Fprintf(w, "  line %d %q [%s]: %s\n", n.Line, n.DisplayName, n.Status, n.Detail)
		}
	}
	// A bulk load treats per-row dedup-skips as a reported outcome (counted in
	// duplicateCandidates + listed in notes), NOT a failure: exit 0 when no row
	// errored, so a script loading a list that contains internal repeats reads
	// success. Only genuine per-row errors (bad kind, malformed input, storage
	// fault) fail the batch. A single `maz entity create` still exits 1 on a
	// same-name duplicate — there the one requested mint did not happen.
	if sum.Errors > 0 {
		return silentError{code: 1, err: errors.New("batch: some lines errored")}
	}
	return nil
}

// createEntityToolName is the registered MCP createEntity tool name, used to
// route the CLI write through the daemon-or-in-process tool path.
const createEntityToolName = "createEntity"

// buildCreateEntityToolRequest maps the CLI flags onto the createEntity tool's
// input fields: --identifier (type:value) becomes a one-element identifiers
// array, --attributes becomes properties, --scope becomes scope, and
// --description is folded into properties under a "description" key so the flag
// PERSISTS rather than being silently dropped (it previously had no transport
// and the value was lost). Returns an error for a malformed --identifier or
// --attributes so the exit stays bad-input (1).
func buildCreateEntityToolRequest(kind, displayName string, scope kernel.Scope, flags *entityCreateFlags) (json.RawMessage, error) {
	in := map[string]any{
		"kind":        kind,
		"displayName": displayName,
	}
	if scope != "" {
		in["scope"] = string(scope)
	}
	if flags.force {
		in["force"] = true
	}
	// properties carries --attributes (a raw JSON object) with --description
	// folded in as a "description" key. Building a map (rather than passing the
	// raw bytes through) both lets the description merge cleanly and surfaces a
	// malformed --attributes as a real error instead of a downstream surprise.
	props := map[string]any{}
	if flags.attributes != "" {
		if err := json.Unmarshal([]byte(flags.attributes), &props); err != nil {
			return nil, fmt.Errorf("--attributes %q: must be a JSON object: %w", flags.attributes, err)
		}
	}
	if flags.description != "" {
		props["description"] = flags.description
	}
	if len(props) > 0 {
		in["properties"] = props
	}
	if flags.identifier != "" {
		idx := strings.IndexByte(flags.identifier, ':')
		if idx <= 0 || idx == len(flags.identifier)-1 {
			return nil, fmt.Errorf("--identifier %q: both type and value must be non-empty (type:value form)", flags.identifier)
		}
		in["identifiers"] = []map[string]string{{
			"type":  flags.identifier[:idx],
			"value": flags.identifier[idx+1:],
		}}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal entity request: %w", err)
	}
	return raw, nil
}

// printEntityCreateResult renders the createEntity tool output in the active
// output mode. InputKind is the kind the user typed; out.Kind is the canonical
// kind the tool minted under. Three statuses are handled:
//
//   - created / reused: print the resource name (exit 0). On reused, note the
//     existing entity was matched by identifier; on created with advisory
//     near-matches, note them so the user can still choose to merge.
//   - duplicate_candidates: nothing was created — print the matching entities
//     and a remediation hint, then exit 1 (silentError) so scripts and agents
//     do not assume a successful mint.
//
// JSON/NDJSON modes always emit the full result (candidates included); the exit
// code still signals refusal on duplicate_candidates.
func printEntityCreateResult(cmd *cobra.Command, inputKind string, out *entityCreateResult) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
		if out.Status == statusDuplicateCandidates {
			return silentError{code: 1, err: errors.New("duplicate candidates: not created")}
		}
		return nil
	}

	if out.Status == statusDuplicateCandidates {
		return printEntityDuplicateCandidates(cmd, out)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, out.Name); err != nil {
		return err
	}
	if out.Status == statusReused {
		if _, err := fmt.Fprintln(tw, "(reused: an existing entity matched by identifier)"); err != nil {
			return err
		}
	}
	// When the user supplied a bare extension kind that was canonicalised to
	// the user: namespace (e.g. "vehicle" -> "user:vehicle"), surface the
	// stored kind so the user knows the exact name to use in queries and
	// filters. Without this note the entity is effectively unqueryable until
	// the user discovers the namespace rewrite themselves.
	if inputKind != out.Kind {
		if _, err := fmt.Fprintf(tw, "(kind: %s)\n", out.Kind); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// Advisory near-matches on a successful create: surfaced so the user can
	// still merge if these turn out to be the same entity. Non-fatal.
	if len(out.Candidates) > 0 {
		//nolint:errcheck // best-effort advisory note on stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: %d similar %s already exist; run `maz entity context %s` or `maz entity merge` if this is a duplicate\n",
			len(out.Candidates), pluralizeEntities(len(out.Candidates)), out.UID)
	}
	return nil
}

// printEntityDuplicateCandidates renders the refused-create case: a table of the
// existing entities that matched, plus a remediation hint, then returns
// silentError(1). The table is sorted by the tool (exact-name first, then
// descending similarity); higher relationCount is the more canonical entity to
// keep.
func printEntityDuplicateCandidates(cmd *cobra.Command, out *entityCreateResult) error {
	errw := cmd.ErrOrStderr()
	//nolint:errcheck // best-effort stderr; the silentError code is the contract
	fmt.Fprintf(errw, "not created: %d existing %s match this name — reuse one below, run `maz entity merge`, or re-run with --force to mint a distinct entity:\n\n",
		len(out.Candidates), pluralizeEntities(len(out.Candidates)))

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "UID\tNAME\tKIND\tSIM\tRELS\tVIA"); err != nil {
		return err
	}
	for _, c := range out.Candidates {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f\t%d\t%s\n",
			c.UID, c.DisplayName, c.Kind, c.Similarity, c.RelationCount, c.Reason); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return silentError{code: 1, err: errors.New("duplicate candidates: not created")}
}

// pluralizeEntities returns the singular entity noun for n==1, else its plural.
func pluralizeEntities(n int) string {
	if n == 1 {
		return diffKindEntity // "entity"
	}
	return "entities"
}

// entityCreateResult is the JSON output shape for maz entity create. Name/uid/
// kind are set on created and reused; on duplicate_candidates only status and
// candidates are populated (nothing was minted).
type entityCreateResult struct {
	Name       string                  `json:"name,omitempty"`
	UID        string                  `json:"uid,omitempty"`
	Kind       string                  `json:"kind,omitempty"`
	Status     string                  `json:"status"`
	Candidates []entityCreateCandidate `json:"candidates,omitempty"`
}

// entityCreateCandidate is an existing entity surfaced as a possible duplicate,
// mirroring the createEntity tool's candidate wire shape.
type entityCreateCandidate struct {
	UID           string  `json:"uid"`
	DisplayName   string  `json:"displayName"`
	Kind          string  `json:"kind"`
	Similarity    float64 `json:"similarity"`
	Reason        string  `json:"reason"`
	RelationCount int     `json:"relationCount"`
}

// validateEntityCreateInput runs client-side preconditions before touching
// the store. Returns a silentError(1) on any validation failure.
func validateEntityCreateInput(cmd *cobra.Command, kind, displayName string, flags *entityCreateFlags) error {
	if !entityKindPatternCLI.MatchString(kind) {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"entity create: kind %q must be a core kind or a namespaced extension kind (e.g. person, user:vehicle)\n", kind)
		return silentError{code: 1, err: fmt.Errorf("invalid kind: %s", kind)}
	}
	if strings.TrimSpace(displayName) == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "entity create: displayName is required and must be non-empty")
		return silentError{code: 1, err: errors.New("displayName required")}
	}
	if flags.attributes != "" {
		var probe map[string]any
		if jerr := json.Unmarshal([]byte(flags.attributes), &probe); jerr != nil {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "entity create: --attributes must be a JSON object: %v\n", jerr)
			return silentError{code: 1, err: fmt.Errorf("invalid --attributes: %w", jerr)}
		}
	}
	if flags.identifier != "" && !strings.Contains(flags.identifier, ":") {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"entity create: --identifier must be in type:value form, e.g. email:merry@mithril.example (got %q)\n", flags.identifier)
		return silentError{code: 1, err: fmt.Errorf("malformed --identifier: %s", flags.identifier)}
	}
	return nil
}

// wrapEntityFatal prints err to stderr and returns a silentError with code 2.
func wrapEntityFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "entity create", err)
}

// -- entity update ------------------------------------------------------------

// entityUpdateFlags holds the parsed flag values for "maz entity update".
type entityUpdateFlags struct {
	kind        string
	displayName string
}

// newEntityUpdateCmd returns "maz entity update <uid> [--kind X] [--display-name Y]".
func newEntityUpdateCmd() *cobra.Command {
	flags := &entityUpdateFlags{}
	cmd := &cobra.Command{
		Use:   "update <uid>",
		Short: "Relabel an entity's kind and/or display name",
		Long: `Relabel a named entity by appending an entityUpdated event. At least one of
--kind or --display-name must be provided; omitting both is a no-op (exit 1).

The entity must exist; an unknown uid exits with code 1.

Exit codes:
  0   entity updated (resource name printed on stdout)
  1   expected failure: unknown uid, no fields provided
  2   unexpected failure: storage or wiring error

Examples:
  maz entity update 01HZZZZZZZZZZZZZZZZZZZEXMP --kind organization
  maz entity update 01HZZZZZZZZZZZZZZZZZZZEXMP --display-name "Acme Corp"
  maz entity update 01HZZZZZZZZZZZZZZZZZZZEXMP --kind organization --display-name "Acme Corp"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntityUpdate(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.kind, "kind", "", "New entity kind in snake_case (optional)")
	cmd.Flags().StringVar(&flags.displayName, "display-name", "", "New human-readable label (optional)")
	return cmd
}

// runEntityUpdate validates inputs and routes an updateEntity tool call through
// the daemon (or in-process materialising log on a non-running daemon). Prints
// the resource name on success.
func runEntityUpdate(cmd *cobra.Command, uid string, flags *entityUpdateFlags) error {
	if strings.TrimSpace(flags.kind) == "" && strings.TrimSpace(flags.displayName) == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "entity update: at least one of --kind or --display-name must be provided")
		return silentError{code: 1, err: errors.New("no fields to update")}
	}

	in := map[string]any{"entityUid": uid}
	if flags.kind != "" {
		in["kind"] = flags.kind
	}
	if flags.displayName != "" {
		in["displayName"] = flags.displayName
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "entity update: %v\n", err)
		return silentError{code: 1, err: err}
	}

	raw, err := invokeNavTool(cmd, "updateEntity", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "entity update", err)
	}

	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return respondFatal(cmd, "entity update", fmt.Errorf("parse response: %w", uerr))
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), out.Name)
	return err
}

// -- entity add-identifier ----------------------------------------------------

// entityAddIdentifierFlags holds the parsed flags for "maz entity add-identifier".
type entityAddIdentifierFlags struct {
	identifier string
	kind       string
}

// newEntityAddIdentifierCmd returns "maz entity add-identifier <uid> --identifier X [--kind alias]".
func newEntityAddIdentifierCmd() *cobra.Command {
	flags := &entityAddIdentifierFlags{}
	cmd := &cobra.Command{
		Use:   "add-identifier <uid>",
		Short: "Attach an alias/identifier to an existing entity",
		Long: `Attach an identifier (an alias by default) to an existing entity by appending
an entityIdentifierAdded event, so the resolver and vault wikilinks match the
entity by that surface form. Use this after renaming an entity's display name
(maz entity update --display-name) to retain the old name:

  maz entity update 01...EXMP --display-name "Erebor Trading Company"
  maz entity add-identifier 01...EXMP --identifier "Erebor Global"

createEntity only seeds identifiers at mint time; this adds them afterwards.
--kind defaults to "alias"; pass email/username/phone/id or a snake_case domain
type (hrb, lei, tax_id, …) for other identifiers.

The entity must exist; an unknown uid exits with code 1.

Exit codes:
  0   identifier added (resource name printed on stdout)
  1   expected failure: unknown uid, empty identifier, malformed kind
  2   unexpected failure: storage or wiring error

Examples:
  maz entity add-identifier 01HZZZZZZZZZZZZZZZZZZZEXMP --identifier "Erebor Global"
  maz entity add-identifier 01HZZZZZZZZZZZZZZZZZZZEXMP --identifier merry@x.example --kind email`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntityAddIdentifier(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.identifier, "identifier", "", "The surface form / identifier value to attach (required)")
	cmd.Flags().StringVar(&flags.kind, "kind", "", `Identifier kind in snake_case (default "alias"): alias|email|username|phone|id or a domain type`)
	//nolint:errcheck // flag is defined immediately above
	cmd.MarkFlagRequired("identifier")
	return cmd
}

// runEntityAddIdentifier validates inputs and routes an addIdentifier tool call
// through the daemon (or in-process materialising log on a non-running daemon).
// Prints the entity resource name on success.
func runEntityAddIdentifier(cmd *cobra.Command, uid string, flags *entityAddIdentifierFlags) error {
	if strings.TrimSpace(flags.identifier) == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "entity add-identifier: --identifier is required and must be non-empty")
		return silentError{code: 1, err: errors.New("identifier required")}
	}

	in := map[string]any{"entityUid": uid, "identifier": flags.identifier}
	if k := strings.TrimSpace(flags.kind); k != "" {
		in["kind"] = k
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "entity add-identifier: %v\n", err)
		return silentError{code: 1, err: err}
	}

	raw, err := invokeNavTool(cmd, "addIdentifier", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "entity add-identifier", err)
	}

	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return respondFatal(cmd, "entity add-identifier", fmt.Errorf("parse response: %w", uerr))
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), out.Name)
	return err
}

// -- entity merge -------------------------------------------------------------

// newEntityMergeCmd returns "maz entity merge <sourceUid> <targetUid>".
func newEntityMergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <sourceUid> <targetUid>",
		Short: "Merge source entity into target (source is removed)",
		Long: `Merge source entity into target by appending an entityMerged event. All
relations, facts, and mentions that referenced the source are repointed to the
target; the source entity row is then deleted. Both entities must exist. The
merge also collapses any duplicate active edges the repoint creates (drops
self-loops, keeps the highest-confidence triple) so the survivor has no
redundant relations.

Graph layer: the SQLite/recall surface is correct the instant this returns. When
a daemon is running it reconciles the graph (RELATES_TO edges) online; after an
offline/in-process merge, run "maz rebuild" to reconcile the graph layer.

Exit codes:
  0   merge applied (surviving resource name printed on stdout)
  1   expected failure: same uid, unknown entity
  2   unexpected failure: storage or wiring error

Examples:
  maz entity merge 01HZZZZZZZZZZZZZZZZZZZDUPL 01HZZZZZZZZZZZZZZZZZZZREAL`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntityMerge(cmd, args[0], args[1])
		},
	}
}

// runEntityMerge validates inputs and routes a mergeEntities tool call through
// the daemon (or in-process materialising log on a non-running daemon).
func runEntityMerge(cmd *cobra.Command, src, tgt string) error {
	if src == tgt {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "entity merge: sourceUid and targetUid must be different")
		return silentError{code: 1, err: errors.New("source and target are the same")}
	}

	reqBody, err := json.Marshal(map[string]any{
		"sourceEntityUid": src,
		"targetEntityUid": tgt,
	})
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "entity merge: %v\n", err)
		return silentError{code: 1, err: err}
	}

	raw, err := invokeNavTool(cmd, "mergeEntities", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "entity merge", err)
	}

	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return respondFatal(cmd, "entity merge", fmt.Errorf("parse response: %w", uerr))
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), out.Name)
	return err
}

// -- entity delete ------------------------------------------------------------

// entityDeleteFlags holds the parsed flag values for "maz entity delete".
type entityDeleteFlags struct {
	cascade bool
	reason  string
}

// newEntityDeleteCmd returns "maz entity delete <uid> [--cascade] [--reason]".
func newEntityDeleteCmd() *cobra.Command {
	flags := &entityDeleteFlags{}
	cmd := &cobra.Command{
		Use:   "delete <uid>",
		Short: "Delete an entity and its derived data (refuses if it has active relations)",
		Long: `Hard-delete an entity by appending an entityDeleted tombstone. The entity row
and all its derived data — relations, facts, mentions, identifiers, citations —
are removed and its graph node detached.

This is for unwanted or junk entities. To consolidate DUPLICATES without losing
data, use ` + "`maz entity merge`" + ` instead, which repoints the source's data onto
the surviving target.

Safe default: if the entity still has active relations the delete is REFUSED, so
a connected entity is never dropped by accident. Pass --cascade to tombstone
those relations first and proceed.

Flags:
  --cascade   Delete even when the entity has active relations (tombstones them first)
  --reason    Free-text reason, surfaced in audit/log views

Exit codes:
  0   entity deleted (resource name printed on stdout)
  1   expected failure: unknown entity, or active relations exist without --cascade
  2   unexpected failure: storage or wiring error

Examples:
  maz entity delete 01HZZZZZZZZZZZZZZZZZZZJUNK
  maz entity delete 01HZZZZZZZZZZZZZZZZZZZJUNK --cascade --reason "spurious extraction"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEntityDelete(cmd, args[0], flags)
		},
	}
	cmd.Flags().BoolVar(&flags.cascade, "cascade", false, "Delete even when the entity has active relations (tombstones them first)")
	cmd.Flags().StringVar(&flags.reason, "reason", "", "Free-text reason for the deletion, surfaced in audit/log views")
	return cmd
}

// runEntityDelete routes a deleteEntity tool call through the daemon-or-in-process
// path. The tool enforces the safe-delete guard (active relations require
// --cascade) and returns E_VALIDATION (exit 1) when that guard trips.
func runEntityDelete(cmd *cobra.Command, uid string, flags *entityDeleteFlags) error {
	req := map[string]any{"entityUid": uid}
	if flags.cascade {
		req["cascade"] = true
	}
	if flags.reason != "" {
		req["reason"] = flags.reason
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "entity delete: %v\n", err)
		return silentError{code: 1, err: err}
	}

	raw, err := invokeNavTool(cmd, "deleteEntity", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "entity delete", err)
	}

	var out struct {
		Name               string `json:"name"`
		Status             string `json:"status"`
		ForgottenRelations int    `json:"forgottenRelations"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return respondFatal(cmd, "entity delete", fmt.Errorf("parse response: %w", uerr))
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), out.Name); err != nil {
		return err
	}
	if out.ForgottenRelations > 0 {
		//nolint:errcheck // best-effort advisory note on stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "(cascade: tombstoned %d active %s)\n",
			out.ForgottenRelations, pluralizeRelations(out.ForgottenRelations))
	}
	return nil
}

// pluralizeRelations returns the singular relation noun for n==1, else its plural.
func pluralizeRelations(n int) string {
	if n == 1 {
		return diffKindRelation // "relation"
	}
	return "relations"
}
