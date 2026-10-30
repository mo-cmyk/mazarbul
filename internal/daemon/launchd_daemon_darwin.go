//go:build darwin

// This file carries the launchd integration for the Mazarbul daemon
// itself, so `maz daemon install` (and the turnkey `maz init`) can hand
// the daemon's lifecycle to launchd: it starts now AND relaunches on
// every login. It reuses the shared launchctl helpers in
// launchd_darwin.go (runLaunchctl, launchdUserDomain, bootstrapOrReload,
// escapeXML, alreadyLoaded/notLoaded) — only the plist shape and the
// label differ from the Ollama agent.

package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// One-shot handoff grace windows. When `maz daemon install` (or the
// turnkey `maz init --daemon=auto`) takes over from a `maz daemon start`
// one-shot, that one-shot must fully exit and free the global PID file
// before launchd's RunAtLoad child tries to claim it. The soft grace
// exceeds mcp.DefaultShutdownTimeout (10 s) so a clean drain finishes
// before SIGKILL; the kill grace is a 1 s pad for the uncatchable signal.
const (
	oneShotHandoffSoftGrace = 12 * time.Second
	oneShotHandoffKillGrace = 1 * time.Second
)

// daemonPlistTemplate is the property list launchd consumes for the
// daemon. ProgramArguments mirrors buildDaemonChildArgs exactly
// (`--data-dir <dir> internal-daemon-serve --token <token>`) so a
// launchd-spawned daemon is byte-for-byte the process `maz daemon start`
// would have forked. RunAtLoad + KeepAlive(SuccessfulExit=false) give
// start-now plus crash/login persistence; HOME and PATH are declared
// because launchd gui agents otherwise inherit a near-empty env.
const daemonPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.ExecutablePath}}</string>
        <string>--data-dir</string>
        <string>{{.DataDir}}</string>
        <string>internal-daemon-serve</string>
        <string>--token</string>
        <string>{{.Token}}</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
{{- if .Home}}
        <key>HOME</key><string>{{.Home}}</string>
{{- end}}
{{- if .Path}}
        <key>PATH</key><string>{{.Path}}</string>
{{- end}}
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
</dict>
</plist>
`

// LaunchAgentPlistPath returns the canonical install location of the
// daemon's user-level launch agent plist. Errors when $HOME cannot be
// resolved (rare; containers and recovery mode).
func LaunchAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve $HOME: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", LaunchAgentLabel+".plist"), nil
}

// RenderLaunchAgentPlist returns the daemon plist XML bytes for opts.
// Pure function: no I/O, no exec — exists so tests can assert plist
// shape (argv, token, paths) without touching the filesystem. The
// dynamic fields are XML-escaped because a data dir or log path with &,
// <, >, or " would otherwise produce malformed XML launchctl silently
// rejects.
func RenderLaunchAgentPlist(opts *LaunchAgentOptions) ([]byte, error) {
	if opts.ExecutablePath == "" {
		return nil, errors.New("daemon: launch agent ExecutablePath is required")
	}
	if opts.DataDir == "" {
		return nil, errors.New("daemon: launch agent DataDir is required")
	}
	if opts.Token == "" {
		return nil, errors.New("daemon: launch agent Token is required")
	}
	if opts.LogPath == "" {
		return nil, errors.New("daemon: launch agent LogPath is required")
	}
	tpl, err := template.New("daemon-plist").Parse(daemonPlistTemplate)
	if err != nil {
		return nil, fmt.Errorf("daemon: parse plist template: %w", err)
	}
	exeEsc, err := escapeXML(opts.ExecutablePath)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape ExecutablePath: %w", err)
	}
	dirEsc, err := escapeXML(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape DataDir: %w", err)
	}
	tokEsc, err := escapeXML(opts.Token)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape Token: %w", err)
	}
	logEsc, err := escapeXML(opts.LogPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape LogPath: %w", err)
	}
	homeEsc, err := escapeXML(opts.Home)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape Home: %w", err)
	}
	pathEsc, err := escapeXML(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape Path: %w", err)
	}
	var buf bytes.Buffer
	data := struct {
		Label          string
		ExecutablePath string
		DataDir        string
		Token          string
		LogPath        string
		Home           string
		Path           string
	}{
		Label:          LaunchAgentLabel, // package-internal constant, no escaping needed
		ExecutablePath: exeEsc,
		DataDir:        dirEsc,
		Token:          tokEsc,
		LogPath:        logEsc,
		Home:           homeEsc,
		Path:           pathEsc,
	}
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("daemon: render plist: %w", err)
	}
	return buf.Bytes(), nil
}

// defaultLaunchAgentOptions fills the empty fields of opts with their
// install-time defaults: the running maz binary, a fresh ownership
// token, <DataDir>/daemon-launchd.log, and the installing shell's HOME +
// PATH. Split out of InstallLaunchAgent to keep that function under the
// gocyclo budget. Assumes opts and opts.DataDir are already validated.
func defaultLaunchAgentOptions(opts *LaunchAgentOptions) error {
	if opts.ExecutablePath == "" {
		exe, err := ResolveExecutable()
		if err != nil {
			return err
		}
		opts.ExecutablePath = exe
	}
	if opts.Token == "" {
		tok, err := mintLaunchAgentToken()
		if err != nil {
			return err
		}
		opts.Token = tok
	}
	if opts.LogPath == "" {
		opts.LogPath = filepath.Join(opts.DataDir, "daemon-launchd.log")
	}
	if opts.Home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			opts.Home = home
		}
	}
	if opts.Path == "" {
		opts.Path = os.Getenv("PATH")
	}
	return nil
}

// InstallLaunchAgent writes the daemon plist to its canonical location
// and loads it via `launchctl bootstrap`, which (RunAtLoad=true) also
// starts the daemon immediately. Empty option fields are defaulted so
// the common call site only needs DataDir.
//
// Idempotent: a plist already at the destination is overwritten and the
// stale job is booted out + reloaded so the fresh definition takes
// effect (bootstrapOrReload). The minted token is fresh on every
// install; an already-running launchd daemon is replaced by the reload.
func InstallLaunchAgent(ctx context.Context, opts *LaunchAgentOptions) error {
	if opts == nil {
		return errors.New("daemon: launch agent opts required")
	}
	if opts.DataDir == "" {
		return errors.New("daemon: dataDir required")
	}
	if err := defaultLaunchAgentOptions(opts); err != nil {
		return err
	}
	body, err := RenderLaunchAgentPlist(opts)
	if err != nil {
		return err
	}
	plistPath, err := LaunchAgentPlistPath()
	if err != nil {
		return err
	}
	// 0700 / 0600: the token rides in the plist argv, so the file must
	// not be world-readable. Tighter than launchd requires and keeps
	// gosec quiet without affecting launchctl's ability to load.
	if mkerr := os.MkdirAll(filepath.Dir(plistPath), 0o700); mkerr != nil {
		return fmt.Errorf("daemon: create LaunchAgents dir: %w", mkerr)
	}
	if werr := os.WriteFile(plistPath, body, 0o600); werr != nil {
		return fmt.Errorf("daemon: write plist %s: %w", plistPath, werr)
	}
	domain, derr := launchdUserDomain()
	if derr != nil {
		return derr
	}
	// Hand off cleanly from any one-shot daemon (`maz daemon start`) that
	// holds the global PID file before launchd's RunAtLoad child tries to
	// claim it — otherwise the child's WritePID returns ErrAlreadyRunning,
	// it exits non-zero, and KeepAlive(SuccessfulExit=false) crash-loops it.
	stopConflictingOneShot(ctx)
	return bootstrapOrReload(ctx, domain, LaunchAgentLabel, plistPath)
}

// stopConflictingOneShot terminates a non-launchd ("one-shot") daemon that
// currently holds the global PID file, so the about-to-load launchd child
// can claim it. Best-effort: a PID file that is absent, stale, or owned by
// an unrelated process is left untouched.
//
// Gated on our agent NOT being loaded: when it is loaded, any live daemon
// on the PID file is the launchd-managed one and bootstrapOrReload's bootout
// replaces it — pre-killing would race that managed lifecycle. A one-shot
// can only coexist while our agent is unloaded (its own WritePID would
// otherwise have returned ErrAlreadyRunning against the managed daemon).
func stopConflictingOneShot(ctx context.Context) {
	if loaded, err := LaunchAgentLoaded(ctx); err != nil || loaded {
		return
	}
	pidPath := DefaultPath()
	info, err := ReadPID(pidPath)
	if err != nil || !info.Running || !IsMazDaemon(info.PID, info.Token) {
		return
	}
	// Wait for the one-shot to actually exit (and free the PID file) before
	// launchd loads the replacement; ignore the signal/result — the only
	// downside of a failure is a brief crash-loop until it exits on its own.
	_, _ = TerminateGracefully(info.PID, oneShotHandoffSoftGrace, oneShotHandoffKillGrace) //nolint:errcheck // best-effort handoff
	_ = DeletePID(pidPath)                                                                 //nolint:errcheck // best-effort; the one-shot's own defer usually removes it
}

// UninstallLaunchAgent runs `launchctl bootout` to stop the daemon and
// removes the plist from disk. Missing service / missing plist are not
// errors (re-running uninstall is a no-op). This is the correct way to
// stop a launchd-managed daemon: a bare SIGTERM would be undone by
// KeepAlive, which relaunches the process the signal just killed.
func UninstallLaunchAgent(ctx context.Context) error {
	plistPath, err := LaunchAgentPlistPath()
	if err != nil {
		return err
	}
	domain, derr := launchdUserDomain()
	if derr != nil {
		return derr
	}
	if cerr := runLaunchctl(ctx, "bootout", domain+"/"+LaunchAgentLabel); cerr != nil {
		if !notLoaded(cerr) {
			return fmt.Errorf("daemon: launchctl bootout %s: %w", LaunchAgentLabel, cerr)
		}
	}
	if rerr := os.Remove(plistPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove plist %s: %w", plistPath, rerr)
	}
	return nil
}

// BootoutLaunchAgent stops a launchd-managed daemon by booting the job
// out of the user domain WITHOUT removing the plist, so it returns on the
// next login. This is the `maz daemon stop` path for a persistent daemon:
// a bare SIGTERM would be undone by KeepAlive, which relaunches the
// process the signal just killed. Idempotent — a not-loaded service is
// not an error. Use UninstallLaunchAgent to disable login persistence.
func BootoutLaunchAgent(ctx context.Context) error {
	domain, err := launchdUserDomain()
	if err != nil {
		return err
	}
	if cerr := runLaunchctl(ctx, "bootout", domain+"/"+LaunchAgentLabel); cerr != nil && !notLoaded(cerr) {
		return fmt.Errorf("daemon: launchctl bootout %s: %w", LaunchAgentLabel, cerr)
	}
	return nil
}

// LaunchAgentLoaded reports whether the daemon's launch agent is
// currently loaded into the user domain. Used by `maz daemon status` to
// surface launchd ownership and by `maz daemon stop` to decide between
// a bootout and the PID-file stop path.
func LaunchAgentLoaded(ctx context.Context) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, launchctlTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "launchctl", "list", LaunchAgentLabel)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// launchctl exits non-zero ("Could not find service") when the
		// label is not loaded. That's the negative answer, not a failure.
		return false, nil
	}
	return false, fmt.Errorf("daemon: launchctl list: %w", err)
}

// LaunchAgentJobPID returns the PID launchd reports for the daemon job,
// and whether the job is currently loaded. `launchctl list <label>` prints
// a plist-ish dict whose `"PID" = <n>;` line names the running child; that
// line is absent when the job is loaded but not currently running (e.g.
// between KeepAlive restarts), in which case pid is 0 and loaded is true.
// A not-loaded label returns (0, false, nil). Callers cross-check this PID
// against the process actually serving the HTTP port to prove the listener
// is the launchd child from this install — not some other maz daemon.
func LaunchAgentJobPID(ctx context.Context) (pid int, loaded bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, launchctlTimeout)
	defer cancel()

	out, runErr := exec.CommandContext(cctx, "launchctl", "list", LaunchAgentLabel).Output()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// Non-zero exit ("Could not find service") == not loaded.
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("daemon: launchctl list %s: %w", LaunchAgentLabel, runErr)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"PID"`) {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		digits := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[eq+1:]), ";"))
		parsed, perr := strconv.Atoi(digits)
		if perr != nil {
			return 0, true, nil
		}
		return parsed, true, nil
	}
	// Loaded but no PID line: job registered, child not currently running.
	return 0, true, nil
}

// mintLaunchAgentToken returns 16 bytes of crypto/rand hex-encoded: the
// per-install ownership secret baked into the plist argv. Mirrors the
// CLI's mintDaemonToken so the launchd path and the `daemon start` path
// produce identical token shapes.
func mintLaunchAgentToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("daemon: mint launch agent token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
