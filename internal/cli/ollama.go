package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// darwinGOOS is the runtime.GOOS value the launchd commands gate on.
// Pulled to a constant per goconst because three RunE closures need it.
const darwinGOOS = "darwin"

// newOllamaCmd assembles the `maz ollama` subtree. The commands let a
// user hand Ollama's lifecycle to launchd (on macOS) so the in-process
// supervisor stops competing for the role. Once launchd owns the
// child, the in-process OllamaSupervisor's pre-spawn HTTP probe
// detects the running endpoint and enters delegated mode automatically
// — so `providers.ollama.managed = true` in config.toml keeps working
// without changes; the supervisor just defers at runtime.
func newOllamaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ollama",
		Short: "Manage the Ollama runtime (macOS launchd integration)",
		Long: `Hand Ollama's lifecycle to macOS launchd.

Subcommands:
  install     write ~/Library/LaunchAgents/io.mazarbul.ollama.plist and load it
  uninstall   unload the agent and remove the plist
  status      report whether the agent is currently loaded

The plist pins the same memory caps as the in-process supervisor:
  OLLAMA_MAX_LOADED_MODELS=2, OLLAMA_KEEP_ALIVE=30s, OLLAMA_FLASH_ATTENTION=1,
  OLLAMA_NUM_PARALLEL from [providers.ollama] num_parallel (default 1).
Re-run install after changing num_parallel in config.toml.

After install the in-process supervisor automatically delegates to the
launchd-managed Ollama via its pre-spawn HTTP probe — no config change
needed.`,
	}
	cmd.AddCommand(newOllamaInstallCmd())
	cmd.AddCommand(newOllamaUninstallCmd())
	cmd.AddCommand(newOllamaStatusCmd())
	return cmd
}

func newOllamaInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the Ollama launchd agent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != darwinGOOS {
				return fmt.Errorf("maz ollama install is macOS-only (current: %s)", runtime.GOOS)
			}
			dataDir, err := resolveDataDir(cmd)
			if err != nil {
				return err
			}
			// Carry the operator's num_parallel into the launchd child so
			// the launchd path matches the supervisor-spawned path. Best
			// effort: a missing/broken config installs with the default.
			var opts daemon.OllamaLaunchAgentOptions
			if cfg, cfgErr := config.LoadUserConfig(dataDir); cfgErr == nil {
				opts.NumParallel = cfg.Providers["ollama"].NumParallel
			}
			if err := daemon.InstallOllamaLaunchAgent(cmd.Context(), dataDir, opts); err != nil {
				return err
			}
			plistPath, perr := daemon.OllamaLaunchAgentPlistPath()
			if perr != nil {
				return perr
			}
			return renderOllamaResult(cmd, cmd.OutOrStdout(), "installed", plistPath, true)
		},
	}
}

func newOllamaUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the Ollama launchd agent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != darwinGOOS {
				return fmt.Errorf("maz ollama uninstall is macOS-only (current: %s)", runtime.GOOS)
			}
			if err := daemon.UninstallOllamaLaunchAgent(cmd.Context()); err != nil {
				return err
			}
			return renderOllamaResult(cmd, cmd.OutOrStdout(), "uninstalled", "", false)
		},
	}
}

func newOllamaStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the Ollama launchd agent is loaded",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != darwinGOOS {
				return fmt.Errorf("maz ollama status is macOS-only (current: %s)", runtime.GOOS)
			}
			loaded, err := daemon.OllamaLaunchAgentLoaded(cmd.Context())
			if err != nil {
				return err
			}
			label := daemon.OllamaLaunchAgentLabel
			plistPath, perr := daemon.OllamaLaunchAgentPlistPath()
			if perr != nil {
				return perr
			}
			return renderOllamaStatus(cmd, cmd.OutOrStdout(), label, plistPath, loaded)
		},
	}
}

// renderOllamaResult prints the outcome of install/uninstall.
// PlistPath may be empty for the uninstall path.
func renderOllamaResult(cmd *cobra.Command, w io.Writer, action, plistPath string, loaded bool) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"action":    action,
			"plistPath": plistPath,
			"loaded":    loaded,
		})
	}
	_, err := fmt.Fprintf(w, "ollama launchd agent %s\n", action)
	return err
}

func renderOllamaStatus(cmd *cobra.Command, w io.Writer, label, plistPath string, loaded bool) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"label":     label,
			"plistPath": plistPath,
			"loaded":    loaded,
		})
	}
	state := "loaded"
	if !loaded {
		state = "not loaded"
	}
	_, err := fmt.Fprintf(w, "%s: %s\n", label, state)
	return err
}

// resolveDataDir reuses the persistent --data-dir flag with the
// config-default fallback. Pulled into a helper so the three ollama
// subcommands stay short and consistent with how `maz init` resolves
// the same flag.
func resolveDataDir(cmd *cobra.Command) (string, error) {
	flag, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if flag != "" {
		return flag, nil
	}
	dir := config.Defaults().DataDir
	if dir == "" {
		return "", errors.New("data dir not set; pass --data-dir or set MAZ_DATA_DIR")
	}
	return dir, nil
}
