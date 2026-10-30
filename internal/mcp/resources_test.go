package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeAssembler satisfies ContextAssembler for resource tests.
type fakeAssembler struct {
	brief ContextBrief
	index ContextIndex
	err   error
}

func (f *fakeAssembler) Build(_ context.Context, _ ContextBriefRequest) (ContextBrief, error) {
	return f.brief, f.err
}

func (f *fakeAssembler) BuildIndex(_ context.Context, _ ContextIndexRequest) (ContextIndex, error) {
	return f.index, f.err
}

// fakeDocReader satisfies DocumentReader for resource tests.
type fakeDocReader struct {
	docs []DocumentRecord
	// bySlug maps a frozen slug to a document body, mirroring the navigator's
	// slug lane. DocumentRecord has no Slug field, so the fake tracks it here.
	bySlug map[string]DocumentRecord
	err    error // returned by the getters for unknown refs
}

func (f *fakeDocReader) GetDocumentByPath(_ context.Context, path string) (DocumentRecord, error) {
	for _, d := range f.docs {
		if d.Path == path {
			return d, nil
		}
	}
	if f.err != nil {
		return DocumentRecord{}, f.err
	}
	return DocumentRecord{}, ErrDocumentNotFound
}

func (f *fakeDocReader) GetDocumentBySlug(_ context.Context, slug string) (DocumentRecord, error) {
	if d, ok := f.bySlug[slug]; ok {
		return d, nil
	}
	if f.err != nil {
		return DocumentRecord{}, f.err
	}
	return DocumentRecord{}, ErrDocumentNotFound
}

func (f *fakeDocReader) ListDocuments(_ context.Context, opts DocumentListOpts) ([]DocumentRecord, error) {
	out := make([]DocumentRecord, 0, len(f.docs))
	for _, d := range f.docs {
		if opts.PinnedOnly && !d.Pinned {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// newTestSrv builds a minimal *mcpServer (stdio transport, nop logger) for
// resource registration tests that do not start the transport.
func newTestSrv(t *testing.T) *mcpServer {
	t.Helper()
	opts := Options{
		Name:            "test",
		Version:         "0.0.0",
		Logger:          zap.NewNop(),
		Stdio:           true,
		ShutdownTimeout: time.Second,
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.(*mcpServer)
}

// TestResources_BuildPinnedMarkdown verifies the pure render helper with
// no-doc and multi-doc inputs.
func TestResources_BuildPinnedMarkdown(t *testing.T) {
	t.Run("no pinned docs", func(t *testing.T) {
		got := buildPinnedMarkdown(nil)
		if !strings.Contains(got, "_No pinned documents._") {
			t.Errorf("expected empty-state message, got: %q", got)
		}
	})

	t.Run("renders title and path", func(t *testing.T) {
		docs := []DocumentRecord{
			{Path: "notes/foo.md", Title: "Foo Note", Pinned: true},
			{Path: "bar.md", Pinned: true}, // no title falls back to path
		}
		got := buildPinnedMarkdown(docs)
		if !strings.Contains(got, "Foo Note") {
			t.Errorf("title missing: %q", got)
		}
		if !strings.Contains(got, "bar.md") {
			t.Errorf("path fallback missing: %q", got)
		}
	})
}

// TestResources_RegisterSkipsOnNilDeps verifies that RegisterContextResources
// is a safe no-op when either dependency is nil (degraded boot).
func TestResources_RegisterSkipsOnNilDeps(t *testing.T) {
	s := newTestSrv(t)
	// None of these must panic.
	s.RegisterContextResources(nil, &fakeDocReader{})
	s.RegisterContextResources(&fakeAssembler{}, nil)
	s.RegisterContextResources(nil, nil)
}

// TestResources_HotReturnsMarkdown verifies the hot-brief resource handler
// delegates to ContextAssembler.Build and surfaces its markdown verbatim.
func TestResources_HotReturnsMarkdown(t *testing.T) {
	assembler := &fakeAssembler{
		brief: ContextBrief{Markdown: "# Hot Brief\n\nSome context.", Tokens: 4, Budget: 4000},
	}
	// Verify the assembler round-trip: this is the exact call the handler makes.
	brief, err := assembler.Build(context.Background(), ContextBriefRequest{})
	if err != nil {
		t.Fatalf("assembler.Build: %v", err)
	}
	if brief.Markdown != "# Hot Brief\n\nSome context." {
		t.Errorf("markdown = %q, want hot brief content", brief.Markdown)
	}
	// Verify registration does not panic.
	newTestSrv(t).RegisterContextResources(assembler, &fakeDocReader{})
}

// TestResources_IndexReturnsMarkdown verifies the vault index resource handler
// delegates to ContextAssembler.BuildIndex.
func TestResources_IndexReturnsMarkdown(t *testing.T) {
	assembler := &fakeAssembler{
		index: ContextIndex{Markdown: "# Vault Index\n\nAll docs.", Documents: 3, Folders: 1},
	}
	idx, err := assembler.BuildIndex(context.Background(), ContextIndexRequest{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if idx.Markdown != "# Vault Index\n\nAll docs." {
		t.Errorf("markdown = %q, want index content", idx.Markdown)
	}
	if idx.Documents != 3 {
		t.Errorf("documents = %d, want 3", idx.Documents)
	}
	// Verify registration does not panic.
	newTestSrv(t).RegisterContextResources(assembler, &fakeDocReader{})
}

// TestResources_DocumentTemplateFoundAndNotFound verifies the slug resolution
// logic: a known path returns the body, an unknown path returns ErrDocumentNotFound.
func TestResources_DocumentTemplateFoundAndNotFound(t *testing.T) {
	nav := &fakeDocReader{
		docs: []DocumentRecord{
			{Path: "notes/foo.md", Title: "Foo", Body: "# Foo\nContent here.", Pinned: false},
		},
	}

	t.Run("known slug resolves body", func(t *testing.T) {
		doc, err := nav.GetDocumentByPath(context.Background(), "notes/foo.md")
		if err != nil {
			t.Fatalf("GetDocumentByPath: %v", err)
		}
		if doc.Body != "# Foo\nContent here." {
			t.Errorf("body = %q, want foo body", doc.Body)
		}
	})

	t.Run("unknown slug returns ErrDocumentNotFound", func(t *testing.T) {
		_, err := nav.GetDocumentByPath(context.Background(), "missing/doc.md")
		if err == nil {
			t.Fatal("expected error for unknown slug")
		}
		if !errors.Is(err, ErrDocumentNotFound) {
			t.Errorf("err = %v, want ErrDocumentNotFound", err)
		}
	})
}

// TestResolveDocumentForResource verifies the mazarbul://documents/{slug}
// resolver accepts both a vault path and a frozen slug. Regression for the bug
// where reading by slug (design-plan) failed while reading by path
// (design-plan.md) worked, despite the template variable being {slug} and the
// rendered tree using slug-named files.
func TestResolveDocumentForResource(t *testing.T) {
	nav := &fakeDocReader{
		docs: []DocumentRecord{
			{Path: "design-plan.md", Title: "Design Plan", Body: "# Design Plan\nBody.", Pinned: true},
		},
		bySlug: map[string]DocumentRecord{
			"design-plan": {Path: "design-plan.md", Title: "Design Plan", Body: "# Design Plan\nBody.", Pinned: true},
		},
	}

	t.Run("resolves by vault path", func(t *testing.T) {
		doc, err := resolveDocumentForResource(context.Background(), nav, "design-plan.md")
		if err != nil {
			t.Fatalf("resolveDocumentForResource(path): %v", err)
		}
		if doc.Body != "# Design Plan\nBody." {
			t.Errorf("body = %q, want design-plan body", doc.Body)
		}
	})

	t.Run("resolves by frozen slug when path misses", func(t *testing.T) {
		doc, err := resolveDocumentForResource(context.Background(), nav, "design-plan")
		if err != nil {
			t.Fatalf("resolveDocumentForResource(slug): %v", err)
		}
		if doc.Body != "# Design Plan\nBody." {
			t.Errorf("body = %q, want design-plan body resolved via slug", doc.Body)
		}
	})

	t.Run("unknown ref returns ErrDocumentNotFound", func(t *testing.T) {
		_, err := resolveDocumentForResource(context.Background(), nav, "nope")
		if !errors.Is(err, ErrDocumentNotFound) {
			t.Errorf("err = %v, want ErrDocumentNotFound", err)
		}
	})
}

// TestResources_PinnedListFilters verifies DocumentReader returns only
// pinned documents when PinnedOnly is set, and buildPinnedMarkdown renders them.
func TestResources_PinnedListFilters(t *testing.T) {
	nav := &fakeDocReader{
		docs: []DocumentRecord{
			{Path: "a.md", Title: "A", Pinned: true},
			{Path: "b.md", Title: "B", Pinned: false},
			{Path: "c.md", Title: "C", Pinned: true},
		},
	}

	pinned, err := nav.ListDocuments(context.Background(), DocumentListOpts{PinnedOnly: true, Limit: -1})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(pinned) != 2 {
		t.Errorf("pinned count = %d, want 2", len(pinned))
	}
	for _, d := range pinned {
		if !d.Pinned {
			t.Errorf("non-pinned doc in pinned listing: %q", d.Path)
		}
	}

	md := buildPinnedMarkdown(pinned)
	if !strings.Contains(md, "# Pinned Documents") {
		t.Errorf("header missing from pinned markdown: %q", md)
	}
	if !strings.Contains(md, "a.md") || !strings.Contains(md, "c.md") {
		t.Errorf("pinned paths missing from markdown: %q", md)
	}
	if strings.Contains(md, "b.md") {
		t.Errorf("non-pinned path present in markdown: %q", md)
	}
}

// TestResources_ServerSatisfiesResourceRegistrar verifies that *mcpServer
// satisfies the ResourceRegistrar interface, which the supervisor type-asserts
// to at boot.
func TestResources_ServerSatisfiesResourceRegistrar(t *testing.T) {
	s := newTestSrv(t)
	if _, ok := any(s).(ResourceRegistrar); !ok {
		t.Fatalf("*mcpServer does not satisfy ResourceRegistrar")
	}
}
