package daemon

import "strings"

// OllamaLaunchAgentLabel is the launchd "Label" used in the plist and
// in `launchctl list` lookups. Inverse-DNS per Apple convention; the
// "io.mazarbul" prefix matches the binary and gives a stable, unique
// identifier across user installs. Declared without a build tag so the
// constant is the single source of truth across Darwin and non-Darwin
// builds — a rename touches one line, not two.
const OllamaLaunchAgentLabel = "io.mazarbul.ollama"

// OllamaLaunchAgentOptions configures the plist content. Declared on
// every build so cobra commands can declare their flag-mapped value
// without a build tag. Field semantics:
//
//   - ExecutablePath: absolute path to the ollama binary. Required
//     on Darwin; Install resolves an empty value via
//     exec.LookPath("ollama"). Ignored on non-Darwin (all entrypoints
//     return ErrLaunchdMacOnly before reading the struct).
//   - LogPath: where launchd writes Ollama's combined stdout/stderr.
//     Required on Darwin; Install defaults to
//     <DataDir>/ollama-launchd.log. Ignored on non-Darwin.
//   - OllamaHost: value for OLLAMA_HOST inside the plist's
//     EnvironmentVariables. Empty omits the key; launchd then leaves
//     Ollama on its compiled-in default (127.0.0.1:11434). Install
//     captures this from the user's $OLLAMA_HOST at install time so
//     a non-default bind address survives the in-process → launchd
//     migration.
//   - OllamaModels: value for OLLAMA_MODELS. Empty omits the key.
//     Install captures this from the user's $OLLAMA_MODELS so models
//     they downloaded into a custom directory remain discoverable.
type OllamaLaunchAgentOptions struct {
	ExecutablePath string
	LogPath        string
	OllamaHost     string
	OllamaModels   string
	// NumParallel sets OLLAMA_NUM_PARALLEL in the launchd child's env,
	// mirroring config.ProviderTable.NumParallel for the supervisor-spawned
	// path. <= 0 renders the historical pinned default of 1. Raising it
	// lets chat and embed calls overlap on one runner — the chat-slot
	// ceiling in wiring_chat follows the same config value.
	NumParallel int
}

// LaunchAgentLabel is the launchd "Label" for the Mazarbul daemon itself
// (distinct from OllamaLaunchAgentLabel). Inverse-DNS per Apple
// convention; "io.mazarbul" matches the binary so a `launchctl list`
// lookup is unambiguous. Declared without a build tag so the constant is
// the single source of truth across Darwin and non-Darwin builds. The
// package's primary subject is the daemon, so its launch-agent symbols
// go unprefixed while the secondary Ollama agent keeps its prefix.
const LaunchAgentLabel = "io.mazarbul.daemon"

// LaunchAgentOptions configures the daemon plist content. Declared on
// every build so the cobra commands can construct it without a build
// tag. Field semantics (all defaulted by InstallLaunchAgent when empty,
// so the common call site only needs DataDir):
//
//   - ExecutablePath: absolute path to the maz binary launchd re-execs.
//     Empty resolves via ResolveExecutable() (os.Executable).
//   - DataDir: the data directory the daemon serves. Required; passed on
//     argv as `--data-dir <DataDir>` so the launchd child opens the same
//     kernel store the user named, matching buildDaemonChildArgs.
//   - Token: the per-install ownership secret carried on argv as
//     `--token <Token>` (mirrors mintDaemonToken). Empty mints a fresh
//     16-byte crypto/rand token at install time. Static across KeepAlive
//     restarts — still unforgeable by other processes, so the
//     `daemon stop`/`status` ownership probe (IsMazDaemon) keeps working.
//   - LogPath: where launchd writes the daemon's combined stdout/stderr.
//     Empty defaults to <DataDir>/daemon-launchd.log.
//   - Home: value for HOME inside the plist's EnvironmentVariables.
//     launchd gui agents get a minimal env; the daemon needs HOME to
//     reach the macOS keychain (API keys, watchword). Empty resolves via
//     os.UserHomeDir().
//   - Path: value for PATH. The in-daemon Ollama supervisor shells out to
//     the `ollama` binary, which needs a usable PATH. Empty captures the
//     installing shell's $PATH.
type LaunchAgentOptions struct {
	ExecutablePath string
	DataDir        string
	Token          string
	LogPath        string
	Home           string
	Path           string
}

// WatchFSLaunchAgentLabel is the launchd "Label" for the agent-agnostic
// filesystem watcher (maz watch-fs), distinct from the daemon and Ollama
// agents. Inverse-DNS per Apple convention; declared without a build tag
// so the constant is the single source of truth across platforms.
const WatchFSLaunchAgentLabel = "io.mazarbul.watch-fs"

// WatchFSLaunchAgentOptions configures the watch-fs plist content.
// Declared on every build so the cobra commands can construct it without
// a build tag. All fields except Roots are defaulted by
// InstallWatchFSLaunchAgent when empty:
//
//   - ExecutablePath: absolute path to the maz binary launchd re-execs.
//     Empty resolves via ResolveExecutable() (os.Executable).
//   - DataDir: the data directory captured episodes land in. Required;
//     passed on argv as `--data-dir <DataDir>` so the watcher writes to
//     (or routes to a daemon serving) the same store the user named.
//   - Roots: the directory trees the watcher observes. Required and
//     non-empty; passed as trailing positional args to `watch-fs
//     --internal`.
//   - LogPath: where launchd writes the watcher's combined
//     stdout/stderr. Empty defaults to <DataDir>/watch-fs-launchd.log.
//   - Home / Path: HOME and PATH for the launchd child, which otherwise
//     gets a minimal env. Empty resolves via os.UserHomeDir() / $PATH.
type WatchFSLaunchAgentOptions struct {
	ExecutablePath string
	DataDir        string
	Roots          []string
	LogPath        string
	Home           string
	Path           string
}

// LaunchAgentState is the launchd view of the daemon's launch agent, as
// read by ReadLaunchAgentState. Declared on every build so `maz doctor`
// and `maz daemon status` can consume it without a build tag; on
// non-macOS builds it is always the zero value (nothing installed).
//
//   - Installed: the plist exists at LaunchAgentPlistPath.
//   - Loaded: launchd has the label registered in the user domain.
//   - PID: the running child's PID, 0 when no child is running.
//   - State: launchd's top-level "state" line ("running", "spawn
//     scheduled", "not running"), empty when not loaded.
//   - LastExitReason: launchd's "last exit reason" line, e.g.
//     "OS_REASON_CODESIGNING", empty when the job never exited.
type LaunchAgentState struct {
	Installed      bool
	Loaded         bool
	PID            int
	State          string
	LastExitReason string
}

// CodesignBlocked reports whether launchd refuses to spawn the daemon
// because the binary's code signature no longer matches the launch
// constraint launchd pinned when the agent was registered. This happens
// after the maz binary is replaced in place (`brew upgrade`, the install
// script, or `maz upgrade`): the job stays loaded in "spawn scheduled"
// with last exit reason OS_REASON_CODESIGNING, and neither kickstart nor
// KeepAlive recovers it. Re-registering (bootout, wait, bootstrap) does.
func (s LaunchAgentState) CodesignBlocked() bool {
	return s.Installed && s.Loaded && s.PID == 0 &&
		strings.Contains(s.LastExitReason, "CODESIGNING")
}
