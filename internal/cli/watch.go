package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemonclient"
	"github.com/mo-cmyk/mazarbul/internal/tui/watch"
)

func newWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Live observability dashboard (events, jobs, embedding, sync)",
		Long: `maz watch opens a full-screen live dashboard showing:
  [1] events tail         — incoming SSE events from the daemon
  [2] extraction jobs     — in-flight episode→entity→relation pipeline
  [3] embedding queue     — depth and worker throughput
  [4] sync status         — git remote ahead/behind + last push
  [5] sources             — connector freshness (● live / ▲ stale / ✕ down)

For a scriptable view of source freshness use 'maz source list' (its JSON adds
status + ageSeconds) or 'maz doctor', which flag stale/down connectors.

Keyboard: q/Ctrl+C quit, 1-5 pane focus, j/k scroll, r reload,
          ? help, / search, e event-kind filter, p pause, f follow.

When stdout is not a TTY, or --output ndjson is set, the dashboard
is skipped and merged {pane,ts,payload} JSON lines are emitted on
stdout instead (agent-friendly path).`,
		RunE: runWatch,
	}
	cmd.Flags().Bool("demo", false, "use synthetic data (offline / for testing); default is the daemon's real event stream")
	return cmd
}

func runWatch(cmd *cobra.Command, _ []string) error {
	// Honour the root persistent --output flag so `maz watch
	// --output ndjson` (placed anywhere on the line) routes through
	// the agent-friendly NDJSON path. output.From resolves the
	// inherited flag + MAZ_OUTPUT + TTY state in one call.
	w := output.From(cmd)
	dataDir, _ := cmd.Flags().GetString("data-dir")
	if dataDir == "" {
		dataDir = os.Getenv("MAZ_DATA_DIR")
	}
	demo, _ := cmd.Flags().GetBool("demo")
	// Default to the real data dir (matching every other command) so `maz watch`
	// streams the daemon's actual events. Only --demo opts into synthetic data;
	// previously an unset --data-dir silently fell back to the fake streamer,
	// which is why `maz watch` showed demo data instead of what was ingested.
	if dataDir == "" && !demo {
		dataDir = config.Defaults().DataDir
	}
	// A real stream needs a daemon bound to this data dir's socket. If a real
	// stream was requested but none is listening (only a stdio `mcp serve`, or
	// no daemon at all), do NOT silently fabricate synthetic events — that hangs
	// on a dead socket in the best case and floods agents with fake data in the
	// worst. Say what's wrong and exit; synthetic data stays opt-in via --demo.
	if notice := watchNoDaemonNotice(dataDir, demo); notice != "" {
		//nolint:errcheck // best-effort advisory on stderr
		fmt.Fprintln(cmd.ErrOrStderr(), notice)
		return nil
	}

	streamer, closeFn, err := resolveWatchStreamer(cmd.Context(), dataDir, demo)
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	defer closeFn()

	if w.Format() == output.FormatNDJSON || !w.IsTTY() {
		return runWatchNDJSON(cmd.Context(), streamer)
	}

	return watch.Run(cmd.Context(), dataDir, streamer, watchProvidersLine(dataDir))
}

// watchProvidersLine renders the active chat + embed provider/model
// pair for the watch footer, read from the resolved user config under
// dataDir. It never fails the dashboard: a missing dataDir or an
// unreadable config degrades to a short placeholder rather than
// returning an error, because the footer is purely cosmetic.
func watchProvidersLine(dataDir string) string {
	if dataDir == "" {
		return "(no data dir)"
	}
	cfg, err := config.LoadResolvedConfig(dataDir)
	if err != nil {
		return "(config unavailable)"
	}
	return formatProvidersLine(&cfg)
}

// formatProvidersLine formats the chat and embed slots from cfg as
// "chat <provider> <model> · embed <provider> <model>". It is pure so
// the footer rendering is unit-testable without touching disk. Model
// resolution mirrors config's slot rules: the chat slot prefers
// ChatModel then falls back to the single Model field; the embed slot
// prefers EmbedModel then Model. An empty embed default renders
// "embed off" (recall degrades to FTS-only — see DefaultsTable).
func formatProvidersLine(cfg *config.UserConfig) string {
	return fmt.Sprintf("chat %s · embed %s",
		providerSlotLabel(cfg, cfg.Defaults.Chat, true),
		providerSlotLabel(cfg, cfg.Defaults.Embed, false))
}

// providerSlotLabel returns "<provider> <model>" for the named provider,
// picking the chat or embed model per chatSlot. An empty provider name
// means the slot is unconfigured (chat) or disabled (embed). Falls back
// to the bare provider name when the provider is absent from the config
// or exposes no model for the slot.
func providerSlotLabel(cfg *config.UserConfig, provider string, chatSlot bool) string {
	if provider == "" {
		if chatSlot {
			return "(unconfigured)"
		}
		return "(disabled)"
	}
	p, ok := cfg.Providers[provider]
	if !ok {
		return provider
	}
	model := p.Model
	switch {
	case chatSlot && p.ChatModel != "":
		model = p.ChatModel
	case !chatSlot && p.EmbedModel != "":
		model = p.EmbedModel
	}
	if model == "" {
		return provider
	}
	return provider + " " + model
}

// watchNoDaemonNotice returns the advisory to print when a real event stream
// was requested but no daemon is bound to dataDir's socket (only a stdio
// `mcp serve`, or nothing running). The caller prints it and exits instead of
// streaming: a live stream would hang on the dead socket, and falling back to
// synthetic data floods the user (or a piping agent) with fabricated events
// that look real. Synthetic data stays opt-in via --demo, so this returns ""
// when --demo is set or a daemon is actually listening.
func watchNoDaemonNotice(dataDir string, demo bool) string {
	if demo || probeDaemonListening(dataDir) {
		return ""
	}
	return fmt.Sprintf(
		"watch: no daemon is serving %s — start one with `maz daemon start` to watch real events, or run `maz watch --demo` for a synthetic demo.",
		dataDir)
}

// resolveWatchStreamer picks the EventStreamer implementation. The
// default is the live daemon stream; --demo (or a missing data-dir)
// falls back to FakeEventStreamer so the command stays usable for
// docs / offline demos without booting the daemon.
//
//nolint:ireturn // intentionally polymorphic: returns either daemonStreamer or FakeEventStreamer depending on --demo/dataDir
func resolveWatchStreamer(ctx context.Context, dataDir string, demo bool) (watch.EventStreamer, func(), error) {
	if demo || dataDir == "" {
		return &watch.FakeEventStreamer{Seed: time.Now().UnixNano()}, func() {}, nil
	}
	client, err := daemonclient.New(dataDir)
	if err != nil {
		return nil, func() {}, fmt.Errorf("daemon client: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	return newDaemonStreamer(streamCtx, client), cancel, nil
}

// runWatchNDJSON drains the streamer and writes merged
// {pane, ts, payload} NDJSON lines until ctx is cancelled. The line
// shape matches what the TUI pane reads internally so an agent
// piping `maz watch --output ndjson | jq` sees the same data the
// human sees in the dashboard.
func runWatchNDJSON(ctx context.Context, streamer watch.EventStreamer) error {
	enc := json.NewEncoder(os.Stdout)
	ch := streamer.Events(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			line := map[string]any{
				"pane":    string(e.Kind),
				"ts":      e.TS.Format(time.RFC3339Nano),
				"payload": e.Payload,
			}
			if err := enc.Encode(line); err != nil {
				return output.Errorf(output.ExitGenericFailure, output.CodeInternal,
					"watch: encode: %v", err)
			}
		}
	}
}
