//go:build darwin

// This file carries the launchd integration for the agent-agnostic
// filesystem watcher, so `maz watch-fs install` hands the watcher's
// lifecycle to launchd: it starts now AND relaunches on every login,
// giving hookless agents a persistent passive-capture process. It reuses
// the shared launchctl helpers in launchd_darwin.go (runLaunchctl,
// launchdUserDomain, bootstrapOrReload, escapeXML, notLoaded) — only the
// plist shape and the label differ from the daemon agent.

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// watchFSPlistTemplate is the property list launchd consumes for the
// watcher. ProgramArguments runs the hidden `watch-fs --internal` loop
// against the named data dir with each watched root as a trailing
// positional arg. RunAtLoad + KeepAlive(SuccessfulExit=false) give
// start-now plus crash/login persistence; HOME and PATH are declared
// because launchd gui agents otherwise inherit a near-empty env (the
// watcher routes writes through the daemon or opens the kernel store
// in-process, which needs HOME to reach the keychain).
const watchFSPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
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
        <string>watch-fs</string>
        <string>--internal</string>
{{- range .Roots}}
        <string>{{.}}</string>
{{- end}}
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

// WatchFSLaunchAgentPlistPath returns the canonical install location of
// the watcher's user-level launch agent plist. Errors when $HOME cannot
// be resolved (rare; containers and recovery mode).
func WatchFSLaunchAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve $HOME: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", WatchFSLaunchAgentLabel+".plist"), nil
}

// RenderWatchFSLaunchAgentPlist returns the watcher plist XML bytes for
// opts. Pure function: no I/O, no exec — exists so tests can assert plist
// shape (argv, roots, paths) without touching the filesystem. Every
// dynamic field is XML-escaped because a data dir, root, or log path with
// &, <, >, or " would otherwise produce malformed XML launchctl silently
// rejects.
func RenderWatchFSLaunchAgentPlist(opts *WatchFSLaunchAgentOptions) ([]byte, error) {
	if err := validateWatchFSPlistOptions(opts); err != nil {
		return nil, err
	}
	tpl, err := template.New("watchfs-plist").Parse(watchFSPlistTemplate)
	if err != nil {
		return nil, fmt.Errorf("daemon: parse plist template: %w", err)
	}
	data, err := escapeWatchFSPlistData(opts)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("daemon: render plist: %w", err)
	}
	return buf.Bytes(), nil
}

// validateWatchFSPlistOptions enforces the required plist inputs.
func validateWatchFSPlistOptions(opts *WatchFSLaunchAgentOptions) error {
	switch {
	case opts.ExecutablePath == "":
		return errors.New("daemon: watch-fs launch agent executable path is required")
	case opts.DataDir == "":
		return errors.New("daemon: watch-fs launch agent data dir is required")
	case len(opts.Roots) == 0:
		return errors.New("daemon: watch-fs launch agent requires at least one root")
	case opts.LogPath == "":
		return errors.New("daemon: watch-fs launch agent log path is required")
	}
	return nil
}

// watchFSPlistData is the escaped view passed to the template.
type watchFSPlistData struct {
	Label          string
	ExecutablePath string
	DataDir        string
	Roots          []string
	LogPath        string
	Home           string
	Path           string
}

// escapeWatchFSPlistData XML-escapes every dynamic plist field so a path
// or root containing &, <, >, or " cannot produce malformed XML.
func escapeWatchFSPlistData(opts *WatchFSLaunchAgentOptions) (watchFSPlistData, error) {
	// Escape into the output struct, leaving opts untouched (no in-place
	// double-escape if called twice). label is a lowercase human
	// description, not a Go field name, so the error string stays
	// convention-compliant on the rare escape failure.
	out := watchFSPlistData{Label: WatchFSLaunchAgentLabel} // package-internal constant, no escaping needed
	fields := []struct {
		label string
		value string
		dst   *string
	}{
		{"executable path", opts.ExecutablePath, &out.ExecutablePath},
		{"data dir", opts.DataDir, &out.DataDir},
		{"log path", opts.LogPath, &out.LogPath},
		{"home", opts.Home, &out.Home},
		{"path", opts.Path, &out.Path},
	}
	for _, f := range fields {
		esc, err := escapeXML(f.value)
		if err != nil {
			return watchFSPlistData{}, fmt.Errorf("daemon: escape %s: %w", f.label, err)
		}
		*f.dst = esc
	}
	out.Roots = make([]string, 0, len(opts.Roots))
	for _, r := range opts.Roots {
		esc, eerr := escapeXML(r)
		if eerr != nil {
			return watchFSPlistData{}, fmt.Errorf("daemon: escape root %q: %w", r, eerr)
		}
		out.Roots = append(out.Roots, esc)
	}
	return out, nil
}

// InstallWatchFSLaunchAgent writes the watcher plist to its canonical
// location and loads it via `launchctl bootstrap`, which (RunAtLoad=true)
// also starts the watcher immediately. Empty option fields are defaulted
// so the common call site only needs DataDir and Roots.
//
// Idempotent: a plist already at the destination is overwritten and the
// stale job is booted out + reloaded so the fresh definition (e.g. a
// changed root set) takes effect (bootstrapOrReload).
func InstallWatchFSLaunchAgent(ctx context.Context, opts *WatchFSLaunchAgentOptions) error {
	if opts == nil {
		return errors.New("daemon: watch-fs launch agent opts required")
	}
	if opts.DataDir == "" {
		return errors.New("daemon: data dir required")
	}
	if len(opts.Roots) == 0 {
		return errors.New("daemon: at least one root required")
	}
	if err := defaultWatchFSLaunchAgentOptions(opts); err != nil {
		return err
	}
	body, err := RenderWatchFSLaunchAgentPlist(opts)
	if err != nil {
		return err
	}
	plistPath, err := WatchFSLaunchAgentPlistPath()
	if err != nil {
		return err
	}
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
	return bootstrapOrReload(ctx, domain, WatchFSLaunchAgentLabel, plistPath)
}

// defaultWatchFSLaunchAgentOptions fills empty fields with install-time
// defaults: the running maz binary, <DataDir>/watch-fs-launchd.log, and
// the installing shell's HOME + PATH. Assumes opts, opts.DataDir, and
// opts.Roots are already validated.
func defaultWatchFSLaunchAgentOptions(opts *WatchFSLaunchAgentOptions) error {
	if opts.ExecutablePath == "" {
		exe, err := ResolveExecutable()
		if err != nil {
			return err
		}
		opts.ExecutablePath = exe
	}
	if opts.LogPath == "" {
		opts.LogPath = filepath.Join(opts.DataDir, "watch-fs-launchd.log")
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

// UninstallWatchFSLaunchAgent runs `launchctl bootout` to stop the
// watcher and removes the plist from disk. Missing service / missing
// plist are not errors (re-running uninstall is a no-op). This is the
// correct way to stop a launchd-managed watcher: a bare SIGTERM would be
// undone by KeepAlive, which relaunches the process the signal just
// killed.
func UninstallWatchFSLaunchAgent(ctx context.Context) error {
	plistPath, err := WatchFSLaunchAgentPlistPath()
	if err != nil {
		return err
	}
	domain, derr := launchdUserDomain()
	if derr != nil {
		return derr
	}
	if cerr := runLaunchctl(ctx, "bootout", domain+"/"+WatchFSLaunchAgentLabel); cerr != nil {
		if !notLoaded(cerr) {
			return fmt.Errorf("daemon: launchctl bootout %s: %w", WatchFSLaunchAgentLabel, cerr)
		}
	}
	if rerr := os.Remove(plistPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove plist %s: %w", plistPath, rerr)
	}
	return nil
}

// WatchFSLaunchAgentLoaded reports whether the watcher's launch agent is
// currently loaded into the user domain. Used by `maz watch-fs status`
// and by maz doctor to surface launchd ownership.
func WatchFSLaunchAgentLoaded(ctx context.Context) (bool, error) {
	loaded, _, err := WatchFSLaunchAgentJobPID(ctx)
	return loaded, err
}

// WatchFSLaunchAgentJobPID returns whether the watcher job is loaded and
// the PID launchd reports for it. `launchctl list <label>` prints a
// plist-ish dict whose `"PID" = <n>;` line names the running child; that
// line is absent when the job is loaded but not currently running (e.g.
// between KeepAlive restarts), in which case pid is 0 and loaded is true.
// A not-loaded label returns (false, 0, nil).
func WatchFSLaunchAgentJobPID(ctx context.Context) (loaded bool, pid int, err error) {
	cctx, cancel := context.WithTimeout(ctx, launchctlTimeout)
	defer cancel()

	out, runErr := exec.CommandContext(cctx, "launchctl", "list", WatchFSLaunchAgentLabel).Output()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return false, 0, nil // "Could not find service" == not loaded
		}
		return false, 0, fmt.Errorf("daemon: launchctl list %s: %w", WatchFSLaunchAgentLabel, runErr)
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
			return true, 0, nil
		}
		return true, parsed, nil
	}
	return true, 0, nil // loaded but child not currently running
}
