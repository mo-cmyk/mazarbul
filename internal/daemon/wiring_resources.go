package daemon

// wiring_resources.go provides thin adapters that bridge the daemon's concrete
// types (*contextbrief.Assembler, *navigate.Navigator) to the mcp package's
// ContextAssembler and DocumentReader interfaces. This keeps the mcp package
// import-cycle-free: it declares the interfaces using its own value types, and
// the daemon — which already imports both contextbrief and navigate — owns the
// adaptation layer.

import (
	"context"
	"errors"

	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
	"github.com/mo-cmyk/mazarbul/internal/feedback"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// newInstructionComposer builds the per-session Channel A instructions composer
// (#214) over the brief assembler. The personal-inclusive ScopeSet is fixed here
// — the least-privilege span that still passes presetBriefReader's fail-closed
// gate (presets are personal-scoped operator config). The mcp.ComposeServerInstructions
// call prepends the static base manual + bootstrap pointer to whatever bounded,
// fenced preset section the assembler returns ("" when no preset is active → base
// + bootstrap only). It returns the plain func mcp.Options.InstructionComposer
// expects, keeping the mcp package free of any contextbrief import.
func newInstructionComposer(a *contextbrief.Assembler, fb feedback.Querier) func(context.Context) (string, error) {
	scopes := kernel.NewScopeSet(kernel.ScopePersonal)
	return func(ctx context.Context) (string, error) {
		section, err := a.OperatingInstructions(ctx, scopes)
		if err != nil {
			return "", err
		}
		// The dynamic lessons surface: recurring agent feedback auto-promotes into
		// a sanitized, capped "## Known gotchas & lessons" block placed LAST. A read
		// failure degrades to no gotchas — it must never break the instructions.
		gotchas := ""
		if fb != nil {
			if gs, gerr := feedback.ActiveGotchas(ctx, fb); gerr == nil {
				gotchas = feedback.RenderGotchasSection(gs)
			}
		}
		return mcp.ComposeServerInstructions(section, gotchas), nil
	}
}

// assemblerAdapter bridges *contextbrief.Assembler to mcp.ContextAssembler.
type assemblerAdapter struct{ a *contextbrief.Assembler }

// Build satisfies mcp.ContextAssembler using contextbrief.Request zero-mapped
// from mcp.ContextBriefRequest.
func (ad assemblerAdapter) Build(ctx context.Context, req mcp.ContextBriefRequest) (mcp.ContextBrief, error) {
	brief, err := ad.a.Build(ctx, contextbrief.Request{
		Query:        req.Query,
		BudgetTokens: req.BudgetTokens,
	})
	if err != nil {
		return mcp.ContextBrief{}, err
	}
	return mcp.ContextBrief{
		Markdown: brief.Markdown,
		Tokens:   brief.Tokens,
		Budget:   brief.Budget,
		Dropped:  brief.Dropped,
	}, nil
}

// BuildIndex satisfies mcp.ContextAssembler using contextbrief.IndexRequest
// zero-mapped from mcp.ContextIndexRequest.
func (ad assemblerAdapter) BuildIndex(ctx context.Context, req mcp.ContextIndexRequest) (mcp.ContextIndex, error) {
	idx, err := ad.a.BuildIndex(ctx, contextbrief.IndexRequest{
		RecentLimit: req.RecentLimit,
		EntityLimit: req.EntityLimit,
	})
	if err != nil {
		return mcp.ContextIndex{}, err
	}
	return mcp.ContextIndex{
		Markdown:  idx.Markdown,
		Documents: idx.Documents,
		Folders:   idx.Folders,
	}, nil
}

// navigatorAdapter bridges *navigate.Navigator to mcp.DocumentReader.
type navigatorAdapter struct{ n *navigate.Navigator }

// GetDocumentByPath satisfies mcp.DocumentReader, mapping navigate.ErrNotFound
// to mcp.ErrDocumentNotFound so the resource handler can distinguish a missing
// document from an internal storage error without importing navigate.
func (ad navigatorAdapter) GetDocumentByPath(ctx context.Context, path string) (mcp.DocumentRecord, error) {
	doc, err := ad.n.GetDocumentByPath(ctx, path)
	if errors.Is(err, navigate.ErrNotFound) {
		return mcp.DocumentRecord{}, mcp.ErrDocumentNotFound
	}
	if err != nil {
		return mcp.DocumentRecord{}, err
	}
	return mcp.DocumentRecord{
		DocumentUID: doc.DocumentUID,
		Path:        doc.Path,
		Title:       doc.Title,
		Body:        doc.Body,
		Pinned:      doc.Pinned,
		Scope:       doc.Scope,
	}, nil
}

// GetDocumentBySlug satisfies mcp.DocumentReader, mapping navigate.ErrNotFound
// to mcp.ErrDocumentNotFound. Mirrors GetDocumentByPath; the resource handler
// falls back to slug resolution when a path lookup misses.
func (ad navigatorAdapter) GetDocumentBySlug(ctx context.Context, slug string) (mcp.DocumentRecord, error) {
	doc, err := ad.n.GetDocumentBySlug(ctx, slug)
	if errors.Is(err, navigate.ErrNotFound) {
		return mcp.DocumentRecord{}, mcp.ErrDocumentNotFound
	}
	if err != nil {
		return mcp.DocumentRecord{}, err
	}
	return mcp.DocumentRecord{
		DocumentUID: doc.DocumentUID,
		Path:        doc.Path,
		Title:       doc.Title,
		Body:        doc.Body,
		Pinned:      doc.Pinned,
		Scope:       doc.Scope,
	}, nil
}

// ListDocuments satisfies mcp.DocumentReader, projecting navigate.DocumentFilter
// from mcp.DocumentListOpts.
func (ad navigatorAdapter) ListDocuments(ctx context.Context, opts mcp.DocumentListOpts) ([]mcp.DocumentRecord, error) {
	docs, err := ad.n.ListDocuments(ctx, navigate.DocumentFilter{
		PinnedOnly: opts.PinnedOnly,
		Limit:      opts.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]mcp.DocumentRecord, len(docs))
	for i := range docs {
		out[i] = mcp.DocumentRecord{
			DocumentUID: docs[i].DocumentUID,
			Path:        docs[i].Path,
			Title:       docs[i].Title,
			Body:        docs[i].Body,
			Pinned:      docs[i].Pinned,
			Scope:       docs[i].Scope,
		}
	}
	return out, nil
}
