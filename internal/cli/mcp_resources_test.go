package cli

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// resourceRegistrarServer implements both mcp.Server and mcp.ResourceRegistrar,
// so registerServeResources must register context resources on it.
type resourceRegistrarServer struct {
	called       bool
	gotAssembler mcp.ContextAssembler
	gotNav       mcp.DocumentReader
}

func (s *resourceRegistrarServer) Register(mcp.Tool) error        { return nil }
func (s *resourceRegistrarServer) Start(context.Context) error    { return nil }
func (s *resourceRegistrarServer) Shutdown(context.Context) error { return nil }
func (s *resourceRegistrarServer) RegisterContextResources(a mcp.ContextAssembler, n mcp.DocumentReader) {
	s.called = true
	s.gotAssembler = a
	s.gotNav = n
}

// plainServer implements only mcp.Server (not ResourceRegistrar), so
// registerServeResources must be a safe no-op against it.
type plainServer struct{}

func (plainServer) Register(mcp.Tool) error        { return nil }
func (plainServer) Start(context.Context) error    { return nil }
func (plainServer) Shutdown(context.Context) error { return nil }

type stubAssembler struct{}

func (stubAssembler) Build(context.Context, mcp.ContextBriefRequest) (mcp.ContextBrief, error) {
	return mcp.ContextBrief{}, nil
}

func (stubAssembler) BuildIndex(context.Context, mcp.ContextIndexRequest) (mcp.ContextIndex, error) {
	return mcp.ContextIndex{}, nil
}

type stubDocReader struct{}

func (stubDocReader) GetDocumentByPath(context.Context, string) (mcp.DocumentRecord, error) {
	return mcp.DocumentRecord{}, nil
}

func (stubDocReader) GetDocumentBySlug(context.Context, string) (mcp.DocumentRecord, error) {
	return mcp.DocumentRecord{}, nil
}

func (stubDocReader) ListDocuments(context.Context, mcp.DocumentListOpts) ([]mcp.DocumentRecord, error) {
	return nil, nil
}

// TestRegisterServeResources_RegistersWhenSupported pins the standalone
// `maz mcp serve` wiring: it must register context resources (hot brief, vault
// index, pinned docs, documents template), threading the wired assembler and
// navigator straight through. Regression for "serve advertised the resources
// capability but exposed an empty resource set" — the call existed only on the
// supervisor daemon path.
func TestRegisterServeResources_RegistersWhenSupported(t *testing.T) {
	srv := &resourceRegistrarServer{}
	asm := &stubAssembler{}
	nav := &stubDocReader{}

	if ok := registerServeResources(srv, asm, nav); !ok {
		t.Fatal("registerServeResources returned false, want true (server supports resources)")
	}
	if !srv.called {
		t.Fatal("RegisterContextResources was not called on a ResourceRegistrar server")
	}
	if srv.gotAssembler != mcp.ContextAssembler(asm) {
		t.Error("assembler was not threaded through to RegisterContextResources")
	}
	if srv.gotNav != mcp.DocumentReader(nav) {
		t.Error("document reader was not threaded through to RegisterContextResources")
	}
}

// TestRegisterServeResources_NoopWhenUnsupported proves the helper degrades
// gracefully: a server without ResourceRegistrar (e.g. a transport build that
// omits resources, or NoopServer) yields a no-op rather than a panic.
func TestRegisterServeResources_NoopWhenUnsupported(t *testing.T) {
	if ok := registerServeResources(plainServer{}, &stubAssembler{}, &stubDocReader{}); ok {
		t.Fatal("registerServeResources returned true for a server without ResourceRegistrar, want false")
	}
}
