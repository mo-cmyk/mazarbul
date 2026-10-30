package cli

// show.go — `maz show <entity>` — print the current rendered stub for one entity.
//
// Primary source of truth is the LIVE store (same projection maz get uses) so a
// freshly-created entity is inspectable without waiting for maz sync push.
//
// When the live store returns not-found (entity does not exist), the command
// falls back to checking the git mirror at the given revspec.  If the entity
// exists in the live store but has no git history, an honest message is printed
// instead of leaking the internal rendered/entities.jsonl path.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// showFlags holds parsed flags for `maz show`.
type showFlags struct {
	scope   string
	rev     string
	remote  bool
	format  string // "text", "markdown", or "json"
	history bool   // show relation history (active / superseded / expired)
}

func newShowCmd() *cobra.Command {
	flags := &showFlags{format: "text", rev: "HEAD"}
	cmd := &cobra.Command{
		Use:   "show <entity>",
		Short: "Print the current rendered state of one entity",
		Long: `show prints the live state of an entity from the local store, so a
freshly-created entity is inspectable immediately without running
maz sync push first.

Default format is text (frontmatter stripped, headings rendered as plain
text). Use --format markdown to get the rendered stub, or --format json
to get a structured object.

Pass --history to see the full relation history: active edges are shown
first, followed by superseded (replaced by a newer edge) and expired
(temporally closed but not replaced) edges. Each superseded edge shows
the UID of its replacement.

<entity> must be one of:
  - a full UID (e.g. 01HZAB3NDEKTSV4RRFFQ69G5FAV) — prefix-only does not resolve
  - an entities/{uid} resource name (as returned by maz get / maz recall)
  - an exact display name (case-insensitive, e.g. "Sarah")
  - a display-name fragment that matches exactly one entity

Examples:
  maz show entities/01HZAB3NDEKTSV4RRFFQ69G5FAV
  maz show sarah --scope personal
  maz show sarah --format json | jq '.displayName'
  maz show sarah --format markdown
  maz show sarah --history
  maz show sarah --history --output json | jq '.relations | length'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			logger := logging.FromContext(cmd.Context())
			return runShow(cmd.Context(), cmd, logger, dataDir, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.scope, "scope", "", "only this scope; empty = first configured scope")
	cmd.Flags().StringVar(&flags.rev, "rev", "HEAD", "git revspec for mirror fallback (HEAD, origin/main, ...)")
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "fetch remote before resolving (mirror fallback only)")
	cmd.Flags().StringVar(&flags.format, "format", "text", "output format: text|markdown|json")
	cmd.Flags().BoolVar(&flags.history, "history", false, "show relation history (active / superseded / expired)")
	cmd.AddCommand(newShowDocumentCmd())
	return cmd
}

// showFormatNDJSON is the internal format token for --output ndjson. A show
// result is a single record, so NDJSON means one compact JSON object per line.
const showFormatNDJSON = "ndjson"

// showFragmentCap bounds how many candidate entities an ambiguous fragment
// lookup displays. We query one beyond it so an exact small match set is
// distinguishable from a capped overflow and the ambiguity message never
// reports a false total.
const showFragmentCap = 5

// effectiveShowFormat resolves the output format for `show`, honoring both the
// show-specific --format flag (which uniquely offers markdown) and the standard
// --output flag. An explicit --format wins; otherwise the global --output — and
// its TTY-vs-pipe default — decides, so `maz show <uid> --output json` (and a
// piped `maz show <uid>`) emit JSON instead of silently falling back to text.
// --output ndjson maps to a single compact JSON line, not pretty multi-line.
func effectiveShowFormat(cmd *cobra.Command, flags *showFlags) string {
	if cmd.Flags().Changed("format") {
		return flags.format
	}
	switch output.From(cmd).Format() {
	case output.FormatNDJSON:
		return showFormatNDJSON
	case output.FormatJSON:
		return diffFormatJSON
	default:
		return "text"
	}
}

// encodeShowJSON writes v as JSON to w. When compact is true it emits a single
// line (one NDJSON record); otherwise it pretty-prints with a two-space indent.
func encodeShowJSON(w io.Writer, v any, compact bool) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if !compact {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

// runShow tries the live store first (via the navigator get tool), then falls
// back to the git mirror for --format markdown/text rendering. When --history
// is set it delegates to runShowHistory instead.
func runShow(ctx context.Context, cmd *cobra.Command, logger *zap.Logger, dataDir, entity string, flags *showFlags) error {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// History mode: resolve entity, then list all relations with full history.
	if flags.history {
		return runShowHistory(cmd, logger, flags, dataDir, entity) //nolint:contextcheck // runShowHistory fetches ctx via cmd.Context(); invokeNavTool has no ctx parameter
	}

	// --- Live store path ---
	// Only short-circuit on the live store when no historical revspec is
	// requested. When --rev or --remote is set the caller explicitly asks for
	// a historical snapshot, so we must read the git mirror path even if the
	// entity exists in the live store today.
	revspecRequested := flags.remote || (flags.rev != "" && flags.rev != "HEAD")
	if !revspecRequested {
		liveResult, liveErr := showFromLiveStore(ctx, cmd, logger, dataDir, entity)
		if liveErr == nil {
			return writeShowLive(cmd.OutOrStdout(), liveResult, effectiveShowFormat(cmd, flags))
		}
		// Live store error that is NOT a "not found": surface it and stop.
		if !isNavNotFound(liveErr) {
			return fmt.Errorf("show: %w", liveErr)
		}
	}

	// --- Git mirror fallback ---
	_, syncCfg, cfgErr := loadResolvedSyncConfig(dataDir)
	if cfgErr != nil {
		// If there is no sync config and live store also missed, the entity
		// simply does not exist yet.
		return fmt.Errorf("show: entity %q not found in live store", entity)
	}
	scopeName, serr := resolveShowScope(flags.scope, syncCfg, entity)
	if serr != nil {
		return serr
	}
	repoDir := filepath.Join(dataDir, "sync", scopeName, "repo")
	if flags.remote {
		fetchErrIgnored(ctx, repoDir)
	}

	stubPath, resolveErr := resolveEntityStubPath(ctx, repoDir, flags.rev, entity)
	if resolveErr != nil {
		// Entity not in git mirror.  Rather than leaking the internal path,
		// print an honest message.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), //nolint:errcheck
			"show: entity %q not found; run `maz sync push` to record history\n", entity)
		return silentError{code: 1, err: resolveErr}
	}

	content, gitErr := gitShowFile(ctx, repoDir, flags.rev, stubPath)
	if gitErr != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), //nolint:errcheck
			"show: entity %q exists locally but not yet versioned; run `maz sync push` to record history\n", entity)
		return silentError{code: 1, err: gitErr}
	}

	switch f := effectiveShowFormat(cmd, flags); f {
	case diffFormatMarkdown:
		return writeShowMarkdown(cmd.OutOrStdout(), content)
	case diffFormatJSON, showFormatNDJSON:
		return writeShowJSON(cmd.OutOrStdout(), content, f == showFormatNDJSON)
	default:
		return writeShowText(cmd.OutOrStdout(), content)
	}
}

// showFromLiveStore invokes the navigator get tool for the entity. It accepts
// both resource names (entities/{uid}), bare uids, and display-name tokens
// (including uppercase names like "Mithril" or "SARAH"). Resolution order:
//  1. If the token looks like a resource name (contains "/") → get directly.
//  2. Try get with entities/<token> as a UID path (fast path for real UIDs).
//  3. On not-found, fall through to display-name resolution via the entities tool.
//
// This mirrors resolveEntityArg in the MCP tools layer so every valid token form
// the command help promises is handled correctly.
func showFromLiveStore(ctx context.Context, cmd *cobra.Command, logger *zap.Logger, dataDir, entity string) (json.RawMessage, error) {
	// Bare tokens may have a kind colon prefix (person:sarah → sarah).
	needle := entity
	if !strings.Contains(needle, "/") {
		if idx := strings.Index(needle, ":"); idx >= 0 {
			needle = needle[idx+1:]
		}
	}

	// Fast path: try get with the token treated as a resource name or UID.
	// This succeeds immediately for actual UIDs and entities/{uid} forms.
	resourceName := needle
	if !strings.Contains(needle, "/") {
		resourceName = "entities/" + needle
	}
	raw, err := showGetByName(ctx, cmd, dataDir, logger, resourceName)
	if err == nil {
		return raw, nil
	}
	if !isNavNotFound(err) {
		// Hard infrastructure failure — stop.
		return nil, err
	}

	// Not found as a UID — try display-name resolution so that any token
	// (including uppercase names like "Mithril") reaches its entity.
	if strings.Contains(needle, "/") {
		// Already a resource name form; no display-name fallback possible.
		return nil, err
	}
	resolved, resolveErr := showResolveDisplayName(ctx, cmd, dataDir, logger, needle)
	if resolveErr != nil {
		return nil, resolveErr
	}
	return showGetByName(ctx, cmd, dataDir, logger, "entities/"+resolved)
}

// showGetByName calls the get tool with the given resource name and returns the
// validated response, or an error.
func showGetByName(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, resourceName string) (json.RawMessage, error) {
	reqBody, err := json.Marshal(map[string]any{"name": resourceName})
	if err != nil {
		return nil, fmt.Errorf("marshal get request: %w", err)
	}
	raw, err := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger,
		"get", reqBody,
		func(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, req json.RawMessage) (json.RawMessage, error) {
			return invokeNamedTool(ctx, wired, "get", req)
		},
	)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("navigation tools unavailable")
	}
	// The get tool returns a discriminated union with "type": "entity"|...
	var disc struct {
		Type string `json:"type"`
	}
	if uerr := json.Unmarshal(raw, &disc); uerr != nil || disc.Type == "" {
		return nil, errors.New("unexpected get response shape")
	}
	if disc.Type != diffKindEntity {
		return nil, errors.New("not an entity")
	}
	return raw, nil
}

// showResolveDisplayName resolves a display-name to an entity UID via the
// entities tool, mirroring the canonical NameExact resolver used by neighbors
// and traverse (display_name = ? COLLATE NOCASE). It uses the entities tool's
// nameExact param — an unbounded, case-insensitive exact match — so `maz show
// mithril` resolves an entity named "Mithril" with no paging hazard (a substring
// scan would clamp at the tool's row limit and could bury the exact match).
func showResolveDisplayName(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, name string) (string, error) {
	body, err := json.Marshal(map[string]any{"nameExact": name})
	if err != nil {
		return "", fmt.Errorf("marshal entities request: %w", err)
	}
	raw, err := showInvokeEntities(ctx, cmd, dataDir, logger, body)
	if err != nil {
		return "", err
	}
	var resp showEntitiesResp
	if uerr := json.Unmarshal(raw, &resp); uerr != nil {
		return "", fmt.Errorf("parse entities response: %w", uerr)
	}

	switch len(resp.Entities) {
	case 1:
		return resp.Entities[0].UID, nil
	case 0:
		// No exact match — try the documented display-name fragment form.
		return showResolveFragment(ctx, cmd, dataDir, logger, name)
	default:
		cands := make([]string, 0, len(resp.Entities))
		for _, e := range resp.Entities {
			if e.DisplayName != "" {
				cands = append(cands, e.DisplayName+"  entities/"+e.UID)
			} else {
				cands = append(cands, "entities/"+e.UID)
			}
		}
		return "", fmt.Errorf("%q matches %d entities — pass the specific entities/{uid}:%s",
			name, len(resp.Entities), showCandidateLines(cands))
	}
}

// showResolveFragment handles the display-name fragment form the command help
// documents (`maz show sarah` → "Rosie Cotton"). Called only after an exact
// match failed, it runs a capped case-insensitive substring scan and:
//   - resolves a UNIQUE fragment match (the documented behavior),
//   - returns a "did you mean" error when the fragment is ambiguous,
//   - returns a "no entity named" error when nothing matches.
func showResolveFragment(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, name string) (string, error) {
	// Query one more than we display so an exact small set is distinguishable
	// from a capped overflow — the count in the ambiguity message must not claim
	// a false total when more matches exist beyond the cap.
	subBody, serr := json.Marshal(map[string]any{"nameLike": name, "limit": showFragmentCap + 1})
	if serr != nil {
		return "", fmt.Errorf("no entity named %q", name)
	}
	subRaw, serr2 := showInvokeEntities(ctx, cmd, dataDir, logger, subBody)
	if serr2 != nil {
		return "", fmt.Errorf("no entity named %q", name)
	}
	var subResp showEntitiesResp
	if uerr2 := json.Unmarshal(subRaw, &subResp); uerr2 != nil || len(subResp.Entities) == 0 {
		return "", fmt.Errorf("no entity named %q", name)
	}
	if len(subResp.Entities) == 1 {
		// Unique fragment match — resolve it, honoring the documented form.
		return subResp.Entities[0].UID, nil
	}
	// More than showFragmentCap matches: the candidate list is capped, so report
	// it as such rather than printing the capped length as the true total.
	capped := len(subResp.Entities) > showFragmentCap
	shown := subResp.Entities
	if capped {
		shown = shown[:showFragmentCap]
	}
	cands := make([]string, 0, len(shown))
	for _, e := range shown {
		cands = append(cands, fmt.Sprintf("%s  entities/%s", e.DisplayName, e.UID))
	}
	if capped {
		return "", fmt.Errorf("%q matches more than %d entities — narrow the fragment or pass the specific entities/{uid}:%s",
			name, showFragmentCap, showCandidateLines(cands))
	}
	return "", fmt.Errorf("%q matches %d entities — pass the specific entities/{uid}:%s",
		name, len(subResp.Entities), showCandidateLines(cands))
}

// showEntitiesResp is the subset of the entities tool response showResolveDisplayName reads.
type showEntitiesResp struct {
	Entities []struct {
		UID         string `json:"uid"`
		DisplayName string `json:"displayName"`
	} `json:"entities"`
}

// showInvokeEntities calls the entities tool with req and returns the raw response.
func showInvokeEntities(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, req json.RawMessage) (json.RawMessage, error) {
	raw, err := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger,
		"entities", req,
		func(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, r json.RawMessage) (json.RawMessage, error) {
			return invokeNamedTool(ctx, wired, "entities", r)
		},
	)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("navigation tools unavailable")
	}
	return raw, nil
}

// isNavNotFound reports whether the error from showFromLiveStore represents a
// "not found" condition (as opposed to a hard infrastructure failure).
func isNavNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "E_NOT_FOUND") ||
		strings.Contains(msg, "unexpected get response shape") ||
		strings.Contains(msg, "not an entity")
}

// writeShowLive renders the get-tool response in the requested format.
func writeShowLive(w io.Writer, raw json.RawMessage, format string) error {
	// Unwrap the discriminated union: {"type":"entity","entity":{...}}
	var outer struct {
		Entity json.RawMessage `json:"entity"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil || len(outer.Entity) == 0 {
		// Fallback: print the raw payload.
		_, err2 := fmt.Fprintln(w, string(raw))
		return err2
	}
	switch format {
	case diffFormatJSON, showFormatNDJSON:
		var obj map[string]any
		if err := json.Unmarshal(outer.Entity, &obj); err != nil {
			_, err2 := fmt.Fprintln(w, string(outer.Entity))
			return err2
		}
		return encodeShowJSON(w, obj, format == showFormatNDJSON)
	case diffFormatMarkdown:
		// Render as a simple markdown stub from the entity fields.
		return writeShowLiveMarkdown(w, outer.Entity)
	default: // "text"
		return writeShowLiveText(w, outer.Entity)
	}
}

// writeShowLiveText renders an entity JSON as plain key:value text.
func writeShowLiveText(w io.Writer, entityJSON json.RawMessage) error {
	var e map[string]any
	if err := json.Unmarshal(entityJSON, &e); err != nil {
		_, err2 := fmt.Fprintln(w, string(entityJSON))
		return err2
	}
	for _, k := range sortedKeys(e) {
		if _, err := fmt.Fprintf(w, "%s: %v\n", k, e[k]); err != nil {
			return err
		}
	}
	return nil
}

// writeShowLiveMarkdown renders a live entity's JSON as a YAML-frontmatter
// markdown stub, matching the style the git mirror would produce. All known
// fields from the get response (including facts, identifiers, and provenance)
// are rendered rather than a subset, so no data is silently dropped.
func writeShowLiveMarkdown(w io.Writer, entityJSON json.RawMessage) error {
	// Decode the full entity object so every field is available.
	var e map[string]any
	if err := json.Unmarshal(entityJSON, &e); err != nil {
		_, err2 := fmt.Fprintln(w, string(entityJSON))
		return err2
	}
	// Emit YAML frontmatter for scalar/string fields in a stable order,
	// then emit a markdown heading. Complex fields (facts, identifiers,
	// aliases) follow the primary block so no known data is dropped.
	// Type assertions use the two-value form; missing/wrong-type fields
	// simply produce empty strings / zero values (no panic).
	displayName, _ := e["displayName"].(string)           //nolint:errcheck // type assertion; missing key = ""
	uid, _ := e["uid"].(string)                           //nolint:errcheck // type assertion; missing key = ""
	kind, _ := e["kind"].(string)                         //nolint:errcheck // type assertion; missing key = ""
	resolutionStatus, _ := e["resolutionStatus"].(string) //nolint:errcheck // type assertion; missing key = ""
	confidence, _ := e["confidence"].(float64)            //nolint:errcheck // type assertion; missing key = 0

	if _, err := fmt.Fprintf(w, "---\nuid: %s\nkind: %s\ndisplayName: %s\nresolutionStatus: %s\nconfidence: %.2f\n",
		uid, kind, displayName, resolutionStatus, confidence); err != nil {
		return err
	}
	// Append remaining fields alphabetically as additional frontmatter lines.
	scalarSkip := map[string]bool{
		"uid": true, "kind": true, "displayName": true,
		"resolutionStatus": true, "confidence": true,
	}
	if err := writeShowLiveMarkdownExtra(w, e, scalarSkip); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "---\n\n# %s\n", displayName); err != nil {
		return err
	}
	return nil
}

// writeShowLiveMarkdownExtra emits the remaining entity fields (those not in
// scalarSkip) as YAML frontmatter lines. Scalars (string/float64/bool) render
// inline; complex values (arrays, objects) are serialised as a single JSON
// value on the same line so no known data is silently omitted.
func writeShowLiveMarkdownExtra(w io.Writer, e map[string]any, skip map[string]bool) error {
	for _, k := range sortedKeys(e) {
		if skip[k] {
			continue
		}
		if err := writeShowLiveMarkdownField(w, k, e[k]); err != nil {
			return err
		}
	}
	return nil
}

// writeShowLiveMarkdownField writes one key/value pair as a YAML frontmatter line.
func writeShowLiveMarkdownField(w io.Writer, k string, v any) error {
	switch val := v.(type) {
	case string:
		_, err := fmt.Fprintf(w, "%s: %s\n", k, val)
		return err
	case float64:
		_, err := fmt.Fprintf(w, "%s: %g\n", k, val)
		return err
	case bool:
		_, err := fmt.Fprintf(w, "%s: %v\n", k, val)
		return err
	default:
		// Complex fields (arrays, objects): serialize as JSON on one line.
		// Skip fields that cannot be marshalled (defensive; json.Marshal
		// only fails for cyclic structures or unsupported types in entity data).
		b, merr := json.Marshal(val)
		if merr != nil {
			return nil //nolint:nilerr // intentional skip of un-marshallable values
		}
		_, err := fmt.Fprintf(w, "%s: %s\n", k, string(b))
		return err
	}
}

// gitShowFile returns the content of path at rev inside repoDir using
// `git show rev:path`.
func gitShowFile(ctx context.Context, repoDir, rev, path string) (string, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git not in PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir, "show", rev+":"+path)
	cmd.Env = cleanGitEnv(repoDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git show %s:%s: %w", rev, path, err)
	}
	return stdout.String(), nil
}

func writeShowMarkdown(w io.Writer, content string) error {
	if _, err := fmt.Fprint(w, content); err != nil {
		return fmt.Errorf("show: write: %w", err)
	}
	return nil
}

// writeShowText strips YAML frontmatter and prints the body as plain text.
func writeShowText(w io.Writer, content string) error {
	text := stripFrontmatter(content)
	if _, err := fmt.Fprint(w, text); err != nil {
		return fmt.Errorf("show: write: %w", err)
	}
	return nil
}

// writeShowJSON extracts frontmatter fields into a JSON object.
func writeShowJSON(w io.Writer, content string, compact bool) error {
	fields := parseFrontmatter(content)
	return encodeShowJSON(w, fields, compact)
}

// stripFrontmatter removes the YAML frontmatter block (--- ... ---) from
// content and returns the body.
func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return content
	}
	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return content
	}
	return strings.TrimPrefix(content[4+end+5:], "\n")
}

// parseFrontmatter extracts key: value pairs from a YAML frontmatter block.
// Handles both simple scalar form ("key: value") and YAML block sequences
// ("- item") which the renderer may emit for list-valued fields.  List items
// are collected under the preceding key; sequence lines (starting with "- ")
// that appear before any key are skipped so they never produce a "- type" key.
func parseFrontmatter(content string) map[string]any {
	result := make(map[string]any)
	if !strings.HasPrefix(content, "---\n") {
		return result
	}
	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return result
	}
	block := content[4 : 4+end]
	var lastKey string
	for _, line := range strings.Split(block, "\n") {
		lastKey = parseFrontmatterLine(line, lastKey, result)
	}
	return result
}

// parseFrontmatterLine processes one line of YAML frontmatter into result and
// returns the (possibly updated) lastKey. Extracted to keep parseFrontmatter's
// cognitive complexity within the gocognit limit.
func parseFrontmatterLine(line, lastKey string, result map[string]any) string {
	// YAML list item under the preceding scalar key.
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "  - ") {
		if lastKey != "" {
			appendFrontmatterListItem(result, lastKey,
				strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")))
		}
		return lastKey
	}
	idx := strings.Index(line, ": ")
	if idx < 0 {
		// Bare key with no value (e.g. "key:").
		if strings.HasSuffix(strings.TrimSpace(line), ":") {
			lastKey = strings.TrimSuffix(strings.TrimSpace(line), ":")
			result[lastKey] = ""
		}
		return lastKey
	}
	lastKey = strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+2:])
	result[lastKey] = val
	return lastKey
}

// appendFrontmatterListItem adds item to result[key], collecting values into a
// []string. An empty-string bare-key placeholder is discarded so that the
// first list item is not preceded by a spurious empty string.
func appendFrontmatterListItem(result map[string]any, key, item string) {
	switch v := result[key].(type) {
	case []string:
		result[key] = append(v, item)
	case string:
		// Empty string is the bare-key placeholder; discard it.
		if v == "" {
			result[key] = []string{item}
		} else {
			result[key] = []string{v, item}
		}
	default:
		result[key] = []string{item}
	}
}

// ---- --history implementation ----

// showRelRow is the subset of a relation wire row that --history consumes.
type showRelRow struct {
	UID             string `json:"uid"`
	Predicate       string `json:"predicate"`
	SubjectUID      string `json:"subjectUid"`
	SubjectName     string `json:"subjectName"`
	ObjectUID       string `json:"objectUid"`
	ObjectName      string `json:"objectName"`
	Direction       string `json:"direction"`
	ValidFrom       string `json:"validFrom"`
	ValidUntil      string `json:"validUntil"`
	SupersededByUID string `json:"supersededByUid"`
}

// showHistoryBuckets holds the three-way partition of relation history rows.
type showHistoryBuckets struct {
	active     []showRelRow // no supersededByUid, no validUntil
	superseded []showRelRow // supersededByUid set — has a named replacement
	expired    []showRelRow // validUntil set, supersededByUid empty
}

// resolveShowScope returns flags.scope when it is non-empty, or the first
// configured scope in syncCfg. Returns an error when no scope is resolvable.
func resolveShowScope(flagScope string, syncCfg config.SyncTable, entity string) (string, error) {
	if flagScope != "" {
		return flagScope, nil
	}
	scopes := sortedScopeNames(syncCfg)
	if len(scopes) == 0 {
		return "", fmt.Errorf("show: entity %q not found", entity)
	}
	return scopes[0], nil
}

// showCandidateLines formats a slice of candidate strings as a
// newline-separated bullet list for embedding in an ambiguous-entity error.
func showCandidateLines(cands []string) string {
	var b strings.Builder
	for _, c := range cands {
		b.WriteString("\n  • ")
		b.WriteString(c)
	}
	return b.String()
}

// partitionHistoryRows classifies rows by retraction state. SupersededByUID
// takes priority when both SupersededByUID and ValidUntil are present.
func partitionHistoryRows(rows []showRelRow) showHistoryBuckets {
	var b showHistoryBuckets
	for i := range rows {
		switch {
		case rows[i].SupersededByUID != "":
			b.superseded = append(b.superseded, rows[i])
		case rows[i].ValidUntil != "":
			b.expired = append(b.expired, rows[i])
		default:
			b.active = append(b.active, rows[i])
		}
	}
	return b
}

// entityCard is the identity header data for the --history view: the entity's
// UID, display name, kind, resolution status, and confidence.
type entityCard struct {
	uid              string
	displayName      string
	kind             string
	resolutionStatus string
	confidence       float64
}

// extractEntityCard pulls the identity fields from a get-tool response
// envelope {"type":"entity","entity":{...}}.
func extractEntityCard(raw json.RawMessage) (entityCard, error) {
	var outer struct {
		Entity struct {
			UID              string  `json:"uid"`
			DisplayName      string  `json:"displayName"`
			Kind             string  `json:"kind"`
			ResolutionStatus string  `json:"resolutionStatus"`
			Confidence       float64 `json:"confidence"`
		} `json:"entity"`
	}
	if uerr := json.Unmarshal(raw, &outer); uerr != nil {
		return entityCard{}, fmt.Errorf("parse entity: %w", uerr)
	}
	if outer.Entity.UID == "" {
		return entityCard{}, errors.New("entity has no uid")
	}
	return entityCard{
		uid:              outer.Entity.UID,
		displayName:      outer.Entity.DisplayName,
		kind:             outer.Entity.Kind,
		resolutionStatus: outer.Entity.ResolutionStatus,
		confidence:       outer.Entity.Confidence,
	}, nil
}

// runShowHistory resolves entity to a UID, fetches all relations with
// includeInactive:true, and renders the three-way partition (active /
// superseded / expired) with tree glyphs. JSON and NDJSON modes emit a
// clean passthrough of the raw relations payload.
func runShowHistory(cmd *cobra.Command, logger *zap.Logger, flags *showFlags, dataDir, entity string) error {
	liveResult, err := showFromLiveStore(cmd.Context(), cmd, logger, dataDir, entity)
	if err != nil {
		if !isNavNotFound(err) {
			return fmt.Errorf("show --history: %w", err)
		}
		return fmt.Errorf("show --history: entity %q not found in live store", entity)
	}
	card, err := extractEntityCard(liveResult)
	if err != nil {
		return fmt.Errorf("show --history: %w", err)
	}
	if card.displayName == "" {
		card.displayName = entity
	}
	reqBody, merr := json.Marshal(map[string]any{
		"entity":          "entities/" + card.uid,
		"includeInactive": true,
	})
	if merr != nil {
		return fmt.Errorf("show --history: marshal request: %w", merr)
	}
	raw, rerr := invokeNavTool(cmd, "relations", reqBody)
	if rerr != nil {
		return wrapNavFatal(cmd, "show --history", rerr)
	}
	if raw == nil {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errNavUnavailableMsg); werr != nil {
			return werr
		}
		return silentError{code: 1, err: fmt.Errorf("%s", errNavUnavailableMsg)}
	}
	// Honor both the show-specific --format and the global --output via
	// effectiveShowFormat — same as the non-history path — so `show <e> --history
	// --format json` emits JSON instead of silently falling back to text.
	// --format markdown has no history rendering and maps to the text view.
	switch effectiveShowFormat(cmd, flags) {
	case showFormatNDJSON:
		return renderHistoryNDJSON(cmd.OutOrStdout(), raw)
	case diffFormatJSON:
		_, ferr := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return ferr
	default:
		return renderShowHistory(cmd, raw, card)
	}
}

// renderHistoryNDJSON emits one JSON object per relation to w.
func renderHistoryNDJSON(w io.Writer, raw json.RawMessage) error {
	var resp struct {
		Relations []json.RawMessage `json:"relations"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("show --history: parse ndjson: %w", err)
	}
	for _, r := range resp.Relations {
		if _, err := fmt.Fprintln(w, string(r)); err != nil {
			return err
		}
	}
	return nil
}

// renderShowHistory parses raw relations JSON and renders the history view.
func renderShowHistory(cmd *cobra.Command, raw json.RawMessage, card entityCard) error {
	var resp struct {
		Relations []showRelRow `json:"relations"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("show --history: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	clr := colorizer.New(interactive.ShouldStyle(cmd, w), w)
	return renderShowHistoryBuckets(w, clr, card, partitionHistoryRows(resp.Relations))
}

// renderShowHistoryBuckets renders the full history view: a ◆ entity card
// header, then flat active rows, then the superseded / closed section with
// supersession links. Called by renderShowHistory and directly from tests.
func renderShowHistoryBuckets(w io.Writer, clr *colorizer.Colorizer, card entityCard, b showHistoryBuckets) error {
	if err := renderEntityCard(w, clr, card); err != nil {
		return err
	}
	if len(b.active)+len(b.superseded)+len(b.expired) == 0 {
		_, err := fmt.Fprintln(w, clr.Dim("no relations"))
		return err
	}
	if err := renderHistoryActive(w, clr, b.active); err != nil {
		return err
	}
	closed := make([]showRelRow, 0, len(b.superseded)+len(b.expired))
	closed = append(closed, b.superseded...)
	closed = append(closed, b.expired...)
	return renderHistoryClosed(w, clr, closed)
}

// renderEntityCard writes the ◆ identity header:
//
//	◆ Sarah Jones   person · resolved · conf 0.91
//	  uid entities/01JZ…
func renderEntityCard(w io.Writer, clr *colorizer.Colorizer, card entityCard) error {
	meta := make([]string, 0, 3)
	if card.kind != "" {
		meta = append(meta, card.kind)
	}
	if card.resolutionStatus != "" {
		meta = append(meta, card.resolutionStatus)
	}
	if card.confidence > 0 {
		meta = append(meta, fmt.Sprintf("conf %.2f", card.confidence))
	}
	header := fmt.Sprintf("%s %s", clr.Accent("◆"), clr.Accent(card.displayName))
	if len(meta) > 0 {
		header += "   " + clr.Dim(strings.Join(meta, " · "))
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	if card.uid != "" {
		if _, err := fmt.Fprintf(w, "  %s %s\n", clr.Dim("uid"), clr.Ident("entities/"+card.uid)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderHistoryActive renders the lowercase "active" section header and its
// flat rows (no tree prefix — active relations are siblings, not a chain).
func renderHistoryActive(w io.Writer, clr *colorizer.Colorizer, rows []showRelRow) error {
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, clr.Dim("active")); err != nil {
		return err
	}
	for i := range rows {
		if err := writeHistoryRow(w, clr, &rows[i], "●", clr.Success); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderHistoryClosed renders the lowercase "superseded / closed" section.
// Each superseded row gets a "└─→ superseded by relations/<uid>" child line.
func renderHistoryClosed(w io.Writer, clr *colorizer.Colorizer, rows []showRelRow) error {
	if len(rows) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, clr.Dim("superseded / closed")); err != nil {
		return err
	}
	for i := range rows {
		if rows[i].SupersededByUID != "" {
			if err := writeHistoryRow(w, clr, &rows[i], "✗", clr.Removed); err != nil {
				return err
			}
			if err := writeSupersederLine(w, clr, rows[i].SupersededByUID); err != nil {
				return err
			}
		} else {
			if err := writeHistoryRow(w, clr, &rows[i], "~", clr.Changed); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeSupersederLine writes the "  └─→ superseded by relations/<uid>" child
// line beneath a superseded relation row.
func writeSupersederLine(w io.Writer, clr *colorizer.Colorizer, uid string) error {
	_, err := fmt.Fprintf(w, "  %s superseded by relations/%s\n",
		clr.Dim("└─→"), clr.Ident(uid))
	return err
}

// writeHistoryRow writes one relation row as "<glyph> <subject> ──<pred>──▶
// <object>  <window>". The predicate labels the directed arrow so the stored
// (subject → object) orientation is always explicit.
func writeHistoryRow(w io.Writer, clr *colorizer.Colorizer, r *showRelRow, glyph string, paint func(string) string) error {
	subject := r.SubjectName
	if subject == "" {
		subject = r.SubjectUID
	}
	object := r.ObjectName
	if object == "" {
		object = r.ObjectUID
	}
	arrow := clr.Dim("──") + clr.Meta(r.Predicate) + clr.Dim("──▶")
	line := fmt.Sprintf("%s %s %s %s", paint(glyph), subject, arrow, object)
	if win := showRelWindow(r); win != "" {
		line += "  " + clr.Dim(win)
	}
	_, err := fmt.Fprintln(w, line)
	return err
}

// showRelWindow formats the validity window of a relation row as a bracketed
// date range. Returns an empty string when neither bound is set.
func showRelWindow(r *showRelRow) string {
	from := showDatePart(r.ValidFrom)
	until := showDatePart(r.ValidUntil)
	switch {
	case from != "" && until != "":
		return "[" + from + " – " + until + "]"
	case from != "":
		return "[since " + from + "]"
	case until != "":
		return "[until " + until + "]"
	default:
		return ""
	}
}

// showDatePart returns the YYYY-MM-DD prefix of an RFC3339 timestamp, or the
// value unchanged when it is shorter than 10 characters.
func showDatePart(ts string) string {
	if len(ts) < 10 {
		return ts
	}
	return ts[:10]
}
