package cli

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/status"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon state, event log freshness, and storage counts",
		Long: `Display the current state of the Mazarbul data directory: storage counts,
daemon health, event log freshness, and setup signposts.

All reads are local — no network calls, no Ollama probe.

Examples:
  # Show status of the default data directory
  maz status

  # Emit a structured JSON document for scripting
  maz status --output json | jq .daemon.status

  # Inspect an alternate data directory
  maz --data-dir /tmp/alt status`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd)
		},
	}
}

// statusDaemonReport is the daemon sub-object in the JSON output.
type statusDaemonReport struct {
	Status string `json:"status"`
	PID    int    `json:"pid,omitempty"`
	Uptime string `json:"uptime,omitempty"`
}

// statusLogReport is the log-freshness sub-object in the JSON output.
type statusLogReport struct {
	LastEventAt string `json:"lastEventAt,omitempty"`
	EventCount  int    `json:"eventCount"`
}

// statusConfigReport is the config-signpost sub-object in the JSON output.
type statusConfigReport struct {
	EmbedProvider string   `json:"embedProvider"`
	SyncScopes    []string `json:"syncScopes"`
}

// statusReport is the top-level JSON document emitted by `maz status --output json`.
type statusReport struct {
	DataDir   string             `json:"dataDir"`
	Episodes  int                `json:"episodes"`
	Entities  int                `json:"entities"`
	Mentions  int                `json:"mentions"`
	Relations int                `json:"relations"`
	Daemon    statusDaemonReport `json:"daemon"`
	Log       statusLogReport    `json:"log"`
	Config    statusConfigReport `json:"config"`
}

func runStatus(cmd *cobra.Command) error {
	ctx := cmd.Context()
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "index.db"))
	if err != nil {
		return wrapStatusFatal(cmd, fmt.Errorf("open sqlite: %w", err))
	}
	defer closeStoreQuietly(store)

	// ── Storage counts ──────────────────────────────────────────────────────
	rep := statusReport{DataDir: dataDir}
	tables := []struct {
		name string
		dest *int
	}{
		{"episodes", &rep.Episodes},
		{"entities", &rep.Entities},
		{"mentions", &rep.Mentions},
		{"relations", &rep.Relations},
	}
	for _, t := range tables {
		if err := store.Reader().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+t.name,
		).Scan(t.dest); err != nil {
			return wrapStatusFatal(cmd, fmt.Errorf("count %s: %w", t.name, err))
		}
	}

	// ── Log freshness — last event time + total event count ─────────────────
	// episodes holds all event-log entries regardless of kind; create_time is
	// stored as an RFC 3339 string (text affinity, see migration 0001). The
	// count reuses rep.Episodes (already queried above) rather than a second
	// COUNT(*), so log.eventCount and episodes can never diverge in the output.
	var lastRaw sql.NullString
	if err := store.Reader().QueryRowContext(ctx,
		"SELECT MAX(create_time) FROM episodes",
	).Scan(&lastRaw); err != nil {
		// Non-fatal: leave log fields at zero values.
		lastRaw = sql.NullString{}
	}
	var lastEventTime time.Time
	if lastRaw.Valid && lastRaw.String != "" {
		if t, perr := time.Parse(time.RFC3339, lastRaw.String); perr == nil {
			lastEventTime = t.UTC()
		}
	}
	rep.Log = statusLogReport{
		EventCount: rep.Episodes,
	}
	if !lastEventTime.IsZero() {
		rep.Log.LastEventAt = lastEventTime.UTC().Format(time.RFC3339)
	}

	// ── Daemon state — cheap local PID-file read ────────────────────────────
	ds := readDaemonState(ctx, dataDir)
	rep.Daemon = statusDaemonReport{Status: ds.Status}
	if ds.PID != 0 {
		rep.Daemon.PID = ds.PID
	}
	if ds.Uptime > 0 {
		rep.Daemon.Uptime = humanDuration(ds.Uptime)
	}

	// ── Config signposts — read-only, never probe ───────────────────────────
	// LoadUserConfig fails gracefully for first-run or missing config; the
	// returned zero-value UserConfig produces sensible "not configured" signposts.
	cfg, cfgErr := config.LoadUserConfig(dataDir)
	if cfgErr != nil {
		cfg = config.UserConfig{} // zero value → signposts degrade gracefully
	}
	rep.Config = statusConfigReport{
		EmbedProvider: cfg.Defaults.Embed,
		SyncScopes:    syncScopeNames(&cfg),
	}

	// ── Render ──────────────────────────────────────────────────────────────
	wr := output.From(cmd)
	if wr.Format() != output.FormatText {
		return wr.Emit(rep)
	}
	return renderStatusText(cmd, wr, &rep, ds, lastEventTime)
}

// renderStatusText writes the human-readable status output using the C5
// status.Printer and the colorizer so NO_COLOR and colour-profile handling
// are automatic. Rep is taken by pointer to avoid copying the 152-byte struct.
func renderStatusText(cmd *cobra.Command, wr output.Writer, rep *statusReport, ds daemonReadState, lastEventTime time.Time) error {
	w := cmd.OutOrStdout()
	styled := interactive.ShouldStyle(cmd, w)
	p := status.New(w, styled)
	c := colorizer.New(styled, w)

	// ◆ Mazarbul brand header with the (tilde-collapsed) data directory.
	if _, err := fmt.Fprintf(w, "%s %s   %s\n\n",
		c.Accent("◆"), c.Accent("Mazarbul"), c.Dim(tildeCollapse(rep.DataDir))); err != nil {
		return err
	}

	// One combined store line + daemon + log + signpost rows via MetadataRows
	// (labels all ≤ 6 chars → clean 7-char column alignment from the %-7s rule).
	rows := [][2]string{
		{"store", formatStoreLine(rep)},
		{"daemon", formatDaemonRow(c, ds)},
		{"log", formatLogRow(c, rep.Log, lastEventTime)},
		{"embed", formatEmbedSignpost(c, rep.Config.EmbedProvider)},
		{"sync", formatSyncSignpost(c, rep.Config.SyncScopes)},
	}
	_, err := fmt.Fprintln(w, p.MetadataRows(rows))
	return err
}

// formatStoreLine combines the four storage counts into one "N episodes ·
// N entities · N relations · N mentions" line for the status store row.
func formatStoreLine(rep *statusReport) string {
	return fmt.Sprintf("%d episodes · %d entities · %d relations · %d mentions",
		rep.Episodes, rep.Entities, rep.Relations, rep.Mentions)
}

// tildeCollapse replaces a leading $HOME with "~" so the data-dir header reads
// as "~/.mazarbul" rather than an absolute path. Returns path unchanged when
// the home directory is unknown or not a prefix.
func tildeCollapse(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// formatDaemonRow builds the coloured daemon value string for MetadataRows.
// It uses the daemonStatus* constants so the switch is exhaustive and the
// literals never drift from the constants defined in daemon_status.go.
func formatDaemonRow(c *colorizer.Colorizer, ds daemonReadState) string {
	switch ds.Status {
	case daemonStatusRunning:
		pid := fmt.Sprintf("pid %d", ds.PID)
		up := humanDuration(ds.Uptime)
		return c.Success("●") + "  running " + c.Ident(pid) + "  uptime " + up
	case daemonStatusStale:
		hint := c.Dim("→ maz daemon start")
		return c.Warn("~") + fmt.Sprintf("  stale (pid %d)  ", ds.PID) + hint
	case daemonStatusOtherDataDir:
		hint := c.Dim("→ maz daemon start")
		return c.Warn("~") + fmt.Sprintf("  running elsewhere (pid %d)  ", ds.PID) + hint
	case daemonStatusNotServing:
		hint := c.Dim("→ retry in a few seconds")
		return c.Warn("~") + fmt.Sprintf("  not serving yet (pid %d)  ", ds.PID) + hint
	default: // daemonStatusNotRunning or any unknown state
		hint := c.Dim("→ maz daemon start")
		return c.Err("✗") + "  not running  " + hint
	}
}

// formatLogRow builds the log-freshness value string for MetadataRows.
func formatLogRow(c *colorizer.Colorizer, lr statusLogReport, lastEventTime time.Time) string {
	if lr.EventCount == 0 {
		return c.Dim("no events recorded")
	}
	rel := RelativeTime(lastEventTime)
	return fmt.Sprintf("last event %s · %d events", rel, lr.EventCount)
}

// formatEmbedSignpost builds the embed-provider value string for MetadataRows.
func formatEmbedSignpost(c *colorizer.Colorizer, provider string) string {
	hint := c.Dim("→ maz doctor for health")
	if provider == "" {
		return c.Warn("not configured") + "  " + hint
	}
	return "defaults.embed=" + provider + "  " + hint
}

// formatSyncSignpost builds the sync-scopes value string for MetadataRows.
func formatSyncSignpost(c *colorizer.Colorizer, scopes []string) string {
	hint := c.Dim("→ maz sync status")
	if len(scopes) == 0 {
		return c.Dim("no remotes configured") + "  " + hint
	}
	return fmt.Sprintf("%d scope(s) configured", len(scopes)) + "  " + hint
}

// syncScopeNames returns the sorted list of scope names with a remote
// configured. An empty or nil config returns an empty (non-nil) slice.
// Cfg is taken by pointer to avoid copying the 464-byte struct (gocritic).
func syncScopeNames(cfg *config.UserConfig) []string {
	if cfg == nil || len(cfg.Sync.Remotes) == 0 {
		return []string{}
	}
	names := make([]string, 0, len(cfg.Sync.Remotes))
	for scope := range cfg.Sync.Remotes {
		names = append(names, scope)
	}
	sort.Strings(names)
	return names
}

func wrapStatusFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "status", err)
}
