// Package daemon hosts the primitives for the maz daemon supervisor:
// PID-file lifecycle today, log file + rotation (issue #41) and the
// start/stop/status/logs command tree (issue #42) once they land. The
// package is internal-only; the CLI surface composes it from
// internal/cli/daemon.go.
package daemon
