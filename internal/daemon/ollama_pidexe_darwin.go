//go:build darwin

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// pidExeProbeTimeout bounds the `ps` invocation so a hung process
// table cannot stall the supervisor goroutine during identity check.
const pidExeProbeTimeout = 1 * time.Second

// defaultPidExePath returns the executable name for pid using `ps -o
// comm=`. MacOS lacks /proc, and pulling proc_pidpath via cgo would add
// build complexity for a check that runs at most a handful of times per
// daemon start. Shelling out to ps is the same compromise used by
// `maz doctor` for other process-identity checks.
//
// The returned value is `ps`'s `comm` column — the basename of the
// executable on POSIX, which is exactly what isOllamaExe wants to match.
// An empty string with no error means "unknown" (ps returned a blank
// line for a PID that does not exist); callers treat that as "do not
// trust the PID.".
func defaultPidExePath(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("pid must be positive")
	}
	cctx, cancel := context.WithTimeout(ctx, pidExeProbeTimeout)
	defer cancel()
	//nolint:gosec // args are fixed strings + a numeric PID; no shell, no user input
	out, err := exec.CommandContext(cctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("daemon: ps -p %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}
