package resolver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// DisambigVerdict is the LLM's answer in the gray-zone disambiguation call.
type DisambigVerdict string

const (
	// VerdictSame means the Mention and the candidate Entity refer to the same
	// real-world thing. The resolver merges them with resolutionStatus=confirmed.
	VerdictSame DisambigVerdict = "same"

	// VerdictDifferent means the Mention and the candidate Entity are clearly
	// distinct. The resolver creates a new Entity.
	VerdictDifferent DisambigVerdict = "different"

	// VerdictUnsure means the LLM cannot determine identity with confidence.
	// The resolver merges tentatively with resolutionStatus=tentative.
	VerdictUnsure DisambigVerdict = "unsure"
)

// disambiguateToolSchema is the JSON Schema passed to ChatProvider.ToolUse for
// the disambiguation call.
//
//nolint:gochecknoglobals // process-lifetime constant, never mutated
var disambiguateToolSchema = json.RawMessage(`{
	"type": "object",
	"required": ["verdict", "rationale"],
	"properties": {
		"verdict":   {"type": "string", "enum": ["same", "different", "unsure"]},
		"rationale": {"type": "string"}
	}
}`)

// llmDisambiguate calls ChatProvider.ToolUse with the disambiguate-v1 prompt
// to decide whether mention and candidate refer to the same real-world entity.
// Returns VerdictUnsure on parser errors (conservative degradation).
func (r *Resolver) llmDisambiguate(ctx context.Context, mention ontology.Mention, candidate ontology.Entity) (DisambigVerdict, error) {
	system, err := r.renderDisambiguatePrompt(ctx, mention, candidate)
	if err != nil {
		return VerdictUnsure, fmt.Errorf("disambiguate: render prompt: %w", err)
	}

	resp, err := r.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:  r.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: "Apply the disambiguation rules to the Mention and Entity above."},
		},
		Tool: kernel.ToolSchema{
			Name:        "disambiguate",
			Description: "Return a verdict on whether the Mention and Entity refer to the same real-world thing.",
			InputSchema: disambiguateToolSchema,
		},
		MaxTokens: r.cfg.DisambigMaxTokens,
	})
	if err != nil {
		return VerdictUnsure, fmt.Errorf("disambiguate: tool-use: %w", err)
	}

	if resp.ToolUse == nil {
		return VerdictUnsure, nil
	}

	var out struct {
		Verdict   string `json:"verdict"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal(resp.ToolUse.Input, &out); err != nil {
		// Conservative degradation: parse error yields unsure so the caller
		// falls back to a tentative merge rather than a hard failure.
		return VerdictUnsure, fmt.Errorf("disambiguate: parse tool output: %w", err)
	}

	switch DisambigVerdict(out.Verdict) {
	case VerdictSame:
		return VerdictSame, nil
	case VerdictDifferent:
		return VerdictDifferent, nil
	default:
		return VerdictUnsure, nil
	}
}
