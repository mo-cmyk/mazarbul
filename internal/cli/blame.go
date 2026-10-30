package cli

// blame.go — `maz blame <entity>` — chronological lifeline for one entity.
//
// Resolves the entity via the LIVE store first (same lookup maz get uses) so a
// local entity that has not yet been pushed to the git mirror is inspectable
// immediately.  Git history is the source of truth for the chronological
// lifeline; when no history exists yet, an honest message is printed instead of
// leaking the internal rendered/entities.jsonl path.
//
// Output mirrors `git blame`'s cadence:
//
//	2026-05-12 10:23 (laptop-A · 01HV...)  + created entity
//	2026-05-15 14:02 (laptop-A · 01HW...)    + alias "Rosie" added

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

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// blameIsJSONFormat reports whether the active output format is JSON, checking
// both the legacy --format flag and the standard --output flag so that
// `maz blame --output json` and `maz blame --format json` both produce JSON.
func blameIsJSONFormat(cmd *cobra.Command, flags *blameFlags) bool {
	if flags.format == diffFormatJSON {
		return true
	}
	return output.From(cmd).Format() == output.FormatJSON
}

// blameEntry is one event in an entity's lifeline.
type blameEntry struct {
	// Date is the commit date in YYYY-MM-DD HH:MM format.
	Date string `json:"date"`
	// Device is the committing device slug parsed from the git author.
	Device string `json:"device"`
	// CommitUID is the ULID portion of the commit subject (best-effort).
	CommitUID string `json:"commitUid,omitempty"`
	// Action describes what changed: "created", "alias added", etc.
	Action string `json:"action"`
	// Detail is free-text context for the action.
	Detail string `json:"detail,omitempty"`
}

// blameFlags holds parsed flags for `maz blame`.
type blameFlags struct {
	scope  string
	since  string
	rev    string
	remote bool
	format string // "text" or "json"
}

func newBlameCmd() *cobra.Command {
	flags := &blameFlags{format: "text"}
	cmd := &cobra.Command{
		Use:   "blame <entity>",
		Short: "Show chronological history of one entity (who changed it and when)",
		Long: `blame walks the git history of the scope mirror for every commit that
touched the named entity's rendered stub, and lists changes in chronological
order — like 'git blame' on a person's memory file.

The entity must exist in the live store. Git history is only available after
maz sync push has been run at least once; blame reports an actionable message
when no history is recorded yet.

<entity> may be a UID, display-name fragment, or entities/{uid} resource name.

Examples:
  maz blame entities/01HZAB3NDEKTSV4RRFFQ69G5FAV
  maz blame 01HZAB --scope personal
  maz blame sarah --rev origin/main --remote
  maz blame sarah --format json | jq '.[] | select(.action=="created")'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			logger := logging.FromContext(cmd.Context())
			return runBlame(cmd.Context(), cmd, logger, dataDir, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.scope, "scope", "", "only this scope; empty = first configured scope")
	cmd.Flags().StringVar(&flags.since, "since", "", "time limit, e.g. 30d or 2026-01-01")
	cmd.Flags().StringVar(&flags.rev, "rev", "HEAD", "revspec to inspect (HEAD, origin/main, @{u}, ...)")
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "fetch remote before resolving")
	cmd.Flags().StringVar(&flags.format, "format", "text", "output format: text|json")
	cmd.AddCommand(newBlameDocumentCmd())
	return cmd
}

func runBlame(ctx context.Context, cmd *cobra.Command, logger *zap.Logger, dataDir, entity string, flags *blameFlags) error {
	w := cmd.OutOrStdout()
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// --- Try to resolve entity via the live store to get a canonical UID ---
	// blameResolveEntity now resolves display names (via showFromLiveStore's
	// entities-tool resolver) so `maz blame sarah` works for a name.
	// When the live lookup fails with not-found (deleted/historical entity),
	// we fall through to the git mirror path instead of erroring immediately.
	uid, displayName, resolveErr := blameResolveEntity(ctx, cmd, logger, dataDir, entity)
	liveAbsent := resolveErr != nil && isNavNotFound(resolveErr)
	if resolveErr != nil && !liveAbsent {
		// Hard infrastructure error — surface it.
		return fmt.Errorf("blame: %w", resolveErr)
	}

	repoDir, scopeErr := blameRepoDir(dataDir, flags.scope)
	if scopeErr != nil {
		return blameNoMirror(cmd, w, entity, flags, liveAbsent, resolveErr)
	}
	if flags.remote {
		fetchErrIgnored(ctx, repoDir)
	}

	// Resolve the stub path. Mirror scan wins (includes kind); fall back to
	// a path synthesised from the live UID; error when neither is available.
	stubPath, pathErr := blameStubPath(ctx, repoDir, flags.rev, entity, uid, displayName, liveAbsent)
	if pathErr != nil {
		_, _ = fmt.Fprintf(w, "blame: entity %q not found\n", entity) //nolint:errcheck // best-effort user message
		return silentError{code: 1, err: resolveErr}
	}

	entries, err := blameEntity(ctx, repoDir, stubPath, "", flags)
	if err != nil {
		return fmt.Errorf("blame: %w", err)
	}

	if len(entries) == 0 {
		if liveAbsent {
			_, _ = fmt.Fprintf(w, "blame: entity %q not found in git history\n", entity) //nolint:errcheck // best-effort user message
			return silentError{code: 1, err: fmt.Errorf("entity %q not found in git history", entity)}
		}
		if blameIsJSONFormat(cmd, flags) {
			return writeBlameJSON(w, []blameEntry{})
		}
		return writeBlameUnversioned(w, entity)
	}

	if blameIsJSONFormat(cmd, flags) {
		return writeBlameJSON(w, entries)
	}
	return writeBlameText(w, entity, stubPath, entries)
}

// blameRepoDir returns the local git repo directory for the first (or named)
// scope. An error means no sync config or no scopes are configured.
func blameRepoDir(dataDir, scope string) (string, error) {
	_, syncCfg, cfgErr := loadResolvedSyncConfig(dataDir)
	if cfgErr != nil {
		return "", cfgErr
	}
	scopeName := scope
	if scopeName == "" {
		scopes := sortedScopeNames(syncCfg)
		if len(scopes) == 0 {
			return "", errors.New("no scopes configured")
		}
		scopeName = scopes[0]
	}
	return filepath.Join(dataDir, "sync", scopeName, "repo"), nil
}

// blameNoMirror handles the case where no git mirror is available. When the
// entity is absent from the live store too, it returns a not-found error.
// Otherwise it returns the "unversioned" message.
func blameNoMirror(cmd *cobra.Command, w io.Writer, entity string, flags *blameFlags, liveAbsent bool, resolveErr error) error {
	if liveAbsent {
		_, _ = fmt.Fprintf(w, "blame: entity %q not found\n", entity) //nolint:errcheck // best-effort user message
		return silentError{code: 1, err: resolveErr}
	}
	if blameIsJSONFormat(cmd, flags) {
		return writeBlameJSON(w, []blameEntry{})
	}
	return writeBlameUnversioned(w, entity)
}

// blameStubPath resolves the rendered stub path for the entity. The mirror
// scan is preferred (it contains the kind so the path is fully qualified);
// when absent from the mirror but present live, the path is synthesised from
// the UID. Returns an error when neither source resolves the entity.
func blameStubPath(ctx context.Context, repoDir, rev, entity, uid, displayName string, liveAbsent bool) (string, error) {
	if mirrorPath, merr := resolveEntityStubPath(ctx, repoDir, rev, entity); merr == nil {
		return mirrorPath, nil
	}
	if !liveAbsent {
		return entityStubPathFromUID(uid, "", displayName), nil
	}
	return "", fmt.Errorf("entity %q not found in live store or git mirror", entity)
}

// blameResolveEntity invokes the get tool for the entity and returns its uid
// and displayName from the live store.
func blameResolveEntity(ctx context.Context, cmd *cobra.Command, logger *zap.Logger, dataDir, entity string) (uid, displayName string, err error) {
	raw, liveErr := showFromLiveStore(ctx, cmd, logger, dataDir, entity)
	if liveErr != nil {
		return "", "", liveErr
	}
	var outer struct {
		Entity struct {
			UID         string `json:"uid"`
			DisplayName string `json:"displayName"`
		} `json:"entity"`
	}
	if uerr := json.Unmarshal(raw, &outer); uerr != nil {
		return "", "", uerr
	}
	return outer.Entity.UID, outer.Entity.DisplayName, nil
}

// writeBlameUnversioned prints the honest "not yet versioned" message.
func writeBlameUnversioned(w io.Writer, entity string) error {
	_, err := fmt.Fprintf(w, "blame: entity %q exists locally but has no version history yet; run `maz sync push` to record history\n", entity)
	return err
}

// resolveEntityStubPath finds the rendered stub path for the entity identifier
// by searching rendered/entities.jsonl at the given revspec.
func resolveEntityStubPath(ctx context.Context, repoDir, rev, entity string) (string, error) {
	rows := readJSONLAtRev(ctx, repoDir, rev, "rendered/entities.jsonl")
	entityLower := stripEntityResolverPrefix(entity)
	for _, row := range rows {
		uid := mapStr(row, "uid")
		dn := mapStr(row, "displayName")
		kind := mapStr(row, "kind")
		if strings.Contains(strings.ToLower(uid), entityLower) ||
			strings.Contains(strings.ToLower(dn), entityLower) {
			// Build the shard path: rendered/by-uid/<first3>/<next2>/<uid>.md
			return entityStubPathFromUID(uid, kind, dn), nil
		}
	}
	return "", fmt.Errorf("entity %q not found in git mirror", entity)
}

// stripEntityResolverPrefix normalises an entity identifier accepted by
// `maz show` / `maz blame` into its lowercase needle. It strips both
// the AIP collection prefix (`entities/<ULID>` — emitted by `maz list
// entities`, `maz remember`, every JSON response shape) and the legacy
// kind colon (`person:frodo`). The remainder is matched against the
// uid + displayName columns in rendered/entities.jsonl.
func stripEntityResolverPrefix(entity string) string {
	needle := strings.ToLower(entity)
	if rest, ok := strings.CutPrefix(needle, "entities/"); ok {
		needle = rest
	}
	if idx := strings.Index(needle, ":"); idx >= 0 {
		needle = needle[idx+1:]
	}
	return needle
}

// entityStubPathFromUID constructs the rendered stub path for a UID.
// Mirrors the shard.go logic: rendered/by-uid/<uid[0:3]>/<uid[3:5]>/<uid>.md.
func entityStubPathFromUID(uid, kind, displayName string) string {
	if len(uid) < 5 {
		return "rendered/by-kind/" + kind + "/" + displayName + ".md"
	}
	return "rendered/by-uid/" + uid[0:3] + "/" + uid[3:5] + "/" + uid + ".md"
}

// blameEntity walks git log for the stub file and parses each commit's
// structured body to extract a per-entity action. The body shape is
// the contract set by internal/sync/transport/git/commit_message.go;
// commits that pre-date the structured form (or use a newer schema)
// fall through to the neutral "modified" action so blame never errors.
// The uidOverride argument supplies the UID used to match commit-body lines;
// document body files use slug filenames rather than the 26-char stub name, so
// the caller passes their resolved UID. An empty override derives the UID from
// the stub path as before.
func blameEntity(ctx context.Context, repoDir, stubPath, uidOverride string, flags *blameFlags) ([]blameEntry, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not in PATH: %w", err)
	}

	// -z: separate records with NUL so the body's own newlines do not
	// collide with the record delimiter. Within each record the first
	// line carries date|author|subject and everything after the first
	// %n is the body — produced by `%ci|%an|%s%n%b`.
	args := []string{
		"-C", repoDir, "log", "--follow", "-z",
		"--pretty=format:%ci|%an|%s%n%b",
		flags.rev, "--", stubPath,
	}
	if flags.since != "" {
		args = append(args, "--since="+flags.since)
	}

	gitCmd := exec.CommandContext(ctx, gitBin, args...)
	gitCmd.Env = cleanGitEnv(repoDir)
	var stdout bytes.Buffer
	gitCmd.Stdout = &stdout
	if err := gitCmd.Run(); err != nil {
		// No commits touching this file — valid for a brand-new entity.
		return nil, nil //nolint:nilerr // no history = empty blame, not an error
	}

	entityUID := uidOverride
	if entityUID == "" {
		entityUID = uidFromStubPath(stubPath)
	}
	records := strings.Split(stdout.String(), "\x00")
	entries := make([]blameEntry, 0, len(records))
	for _, rec := range records {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		entries = append(entries, parseBlameRecord(rec, entityUID))
	}
	// Reverse so oldest is first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// uidFromStubPath extracts the entity UID from a rendered stub path
// like `rendered/by-uid/01K/T8/01KT8X0SERT5FJB0VGPYQQGAEN.md`. Returns
// the empty string for shapes that do not match (the per-entity action
// lookup degrades to the neutral "modified" in that case).
func uidFromStubPath(stubPath string) string {
	base := filepath.Base(stubPath)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if len(base) == 26 {
		return base
	}
	return ""
}

// parseBlameRecord parses one NUL-delimited git log record into a
// blameEntry. The first line is `ci|an|subject`; everything after the
// first newline is the body.
func parseBlameRecord(record, entityUID string) blameEntry {
	subjectLine, body, _ := strings.Cut(record, "\n")
	parts := strings.SplitN(subjectLine, "|", 3)
	if len(parts) != 3 {
		return blameEntry{Action: subjectLine}
	}
	date := parts[0]
	if len(date) > 16 {
		date = date[:16] // "2026-05-12 10:23"
	}
	device := parts[1]
	subject := strings.TrimSpace(parts[2])

	return blameEntry{
		Date:   date,
		Device: device,
		Action: parseCommitAction(subject, body, entityUID),
		Detail: subject,
	}
}

// parseCommitAction picks the per-entity action label for a commit.
// Structured commits (Mazarbul-Schema: sync-commit/v1) carry a body
// section with `+`/`~`/`-` prefixes per entity UID; we lift the
// prefix verbatim. Legacy commits and unknown schemas fall through to
// commitActionUpdated is the action label emitted when an entity was modified
// in a structured sync commit.
const commitActionUpdated = "updated"

// keyword heuristics on the subject so old history still renders.
func parseCommitAction(subject, body, entityUID string) string {
	if strings.Contains(body, commitSchemaV1Trailer) && entityUID != "" {
		switch actionPrefixForEntity(body, entityUID) {
		case "+":
			return diffActionAdded
		case "~":
			return commitActionUpdated
		case "-":
			return "forgotten"
		}
		return "modified"
	}
	// Legacy heuristic kept as a fallback only — every commit produced
	// after PR #144 carries the v1 trailer so this branch is for
	// pre-existing remotes.
	switch {
	case strings.Contains(subject, "forgotten"):
		return "forgotten"
	case strings.Contains(subject, "superseded"):
		return "superseded"
	default:
		return "modified"
	}
}

// commitSchemaV1Trailer is the trailer string emitted by
// buildCommitMessage. Kept as a private copy in the CLI so this
// package does not depend on internal/sync/transport/git just to
// read history.
const commitSchemaV1Trailer = "Mazarbul-Schema: sync-commit/v1"

// actionPrefixForEntity scans a structured commit body for a line
// mentioning entityUID and returns its action prefix (one of "+",
// "~", "-"). Empty when the entity isn't enumerated — meaning the
// commit touched the stub via a re-render without a direct semantic
// change for this UID.
func actionPrefixForEntity(body, entityUID string) string {
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, entityUID) {
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" {
			continue
		}
		switch trimmed[0] {
		case '+', '~', '-':
			return string(trimmed[0])
		}
	}
	return ""
}

func writeBlameText(w io.Writer, entity, stubPath string, entries []blameEntry) error {
	var buf strings.Builder
	fmt.Fprintf(&buf, "\nentity: %s  (%s)\n\n", entity, stubPath)
	for _, e := range entries {
		fmt.Fprintf(&buf, "  %s (%s)  %s", e.Date, e.Device, e.Action)
		if e.Detail != "" {
			fmt.Fprintf(&buf, "\n       %s", e.Detail)
		}
		buf.WriteString("\n")
	}
	if len(entries) == 0 {
		buf.WriteString("  (no history found)\n")
	}
	if _, err := fmt.Fprint(w, buf.String()); err != nil {
		return fmt.Errorf("blame: write: %w", err)
	}
	return nil
}

func writeBlameJSON(w io.Writer, entries []blameEntry) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if entries == nil {
		entries = []blameEntry{}
	}
	return enc.Encode(entries)
}
