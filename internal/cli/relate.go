package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

type relateFlags struct {
	validFrom  string
	validUntil string
	confidence float64
	source     string
	scope      string
	batch      string // path to a JSONL file of {subject,predicate,object,...} objects
}

// selfEntityUID is the canonical UID of the seeded self entity (migration 0012;
// resolver.SelfEntityUID). Mirrored as a local constant to avoid importing the
// resolver into the CLI command layer.
const selfEntityUID = "self"

// rebindSelfToPrincipal maps the local "self" sentinel to the bound principal
// UID so a CLI-authored relation or fact references the shareable principal
// entity rather than the device-local seeded "self" row — mirroring the
// self-resolution the MCP relate/fact tools already perform (#209 Part F). The
// uid must already be stripped of any "entities/" prefix. Any other value is
// returned untouched, and "self" is left verbatim when the device is unbound
// (genesis has not run), preserving pre-identity behaviour.
func rebindSelfToPrincipal(dataDir, uid string) string {
	if uid != selfEntityUID {
		return uid
	}
	if pid, err := config.SelfPrincipalUID(dataDir); err == nil && pid != "" {
		return pid
	}
	return uid
}

func newRelateCmd() *cobra.Command {
	flags := &relateFlags{confidence: 1.0, source: "cli:relate"}
	cmd := &cobra.Command{
		Use:   "relate <subject> <predicate> <object>",
		Short: "Directly assert a relation (skips LLM extraction)",
		Long: `Append a RelationCandidatesProposed event that the resolver promotes
into a canonical Relation. Use this for facts you want recorded without
remember/extraction round-tripping through the LLM.

relate APPENDS a new edge; it does not supersede an existing identical edge.
To mark an edge no longer valid, forget it with "maz forget relations/<ulid>".

Both <subject> and <object> must name entities that already exist in the
projection. Each argument accepts a human display name (e.g. "Bilbo Baggins"),
a bare ULID, the entities/{uid} resource name, or the literal "self". Display
names are resolved case-insensitively; an unknown or ambiguous name returns a
pointed error. Mint entities first with maz entity create or maz remember.

<predicate> must be snake_case (lowercase letters, digits, underscores; must
start with a letter), e.g. works_at, member_of, has_role. If the predicate is
not already in the vocabulary it is auto-registered so it appears in
maz schema predicates / maz introspect immediately.

The write is materialised synchronously via the dispatcher-backed event log,
so maz relations <subject-uid> returns the new edge the instant this command
returns — no maz rebuild needed.

If a Mazarbul daemon already serves this data dir it holds the store lock; stop
it with maz daemon stop before using this command, or call the relate MCP tool
through the daemon instead.

Exit codes:
  0   relation asserted
  1   expected failure: entity not found, bad input
  2   unexpected failure: storage or wiring error

Examples:
  maz relate "Bilbo Baggins" knows "Gandalf the Grey"
  maz relate 01HZZZA... works_at 01HZZZB... --valid-from 2024-01-01T00:00:00Z
  maz relate 01HZZZA... knows   01HZZZC... --confidence 0.8`,
		Args: func(cmd *cobra.Command, args []string) error {
			if flags.batch != "" {
				if len(args) != 0 {
					return errors.New("cannot combine positional arguments with --batch")
				}
				return nil
			}
			return cobra.ExactArgs(3)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.batch != "" {
				return runRelateBatch(cmd, flags)
			}
			return runRelate(cmd, args[0], args[1], args[2], flags)
		},
	}
	cmd.Flags().StringVar(&flags.validFrom, "valid-from", "", "RFC 3339 lower bound on the relation's valid-time")
	cmd.Flags().StringVar(&flags.validUntil, "valid-until", "", "RFC 3339 upper bound on the relation's valid-time")
	cmd.Flags().Float64Var(&flags.confidence, "confidence", 1.0, "writer confidence in the relation, range 0..1")
	cmd.Flags().StringVar(&flags.source, "source", "cli:relate", `event source identifier (defaults to "cli:relate")`)
	cmd.Flags().StringVar(&flags.scope, "scope", "", `scope to write into (default personal): personal=private to you; project=this repo; team=shared team brain (needs membership); draft=private staging, invisible to recall until promoted. See 'maz help-topics scopes'.`)
	cmd.Flags().StringVar(&flags.batch, "batch", "", "path to a JSONL file of {subject,predicate,object,...} objects; asserts all in one atomic append via relateBatch")
	return cmd
}

// relateTeamSelfGuard rejects a team-scope edge whose subject or object is
// still the unresolved "self" sentinel — persisting it would leak a
// device-local literal into a shared edge. Extracted to keep runRelate within
// the gocyclo gate.
func relateTeamSelfGuard(cmd *cobra.Command, scope kernel.Scope, subjectUID, objectUID string) error {
	if scope == kernel.ScopeTeam && (subjectUID == selfEntityUID || objectUID == selfEntityUID) {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"relate: team-scope write cannot reference the unresolved \"self\" sentinel; provision the self→principal binding (team genesis) first\n")
		return silentError{code: 1, err: errors.New("team-scope relate references unresolved self")}
	}
	return nil
}

func runRelate(cmd *cobra.Command, subjectUID, predicate, objectUID string, flags *relateFlags) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// Accept either a bare ULID or an entities/{uid} resource name for both
	// endpoints. The help advertises both forms, and `maz entity create`
	// prints the entities/{uid} form — so a newcomer pasting that name straight
	// into `relate` must work. Strip the prefix here so the value matches the
	// entities projection's uid column the MCP relate tool checks against.
	subjectUID = strings.TrimPrefix(subjectUID, "entities/")
	objectUID = strings.TrimPrefix(objectUID, "entities/")

	scope, serr := parseWriteScope(flags.scope)
	if serr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "relate: %s\n", serr.Error())
		return silentError{code: 1, err: serr}
	}

	// Rebind the "self" sentinel on either endpoint to the concrete principal
	// UID (#209 Part F) so a CLI-authored edge references the shareable principal
	// rather than the device-local seeded "self" row, matching the MCP tool.
	subjectUID = rebindSelfToPrincipal(dataDir, subjectUID)
	objectUID = rebindSelfToPrincipal(dataDir, objectUID)

	if gerr := relateTeamSelfGuard(cmd, scope, subjectUID, objectUID); gerr != nil {
		return gerr
	}

	// Validate the predicate name before the tool call so a malformed name is a
	// clean exit 1 with an actionable message. `schema predicates add` enforces
	// the same snake_case shape; the dispatcher now auto-registers an asserted
	// predicate (Change Set 2), so we no longer register it here.
	if !predicateNamePattern.MatchString(predicate) {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"relate: predicate %q must be snake_case (lowercase letters, digits, underscores; start with a letter), e.g. works_at\n", predicate)
		return silentError{code: 1, err: fmt.Errorf("invalid predicate name: %s", predicate)}
	}

	reqBody, berr := buildRelateToolRequest(subjectUID, predicate, objectUID, scope, flags)
	if berr != nil {
		return wrapRelateFatal(cmd, berr)
	}

	// Route through the daemon-or-in-process tool path so a team write lands in
	// the running daemon's event log (where it can be sealed) — or in-process
	// when no daemon serves this data dir. The in-process fallback invokes the
	// same registered MCP relate tool, so behaviour is identical either way.
	// Endpoint existence is enforced by the relate tool.
	//
	// The relate tool registers under "relate" (Relate.Name()), which is NOT
	// ontology.KindRelation ("relation"); the event KIND it appends is the
	// relation kind, but the tool NAME differs.
	raw, err := invokeNavTool(cmd, relateToolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "relate", err)
	}

	var resp struct {
		Name string `json:"name"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapRelateFatal(cmd, fmt.Errorf("parse relation response: %w", uerr))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{
			"name": resp.Name,
		})
	}
	_, err = fmt.Fprintln(w, resp.Name)
	return err
}

// buildRelateToolRequest assembles the JSON request body for the relate tool
// from the parsed CLI arguments. The endpoints and scope are already
// resolved/validated by the caller. Confidence is sent only when it differs
// from the default 1.0, and valid-from/valid-until only when set, so the routed
// payload mirrors what the in-process write used to produce.
func buildRelateToolRequest(subjectUID, predicate, objectUID string, scope kernel.Scope, flags *relateFlags) (json.RawMessage, error) {
	in := map[string]any{
		"subject":   subjectUID,
		"predicate": predicate,
		"object":    objectUID,
		"source":    flags.source,
	}
	if scope != "" {
		in["scope"] = string(scope)
	}
	if flags.confidence != relateDefaultConfidenceCLI {
		in["confidence"] = flags.confidence
	}
	if flags.validFrom != "" {
		in["validFrom"] = flags.validFrom
	}
	if flags.validUntil != "" {
		in["validUntil"] = flags.validUntil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal relation request: %w", err)
	}
	return raw, nil
}

// relateDefaultConfidenceCLI is the relate command's default confidence. The
// value is omitted from the tool request when unchanged so the relate tool
// applies its own default, keeping the event-log row and projection in sync.
const relateDefaultConfidenceCLI = 1.0

// relateToolName is the registered MCP relate tool name (Relate.Name()). It is
// deliberately "relate", not ontology.KindRelation ("relation"): the tool name
// and the event kind it appends differ.
const relateToolName = "relate"

func wrapRelateFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "relate", err)
}

// relateBatchLine is the per-line JSON shape for --batch files. Fields mirror
// the relateBatch MCP tool's per-relation object so the same file works with
// both surfaces.
type relateBatchLine struct {
	Subject    string            `json:"subject"`
	Predicate  string            `json:"predicate"`
	Object     string            `json:"object"`
	Confidence *float64          `json:"confidence,omitempty"`
	ValidFrom  string            `json:"validFrom,omitempty"`
	ValidUntil string            `json:"validUntil,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Provenance string            `json:"provenance,omitempty"`
	Scope      string            `json:"scope,omitempty"`
}

// batchLineToEntry converts a relateBatchLine to the map[string]any shape the
// relateBatch MCP tool expects, applying the fallback CLI scope when the line
// does not specify its own.
func batchLineToEntry(line *relateBatchLine, cliScope string) map[string]any {
	entry := map[string]any{
		"subject":   line.Subject,
		"predicate": line.Predicate,
		"object":    line.Object,
	}
	if line.Confidence != nil {
		entry["confidence"] = *line.Confidence
	}
	if line.ValidFrom != "" {
		entry["validFrom"] = line.ValidFrom
	}
	if line.ValidUntil != "" {
		entry["validUntil"] = line.ValidUntil
	}
	if len(line.Attributes) > 0 {
		entry["attributes"] = line.Attributes
	}
	if line.Provenance != "" {
		entry["provenance"] = line.Provenance
	}
	if line.Scope != "" {
		entry["scope"] = line.Scope
	} else if cliScope != "" {
		entry["scope"] = cliScope
	}
	return entry
}

// parseBatchFile reads flags.batch as JSONL and returns the decoded lines.
// It reports parse errors to cmd.ErrOrStderr and wraps them as silentErrors.
func parseBatchFile(cmd *cobra.Command, path string) ([]relateBatchLine, error) {
	f, err := os.Open(path)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "relate --batch: open %q: %v\n", path, err)
		return nil, silentError{code: 1, err: err}
	}
	defer func() { _ = f.Close() }()

	var lines []relateBatchLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 10<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var line relateBatchLine
		if jerr := json.Unmarshal(raw, &line); jerr != nil {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintf(cmd.ErrOrStderr(), "relate --batch: decode line: %v\n", jerr)
			return nil, silentError{code: 1, err: jerr}
		}
		if line.Subject == "" || line.Predicate == "" || line.Object == "" {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintln(cmd.ErrOrStderr(), "relate --batch: each line must have subject, predicate, and object")
			return nil, silentError{code: 1, err: errors.New("incomplete relation line")}
		}
		lines = append(lines, line)
	}
	if serr := sc.Err(); serr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "relate --batch: read %q: %v\n", path, serr)
		return nil, silentError{code: 1, err: serr}
	}
	if len(lines) == 0 {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "relate --batch: file is empty")
		return nil, silentError{code: 1, err: errors.New("batch file is empty")}
	}
	return lines, nil
}

// runRelateBatch reads the JSONL file at flags.batch, assembles a relateBatch
// MCP request, and dispatches it via the daemon-or-in-process tool path so
// all N edges land in ONE atomic event-log append.
func runRelateBatch(cmd *cobra.Command, flags *relateFlags) error {
	lines, err := parseBatchFile(cmd, flags.batch)
	if err != nil {
		return err
	}

	source := flags.source
	if source == "" || source == "cli:relate" {
		source = "cli:relateBatch"
	}

	rels := make([]map[string]any, 0, len(lines))
	for i := range lines {
		rels = append(rels, batchLineToEntry(&lines[i], flags.scope))
	}

	reqBody, merr := json.Marshal(map[string]any{
		"relations": rels,
		"source":    source,
	})
	if merr != nil {
		return wrapRelateFatal(cmd, fmt.Errorf("marshal batch request: %w", merr))
	}

	raw, err := invokeNavTool(cmd, "relateBatch", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "relate --batch", err)
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, err = fmt.Fprintln(w, string(raw))
		return err
	}
	var resp struct {
		Count int      `json:"count"`
		Names []string `json:"names"`
	}
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return wrapRelateFatal(cmd, fmt.Errorf("parse batch response: %w", uerr))
	}
	_, err = fmt.Fprintf(w, "asserted %d relation(s)\n", resp.Count)
	return err
}
