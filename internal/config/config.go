// Package config loads YAML configuration with environment-variable overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds runtime configuration. Kept intentionally small at M0; expand
// as kernel, plugins, and sync land in later milestones.
type Config struct {
	Environment string    `yaml:"environment"`
	DataDir     string    `yaml:"data_dir"`
	TUI         TUIConfig `yaml:"tui"`
	Log         LogConfig `yaml:"log"`
	Debug       DebugCfg  `yaml:"debug"`
}

// TUIConfig configures the terminal UI.
type TUIConfig struct {
	// Interactive controls whether bare `maz` opens the interactive hub
	// (default true). Set to false (or MAZ_HEADLESS=1) to make bare `maz`
	// print help and exit instead of launching the hub.
	Interactive bool `yaml:"interactive"`
}

// LogConfig configures logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// FilePath, when non-empty, routes logs to a rotated file. Used by the
	// daemon supervisor; foreground commands leave it empty and write to
	// stderr per 12-factor.
	FilePath string `yaml:"file_path"`
}

// DebugCfg configures the optional debug HTTP server (pprof).
type DebugCfg struct {
	Addr string `yaml:"addr"`
}

// Defaults returns a Config populated with built-in defaults.
func Defaults() Config {
	return Config{
		Environment: "dev",
		DataDir:     defaultDataDir(),
		TUI:         TUIConfig{Interactive: true},
		Log:         LogConfig{Level: "info", Format: "json", FilePath: defaultDaemonLogFile()},
	}
}

// DaemonLogFile returns the resolved daemon log file path for the current
// environment. Exposed so the CLI and supervisor agree on a single location
// without re-implementing the resolution rules.
func DaemonLogFile() string {
	return defaultDaemonLogFile()
}

// OwnedDaemonLogDir returns the directory Mazarbul owns for daemon logs,
// ignoring any MAZ_LOG_FILE override. Callers (notably the logger) gate
// directory-mode enforcement on this: a parent that matches is safe to
// chmod 0700; arbitrary user-supplied parents (e.g. /tmp via
// MAZ_LOG_FILE=/tmp/maz.log) must be left alone. Returns an empty string
// when the user's home directory cannot be resolved.
func OwnedDaemonLogDir() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "mazarbul")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "mazarbul")
}

// Load reads YAML at path, merges over Defaults, then applies environment
// overrides (MAZ_* variables). A missing file is not an error.
func Load(path string) (Config, error) {
	cfg := Defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			// fall through to env overrides
		case err != nil:
			return cfg, fmt.Errorf("read %s: %w", path, err)
		default:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse %s: %w", path, err)
			}
		}
	}

	applyEnv(&cfg)
	return cfg, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("MAZ_ENV"); v != "" {
		c.Environment = v
	}
	if v := os.Getenv("MAZ_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if os.Getenv("MAZ_HEADLESS") == "1" || strings.EqualFold(os.Getenv("MAZ_HEADLESS"), "true") {
		c.TUI.Interactive = false
	}
	if v := os.Getenv("MAZ_LOG_LEVEL"); v != "" {
		c.Log.Level = strings.ToLower(v)
	}
	if v := os.Getenv("MAZ_LOG_FORMAT"); v != "" {
		c.Log.Format = strings.ToLower(v)
	}
	if v := os.Getenv("MAZ_LOG_FILE"); v != "" {
		c.Log.FilePath = v
	}
	if v := os.Getenv("MAZ_DEBUG_ADDR"); v != "" {
		c.Debug.Addr = v
	}
}

func defaultDataDir() string {
	if v := os.Getenv("MAZ_DATA_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "mazarbul")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".mazarbul-data"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Mazarbul")
	}
	return filepath.Join(home, ".local", "share", "mazarbul")
}

func defaultDaemonLogFile() string {
	if v := os.Getenv("MAZ_LOG_FILE"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "mazarbul", "daemon.log")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".mazarbul-cache", "daemon.log")
	}
	return filepath.Join(home, ".cache", "mazarbul", "daemon.log")
}
