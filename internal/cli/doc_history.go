package cli

// doc_history.go — `maz doc log` and `maz doc diff`: the free version history a
// document layer earns from being event-sourced. Both read the revision chain
// straight from the event log via the documentHistory tool; `maz doc diff`
// renders a unified body diff between two revisions with go-udiff.

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	udiff "github.com/aymanbagabas/go-udiff"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

// docRevisionRow mirrors the documentHistory tool's revision wire shape.
type docRevisionRow struct {
	UID                 string `json:"uid"`
	CreateTime          string `json:"createTime"`
	Kind                string `json:"kind"` // "revision" | "deleted"
	ContentHash         string `json:"contentHash"`
	Title               string `json:"title"`
	Bytes               int    `json:"bytes"`
	PreviousRevisionUID string `json:"previousRevisionUid"`
	Reason              string `json:"reason"`
	Body                string `json:"body"`
	BodyTruncated       bool   `json:"bodyTruncated"`
}

type docHistoryResponse struct {
	DocumentUID string           `json:"documentUid"`
	Path        string           `json:"path"`
	Revisions   []docRevisionRow `json:"revisions"`
}

// fetchDocHistory invokes documentHistory, honoring the global --format json
// flag (raw passthrough) and decoding into a typed response otherwise. The
// handled bool reports that JSON was already emitted, so the caller skips its
// human-readable rendering.
func fetchDocHistory(cmd *cobra.Command, label, path string, includeBodies bool) (resp docHistoryResponse, handled bool, err error) {
	reqBody, mErr := json.Marshal(map[string]any{"path": path, "includeBodies": includeBodies})
	if mErr != nil {
		return resp, false, wrapNavFatal(cmd, label, mErr)
	}
	raw, iErr := invokeNavTool(cmd, "documentHistory", reqBody)
	if iErr != nil {
		return resp, false, wrapNavFatal(cmd, label, iErr)
	}
	if h, hErr := emitNavResult(cmd, raw); h {
		return resp, true, hErr
	}
	if uErr := json.Unmarshal(raw, &resp); uErr != nil {
		return resp, false, fmt.Errorf("%s: parse response: %w", label, uErr)
	}
	return resp, false, nil
}

// ---- doc log ----

func newDocLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "log <path>",
		Short: "Show a document's revision history",
		Long: `List every saved revision (and any deletion) of a document in
chronological order (oldest first), straight from the event log. Each revision is
content-addressed, so the hash column lets you confirm two revisions are
byte-identical.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, handled, err := fetchDocHistory(cmd, "doc log", args[0], false)
			if err != nil || handled {
				return err
			}
			return renderDocLog(cmd, resp)
		},
	}
}

func renderDocLog(cmd *cobra.Command, resp docHistoryResponse) error {
	if len(resp.Revisions) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no revisions")
		return err
	}
	c := cmdColorizer(cmd)
	return output.MaybePage(cmd, func(pw io.Writer) error {
		tbl := output.NewTable(output.New(output.FormatText, pw, cmd.ErrOrStderr()), c, []output.Column{
			{Title: "revision"},
			{Title: "when"},
			{Title: "kind"},
			{Title: "bytes", Align: output.AlignRight},
			{Title: "hash"},
			{Title: "title"},
		})
		// Newest first, mirroring `git log`.
		for i := len(resp.Revisions) - 1; i >= 0; i-- {
			r := resp.Revisions[i]
			title := r.Title
			if r.Kind == "deleted" {
				title = "(deleted)"
				if r.Reason != "" {
					title = "(deleted: " + r.Reason + ")"
				}
			}
			tbl.Row(shortRevUID(r.UID), formatRevTime(r.CreateTime), r.Kind, strconv.Itoa(r.Bytes), shortHash(r.ContentHash), title)
		}
		return tbl.Render()
	})
}

// ---- doc diff ----

func newDocDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <path> [revA] [revB]",
		Short: "Diff two revisions of a document",
		Long: `Show a unified body diff between two revisions of a document.

With no revision arguments, diffs the two most recent revisions. With one, diffs
that revision against the latest. Revisions are matched by a prefix of their UID
(as shown by 'maz doc log').`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, handled, err := fetchDocHistory(cmd, "doc diff", args[0], true)
			if err != nil || handled {
				return err
			}
			return renderDocDiff(cmd, resp, args[1:])
		},
	}
}

func renderDocDiff(cmd *cobra.Command, resp docHistoryResponse, revArgs []string) error {
	revs := contentRevisions(resp.Revisions)
	if len(revs) < 2 && len(revArgs) < 2 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "need at least two revisions to diff")
		return err
	}
	a, b, err := pickDiffRevisions(revs, revArgs)
	if err != nil {
		return wrapNavFatal(cmd, "doc diff", err)
	}
	// A truncated body is NOT verbatim, so diffing it would silently hide any
	// change past the cap (or invent a phantom one at the marker). Refuse with a
	// clear message rather than print a wrong diff; the full body is on disk.
	if a.BodyTruncated || b.BodyTruncated {
		which := shortRevUID(a.UID)
		if b.BodyTruncated {
			which = shortRevUID(b.UID)
		}
		return wrapNavFatal(cmd, "doc diff", fmt.Errorf(
			"revision %s is too large to diff in full; compare the verbatim bodies in the vault file %s instead",
			which, resp.Path))
	}
	labelA := fmt.Sprintf("%s @ %s", resp.Path, shortRevUID(a.UID))
	labelB := fmt.Sprintf("%s @ %s", resp.Path, shortRevUID(b.UID))
	out := udiff.Unified(labelA, labelB, a.Body, b.Body)
	if strings.TrimSpace(out) == "" {
		_, werr := fmt.Fprintf(cmd.OutOrStdout(), "no changes between %s and %s\n", shortRevUID(a.UID), shortRevUID(b.UID))
		return werr
	}
	_, werr := fmt.Fprint(cmd.OutOrStdout(), out)
	return werr
}

// contentRevisions keeps only the revision entries (drops deletion tombstones),
// preserving chronological order (oldest first).
func contentRevisions(rows []docRevisionRow) []docRevisionRow {
	out := make([]docRevisionRow, 0, len(rows))
	for i := range rows {
		if rows[i].Kind == "revision" {
			out = append(out, rows[i])
		}
	}
	return out
}

// pickDiffRevisions resolves the (older, newer) pair to diff from the revision
// list and the user's optional revspec arguments.
func pickDiffRevisions(revs []docRevisionRow, revArgs []string) (a, b docRevisionRow, err error) {
	switch len(revArgs) {
	case 0:
		return revs[len(revs)-2], revs[len(revs)-1], nil
	case 1:
		ra, e := matchRevision(revs, revArgs[0])
		if e != nil {
			return a, b, e
		}
		return ra, revs[len(revs)-1], nil
	default:
		ra, e := matchRevision(revs, revArgs[0])
		if e != nil {
			return a, b, e
		}
		rb, e := matchRevision(revs, revArgs[1])
		if e != nil {
			return a, b, e
		}
		return ra, rb, nil
	}
}

// matchRevision finds the revision whose UID has the given prefix. The match is
// case-insensitive because ULIDs are uppercase but users may paste lowercase.
func matchRevision(revs []docRevisionRow, prefix string) (docRevisionRow, error) {
	up := strings.ToUpper(prefix)
	var match *docRevisionRow
	for i := range revs {
		if strings.HasPrefix(revs[i].UID, up) {
			if match != nil {
				return docRevisionRow{}, fmt.Errorf("revision %q is ambiguous", prefix)
			}
			match = &revs[i]
		}
	}
	if match == nil {
		return docRevisionRow{}, fmt.Errorf("no revision matching %q", prefix)
	}
	return *match, nil
}

// shortRevUID returns the first 8 characters of a revision ULID for display.
func shortRevUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// shortHash returns the first 12 characters of a content hash, or "-" when empty.
func shortHash(h string) string {
	if h == "" {
		return "-"
	}
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// formatRevTime renders an RFC 3339 timestamp as a compact local-agnostic form,
// falling back to the raw string when it does not parse.
func formatRevTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}
