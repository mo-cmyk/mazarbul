package cli

// load.go implements the "maz load <file.jsonl>" command: bulk-load events from
// a JSONL snapshot into the event log in one atomic batch.
//
// The JSONL file must contain one kernel.Event per line (the same format that
// "maz export" / "maz log" emits). Every line is decoded and validated before
// any write; the whole batch is either accepted or rejected atomically. A
// partial failure (e.g. a schema-invalid event amid otherwise valid ones) means
// zero events are appended.
//
// Modes:
//
//   - append (default): append every parsed event; re-loading yields duplicates.
//   - merge:  idempotent — skip events whose UID is already present in the log
//     so re-loading the same export file twice does not duplicate rows.
//
// Overwrite is NOT implemented: the event log is append-only. To start fresh,
// reset the data dir (see `maz init --reset`) then load.
//
// --dry-run: parse + validate + count without writing anything.
//
// Exit codes:
//
//	0  success (events loaded / dry-run report)
//	1  expected failure: bad file, invalid events, unknown flag value
//	2  unexpected failure: storage or wiring error

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// loadMode controls the dedup behaviour.
type loadMode string

const (
	loadModeAppend loadMode = "append"
	loadModeMerge  loadMode = "merge"
)

// loadResult is the JSON / text output shape.
type loadResult struct {
	Mode     string             `json:"mode"`
	Source   string             `json:"source"`
	Total    int                `json:"total"`
	Appended int                `json:"appended"`
	Skipped  int                `json:"skipped"`
	Invalid  int                `json:"invalid"`
	DryRun   bool               `json:"dryRun"`
	Warnings []warncode.Warning `json:"warnings,omitempty"`
}

type loadFlags struct {
	mode   string
	dryRun bool
}

func newLoadCmd() *cobra.Command {
	flags := &loadFlags{}
	cmd := &cobra.Command{
		Use:   "load <file.jsonl>",
		Short: "Bulk-load events from a JSONL snapshot into the event log",
		Long: `Parse and append events from a newline-delimited JSON file into the event
log in one atomic batch. The file format is the same as "maz export" output:
one kernel.Event JSON object per line.

Modes (--mode):
  append  Append every parsed event. The kernel enforces UID uniqueness across
          the entire log: re-loading the same file fails with ErrInvalidEvent
          if the events carry UIDs already present.
  merge   Skip events whose UID is already present in the log, so loading the
          same file twice is idempotent. Skipped events are counted and warned
          with code ` + warncode.CodeLoadDuplicateMerged + `.

Overwrite is NOT supported: the event log is immutable (append-only). To reset
and reload, remove the data dir and re-init, then load.

All events are validated before any write: invalid events are skipped and
counted but do not abort the batch. Events that pass validation are appended
together. Invalid events are warned with code ` + warncode.CodeLoadRecordSkipped + `.

--dry-run parses and validates but writes nothing. Useful for preflight checks.

Exit codes:
  0  success (events loaded, or --dry-run report)
  1  expected failure: bad file, invalid lines, unsupported mode
  2  unexpected failure: storage or wiring error

Examples:
  # import a backup
  maz export > backup.ndjson && maz load backup.ndjson

  # idempotent re-import
  maz load backup.ndjson --mode merge

  # preflight: check without writing
  maz load backup.ndjson --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLoad(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.mode, "mode", string(loadModeAppend),
		`dedup mode: append (default) or merge (skip existing UIDs)`)
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false,
		"parse and validate without appending any events")
	return cmd
}

// runLoad is the main entry point for "maz load".
func runLoad(cmd *cobra.Command, file string, flags *loadFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	mode, err := parseLoadMode(flags.mode)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "load: %v\n", err)
		return silentError{code: 1, err: err}
	}

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	events, invalids, err := parseLoadFileByPath(cmd, file)
	if err != nil {
		return err
	}

	warnings := make([]warncode.Warning, 0)
	if invalids > 0 {
		warnings = append(warnings, warncode.New(warncode.CodeLoadRecordSkipped,
			"%d invalid record(s) skipped (could not be decoded or had empty kind/payload)", invalids))
	}

	existingUIDs, err := loadMergeUIDs(ctx, cmd, logger, dataDir, mode)
	if err != nil {
		return err
	}

	toAppend, skipped := filterForMode(events, mode, existingUIDs)
	if skipped > 0 {
		warnings = append(warnings, warncode.New(warncode.CodeLoadDuplicateMerged,
			"%d event(s) skipped (UID already present in the log; merge mode)", skipped))
	}

	res := &loadResult{
		Mode:     string(mode),
		Source:   file,
		Total:    len(events) + invalids,
		Appended: len(toAppend),
		Skipped:  skipped,
		Invalid:  invalids,
		DryRun:   flags.dryRun,
		Warnings: warnings,
	}

	if flags.dryRun || len(toAppend) == 0 {
		if err := renderLoadResult(cmd, res); err != nil {
			return err
		}
		// Nothing was loaded and there were invalid lines: that is an expected
		// failure (the caller provided only bad input). Return exit 1 so scripts
		// can distinguish this from a genuinely empty file (total=0, invalids=0)
		// which exits 0. A dry-run with zero valid events also exits 0 — that is
		// intentional: --dry-run is a preflight check, not a write.
		if !flags.dryRun && invalids > 0 {
			return silentError{code: 1, err: errors.New("load: no events appended (all lines were invalid)")}
		}
		return nil
	}

	return appendLoadBatch(ctx, cmd, logger, dataDir, toAppend, res)
}

// parseLoadFileByPath opens the JSONL file, parses it, and returns the events.
func parseLoadFileByPath(cmd *cobra.Command, file string) ([]kernel.Event, int, error) {
	f, err := os.Open(file)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "load: open %q: %v\n", file, err)
		return nil, 0, silentError{code: 1, err: err}
	}
	defer func() { _ = f.Close() }()

	events, invalids, parseErr := parseLoadFile(f)
	if parseErr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "load: parse %q: %v\n", file, parseErr)
		return nil, invalids, silentError{code: 1, err: parseErr}
	}
	return events, invalids, nil
}

// loadMergeUIDs opens the log read-only and returns the set of existing UIDs
// when mode is merge. Returns an empty map for append mode (callers treat nil
// and empty maps identically via the existing[uid] lookup).
func loadMergeUIDs(ctx context.Context, cmd *cobra.Command, logger *zap.Logger, dataDir string, mode loadMode) (map[string]bool, error) {
	if mode != loadModeMerge {
		return make(map[string]bool), nil
	}
	// kernelboot.OpenLog uses the raw JSONL reader — safe even with a daemon
	// holding the exclusive LadybugDB write lock.
	log, closeFn, oerr := kernelboot.OpenLog(ctx, dataDir)
	if oerr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "load: open event log: %v\n", oerr)
		return nil, silentError{code: 2, err: oerr}
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("load: close event log", zap.Error(cerr))
		}
	}()
	uids, err := collectExistingUIDs(ctx, log)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "load: read existing UIDs: %v\n", err)
		return nil, silentError{code: 2, err: err}
	}
	return uids, nil
}

// appendLoadBatch routes the batch through the live daemon's admin endpoint
// when one is running, and falls back to the in-process materialising log
// path otherwise. No post-append materialisation checks are used for load:
// the projection latency is acceptable and the events are already durable in
// the JSONL log. On success it renders the load result.
//
// Event sources are preserved verbatim on both paths. A JSONL snapshot is a
// backup whose original provenance (e.g. "mcp:remember", "schemapack:foo") is
// the whole point of re-importing it; the admin handler deliberately does not
// clamp or allowlist Source (see adminhandler.go), so the daemon and in-process
// paths produce identical events.
func appendLoadBatch(ctx context.Context, cmd *cobra.Command, logger *zap.Logger, dataDir string, toAppend []kernel.Event, res *loadResult) error {
	if aerr := appendBatchViaDaemonOrInProcess(
		ctx, cmd, dataDir, logger, "load",
		toAppend,
		nil, // no precheck needed for load
		nil, // no post-append checks needed for load
	); aerr != nil {
		return aerr
	}
	return renderLoadResult(cmd, res)
}

// parseLoadMode validates the --mode flag value.
func parseLoadMode(raw string) (loadMode, error) {
	switch raw {
	case string(loadModeAppend):
		return loadModeAppend, nil
	case string(loadModeMerge):
		return loadModeMerge, nil
	case "overwrite":
		return "", errors.New(`mode "overwrite" is not supported: the event log is append-only and immutable; use "maz forget" to tombstone individual records, or reset the data dir and reload`)
	default:
		return "", fmt.Errorf("unknown mode %q: must be append or merge", raw)
	}
}

// parseLoadFile reads f line by line and decodes each as kernel.Event.
// Lines that are empty or cannot be decoded are counted as invalids (not fatal).
// A read error from the underlying io.Reader IS fatal and is returned.
func parseLoadFile(r io.Reader) (events []kernel.Event, invalids int, err error) {
	sc := bufio.NewScanner(r)
	// Allow up to 10 MB lines to handle large payloads.
	sc.Buffer(make([]byte, 1<<20), 10<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev kernel.Event
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			invalids++
			continue
		}
		// Basic validity: kind and payload must be present.
		if ev.Kind == "" || len(ev.Payload) == 0 {
			invalids++
			continue
		}
		events = append(events, ev)
	}
	if serr := sc.Err(); serr != nil {
		return nil, invalids, fmt.Errorf("read jsonl: %w", serr)
	}
	return events, invalids, nil
}

// collectExistingUIDs reads the entire event log and returns a set of all UIDs
// currently in it. Used by merge mode to detect duplicates before appending.
func collectExistingUIDs(ctx context.Context, log kernel.EventLog) (map[string]bool, error) {
	seen := make(map[string]bool)
	for ev, err := range log.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			return nil, fmt.Errorf("iterate log: %w", err)
		}
		if ev.UID != "" {
			seen[ev.UID] = true
		}
	}
	return seen, nil
}

// filterForMode partitions events into the append slice and the skip count
// according to mode and existingUIDs.
func filterForMode(events []kernel.Event, mode loadMode, existing map[string]bool) (toAppend []kernel.Event, skipped int) {
	for i := range events {
		if mode == loadModeMerge && existing[events[i].UID] {
			skipped++
			continue
		}
		toAppend = append(toAppend, events[i])
	}
	return toAppend, skipped
}

// renderLoadResult writes the result to the output writer in the active format.
func renderLoadResult(cmd *cobra.Command, res *loadResult) error {
	w := output.From(cmd)
	switch w.Format() {
	case output.FormatJSON, output.FormatNDJSON:
		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	default:
		return renderLoadResultText(cmd, res)
	}
}

func renderLoadResultText(cmd *cobra.Command, res *loadResult) error {
	action := "Appended"
	if res.DryRun {
		action = "Would append"
	}
	//nolint:errcheck // best-effort output
	fmt.Fprintf(cmd.OutOrStdout(), "%s %d event(s) from %q (total=%d, skipped=%d, invalid=%d, mode=%s)\n",
		action, res.Appended, res.Source, res.Total, res.Skipped, res.Invalid, res.Mode)
	for _, w := range res.Warnings {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "warning [%s]: %s\n", w.Code, w.Message)
	}
	return nil
}
