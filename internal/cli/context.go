package cli

// context.go — `maz context`: assemble the cold-start working-set brief from the
// unified graph (pinned + recent documents, key entities, optional query-relevant
// hits), and persist it as a real versioned document. Both subcommands route
// through the same daemon-or-in-process tool dispatch as the navigation verbs;
// `build` calls getContext, `index` calls getContext then writeDocument.
//
// hot.md vs index.md:
//
//   - hot.md  = the token-budgeted BRIEF (getContext / contextbrief.Build
//     output). Priority order: pinned → recent → entities → query-relevant.
//     This is the "hot cache" an agent loads at session open to warm up context.
//   - index.md = the master vault DIRECTORY: a folder-grouped listing of every
//     vault document, a recently-updated section, and a key-entity roll-up.
//     Not budget-gated; always enumeration-complete. Human-navigable navigation
//     aid, not a working-set summary.
//
// `maz context index` regenerates BOTH, writing each as a real versioned vault
// document via writeDocument (content-hash idempotent — no-op revision when
// unchanged).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/tui/style/mdrender"
)

// newContextCmd is the `maz context` group.
func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Assemble and persist the cold-start context brief",
		Long: `Assemble a token-budgeted markdown brief of the working set — pinned and
recently-updated documents, key entities, and (with --query) the most relevant
hits — for loading at the start of a session.

The brief is deterministic and LLM-free; it never exceeds the token budget and
drops the lowest-priority tail behind a visible marker.

Bare ` + "`maz context`" + ` prints the default brief followed by a 📌 section of
your active steering pins and corrections. Steer the brief with:

  maz context note "<text>"        pin a free-text steering note (leads the brief)
  maz context correct <id> "<text>" override a wrong surfaced item
  maz context unpin <id>           retire a pin or correction

(These are distinct from ` + "`maz context pin <documentUid>`" + `, which pins a vault
document.)

Examples:
  maz context
  maz context --json
  maz context note "Mid-migration off Postgres — don't suggest new Postgres features"`,
		// Bare `maz context` renders the brief + active steering pins; an unknown
		// subcommand still errors via NoArgs rather than silently succeeding.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runContextShow(cmd) },
	}
	cmd.AddCommand(newContextBuildCmd())
	cmd.AddCommand(newContextIndexCmd())
	cmd.AddCommand(newContextPinCmd())
	cmd.AddCommand(newContextNoteCmd())
	cmd.AddCommand(newContextUnpinCmd())
	cmd.AddCommand(newContextCorrectCmd())
	return cmd
}

// contextBriefResponse mirrors the getContext tool output.
type contextBriefResponse struct {
	Markdown string `json:"markdown"`
	Tokens   int    `json:"tokens"`
	Budget   int    `json:"budget"`
	Dropped  int    `json:"dropped"`
}

// fetchContextBrief invokes getContext and decodes its response.
//
// When emit is true (the `context build` path) it honors the global --json flag
// via emitNavResult: in JSON mode it prints the raw getContext response and
// returns handled=true so the caller stops. When emit is false (the `context
// index` path, which runs further sub-tool calls before producing a single
// final result) it never emits and always decodes, so a JSON-mode invocation
// still proceeds to the writes.
func fetchContextBrief(cmd *cobra.Command, label, query string, budget int, scopes []string, emit bool) (resp contextBriefResponse, handled bool, err error) {
	if verr := validateScopeFlag(cmd, label, scopes); verr != nil {
		return resp, false, verr
	}
	in := map[string]any{}
	if query != "" {
		in["query"] = query
	}
	if budget > 0 {
		in["budgetTokens"] = budget
	}
	if len(scopes) > 0 {
		in["scopes"] = scopes
	}
	reqBody, mErr := json.Marshal(in)
	if mErr != nil {
		return resp, false, wrapNavFatal(cmd, label, mErr)
	}
	raw, iErr := invokeNavTool(cmd, "getContext", reqBody)
	if iErr != nil {
		return resp, false, wrapNavFatal(cmd, label, iErr)
	}
	if emit {
		if h, hErr := emitNavResult(cmd, raw); h {
			return resp, true, hErr
		}
	} else if raw == nil {
		// emit=false skips the emitNavResult unavailable branch, so guard the
		// nil (tool-not-registered) case explicitly.
		return resp, false, wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	if uErr := json.Unmarshal(raw, &resp); uErr != nil {
		return resp, false, fmt.Errorf("%s: parse response: %w", label, uErr)
	}
	return resp, false, nil
}

// ---- context build ----

func newContextBuildCmd() *cobra.Command {
	var (
		query    string
		budget   int
		out      string
		scopes   []string
		sinceStr string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Assemble the context brief and print it (or write it to a file)",
		Long: `Assemble the working-set brief and print it to stdout, or write it to a file
with -o. Pass --query to add a section of query-relevant hits. Pass --since to
surface only entities created and documents updated after a given time.

In JSON mode (stdout piped, --json, or --output json) the output shape is:

  {"brief":{"markdown":"...","tokens":N,"budget":N,"dropped":N},"pins":[...]}

This matches bare ` + "`maz context --json`" + ` so both commands are interchangeable
in scripts. The --since path emits a different shape (see --since help). The
--out path writes raw markdown to the file and is unaffected by --json.

Examples:
  maz context build
  maz context build --json
  maz context build --query "Q3 launch" --budget 8000
  maz context build --scope personal --scope team
  maz context build --since 7d
  maz context build --since 2025-06-01T00:00:00Z
  maz context build -o /tmp/context.md`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runContextBuild(cmd, query, budget, scopes, sinceStr, out)
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "add a section of hits relevant to this query")
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for the brief (default 4000)")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil,
		"scope to read from (repeatable): project, personal, team. Default: project, personal.")
	cmd.Flags().StringVar(&sinceStr, "since", "",
		"only surface entities created and documents updated after this time (duration like 7d or RFC 3339 timestamp)")
	// --out (not --output): the global persistent --output selects the format
	// (text|json|ndjson). A local --output here shadowed it, so `--output text`
	// silently wrote a file literally named "text" and `-o FILE` collided with
	// the format flag. --out/-o is the file-path escape hatch; --output stays the
	// format selector.
	cmd.Flags().StringVarP(&out, "out", "o", "", "write the brief to this file instead of stdout")
	return cmd
}

// runContextBuild implements the `maz context build` RunE body. Extracted from
// newContextBuildCmd to keep the command builder within the cognitive complexity
// budget while preserving identical behaviour.
func runContextBuild(cmd *cobra.Command, query string, budget int, scopes []string, sinceStr, out string) error {
	// --since mode: build a delta brief of new entities + updated docs.
	if sinceStr != "" {
		sinceTime, err := ParseSince(sinceStr)
		if err != nil {
			if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "context build: --since: %v\n", err); werr != nil {
				return werr
			}
			return silentError{code: 1, err: err}
		}
		return runContextBuildSince(cmd, sinceTime, scopes, out)
	}
	// Always decode the brief internally (emit=false). The JSON path is
	// handled explicitly below so it emits contextShowOutput{Brief,Pins}
	// — the same shape as bare `maz context --json` — instead of the
	// raw contextBriefResponse that the old emit=true short-circuit would
	// have printed.
	resp, _, err := fetchContextBrief(cmd, "context build", query, budget, scopes, false)
	if err != nil {
		return err
	}
	if out != "" {
		if werr := os.WriteFile(out, []byte(resp.Markdown), 0o600); werr != nil {
			return wrapNavFatal(cmd, "context build", werr)
		}
		_, perr := fmt.Fprintf(cmd.ErrOrStderr(),
			"wrote %s (%d tokens, budget %d, %d dropped)\n", out, resp.Tokens, resp.Budget, resp.Dropped)
		return perr
	}
	return emitContextBuildResult(cmd, resp)
}

// emitContextBuildResult writes the context brief to stdout in the requested
// format (JSON/NDJSON envelope or raw markdown). Extracted from runContextBuild
// to reduce its cognitive complexity.
func emitContextBuildResult(cmd *cobra.Command, resp contextBriefResponse) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		pins, perr := fetchContextPins(cmd, "context build")
		if perr != nil {
			return perr
		}
		raw, merr := json.Marshal(contextShowOutput{Brief: resp, Pins: pins})
		if merr != nil {
			return wrapNavFatal(cmd, "context build", merr)
		}
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return werr
	}
	_, werr := fmt.Fprint(cmd.OutOrStdout(), resp.Markdown)
	return werr
}

// ---- context build --since ----

// contextSinceEntity is the JSON shape for one new entity in a since-mode brief.
type contextSinceEntity struct {
	UID         string `json:"uid"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	CreateTime  string `json:"createTime,omitempty"`
}

// contextSinceDocument is the JSON shape for one updated document in a since-mode brief.
type contextSinceDocument struct {
	DocumentUID string `json:"documentUid"`
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
	UpdateTime  string `json:"updateTime,omitempty"`
}

// contextSinceLimit caps how many entities and documents the --since query
// pulls per section. It is a server-side page size, not a guarantee of
// completeness: when a section returns a full page the brief flags it as
// truncated (JSON "truncated" + a text footer) so a wide --since window never
// silently under-delivers.
const contextSinceLimit = 200

// contextSinceOutput is the --output json shape for `maz context build --since`.
type contextSinceOutput struct {
	// Since is the RFC 3339 lower bound applied to the query.
	Since string `json:"since"`
	// Entities are entities whose createTime is at or after Since.
	Entities []contextSinceEntity `json:"entities"`
	// Documents are documents whose updateTime (or createTime) is at or after Since.
	Documents []contextSinceDocument `json:"documents"`
	// Truncated is true when either section hit contextSinceLimit, meaning more
	// matching entities or documents exist beyond what is shown.
	Truncated bool `json:"truncated"`
	// Markdown is the assembled delta brief.
	Markdown string `json:"markdown"`
}

// runContextBuildSince implements `maz context build --since`: it fetches new
// entities (via the `entities` tool with an AIP-160 createTime filter) and
// recently updated documents (via `listDocuments`, filtered client-side), then
// assembles a delta brief. The MCP schema for getContext does not carry a since
// parameter, so this uses the lower-level navigation tools instead.
func runContextBuildSince(cmd *cobra.Command, since time.Time, scopes []string, filePath string) error {
	const label = "context build"
	if verr := validateScopeFlag(cmd, label, scopes); verr != nil {
		return verr
	}
	sinceRFC := since.UTC().Format(time.RFC3339)

	entities, entTrunc, err := fetchEntitiesSince(cmd, label, sinceRFC, scopes)
	if err != nil {
		return err
	}
	documents, docTrunc, err := fetchDocsSince(cmd, label, since)
	if err != nil {
		return err
	}

	md := buildSinceBrief(sinceRFC, entities, documents)
	return emitSinceBrief(cmd, label, sinceRFC, entities, documents, md, filePath, entTrunc || docTrunc)
}

// fetchEntitiesSince calls the `entities` tool with an AIP-160 createTime
// filter and returns the decoded entity list. The bool is true when the result
// hit contextSinceLimit (more matching entities exist beyond what is returned).
func fetchEntitiesSince(cmd *cobra.Command, label, sinceRFC string, scopes []string) ([]contextSinceEntity, bool, error) {
	entIn := map[string]any{
		"filter": `createTime > "` + sinceRFC + `"`,
		"limit":  contextSinceLimit,
	}
	if len(scopes) > 0 {
		entIn["scopes"] = scopes
	}
	entBody, mErr := json.Marshal(entIn)
	if mErr != nil {
		return nil, false, wrapNavFatal(cmd, label, mErr)
	}
	entRaw, iErr := invokeNavTool(cmd, "entities", entBody)
	if iErr != nil {
		return nil, false, wrapNavFatal(cmd, label, iErr)
	}
	if entRaw == nil {
		return nil, false, wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	var out struct {
		Entities []struct {
			UID         string `json:"uid"`
			Kind        string `json:"kind"`
			DisplayName string `json:"displayName"`
			CreateTime  string `json:"createTime"`
		} `json:"entities"`
	}
	if uerr := json.Unmarshal(entRaw, &out); uerr != nil {
		return nil, false, fmt.Errorf("%s: parse entities response: %w", label, uerr)
	}
	result := make([]contextSinceEntity, 0, len(out.Entities))
	for _, e := range out.Entities {
		result = append(result, contextSinceEntity{
			UID: e.UID, Kind: e.Kind,
			DisplayName: e.DisplayName, CreateTime: e.CreateTime,
		})
	}
	// The server filters by createTime, so a full page means more matched.
	return result, len(out.Entities) >= contextSinceLimit, nil
}

// fetchDocsSince calls `listDocuments` (top contextSinceLimit, most-recently-
// updated first) and returns only documents whose updateTime (or createTime) is
// at or after since, filtered client-side. The bool is true when the page was
// full AND every returned doc fell within the window, meaning older matching
// documents beyond the cap were dropped.
func fetchDocsSince(cmd *cobra.Command, label string, since time.Time) ([]contextSinceDocument, bool, error) {
	docBody, mErr := json.Marshal(map[string]any{"limit": contextSinceLimit})
	if mErr != nil {
		return nil, false, wrapNavFatal(cmd, label, mErr)
	}
	docRaw, iErr := invokeNavTool(cmd, "listDocuments", docBody)
	if iErr != nil {
		return nil, false, wrapNavFatal(cmd, label, iErr)
	}
	if docRaw == nil {
		return nil, false, wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	var out struct {
		Documents []struct {
			DocumentUID string `json:"documentUid"`
			Path        string `json:"path"`
			Title       string `json:"title"`
			UpdateTime  string `json:"updateTime"`
			CreateTime  string `json:"createTime"`
		} `json:"documents"`
	}
	if uerr := json.Unmarshal(docRaw, &out); uerr != nil {
		return nil, false, fmt.Errorf("%s: parse documents response: %w", label, uerr)
	}
	result := make([]contextSinceDocument, 0)
	for _, d := range out.Documents {
		ut := parseSinceDocTime(d.UpdateTime, d.CreateTime)
		if ut.IsZero() || !ut.Before(since) {
			result = append(result, contextSinceDocument{
				DocumentUID: d.DocumentUID,
				Path:        d.Path,
				Title:       d.Title,
				UpdateTime:  d.UpdateTime,
			})
		}
	}
	// listDocuments returns documents most-recently-updated first (a guarantee
	// of its output schema; there is no sort parameter to pass). Given that
	// order, a full page where EVERY doc passed the since filter means the
	// oldest returned doc is still inside the window — so the cap, not the
	// filter, bounded the result and older matching docs were dropped. (If the
	// page held any out-of-window doc, len(result) < len(out.Documents) and the
	// window, not the cap, was the limit.)
	truncated := len(out.Documents) >= contextSinceLimit && len(result) == len(out.Documents)
	return result, truncated, nil
}

// emitSinceBrief writes the delta brief to stdout (or a file) in the format
// selected by --output (JSON / text). It is split out of runContextBuildSince
// to keep each function's cognitive complexity in range.
func emitSinceBrief(cmd *cobra.Command, label, sinceRFC string, entities []contextSinceEntity, documents []contextSinceDocument, md, filePath string, truncated bool) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		raw, merr := json.Marshal(contextSinceOutput{
			Since:     sinceRFC,
			Entities:  entities,
			Documents: documents,
			Truncated: truncated,
			Markdown:  md,
		})
		if merr != nil {
			return wrapNavFatal(cmd, label, merr)
		}
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return werr
	}
	if filePath != "" {
		if werr := os.WriteFile(filePath, []byte(md), 0o600); werr != nil {
			return wrapNavFatal(cmd, label, werr)
		}
		if _, perr := fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", filePath); perr != nil {
			return perr
		}
		emitSinceTruncationFooter(cmd, truncated)
		return nil
	}
	rendered := mdrender.RenderForWriter(cmd.OutOrStdout(), md)
	if _, werr := fmt.Fprint(cmd.OutOrStdout(), rendered); werr != nil {
		return werr
	}
	emitSinceTruncationFooter(cmd, truncated)
	return nil
}

// emitSinceTruncationFooter prints a warn footer to stderr when a --since
// section hit contextSinceLimit, so a wide window never silently under-delivers.
// It is a no-op when nothing was truncated. JSON mode skips this (the
// "truncated" field carries the signal in-band).
func emitSinceTruncationFooter(cmd *cobra.Command, truncated bool) {
	if !truncated {
		return
	}
	w := cmd.ErrOrStderr()
	clr := colorizer.New(interactive.ShouldStyle(cmd, w), w)
	msg := fmt.Sprintf("! capped at %d per section — more entities or documents match; narrow --since to see the rest", contextSinceLimit)
	_, _ = fmt.Fprintln(w, clr.Warn(msg)) //nolint:errcheck // best-effort footer
}

// parseSinceDocTime returns the best available timestamp for a document: its
// updateTime when parseable, falling back to createTime, otherwise zero.
func parseSinceDocTime(updateTime, createTime string) time.Time {
	for _, s := range []string{updateTime, createTime} {
		if s == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// buildSinceBrief assembles the delta-brief markdown from new entities and
// recently updated documents.
func buildSinceBrief(sinceRFC string, entities []contextSinceEntity, documents []contextSinceDocument) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Context\n\n_Showing entities created and documents updated since %s._\n", sinceRFC)

	b.WriteString("\n## New entities\n\n")
	if len(entities) == 0 {
		b.WriteString("_No new entities since this time._\n")
	} else {
		for _, e := range entities {
			name := e.DisplayName
			if name == "" {
				name = e.UID
			}
			fmt.Fprintf(&b, "- **%s** (%s) — `entities/%s`\n", name, e.Kind, e.UID)
		}
	}

	b.WriteString("\n## Updated documents\n\n")
	if len(documents) == 0 {
		b.WriteString("_No documents updated since this time._\n")
	} else {
		for _, d := range documents {
			title := d.Title
			if title == "" {
				title = d.Path
			}
			fmt.Fprintf(&b, "- [%s](%s)\n", title, d.Path)
		}
	}
	return b.String()
}

// ---- context index ----

// writeVaultDoc calls writeDocument via the nav-tool dispatcher with the given
// path, body, title, and pin flag. Unlike a top-level nav command it does NOT
// route the result through emitNavResult: `context index` performs MULTIPLE
// sub-tool calls (the brief fetch + two writes) and must run all of them before
// emitting a single final command result. Passing each sub-call through
// emitNavResult would short-circuit the command in JSON mode after the first
// call and skip the remaining writes. It returns an error only on a genuine
// failure (marshal / dispatch error, or the writeDocument tool being
// unavailable).
func writeVaultDoc(cmd *cobra.Command, label, path, title, body string, pin bool) error {
	payload := map[string]any{
		"path":             path,
		"body":             body,
		"title":            title,
		contextPinnedField: pin,
	}
	writeBody, mErr := json.Marshal(payload)
	if mErr != nil {
		return wrapNavFatal(cmd, label, mErr)
	}
	raw, iErr := invokeNavTool(cmd, "writeDocument", writeBody)
	if iErr != nil {
		return wrapNavFatal(cmd, label, iErr)
	}
	// A nil result means the writeDocument tool is not registered (daemon down /
	// minimal boot). The command cannot persist anything; surface it as a fatal
	// error rather than silently claiming success.
	if raw == nil {
		return wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	return nil
}

// fetchIndexMarkdown invokes getIndex (the directory-listing tool) and returns
// its markdown. The index is built in-process via contextbrief.Assembler.
// BuildIndex behind the getIndex MCP tool (registered in wiring.go).
//
// A nil result means the tool is not registered (daemon down / minimal boot).
// `context index` cannot fulfil its contract — regenerate BOTH hot.md and
// index.md — without it, so this is surfaced as a fatal error rather than
// silently skipping the index write and then printing a misleading success line.
//
// Like fetchContextBrief(emit=false) it does NOT route the response through
// emitNavResult: the index command emits a single final result after all
// sub-tool calls, so a JSON-mode invocation must proceed to the write.
func fetchIndexMarkdown(cmd *cobra.Command, label string) (markdown string, err error) {
	raw, iErr := invokeNavTool(cmd, "getIndex", json.RawMessage(`{}`))
	if iErr != nil {
		return "", wrapNavFatal(cmd, label, iErr)
	}
	if raw == nil {
		return "", wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	var resp struct {
		Markdown string `json:"markdown"`
	}
	if uErr := json.Unmarshal(raw, &resp); uErr != nil {
		return "", fmt.Errorf("%s: parse index response: %w", label, uErr)
	}
	return resp.Markdown, nil
}

func newContextIndexCmd() *cobra.Command {
	var (
		query   string
		budget  int
		hotPath string
		idxPath string
		pin     bool
	)
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Regenerate the hot brief (hot.md) and vault directory index (index.md)",
		Long: `Regenerate two vault documents:

  hot.md   — the token-budgeted BRIEF (pinned → recent → entities →
             query-relevant). This is the "hot cache" an agent loads at
             session open to warm up its context quickly.

  index.md — the master vault DIRECTORY: a folder-grouped listing of all
             vault documents, a recently-updated section, and a key-entity
             roll-up. Not budget-gated; always enumeration-complete.

Both are written as real, versioned vault documents via writeDocument.
Content-hash idempotency means a re-run is a no-op when nothing changed.

Examples:
  maz context index
  maz context index --query "Q3 launch" --budget 8000
  maz context index --hot-path working-context.md --index-path toc.md
  maz context index --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			const label = "context index"

			// Each sub-tool call (getContext, getIndex, the two writeDocument
			// calls) is consumed INTERNALLY — never routed through emitNavResult
			// — so a --json invocation runs every write and then emits one final
			// command result, instead of short-circuiting after the first call.

			// Fetch and validate BOTH generated bodies FIRST, before either
			// write. The two context artifacts (hot.md + index.md) must stay in
			// sync: writing hot.md and then discovering getIndex is unavailable
			// would leave a fresh hot cache beside a stale/absent index. Reading
			// is side-effect-free, so a fetch failure aborts cleanly with the
			// vault untouched.

			// ── hot.md: the budgeted brief ──────────────────────────────────
			// nil scopes: the indexed hot brief uses the default read span
			// ({project,personal}); narrow with `maz context build --scope` when
			// a scoped one-off is needed.
			//
			// #210 note: when a preset is active, this brief includes the
			// "Operating instructions" section carrying the verbatim preset body,
			// so it is persisted into hot.md (a git-rendered vault doc). Accepted:
			// hot.md is written at personal scope and the body is the operator's
			// own config (see contextbrief.operatingBlocks' PERSISTENCE NOTE).
			briefResp, _, err := fetchContextBrief(cmd, label, query, budget, nil, false)
			if err != nil {
				return err
			}
			if briefResp.Markdown == "" {
				return wrapNavFatal(cmd, label, errors.New("empty brief"))
			}

			// ── index.md: the vault directory ────────────────────────────────
			indexMD, ierr := fetchIndexMarkdown(cmd, label)
			if ierr != nil {
				return ierr
			}

			// Both bodies are in hand — now persist them. A write failure here
			// is a genuine storage error, not a "one artifact succeeded, the
			// other was never attempted" partial run.
			if werr := writeVaultDoc(cmd, label, hotPath, "Context Hot Cache", briefResp.Markdown, pin); werr != nil {
				return werr
			}
			if werr := writeVaultDoc(cmd, label, idxPath, "Vault Index", indexMD, false); werr != nil {
				return werr
			}

			return emitContextIndexResult(cmd, contextIndexOutput{
				HotPath:   hotPath,
				IndexPath: idxPath,
				Tokens:    briefResp.Tokens,
				Dropped:   briefResp.Dropped,
			})
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "add a section of hits relevant to this query (hot.md only)")
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for the brief in hot.md (default 4000)")
	cmd.Flags().StringVar(&hotPath, "hot-path", "hot.md", "vault-relative path for the hot brief document")
	cmd.Flags().StringVar(&idxPath, "index-path", "index.md", "vault-relative path for the directory index document")
	cmd.Flags().BoolVar(&pin, "pin", false, "mark hot.md pinned (always in the brief)")
	return cmd
}

// contextIndexOutput is the single final result `maz context index` emits after
// regenerating both documents. It is the command's own result — distinct from
// the individual sub-tool (getContext / writeDocument / getIndex) payloads,
// which are consumed internally.
type contextIndexOutput struct {
	// HotPath is the vault-relative path of the regenerated hot brief.
	HotPath string `json:"hotPath"`
	// IndexPath is the vault-relative path of the regenerated directory index.
	IndexPath string `json:"indexPath"`
	// Tokens is the hot brief's token count.
	Tokens int `json:"tokens"`
	// Dropped is the number of hot-brief sections dropped to fit the budget.
	Dropped int `json:"dropped"`
}

// emitContextIndexResult writes the single final command result in JSON or
// plain-text mode, mirroring emitPinResult. In JSON/NDJSON mode (which
// output.From selects automatically when stdout is piped) it prints the JSON
// shape; otherwise it prints the human summary line.
func emitContextIndexResult(cmd *cobra.Command, out contextIndexOutput) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		raw, err := json.Marshal(out)
		if err != nil {
			return wrapNavFatal(cmd, "context index", err)
		}
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return werr
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(),
		"wrote %s (%d tokens, %d dropped); wrote %s\n",
		out.HotPath, out.Tokens, out.Dropped, out.IndexPath)
	return werr
}

// ---- context pin ----

// contextPinOutput is the JSON shape returned by `maz context pin`.
type contextPinOutput struct {
	// Path is the vault-relative document path.
	Path string `json:"path"`
	// Pinned is the resulting pinned state after the operation.
	Pinned bool `json:"pinned"`
	// Changed is false when the document was already in the desired state
	// (idempotent no-op).
	Changed bool `json:"changed"`
	// DocumentUID is the stable document UID.
	DocumentUID string `json:"documentUid,omitempty"`
}

func newContextPinCmd() *cobra.Command {
	var unpin bool
	cmd := &cobra.Command{
		Use:   "pin <path|documentUid>",
		Short: "Pin (or unpin) a vault document so it leads the context brief",
		Long: `Mark an existing vault document pinned so it always leads the context brief
(hot.md). Pinned documents surface first in getContext / maz context build.
Use --unpin to clear the flag.

The operation is IDEMPOTENT: pinning an already-pinned document (or unpinning
one that is not pinned) is a clean success with changed=false — no spurious
revision is appended to the event log.

The mechanism is a read-then-write: the current body is preserved VERBATIM
(including its own frontmatter when it has one); only the pinned flag is
toggled. The write goes through writeDocument with the pinned field synthesized
into the frontmatter — identical to what maz doc new --pin does.

Examples:
  maz context pin notes/project-alpha.md
  maz context pin 01ARZ3NDEKTSV4RRFFQ69G5FAV --unpin
  maz context pin hot.md --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runContextPin(cmd, args[0], !unpin)
		},
	}
	cmd.Flags().BoolVar(&unpin, "unpin", false, "clear the pinned flag instead of setting it")
	return cmd
}

// pinDocInfo is the minimal document shape decoded from a readDocument response.
type pinDocInfo struct {
	Path        string `json:"path"`
	Body        string `json:"body"`
	Pinned      bool   `json:"pinned"`
	DocumentUID string `json:"documentUid"`
}

// buildPinReadInput maps a pin ref to the readDocument request body. A
// ULID-shaped ref becomes a documentUid lookup; everything else is treated as a
// vault path and normalised through CleanVaultPath — readDocument looks up the
// EXACT stored path and does not apply CleanVaultPath itself, so without this
// `maz context pin inbox` would miss the stored `inbox.md`. Normalising here
// makes pin accept the same path forms as the write/edit/move document flows
// (e.g. extensionless names and back-slash separators).
func buildPinReadInput(ref string) (map[string]any, error) {
	if !looksLikePath(ref) {
		return map[string]any{"documentUid": ref}, nil
	}
	rel, err := document.CleanVaultPath(ref)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": rel}, nil
}

// readPinDoc fetches the document at ref (path or UID) via the readDocument
// tool and returns the fields needed for a pin toggle. The sub-tool result is
// consumed INTERNALLY — never routed through emitNavResult — so a --json pin
// invocation does not short-circuit on the read and instead reaches the single
// final contextPinOutput emit. A nil result (tool unavailable) is a fatal error.
func readPinDoc(cmd *cobra.Command, label, ref string) (pinDocInfo, error) {
	readInput, cErr := buildPinReadInput(ref)
	if cErr != nil {
		return pinDocInfo{}, wrapNavFatal(cmd, label, cErr)
	}
	readBody, mErr := json.Marshal(readInput)
	if mErr != nil {
		return pinDocInfo{}, wrapNavFatal(cmd, label, mErr)
	}
	readRaw, rErr := invokeNavTool(cmd, "readDocument", readBody)
	if rErr != nil {
		return pinDocInfo{}, wrapNavFatal(cmd, label, rErr)
	}
	if readRaw == nil {
		return pinDocInfo{}, wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	var resp struct {
		Document pinDocInfo `json:"document"`
	}
	if uErr := json.Unmarshal(readRaw, &resp); uErr != nil {
		return pinDocInfo{}, fmt.Errorf("%s: parse readDocument response: %w", label, uErr)
	}
	return resp.Document, nil
}

// writePinDoc applies the pin toggle to doc and calls writeDocument. Like
// readPinDoc it consumes the sub-tool result internally rather than routing it
// through emitNavResult: the pin command emits a single final contextPinOutput,
// so a --json invocation must NOT short-circuit on the writeDocument response
// (which would emit the wrong schema). A nil result (tool unavailable) is fatal.
func writePinDoc(cmd *cobra.Command, label string, doc pinDocInfo, wantPinned bool) error {
	writePayload, cErr := composePinWrite(doc.Path, doc.Body, wantPinned)
	if cErr != nil {
		return wrapNavFatal(cmd, label, cErr)
	}
	writeBody, mErr := json.Marshal(writePayload)
	if mErr != nil {
		return wrapNavFatal(cmd, label, mErr)
	}
	writeRaw, wErr := invokeNavTool(cmd, "writeDocument", writeBody)
	if wErr != nil {
		return wrapNavFatal(cmd, label, wErr)
	}
	if writeRaw == nil {
		return wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	return nil
}

// runContextPin implements the `maz context pin` logic: read → idempotency
// check → write. Extracted from the RunE closure to keep gocognit in range.
// Every sub-tool call is consumed internally so the command emits exactly one
// contextPinOutput — the documented {path,pinned,changed,documentUid} shape —
// in both plain-text and JSON modes.
func runContextPin(cmd *cobra.Command, ref string, wantPinned bool) error {
	const label = "context pin"

	doc, err := readPinDoc(cmd, label, ref)
	if err != nil {
		return err
	}

	// Idempotency: already in the desired state — no-op, no new revision.
	if doc.Pinned == wantPinned {
		return emitPinResult(cmd, contextPinOutput{
			Path: doc.Path, Pinned: doc.Pinned,
			Changed: false, DocumentUID: doc.DocumentUID,
		})
	}

	if werr := writePinDoc(cmd, label, doc, wantPinned); werr != nil {
		return werr
	}
	return emitPinResult(cmd, contextPinOutput{
		Path: doc.Path, Pinned: wantPinned,
		Changed: true, DocumentUID: doc.DocumentUID,
	})
}

// looksLikePath reports whether ref should be treated as a vault-relative
// document path rather than a document UID. The discriminant is positive on the
// UID side: a ref is treated as a UID ONLY when it is shaped exactly like a
// ULID (26 Crockford-base32 characters); EVERYTHING else is a path.
//
// This matters because vault paths can be extensionless and root-level —
// CleanVaultPath appends ".md" for a bare name like "inbox", so "inbox" names
// the document "inbox.md". The earlier slash/dot heuristic misclassified such
// names as UIDs and sent documentUid:"inbox" to readDocument, which then failed
// to find the document. Defaulting non-ULID refs to the path lane fixes that.
func looksLikePath(ref string) bool {
	return !looksLikeULID(ref)
}

// looksLikeULID reports whether ref is shaped like a ULID: exactly 26
// characters drawn from Crockford base32 (digits plus A-Z excluding I, L, O,
// U). ULIDs are conventionally uppercase; we accept either case to be lenient.
func looksLikeULID(ref string) bool {
	const ulidLen = 26
	if len(ref) != ulidLen {
		return false
	}
	for i := range len(ref) {
		if !isCrockfordBase32(ref[i]) {
			return false
		}
	}
	return true
}

// isCrockfordBase32 reports whether c is a Crockford base32 symbol: a digit, or
// a letter (either case) that is not one of the four excluded letters I, L, O,
// U. It is the per-character predicate looksLikeULID applies.
func isCrockfordBase32(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		c -= 'a' - 'A' // fold to upper for the exclusion check
		fallthrough
	case c >= 'A' && c <= 'Z':
		return c != 'I' && c != 'L' && c != 'O' && c != 'U'
	default:
		return false
	}
}

// composePinWrite builds the writeDocument payload for a pin toggle, keeping
// the body verbatim. If the body already has a frontmatter block we patch it
// in-place (or append the field when absent from the block); otherwise we let
// writeDocument synthesize a fresh frontmatter block via the pinned field.
func composePinWrite(path, body string, pinned bool) (map[string]any, error) {
	hasFM := hasFrontmatter(body)
	if !hasFM {
		// No frontmatter: writeDocument will synthesize one.
		return map[string]any{
			"path":             path,
			"body":             body,
			contextPinnedField: pinned,
		}, nil
	}
	// The body has its own frontmatter — writeDocument rejects the pinned field
	// when the body starts with a frontmatter block. Patch the frontmatter in
	// the body directly.
	patched, err := patchFrontmatterPinned(body, pinned)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path": path,
		"body": patched,
	}, nil
}

// hasFrontmatter reports whether body opens with a YAML frontmatter fence.
// Both LF ("---\n") and CRLF ("---\r\n") openers are recognised, matching
// writeDocument's bodyHasFrontmatter: a CRLF markdown file must take the
// patch-in-body path, not the synthesize-from-field path that writeDocument
// would then reject.
func hasFrontmatter(body string) bool {
	return strings.HasPrefix(body, "---\n") || strings.HasPrefix(body, "---\r\n")
}

// patchFrontmatterPinned replaces or inserts "pinned: <value>" inside the
// leading YAML frontmatter block of body, preserving the body's line-ending
// style. The closing "---" delimiter is required; returns an error when the
// block is malformed.
func patchFrontmatterPinned(body string, pinned bool) (string, error) {
	if !hasFrontmatter(body) {
		return "", errors.New("body does not start with frontmatter")
	}
	// The opener is either "---\n" (LF) or "---\r\n" (CRLF). Detect which so the
	// reconstructed block reuses the same line ending for the closing fence.
	open := "---\n"
	eol := "\n"
	if strings.HasPrefix(body, "---\r\n") {
		open = "---\r\n"
		eol = "\r\n"
	}
	rest := body[len(open):]
	end := findFrontmatterEnd(rest)
	if end < 0 {
		return "", errors.New("frontmatter block is not closed (missing closing ---)")
	}
	fmContent := rest[:end]
	after := rest[end+len("---"):]

	newFM := setPinnedInYAML(fmContent, strconv.FormatBool(pinned), eol)
	return open + newFM + "---" + after, nil
}

// findFrontmatterEnd returns the byte offset of the closing fence line within
// s (the frontmatter content after the opening "---\n" / "---\r\n"), or -1 when
// not found. A closing fence is a line whose content is exactly "---",
// tolerating a trailing "\r" from CRLF line endings (mirrors the importer's
// indexClosingFence).
func findFrontmatterEnd(s string) int {
	for i := 0; i < len(s); {
		nl := indexByte(s[i:], '\n')
		var line string
		if nl < 0 {
			line = s[i:]
		} else {
			line = s[i : i+nl]
		}
		if line == "---" || line == "---\r" {
			return i
		}
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	return -1
}

// indexByte is a local copy of strings.IndexByte so this file compiles without
// importing strings in a way that might collide with the package-level import.
func indexByte(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// setPinnedInYAML replaces the "pinned:" line in the YAML content, or appends
// it when not present. The yamlContent argument is the raw YAML between the two
// "---" fences (excluding the fences themselves), and the eol argument is the
// line ending to emit ("\n" or "\r\n"). Each existing line's trailing "\r" is
// normalised away first so the rebuilt block uses eol uniformly.
func setPinnedInYAML(yamlContent, pinnedVal, eol string) string {
	lines := splitLines(yamlContent)
	found := false
	for i, line := range lines {
		// Match "pinned:" at the start of the line (bare key or with value).
		if strings.HasPrefix(line, "pinned:") {
			lines[i] = "pinned: " + pinnedVal
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, "pinned: "+pinnedVal)
	}
	return joinLines(lines, eol)
}

// splitLines splits s into lines on '\n', trimming any trailing '\r' so each
// returned line is line-ending agnostic.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			lines = append(lines, strings.TrimSuffix(s[start:i], "\r"))
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, strings.TrimSuffix(s[start:], "\r"))
	}
	return lines
}

// joinLines rebuilds a YAML block from split lines, terminating each with eol
// (the body's detected line ending).
func joinLines(lines []string, eol string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString(eol)
	}
	return b.String()
}

// contextPinnedField is the "pinned" field name used in writeDocument payloads
// and the action-label string returned by pinActionLabel.
const contextPinnedField = "pinned"

// pinActionLabel returns the human-readable action description for a pin result.
// Extracted so tests can assert on the label without going through output.From.
func pinActionLabel(out contextPinOutput) string {
	action := contextPinnedField
	if !out.Pinned {
		action = "unpinned"
	}
	if !out.Changed {
		action = "already " + action + " (no-op)"
	}
	return action
}

// emitPinResult writes the contextPinOutput in JSON or plain-text mode,
// mirroring the output.From(cmd) pattern used by every other nav command.
// Note: output.From defaults to FormatJSON when stdout is not a TTY (piped),
// so scripted callers always receive JSON without needing --json.
func emitPinResult(cmd *cobra.Command, out contextPinOutput) error {
	raw, err := json.Marshal(out)
	if err != nil {
		return wrapNavFatal(cmd, "context pin", err)
	}
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return werr
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", out.Path, pinActionLabel(out))
	return werr
}

// ---- context steering: note / correct / unpin + show (#15) ----
//
// These are the brief-STEERING surface, distinct from `context pin
// <documentUid>` above (which toggles Document.Pinned on a vault document). A
// steering note is free text backed by the context_pins table; it leads the
// brief's Pinned section. The verbs are deliberately separate (`note`, not an
// overloaded `pin`) so doc-pin and free-text-pin never collide in help or args.

// contextSteerResponse is the {name,status} shape the three steering write
// tools (contextPin / contextCorrect / contextUnpin) return.
type contextSteerResponse struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// contextPinView is one live steering pin or correction, mirroring the
// contextPins list tool's per-row output shape.
type contextPinView struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Text       string `json:"text"`
	TargetRef  string `json:"targetRef,omitempty"`
	CreateTime string `json:"createTime,omitempty"`
	Scope      string `json:"scope,omitempty"`
}

// contextPinsResponse mirrors the contextPins list tool output.
type contextPinsResponse struct {
	Pins  []contextPinView `json:"pins"`
	Count int              `json:"count"`
}

// contextShowOutput is the --json shape of bare `maz context`: the assembled
// brief plus the active steering pins/corrections.
type contextShowOutput struct {
	Brief contextBriefResponse `json:"brief"`
	Pins  []contextPinView     `json:"pins"`
}

func newContextNoteCmd() *cobra.Command {
	var target, scope string
	cmd := &cobra.Command{
		Use:   "note <text>",
		Short: "Pin a free-text steering note to the top of the context brief",
		Long: `Pin a free-text steering note that leads the cold-start context brief
(getContext / maz context). Use it for a durable preference or constraint you
want surfaced first every session — it wins a budget slot before auto-surfaced
documents and entities.

This is DISTINCT from "maz context pin <documentUid>", which pins a vault
document. A note is free text, not a document reference.

The note is recorded as a contextPinned event; the command prints the new pin's
contextPins/{uid}, which you pass to "maz context correct" or "maz context
unpin".

Examples:
  maz context note "Mid-migration off Postgres — don't suggest new Postgres features"
  maz context note "Ship the wave Friday" --scope project
  maz context note "Prefers Go over Rust" --target entities/01ARZ3NDEKTSV4RRFFQ69G5FAV`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{"text": args[0]}
			if target != "" {
				in["targetRef"] = target
			}
			if scope != "" {
				in["scope"] = scope
			}
			return runContextSteer(cmd, "context note", "contextPin", in)
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "optional resource the note is about, e.g. entities/{uid}")
	cmd.Flags().StringVar(&scope, "scope", "", `scope to write into: project|personal|team (default personal)`)
	return cmd
}

func newContextCorrectCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "correct <id> <text>",
		Short: "Correct a wrong item the brief surfaced (a pin or an entity)",
		Long: `Override the rendered text of a surfaced brief item. Pass the item's id — a
pin's contextPins/{uid} (or bare ULID) shown by "maz context", or an
entities/{uid} the brief surfaced — and the corrected text the brief should
show in its place.

The correction is a NEW event (contextCorrected), not an in-place edit: history
is preserved and the latest correction per item wins. Correcting a pin replaces
that pin's text where it renders; correcting an entity annotates its line.

A pin-style id that names no existing pin returns exit 1 (not found).

Examples:
  maz context correct 01ARZ3NDEKTSV4RRFFQ69G5FAV "Actually ships next Friday"
  maz context correct contextPins/01ARZ3NDEKTSV4RRFFQ69G5FAV "Corrected note"
  maz context correct entities/01ARZ3NDEKTSV4RRFFQ69G5FB0 "Left Acme in May"`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{"targetId": args[0], "text": args[1]}
			if scope != "" {
				in["scope"] = scope
			}
			return runContextSteer(cmd, "context correct", "contextCorrect", in)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", `scope to write into: project|personal|team (default personal)`)
	return cmd
}

func newContextUnpinCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "unpin <id>",
		Short: "Retire a steering pin or correction so it stops surfacing",
		Long: `Retire a steering pin or correction by its contextPins/{uid} (or bare ULID),
so it no longer surfaces in the context brief. The row is tombstoned, not
deleted — history is preserved and the retire converges across devices.

This targets a steering pin/correction (the ids shown by "maz context"); it is
distinct from "maz context pin <documentUid> --unpin", which unpins a vault
document.

An id that names no existing pin returns exit 1 (not found).

Examples:
  maz context unpin 01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz context unpin contextPins/01ARZ3NDEKTSV4RRFFQ69G5FAV`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{"pin": args[0]}
			if scope != "" {
				in["scope"] = scope
			}
			return runContextSteer(cmd, "context unpin", "contextUnpin", in)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", `scope the tombstone is written into; match the pin's scope (default personal)`)
	return cmd
}

// runContextSteer dispatches one steering write tool (contextPin / contextCorrect
// / contextUnpin) and emits its {name,status} result. Every sub-tool call is
// consumed internally so the command emits exactly one result in both plain-text
// and JSON modes; a tool error (E_VALIDATION / E_NOT_FOUND / E_PERMISSION) is an
// exit-1 failure via wrapNavFatal.
func runContextSteer(cmd *cobra.Command, label, toolName string, in map[string]any) error {
	reqBody, merr := json.Marshal(in)
	if merr != nil {
		return wrapNavFatal(cmd, label, merr)
	}
	raw, err := invokeNavTool(cmd, toolName, reqBody)
	if err != nil {
		return wrapNavFatal(cmd, label, err)
	}
	if raw == nil {
		return wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	var resp contextSteerResponse
	if uErr := json.Unmarshal(raw, &resp); uErr != nil {
		return fmt.Errorf("%s: parse response: %w", label, uErr)
	}
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(resp)
	}
	_, werr := fmt.Fprintf(w, "%s: %s\n", resp.Status, resp.Name)
	return werr
}

// runContextShow implements bare `maz context`: the default brief followed by a
// 📌 section of active steering pins and corrections. Both sub-tool calls
// (getContext + contextPins) are consumed internally so a --json invocation
// emits one combined object instead of short-circuiting on the first call.
func runContextShow(cmd *cobra.Command) error {
	const label = "context"
	resp, _, err := fetchContextBrief(cmd, label, "", 0, nil, false)
	if err != nil {
		return err
	}
	pins, perr := fetchContextPins(cmd, label)
	if perr != nil {
		return perr
	}
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		raw, merr := json.Marshal(contextShowOutput{Brief: resp, Pins: pins})
		if merr != nil {
			return wrapNavFatal(cmd, label, merr)
		}
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return werr
	}
	rendered := mdrender.RenderForWriter(cmd.OutOrStdout(), resp.Markdown)
	if _, werr := fmt.Fprint(cmd.OutOrStdout(), rendered); werr != nil {
		return werr
	}
	return renderContextPins(cmd, pins)
}

// fetchContextPins invokes the contextPins list tool and decodes the live pins.
// A nil result (tool not registered: minimal boot / non-dispatcher storage) is a
// fatal error, matching every other context sub-tool fetch.
func fetchContextPins(cmd *cobra.Command, label string) ([]contextPinView, error) {
	raw, err := invokeNavTool(cmd, "contextPins", json.RawMessage(`{}`))
	if err != nil {
		return nil, wrapNavFatal(cmd, label, err)
	}
	if raw == nil {
		return nil, wrapNavFatal(cmd, label, errors.New(errNavUnavailableMsg))
	}
	var resp contextPinsResponse
	if uErr := json.Unmarshal(raw, &resp); uErr != nil {
		return nil, fmt.Errorf("%s: parse pins response: %w", label, uErr)
	}
	return resp.Pins, nil
}

// renderContextPins prints the 📌 active-pins/corrections section in plain-text
// mode, each row showing its contextPins/{uid} so the reader can copy an id into
// `maz context correct` / `maz context unpin`. An empty set prints a one-line hint.
func renderContextPins(cmd *cobra.Command, pins []contextPinView) error {
	w := cmd.OutOrStdout()
	if len(pins) == 0 {
		_, werr := fmt.Fprintln(w, "\n📌 No active pins. Add one with: maz context note \"<text>\"")
		return werr
	}
	if _, werr := fmt.Fprintln(w, "\n📌 Active pins & corrections:"); werr != nil {
		return werr
	}
	for i := range pins {
		p := &pins[i]
		tag := p.Type
		if p.Type == "correction" && p.TargetRef != "" {
			tag = "correction → " + p.TargetRef
		}
		if _, werr := fmt.Fprintf(w, "  %s  [%s] %s\n", p.Name, tag, p.Text); werr != nil {
			return werr
		}
	}
	_, werr := fmt.Fprintln(w, "\nSteer: maz context correct <id> \"...\"  ·  maz context unpin <id>")
	return werr
}
