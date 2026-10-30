package document

import (
	"context"

	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// ExtractorAdapter adapts *extractor.Extractor to the ProseExtractor interface
// used by Importer. It calls Extractor.Extract, which goes through the same
// chat-concurrency semaphore as episode extraction, so simultaneous
// document-prose extractions queue at the provider level rather than spawning
// unbounded Ollama requests.
//
// Construct with NewExtractorAdapter and pass to Importer.WithProseExtractor.
type ExtractorAdapter struct {
	ext *extractor.Extractor
}

// NewExtractorAdapter wraps a *extractor.Extractor as a ProseExtractor.
// The adapter is safe for concurrent use because *extractor.Extractor is.
func NewExtractorAdapter(e *extractor.Extractor) *ExtractorAdapter {
	return &ExtractorAdapter{ext: e}
}

// ExtractProse implements ProseExtractor. It calls the underlying
// Extractor.Extract and returns only the grounded Mention slice; relation
// candidates are discarded because the document layer emits its own
// document->entity "mentions" edges rather than entity->entity relations.
func (a *ExtractorAdapter) ExtractProse(ctx context.Context, docUID, text string) ([]ontology.Mention, error) {
	mentions, _, err := a.ext.Extract(ctx, docUID, text)
	return mentions, err
}

// Ensure ExtractorAdapter satisfies ProseExtractor at compile time.
var _ ProseExtractor = (*ExtractorAdapter)(nil)
