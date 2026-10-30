package resolver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

const (
	// NamingCanonTemplateName is the prompt-registry name for the
	// LLM leverage point #8 naming canonicalisation template.
	namingCanonTemplateName = "naming-canon-v1"

	// NamingCanonTemplateVersion bumps when the prompt body changes.
	namingCanonTemplateVersion prompts.Version = 1

	// NamingCanonMinNames is the minimum number of distinct surface forms
	// (displayName + alternateNames) below which the resolver does not
	// invoke the LLM. Two forms already produce an interesting choice;
	// one form has nothing to canonicalise.
	namingCanonMinNames = 2

	// NamingCanonMaxTokens caps the model response. The output is a single
	// canonical name plus a tiny JSON envelope, so 128 tokens is generous.
	namingCanonMaxTokens = 128
)

// namingCanonSystemPrompt picks the most canonical surface form among a set
// of candidate names for an Entity. The model returns a JSON object with a
// single "canonical" string field.
const namingCanonSystemPrompt = `You pick the most canonical surface form for an entity.

INPUT
You are given an entity Kind and a JSON array of surface-form names that have
all been recorded for that entity. Names may differ in casing, spelling,
abbreviations, or completeness.

TASK
Pick the single most canonical, professional, and complete name from the list.
Prefer full names over initials, full company names over abbreviations, and
proper-cased forms over all-lowercase. Do not invent a name that is not in the
list; you must return exactly one of the input names verbatim.

OUTPUT
Return a JSON object of exactly this shape, with no commentary, no markdown,
and no surrounding text:
{"canonical": "<one of the input names, verbatim>"}

EXAMPLES

Kind: person
Names: ["F. Baggins", "Frodo Baggins", "frodo"]
Output: {"canonical": "Frodo Baggins"}

Kind: organization
Names: ["erebor", "Erebor Inc.", "Erebor"]
Output: {"canonical": "Erebor Inc."}

Kind: place
Names: ["nyc", "New York City", "New York"]
Output: {"canonical": "New York City"}

Now classify:
Kind: {{ .kind }}
Names: {{ .names }}`

// canonicalNameForEntity invokes the LLM to pick the most canonical surface
// form for the given entity from {entity.DisplayName, entity.AlternateNames}.
// On any failure (prompt render, provider error, malformed response, or a
// response that is not one of the input names) it returns the entity
// unchanged so callers can treat the result as best-effort.
//
// Called only when len(alternateNames) >= namingCanonMinNames - 1, so the
// resolver does not pay the LLM cost on the first Mention of an Entity.
func (r *Resolver) canonicalNameForEntity(ctx context.Context, entity ontology.Entity) ontology.Entity {
	if r.chat == nil || r.promptReg == nil {
		return entity
	}
	if entity.DisplayName == "" {
		return entity
	}
	names := append([]string{entity.DisplayName}, entity.AlternateNames...)
	if len(names) < namingCanonMinNames {
		return entity
	}

	namesJSON, err := json.Marshal(names)
	if err != nil {
		return entity
	}

	system, err := r.promptReg.Render(ctx, namingCanonTemplateName, namingCanonTemplateVersion, prompts.RenderVars{
		"kind":  string(entity.Kind),
		"names": string(namesJSON),
	})
	if err != nil {
		return entity
	}

	resp, err := r.chat.Complete(ctx, kernel.CompleteRequest{
		Model:  r.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: "Return JSON only."},
		},
		MaxTokens: namingCanonMaxTokens,
	})
	if err != nil {
		return entity
	}

	picked, ok := parseCanonicalName(resp.Content, names)
	if !ok {
		return entity
	}
	if picked == entity.DisplayName {
		return entity
	}

	previous := entity.DisplayName
	entity.DisplayName = picked
	entity.AlternateNames = mergeAlternateNames(entity.AlternateNames, previous, picked)
	return entity
}

// parseCanonicalName extracts the {"canonical": "..."} field from the LLM
// response and checks that the picked name is one of the input names.
// Returns (name, true) on success, ("", false) on any parse or membership
// failure.
func parseCanonicalName(raw string, allowed []string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var envelope struct {
		Canonical string `json:"canonical"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return "", false
	}
	for _, n := range allowed {
		if n == envelope.Canonical {
			return n, true
		}
	}
	return "", false
}

// mergeAlternateNames rebuilds the alternateNames slice so that the newly
// promoted canonical name (`picked`) is not duplicated and the previously
// promoted name (`previous`) is preserved as an alternate. Duplicates are
// removed in O(n).
func mergeAlternateNames(existing []string, previous, picked string) []string {
	seen := map[string]struct{}{picked: {}}
	out := make([]string, 0, len(existing)+1)
	if previous != "" && previous != picked {
		out = append(out, previous)
		seen[previous] = struct{}{}
	}
	for _, n := range existing {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
