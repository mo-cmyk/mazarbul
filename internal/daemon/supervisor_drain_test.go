package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// permissiveDrainOntology accepts every payload so the test can seed raw episode
// events without registering schemas. Mirrors the kernel test's fakeOntology.
type permissiveDrainOntology struct{}

func (permissiveDrainOntology) Schema(_ context.Context, _ string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}

func (permissiveDrainOntology) Register(_ context.Context, _ kernel.Schema) error {
	return kernel.ErrNotImplemented
}

func (permissiveDrainOntology) List(_ context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}

func (permissiveDrainOntology) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}

// drainTrackingExtractor records each episode UID passed to ExtractFrom.
type drainTrackingExtractor struct {
	mu   sync.Mutex
	uids map[string]bool
}

//nolint:gocritic // ev is passed by value to satisfy the kernel.ExtractorIface signature.
func (e *drainTrackingExtractor) ExtractFrom(_ context.Context, ev kernel.Event) ([]kernel.Event, error) {
	if string(ev.Kind) != "episode" {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.uids == nil {
		e.uids = map[string]bool{}
	}
	e.uids[ev.UID] = true
	return nil, nil
}

func (e *drainTrackingExtractor) called(uid string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.uids[uid]
}

// noopDrainServer is a minimal mcp.Server for wireAndBuildServer, which builds
// and registers but never Starts the server in this unit.
type noopDrainServer struct{}

func (noopDrainServer) Register(_ mcp.Tool) error        { return nil }
func (noopDrainServer) Start(_ context.Context) error    { return nil }
func (noopDrainServer) Shutdown(_ context.Context) error { return nil }

// TestWireAndBuildServer_DrainsBacklog is the failing-test-first anchor for
// moving the boot-time un-extracted-episode backlog drain OUT of wireMemoryLayer
// (which runs on EVERY OpenWiring, including one-shot in-process CLI commands
// whose deferred drainAllBackground would then block until the whole backlog
// extracted) and INTO the daemon supervisor's wireAndBuildServer.
//
// The injected NewWiring bypasses wireMemoryLayer entirely, so the seeded
// backlog is drained ONLY if wireAndBuildServer itself drains it. Before the
// move this fails (the supervisor never drained); after it passes.
func TestWireAndBuildServer_DrainsBacklog(t *testing.T) {
	ctx := context.Background()
	ont := permissiveDrainOntology{}
	dir := t.TempDir()

	const (
		ep1 = "01JZDRN000000000000000DRN1"
		ep2 = "01JZDRN000000000000000DRN2"
	)

	// Phase 1: append episodes with no extractor wired, so they stay
	// un-extracted in the JSONL log.
	writerLog := kernel.NewFileEventLog(dir, ont)
	if _, err := writerLog.Append(ctx, []kernel.Event{
		{UID: ep1, Kind: "episode", Source: "test", Confidence: 1.0, Payload: json.RawMessage(`{"text":"Frodo joined Erebor."}`)},
		{UID: ep2, Kind: "episode", Source: "test", Confidence: 1.0, Payload: json.RawMessage(`{"text":"Sam left the Shire."}`)},
	}); err != nil {
		t.Fatalf("append un-extracted episodes: %v", err)
	}
	if err := writerLog.Close(); err != nil {
		t.Fatalf("close writer log: %v", err)
	}

	// Phase 2: reopen with a tracking extractor and inject as the daemon wiring.
	ex := &drainTrackingExtractor{}
	log := kernel.NewFileEventLog(dir, ont, kernel.WithExtractor(ex))
	t.Cleanup(func() { _ = log.Close() })

	wired := &Wired{Orchestrator: &orchestrator.Orchestrator{Log: log}}
	opts := &SupervisorOptions{
		Name:    "maz-test",
		Version: "test",
		Logger:  zap.NewNop(),
		DataDir: dir,
		NewServer: func(_ mcp.Options) (mcp.Server, error) {
			return noopDrainServer{}, nil
		},
		NewWiring: func(_ context.Context, _ WiringConfig) (*Wired, error) {
			return wired, nil
		},
	}

	coord := newRebuildCoordinator(ctx, zap.NewNop())
	if _, _, err := wireAndBuildServer(ctx, opts, "tok", coord); err != nil {
		t.Fatalf("wireAndBuildServer: %v", err)
	}

	// The drain is async; poll until both seeded episodes reached the extractor,
	// proving the supervisor (not wireMemoryLayer) drained the backlog.
	deadline := time.Now().Add(5 * time.Second)
	for !ex.called(ep1) || !ex.called(ep2) {
		if time.Now().After(deadline) {
			t.Fatalf("backlog not drained by supervisor: ep1 called=%v ep2 called=%v",
				ex.called(ep1), ex.called(ep2))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := log.WaitForBackgroundWork(ctx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}
}
