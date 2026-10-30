package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemonclient"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

// rebuildExitError is the sentinel error returned by runRebuild when the
// rebuild completes with non-zero exit conditions. Carries the exit code
// through the error chain so cobra's RunE contract stays clean and tests
// can intercept the value without calling os.Exit directly.
type rebuildExitError struct{ code int }

func (e *rebuildExitError) Error() string {
	return fmt.Sprintf("rebuild: exit code %d", e.code)
}

// Code returns the process exit code embedded in the error.
func (e *rebuildExitError) Code() int { return e.code }

// ResolveRebuildExitCode unwraps a rebuildExitError to its embedded code.
// Returns (0, false) for nil or any non-rebuild error. Used by tests to
// translate the cobra error into the expected process exit code.
func ResolveRebuildExitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var r *rebuildExitError
	if errors.As(err, &r) {
		return r.Code(), true
	}
	return 0, false
}

// rebuildFlags holds the flag values wired onto `maz rebuild`.
type rebuildFlags struct {
	strict        bool
	yes           bool
	progressEvery int
}

func newRebuildCmd() *cobra.Command {
	flags := &rebuildFlags{}
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild derived stores from the event log",
		Long: `Replay every event in the JSONL event log and reconstruct the four
derived stores: SQLite (relational), LadybugDB (graph), sqlite-vec
(vector), and FTS5 (full-text search).

Use this after corruption (rm -rf the index), after a failed Append fanout,
or on a fresh checkout where the log is present but the indexes are not.

The rebuild is idempotent: events already present in all layers are skipped.
Non-strict mode (default) collects per-event Apply errors and continues;
use --strict to abort on the first failure.

In TTY mode (interactive) you are prompted to confirm the destructive
action before replay begins. Pass --yes to skip the prompt.

Exit codes:
  0   clean rebuild or skip-only (no Apply errors)
  1   non-strict Apply errors, user cancelled, or scripted without --yes
  2   rebuild-level failure (context error, storage fault, nil storage)
  130 interrupted by Ctrl-C (context.Canceled via signal)

Examples:
  # Interactive rebuild with confirmation prompt
  maz rebuild

  # Scripted rebuild (no prompt required)
  maz rebuild --yes

  # Abort on first error, useful for recovery scripts
  maz rebuild --yes --strict

  # Machine-readable output for agent callers
  maz rebuild --yes --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRebuild(cmd, flags)
		},
	}
	cmd.Flags().BoolVar(&flags.strict, "strict", false, "abort on first ApplyError")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "skip destructive-action confirmation")
	cmd.Flags().IntVar(&flags.progressEvery, "progress-every", 0, "progress cadence in events (0 = kernel default 100)")
	return cmd
}

// runRebuild is the command body. Resolves the data dir, confirms the
// destructive action, opens the event log + storage, and calls Rebuild.
func runRebuild(cmd *cobra.Command, flags *rebuildFlags) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	if confirmed, err := confirmRebuild(cmd, flags.yes); err != nil {
		return err
	} else if !confirmed {
		return nil // user declined; exit 0
	}

	ctx := cmd.Context()

	// Route to the live daemon when one holds the store: the rebuild runs ONLINE
	// (shadow build + swap) with the daemon up — no stop required. preflightDaemon
	// returns non-nil exactly when a live daemon holds the lock. When no daemon is
	// running, fall through to the in-process path below.
	if preflightDaemon("", dataDir) != nil {
		return runRebuildViaDaemon(ctx, cmd, dataDir, flags.strict)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return wrapRebuildFatal(cmd, fmt.Errorf("open event log: %w", err))
	}
	defer func() {
		if cerr := closeLog(); cerr != nil {
			//nolint:errcheck // best-effort: stderr write on close failure
			fmt.Fprintf(cmd.ErrOrStderr(), "close event log: %v\n", cerr)
		}
	}()

	// Resolve the embedder from the persisted user config. Fatal config errors
	// (unknown provider, missing model) abort here; transient failures degrade
	// to nil and warn on stderr. embed=none returns nil silently.
	emb, err := resolveRebuildEmbedder(ctx, cmd, dataDir)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return silentError{code: 130, err: err}
		}
		return wrapRebuildFatal(cmd, err)
	}

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir, Embedder: emb})
	if err != nil {
		return wrapRebuildFatal(cmd, rebuildStorageOpenError(dataDir, err))
	}
	defer func() {
		if cerr := disp.Close(); cerr != nil {
			//nolint:errcheck // best-effort: stderr write on close failure
			fmt.Fprintf(cmd.ErrOrStderr(), "close storage: %v\n", cerr)
		}
	}()

	rerr := execRebuild(ctx, cmd, log, disp, flags)
	// The editable vault is a derived store too: re-materialize it whenever the
	// replay itself completed (clean or with per-event errors). Fatal rebuild
	// errors skip the sync — the projection would read half-rebuilt state.
	var applyErrExit *rebuildExitError
	if rerr == nil || errors.As(rerr, &applyErrExit) {
		syncVaultAfterRebuild(ctx, cmd, disp, dataDir)
		// Let in-flight background embeds finish before the deferred disp.Close()
		// fires. Apply schedules entity/mention/document-chunk vector upserts on
		// detached goroutines; replay returns long before they drain. Close() sets
		// d.closed=true and only then waits, which makes every still-queued worker
		// short-circuit and SKIP its vec0 upsert (logging "dispatcher: closed") —
		// silently dropping embeddings for a chunk of the rebuilt corpus, leaving
		// those entities unsearchable by the vector lane. Draining to idle first
		// lets each scheduled embed complete, so a rebuild yields a fully-populated
		// vector index. Bounded so a stalled embedder cannot hang the command; the
		// rows are durable in the event log and a later rebuild regenerates them.
		drainRebuildVec(ctx, cmd, disp)
	}
	return rerr
}

// rebuildVecDrainTimeout bounds the post-replay wait for background embedding
// goroutines. A full from-scratch rebuild re-embeds the whole corpus, so the
// tail can take longer than a single-write CLI drain; the ceiling keeps a wedged
// embedder (e.g. Ollama unreachable) from hanging the command indefinitely.
const rebuildVecDrainTimeout = 5 * time.Minute

// drainRebuildVec waits for the dispatcher's in-flight background vector upserts
// to finish before the caller returns (and the deferred Close runs). See the
// call site for why Close alone is insufficient. Context.WithoutCancel keeps the
// drain running even if the rebuild's own ctx is already done, bounded by
// rebuildVecDrainTimeout.
func drainRebuildVec(ctx context.Context, cmd *cobra.Command, disp *dispatcher.Dispatcher) {
	if disp == nil {
		return
	}
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebuildVecDrainTimeout)
	defer cancel()
	if err := disp.WaitForBackgroundVec(drainCtx); err != nil {
		//nolint:errcheck // best-effort advisory on stderr; rows are durable in the event log
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: background embedding drain did not finish before close: %v\n", err)
	}
}

// syncVaultAfterRebuild re-materializes the editable vault from the freshly
// rebuilt SQLite projection. Best-effort: any failure warns on stderr without
// changing the rebuild's exit code (the four primary stores are already
// rebuilt; `maz doc sync` retries the projection alone). Manual edits that
// were never ingested are reported as pending, never overwritten.
func syncVaultAfterRebuild(ctx context.Context, cmd *cobra.Command, disp *dispatcher.Dispatcher, dataDir string) {
	warn := func(err error) {
		//nolint:errcheck // best-effort: stderr write on degraded path
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: vault sync skipped: %v\n", err)
	}
	user, err := config.LoadUserConfig(dataDir)
	if err != nil && !errors.Is(err, config.ErrUserConfigNotFound) {
		warn(err)
		return
	}
	reader, err := disp.SQLiteReader()
	if err != nil {
		warn(err)
		return
	}
	// AllScopes: vault sync is a maintenance path that materializes documents from
	// all user scopes.
	nav, err := navigate.NewAllScopes(reader)
	if err != nil {
		warn(err)
		return
	}
	stats, err := vault.NewProjector(nav, config.VaultPath(dataDir, &user), nil).SyncAll(ctx)
	if err != nil {
		warn(err)
		return
	}
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		//nolint:errcheck // best-effort: progress sink, mirrors rebuild ticks
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"type":         "vault",
			"written":      stats.Written,
			"unchanged":    stats.Unchanged,
			"pendingEdits": stats.PendingEdits,
			"deleted":      stats.Deleted,
			"kept":         stats.Kept,
		})
		return
	}
	//nolint:errcheck // best-effort: summary line
	fmt.Fprintf(cmd.OutOrStdout(), "ok  vault synced (written %d, unchanged %d, pending edits %d, deleted %d)\n",
		stats.Written, stats.Unchanged, stats.PendingEdits, stats.Deleted)
}

// confirmRebuild enforces the confirmation gate. Returns (true, nil) when the
// rebuild should proceed, (false, nil) when the user declined interactively,
// and (false, err) when a scripted invocation is missing --yes or a write
// error occurred.
func confirmRebuild(cmd *cobra.Command, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		// Scripted, non-TTY, CI, or agent: never block on stdin.
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
			"rebuild rewrites derived stores; pass --yes to proceed"); werr != nil {
			return false, werr
		}
		return false, &rebuildExitError{code: 1}
	}
	// Interactive TTY: prompt on stderr so `maz rebuild | jq` does not eat it.
	proceed, err := uiform.PromptYN(cmd.ErrOrStderr(),
		"Rebuild will re-populate derived stores from the event log. Proceed?", false)
	if err != nil {
		return false, err
	}
	if !proceed {
		if _, werr := fmt.Fprintln(cmd.OutOrStdout(), "rebuild cancelled"); werr != nil {
			return false, werr
		}
		return false, nil // exit 0 per spec
	}
	return true, nil
}

// execRebuild orchestrates the Rebuild call with progress tracking and
// renders results in the active output mode.
func execRebuild(
	ctx context.Context,
	cmd *cobra.Command,
	log kernel.EventLog,
	store kernel.Storage,
	flags *rebuildFlags,
) error {
	w := cmd.OutOrStdout()
	start := time.Now()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return execRebuildJSON(ctx, cmd, log, store, flags, w, start)
	}
	return execRebuildCLI(ctx, cmd, log, store, flags, w)
}

// execRebuildCLI runs Rebuild with a progress writer that emits plain-text
// progress lines and renders the final summary to stdout.
func execRebuildCLI(
	ctx context.Context,
	cmd *cobra.Command,
	log kernel.EventLog,
	store kernel.Storage,
	flags *rebuildFlags,
	w io.Writer,
) error {
	applyErrs, err := log.Rebuild(ctx, store, rebuildOptions(cmd, flags, w)...)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return silentError{code: 130, err: err}
		}
		return wrapRebuildFatal(cmd, err)
	}

	if werr := renderApplyErrors(cmd, applyErrs); werr != nil {
		return werr
	}

	if len(applyErrs) == 0 {
		if _, werr := fmt.Fprintln(w, "ok  rebuild complete"); werr != nil {
			return werr
		}
		return nil
	}
	if _, werr := fmt.Fprintln(w, "ok  rebuild complete (with errors)"); werr != nil {
		return werr
	}
	return &rebuildExitError{code: 1}
}

// renderApplyErrors writes a warning summary to stderr when there are
// apply errors, capped at 3 UIDs with a truncation hint when there are more.
func renderApplyErrors(cmd *cobra.Command, applyErrs []kernel.ApplyError) error {
	if len(applyErrs) == 0 {
		return nil
	}
	if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d event(s) failed to apply\n", len(applyErrs)); werr != nil {
		return werr
	}
	limit := len(applyErrs)
	if limit > 3 {
		limit = 3
	}
	for _, ae := range applyErrs[:limit] {
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "  uid=%s: %v\n", ae.UID, ae.Err); werr != nil {
			return werr
		}
	}
	if len(applyErrs) > 3 {
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "  (and %d more)\n", len(applyErrs)-3); werr != nil {
			return werr
		}
	}
	return nil
}

// execRebuildJSON runs Rebuild with a countingStorage adapter that emits
// JSON Lines progress ticks every N Apply calls, then emits a final summary.
func execRebuildJSON(
	ctx context.Context,
	cmd *cobra.Command,
	log kernel.EventLog,
	store kernel.Storage,
	flags *rebuildFlags,
	w io.Writer,
	start time.Time,
) error {
	cadence := flags.progressEvery
	if cadence <= 0 {
		cadence = 100 // mirrors defaultProgressEvery in the kernel
	}

	enc := json.NewEncoder(w)
	cs := &countingStorage{
		inner:   store,
		cadence: cadence,
		emit:    enc,
		start:   start,
	}

	var opts []kernel.RebuildOption
	if flags.strict {
		opts = append(opts, kernel.WithStrict())
	}
	if flags.progressEvery > 0 {
		opts = append(opts, kernel.WithProgressEvery(flags.progressEvery))
	}

	applyErrs, err := log.Rebuild(ctx, cs, opts...)
	elapsed := time.Since(start)

	if err != nil {
		return renderJSONError(cmd, enc, elapsed, err)
	}

	return renderJSONSummary(enc, cs, applyErrs, elapsed)
}

// renderJSONError emits a best-effort summary line for a fatal rebuild error
// and returns the appropriate exit error.
func renderJSONError(cmd *cobra.Command, enc *json.Encoder, elapsed time.Duration, err error) error {
	if errors.Is(err, context.Canceled) {
		//nolint:errcheck // best-effort: summary on interrupted rebuild
		_ = enc.Encode(map[string]any{
			"type":    "summary",
			"ok":      false,
			"elapsed": elapsed.String(),
			"error":   "interrupted",
		})
		return silentError{code: 130, err: err}
	}
	//nolint:errcheck // best-effort: emit error summary before fatal return
	_ = enc.Encode(map[string]any{
		"type":    "summary",
		"ok":      false,
		"elapsed": elapsed.String(),
		"error":   err.Error(),
	})
	return wrapRebuildFatal(cmd, err)
}

// renderJSONSummary emits the final {"type":"summary",...} line and returns
// a rebuildExitError{1} when there were apply errors.
func renderJSONSummary(enc *json.Encoder, cs *countingStorage, applyErrs []kernel.ApplyError, elapsed time.Duration) error {
	ok := len(applyErrs) == 0
	summary := map[string]any{
		"type":    "summary",
		"ok":      ok,
		"applied": cs.applied.Load(),
		"skipped": cs.skipped.Load(),
		"failed":  len(applyErrs),
		"elapsed": elapsed.String(),
	}
	if len(applyErrs) > 0 {
		limit := len(applyErrs)
		if limit > 3 {
			limit = 3
		}
		errUIDs := make([]string, limit)
		for i, ae := range applyErrs[:limit] {
			errUIDs[i] = ae.UID
		}
		summary["errorUIDs"] = errUIDs
		if len(applyErrs) > 3 {
			summary["truncated"] = true
		}
	}
	if encErr := enc.Encode(summary); encErr != nil {
		return encErr
	}
	if !ok {
		return &rebuildExitError{code: 1}
	}
	return nil
}

// rebuildOptions builds the []RebuildOption slice from flags. In JSON mode
// the countingStorage adapter emits ticks directly, so WithProgress is not
// set. In CLI mode progress lines go to the provided writer.
func rebuildOptions(cmd *cobra.Command, flags *rebuildFlags, progressWriter io.Writer) []kernel.RebuildOption {
	var opts []kernel.RebuildOption
	if flags.strict {
		opts = append(opts, kernel.WithStrict())
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if progressWriter != nil && !jsonMode {
		opts = append(opts, kernel.WithProgress(progressWriter))
	}
	if flags.progressEvery > 0 {
		opts = append(opts, kernel.WithProgressEvery(flags.progressEvery))
	}
	return opts
}

// resolveRebuildEmbedder reads ${dataDir}/config.toml and builds the Ollama
// embedder when the user has one configured. When config.toml is absent it
// falls back to the same M1 Ollama default the daemon uses (localhost:11434,
// bge-m3) so a fresh-data-dir rebuild still attempts to repair episodes_vec.
//
// Error classification mirrors daemon.classifyEmbedderErr:
//   - Unknown embed provider or context cancellation: returns (nil, error) so
//     the rebuild is aborted before it starts -- a configuration error the user
//     must fix, not a transient state.
//   - Transient probe failure (Ollama daemon unreachable): returns (nil, nil)
//     with a warning on stderr -- the rebuild proceeds and repairs the three
//     non-vector layers; the user must run `maz rebuild` again once Ollama is up.
//   - embed=none or empty Defaults.Embed: returns (nil, nil) silently -- the
//     user explicitly opted out of embeddings.
//
//nolint:ireturn // returns dispatcher.Embedder; nil is the documented degraded/disabled sentinel
func resolveRebuildEmbedder(ctx context.Context, cmd *cobra.Command, dataDir string) (dispatcher.Embedder, error) {
	user, err := config.LoadUserConfig(dataDir)
	if errors.Is(err, config.ErrUserConfigNotFound) {
		// config.toml absent (first-run or no init yet): fall back to the same
		// M1 Ollama default the daemon uses so a fresh-dir rebuild still tries
		// to repair episodes_vec if the daemon is running.
		user = config.UserConfig{
			Providers: map[string]config.ProviderTable{chatProviderOllama: {}},
			Defaults:  config.DefaultsTable{Embed: chatProviderOllama},
		}
	} else if err != nil {
		return nil, fmt.Errorf("rebuild: load user config: %w", err)
	}

	key := user.Defaults.Embed
	if key == "" || key == providerSentinelNone {
		// User explicitly disabled embeddings; vector layer is intentionally empty.
		return nil, nil //nolint:nilnil // embed=none is the documented opt-out sentinel
	}
	if key != chatProviderOllama {
		// Unknown provider is a configuration error -- fail fast so the user
		// knows they must fix config.toml before rebuilding vector recall.
		return nil, fmt.Errorf("rebuild: unsupported embed provider %q (supported: ollama, none)", key)
	}

	// Context cancellation is fatal: the user interrupted the process.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	prov := user.Providers[key]
	client, err := ollama.New(ctx, ollama.Options{
		BaseURL:    prov.BaseURL,
		EmbedModel: prov.EmbedModel,
	})
	if err != nil {
		// Classify: malformed URL, model-not-pulled, and caller cancellation are
		// fatal so the user knows episodes_vec was NOT rebuilt.
		// Transient connection failures degrade gracefully with a warning.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if errors.Is(err, ollama.ErrInvalidConfig) || errors.Is(err, ollama.ErrModelMissing) {
			return nil, fmt.Errorf("rebuild: embedder: %w (fix config.toml or pull the model, then rerun)", err)
		}
		// Transient: Ollama daemon unreachable. Warn and continue without vectors.
		//nolint:errcheck // best-effort: stderr write on degraded path
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: rebuild: Ollama unreachable (%v); vector layer will not be rebuilt\n", err)
		return nil, nil //nolint:nilnil // transient probe failure; caller proceeds without vectors
	}
	return client, nil
}

// rebuildStorageOpenError wraps a dispatcher.Open failure. Rebuild replays the
// whole event log to rewrite the graph projection, so it needs exclusive access
// to the store. The common failure is the LadybugDB single-opener lock — almost
// always because the daemon (or an editor's `maz mcp serve`) already holds the
// database — which surfaces as the opaque "failed to open database with status
// 1". Turn that into the actionable "stop the holder" hint instead of leaking
// the raw driver string; any other failure keeps the plain wrap.
func rebuildStorageOpenError(dataDir string, err error) error {
	if isDatabaseLockHeld(err) {
		return fmt.Errorf("%s: %w", dataDirLockedHint(dataDir), err)
	}
	return fmt.Errorf("open storage: %w", err)
}

// wrapRebuildFatal prints the error to stderr and returns a silentError
// with code 2 so Execute exits with the right code without printing a
// duplicate message.
func wrapRebuildFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "rebuild", err)
}

// countingStorage wraps a kernel.Storage to count Apply and HasEvent calls
// so the JSON mode can emit progress ticks at the configured cadence without
// parsing the kernel's text progress format. All Storage methods other than
// Apply and HasEvent delegate to the inner store unchanged.
//
// Rebuild calls Apply and HasEvent sequentially (single goroutine), but the
// atomics are used to make Load reads in the summary safe regardless.
type countingStorage struct {
	inner     kernel.Storage
	cadence   int
	emit      *json.Encoder
	start     time.Time
	applied   atomic.Int64 // events successfully committed
	skipped   atomic.Int64 // events skipped via HasEvent
	attempted atomic.Int64 // total Apply calls (success + failure); drives cadence
}

var _ kernel.Storage = (*countingStorage)(nil)

// Apply delegates to the inner store and counts processed event batches.
// Every Apply call (success or failure) increments the attempted counter so
// the cadence fires even when a non-strict rebuild accumulates ApplyErrors
// without any successful commits or HasEvent skips. The tick's "applied" field
// reflects only successfully committed events so the summary remains accurate.
func (c *countingStorage) Apply(ctx context.Context, events []kernel.Event) error {
	err := c.inner.Apply(ctx, events)
	// Increment attempted unconditionally so failed applies advance the cadence.
	attempted := c.attempted.Add(int64(len(events)))
	if err == nil {
		c.applied.Add(int64(len(events)))
	}
	total := attempted + c.skipped.Load()
	if total > 0 && int(total)%c.cadence == 0 {
		applied := c.applied.Load()
		elapsed := time.Since(c.start)
		var rate float64
		if secs := elapsed.Seconds(); secs > 0 {
			rate = float64(applied) / secs
		}
		// Progress encoding errors are best-effort: a closed pipe must
		// not abort an in-flight rebuild.
		//nolint:errcheck // best-effort progress sink
		_ = c.emit.Encode(map[string]any{
			"type":    "progress",
			"applied": applied,
			"elapsed": elapsed.Truncate(time.Millisecond).String(),
			"rate":    fmt.Sprintf("%.1f/s", rate),
		})
	}
	return err
}

// HasEvent delegates to the inner store and counts skipped events.
func (c *countingStorage) HasEvent(ctx context.Context, uid string) (bool, error) {
	has, err := c.inner.HasEvent(ctx, uid)
	if err == nil && has {
		c.skipped.Add(1)
	}
	return has, err
}

// OpenSQLite delegates to the inner store.
func (c *countingStorage) OpenSQLite(ctx context.Context) (*sql.DB, error) {
	return c.inner.OpenSQLite(ctx)
}

// OpenGraph delegates to the inner store.
//
//nolint:ireturn // delegating adapter; return type is dictated by kernel.Storage interface
func (c *countingStorage) OpenGraph(ctx context.Context) (kernel.GraphConn, error) {
	return c.inner.OpenGraph(ctx)
}

// OpenVector delegates to the inner store.
//
//nolint:ireturn // delegating adapter; return type is dictated by kernel.Storage interface
func (c *countingStorage) OpenVector(ctx context.Context) (kernel.VectorConn, error) {
	return c.inner.OpenVector(ctx)
}

// OpenFTS delegates to the inner store.
//
//nolint:ireturn // delegating adapter; return type is dictated by kernel.Storage interface
func (c *countingStorage) OpenFTS(ctx context.Context) (kernel.FTSConn, error) {
	return c.inner.OpenFTS(ctx)
}

// Provenance delegates to the inner store.
func (c *countingStorage) Provenance(ctx context.Context, eventUID string) ([]kernel.ProvenanceRecord, error) {
	return c.inner.Provenance(ctx, eventUID)
}

// Close delegates to the inner store. The countingStorage does not own any
// resources beyond the inner store, so Close is a thin pass-through.
func (c *countingStorage) Close() error {
	return c.inner.Close()
}

// newReindexCmd returns the `maz reindex` command with the `--reembed` and
// `--dry-run` sub-flags. Feature 9: drop the episodes_vec projection and
// re-run Rebuild so the embedder is exercised for every episode, refreshing
// the vector layer without touching the other three stores.
func newReindexCmd() *cobra.Command {
	var reembed bool
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Reindex derived stores (e.g. refresh all embeddings)",
		Long: `Reindex derived stores from the event log. Currently supports:

  --reembed   Drop the episodes_vec projection and re-run Rebuild so every
              episode is re-embedded by the configured embedder. Use this when
              you have changed the embed model, or after a partial failure left
              episodes_vec incomplete. The other three derived stores (SQLite,
              graph, FTS5) are not affected.

  --dry-run   Count the episodes that would be re-embedded without making any
              changes. Useful to gauge scope before committing.

--reembed requires an embedder to be configured (maz init --embed=ollama).
If no embedder is available the command exits with code 1.

Exit codes:
  0   reindex complete (or dry-run counted)
  1   no --reembed flag set, no embedder available, or user error
  2   storage fault`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReindex(cmd, reembed, dryRun)
		},
	}
	cmd.Flags().BoolVar(&reembed, "reembed", false, "drop and rebuild the episodes vector index")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report episode count without writing (requires --reembed)")
	return cmd
}

// runReindex implements `maz reindex --reembed [--dry-run]`.
func runReindex(cmd *cobra.Command, reembed, dryRun bool) error {
	if !reembed {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
			"reindex: specify --reembed to refresh the embedding index"); werr != nil {
			return werr
		}
		return &rebuildExitError{code: 1}
	}

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	ctx := cmd.Context()

	// Route to the live daemon when one holds the store: reindex runs ONLINE
	// (re-embed via a from-log rebuild) with the daemon up. --dry-run needs the
	// in-process store, which the daemon holds, so it is unavailable while the
	// daemon runs; the online reindex is non-destructive (it replays the log).
	if preflightDaemon("", dataDir) != nil {
		if dryRun {
			//nolint:errcheck // best-effort stderr
			fmt.Fprintln(cmd.ErrOrStderr(),
				"reindex: --dry-run is unavailable while the daemon is running; the online reindex replays the log non-destructively — omit --dry-run to run it live")
			return &rebuildExitError{code: 1}
		}
		return runReindexViaDaemon(ctx, cmd, dataDir)
	}

	log, closeLog, disp, db, err := openReindexStorage(ctx, cmd, dataDir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeLog(); cerr != nil {
			//nolint:errcheck // best-effort
			fmt.Fprintf(cmd.ErrOrStderr(), "close event log: %v\n", cerr)
		}
		if cerr := disp.Close(); cerr != nil {
			//nolint:errcheck // best-effort
			fmt.Fprintf(cmd.ErrOrStderr(), "close storage: %v\n", cerr)
		}
	}()

	var count int
	if cerr := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM episodes_vec_ids`).Scan(&count); cerr != nil {
		return respondFatal(cmd, "reindex", fmt.Errorf("count episodes_vec_ids: %w", cerr))
	}

	if dryRun {
		if _, werr := fmt.Fprintf(cmd.OutOrStdout(),
			"ok  dry-run: %d episode embedding(s) would be dropped and re-built\n", count); werr != nil {
			return werr
		}
		return nil
	}

	return runReembedPass(ctx, cmd, log, disp, db, count)
}

// openReindexStorage opens all storage layers needed by runReindex and
// returns them together with a close function for the event log. The
// dispatcher must be closed by the caller via disp.Close().
func openReindexStorage(ctx context.Context, cmd *cobra.Command, dataDir string) (
	log *kernel.FileEventLog,
	closeLog func() error,
	disp *dispatcher.Dispatcher,
	db *sql.DB,
	err error,
) {
	log, closeLog, err = kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return nil, nil, nil, nil, respondFatal(cmd, "reindex", fmt.Errorf("open event log: %w", err))
	}

	emb, embErr := resolveRebuildEmbedder(ctx, cmd, dataDir)
	if embErr != nil {
		_ = closeLog() //nolint:errcheck // best-effort cleanup on error path
		if errors.Is(embErr, context.Canceled) {
			return nil, nil, nil, nil, silentError{code: 130, err: embErr}
		}
		return nil, nil, nil, nil, respondFatal(cmd, "reindex", embErr)
	}
	if emb == nil {
		_ = closeLog() //nolint:errcheck // best-effort cleanup on error path
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
			"reindex: no embedder configured; run `maz init --embed=ollama` first"); werr != nil {
			return nil, nil, nil, nil, werr
		}
		return nil, nil, nil, nil, &rebuildExitError{code: 1}
	}

	disp, err = dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir, Embedder: emb})
	if err != nil {
		_ = closeLog() //nolint:errcheck // best-effort cleanup on error path
		return nil, nil, nil, nil, respondFatal(cmd, "reindex", rebuildStorageOpenError(dataDir, err))
	}

	db, err = disp.OpenSQLite(ctx)
	if err != nil {
		_ = disp.Close() //nolint:errcheck // best-effort cleanup on error path
		_ = closeLog()   //nolint:errcheck // best-effort cleanup on error path
		return nil, nil, nil, nil, respondFatal(cmd, "reindex", fmt.Errorf("open sqlite: %w", err))
	}
	return log, closeLog, disp, db, nil
}

// runReembedPass drops the existing vector projection and replays the event
// log so every episode is re-embedded. It prints progress to cmd.OutOrStdout.
func runReembedPass(ctx context.Context, cmd *cobra.Command, log *kernel.FileEventLog, disp *dispatcher.Dispatcher, db *sql.DB, count int) error {
	// Drop the vector projection so HasEvent returns false for all episodes,
	// forcing Rebuild to re-apply and re-embed each one.
	if _, xerr := db.ExecContext(ctx, `DELETE FROM episodes_vec`); xerr != nil {
		return respondFatal(cmd, "reindex", fmt.Errorf("clear episodes_vec: %w", xerr))
	}
	if _, xerr := db.ExecContext(ctx, `DELETE FROM episodes_vec_ids`); xerr != nil {
		return respondFatal(cmd, "reindex", fmt.Errorf("clear episodes_vec_ids: %w", xerr))
	}

	if _, werr := fmt.Fprintf(cmd.OutOrStdout(),
		"  dropped %d embedding(s); replaying event log to re-embed...\n", count); werr != nil {
		return werr
	}

	applyErrs, rerr := log.Rebuild(ctx, disp, kernel.WithProgress(cmd.OutOrStdout()))
	if rerr != nil {
		if errors.Is(rerr, context.Canceled) {
			return silentError{code: 130, err: rerr}
		}
		return respondFatal(cmd, "reindex", rerr)
	}

	// Drain background embed/upsert goroutines before returning. Rebuild
	// schedules vector upserts asynchronously; without this drain the
	// dispatcher's bgVec WaitGroup counter is still positive when Close() runs
	// (called by the defer in runReindex), which causes every pending upsert to
	// log a "dispatcher: closed" error and leaves episodes_vec incomplete.
	if derr := disp.WaitForBackgroundVec(ctx); derr != nil {
		//nolint:errcheck // best-effort: stderr write on drain timeout
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: reindex: embed drain incomplete: %v\n", derr)
	}

	if werr := renderApplyErrors(cmd, applyErrs); werr != nil {
		return werr
	}

	msg := "ok  reindex complete"
	if len(applyErrs) > 0 {
		msg = "ok  reindex complete (with errors)"
	}
	if _, werr := fmt.Fprintln(cmd.OutOrStdout(), msg); werr != nil {
		return werr
	}
	if len(applyErrs) > 0 {
		return &rebuildExitError{code: 1}
	}
	return nil
}

// runRebuildViaDaemon triggers an ONLINE rebuild on the running daemon and
// tails its progress. The daemon shadow-builds a fresh projection and swaps it
// in without stopping; the brief socket-drop during the swap is tolerated by
// the poll loop, so the daemon never needs to be stopped.
func runRebuildViaDaemon(ctx context.Context, cmd *cobra.Command, dataDir string, strict bool) error {
	c, err := daemonclient.New(dataDir)
	if err != nil {
		return wrapRebuildFatal(cmd, err)
	}
	resp, terr := c.TriggerRebuild(ctx, strict)
	if terr != nil {
		return mapRebuildTriggerError(cmd, "rebuild", terr)
	}
	return tailDaemonRebuild(ctx, cmd, c, resp.JobUID, "rebuild")
}

// runReindexViaDaemon triggers an ONLINE reindex on the running daemon (a
// from-log rebuild that regenerates every vector with the current embedder).
func runReindexViaDaemon(ctx context.Context, cmd *cobra.Command, dataDir string) error {
	c, err := daemonclient.New(dataDir)
	if err != nil {
		return respondFatal(cmd, "reindex", err)
	}
	resp, terr := c.TriggerReindex(ctx)
	if terr != nil {
		return mapRebuildTriggerError(cmd, "reindex", terr)
	}
	return tailDaemonRebuild(ctx, cmd, c, resp.JobUID, "reindex")
}

// mapRebuildTriggerError maps a trigger error to a CLI exit. A 409 (a rebuild
// is already running) is a user-actionable exit-1; anything else is fatal.
func mapRebuildTriggerError(cmd *cobra.Command, opName string, err error) error {
	var he *daemonclient.HTTPError
	if errors.As(err, &he) && he.StatusCode == http.StatusConflict {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: a rebuild or reindex is already running on the daemon\n", opName)
		return &rebuildExitError{code: 1}
	}
	return respondFatal(cmd, opName, err)
}

// tailDaemonRebuild polls the daemon rebuild status to completion, rendering
// progress in the active output mode. The poll tolerates the swap's socket-drop
// window (the coordinator status survives the reload), so the command does not
// fail when the daemon briefly reloads its serving stack.
func tailDaemonRebuild(ctx context.Context, cmd *cobra.Command, c *daemonclient.Client, jobUID, opName string) error {
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	enc := json.NewEncoder(cmd.OutOrStdout())

	if jsonMode {
		//nolint:errcheck // best-effort progress sink
		_ = enc.Encode(map[string]any{"type": "started", "jobUid": jobUID, "kind": opName})
	} else {
		//nolint:errcheck // best-effort header line
		fmt.Fprintf(cmd.OutOrStdout(), "%s started (job %s); daemon stays up\n", opName, jobUID)
	}

	onUpdate := func(st daemonclient.RebuildStatus) {
		if jsonMode {
			//nolint:errcheck // best-effort progress sink
			_ = enc.Encode(map[string]any{"type": "progress", "state": st.State, "progress": st.Progress, "detail": st.Detail})
			return
		}
		//nolint:errcheck // best-effort progress line
		fmt.Fprintf(cmd.OutOrStdout(), "  %-9s %s\n", st.State, st.Detail)
	}

	perr := c.PollRebuild(ctx, 500*time.Millisecond, onUpdate)
	if perr != nil {
		if errors.Is(perr, context.Canceled) {
			return silentError{code: 130, err: perr}
		}
		if jsonMode {
			//nolint:errcheck // best-effort summary
			_ = enc.Encode(map[string]any{"type": "summary", "ok": false, "error": perr.Error()})
		} else {
			//nolint:errcheck // best-effort summary
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", opName, perr)
		}
		return &rebuildExitError{code: 1}
	}
	if jsonMode {
		//nolint:errcheck // best-effort summary
		_ = enc.Encode(map[string]any{"type": "summary", "ok": true})
	} else {
		//nolint:errcheck // best-effort summary
		fmt.Fprintf(cmd.OutOrStdout(), "ok  %s complete (online; daemon stayed up)\n", opName)
	}
	return nil
}
