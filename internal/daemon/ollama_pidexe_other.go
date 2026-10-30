//go:build !darwin && !linux && !windows

package daemon

import "context"

// defaultPidExePath is a fallback for platforms where the supervisor
// runs (the file is build-tagged !windows) but there's no
// off-the-shelf way to map a PID to an executable path. The returned
// empty string signals "identity check unavailable"; isOllamaExe
// treats that as "trust the PID" so the supervisor still works on
// FreeBSD, OpenBSD, etc. without code changes.
func defaultPidExePath(_ context.Context, _ int) (string, error) {
	return "", nil
}
