package connect

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// ResolveEntry returns the ServerEntry to register Mazarbul with: the
// command to run, the args, and any env vars needed to point a fresh
// process at the user's data directory.
//
// Command resolution order:
//  1. If `maz` is on $PATH and resolves to the same inode as
//     os.Executable(), use the bare "maz" — survives PATH-relative
//     installs (homebrew, asdf, mise).
//  2. Otherwise the absolute path of the running binary.
//  3. As a last resort the literal "maz" (used by tests where
//     os.Executable() returns a temp test binary).
//
// dataDir is set in Env iff it differs from the default ("" means
// "don't bother setting MAZ_DATA_DIR"; the client uses Mazarbul's own
// default-resolution path). When dataDir matches the current build's
// default-resolution result, Env is omitted so client configs are not
// pinned to a frozen-in-time path; future Mazarbul processes resolve
// their own default each time.
func ResolveEntry(dataDir string) (ServerEntry, error) {
	cmd, err := resolveCommand()
	if err != nil {
		return ServerEntry{}, fmt.Errorf("resolve maz command: %w", err)
	}
	entry := ServerEntry{
		Command: cmd,
		Args:    []string{"mcp", "serve"},
	}
	if dataDir != "" && dataDir != config.Defaults().DataDir {
		entry.Env = map[string]string{"MAZ_DATA_DIR": dataDir}
	}
	return entry, nil
}

func resolveCommand() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return "", fmt.Errorf("stat self binary: %w", err)
	}
	if mazPath, lookupErr := exec.LookPath("maz"); lookupErr == nil {
		pathInfo, statErr := os.Stat(mazPath)
		if statErr == nil && os.SameFile(selfInfo, pathInfo) {
			return "maz", nil
		}
	}
	if self != "" {
		return self, nil
	}
	return "", errors.New("could not determine maz binary path")
}
