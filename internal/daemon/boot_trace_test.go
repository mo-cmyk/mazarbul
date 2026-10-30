package daemon_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// TestBootTracer_MarksAreDurable verifies the central invariant: every
// Mark is on disk by the time it returns. Mid-boot kernel panics on
// macOS lose any buffered data, so this layer only earns its keep if
// each phase line is fsync'd before the next instruction runs.
func TestBootTracer_MarksAreDurable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr, err := daemon.NewBootTracer(dir)
	if err != nil {
		t.Fatalf("NewBootTracer: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	phases := []daemon.BootPhase{
		daemon.PhaseEntry,
		daemon.PhasePIDWrite,
		daemon.PhaseWiringOpen,
	}
	for _, p := range phases {
		tr.Mark(p)
		// Read while the tracer file is still open: every Mark must be
		// visible immediately, not buffered until Close.
		body, rerr := os.ReadFile(filepath.Join(dir, daemon.BootTraceFile))
		if rerr != nil {
			t.Fatalf("read trace after %s: %v", p, rerr)
		}
		if !strings.Contains(string(body), string(p)) {
			t.Fatalf("trace missing phase %q after Mark; got: %s", p, body)
		}
	}
}

// TestBootTracer_NilReceiverNoOps documents that callers can carry an
// unconditional *BootTracer field. A failed NewBootTracer must not turn
// every downstream tr.Mark site into a nil-check ladder; the receiver
// is defensively nil-safe.
func TestBootTracer_NilReceiverNoOps(t *testing.T) {
	t.Parallel()
	var tr *daemon.BootTracer
	tr.Mark(daemon.PhaseEntry)
	if err := tr.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	if path := tr.Path(); path != "" {
		t.Errorf("nil Path = %q, want empty", path)
	}
}

// TestBootTracer_TruncatesOnEachBoot verifies that opening a new tracer
// over an existing file replaces it rather than appending. A surviving
// phase line from a prior healthy boot would otherwise mask the current
// boot's hang.
func TestBootTracer_TruncatesOnEachBoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr1, err := daemon.NewBootTracer(dir)
	if err != nil {
		t.Fatalf("NewBootTracer (1): %v", err)
	}
	tr1.Mark(daemon.PhaseReady)
	if err := tr1.Close(); err != nil {
		t.Fatalf("close tr1: %v", err)
	}

	tr2, err := daemon.NewBootTracer(dir)
	if err != nil {
		t.Fatalf("NewBootTracer (2): %v", err)
	}
	t.Cleanup(func() { _ = tr2.Close() })
	tr2.Mark(daemon.PhaseEntry)

	body, err := os.ReadFile(filepath.Join(dir, daemon.BootTraceFile))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if strings.Contains(string(body), string(daemon.PhaseReady)) {
		t.Errorf("trace from prior boot leaked into current run: %s", body)
	}
	if !strings.Contains(string(body), string(daemon.PhaseEntry)) {
		t.Errorf("current boot's entry phase missing: %s", body)
	}
}

// TestReadBootTrace_MissingFileReturnsEmpty verifies the CLI helper
// degrades gracefully when no boot has happened yet under dataDir.
func TestReadBootTrace_MissingFileReturnsEmpty(t *testing.T) {
	t.Parallel()
	body, err := daemon.ReadBootTrace(t.TempDir())
	if err != nil {
		t.Fatalf("ReadBootTrace: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body for missing trace, got %q", body)
	}
}
