package mcp

// resources.go registers the static MCP resources and the documents template
// on the underlying MCPServer. Resources are ADDITIVE — tools continue to work
// whether or not a client uses resources. The registration functions are called
// from the daemon wiring layer after the assembler and navigator are known.
//
// Static resources:
//   - mazarbul://context/hot     – token-budgeted brief (same as getContext)
//   - mazarbul://context/index   – complete vault directory (same as getIndex)
//   - mazarbul://context/pinned  – pinned documents listing
//
// Dynamic resource template:
//   - mazarbul://documents/{slug} – verbatim document body by vault slug/path

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// RegisterContextResources implements ResourceRegistrar. It installs the three
// static context resources (hot brief, vault index, pinned listing) and the
// documents/{slug} template on s. Called from the daemon after the assembler
// and navigator are available. If assembler or nav is nil, registration is
// silently skipped (degraded boot).
func (s *mcpServer) RegisterContextResources(assembler ContextAssembler, nav DocumentReader) {
	if assembler == nil || nav == nil {
		return
	}

	// --- static: mazarbul://context/hot ---
	hotResource := mcpgo.NewResource(
		"mazarbul://context/hot",
		"context/hot",
		mcpgo.WithResourceDescription("Token-budgeted context brief (hot.md): pinned documents, recently-updated documents, key entities. Same content as the getContext tool. Load at session start."),
		mcpgo.WithMIMEType("text/markdown"),
	)
	s.inner.AddResource(hotResource, func(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
		brief, err := assembler.Build(ctx, ContextBriefRequest{})
		if err != nil {
			return nil, fmt.Errorf("mazarbul://context/hot: %w", err)
		}
		return []mcpgo.ResourceContents{
			mcpgo.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "text/markdown",
				Text:     brief.Markdown,
			},
		}, nil
	})

	// --- static: mazarbul://context/index ---
	indexResource := mcpgo.NewResource(
		"mazarbul://context/index",
		"context/index",
		mcpgo.WithResourceDescription("Complete vault directory listing (index.md): all documents grouped by folder, recently-updated section, key-entity roll-up. NOT token-budget-gated. Same content as the getIndex tool."),
		mcpgo.WithMIMEType("text/markdown"),
	)
	s.inner.AddResource(indexResource, func(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
		idx, err := assembler.BuildIndex(ctx, ContextIndexRequest{})
		if err != nil {
			return nil, fmt.Errorf("mazarbul://context/index: %w", err)
		}
		return []mcpgo.ResourceContents{
			mcpgo.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "text/markdown",
				Text:     idx.Markdown,
			},
		}, nil
	})

	// --- static: mazarbul://context/pinned ---
	pinnedResource := mcpgo.NewResource(
		"mazarbul://context/pinned",
		"context/pinned",
		mcpgo.WithResourceDescription("Pinned vault documents: the subset of the vault whose frontmatter carries pinned: true. Use maz context pin <path> to promote a document."),
		mcpgo.WithMIMEType("text/markdown"),
	)
	s.inner.AddResource(pinnedResource, func(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
		docs, err := nav.ListDocuments(ctx, DocumentListOpts{PinnedOnly: true, Limit: -1})
		if err != nil {
			return nil, fmt.Errorf("mazarbul://context/pinned: %w", err)
		}
		text := buildPinnedMarkdown(docs)
		return []mcpgo.ResourceContents{
			mcpgo.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "text/markdown",
				Text:     text,
			},
		}, nil
	})

	// --- dynamic template: mazarbul://documents/{slug} ---
	docTemplate := mcpgo.NewResourceTemplate(
		"mazarbul://documents/{slug}",
		"documents",
		mcpgo.WithTemplateDescription("Verbatim document body by vault path (e.g. mazarbul://documents/notes/foo.md, as listDocuments returns) or by frozen slug (e.g. mazarbul://documents/foo, as the rendered tree uses). Returns the raw markdown body. Not found when no live document matches."),
		mcpgo.WithTemplateMIMEType("text/markdown"),
	)
	s.inner.AddResourceTemplate(docTemplate, func(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
		// Extract the slug by stripping the scheme+host prefix.
		// URI form: "mazarbul://documents/notes/foo.md" → slug = "notes/foo.md".
		const prefix = "mazarbul://documents/"
		uri := req.Params.URI
		if len(uri) <= len(prefix) {
			return nil, fmt.Errorf("mazarbul://documents/{slug}: empty slug in URI %q", uri)
		}
		slug := uri[len(prefix):]

		doc, err := resolveDocumentForResource(ctx, nav, slug)
		if errors.Is(err, ErrDocumentNotFound) {
			return nil, fmt.Errorf("document %q not found", slug)
		}
		if err != nil {
			return nil, fmt.Errorf("mazarbul://documents/%s: %w", slug, err)
		}
		return []mcpgo.ResourceContents{
			mcpgo.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "text/markdown",
				Text:     doc.Body,
			},
		}, nil
	})
}

// resolveDocumentForResource resolves the {slug} variable of the
// mazarbul://documents/{slug} resource, which may be EITHER a vault path
// (notes/foo.md, as listDocuments returns) OR a frozen slug (foo, as the
// rendered git tree and `maz doc ls` surface). The path lane is tried first —
// it is the primary handle and unique — then slug resolution is the fallback,
// so a caller that took the identifier from anywhere still resolves. Separated
// from the handler closure so it can be tested without a live transport.
func resolveDocumentForResource(ctx context.Context, nav DocumentReader, ref string) (DocumentRecord, error) {
	doc, err := nav.GetDocumentByPath(ctx, ref)
	if errors.Is(err, ErrDocumentNotFound) {
		return nav.GetDocumentBySlug(ctx, ref)
	}
	return doc, err
}

// buildPinnedMarkdown renders a minimal markdown listing of pinned documents.
// Separated from the handler so it can be tested without a live store.
func buildPinnedMarkdown(docs []DocumentRecord) string {
	if len(docs) == 0 {
		return "# Pinned Documents\n\n_No pinned documents._\n"
	}
	var b strings.Builder
	b.WriteString("# Pinned Documents\n\n")
	for i := range docs {
		title := docs[i].Title
		if title == "" {
			title = docs[i].Path
		}
		fmt.Fprintf(&b, "- [%s](%s)\n", title, docs[i].Path)
	}
	return b.String()
}
