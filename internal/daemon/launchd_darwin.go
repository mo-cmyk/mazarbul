//go:build darwin

// Package daemon's launchd helpers wrap the macOS `launchctl` toolchain
// so `maz ollama install` can hand Ollama's lifecycle to launchd
// instead of supervising it in-process. Once the launch agent is
// loaded, the in-process OllamaSupervisor automatically delegates to
// it via its pre-spawn HTTP probe (see attemptRestart in
// ollama_supervisor.go), so users keep the existing `managed = true`
// config without breakage.

package daemon

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// launchctlTimeout bounds each launchctl invocation. Launchd commands
// are normally instantaneous; a multi-second runtime indicates the
// service manager is wedged and the caller should not block longer.
const launchctlTimeout = 5 * time.Second

// ollamaPlistTemplate is the property list launchd consumes. The four
// OLLAMA_* environment variables mirror the in-process supervisor's
// resource caps so a launchd-managed Ollama has the same memory
// footprint guarantees as the F1 fix.
const ollamaPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.ExecutablePath}}</string>
        <string>serve</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>OLLAMA_MAX_LOADED_MODELS</key><string>2</string>
        <key>OLLAMA_NUM_PARALLEL</key><string>{{.NumParallel}}</string>
        <key>OLLAMA_KEEP_ALIVE</key><string>30s</string>
        <key>OLLAMA_FLASH_ATTENTION</key><string>1</string>
{{- if .OllamaHost}}
        <key>OLLAMA_HOST</key><string>{{.OllamaHost}}</string>
{{- end}}
{{- if .OllamaModels}}
        <key>OLLAMA_MODELS</key><string>{{.OllamaModels}}</string>
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

// OllamaLaunchAgentPlistPath returns the canonical install location of
// the user-level launch agent plist for the calling user. Returns an
// error when $HOME cannot be resolved (a rare environmental hazard
// inside containers and recovery mode).
func OllamaLaunchAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve $HOME: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", OllamaLaunchAgentLabel+".plist"), nil
}

// RenderOllamaLaunchAgentPlist returns the plist XML bytes for opts.
// Pure function: no I/O, no exec — exists so tests can assert plist
// shape (caps, paths, label) without touching the filesystem.
func RenderOllamaLaunchAgentPlist(opts OllamaLaunchAgentOptions) ([]byte, error) {
	if opts.ExecutablePath == "" {
		return nil, errors.New("daemon: launch agent ExecutablePath is required")
	}
	if opts.LogPath == "" {
		return nil, errors.New("daemon: launch agent LogPath is required")
	}
	tpl, err := template.New("plist").Parse(ollamaPlistTemplate)
	if err != nil {
		return nil, fmt.Errorf("daemon: parse plist template: %w", err)
	}
	// text/template does not escape values; a path containing &, <, >,
	// or " (rare but possible — snap paths, user homedirs with shell
	// metacharacters) would produce malformed XML that launchctl
	// silently rejects. xml.EscapeText handles every case the spec
	// requires while leaving plain ASCII paths byte-identical.
	exeEsc, err := escapeXML(opts.ExecutablePath)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape ExecutablePath: %w", err)
	}
	logEsc, err := escapeXML(opts.LogPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape LogPath: %w", err)
	}
	hostEsc, err := escapeXML(opts.OllamaHost)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape OllamaHost: %w", err)
	}
	modelsEsc, err := escapeXML(opts.OllamaModels)
	if err != nil {
		return nil, fmt.Errorf("daemon: escape OllamaModels: %w", err)
	}
	// <= 0 keeps the historical pinned default; the template always
	// renders the key so the child env is fully explicit.
	numParallel := opts.NumParallel
	if numParallel <= 0 {
		numParallel = 1
	}
	var buf bytes.Buffer
	data := struct {
		Label          string
		ExecutablePath string
		LogPath        string
		OllamaHost     string
		OllamaModels   string
		NumParallel    int
	}{
		Label:          OllamaLaunchAgentLabel, // package-internal constant, no escaping needed
		ExecutablePath: exeEsc,
		LogPath:        logEsc,
		OllamaHost:     hostEsc,
		OllamaModels:   modelsEsc,
		NumParallel:    numParallel,
	}
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("daemon: render plist: %w", err)
	}
	return buf.Bytes(), nil
}

// escapeXML returns s with the five XML predefined entities replaced
// (xml.EscapeText is the canonical implementation; the wrapper hides
// the io.Writer ceremony).
func escapeXML(s string) (string, error) {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return "", fmt.Errorf("daemon: xml.EscapeText: %w", err)
	}
	return buf.String(), nil
}

// InstallOllamaLaunchAgent writes the plist to its canonical location
// and loads it via `launchctl bootstrap`. When opts.ExecutablePath is
// empty it resolves via exec.LookPath; when LogPath is empty it
// defaults to <dataDir>/ollama-launchd.log so the user has one
// well-known location for both supervisor-spawned and launchd-spawned
// Ollama logs.
//
// Idempotent: a plist already at the destination is overwritten with
// fresh content (cheap; lets users re-run after an Ollama upgrade
// changed the binary path). A "service already loaded" error from
// bootstrap is swallowed; the on-disk plist always reflects the most
// recent install.
func InstallOllamaLaunchAgent(ctx context.Context, dataDir string, opts OllamaLaunchAgentOptions) error {
	if dataDir == "" {
		return errors.New("daemon: dataDir required")
	}
	if opts.ExecutablePath == "" {
		path, err := exec.LookPath("ollama")
		if err != nil {
			return fmt.Errorf("daemon: ollama not on $PATH; install Ollama from https://ollama.com/download first: %w", err)
		}
		opts.ExecutablePath = path
	}
	if opts.LogPath == "" {
		opts.LogPath = filepath.Join(dataDir, "ollama-launchd.log")
	}
	// Capture the user's per-install Ollama env so the launchd child
	// keeps their custom bind address (OLLAMA_HOST) and model directory
	// (OLLAMA_MODELS). The in-process supervisor inherits these via
	// os.Environ; the launchd child has only what the plist declares, so
	// without this capture installed models can appear missing and the
	// daemon may probe the wrong endpoint after `maz ollama install`.
	// Explicit opts values (set by tests) override env capture.
	if opts.OllamaHost == "" {
		opts.OllamaHost = os.Getenv("OLLAMA_HOST")
	}
	if opts.OllamaModels == "" {
		opts.OllamaModels = os.Getenv("OLLAMA_MODELS")
	}
	body, err := RenderOllamaLaunchAgentPlist(opts)
	if err != nil {
		return err
	}
	plistPath, err := OllamaLaunchAgentPlistPath()
	if err != nil {
		return err
	}
	// 0700 / 0600 are tighter than launchd requires (the LaunchAgents
	// dir is per-user; no other principals need read access) and keep
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
	return bootstrapOrReload(ctx, domain, OllamaLaunchAgentLabel, plistPath)
}

// bootstrapOrReload loads the plist for label via `launchctl bootstrap`.
// When the agent is already loaded launchd silently keeps running the
// OLD job definition even after the plist on disk is rewritten, so
// re-running install to pick up a moved binary or changed env would
// otherwise be a no-op. Recover by booting the stale job out and
// bootstrapping the fresh plist. Label is parameterised so both the
// Ollama agent and the daemon's own agent share this one reload path.
func bootstrapOrReload(ctx context.Context, domain, label, plistPath string) error {
	cerr := runLaunchctl(ctx, "bootstrap", domain, plistPath)
	if cerr == nil {
		return nil
	}
	if !alreadyLoaded(cerr) {
		return fmt.Errorf("daemon: launchctl bootstrap %s: %w", plistPath, cerr)
	}
	if berr := runLaunchctl(ctx, "bootout", domain+"/"+label); berr != nil && !notLoaded(berr) {
		return fmt.Errorf("daemon: launchctl bootout for reload %s: %w", label, berr)
	}
	if rerr := runLaunchctl(ctx, "bootstrap", domain, plistPath); rerr != nil {
		return fmt.Errorf("daemon: launchctl bootstrap (reload) %s: %w", plistPath, rerr)
	}
	return nil
}

// UninstallOllamaLaunchAgent runs `launchctl bootout` to stop the
// service and removes the plist from disk. Missing plist is not an
// error (re-running uninstall is a no-op).
func UninstallOllamaLaunchAgent(ctx context.Context) error {
	plistPath, err := OllamaLaunchAgentPlistPath()
	if err != nil {
		return err
	}
	domain, derr := launchdUserDomain()
	if derr != nil {
		return derr
	}
	// bootout's "service not loaded" error must not block the plist
	// removal — uninstall must be idempotent.
	if cerr := runLaunchctl(ctx, "bootout", domain+"/"+OllamaLaunchAgentLabel); cerr != nil {
		if !notLoaded(cerr) {
			return fmt.Errorf("daemon: launchctl bootout %s: %w", OllamaLaunchAgentLabel, cerr)
		}
	}
	if rerr := os.Remove(plistPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove plist %s: %w", plistPath, rerr)
	}
	return nil
}

// OllamaLaunchAgentLoaded reports whether the launch agent is
// currently loaded into the user domain. Used by `maz ollama status`
// and by maz doctor to surface launchd ownership.
func OllamaLaunchAgentLoaded(ctx context.Context) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, launchctlTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "launchctl", "list", OllamaLaunchAgentLabel)
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

// launchdUserDomain returns the per-user launchd domain ("gui/<uid>")
// expected by `launchctl bootstrap`. Errors when the calling user has
// no numeric UID (essentially impossible on macOS but cheap to guard).
func launchdUserDomain() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("daemon: current user: %w", err)
	}
	return "gui/" + u.Uid, nil
}

// runLaunchctl is a single point through which all launchctl invocations
// flow; gives the test seam (replace with a fake) and the timeout
// guarantee (no command runs longer than launchctlTimeout).
func runLaunchctl(ctx context.Context, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, launchctlTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// alreadyLoaded reports whether err is the launchctl-bootstrap idiom
// for "this label is already known to launchd." Distinguishing it from
// a real failure lets Install be idempotent.
func alreadyLoaded(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "service already loaded") ||
		strings.Contains(msg, "already loaded") ||
		strings.Contains(msg, "operation already in progress")
}

// notLoaded reports whether err is the bootout idiom for "no such
// service is registered." Lets Uninstall be idempotent on re-run.
// "operation not permitted" is deliberately excluded — that is a
// permissions failure, not a missing service, and must propagate.
func notLoaded(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not find service") ||
		strings.Contains(msg, "no such process")
}
