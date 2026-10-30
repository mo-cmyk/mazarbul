package cli

// hook_status.go implements `maz hook status`: a read-out of the ambient-capture wiring.
// It answers three questions without touching the running session: are the Stop
// and SessionStart lifecycle hooks installed in Claude Code's settings.json, how
// many capture episodes Mazarbul holds, and when capture last fired.
//
// The command is a pure read: install state is detected STRUCTURALLY from
// settings.json (reusing connect.HasMazHookEntry, the same ownership check
// `maz connect` applies), and the capture count/newest come from the event log
// (episodes whose Source carries the passive-capture "hook:" prefix). An absent
// or hook-free settings.json is NOT an error: it reports "not installed" and
// exits 0 so a script can probe the wiring safely.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/connect"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const (
	// Source prefix marking Episode events written by a passive client
	// lifecycle hook (e.g. "hook:claude-code/stop"). Mirrors the kernel's
	// unexported passiveCaptureSourcePrefix; status counts these to report
	// capture volume independently of the deliberate `remember` surface.
	hookCaptureSourcePrefix = "hook:"
	// Cap on the newest-capture text preview, in runes so a multibyte tail is
	// never split mid-character.
	hookStatusPreviewRunes = 120
	// Claude Code hook-event keys under which `maz connect claude-code
	// --capture` and `--warm-start` write their entries in settings.json.
	claudeCodeStopEvent         = "Stop"
	claudeCodeSessionStartEvent = "SessionStart"
	// Conventional user-scope settings file Claude Code reads hooks from.
	// Mirrors clients.claudeCodeHookConfigPath (user scope); shown in text mode
	// so the user knows what to inspect.
	hookSettingsDisplayPath = "~/.claude/settings.json"
	// Hint rendered when neither hook is present in settings.json.
	notInstalledHint = "not installed · run `maz connect claude-code --capture`"
	// Column width that left-aligns the text-mode label column.
	hookStatusLabelWidth = 18
)

// hookEpisodeSummary is the stable JSON shape for the newest capture episode.
type hookEpisodeSummary struct {
	UID        string    `json:"uid"`
	Source     string    `json:"source"`
	CreateTime time.Time `json:"createTime"`
	Preview    string    `json:"preview,omitempty"`
}

// hookStatusReport is the stable JSON shape emitted by `maz hook status --json`.
// The stopInstalled / sessionStartInstalled flags are detected structurally
// from settings.json; captureEpisodeCount, lastFired, and newest come from the
// event log. The lastFired and newest fields are omitted when no capture has
// fired yet.
type hookStatusReport struct {
	Client                string              `json:"client"`
	ConfigPath            string              `json:"configPath"`
	StopInstalled         bool                `json:"stopInstalled"`
	SessionStartInstalled bool                `json:"sessionStartInstalled"`
	CaptureEpisodeCount   int                 `json:"captureEpisodeCount"`
	LastFired             *time.Time          `json:"lastFired,omitempty"`
	Newest                *hookEpisodeSummary `json:"newest,omitempty"`
}

// newHookStatusCmd is `maz hook status`, the ambient-capture wiring read-out.
func newHookStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report ambient-capture hook wiring and capture volume",
		Long: `Report the ambient-capture wiring for Claude Code: whether the Stop and
SessionStart lifecycle hooks are installed in ~/.claude/settings.json, when a
capture last fired, and how many capture episodes Mazarbul holds.

Install state is detected structurally from settings.json (the same ownership
check 'maz connect' uses), so a hook another tool wrote is not miscounted as
Mazarbul's. When no Mazarbul hooks are present this is NOT an error: it reports
"not installed" and exits 0, so a script can probe the wiring safely.

Examples:
  maz hook status
  maz hook status --json
  maz hook status --output json | jq '.stopInstalled'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHookStatus(cmd)
		},
	}
}

// runHookStatus collects the report and renders it in the active output format.
func runHookStatus(cmd *cobra.Command) error {
	report, err := collectHookStatus(cmd)
	if err != nil {
		return respondFatal(cmd, "hook status", err)
	}
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		if eerr := wr.Emit(report); eerr != nil {
			return respondFatal(cmd, "hook status", eerr)
		}
		return nil
	}
	return renderHookStatusText(cmd, report)
}

// collectHookStatus resolves the install state from settings.json and the
// capture count/newest from the event log.
func collectHookStatus(cmd *cobra.Command) (hookStatusReport, error) {
	settingsPath, err := claudeCodeUserSettingsPath()
	if err != nil {
		return hookStatusReport{}, fmt.Errorf("resolve settings path: %w", err)
	}
	stop, sessionStart, err := detectInstalledHooks(settingsPath)
	if err != nil {
		return hookStatusReport{}, err
	}

	count, newest, err := countHookCaptures(cmd.Context(), resolveHookDataDir(cmd))
	if err != nil {
		return hookStatusReport{}, err
	}

	report := hookStatusReport{
		Client:                "claude-code",
		ConfigPath:            settingsPath,
		StopInstalled:         stop,
		SessionStartInstalled: sessionStart,
		CaptureEpisodeCount:   count,
	}
	if newest != nil {
		report.LastFired = &newest.CreateTime
		report.Newest = newest
	}
	return report, nil
}

// claudeCodeUserSettingsPath resolves ~/.claude/settings.json (Claude Code's
// user-scope hook config), mirroring clients.claudeCodeHookConfigPath.
func claudeCodeUserSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// detectInstalledHooks reports whether a Mazarbul-owned Stop / SessionStart hook
// is present in settingsPath. An absent or empty file means "nothing installed"
// (not an error), so the not-installed case stays graceful and exits 0.
func detectInstalledHooks(settingsPath string) (stop, sessionStart bool, err error) {
	data, rerr := os.ReadFile(settingsPath)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read settings: %w", rerr)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return false, false, nil
	}
	var doc map[string]any
	if uerr := json.Unmarshal(data, &doc); uerr != nil {
		return false, false, fmt.Errorf("parse settings: %w", uerr)
	}
	return connect.HasMazHookEntry(doc, claudeCodeStopEvent),
		connect.HasMazHookEntry(doc, claudeCodeSessionStartEvent),
		nil
}

// countHookCaptures scans the event log for Episode events whose Source carries
// the passive-capture "hook:" prefix, returning the total and the newest. The
// event log is the source of truth (no SQLite-index dependency, no scope
// bypass); the scan is bounded by the log size and acceptable for a status read.
func countHookCaptures(ctx context.Context, dataDir string) (int, *hookEpisodeSummary, error) {
	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return 0, nil, fmt.Errorf("open eventlog: %w", err)
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logging.FromContext(ctx).Warn("hook status: close eventlog", zap.Error(cerr))
		}
	}()

	count := 0
	var newest *hookEpisodeSummary
	for ev, rerr := range log.Read(ctx, kernel.ReadOpts{Kinds: []kernel.EventKind{ontology.KindEpisode}}) {
		if rerr != nil {
			return 0, nil, fmt.Errorf("read eventlog: %w", rerr)
		}
		if !strings.HasPrefix(ev.Source, hookCaptureSourcePrefix) {
			continue
		}
		count++
		// Read yields events in chronological order (CreateTime, with the ULID
		// as the within-millisecond tiebreaker), so the last hook episode read
		// is the newest. Overwrite unconditionally.
		newest = &hookEpisodeSummary{
			UID:        ev.UID,
			Source:     ev.Source,
			CreateTime: ev.CreateTime.UTC(),
			Preview:    episodePreview(ev.Payload),
		}
	}
	return count, newest, nil
}

// episodePreview renders a one-line, rune-bounded preview of an episode's text.
func episodePreview(payload json.RawMessage) string {
	var p struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	// Collapse interior whitespace so the multi-turn capture text stays one line.
	text := strings.Join(strings.Fields(p.Text), " ")
	r := []rune(text)
	if len(r) > hookStatusPreviewRunes {
		return string(r[:hookStatusPreviewRunes]) + "…"
	}
	return text
}

// renderHookStatusText renders the human-readable status block. Colour is gated
// on interactive.ShouldStyle, so a non-TTY / NO_COLOR invocation is byte-stable.
func renderHookStatusText(cmd *cobra.Command, r hookStatusReport) error {
	w := cmd.OutOrStdout()
	clr := colorizer.New(interactive.ShouldStyle(cmd, w), w)

	lines := []string{
		clr.Accent("Ambient capture (Claude Code)"),
		"",
		statusRow("Stop hook", installedLabel(clr, r.StopInstalled)),
		statusRow("SessionStart hook", installedLabel(clr, r.SessionStartInstalled)),
		statusRow("Capture episodes", clr.Meta(strconv.Itoa(r.CaptureEpisodeCount))),
	}
	if r.LastFired != nil {
		when := r.LastFired.Format(time.RFC3339) + " (" + humanDuration(time.Since(*r.LastFired)) + " ago)"
		lines = append(lines, statusRow("Last fired", when))
	}
	if r.Newest != nil {
		lines = append(lines, statusRow("Newest", clr.Meta(r.Newest.Source)))
		if r.Newest.Preview != "" {
			lines = append(lines, statusRow("", clr.Dim(`"`+r.Newest.Preview+`"`)))
		}
	}
	if !r.StopInstalled && !r.SessionStartInstalled {
		lines = append(lines, "", "  "+clr.Dim(notInstalledHint))
	}
	lines = append(lines, "", "Configured in "+hookSettingsDisplayPath)

	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}

// statusRow formats one indented "label   value" row with an aligned label
// column. Colour, when present, is confined to the value (the last column), so
// label alignment is unaffected by escape sequences.
func statusRow(label, value string) string {
	return fmt.Sprintf("  %-*s%s", hookStatusLabelWidth, label, value)
}

// installedLabel renders the install state word with semantic colour.
func installedLabel(clr *colorizer.Colorizer, installed bool) string {
	if installed {
		return clr.Success("installed")
	}
	return clr.Dim("not installed")
}
