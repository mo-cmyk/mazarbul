package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// ulidRE matches a bare Crockford Base32 ULID (26 uppercase characters).
var ulidRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// forgetCollection names the resource collection a forget argument refers to.
type forgetCollection int

const (
	forgetEpisode  forgetCollection = iota // episodeUid field.
	forgetRelation                         // relationUid field.
	forgetFact                             // factUid field.
)

// normalizeForgetUID accepts the raw argument the user typed and returns the
// bare ULID plus the collection it belongs to. It accepts both:
//   - a bare ULID (26 Crockford Base32 chars) — collection defaults to episode.
//   - a resource-name form: "episodes/<ULID>", "relations/<ULID>", or
//     "facts/<ULID>".
//
// Any other form is rejected with a descriptive error so the caller can return
// exit code 1.
func normalizeForgetUID(raw string) (uid string, coll forgetCollection, err error) {
	switch {
	case strings.HasPrefix(raw, "episodes/"):
		uid = strings.TrimPrefix(raw, "episodes/")
		coll = forgetEpisode
	case strings.HasPrefix(raw, "relations/"):
		uid = strings.TrimPrefix(raw, "relations/")
		coll = forgetRelation
	case strings.HasPrefix(raw, "facts/"):
		uid = strings.TrimPrefix(raw, "facts/")
		coll = forgetFact
	default:
		// Detect a well-formed resource name whose collection is not forgettable
		// (e.g. "entities/<ulid>"). Splitting on "/" and checking that the prefix
		// looks like a collection name gives a clear error rather than the
		// confusing "not a valid ULID" message.
		if slash := strings.IndexByte(raw, '/'); slash > 0 {
			collection := raw[:slash]
			return "", 0, fmt.Errorf(
				"collection %q cannot be forgotten; forget accepts episodes/<ulid>, relations/<ulid>, or facts/<ulid>",
				collection,
			)
		}
		uid = raw
		coll = forgetEpisode
	}
	if !ulidRE.MatchString(uid) {
		return "", 0, fmt.Errorf(
			"argument %q is not a valid ULID or resource name (episodes/<ulid> / relations/<ulid> / facts/<ulid>)",
			raw,
		)
	}
	return uid, coll, nil
}

func newForgetCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "forget <uid>",
		Short: "Tombstone an Episode, Relation, or Fact across the storage layers",
		Long: `Append an episodeForgotten, relationForgotten, or factForgotten
tombstone for the given identifier. The dispatcher removes derived state
(episodes row, mentions, provenance, vec0 row, LadybugDB Episode node and
its incident edges for episodes; the relations row and RELATES_TO graph
edge for relations; the facts row for facts). The JSONL log retains both
the original event and the tombstone so 'maz rebuild' replays both and
arrives at the same end state.

<uid> accepts a bare ULID or the resource-name form printed by
'maz list episodes', 'maz list relations', 'maz fact get', and 'maz recall':

  maz forget 01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz forget episodes/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz forget relations/01ARZ3NDEKTSV4RRFFQ69G5FAV
  maz forget facts/01ARZ3NDEKTSV4RRFFQ69G5FAV

Entities cannot be forgotten; forgetting an episode does NOT cascade-remove
the entities or relations extracted from it — forget is per-record. To close
a fact while preserving its history (instead of hard-deleting it), use
'maz fact retract'.

Use 'maz list episodes' or 'maz recall' to find the identifier first; this
command does not search.

For a draft-scoped (private staging) episode, use 'maz scope draft discard'
instead — it removes the staged record without it ever entering personal scope.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runForget(cmd, args[0], reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "optional free-text reason for the forget (surfaced in audit)")
	return cmd
}

func runForget(cmd *cobra.Command, rawUID, reason string) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	uid, coll, err := normalizeForgetUID(rawUID)
	if err != nil {
		return wrapForgetError(cmd, mcp.NewValidationError(err.Error(), nil))
	}

	var toolArgs map[string]any
	switch coll {
	case forgetRelation:
		toolArgs = map[string]any{"relationUid": uid}
	case forgetFact:
		toolArgs = map[string]any{"factUid": uid}
	default:
		toolArgs = map[string]any{"episodeUid": uid}
	}
	if reason != "" {
		toolArgs["reason"] = reason
	}
	body, merr := json.Marshal(toolArgs)
	if merr != nil {
		return wrapForgetFatal(cmd, fmt.Errorf("marshal request: %w", merr))
	}

	// isBareUID is true when rawUID was not prefixed — the caller typed a bare
	// ULID and normalizeForgetUID defaulted it to an episode. We use this flag
	// later to give an entity-aware hint when the episode lookup 404s.
	isBareUID := coll == forgetEpisode && rawUID == uid

	raw, rerr := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger,
		"forget", body,
		func(ctx context.Context, _ *cobra.Command, wired *daemon.Wired, req json.RawMessage) (json.RawMessage, error) {
			return invokeForgetTool(ctx, wired, req)
		},
	)
	if rerr != nil {
		// E1: when a bare ULID 404s as an episode, check whether it is actually
		// an entity so the error message is actionable rather than confusing.
		if isBareUID && isExpectedNavError(rerr) && isNotFoundError(rerr) {
			rerr = clarifyBareUIDNotFound(cmd, uid, rerr)
		}
		return wrapForgetError(cmd, rerr)
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		_, werr := fmt.Fprintln(w, string(raw))
		return werr
	}
	var out struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return wrapForgetFatal(cmd, fmt.Errorf("decode response: %w", uerr))
	}
	_, werr := fmt.Fprintf(w, "%s %s\n", out.Status, out.Name)
	return werr
}

// isNotFoundError reports whether err carries the E_NOT_FOUND code, whether as
// a typed *mcp.ToolError or a plain-text transport error. It defers to the
// shared toolErrorCode classifier so the CLI has one notion of "not found".
func isNotFoundError(err error) bool {
	return toolErrorCode(err) == string(mcp.CodeNotFound)
}

// clarifyBareUIDNotFound is called when a bare ULID 404d as an episode.
// It attempts a `get entities/<uid>` probe via the same runToolViaDaemonOrInProcess
// seam. If the probe succeeds the uid is an entity and we return a clear
// "cannot forget an entity" error. If the probe also 404s (or the get tool is
// unavailable) we return a broadened hint noting that a bare uid is treated as
// an episode and that entities cannot be forgotten.
func clarifyBareUIDNotFound(cmd *cobra.Command, uid string, original error) error {
	getReq, merr := json.Marshal(map[string]any{"name": "entities/" + uid})
	if merr != nil {
		return original
	}
	_, gerr := invokeNavTool(cmd, "get", getReq)
	if gerr == nil {
		// The uid belongs to an entity — give the definitive message.
		return mcp.NewValidationError(
			uid+" is an entity; forget only tombstones episodes and relations "+
				"(entities cannot be deleted directly). "+
				"Find forgettable ids with `maz list episodes` / `maz list relations` / `maz recall`.",
			nil,
		)
	}
	// Entity check failed (also not found, or tool unavailable): broaden the
	// original error with a hint rather than replacing it.
	return fmt.Errorf(
		"%w (a bare uid is treated as an episode; if this is an entity it "+
			"cannot be forgotten, and relations must be passed as relations/<ulid>)",
		original,
	)
}

func invokeForgetTool(ctx context.Context, wired *daemon.Wired, body json.RawMessage) (json.RawMessage, error) {
	for _, t := range wired.Tools {
		if t.Name() != "forget" {
			continue
		}
		return t.Invoke(ctx, body)
	}
	return nil, errors.New("forget tool not available")
}

// wrapForgetError handles a tool-layer error from forget, emitting the shared
// {error,code} JSON envelope in JSON mode and routing E_NOT_FOUND /
// E_VALIDATION / E_PERMISSION to exit code 1 (expected failures). All other
// errors exit 2.
func wrapForgetError(cmd *cobra.Command, err error) error {
	return respondError(cmd, "forget", err)
}

// wrapForgetFatal handles an internal (unexpected) error from forget, always
// exiting 2. Use wrapForgetError when the error comes from the tool layer.
func wrapForgetFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "forget", err)
}
