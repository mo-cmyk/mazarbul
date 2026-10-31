package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"charm.land/huh/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/connect"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/tui/style"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// dataDirMode is the canonical permission set for the data directory.
const dataDirMode os.FileMode = 0o700

// initFlags collects the flags wired onto `maz init`. Only consumed by
// the non-TUI paths; the TUI form prompts for everything interactively.
type initFlags struct {
	provider      string
	chatModel     string
	embedProvider string
	embedModel    string
	apiKey        string
	noIdentity    bool
}

func newInitCmd() *cobra.Command {
	flags := &initFlags{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a Mazarbul data directory",
		Long: `Set up the local data directory and the model providers.

Your agent is the brain, Mazarbul is the memory. The agent that calls Mazarbul
over MCP (Claude Code, Cursor, ...) already is a strong model, so by default no
server-side chat model is configured (--provider none): the agent decides what
to remember, writes entities, relations, and facts with the MCP tools, and
answers from recalled context. Pick anthropic or ollama only when you want
Mazarbul to reason without an agent in the loop (passive file watching,
connector ingestion, ` + "`maz ask`" + ` from a plain terminal, background
consolidation). Embeddings (--embed ollama) power semantic recall either way.

Re-runnable; idempotent. In TTY mode (default) you get an interactive form.
With --yes or --output=json the command accepts the same inputs as flags and
emits plain or machine-readable output.

By default init also brings the background daemon up so the CLI and connected
agents work immediately. On macOS that installs a launchd agent (starts now,
relaunches on every login); elsewhere it's a one-shot background process.
Control this with --daemon: "auto" (the default for the form and --yes),
"once" (one-shot, no login persistence), or "off". --output=json never starts
the daemon unless --daemon is given.

Examples:
  # Interactive form (TTY) — confirms starting the daemon + connecting clients:
  maz init

  # Non-interactive one-shot with all defaults (starts the daemon):
  maz init --yes

  # Set up config but do not touch the daemon:
  maz init --yes --daemon=off

  # Add a local server-side chat model for agent-less ingestion and maz ask:
  maz init --yes --provider ollama

  # Machine-readable, explicitly start the daemon:
  maz init --output=json --daemon=auto`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.provider, "provider", chatProviderNone, "server-side chat provider: none (your agent does the reasoning) | anthropic | ollama")
	cmd.Flags().StringVar(&flags.chatModel, "chat-model", "", "chat model identifier (defaults to provider's recommended model)")
	cmd.Flags().StringVar(&flags.embedProvider, "embed", embedProviderOllama, "embedding provider: ollama | none (none disables recall)")
	cmd.Flags().StringVar(&flags.embedModel, "embed-model", ollama.DefaultEmbedModel, "Ollama embedding model (ignored when --embed=none)")
	cmd.Flags().StringVar(&flags.apiKey, "api-key", "", "API key for the chat provider (stored in OS keychain)")
	cmd.Flags().BoolVar(&flags.noIdentity, "no-identity", false, "skip Watchword and identity provisioning (for deployments that manage credentials externally)")
	cmd.Flags().Bool("yes", false, "accept all defaults and initialize non-interactively")
	cmd.Flags().String("connect", "none", "after init, register Mazarbul with AI clients: \"none\" | \"all\" | comma list (claude-code,claude-desktop,…)")
	cmd.Flags().String("daemon", "", "start the background daemon after init: \"auto\" (launchd on macOS, else one-shot) | \"once\" (one-shot) | \"off\" (default: auto for the form and --yes; off for --output=json)")
	cmd.Flags().String("preset", "", "install a steering preset that teaches the agent when to use memory: \"agent-memory\" | a builtin name | \"none\" (default: the form offers it; non-interactive skips unless named)")
	return cmd
}

func runInit(cmd *cobra.Command, flags *initFlags) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	yes, _ := cmd.Flags().GetBool("yes")
	if yes {
		detectYesDefaults(cmd.Flags(), flags)
	}
	impliedProviderFromAPIKey(cmd.Flags(), flags)
	connectSpec, _ := cmd.Flags().GetString("connect")
	daemonFlag, _ := cmd.Flags().GetString("daemon")
	presetSpec, _ := cmd.Flags().GetString("preset")
	if err := validateDaemonMode(daemonFlag); err != nil {
		return err
	}

	w := output.From(cmd)
	jsonMode := w.Format() == output.FormatJSON
	daemonMode := resolveDaemonMode(daemonFlag, yes, jsonMode)
	switch {
	case jsonMode:
		if err := validateInitFlags(flags); err != nil {
			return err
		}
		return runInitJSON(cmd, dataDir, flags, daemonMode, resolveInitPreset(presetSpec), connectSpec)
	case yes || !w.IsTTY():
		if err := validateInitFlags(flags); err != nil {
			return err
		}
		if err := runInitCLI(cmd, dataDir, flags, yes, daemonMode, resolveInitPreset(presetSpec)); err != nil {
			return err
		}
		return runInitConnectStep(cmd.Context(), cmd.OutOrStdout(), dataDir, connectSpec)
	default:
		// The interactive form owns the daemon confirm, the preset offer, and
		// (unless the user passed --connect explicitly) the client multi-select.
		offerConnect := !cmd.Flags().Changed("connect")
		if err := runInitForm(cmd, dataDir, daemonFlag, offerConnect, presetSpec); err != nil {
			return err
		}
		if !offerConnect {
			return runInitConnectStep(cmd.Context(), cmd.OutOrStdout(), dataDir, connectSpec)
		}
		return nil
	}
}

// daemon start modes for `maz init --daemon`.
const (
	daemonModeAuto = "auto"
	daemonModeOnce = "once"
	daemonModeOff  = "off"
)

// validateDaemonMode rejects an unknown --daemon value early (empty is
// valid: it defers to the per-mode default resolved by resolveDaemonMode).
func validateDaemonMode(flagVal string) error {
	switch flagVal {
	case "", daemonModeAuto, daemonModeOnce, daemonModeOff:
		return nil
	default:
		return fmt.Errorf("unknown --daemon %q (supported: auto, once, off)", flagVal)
	}
}

// resolveDaemonMode applies the empty-default policy for --daemon:
//   - an explicit auto|once|off is honored verbatim (and, in the form,
//     skips the interactive confirm);
//   - empty + --yes -> auto (turnkey default). --yes is an explicit
//     "accept the defaults" signal, so it wins even when stdout is piped
//     and the renderer falls back to JSON;
//   - empty + --output=json (without --yes) -> off, so a bare machine
//     caller opts in explicitly;
//   - empty + interactive TTY -> "" so the form's confirm prompt decides.
func resolveDaemonMode(flagVal string, yes, jsonMode bool) string {
	if flagVal != "" {
		return flagVal
	}
	switch {
	case yes:
		return daemonModeAuto
	case jsonMode:
		return daemonModeOff
	default:
		return ""
	}
}

// initDaemonOutcome is what the init daemon step reports back to the
// renderers (text summary line + JSON object).
type initDaemonOutcome struct {
	Mode      string // resolved mode that ran: auto | once | off
	Persisted bool   // a launchd agent was installed (login persistence)
	Running   bool   // daemon is up (or was already running)
	PID       int    // child PID when known (one-shot path)
	Detail    string // human summary for the text scrollback
}

// startDaemonForInitFn is the seam tests stub so init never spawns a real
// daemon or touches ~/Library/LaunchAgents. Production code must not
// reassign it.
var startDaemonForInitFn = startDaemonForInit //nolint:gochecknoglobals

// provisionIdentityIfNeeded generates and persists a Watchword when none
// exists yet, then derives and caches the identity keypairs. Idempotent:
// if a Watchword is already present the function is a no-op and returns
// ("", nil). The returned mnemonic (non-empty only on a fresh generation)
// must be disclosed to the operator exactly once so they can save it.
func provisionIdentityIfNeeded(ctx context.Context) (string, error) {
	chain := secret.PersistentChain()
	existing, err := chain.Get(ctx, watchword.KeychainKey)
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		return "", fmt.Errorf("init: check watchword: %w", err)
	}
	if existing != "" {
		return "", nil // already provisioned; idempotent
	}
	mnemonic, err := watchword.Generate()
	if err != nil {
		return "", fmt.Errorf("init: generate watchword: %w", err)
	}
	if err := chain.Set(ctx, watchword.KeychainKey, mnemonic); err != nil {
		return "", fmt.Errorf("init: persist watchword: %w", err)
	}
	// Best-effort: derive and cache keypairs now so the daemon's first boot
	// does not pay the Argon2id cost cold. A failure here is non-fatal;
	// the daemon re-derives from the Watchword on boot.
	//nolint:errcheck // best-effort key-cache warm; daemon re-derives on boot
	_, _ = identity.New(chain).Initialize(ctx)
	return mnemonic, nil
}

// provisionIdentityForInitFn is the seam tests override to avoid touching
// the OS keychain during isolated test runs. Production code must not
// reassign it.
var provisionIdentityForInitFn = provisionIdentityIfNeeded //nolint:gochecknoglobals

// startDaemonForInit brings the daemon up per the resolved mode. "off"
// (and empty) is a no-op. "auto" installs a launchd agent on macOS
// (persistent + start now) and degrades to a one-shot spawn elsewhere;
// "once" always does a one-shot spawn. Daemon.ErrAlreadyRunning is not an
// error here — init is idempotent. A returned error is surfaced by the
// caller as a soft warning; init itself still succeeds.
func startDaemonForInit(ctx context.Context, dataDir, mode string) (initDaemonOutcome, error) {
	switch mode {
	case daemonModeOff, "":
		return initDaemonOutcome{Mode: daemonModeOff}, nil
	case daemonModeAuto:
		if runtime.GOOS == darwinGOOS {
			ready, err := installDaemonLaunchAgent(ctx, dataDir)
			if err != nil {
				return initDaemonOutcome{Mode: daemonModeAuto}, err
			}
			// The launchd child binds only the Unix socket (its plist
			// enables no TCP transport), so report the socket path.
			socket := daemonSocketPath(dataDir)
			detail := "running on socket " + socket + " (launchd; relaunches on login)"
			if !ready {
				detail = "starting via launchd (socket " + socket + ")"
			}
			return initDaemonOutcome{Mode: daemonModeAuto, Persisted: true, Running: ready, Detail: detail}, nil
		}
		return spawnDaemonOnceForInit(ctx, dataDir, daemonModeAuto, true)
	case daemonModeOnce:
		return spawnDaemonOnceForInit(ctx, dataDir, daemonModeOnce, false)
	default:
		return initDaemonOutcome{Mode: mode}, fmt.Errorf("unknown daemon mode %q", mode)
	}
}

// spawnDaemonOnceForInit forks a one-shot daemon and shapes the outcome.
// Degraded=true means this is the non-macOS fallback for "auto", where
// login-persistence is unavailable — the summary says so.
func spawnDaemonOnceForInit(ctx context.Context, dataDir, mode string, degraded bool) (initDaemonOutcome, error) {
	res, err := spawnDaemonOnce(ctx, dataDir, "")
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			return initDaemonOutcome{Mode: mode, Running: true, Detail: "already running"}, nil
		}
		return initDaemonOutcome{Mode: mode}, err
	}
	detail := fmt.Sprintf("running (pid %d)", res.PID)
	if degraded {
		detail += "; login-persistence is macOS-only"
	}
	return initDaemonOutcome{Mode: mode, Running: true, PID: res.PID, Detail: detail}, nil
}

// applyInitDaemonStep runs the daemon step for the text paths (form +
// CLI) and prints a summary line. Failures are soft: a warning line is
// written and nil is returned so init still completes.
func applyInitDaemonStep(ctx context.Context, w io.Writer, dataDir, mode string) error {
	if mode == daemonModeOff || mode == "" {
		return nil
	}
	outcome, err := startDaemonForInitFn(ctx, dataDir, mode)
	if err != nil {
		_, werr := fmt.Fprintf(w, "warning: daemon not started: %v (run `maz daemon start` later)\n", err)
		return werr
	}
	return printStepSummary(w, "daemon", outcome.Detail)
}

// initPresetNone is the --preset sentinel that skips the steering on-ramp.
const initPresetNone = "none"

// resolveInitPreset maps --preset to the preset to install on the
// NON-interactive paths. An empty value means "skip": a turnkey `maz init --yes`
// must not silently inject agent-steering instructions, so the on-ramp is
// opt-in there. An explicit name (e.g. "agent-memory") is honored verbatim.
func resolveInitPreset(spec string) string {
	if spec == "" {
		return initPresetNone
	}
	return spec
}

// applyInitPresetStep installs the named steering preset against dataDir unless
// name is the skip sentinel. It runs in the storage-unlocked window (after
// prewarmStorage, before the daemon starts) because installBuiltinPreset opens
// its own wiring and needs the store lock. Soft-fails: a warning is written and
// nil returned so a preset hiccup never aborts an otherwise-complete init.
func applyInitPresetStep(ctx context.Context, cmd *cobra.Command, w io.Writer, dataDir, name string) error {
	if name == "" || name == initPresetNone {
		return nil
	}
	// installBuiltinPreset opens its own wiring and needs the store lock. If a
	// daemon already serves this dir that open would contend (the same "status 1"
	// prewarmStorage avoids above), so skip and point the user at the explicit
	// install path against the running daemon instead.
	if probeDaemonListening(dataDir) {
		_, werr := fmt.Fprintf(w,
			"note: a daemon is already serving this data dir; skipping preset install — run `maz preset install %s` once if you want it.\n",
			name)
		return werr
	}
	if _, err := installBuiltinPreset(ctx, cmd, dataDir, name); err != nil {
		_, werr := fmt.Fprintf(w,
			"warning: steering preset %q not installed: %v (install later with `maz preset install %s`)\n",
			name, err, name)
		return werr
	}
	return printStepSummary(w, "preset", name+" (steering instructions active)")
}

// detectYesDefaults fills zero-value flag fields with opinionated defaults
// when --yes is set. Only fields that the user has NOT explicitly changed
// (cmd.Flags().Changed == false) are touched, so explicit overrides always win.
// This function is pure (no I/O) and runs before validateInitFlags.
func detectYesDefaults(fs *pflag.FlagSet, flags *initFlags) {
	if !fs.Changed("provider") {
		flags.provider = chatProviderNone
	}
	// --chat-model: leave empty; defaultChatModel resolves it per provider.
	if !fs.Changed("embed") {
		flags.embedProvider = embedProviderOllama
	}
	if !fs.Changed("embed-model") {
		flags.embedModel = ollama.DefaultEmbedModel
	}
}

// impliedProviderFromAPIKey keeps `maz init --api-key <key>` meaningful now
// that the chat default is none: an Anthropic key without an explicit
// --provider selects the Anthropic chat provider. An explicit --provider
// always wins (and persistAPIKeyIfProvided rejects a key for non-Anthropic).
func impliedProviderFromAPIKey(fs *pflag.FlagSet, flags *initFlags) {
	if flags.apiKey != "" && !fs.Changed("provider") {
		flags.provider = chatProviderAnthropic
	}
}

// validateInitFlags rejects flag combinations the non-interactive paths
// cannot satisfy. The form validates interactively and does not need
// this gate.
func validateInitFlags(flags *initFlags) error {
	switch flags.provider {
	case chatProviderNone:
		// No server-side chat model: the calling agent does the reasoning.
	case chatProviderAnthropic:
		// Anthropic uses a baked-in default model; --chat-model is optional.
	case chatProviderOllama:
		// Ollama defaults to ollama.DefaultChatModel via defaultChatModel when
		// --chat-model is omitted, so no explicit pick is required here.
	default:
		return fmt.Errorf("unknown --provider %q (supported: none, anthropic, ollama)", flags.provider)
	}
	switch flags.embedProvider {
	case embedProviderOllama, embedProviderNone:
	default:
		return fmt.Errorf("unknown --embed %q (supported: ollama, none)", flags.embedProvider)
	}
	return nil
}

// userConfigFromFlags builds a UserConfig from CLI flag values. Used by
// both the CLI and JSON modes so the two stay in sync. The Ollama table
// is omitted entirely when neither chat nor embed routes through it, so
// an Anthropic+none install produces the minimum-viable config. With
// --provider none, defaults.chat is written as the explicit "none" sentinel
// (the daemon's chat-disabled path) and no chat model is recorded anywhere:
// no [providers.anthropic] table and no ollama chat_model. The anthropic and
// ollama paths keep writing the [providers.anthropic] table as before.
func userConfigFromFlags(f *initFlags) config.UserConfig {
	chatModel := f.chatModel
	if chatModel == "" {
		chatModel = defaultChatModel(f.provider)
	}
	providers := map[string]config.ProviderTable{}
	if f.provider != chatProviderNone {
		providers["anthropic"] = config.ProviderTable{
			Model:  defaultChatModel(chatProviderAnthropic),
			Secret: config.SecretURIScheme + secret.NameAnthropicAPIKey,
		}
	}
	if ollamaTableNeeded(f.provider, f.embedProvider) {
		providers["ollama"] = config.ProviderTable{
			BaseURL:    ollama.DefaultBaseURL,
			ChatModel:  ollamaChatFor(f.provider, chatModel),
			EmbedModel: ollamaEmbedFor(f.embedProvider, f.embedModel),
		}
	}
	return config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Providers:     providers,
		Defaults: config.DefaultsTable{
			Chat:  f.provider,
			Embed: f.embedProvider,
		},
	}
}

// ollamaTableNeeded reports whether the resulting config.toml should
// include a [providers.ollama] section. Today that's iff Ollama serves
// at least one slot (chat or embed); future per-slot providers tighten
// this check the same way.
func ollamaTableNeeded(chat, embed string) bool {
	return chat == chatProviderOllama || embed == embedProviderOllama
}

// ollamaEmbedFor returns the embed model string to record under
// [providers.ollama] for the given embed-provider selection. Empty when
// the user picked --embed=none so the persisted table does not claim a
// model the daemon will never load.
func ollamaEmbedFor(embedProvider, embedModel string) string {
	if embedProvider == embedProviderOllama {
		return embedModel
	}
	return ""
}

// defaultChatModel returns the recommended chat model for a given provider, so
// `maz init --provider <p>` works without an explicit --chat-model (symmetric
// with --embed, which already defaults via ollama.DefaultEmbedModel). Empty
// for the none sentinel: no server-side chat model is configured.
func defaultChatModel(provider string) string {
	switch provider {
	case chatProviderNone:
		return ""
	case chatProviderAnthropic:
		return "claude-opus-4-7"
	}
	return ollama.DefaultChatModel
}

// ollamaChatFor returns the chat-model string to record under
// [providers.ollama] for the given selected provider. Empty unless Ollama
// is the chat provider.
func ollamaChatFor(provider, chatModel string) string {
	if provider == chatProviderOllama {
		return chatModel
	}
	return ""
}

// chatSummary renders the chat-step summary line. The none sentinel reads
// as the intended agent-side-reasoning setup rather than "none ()".
func chatSummary(provider, model string) string {
	if provider == chatProviderNone {
		return "none (your agent does the reasoning; add one later with `maz init --provider anthropic|ollama`)"
	}
	return fmt.Sprintf("%s (%s)", provider, model)
}

// embeddingsSummary renders the embed-step summary line so the post-init
// scrollback shows "skipped" for the none-sentinel rather than the
// misleading "none ()" the raw formatter would produce.
func embeddingsSummary(provider, model string) string {
	if provider == embedProviderNone {
		return "skipped (recall disabled; re-init to enable)"
	}
	return fmt.Sprintf("%s (%s)", provider, model)
}

// prepareInit runs the shared non-interactive init preamble for both the JSON
// and plain-text paths: it ensures the data dir exists, keeps the per-project
// config overlay out of version control, persists any provided API key, then
// builds, bootstraps, writes, and pre-warms the user config. It returns the
// resolved config so the JSON path can echo the effective defaults.
func prepareInit(ctx context.Context, dataDir string, flags *initFlags) (config.UserConfig, error) {
	if err := ensureDataDir(dataDir); err != nil {
		return config.UserConfig{}, err
	}
	// Best-effort: add .mazarbul.toml to .gitignore in cwd so per-project
	// config overlays stay local. Non-fatal on failure.
	if wd, err := os.Getwd(); err == nil {
		//nolint:errcheck // best-effort gitignore update; non-fatal
		_ = config.AddProjectConfigToGitignore(wd)
	}
	if err := persistAPIKeyIfProvided(ctx, flags); err != nil {
		return config.UserConfig{}, err
	}
	cfg := userConfigFromFlags(flags)
	if err := bootstrapLocalScope(ctx, dataDir, &cfg); err != nil {
		return config.UserConfig{}, err
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		return config.UserConfig{}, err
	}
	if err := prewarmStorage(ctx, dataDir); err != nil {
		return config.UserConfig{}, err
	}
	return cfg, nil
}

func runInitJSON(cmd *cobra.Command, dataDir string, flags *initFlags, daemonMode, presetName, connectSpec string) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	cfg, err := prepareInit(ctx, dataDir, flags)
	if err != nil {
		return err
	}

	// Provision Watchword + identity keypairs so the device has a bound
	// principal after a headless install. Idempotent: a no-op when the
	// Watchword already exists. The recovery phrase (when newly generated)
	// is included in the JSON response so the operator can save it.
	var recoveryPhrase, identityWarning string
	if !flags.noIdentity {
		rp, iErr := provisionIdentityForInitFn(ctx)
		if iErr != nil {
			identityWarning = iErr.Error()
		} else {
			recoveryPhrase = rp
		}
	}

	out := map[string]any{
		"ok":         true,
		"dataDir":    dataDir,
		"configPath": config.UserConfigPath(dataDir),
		"chat":       cfg.Defaults.Chat,
		"embed":      cfg.Defaults.Embed,
		"localScope": cfg.Sync.Remotes[defaultLocalScopeName].URL,
	}
	if recoveryPhrase != "" {
		out["recoveryPhrase"] = recoveryPhrase
	}
	if identityWarning != "" {
		out["identityWarning"] = identityWarning
	}
	// Steering preset on-ramp (opt-in via --preset). Seeded here, before the
	// daemon takes the store lock. Quiet on stdout so JSON stays clean; the
	// outcome is reported in the block.
	if presetName != "" && presetName != initPresetNone {
		block := map[string]any{"name": presetName}
		if _, perr := installBuiltinPreset(ctx, cmd, dataDir, presetName); perr != nil {
			block["error"] = perr.Error()
		} else {
			block["installed"] = true
		}
		out["preset"] = block
	}
	// JSON mode only touches the daemon when explicitly asked (resolved
	// to off by default). Failures are reported in the block, not fatal.
	if daemonMode != "" && daemonMode != daemonModeOff {
		outcome, derr := startDaemonForInitFn(ctx, dataDir, daemonMode)
		block := map[string]any{
			"mode":      outcome.Mode,
			"running":   outcome.Running,
			"persisted": outcome.Persisted,
		}
		if outcome.PID > 0 {
			block["pid"] = outcome.PID
		}
		if derr != nil {
			block["error"] = derr.Error()
		}
		out["daemon"] = block
	}
	// Connect step: always present in the JSON output so scripted callers can
	// introspect the result. When --connect=none (or unset) the field carries a
	// single skipped entry with a reason; otherwise each named client gets its
	// own entry with action/configPath/error.
	out["connect"] = collectConnectResults(ctx, dataDir, connectSpec)
	return json.NewEncoder(w).Encode(out)
}

// initConnectRow is the JSON shape used by collectConnectResults. A separate
// type (not the CLI-internal connectResult) lets the init output stay stable
// regardless of changes to the standalone `maz connect` renderer.
type initConnectRow struct {
	// ClientID is the adapter identifier (e.g. "claude-code").
	ClientID string `json:"clientId"`
	// Action describes what the adapter did ("created", "updated", "noop", "skipped", …).
	Action string `json:"action,omitempty"`
	// Scope is the registration breadth ("user", "project", …).
	Scope string `json:"scope,omitempty"`
	// ConfigPath is the on-disk file the adapter wrote.
	ConfigPath string `json:"configPath,omitempty"`
	// Skipped is non-empty when the step was deliberately omitted.
	Skipped string `json:"skipped,omitempty"`
	// Error is non-empty when the adapter returned an error.
	Error string `json:"error,omitempty"`
}

// collectConnectResults runs the connect step and returns structured per-client
// rows for embedding in the JSON init output. When connectSpec is "none" or
// empty a single skipped entry is returned so the caller always has a "connect"
// key, making the output stable for scripted consumers.
func collectConnectResults(ctx context.Context, dataDir, spec string) []initConnectRow {
	if spec == "" || spec == connectSpecNone {
		return []initConnectRow{{
			ClientID: connectSpecAll,
			Skipped:  "pass --connect=all or --connect=<client> to register clients",
		}}
	}
	svc := newConnectService(dataDir)
	var targets []string
	if spec == connectSpecAll {
		for _, c := range svc.Clients() {
			targets = append(targets, c.ID())
		}
	} else {
		for _, t := range strings.Split(spec, ",") {
			if t = strings.TrimSpace(t); t != "" {
				targets = append(targets, t)
			}
		}
	}
	rows := make([]initConnectRow, 0, len(targets))
	for _, id := range targets {
		rep, applyErr := svc.Apply(ctx, id, connect.ScopeUser, "", connect.ApplyOptions{})
		row := initConnectRow{
			ClientID:   rep.ClientID,
			Action:     rep.Action,
			Scope:      string(rep.Scope),
			ConfigPath: rep.ConfigPath,
		}
		if row.ClientID == "" {
			row.ClientID = id
		}
		if applyErr != nil {
			row.Error = applyErr.Error()
		}
		rows = append(rows, row)
	}
	return rows
}

// runInitCLI is the plain-text non-interactive init path. The yes parameter
// controls whether to print the full summary block (data dir, chat, embeddings,
// ready line) that would otherwise only appear in the form. DaemonMode is the
// already-resolved daemon policy ("" / "off" is a no-op).
func runInitCLI(cmd *cobra.Command, dataDir string, flags *initFlags, yes bool, daemonMode, presetName string) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	if _, err := prepareInit(ctx, dataDir, flags); err != nil {
		return err
	}

	// Provision Watchword + identity keypairs so the device has a bound
	// principal after a headless install. Idempotent: a no-op when the
	// Watchword already exists. Newly generated phrases are printed once
	// so the operator can record them.
	if !flags.noIdentity {
		mnemonic, iErr := provisionIdentityForInitFn(ctx)
		if iErr != nil {
			if _, werr := fmt.Fprintf(w, "warning: identity not provisioned: %v (run `maz secret set %s <phrase>` to provision manually)\n", iErr, watchword.KeychainKey); werr != nil {
				return werr
			}
		} else if mnemonic != "" {
			if _, werr := fmt.Fprintf(w, "Watchword generated. Store these 24 words securely — they are the only way to recover your identity:\n%s\n", mnemonic); werr != nil {
				return werr
			}
		}
	}

	if err := writeInitSummary(w, dataDir, flags, yes); err != nil {
		return err
	}

	// Install the steering preset (opt-in via --preset) while the storage lock
	// is free — before the daemon below takes it.
	if err := applyInitPresetStep(ctx, cmd, w, dataDir, presetName); err != nil {
		return err
	}

	// Start the daemon (prewarm above already released the storage lock)
	// before the final ready line so its outcome — including a soft
	// "warning: daemon not started" — is not contradicted by an earlier
	// "Ready.", matching the interactive form's ordering.
	if err := applyInitDaemonStep(ctx, w, dataDir, daemonMode); err != nil {
		return err
	}

	// Warn if Anthropic was selected without a key on hand. We don't
	// block; init is idempotent and the user can run `maz secret set`
	// later (or re-run init with --api-key).
	if flags.provider == chatProviderAnthropic && flags.apiKey == "" && !anthropicKeyAvailableFn() {
		if _, err := fmt.Fprintln(w, "warning: Anthropic API key not found; run `maz secret set anthropic.api_key` before `maz remember`."); err != nil {
			return err
		}
	}

	// The full summary block (yes=true) ends with the ready line; emit it
	// last, once the daemon step above has reported its real status.
	if yes {
		return printReady(w)
	}
	return nil
}

// connectSpecNone is the default --connect value: skip the step and
// print a hint pointing at `maz connect all`.
const connectSpecNone = "none"

// connectSpecAll is the --connect value that registers every detected client.
const connectSpecAll = "all"

// runInitConnectStep is invoked after a successful init when the user
// supplied --connect=<clients>. It registers Mazarbul with each named
// client via the same connect.Service the standalone `maz connect`
// command uses; default connectSpecNone / empty skips with a hint.
func runInitConnectStep(ctx context.Context, w io.Writer, dataDir, spec string) error {
	if spec == "" || spec == connectSpecNone {
		_, err := fmt.Fprintln(w,
			"Tip: run 'maz connect all' to register Mazarbul with detected AI clients,\n"+
				"     then 'maz preset install agent-memory' to teach the agent when to use it.")
		if err != nil {
			return fmt.Errorf("write tip: %w", err)
		}
		return nil
	}
	svc := newConnectService(dataDir)
	var targets []string
	if spec == connectSpecAll {
		for _, c := range svc.Clients() {
			targets = append(targets, c.ID())
		}
	} else {
		for _, t := range strings.Split(spec, ",") {
			if t = strings.TrimSpace(t); t != "" {
				targets = append(targets, t)
			}
		}
	}
	for _, id := range targets {
		rep, applyErr := svc.Apply(ctx, id, connect.ScopeUser, "", connect.ApplyOptions{})
		if _, err := fmt.Fprintln(w, formatConnectReport(&rep, applyErr)); err != nil {
			return fmt.Errorf("write connect report: %w", err)
		}
	}
	return nil
}

// presentConnectClients returns the connect detection results for clients
// that actually exist on disk (Status != NotInstalled) — the candidates
// worth offering in the interactive init multi-select. Best-effort:
// detection errors fold in as not-present.
func presentConnectClients(ctx context.Context, dataDir string) []connect.DetectionResult {
	var present []connect.DetectionResult
	for _, r := range newConnectService(dataDir).DetectAll(ctx) {
		if r.Err == nil && r.Status != connect.StatusNotInstalled {
			present = append(present, r)
		}
	}
	return present
}

// connectMultiSelectGroup builds the init form's client picker from the
// present clients, pre-checking those already registered (or stale).
// Selected is the bound slice huh populates with the chosen client IDs.
func connectMultiSelectGroup(present []connect.DetectionResult, selected *[]string) *huh.Group {
	opts := make([]huh.Option[string], 0, len(present))
	for _, r := range present {
		label := r.Client.DisplayName()
		registered := r.Status == connect.StatusRegistered || r.Status == connect.StatusRegisteredStale
		switch r.Status {
		case connect.StatusRegistered:
			label += " (already connected)"
		case connect.StatusRegisteredStale:
			label += " (update)"
		case connect.StatusUnknown, connect.StatusNotInstalled, connect.StatusInstalledNotRegistered:
			// installed-not-registered: offer it unchecked; the other two
			// don't reach here (presentConnectClients filters them out).
		}
		opts = append(opts, huh.NewOption(label, r.Client.ID()).Selected(registered))
	}
	return huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Connect AI clients").
			Description("Register Mazarbul as an MCP server with these installed clients (space toggles).").
			Options(opts...).
			Value(selected),
	)
}

// selectInitConnectClients runs the interactive client picker for the form
// path. It detects installed clients against the FINAL data dir (so the
// "already connected"/"update" labels reflect the dir registrations are
// written for, even when the user edited the data dir in the main form),
// and when any are present runs a one-group multi-select, returning the
// chosen client IDs. Returns nil when connect was not offered or no clients
// are present; a user abort means "selected nothing", not an error.
func selectInitConnectClients(ctx context.Context, dataDir string, offer bool) ([]string, error) {
	if !offer {
		return nil, nil
	}
	present := presentConnectClients(ctx, dataDir)
	if len(present) == 0 {
		return nil, nil
	}
	selected := []string{}
	form := uiform.New(connectMultiSelectGroup(present, &selected))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, nil
		}
		return nil, fmt.Errorf("init connect form: %w", err)
	}
	return selected, nil
}

// applyInitConnectSelection registers each selected client and prints a
// report row. Per-row errors are surfaced in the row and non-fatal so the
// init flow still finishes.
func applyInitConnectSelection(ctx context.Context, w io.Writer, dataDir string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	svc := newConnectService(dataDir)
	for _, id := range ids {
		rep, applyErr := svc.Apply(ctx, id, connect.ScopeUser, "", connect.ApplyOptions{})
		if _, err := fmt.Fprintln(w, formatConnectReport(&rep, applyErr)); err != nil {
			return fmt.Errorf("write connect report: %w", err)
		}
	}
	return nil
}

// formDaemonMode resolves the daemon mode for the interactive form: an
// explicit --daemon wins; otherwise the confirm toggle maps to auto/off.
func formDaemonMode(daemonFlag string, startDaemon bool) string {
	if daemonFlag != "" {
		return daemonFlag
	}
	if startDaemon {
		return daemonModeAuto
	}
	return daemonModeOff
}

// writeInitSummary prints the post-init summary. With yes=true it emits the
// full TUI-style block (data dir, chat, embeddings, ready); without it just
// the one-line confirmation. Extracted from runInitCLI to keep the outer
// function under the project's gocognit budget.
func writeInitSummary(w io.Writer, dataDir string, flags *initFlags, yes bool) error {
	if !yes {
		_, err := fmt.Fprintf(w, "Mazarbul initialized at %s\n", dataDir)
		return err
	}
	chatModel := flags.chatModel
	if chatModel == "" {
		chatModel = defaultChatModel(flags.provider)
	}
	if err := printStepSummary(w, "data directory", dataDir); err != nil {
		return err
	}
	if err := printStepSummary(w, "chat", chatSummary(flags.provider, chatModel)); err != nil {
		return err
	}
	// The final "Ready." line is intentionally NOT printed here: the caller
	// prints it only after the daemon step runs, so a daemon-start failure
	// (a warning line) is never preceded by a contradictory "Ready."
	return printStepSummary(w, "embeddings", embeddingsSummary(flags.embedProvider, flags.embedModel))
}

// prewarmStorage opens the dispatcher against dataDir and immediately closes
// it. The Open call runs every SQLite migration, loads the sqlite-vec
// extension, and initialises the LadybugDB graph file. Doing this at init
// time means the first `maz remember` does not pay the migration cost AND
// failures (disk permissions, missing extension, corrupted graph file)
// surface at the moment the user is most able to fix them. The dispatcher
// is configured with a nil Embedder because no events are written here:
// this is a schema warmup, not a write.
func prewarmStorage(ctx context.Context, dataDir string) error {
	// If a daemon is already serving this data dir it owns the single-opener
	// LadybugDB lock and has already run every migration + warmed the schema.
	// Opening the dispatcher in-process here would only contend for that lock
	// and fail with the opaque "status 1" — so reuse the running daemon instead
	// of racing it. This is the common case when a user re-runs `maz init` on a
	// machine whose daemon is already up (the exact failure the user reported).
	if probeDaemonListening(dataDir) {
		return nil
	}
	d, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		// LadybugDB reports a held lock as the opaque "failed to open database
		// with status 1". Name the actual holder + fix instead of leaking the C
		// status (and instead of blindly suggesting `maz daemon stop`, which
		// cannot release a lock a `maz mcp serve` holds).
		if isDataDirLocked(err) {
			// If config.toml already exists the dir is already initialized, so the
			// user almost never needs to re-run init — say so plainly on top of the
			// lock-holder hint, since "re-init my working dir" is the common way to
			// hit this.
			if _, statErr := os.Stat(config.UserConfigPath(dataDir)); statErr == nil {
				return fmt.Errorf("init: this data dir is already initialized and currently in use, so there is nothing to do — %s. (To change settings without re-running init, use `maz config set <key> <value>`): %w",
					dataDirLockedHint(dataDir), err)
			}
			return fmt.Errorf("init: %s: %w", dataDirLockedHint(dataDir), err)
		}
		return fmt.Errorf("init: prewarm storage: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("init: close prewarm dispatcher: %w", err)
	}
	return nil
}

// ensureDataDir creates the data directory with mode 0700 if it does not
// already exist. Idempotent.
func ensureDataDir(dataDir string) error {
	if err := os.MkdirAll(dataDir, dataDirMode); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, dataDirMode); err != nil {
		return fmt.Errorf("chmod data dir: %w", err)
	}
	// Write the one-time orientation README so a human opening the data dir
	// knows the vault is the hand-edit surface and everything else is
	// machine-managed. Non-fatal: an orientation aid must not fail init.
	if err := config.EnsureDataDirReadme(dataDir); err != nil {
		return fmt.Errorf("write data dir README: %w", err)
	}
	return nil
}

// persistAPIKeyIfProvided stores --api-key in the keychain when supplied.
// No-op when the flag is empty.
func persistAPIKeyIfProvided(ctx context.Context, flags *initFlags) error {
	if flags.apiKey == "" {
		return nil
	}
	if flags.provider != chatProviderAnthropic {
		return errors.New("--api-key is only meaningful for the Anthropic chat provider")
	}
	chain := secret.PersistentChain()
	if err := chain.Set(ctx, secret.NameAnthropicAPIKey, flags.apiKey); err != nil {
		return fmt.Errorf("store anthropic api key: %w", err)
	}
	return nil
}

// anthropicKeyAvailable returns true when the canonical Anthropic key is
// already present in env or the keychain. Used to decide whether to warn
// at the end of CLI init.
func anthropicKeyAvailable() bool {
	chain := secret.DefaultChain()
	v, err := chain.Get(context.Background(), secret.NameAnthropicAPIKey)
	return err == nil && v != ""
}

// anthropicKeyAvailableFn is the seam for tests. Override to avoid keychain
// dependency; production code must not reassign this.
var anthropicKeyAvailableFn = anthropicKeyAvailable //nolint:gochecknoglobals

// probeOllamaModels performs a best-effort GET /api/tags against
// localhost:11434 with a 1s timeout. Returns an empty slice (not an
// error) when Ollama is unreachable so callers degrade gracefully.
func probeOllamaModels(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollama.DefaultBaseURL+"/api/tags", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort: probe response is read-only
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	names := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		names = append(names, m.Name)
	}
	return names
}

// runInitForm runs the interactive huh form for `maz init`. It builds a
// single uiform form (no tea.NewProgram). Groups are
// conditionally shown via WithHideFunc based on pointer values.
//
// DaemonFlag is the raw --daemon value: "" means the form shows a confirm
// (default yes); an explicit auto|once|off skips the confirm. OfferConnect
// is true when the user did NOT pass --connect, so the form presents the
// detection-driven client multi-select (the explicit-flag case is handled
// by the caller via runInitConnectStep). PresetSpec is the raw --preset value:
// "" makes the form offer the steering preset via a confirm; an explicit name
// installs it without asking; "none" skips it.
//
//nolint:gocognit,gocyclo // linear form flow; splitting into helpers just scatters the binding logic.
func runInitForm(cmd *cobra.Command, initialDir, daemonFlag string, offerConnect bool, presetSpec string) error {
	ctx := cmd.Context()
	stdout := cmd.OutOrStdout()
	// --- Pre-form probing (all best-effort, no blocking I/O after) ---
	installedModels := probeOllamaModels(ctx)

	haveKey := anthropicKeyAvailableFn()

	// Best-effort: surface the gh-authenticated GitHub login so the sync step
	// can offer a private remote repo. Empty when gh is absent/unauthenticated.
	ghLogin := detectGitHubLoginFn(ctx)

	// Client detection is deferred until AFTER the form resolves the data
	// directory (the user may edit it here): a picker built against
	// initialDir would label clients by the wrong data dir's registration
	// state while the apply writes entries for the resolved dir. See the
	// post-form connect step below.

	// Pointers that huh binds to. Must be allocated on the heap so the
	// value survives across the form groups (Elm-style value copies).
	dataDirVal := initialDir
	// The chat provider always defaults to none, whatever the environment
	// offers: an Anthropic key in env or a running Ollama is not a reason to
	// run a second model next to the calling agent, which already reasons.
	chatProviderVal := chatProviderNone
	apiKeyVal := ""
	anthropicModelVal := defaultChatModel(chatProviderAnthropic)
	ollamaModelTextVal := ""
	ollamaModelSelectVal := ""
	embedProviderVal := embedProviderOllama
	startDaemonVal := true
	installPresetVal := true
	// mnemonic is generated once and shown in the final note group.
	mnemonic, mnemonicErr := watchword.Generate()
	if mnemonicErr != nil {
		// Non-fatal: show empty string; user can run `maz secret set` later.
		mnemonic = "(generation failed; run `maz secret set mazarbul:watchword` later)"
	}

	// --- Group 1: data directory ---
	grp1 := huh.NewGroup(
		huh.NewInput().
			Title("Data directory").
			Description("Where Mazarbul stores its event log and indexes.").
			Placeholder(initialDir).
			Value(&dataDirVal).
			Validate(func(s string) error {
				if s == "" {
					return nil // empty keeps the placeholder default
				}
				if !filepath.IsAbs(s) && !strings.HasPrefix(s, "~") {
					return errors.New("path must be absolute (or start with ~)")
				}
				if s == "/tmp" || strings.HasPrefix(s, "/tmp/") ||
					s == "/private/tmp" || strings.HasPrefix(s, "/private/tmp/") {
					return errors.New("/tmp is wiped on reboot; choose a stable path")
				}
				for _, root := range []string{"/", "/usr", "/etc", "/var", "/home", "/Users"} {
					if s == root {
						return fmt.Errorf("cannot use system root %s as data dir", root)
					}
				}
				return nil
			}),
	)

	// --- Group 2: chat provider ---
	grp2 := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Server-side chat model").
			Description("Your agent (Claude Code, Cursor, ...) is the brain; Mazarbul is the memory. A server-side model is optional, only for ingestion without an agent and `maz ask` from a terminal.").
			Options(
				huh.NewOption("None, my agent does the reasoning (recommended)", chatProviderNone),
				huh.NewOption("Anthropic (optional: agent-less ingestion, maz ask)", chatProviderAnthropic),
				huh.NewOption("Ollama, local (optional: agent-less ingestion, maz ask)", chatProviderOllama),
			).
			Value(&chatProviderVal),
	)

	// --- Group 3: Anthropic API key (conditional) ---
	// Hidden when not using Anthropic OR when the key is already present.
	grp3 := huh.NewGroup(
		huh.NewInput().
			Title("Anthropic API key").
			Description("Stored in your OS keychain. Never logged, never printed.").
			EchoMode(huh.EchoModePassword).
			Value(&apiKeyVal).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("api key is required")
				}
				return nil
			}),
	).WithHideFunc(func() bool {
		return chatProviderVal != chatProviderAnthropic || haveKey
	})

	// --- Group 4: chat model ---
	// Anthropic model select (hidden when Ollama chosen).
	grp4Anthropic := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Anthropic chat model").
			Options(
				huh.NewOption("claude-opus-4-7 (most capable)", "claude-opus-4-7"),
				huh.NewOption("claude-sonnet-4-6 (balanced)", "claude-sonnet-4-6"),
				huh.NewOption("claude-haiku-4-5 (fast)", "claude-haiku-4-5"),
			).
			Value(&anthropicModelVal),
	).WithHideFunc(func() bool {
		return chatProviderVal != chatProviderAnthropic
	})

	// Ollama model group: select from installed if available, otherwise text input.
	var grp4Ollama *huh.Group
	if len(installedModels) > 0 {
		opts := make([]huh.Option[string], 0, len(installedModels))
		for _, m := range installedModels {
			opts = append(opts, huh.NewOption(m, m))
		}
		if ollamaModelSelectVal == "" && len(installedModels) > 0 {
			ollamaModelSelectVal = installedModels[0]
		}
		grp4Ollama = huh.NewGroup(
			huh.NewSelect[string]().
				Title("Ollama chat model").
				Description("Select an installed model to use for chat.").
				Options(opts...).
				Value(&ollamaModelSelectVal),
		).WithHideFunc(func() bool {
			return chatProviderVal != chatProviderOllama
		})
	} else {
		grp4Ollama = huh.NewGroup(
			huh.NewInput().
				Title("Ollama chat model").
				Description("Ollama is not reachable or has no models. Enter a model name (e.g. qwen2.5:7b).").
				Placeholder("qwen2.5:7b").
				Value(&ollamaModelTextVal).
				Validate(func(s string) error {
					if chatProviderVal == chatProviderOllama && strings.TrimSpace(s) == "" {
						return errors.New("model name is required for Ollama")
					}
					return nil
				}),
		).WithHideFunc(func() bool {
			return chatProviderVal != chatProviderOllama
		})
	}

	// --- Group 5: embed provider ---
	grp5 := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Embedding provider").
			Description("Embeddings enable semantic recall. Local-first via Ollama.").
			Options(
				huh.NewOption("Ollama (recommended, local)", embedProviderOllama),
				huh.NewOption("None (recall disabled)", embedProviderNone),
			).
			Value(&embedProviderVal),
	)

	// --- Group 5b: sync remote (only offered when gh surfaces a login) ---
	// Default to the local mirror; a remote is an explicit opt-in. Repo name is
	// pre-filled and only validated once the GitHub target is chosen.
	syncTargetVal := syncTargetLocal
	repoNameVal := defaultPersonalRepoName
	var grpSyncSelect, grpSyncRepo *huh.Group
	if ghLogin != "" {
		grpSyncSelect = huh.NewGroup(
			huh.NewSelect[string]().
				Title("Back up your memory to a remote?").
				Description(fmt.Sprintf("Signed in to GitHub as %s. A remote syncs your memory across machines; local keeps history on this machine only.", ghLogin)).
				Options(
					huh.NewOption("Local only (default)", syncTargetLocal),
					huh.NewOption("Create a private GitHub repo under "+ghLogin, syncTargetGitHub),
				).
				Value(&syncTargetVal),
		)
		grpSyncRepo = huh.NewGroup(
			huh.NewInput().
				Title("GitHub repository name").
				Description("A new private repo for your 'personal' scope.").
				Placeholder(defaultPersonalRepoName).
				Value(&repoNameVal).
				Validate(validateRepoName),
		).WithHideFunc(func() bool {
			return syncTargetVal != syncTargetGitHub
		})
	}

	// --- Group 5c: encryption (always offered; OFF by default for convenience) ---
	// Plaintext keeps the synced git history human-readable (diff/blame/log).
	// Encryption seals chunks with the Watchword — only then does the recovery
	// phrase below matter, so the note that shows it is hidden unless this is on.
	encryptVal := false
	grpEncrypt := huh.NewGroup(
		huh.NewConfirm().
			Title("Encrypt your memory?").
			Description("Off (default): synced chunks stay plaintext so git diff/blame/history work. On: chunks are sealed with your 24-word Watchword — you must keep the recovery phrase safe.").
			Value(&encryptVal),
	)

	// --- Group 6: watchword recovery phrase (shown only when encrypting) ---
	// The Watchword is always generated (it derives the signing identity), but
	// the 24-word phrase only needs safeguarding when it also seals chunks, so
	// the note stays hidden unless encryption is enabled.
	noteBody := formatWatchwordNote(mnemonic)
	grp6 := huh.NewGroup(
		huh.NewNote().
			Title("Watchword recovery phrase").
			Description("Encryption is on. Store these 24 words securely — they are the ONLY way to recover your encrypted scopes (and your signing identity).\n\n" + noteBody),
	).WithHideFunc(func() bool {
		return !encryptVal
	})

	groups := []*huh.Group{grp1, grp2, grp3, grp4Anthropic, grp4Ollama, grp5}
	if grpSyncSelect != nil {
		groups = append(groups, grpSyncSelect, grpSyncRepo)
	}
	groups = append(groups, grpEncrypt, grp6)

	// --- Group 7: start the daemon now? (skipped when --daemon is explicit) ---
	if daemonFlag == "" {
		groups = append(groups, huh.NewGroup(
			huh.NewConfirm().
				Title("Start Mazarbul in the background now?").
				Description("Keeps the daemon running so the CLI and connected agents work immediately, and relaunches it on every login (macOS).").
				Value(&startDaemonVal),
		))
	}

	// --- Group 8: install the steering preset? (skipped when --preset is explicit) ---
	if presetSpec == "" {
		groups = append(groups, huh.NewGroup(
			huh.NewConfirm().
				Title("Teach your agent when to use Mazarbul?").
				Description("Installs the agent-memory preset: a short playbook steering the agent to warm up with getContext, recall before answering, remember what it learns, and write entities and relations itself.").
				Value(&installPresetVal),
		))
	}

	// The client-connect multi-select is NOT a group here: it runs as a
	// second form after the data directory is resolved (see below), so its
	// detection labels reflect the dir registrations are written for.

	form := uiform.New(groups...)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			//nolint:errcheck // best-effort stdout write on cancel
			_, _ = fmt.Fprintln(stdout, "init cancelled; rerun anytime with `maz init`")
			return nil
		}
		return fmt.Errorf("init form: %w", err)
	}

	// Resolve final values from the form bindings.
	resolvedDir := resolveFormDataDir(dataDirVal, initialDir)
	resolvedProvider := chatProviderVal
	resolvedChatModel := resolveInitChatModel(
		resolvedProvider, anthropicModelVal, ollamaModelSelectVal, ollamaModelTextVal,
		installedModels,
	)
	resolvedEmbed := embedProviderVal
	resolvedAPIKey := apiKeyVal

	// Persist the API key when provided via the form.
	if resolvedProvider == chatProviderAnthropic && resolvedAPIKey != "" {
		chain := secret.PersistentChain()
		if err := chain.Set(ctx, secret.NameAnthropicAPIKey, resolvedAPIKey); err != nil {
			//nolint:errcheck // best-effort warning write; non-fatal
			_, _ = fmt.Fprintf(stdout, "warning: store api key: %v\n", err)
		}
	}

	// Persist the watchword mnemonic.
	if mnemonicErr == nil && mnemonic != "" {
		chain := secret.PersistentChain()
		if err := chain.Set(ctx, watchword.KeychainKey, mnemonic); err != nil {
			//nolint:errcheck // best-effort warning write; non-fatal
			_, _ = fmt.Fprintf(stdout, "warning: store watchword: %v\n", err)
		} else if _, err := identity.New(chain).Initialize(ctx); err != nil {
			// Derive + cache the identity keypairs now that the Watchword exists,
			// so a mellon-paired device that skips the daemon still has keys. The
			// daemon remains authoritative for the genesis principalRegistered /
			// deviceEnrolled events. Non-fatal: the daemon re-derives on boot.
			//nolint:errcheck // best-effort warning write; non-fatal
			_, _ = fmt.Fprintf(stdout, "warning: initialize identity keys: %v\n", err)
		}
	}

	// Build and write config.
	formFlags := &initFlags{
		provider:      resolvedProvider,
		chatModel:     resolvedChatModel,
		embedProvider: resolvedEmbed,
		embedModel:    ollama.DefaultEmbedModel,
	}
	if err := ensureDataDir(resolvedDir); err != nil {
		return err
	}
	cfg := userConfigFromFlags(formFlags)
	if err := applyInitSyncRemote(ctx, stdout, resolvedDir, &cfg, syncTargetVal, ghLogin, repoNameVal, encryptVal); err != nil {
		return fmt.Errorf("configure sync remote: %w", err)
	}
	if err := config.WriteUserConfig(resolvedDir, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := prewarmStorage(ctx, resolvedDir); err != nil {
		return err
	}

	// Offer the client multi-select now that the data dir is final, so its
	// detection labels match the dir the registrations are written for.
	// Runs as a second form back-to-back with the main one (huh shows a
	// group per screen, so this is visually seamless).
	selectedClients, err := selectInitConnectClients(ctx, resolvedDir, offerConnect)
	if err != nil {
		return err
	}

	// Print summary.
	if err := printStepSummary(stdout, "data directory", resolvedDir); err != nil {
		return err
	}
	if err := printStepSummary(stdout, "chat", chatSummary(resolvedProvider, resolvedChatModel)); err != nil {
		return err
	}
	if err := printStepSummary(stdout, "embeddings", embeddingsSummary(resolvedEmbed, ollama.DefaultEmbedModel)); err != nil {
		return err
	}
	if err := printStepSummary(stdout, "sync", syncSummaryLine(&cfg)); err != nil {
		return err
	}

	// Install the steering preset while the storage lock is still free (the
	// daemon below takes it). Resolve the form's confirm: an explicit --preset
	// name wins; otherwise the confirm toggles the default agent-memory preset.
	presetToInstall := presetSpec
	if presetSpec == "" {
		presetToInstall = initPresetNone
		if installPresetVal {
			presetToInstall = schemapack.DefaultSteeringPreset
		}
	}
	if err := applyInitPresetStep(ctx, cmd, stdout, resolvedDir, presetToInstall); err != nil {
		return err
	}

	// Start the daemon (prewarm above already released the storage lock),
	// then register the selected AI clients. Both are best-effort: a
	// failure prints a warning but does not abort the completed init.
	if err := applyInitDaemonStep(ctx, stdout, resolvedDir, formDaemonMode(daemonFlag, startDaemonVal)); err != nil {
		return err
	}
	if err := applyInitConnectSelection(ctx, stdout, resolvedDir, selectedClients); err != nil {
		return err
	}
	return printReady(stdout)
}

// resolveFormDataDir expands an empty or tilde-prefixed data dir to its
// canonical absolute path, falling back to initialDir when empty.
func resolveFormDataDir(input, fallback string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return fallback
	}
	if trimmed == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return fallback
	}
	if strings.HasPrefix(trimmed, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, trimmed[2:])
		}
	}
	return filepath.Clean(trimmed)
}

// resolveInitChatModel picks the final chat model from the form values,
// preferring the provider-appropriate binding.
func resolveInitChatModel(provider, anthropicModel, ollamaSelect, ollamaText string, installed []string) string {
	switch provider {
	case chatProviderAnthropic:
		if anthropicModel != "" {
			return anthropicModel
		}
		return defaultChatModel(chatProviderAnthropic)
	case chatProviderOllama:
		if len(installed) > 0 && ollamaSelect != "" {
			return ollamaSelect
		}
		if ollamaText != "" {
			return ollamaText
		}
	}
	return ""
}

// formatWatchwordNote lays the 24 words out in a 4-row × 6-col grid
// for display inside the huh.Note field.
func formatWatchwordNote(mnemonic string) string {
	const wordsPerLine = 6
	words := strings.Fields(mnemonic)
	var b strings.Builder
	for i, w := range words {
		if i%wordsPerLine == 0 && i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(fmt.Sprintf("%2d. %-10s", i+1, w))
	}
	return b.String()
}

// printStepSummary writes a single inline summary line to w in the
// EntireIO style: `| <label>: <value>`. The bar uses the active theme's
// accent; the label is faint, the value is plain ink so the user's choice
// stands out without colour spam.
func printStepSummary(w io.Writer, label, value string) error {
	bar := lipgloss.NewStyle().Foreground(style.Accent()).Render("│")
	lbl := lipgloss.NewStyle().Foreground(style.SubtleColor()).Render(label + ":")
	val := lipgloss.NewStyle().Foreground(style.Ink()).Render(value)
	_, err := fmt.Fprintf(w, "%s %s %s\n", bar, lbl, val)
	return err
}

// printReady writes the EntireIO-style finale to w: a single accent-coloured
// vertical bar followed by `Ready.` and the next-step hint. No panel,
// no spinner, no extra blank lines — the goal is a clean, terse wrap
// that doesn't dominate the scrollback above it.
func printReady(w io.Writer) error {
	bar := lipgloss.NewStyle().Foreground(style.Accent()).Render("│")
	ready := lipgloss.NewStyle().Foreground(style.Ink()).Bold(true).Render("Ready.")
	hint := lipgloss.NewStyle().Foreground(style.FaintColor()).Render("next: maz recall <token>")
	_, err := fmt.Fprintf(w, "%s %s  %s\n", bar, ready, hint)
	return err
}
