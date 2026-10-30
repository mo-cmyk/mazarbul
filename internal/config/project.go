package config

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ProjectConfigFile is the filename that triggers per-project config
// overlay. When the CLI is invoked from a directory containing this file
// it is loaded as the highest-priority TOML source above
// ${DATA_DIR}/config.toml (but below CLI flags and MAZ_* env vars).
//
// The file is opt-in: maz init adds it to .gitignore when .gitignore
// exists so the per-project knobs stay local to the developer's machine.
const ProjectConfigFile = ".mazarbul.toml"

// ErrProjectConfigNotFound is returned by LoadProjectConfig when no
// .mazarbul.toml is present in the search path. Callers treat this as
// "no per-project overlay" rather than an error.
var ErrProjectConfigNotFound = errors.New("config: .mazarbul.toml not found")

// LoadProjectConfig searches dir and its parents up to the filesystem root
// for a .mazarbul.toml file. The first match wins (closest to the working
// directory). A missing file returns ErrProjectConfigNotFound. Any read
// or parse failure returns the underlying error.
//
// Parsed values are merged into a UserConfig; the caller is responsible
// for calling Merge with the result. All fields use zero-means-inherit
// semantics identical to UserState.
func LoadProjectConfig(dir string) (UserConfig, error) {
	path, found := findProjectConfig(dir)
	if !found {
		return UserConfig{}, ErrProjectConfigNotFound
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return UserConfig{}, err
	}
	var cfg UserConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return UserConfig{}, err
	}
	return cfg, nil
}

// FindProjectConfigPath walks up from dir until it finds .mazarbul.toml
// or reaches the filesystem root. Returns the absolute path and true when
// found, or ("", false) when not.
func FindProjectConfigPath(dir string) (string, bool) {
	return findProjectConfig(dir)
}

// findProjectConfig is the unexported walker used by LoadProjectConfig.
func findProjectConfig(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	for {
		candidate := filepath.Join(dir, ProjectConfigFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root.
			return "", false
		}
		dir = parent
	}
}

// MergeProjectConfig applies the per-project overlay on top of base.
// It reuses the same field-by-field zero-means-inherit logic as Merge.
// The overlay may be the zero value (from a missing .mazarbul.toml); in that
// case base is returned unchanged.
//
//nolint:gocritic // copied by value to keep call sites natural.
func MergeProjectConfig(base UserConfig, overlay UserConfig) UserConfig {
	// Build a synthetic UserState from the overlay and use the existing
	// Merge logic to apply field-by-field overrides.
	state := UserState{
		Defaults: DefaultsStateTable{
			Chat:  overlay.Defaults.Chat,
			Embed: overlay.Defaults.Embed,
		},
		Recall:    overlay.Recall,
		Composer:  overlay.Composer,
		Ingestion: overlay.Ingestion,
		Log: LogStateTable{
			Level:  overlay.Log.Level,
			Format: overlay.Log.Format,
		},
	}
	if len(overlay.Providers) > 0 {
		state.Providers = make(map[string]ProviderStateTable, len(overlay.Providers))
		for name, pt := range overlay.Providers {
			// Managed is a plain bool in UserConfig; we cannot tell
			// "omitted" from "explicitly false". Treat true as an
			// explicit opt-in and false as "inherit base" so a project
			// overlay that only overrides a model does not silently
			// disable a managed Ollama setting inherited from the user
			// config. Operators who specifically want to disable it
			// can still do so via state.toml or `maz config set`.
			var managedPtr *bool
			if pt.Managed {
				v := true
				managedPtr = &v
			}
			state.Providers[name] = ProviderStateTable{
				Model:       pt.Model,
				Secret:      pt.Secret,
				BaseURL:     pt.BaseURL,
				ChatModel:   pt.ChatModel,
				EmbedModel:  pt.EmbedModel,
				Managed:     managedPtr,
				NumParallel: pt.NumParallel,
			}
		}
	}
	// Sync overlay: scalar settings apply unconditionally so a
	// `.mazarbul.toml` carrying just `[sync] auto_push = true` (no
	// `[sync.remotes.*]`) still takes effect. The earlier shape gated
	// the entire branch on len(Remotes)>0, which dropped the scalars.
	state.Sync = SyncStateTable{
		CoalesceSeconds:        overlay.Sync.CoalesceSeconds, // *int; nil == "inherit"
		SealMaxEvents:          overlay.Sync.SealMaxEvents,
		SealMaxEventsPlaintext: overlay.Sync.SealMaxEventsPlaintext,
		SealIntervalSeconds:    overlay.Sync.SealIntervalSeconds,
	}
	if overlay.Sync.AutoPush {
		v := true
		state.Sync.AutoPush = &v
	}
	if len(overlay.Sync.Remotes) > 0 {
		state.Sync.Remotes = make(map[string]ScopeRemoteStateTable, len(overlay.Sync.Remotes))
		for scope, sr := range overlay.Sync.Remotes {
			enc := sr.Encrypted
			state.Sync.Remotes[scope] = ScopeRemoteStateTable{
				URL:        sr.URL,
				AuthMethod: sr.AuthMethod,
				Encrypted:  enc,
			}
		}
	}
	return Merge(base, state)
}

// AddProjectConfigToGitignore adds .mazarbul.toml to the .gitignore file
// in dir if .gitignore exists and the entry is not already present.
// No-op when .gitignore is absent. Returns no error in the no-op case.
func AddProjectConfigToGitignore(dir string) error {
	gitignorePath := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // .gitignore absent; no-op
		}
		return err
	}
	entry := ProjectConfigFile
	if containsLine(string(data), entry) {
		return nil // already present
	}
	// Append with a trailing newline.
	appended := string(data)
	if len(appended) > 0 && appended[len(appended)-1] != '\n' {
		appended += "\n"
	}
	appended += entry + "\n"
	return os.WriteFile(gitignorePath, []byte(appended), 0o644)
}

// containsLine reports whether s contains line as a standalone line.
func containsLine(s, line string) bool {
	for len(s) > 0 {
		var row string
		if i := indexByte(s, '\n'); i >= 0 {
			row = s[:i]
			s = s[i+1:]
		} else {
			row = s
			s = ""
		}
		if row == line {
			return true
		}
	}
	return false
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}
