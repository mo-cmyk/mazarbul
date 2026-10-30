package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// errNavUnavailableMsg is emitted when a navigation tool is absent from the
// wired tool slice — i.e. storage is not the production dispatcher (e.g.
// minimal boot). The navigation verbs are LLM-free and do not need an embedder,
// so this is rare in practice.
const errNavUnavailableMsg = `navigation tools not available (minimal boot or non-dispatcher storage)`

// Relation validity states shown in the `maz relations` text rendering.
const (
	relationStateOpen   = "open"
	relationStateClosed = "closed"
)

// navFilterFlags are the temporal + predicate knobs shared by `neighbors` and
// `relations`, mirroring the MCP relationFilterInput shape.
type navFilterFlags struct {
	predicates      []string
	at              string
	asOf            string
	includeInactive bool
	limit           int
	scopes          []string
}

func (f *navFilterFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&f.predicates, "predicate", nil, "restrict to this predicate (repeatable), e.g. --predicate works_at")
	cmd.Flags().StringVar(&f.at, "at", "", "valid-time anchor (RFC 3339 UTC); defaults to now")
	cmd.Flags().StringVar(&f.asOf, "as-of", "", "transaction-time anchor (RFC 3339 UTC); hide edges recorded after this instant")
	cmd.Flags().BoolVar(&f.includeInactive, "include-inactive", false, "include superseded / expired edges (full history)")
	cmd.Flags().IntVar(&f.limit, "limit", 50, "maximum rows to return")
	cmd.Flags().StringArrayVar(&f.scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
}

// validateScopes pre-validates the --scope values client-side and returns a
// silentError with code 1 if any value is unrecognised. The daemon is the
// authority; this gives a fast, clean error before a round-trip.
func (f *navFilterFlags) validateScopes(cmd *cobra.Command, tool string) error {
	return validateScopeFlag(cmd, tool, f.scopes)
}

// validateScopeFlag is the shared client-side --scope check for navigation
// commands that do not carry a full navFilterFlags (get, entities). Empty is
// allowed (the daemon applies DefaultReadSet); an unrecognised value fails fast.
func validateScopeFlag(cmd *cobra.Command, tool string, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	if _, err := kernel.ParseScopeSet(scopes); err != nil {
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "%s: --scope: %v\n", tool, err); werr != nil {
			return werr
		}
		return silentError{code: 1, err: err}
	}
	return nil
}

// apply folds the set filter flags into the tool-input map.
func (f *navFilterFlags) apply(in map[string]any) {
	if len(f.predicates) > 0 {
		in["predicates"] = f.predicates
	}
	if f.at != "" {
		in["at"] = f.at
	}
	if f.asOf != "" {
		in["asOf"] = f.asOf
	}
	if f.includeInactive {
		in["includeInactive"] = true
	}
	if f.limit > 0 {
		in["limit"] = f.limit
	}
	if len(f.scopes) > 0 {
		in["scopes"] = f.scopes
	}
}

// invokeNavTool dispatches a navigation tool by name through the daemon (when
// running) or an in-process wiring, returning the raw JSON result. A nil result
// means the tool was not registered; callers surface errNavUnavailableMsg.
func invokeNavTool(cmd *cobra.Command, toolName string, reqBody json.RawMessage) (json.RawMessage, error) {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	return runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger,
		toolName, reqBody,
		func(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, req json.RawMessage) (json.RawMessage, error) {
			return invokeNamedTool(ctx, wired, toolName, req)
		},
	)
}

// isExpectedNavError reports whether err is an E_NOT_FOUND, E_VALIDATION, or
// E_PERMISSION error from the navigation tool layer. These are expected
// failures (missing resource, bad input, or an unauthorized team-scope write by
// a non-member) that should exit 1, not 2.
func isExpectedNavError(err error) bool {
	if err == nil {
		return false
	}
	switch toolErrorCode(err) {
	case string(mcp.CodeNotFound), string(mcp.CodeValidation), string(mcp.CodePermission):
		return true
	default:
		return false
	}
}

// wrapNavFatal reports a navigation-tool failure: it prints err to stderr under
// the tool name and, in JSON/NDJSON mode, the shared {error,code} envelope to
// stdout. E_NOT_FOUND / E_VALIDATION / E_PERMISSION are expected failures (bad
// input / resource missing / unauthorized) and exit 1; all others exit 2.
func wrapNavFatal(cmd *cobra.Command, tool string, err error) error {
	return respondError(cmd, tool, err)
}

// emitNavResult handles the JSON-mode passthrough + the nil (unavailable)
// case shared by every navigation command. It returns (handled, error): when
// handled is true the caller is done; otherwise it should render raw itself.
// NDJSON is intentionally NOT handled here — commands that have array fields
// to stream must handle FormatNDJSON before calling emitNavResult so they can
// emit one object per element rather than a single blob.
func emitNavResult(cmd *cobra.Command, raw json.RawMessage) (bool, error) {
	if raw == nil {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg); werr != nil {
			return true, werr
		}
		return true, silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return true, err
	}
	// csv/kv: emit the first top-level array field as individual records, or
	// fall through to text rendering when no array is found.
	if wr.Format() == output.FormatCSV || wr.Format() == output.FormatKV {
		handled, err := emitNavResultTabular(wr, raw)
		if handled {
			return true, err
		}
	}
	return false, nil
}

// emitNavResultTabular handles csv/kv rendering for nav-tool responses.
// It tries to find the first top-level JSON array field in raw and emits
// each element as a record. Returns (false, nil) when no array is found so
// the caller can fall through to text rendering.
func emitNavResultTabular(wr output.Writer, raw json.RawMessage) (bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false, nil //nolint:nilerr // non-object raw → not an array; fall through to text rendering
	}
	// Walk the top-level keys in sorted order so the behaviour is deterministic.
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		var arr []json.RawMessage
		if err := json.Unmarshal(top[k], &arr); err == nil && arr != nil {
			for _, item := range arr {
				var m map[string]any
				if err2 := json.Unmarshal(item, &m); err2 == nil {
					if err3 := wr.Emit(m); err3 != nil {
						return true, err3
					}
				}
			}
			return true, wr.Flush()
		}
	}
	return false, nil
}

// ---- get ----

func newGetCmd() *cobra.Command {
	var scopes []string
	var at, asOf string
	var includeInactive bool
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Fetch a record by resource name, entity display name, or uid",
		Long: `Fetch a fully hydrated record. Accepts an AIP resource name whose collection
prefix routes the lookup (entities/{uid}, episodes/{uid}, relations/{uid},
facts/{uid}, …), OR — for an entity — a bare uid or a human display name, the
same forms show/relations/neighbors accept. An ambiguous or unknown name returns
a pointed error naming the candidates.

This is the LLM-free way to follow a recall hit's entity anchor or citation to
the full record. Pair with neighbors and relations to traverse the graph.

For an entity, --at / --as-of / --include-inactive time-travel its fact rows:
--at returns each slot's value live at an instant, --as-of hides facts recorded
after an instant, and --include-inactive returns the full slot history. They
narrow ONLY the entity's facts[]; the identity row (uid, kind, displayName) is
unchanged, and they are ignored for non-entity records.

JSON shape (--output=json): a type-discriminated union object:
  {"type": "<kind>", "<kind>": { ...payload fields... }}
where <kind> is one of: entity, relation, episode, fact.
Entity results also carry "facts[]" (the entity's current fact slots) and
"provenance[]" (origin episodes). Relation-qualifying attributes (role, since,
confidence, ...) live on the relation object itself; they are NOT in the
entity's facts[]. Use "maz relations <entity>" to see those attributes.

Examples:
  maz get "Mira Stonebrook"
  maz get entities/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz get entities/01ARZ3NDEKTSV4RRFFQ69G5FAV --scope team
  maz get "Mira Stonebrook" --at 2025-01-01T00:00:00Z
  maz get "Mira Stonebrook" --include-inactive --output=json
  maz recall "Erik" --output=json | jq -r '.hits[0].entities[0].name' | xargs maz get`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGetCmd(cmd, args[0], scopes, at, asOf, includeInactive)
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	cmd.Flags().StringVar(&at, "at", "",
		"valid-time anchor (RFC 3339 UTC) for the entity's fact rows; defaults to now")
	cmd.Flags().StringVar(&asOf, "as-of", "",
		"transaction-time anchor (RFC 3339 UTC); hide facts recorded after this instant")
	cmd.Flags().BoolVar(&includeInactive, "include-inactive", false,
		"include superseded / expired facts (full slot history) on an entity")
	return cmd
}

// runGetCmd implements the `maz get` RunE body. Extracted from newGetCmd to
// keep the command builder within the cognitive complexity budget while
// preserving identical behaviour.
func runGetCmd(cmd *cobra.Command, arg string, scopes []string, at, asOf string, includeInactive bool) error {
	if err := validateScopeFlag(cmd, "get", scopes); err != nil {
		return err
	}
	// Convenience: `maz get self` is the natural way to fetch one's own
	// identity, but the get tool's resource-name contract needs a
	// collection prefix. "self" is unambiguously an entity (the bound
	// principal), so promote it to entities/self; the tool's self-binder
	// then resolves it to the principal uid. Bare ULIDs stay verbatim —
	// their collection is ambiguous, so the prefix is required.
	name := arg
	if name == selfEntityUID {
		name = "entities/" + selfEntityUID
	}
	in := map[string]any{"name": name}
	if len(scopes) > 0 {
		in["scopes"] = scopes
	}
	// Bi-temporal anchor for the entity's fact rows. A bad timestamp is
	// validated by the get tool and surfaces as E_VALIDATION (exit 1).
	if at != "" {
		in["at"] = at
	}
	if asOf != "" {
		in["asOf"] = asOf
	}
	if includeInactive {
		in["includeInactive"] = true
	}
	reqBody, err := json.Marshal(in)
	if err != nil {
		return wrapNavFatal(cmd, "get", err)
	}
	raw, err := invokeNavTool(cmd, "get", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, "get", err)
	}
	wr := output.From(cmd)
	// NDJSON: get returns a single object, so emit it as one JSON line.
	if wr.Format() == output.FormatNDJSON {
		return renderGetNDJSON(cmd, raw)
	}
	// csv/kv: emit the payload object (entity/episode/etc.) as a single
	// record rather than letting emitNavResultTabular emit one of the
	// inner array fields (e.g. provenance) which is not what callers want.
	if wr.Format() == output.FormatCSV || wr.Format() == output.FormatKV {
		return renderGetTabular(wr, raw)
	}
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}
	return renderGetCLI(cmd, raw)
}

// renderGetCLI prints the discriminated-union record as aligned key/value rows.
func renderGetCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("get: parse response: %w", err)
	}
	var kind string
	if err := json.Unmarshal(rec["type"], &kind); err != nil {
		return fmt.Errorf("get: parse type discriminator: %w", err)
	}
	body, ok := rec[kind]
	if !ok {
		// Nothing matched the discriminator; echo the raw payload.
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("get: parse %s record: %w", kind, err)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "type\t%s\n", kind); err != nil {
		return err
	}
	for _, key := range sortedKeys(fields) {
		if _, err := fmt.Fprintf(tw, "%s\t%v\n", key, fields[key]); err != nil {
			return err
		}
	}
	if err := renderProvenanceCLI(tw, rec["provenance"]); err != nil {
		return err
	}
	if err := renderShadowedDocsCLI(tw, rec["shadowedDocuments"]); err != nil {
		return err
	}
	return tw.Flush()
}

// renderGetTabular emits the get result as a single structured record for
// csv/kv output. It unwraps the discriminated union and emits the inner
// payload object (entity, episode, relation, or fact), so csv produces a
// header + one data row and kv produces a key:value block. A "type" field is
// prepended so the record kind is visible in the output.
func renderGetTabular(wr output.Writer, raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("get: parse response: %w", err)
	}
	var kind string
	if err := json.Unmarshal(rec["type"], &kind); err != nil {
		return fmt.Errorf("get: parse type discriminator: %w", err)
	}
	body, ok := rec[kind]
	if !ok {
		// Fallback: emit the top-level object without the inner payload.
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("get: parse fallback: %w", err)
		}
		if err := wr.Emit(obj); err != nil {
			return err
		}
		return wr.Flush()
	}
	// Unmarshal the inner payload, inject "type" as the first field.
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("get: parse %s record: %w", kind, err)
	}
	fields["type"] = kind
	if err := wr.Emit(fields); err != nil {
		return err
	}
	return wr.Flush()
}

// renderShadowedDocsCLI surfaces get's top-level `shadowedDocuments` envelope
// field in text mode. It is set when a non-document entity won name resolution
// over one or more same-named documents; renderGetCLI walks only the entity body
// plus provenance, so without this the text renderer would silently drop the
// signal and leave a CLI user unaware a same-named note exists. Each line is the
// document's entities/{uid} name, ready to pass back to `maz get`.
func renderShadowedDocsCLI(tw *tabwriter.Writer, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var docs []struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil || len(docs) == 0 {
		return nil //nolint:nilerr // advisory annotation; never fail a get over a malformed shadow entry
	}
	for _, d := range docs {
		if _, err := fmt.Fprintf(tw, "shadowedDocument\t%s\n", d.Name); err != nil {
			return err
		}
	}
	return nil
}

// renderGetNDJSON emits the get result as a single JSON object on one line,
// satisfying the --output ndjson contract for commands that return a single
// record rather than an array.
func renderGetNDJSON(cmd *cobra.Command, raw json.RawMessage) error {
	if raw == nil {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}

// renderProvenanceCLI appends one row per provenance record — the origin
// episode an entity/relation/episode traces back to. A nil or empty provenance
// is silently skipped: lineage is supplementary, and its absence is normal.
func renderProvenanceCLI(tw *tabwriter.Writer, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var prov []struct {
		EventUID   string  `json:"eventUid"`
		Source     string  `json:"source"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &prov); err != nil {
		return fmt.Errorf("get: parse provenance: %w", err)
	}
	for i, p := range prov {
		if _, err := fmt.Fprintf(tw, "provenance[%d]\tepisodes/%s (%s, confidence %.2f)\n",
			i, p.EventUID, p.Source, p.Confidence); err != nil {
			return err
		}
	}
	return nil
}

// ---- neighbors ----

func newNeighborsCmd() *cobra.Command {
	var depth int
	filter := &navFilterFlags{}
	cmd := &cobra.Command{
		Use:   "neighbors <entity>",
		Short: fmt.Sprintf("Expand an entity's neighborhood up to %d hops (LLM-free)", navigate.MaxHops),
		Long: `Expand an entity's graph neighborhood and return the hydrated neighbor
entities plus the relations connecting them. The seed is an entities/{uid}
resource name (as returned in a recall hit's entities[]) or a bare uid.

Superset of "maz traverse": adds --scope, --include-inactive, --at, --as-of
for scope-filtered, history-aware, and temporal walks. Use "maz traverse" for
quick N-hop walks that do not need those controls.

Examples:
  maz neighbors entities/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz neighbors 01ARZ3... --depth 2 --predicate reports_to --output=json
  maz neighbors 01ARZ3... --scope project --scope personal`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Reject an out-of-range depth client-side (matching maz traverse and
			// maz doc canvas) rather than letting the tool silently clamp it.
			if depth < 1 || depth > navigate.MaxHops {
				//nolint:errcheck // best-effort stderr on fatal path
				fmt.Fprintf(cmd.ErrOrStderr(), "neighbors: --depth must be between 1 and %d (got %d)\n", navigate.MaxHops, depth)
				return silentError{code: 1, err: fmt.Errorf("--depth must be between 1 and %d (got %d)", navigate.MaxHops, depth)}
			}
			if err := filter.validateScopes(cmd, "neighbors"); err != nil {
				return err
			}
			in := map[string]any{"seed": args[0], "depth": depth}
			filter.apply(in)
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "neighbors", err)
			}
			raw, err := invokeNavTool(cmd, "neighbors", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "neighbors", err)
			}
			// NDJSON: emit one JSON object per neighbor before the generic passthrough.
			if output.From(cmd).Format() == output.FormatNDJSON {
				return renderNeighborsNDJSON(cmd, raw)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderNeighborsCLI(cmd, raw, filter.limit)
		},
	}
	cmd.Flags().IntVar(&depth, "depth", 1, fmt.Sprintf("hops to expand (1..%d)", navigate.MaxHops))
	filter.register(cmd)
	return cmd
}

func renderNeighborsCLI(cmd *cobra.Command, raw json.RawMessage, limit int) error {
	var out struct {
		Neighbors []struct {
			Entity    struct{ Name, Kind, DisplayName string } `json:"entity"`
			Via       struct{ Predicate string }               `json:"via"`
			Direction string                                   `json:"direction"`
			Hops      int                                      `json:"hops"`
		} `json:"neighbors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("neighbors: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if len(out.Neighbors) == 0 {
		_, err := fmt.Fprintln(w, "no neighbors")
		return err
	}
	c := cmdColorizer(cmd)
	wr := output.From(cmd)
	if perr := output.MaybePage(cmd, func(pw io.Writer) error {
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, []output.Column{
			{Title: "hops", Align: output.AlignRight},
			{Title: "dir"},
			{Title: "predicate"},
			{Title: "entity"},
			{Title: "name"},
		})
		for _, n := range out.Neighbors {
			tbl.Row(n.Hops, n.Direction, n.Via.Predicate, n.Entity.Name, n.Entity.DisplayName)
		}
		return tbl.Render()
	}); perr != nil {
		return perr
	}
	output.ShowingNofM(wr, c, len(out.Neighbors), 0, limit, "raise --limit")
	return nil
}

// renderNeighborsNDJSON emits one JSON object per neighbor to cmd's stdout.
func renderNeighborsNDJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Neighbors []json.RawMessage `json:"neighbors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("neighbors: parse ndjson response: %w", err)
	}
	w := cmd.OutOrStdout()
	for _, n := range out.Neighbors {
		if _, err := fmt.Fprintln(w, string(n)); err != nil {
			return err
		}
	}
	return nil
}

// ---- relations ----

func newRelationsCmd() *cobra.Command {
	filter := &navFilterFlags{}
	cmd := &cobra.Command{
		Use:   "relations <entity>",
		Short: "List an entity's typed relations (LLM-free)",
		Long: `List the typed relations (incoming and outgoing) of an entity, with
hydrated endpoint names, validity windows, and attributes. The entity is an
entities/{uid} resource name or a bare uid.

By default only relations live now and not superseded are shown; pass
--include-inactive for full history, or --at / --as-of to time-travel.

Examples:
  maz relations entities/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz relations 01ARZ3... --predicate works_at --include-inactive --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{"entity": args[0]}
			filter.apply(in)
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "relations", err)
			}
			raw, err := invokeNavTool(cmd, "relations", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "relations", err)
			}
			// NDJSON: emit one JSON object per relation before the generic passthrough.
			if output.From(cmd).Format() == output.FormatNDJSON {
				return renderRelationsNDJSON(cmd, raw)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderRelationsCLI(cmd, raw, filter.limit)
		},
	}
	filter.register(cmd)
	return cmd
}

func renderRelationsCLI(cmd *cobra.Command, raw json.RawMessage, limit int) error {
	var out struct {
		Relations []struct {
			Predicate   string `json:"predicate"`
			SubjectName string `json:"subjectName"`
			ObjectName  string `json:"objectName"`
			Direction   string `json:"direction"`
			ValidUntil  string `json:"validUntil"`
		} `json:"relations"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("relations: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if len(out.Relations) == 0 {
		_, err := fmt.Fprintln(w, "no relations")
		return err
	}
	c := cmdColorizer(cmd)
	wr := output.From(cmd)
	if perr := output.MaybePage(cmd, func(pw io.Writer) error {
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, []output.Column{
			{Title: "dir"},
			{Title: "subject", MaxWidth: 30, Truncate: true},
			{Title: "predicate"},
			{Title: "object", MaxWidth: 30, Truncate: true},
			{Title: "status"},
		})
		for _, r := range out.Relations {
			status := relationStateOpen
			if r.ValidUntil != "" {
				status = relationStateClosed
			}
			tbl.Row(r.Direction, r.SubjectName, r.Predicate, r.ObjectName, status)
		}
		return tbl.Render()
	}); perr != nil {
		return perr
	}
	output.ShowingNofM(wr, c, len(out.Relations), 0, limit, "raise --limit")
	return nil
}

// renderRelationsNDJSON emits one JSON object per relation to cmd's stdout.
func renderRelationsNDJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Relations []json.RawMessage `json:"relations"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("relations: parse ndjson response: %w", err)
	}
	w := cmd.OutOrStdout()
	for _, r := range out.Relations {
		if _, err := fmt.Fprintln(w, string(r)); err != nil {
			return err
		}
	}
	return nil
}

// ---- entities ----

// entitiesFlags holds the bound flag values for `maz entities`, kept off the
// RunE closure so the map projection (entitiesInput) lives in its own function
// — otherwise the command's cognitive complexity trips the linter.
type entitiesFlags struct {
	kind          string
	nameLike      string
	filter        string
	minConfidence float64
	limit         int
	offset        int
	sortBy        string
	window        string
	scopes        []string
}

// entitiesInput projects the set flags into the entities tool-input map. Changed()
// distinguishes an explicit --flag 0 from the default so the tool's own defaults
// (e.g. limit 50) apply when the flag is unset.
func (f *entitiesFlags) entitiesInput(cmd *cobra.Command) map[string]any {
	in := map[string]any{}
	for key, val := range map[string]string{"kind": f.kind, "nameLike": f.nameLike, "filter": f.filter, "sortBy": f.sortBy, "window": f.window} {
		if val != "" {
			in[key] = val
		}
	}
	if cmd.Flags().Changed("min-confidence") {
		in["minConfidence"] = f.minConfidence
	}
	if cmd.Flags().Changed("limit") {
		in["limit"] = f.limit
	}
	if cmd.Flags().Changed("offset") {
		in["offset"] = f.offset
	}
	if len(f.scopes) > 0 {
		in["scopes"] = f.scopes
	}
	return in
}

func newEntitiesCmd() *cobra.Command {
	f := &entitiesFlags{}
	cmd := &cobra.Command{
		Use:   "entities",
		Short: "List and filter entities (LLM-free discovery)",
		Long: `List and filter entities by kind, name substring, confidence, and an
AIP-160 attribute filter — the LLM-free discovery surface. Find an anchor here,
then pivot into neighbors or relations. --sort-by recallCount returns the
most-recalled entities first.

The --filter expression references entity columns (kind, displayName,
confidence, resolutionStatus, createTime), the short alias "status" for
resolutionStatus (values: resolved, confirmed, tentative, split_candidate,
deleted), an entity's fact slots by state_key, and related("predicate",
"target"). Operators: = != < > <= >= : (substring) with AND / OR / NOT.

Examples:
  maz entities --kind person
  maz entities --name-like chen --min-confidence 0.8
  maz entities --filter 'kind = "person" AND status = "resolved"'
  maz entities --filter 'status != "deleted"'
  maz entities --filter 'related("works_at", "Mithril")' --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateScopeFlag(cmd, "entities", f.scopes); err != nil {
				return err
			}
			reqBody, err := json.Marshal(f.entitiesInput(cmd))
			if err != nil {
				return wrapNavFatal(cmd, "entities", err)
			}
			raw, err := invokeNavTool(cmd, "entities", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "entities", err)
			}
			// NDJSON: emit one JSON object per entity before the generic passthrough.
			if output.From(cmd).Format() == output.FormatNDJSON {
				return renderEntitiesNDJSON(cmd, raw)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderEntitiesCLI(cmd, raw, f.limit)
		},
	}
	cmd.Flags().StringVar(&f.kind, "kind", "", "filter to one entity kind, e.g. person, place, organization")
	cmd.Flags().StringVar(&f.nameLike, "name-like", "", "case-insensitive substring match on display name")
	cmd.Flags().StringVar(&f.filter, "filter", "", `AIP-160 filter, e.g. 'kind = "person" AND status = "pending"' (recent sort only)`)
	cmd.Flags().Float64Var(&f.minConfidence, "min-confidence", 0, "drop entities below this resolution confidence (0-1)")
	cmd.Flags().IntVar(&f.limit, "limit", 50, "maximum entities to return")
	cmd.Flags().IntVar(&f.offset, "offset", 0, "pagination offset (recent sort only)")
	cmd.Flags().StringVar(&f.sortBy, "sort-by", "", "ordering: recent (default) or recallCount")
	cmd.Flags().StringVar(&f.window, "window", "", "window for --sort-by recallCount: today|last7Days|last30Days|lifetime")
	cmd.Flags().StringArrayVar(&f.scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	return cmd
}

func renderEntitiesCLI(cmd *cobra.Command, raw json.RawMessage, limit int) error {
	var out struct {
		Entities []struct {
			Name        string  `json:"name"`
			Kind        string  `json:"kind"`
			DisplayName string  `json:"displayName"`
			Confidence  float64 `json:"confidence"`
			RecallCount *int64  `json:"recallCount"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("entities: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if len(out.Entities) == 0 {
		_, err := fmt.Fprintln(w, "no entities")
		return err
	}
	// The recallCount sort adds a "recalled" column; detect it once so a plain
	// listing stays four columns wide.
	hasRecall := false
	for _, e := range out.Entities {
		if e.RecallCount != nil {
			hasRecall = true
			break
		}
	}
	c := cmdColorizer(cmd)
	wr := output.From(cmd)
	if perr := output.MaybePage(cmd, func(pw io.Writer) error {
		cols := []output.Column{
			{Title: "entity"},
			{Title: "kind"},
			{Title: "name"},
			{Title: "conf", Align: output.AlignRight},
		}
		if hasRecall {
			cols = append(cols, output.Column{Title: "recalled", Align: output.AlignRight})
		}
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, cols)
		for _, e := range out.Entities {
			if hasRecall {
				rc := int64(0)
				if e.RecallCount != nil {
					rc = *e.RecallCount
				}
				tbl.Row(e.Name, e.Kind, e.DisplayName, fmt.Sprintf("%.2f", e.Confidence), rc)
			} else {
				tbl.Row(e.Name, e.Kind, e.DisplayName, fmt.Sprintf("%.2f", e.Confidence))
			}
		}
		return tbl.Render()
	}); perr != nil {
		return perr
	}
	output.ShowingNofM(wr, c, len(out.Entities), 0, limit, fmt.Sprintf("use --offset %d", limit))
	return nil
}

// renderEntitiesNDJSON emits one JSON object per entity to cmd's stdout,
// matching the --output ndjson contract: one parseable JSON record per line.
func renderEntitiesNDJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Entities []json.RawMessage `json:"entities"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("entities: parse ndjson response: %w", err)
	}
	w := cmd.OutOrStdout()
	for _, e := range out.Entities {
		if _, err := fmt.Fprintln(w, string(e)); err != nil {
			return err
		}
	}
	return nil
}

// ---- introspect ----

func newIntrospectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "introspect",
		Short: "Show the ontology you navigate by: predicates, entity kinds, event types",
		Long: `Print the navigable ontology: the registered relation predicates (edge
vocabulary, with functional/non-functional semantics), the entity kinds present
in the store (node vocabulary with counts), and the recorded event types.

Use it to learn the vocabulary before filtering neighbors/relations/entities.

Examples:
  maz introspect
  maz introspect --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := invokeNavTool(cmd, "introspect", json.RawMessage(`{}`))
			if err != nil {
				return wrapNavFatal(cmd, "introspect", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderIntrospectCLI(cmd, raw)
		},
	}
}

func renderIntrospectCLI(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Predicates []struct {
			Predicate, Kind, Description string
		} `json:"predicates"`
		EntityKinds []struct {
			Kind  string
			Count int
		} `json:"entityKinds"`
		EventTypes []struct {
			Kind, Version string
		} `json:"eventTypes"`
		Views []introspectViewLine `json:"views"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("introspect: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "predicate"},
		{Title: "kind"},
		{Title: "description"},
	})
	for _, p := range out.Predicates {
		tbl.Row(p.Predicate, p.Kind, p.Description)
	}
	if err := tbl.Render(); err != nil {
		return err
	}
	kinds := make([]string, 0, len(out.EntityKinds))
	for _, k := range out.EntityKinds {
		kinds = append(kinds, fmt.Sprintf("%s(%d)", k.Kind, k.Count))
	}
	events := make([]string, 0, len(out.EventTypes))
	for _, e := range out.EventTypes {
		events = append(events, e.Kind)
	}
	if _, err := fmt.Fprintf(w, "\nentity kinds: %s\n", strings.Join(kinds, ", ")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event types:  %s\n", strings.Join(events, ", ")); err != nil {
		return err
	}
	return renderIntrospectViewsCLI(w, out.Views)
}

// introspectViewLine decodes one curated query view from the introspect
// response for the text rendering.
type introspectViewLine struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Columns     []struct {
		Name string `json:"name"`
	} `json:"columns"`
}

// renderIntrospectViewsCLI lists the curated query views and their columns — the
// schema an agent (or a human writing `maz query`) targets.
func renderIntrospectViewsCLI(w io.Writer, views []introspectViewLine) error {
	if len(views) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\nquery views (maz query):\n"); err != nil {
		return err
	}
	for _, v := range views {
		cols := make([]string, 0, len(v.Columns))
		for _, c := range v.Columns {
			cols = append(cols, c.Name)
		}
		if _, err := fmt.Fprintf(w, "  %s(%s)\n", v.Name, strings.Join(cols, ", ")); err != nil {
			return err
		}
	}
	return nil
}

// cmdColorizer returns a Colorizer gated by whether cmd's stdout accepts
// ANSI styling. Callers pass this to output.NewTable so themed headers and
// dim ellipses are automatic without raw escapes.
func cmdColorizer(cmd *cobra.Command) *colorizer.Colorizer {
	return colorizer.New(interactive.ShouldStyle(cmd, cmd.OutOrStdout()), cmd.OutOrStdout())
}

// sortedKeys returns the map keys in deterministic order for stable rendering.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
