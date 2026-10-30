package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// localVaultRoot resolves the absolute vault root directory using the same
// config the daemon reads. It is the client-local fallback when no daemon is
// available to report its authoritative root.
func localVaultRoot(cmd *cobra.Command) (string, error) {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	user, err := config.LoadUserConfig(dataDir)
	if err != nil && !errors.Is(err, config.ErrUserConfigNotFound) {
		return "", fmt.Errorf("load user config: %w", err)
	}
	return config.VaultPath(dataDir, &user), nil
}

// localVaultFile resolves the absolute on-disk location of a vault-relative
// document path using the same config the daemon reads, so `maz doc path` and
// the daemon's projector always agree.
//
// The vault is partitioned by scope (<root>/<scope>/<path>). This offline
// fallback assumes the DEFAULT (personal) scope, matching what the online
// materializeVault tool defaults to when no scope is supplied — `doc path/edit/
// open` carry no scope selector, so a non-personal document is only addressable
// through the running daemon (which reports its authoritative path).
func localVaultFile(cmd *cobra.Command, path string) (string, error) {
	rel, err := document.CleanVaultPath(path)
	if err != nil {
		return "", err
	}
	root, err := localVaultRoot(cmd)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, string(kernel.DefaultScope), filepath.FromSlash(rel)), nil
}

// resolveVaultFileFn is a test seam over resolveVaultFile so the editor-backed
// `doc new` path can be exercised hermetically without standing up daemon
// wiring (the same seam pattern as preflightDaemon).
//
//nolint:gochecknoglobals // test seam: tests stub this to inject a fake vault-root resolution
var resolveVaultFileFn = resolveVaultFile

// resolveVaultFile returns a document's absolute on-disk vault path, refreshing
// it from the latest revision first. It routes through the materializeVault tool
// and prefers the absolute path that tool reports: when a daemon is running, the
// path reflects the DAEMON's vault root (which may live under a different data
// dir than this client process would compute), so `maz doc path/edit/open/new`
// never point at a phantom file in the local default vault. The tool reports the
// path even for a document that does not exist yet (materialization is a no-op,
// but the location is well defined), so this works for `doc new` too.
//
// Error handling is deliberate: a tool/daemon error (transient transport,
// validation, projector failure) is FATAL and surfaced to the caller — it must
// NOT silently degrade to a client-local path, which would point editing or
// imports at the wrong vault when a daemon owns a non-default data dir. The
// local fallback fires ONLY when the navigation tool is unavailable (nil
// result: minimal boot or non-dispatcher storage), where there is no daemon to
// disagree with and the in-process computation is authoritative.
func resolveVaultFile(cmd *cobra.Command, path string) (string, error) {
	reqBody, merr := json.Marshal(map[string]any{"path": path})
	if merr != nil {
		return "", merr
	}
	raw, ierr := invokeNavTool(cmd, "materializeVault", reqBody)
	if ierr != nil {
		return "", ierr
	}
	if raw == nil {
		// Tool not registered (minimal boot / non-dispatcher storage): no daemon
		// can disagree, so the client-local computation is authoritative.
		return localVaultFile(cmd, path)
	}
	if file := materializedFile(raw); file != "" {
		return file, nil
	}
	// A live tool that returned no file means a whole-vault sync shape, which
	// resolveVaultFile never requests (it always passes a path). Treat the
	// missing field as a contract violation rather than guessing a path.
	return "", fmt.Errorf("materializeVault returned no file for %q", path)
}

// materializedFile extracts the absolute "file" path from a materializeVault
// tool result, or "" when absent (whole-vault sync or a decode error).
func materializedFile(raw json.RawMessage) string {
	var out struct {
		File string `json:"file"`
	}
	if json.Unmarshal(raw, &out) == nil {
		return out.File
	}
	return ""
}

// resolveEditor returns the user's editor: $VISUAL, then $EDITOR, then vi.
func resolveEditor() string {
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	if e := os.Getenv("EDITOR"); e != "" {
		return e
	}
	return "vi"
}

// editorCommand splits an editor setting into the executable and its leading
// arguments, with file appended last. $VISUAL/$EDITOR conventionally carry
// flags ("code --wait", "vim -n", "emacsclient -c"), so the value is
// whitespace-split rather than treated as one executable name. Quoted
// arguments with embedded spaces are not supported — the same limitation as
// GIT_EDITOR without a shell — and a whitespace-only value falls back to vi.
func editorCommand(editor, file string) (string, []string) {
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		parts = []string{"vi"}
	}
	return parts[0], append(parts[1:], file)
}

// runEditor opens file in the user's editor, attached to the terminal.
func runEditor(cmd *cobra.Command, file string) error {
	editor := resolveEditor()
	name, args := editorCommand(editor, file)
	editorCmd := exec.CommandContext(cmd.Context(), name, args...)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("editor %q: %w", editor, err)
	}
	return nil
}

// stdinIsPiped reports whether stdin carries piped data (not a terminal).
func stdinIsPiped() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0
}

// invokeWriteDocument calls the writeDocument tool and renders the result.
func invokeWriteDocument(cmd *cobra.Command, label string, args map[string]any) error {
	reqBody, err := json.Marshal(args)
	if err != nil {
		return wrapNavFatal(cmd, label, err)
	}
	raw, err := invokeNavTool(cmd, "writeDocument", reqBody)
	if err != nil {
		return wrapNavFatal(cmd, label, err)
	}
	if handled, herr := emitNavResult(cmd, raw); handled {
		return herr
	}
	return renderDocWrite(cmd, label, raw)
}

func renderDocWrite(cmd *cobra.Command, label string, raw json.RawMessage) error {
	var res struct {
		Path        string `json:"path"`
		RevisionUID string `json:"revisionUid"`
		Changed     bool   `json:"changed"`
		Edges       int    `json:"edges"`
		Unresolved  int    `json:"unresolved"`
		File        string `json:"file"`
		PendingEdit bool   `json:"pendingEdit"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("%s: parse response: %w", label, err)
	}
	out := cmd.OutOrStdout()
	if !res.Changed {
		_, err := fmt.Fprintf(out, "%s unchanged (content already current)\n", res.Path)
		return err
	}
	if _, err := fmt.Fprintf(out, "%s revision %s (edges %d, unresolved %d)\n",
		res.Path, res.RevisionUID, res.Edges, res.Unresolved); err != nil {
		return err
	}
	if res.File != "" {
		if _, err := fmt.Fprintf(out, "file: %s\n", res.File); err != nil {
			return err
		}
	}
	if res.PendingEdit {
		//nolint:errcheck // best-effort advisory
		fmt.Fprintln(cmd.ErrOrStderr(), "note: the vault file holds an un-ingested manual edit and was left untouched; run `maz doc import` on it or `maz doc sync` after ingesting")
	}
	return nil
}

// ---- doc new ----

type docNewFlags struct {
	title    string
	pin      bool
	tags     []string
	body     string
	template string
	scope    string
}

func newDocNewCmd() *cobra.Command {
	flags := &docNewFlags{}
	cmd := &cobra.Command{
		Use:   "new [path]",
		Short: "Create a document",
		Long: `Create a document at a vault-relative path. The body comes from --body,
from piped stdin, or — interactively — from your editor ($VISUAL, then
$EDITOR, then vi) opened on the new vault file.

Created document bodies are indexed for "maz recall" (FTS5 + vector), so
recall queries will surface document hits alongside episodes and entities.

--title, --pin, and --tag synthesize a YAML frontmatter block; alternatively
write your own frontmatter in the body (but not both).

--template <name> starts the document from a stored template. Templates live
as ordinary vault documents under templates/ (e.g. templates/daily.md). A name
without a slash is looked up there; a name with a slash is a literal
vault-relative path (e.g. templates/project/weekly.md). Placeholders in the
template body are expanded: {{date}} (YYYY-MM-DD), {{time}} (HH:MM), {{title}}.
When --template is given and no path is provided, the path defaults to
journal/YYYY-MM-DD.md. Explicit --title/--tag/--pin flags override the
template's own frontmatter.

Examples:
  maz doc new notes/standup --title "Standup notes" --tag work
  echo "# Scratch" | maz doc new scratch.md
  maz doc new journal/today          # opens your editor
  maz doc new --template daily       # creates journal/YYYY-MM-DD.md from templates/daily.md
  maz doc new journal/2026-06-08 --template daily --title "Monday review"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 1 {
				path = args[0]
			}
			return runDocNew(cmd, path, flags)
		},
	}
	cmd.Flags().StringVar(&flags.title, "title", "", "document title (synthesized into frontmatter)")
	cmd.Flags().BoolVar(&flags.pin, "pin", false, "pin the document (surfaces first in context packs)")
	cmd.Flags().StringSliceVar(&flags.tags, "tag", nil, "tag (repeatable; synthesized into frontmatter)")
	cmd.Flags().StringVar(&flags.body, "body", "", "document body (skips the editor)")
	cmd.Flags().StringVar(&flags.template, "template", "", "start from a stored template document (name or vault-relative path)")
	cmd.Flags().StringVar(&flags.scope, "scope", "", "storage scope: personal (default), project, team, or draft — fixed at creation (see `maz help-topics scopes`)")
	return cmd
}

func runDocNew(cmd *cobra.Command, path string, flags *docNewFlags) error {
	// When --template is given, load the template body and apply defaults.
	if flags.template != "" {
		return runDocNewFromTemplate(cmd, path, flags)
	}
	if path == "" {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintln(cmd.ErrOrStderr(), "doc new: path argument is required when --template is not set")
		return silentError{code: 1, err: errors.New("path required")}
	}
	body, err := docNewBody(cmd, path, flags)
	if err != nil {
		return wrapNavFatal(cmd, "doc new", err)
	}
	if strings.TrimSpace(body) == "" {
		//nolint:errcheck // best-effort stderr write
		fmt.Fprintln(cmd.ErrOrStderr(), "doc new: aborted (empty document)")
		return silentError{code: 1, err: errors.New("empty document")}
	}
	toolArgs := map[string]any{"path": path, "body": body}
	if flags.title != "" {
		toolArgs["title"] = flags.title
	}
	if flags.pin {
		toolArgs["pinned"] = true
	}
	if len(flags.tags) > 0 {
		toolArgs["tags"] = flags.tags
	}
	if flags.scope != "" {
		toolArgs["scope"] = flags.scope
	}
	return invokeWriteDocument(cmd, "doc new", toolArgs)
}

// runDocNewFromTemplate handles the --template path: load the template,
// apply placeholders, merge frontmatter defaults, and write the document.
func runDocNewFromTemplate(cmd *cobra.Command, path string, flags *docNewFlags) error {
	templatePath := resolveTemplatePath(flags.template)
	tmplBody, err := readTemplateBody(cmd, templatePath)
	if err != nil {
		return wrapNavFatal(cmd, "doc new", err)
	}

	// Default the destination path when none was given.
	now := nowFn()
	if path == "" {
		path = defaultJournalPath(now)
	}

	// Extract frontmatter defaults from the template. Explicit CLI flags
	// take precedence over everything the template supplies.
	tmplFM := parseTemplateFrontmatter(tmplBody)

	// Resolve the title: --title flag > template frontmatter title > destination
	// path basename. The basename fallback guarantees {{title}} (and a literal
	// "# {{title}}" heading) never expand to an empty string, per the contract.
	title := flags.title
	if title == "" {
		title = tmplFM.Title
	}
	if title == "" {
		title = titleFromPath(path)
	}

	// Apply placeholder substitution with the resolved title.
	body := applyTemplatePlaceholders(tmplBody, title, now)

	toolArgs := map[string]any{"path": path}
	if flags.scope != "" {
		toolArgs["scope"] = flags.scope
	}

	if hasLeadingFrontmatter(body) {
		// The template supplies its own frontmatter. writeDocument rejects
		// title/pinned/tags metadata fields alongside a body that already has a
		// frontmatter block, so merge any explicit CLI-flag overrides directly
		// into the block and send the body alone. With no overrides the body is
		// forwarded byte-for-byte and the template's own frontmatter stands.
		merged, merr := mergeTemplateFrontmatter(body, flags)
		if merr != nil {
			return wrapNavFatal(cmd, "doc new", merr)
		}
		toolArgs["body"] = merged
		return invokeWriteDocument(cmd, "doc new", toolArgs)
	}

	// No frontmatter in the body: let writeDocument synthesize one from the
	// resolved metadata. title is always non-empty (basename fallback); CLI flags
	// take precedence over the template's frontmatter defaults.
	toolArgs["body"] = body
	toolArgs["title"] = title
	if flags.pin || tmplFM.Pinned {
		toolArgs["pinned"] = true
	}
	mergedTags := flags.tags
	if len(mergedTags) == 0 {
		mergedTags = tmplFM.Tags
	}
	if len(mergedTags) > 0 {
		toolArgs["tags"] = mergedTags
	}

	return invokeWriteDocument(cmd, "doc new", toolArgs)
}

// docNewBody resolves the new document's body: --body wins, then piped stdin,
// then an interactive editor session on the (new) vault file itself.
func docNewBody(cmd *cobra.Command, path string, flags *docNewFlags) (string, error) {
	if flags.body != "" {
		return flags.body, nil
	}
	if stdinIsPiped() {
		raw, err := os.ReadFile("/dev/stdin")
		if err != nil {
			// Portable fallback for platforms without /dev/stdin semantics.
			var buf bytes.Buffer
			if _, rerr := buf.ReadFrom(os.Stdin); rerr != nil {
				return "", fmt.Errorf("read stdin: %w", rerr)
			}
			return buf.String(), nil
		}
		return string(raw), nil
	}
	return docNewViaEditor(cmd, path, flags.title)
}

// docNewViaEditor opens the editor on the future vault file (seeded with a
// heading when --title is set), reads it back, and removes it again when the
// session produced nothing — the file only becomes real through ingestion.
//
// The seed path is resolved through the daemon (resolveVaultFileFn), not
// computed locally: with a daemon serving a non-default data dir the editor
// must open the future file under the DAEMON's vault root, or the human edits a
// phantom file while the subsequent writeDocument call (also daemon-routed)
// stores the document somewhere else — the same split-brain fixed for
// doc edit/open/path; materializeVault reports the location even for a
// not-yet-existing document, so this resolves correctly before the file exists.
func docNewViaEditor(cmd *cobra.Command, path, title string) (string, error) {
	file, err := resolveVaultFileFn(cmd, path)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(file); statErr == nil {
		return "", fmt.Errorf("%s already exists; use `maz doc edit %s`", file, path)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", fmt.Errorf("create vault dir: %w", err)
	}
	var seed []byte
	if title != "" {
		seed = []byte("# " + title + "\n\n")
	}
	if err := os.WriteFile(file, seed, 0o600); err != nil {
		return "", fmt.Errorf("seed %s: %w", file, err)
	}
	if err := runEditor(cmd, file); err != nil {
		//nolint:errcheck // best-effort cleanup of the seeded file
		os.Remove(file)
		return "", err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read back %s: %w", file, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		//nolint:errcheck // best-effort cleanup of the empty file
		os.Remove(file)
	}
	return string(raw), nil
}

// ---- doc edit ----

func newDocEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <path>",
		Short: "Edit a document in your editor",
		Long: `Open a document's vault file in your editor ($VISUAL, then $EDITOR, then
vi) and ingest the result as a new revision on save. History is kept; the
write is skipped when nothing changed.

Prefer your own editor session? Use the escape hatch:
  vim "$(maz doc path notes/foo)"   # then: maz doc import "$(maz doc path notes/foo)"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDocEdit(cmd, args[0])
		},
	}
}

func runDocEdit(cmd *cobra.Command, path string) error {
	// Resolve (and refresh) the vault file via the daemon so the editor opens
	// the real on-disk file under the daemon's vault root — not a phantom path
	// in this client's default data dir. A pending manual edit is intentionally
	// left in place — editing it continues from the human's bytes and the save
	// ingests everything.
	file, err := resolveVaultFileFn(cmd, path)
	if err != nil {
		return wrapNavFatal(cmd, "doc edit", err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		return wrapNavFatal(cmd, "doc edit", fmt.Errorf("no document at %q (create it with `maz doc new`): %w", path, err))
	}
	if err := runEditor(cmd, file); err != nil {
		return wrapNavFatal(cmd, "doc edit", err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		return wrapNavFatal(cmd, "doc edit", err)
	}
	if bytes.Equal(before, after) {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), "no changes")
		return werr
	}
	return invokeWriteDocument(cmd, "doc edit", map[string]any{"path": path, "body": string(after)})
}

// ---- doc path ----

func newDocPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path <path>",
		Short: "Print a document's vault file location",
		Long: `Print the absolute on-disk location of a document's editable vault file —
the "use your own editor" escape hatch:

  vim "$(maz doc path notes/foo)"

The file is refreshed from the latest revision first (best-effort). After
editing, ingest with:

  maz doc import "$(maz doc path notes/foo)"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve via the daemon so the printed path points at the daemon's
			// actual vault file (refreshed from the latest revision), not a
			// phantom path under this client's default data dir.
			file, err := resolveVaultFileFn(cmd, args[0])
			if err != nil {
				return wrapNavFatal(cmd, "doc path", err)
			}
			_, werr := fmt.Fprintln(cmd.OutOrStdout(), file)
			return werr
		},
	}
}

// ---- doc open ----

func newDocOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open <path>",
		Short: "Open a document with the system handler",
		Long:  "Refresh a document's vault file and open it with the OS handler (`open` on macOS, `xdg-open` elsewhere) — e.g. your Obsidian or Markdown app.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve via the daemon (refreshing first) so we open the daemon's
			// real vault file, not a phantom under this client's default dir.
			file, err := resolveVaultFileFn(cmd, args[0])
			if err != nil {
				return wrapNavFatal(cmd, "doc open", err)
			}
			if _, err := os.Stat(file); err != nil {
				return wrapNavFatal(cmd, "doc open", fmt.Errorf("no vault file at %s (create the document first)", file))
			}
			opener := "xdg-open"
			if runtime.GOOS == "darwin" {
				opener = "open"
			}
			if err := exec.CommandContext(cmd.Context(), opener, file).Start(); err != nil {
				return wrapNavFatal(cmd, "doc open", fmt.Errorf("%s: %w", opener, err))
			}
			return nil
		},
	}
}

// ---- doc sync ----

func newDocSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync [path]",
		Short: "Materialize event-sourced documents as vault files",
		Long: `Project documents from the event-sourced store to editable markdown files
under the vault root (all documents, or one by vault-relative path).

Files holding manual edits that were never ingested are left untouched and
reported as pending; ingest them with maz doc import.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := map[string]any{}
			if len(args) == 1 {
				// A single-path sync names a specific document; mustExist turns a
				// missing path into E_NOT_FOUND so a typo is surfaced rather than
				// reported as a successful no-op sync.
				in["path"] = args[0]
				in["mustExist"] = true
			}
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "doc sync", err)
			}
			raw, err := invokeNavTool(cmd, "materializeVault", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "doc sync", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			var res struct {
				Root         string `json:"root"`
				Written      int    `json:"written"`
				Unchanged    int    `json:"unchanged"`
				PendingEdits int    `json:"pendingEdits"`
				Deleted      int    `json:"deleted"`
				Kept         int    `json:"kept"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("doc sync: parse response: %w", err)
			}
			_, werr := fmt.Fprintf(cmd.OutOrStdout(),
				"vault %s: written %d, unchanged %d, pending edits %d, deleted %d, kept %d\n",
				res.Root, res.Written, res.Unchanged, res.PendingEdits, res.Deleted, res.Kept)
			return werr
		},
	}
}
