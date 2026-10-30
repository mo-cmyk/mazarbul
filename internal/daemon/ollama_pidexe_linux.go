//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// defaultPidExePath returns the target of /proc/<pid>/exe — the
// absolute path of the executable a process was started from. Used to
// confirm a PID claimed by the sidecar file (or discovered on a
// listening port) is in fact `ollama` before the supervisor adopts it.
//
// A "permission denied" readlink indicates the PID belongs to another
// user; we surface the raw error so callers can decide whether to
// proceed (a wrong-user Ollama is almost certainly not ours, so the
// identity check fails closed).
//
// Ctx is accepted for signature parity with the darwin implementation
// (which shells out to `ps` and benefits from cancellation). The Linux
// readlink syscall returns essentially instantly and ignores ctx.
func defaultPidExePath(_ context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("pid must be positive")
	}
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return "", fmt.Errorf("daemon: readlink /proc/%d/exe: %w", pid, err)
	}
	return target, nil
}
