package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BootTraceFile is the basename written under <dataDir>. Kept relative so
// the file lives alongside the rest of the kernel store and survives the
// same backup/copy procedures.
const BootTraceFile = "boot-trace.log"

// BootPhase names every gate the daemon crosses between "process started"
// and "MCP server accepting requests". The set is intentionally fine-
// grained: when the host freezes mid-boot, the trace file's final line
// names the phase that started but never finished, which is the entire
// reason this layer exists. Adding a phase is cheap; removing one breaks
// the diagnostic.
//
// Phase order in a healthy boot:
//
//	entry -> logger-init -> pid-write -> mcp-new -> wiring-open ->
//	  config-load -> embedder-probe -> ontology -> orchestrator-new ->
//	  hybrid-query -> tool-register -> ollama-supervisor ->
//	  server-start -> ready
type BootPhase string

// Phase* enumerate the boot gates the tracer surfaces. The names appear
// verbatim in the trace file and the `maz daemon boot-trace` output; do
// not rename without grepping the docs and the operator runbooks too.
const (
	PhaseEntry            BootPhase = "entry"
	PhaseLoggerInit       BootPhase = "logger-init"
	PhasePIDWrite         BootPhase = "pid-write"
	PhaseMCPNew           BootPhase = "mcp-new"
	PhaseWiringOpen       BootPhase = "wiring-open"
	PhaseConfigLoad       BootPhase = "config-load"
	PhaseEmbedderProbe    BootPhase = "embedder-probe"
	PhaseOntology         BootPhase = "ontology"
	PhaseOrchestratorNew  BootPhase = "orchestrator-new"
	PhaseHybridQuery      BootPhase = "hybrid-query"
	PhaseToolRegister     BootPhase = "tool-register"
	PhaseOllamaSupervisor BootPhase = "ollama-supervisor"
	PhaseServerStart      BootPhase = "server-start"
	PhaseReady            BootPhase = "ready"
)

// BootTracer is the on-disk "where did we get stuck" sentinel. Each Mark
// appends `<RFC3339Nano> <phase>\n` and fsyncs BEFORE the calling code
// proceeds with the named phase. A kernel panic, OOM kill, or hard host
// freeze mid-phase leaves the trace ending on the phase that started but
// did not complete — diagnostics zap's buffered file logger cannot give
// because its writer is lost when the process dies abruptly.
//
// Zero value is safe: Mark and Close are no-ops on a nil receiver, so the
// supervisor can carry an *unconditional* BootTracer field without
// branching at every call site.
type BootTracer struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// NewBootTracer truncates and opens <dataDir>/boot-trace.log for append.
// Truncation is intentional: only the most recent boot attempt is
// interesting, and keeping prior runs around would let a phase from a
// previous (successful) boot mask the current one's hang.
//
// Returns (nil, err) on filesystem failure. Callers must treat the
// tracer as optional — a missing trace must NOT block the daemon from
// starting; the trace is a diagnostic aid, not a critical path.
func NewBootTracer(dataDir string) (*BootTracer, error) {
	if dataDir == "" {
		return nil, errors.New("daemon: BootTracer requires DataDir")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: boot trace mkdir: %w", err)
	}
	path := filepath.Join(dataDir, BootTraceFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // boot trace lives under user-owned dataDir
	if err != nil {
		return nil, fmt.Errorf("daemon: boot trace open: %w", err)
	}
	return &BootTracer{f: f, path: path}, nil
}

// Mark writes one phase line and fsyncs it to disk. The whole point of
// the file is that the line is durable BEFORE the next instruction
// executes; a buffered write defeats the purpose. Write failures are
// swallowed (best effort) so a full disk cannot wedge daemon boot.
func (b *BootTracer) Mark(phase BootPhase) {
	if b == nil || b.f == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	line := time.Now().UTC().Format(time.RFC3339Nano) + " " + string(phase) + "\n"
	if _, err := b.f.WriteString(line); err != nil {
		return
	}
	_ = b.f.Sync() //nolint:errcheck // best-effort; a full disk must not wedge boot
}

// Path returns the absolute file path the tracer writes to. Used by the
// `maz daemon boot-trace` CLI to locate the most recent trace.
func (b *BootTracer) Path() string {
	if b == nil {
		return ""
	}
	return b.path
}

// Close releases the file handle. Safe to call multiple times.
func (b *BootTracer) Close() error {
	if b == nil || b.f == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.f.Close()
	b.f = nil
	if err != nil {
		return fmt.Errorf("daemon: boot trace close: %w", err)
	}
	return nil
}

// ReadBootTrace returns the contents of the most recent boot trace under
// dataDir. Empty bytes + nil error when the file does not exist (no boot
// has happened yet on this data dir).
func ReadBootTrace(dataDir string) ([]byte, error) {
	if dataDir == "" {
		return nil, errors.New("daemon: ReadBootTrace requires DataDir")
	}
	path := filepath.Join(dataDir, BootTraceFile)
	body, err := os.ReadFile(path) //nolint:gosec // boot trace is under user dataDir
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("daemon: read boot trace: %w", err)
	}
	return body, nil
}
