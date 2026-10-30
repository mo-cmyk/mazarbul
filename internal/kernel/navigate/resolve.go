package navigate

import (
	"context"
	"fmt"
	"strings"
)

// ResolveMode controls how ResolveDisplayName handles ambiguity.
type ResolveMode int

const (
	// ResolveStrict is used by MCP tools: ambiguity among multiple non-document
	// candidates returns ResolvedAmbiguous with a filled Candidates list so the
	// caller can surface an E_VALIDATION error naming the choices.
	ResolveStrict ResolveMode = iota
	// ResolveDangle is used by the document wikilink resolver: ambiguity also
	// returns ResolvedAmbiguous, but it is expected and the caller leaves the
	// link dangling rather than guessing. The mode value is a reminder in
	// call-sites that "dangle on ambiguity" is the intended policy, not an error.
	ResolveDangle
)

// ResolveResultStatus describes the outcome of a ResolveDisplayName call.
type ResolveResultStatus int

const (
	// ResolvedOK means exactly one winner was found.
	ResolvedOK ResolveResultStatus = iota
	// ResolvedAmbiguous means multiple candidates share the name with no
	// clear winner. Candidates is populated.
	ResolvedAmbiguous
	// ResolvedNotFound means no entity with that name exists in the store.
	ResolvedNotFound
	// ResolvedError means the resolution query itself failed (e.g. a transient
	// DB error). Err carries the cause. Distinct from ResolvedNotFound so a
	// caller can propagate a real failure instead of silently degrading to a
	// semantic / suggestion fallback as though the name simply did not exist.
	ResolvedError
)

// ResolveResult is the outcome of a ResolveDisplayName call.
type ResolveResult struct {
	// Status is the resolution outcome.
	Status ResolveResultStatus
	// UID is the resolved entity UID. Non-empty only when Status is ResolvedOK.
	UID string
	// Candidates is the list of entities that matched when Status is
	// ResolvedAmbiguous. Capped at exactNameResolveCap.
	Candidates []Entity
	// ShadowedDocument is true when Status is ResolvedOK and a document entity
	// with the same display name exists but was passed over in favour of the
	// non-document winner. Callers may surface a note so the lookup is not
	// silently misleading.
	ShadowedDocument bool
	// ShadowedDocuments is the slice of document entities that share the winning
	// display name but were passed over in favour of a non-document winner.
	// Non-nil and non-empty exactly when ShadowedDocument is true. Provides the
	// identity (UID, DisplayName) for output annotation without an extra query —
	// the docs slice is already in hand at the decision point in PartitionAndResolve.
	// Callers that only need the bool signal are unaffected; callers that want to
	// surface the shadowed document's name/uid read this field.
	ShadowedDocuments []Entity
	// Err is the underlying failure when Status is ResolvedError; nil otherwise.
	Err error
}

// exactNameResolveCap is the maximum number of same-name entities the resolver
// inspects. It mirrors the cap in document/resolve.go: more entities than this
// overflows and returns ResolvedAmbiguous without further inspection. 50 is far
// above the realistic count of entities sharing a verbatim display name.
const exactNameResolveCap = 50

// kindResolutionPriority orders entity kinds for exact-name tie-breaking when
// multiple non-document canonical entities share a display name. Lower rank wins.
// Kinds absent from the table share the lowest priority (1<<30) and tie among
// themselves. This table is the single authoritative copy; document/resolve.go
// delegates to ResolveDisplayName so both surfaces share one policy.
//
//nolint:gochecknoglobals // small static priority table, read-only
var kindResolutionPriority = map[string]int{
	"person":       0,
	"organization": 1,
	"project":      2,
	"product":      3,
	"tool":         4,
	"team":         5,
	"event":        6,
	"place":        7,
	"topic":        8,
}

// preferByKind returns the single highest-priority entity among candidates and
// whether that choice is unique. Unique is false when two or more candidates
// share the top priority — a genuine same-kind ambiguity the caller leaves
// dangling. Candidates must be non-empty.
func preferByKind(candidates []Entity) (Entity, bool) {
	const unranked = 1 << 30
	rankOf := func(kind string) int {
		if r, ok := kindResolutionPriority[kind]; ok {
			return r
		}
		return unranked
	}
	best := candidates[0]
	bestRank := rankOf(best.Kind)
	tie := false
	for _, e := range candidates[1:] {
		switch er := rankOf(e.Kind); {
		case er < bestRank:
			best, bestRank, tie = e, er, false
		case er == bestRank:
			tie = true
		}
	}
	return best, !tie
}

// documentKind is the entity kind value for document entities. Duplicated here
// as a local literal to keep the navigate package free of an ontology import;
// the document layer uses ontology.EntityKindDocument directly.
const documentKind = "document"

// filterCanonicalDocs returns the subset of docs whose Canonical flag is true.
// Extracted from PartitionAndResolve so that function stays within the
// gocyclo complexity threshold.
func filterCanonicalDocs(docs []Entity) []Entity {
	var out []Entity
	for _, d := range docs {
		if d.Canonical {
			out = append(out, d)
		}
	}
	return out
}

// ResolveDisplayName maps name to exactly one entity using a three-lane strategy:
//
//  1. Exact display-name lane: queries ListEntities(EntityFilter{NameExact:name})
//     (or a kind-narrowed variant when kindHint is non-empty), partitions results
//     into non-document canonical entities and document namesakes, and applies the
//     kind-priority tie-break for multi-non-document same-kind cases.
//  2. Alias lane: when the exact display-name lane returns ResolvedNotFound and no
//     kindHint is given, queries ListEntities(EntityFilter{AliasExact:name}) against
//     the entity_aliases projection. The same PartitionAndResolve logic and
//     ambiguity semantics apply: a unique alias resolves deterministically; multiple
//     alias matches return ResolvedAmbiguous.
//  3. Caller falls through to semantic/fuzzy resolution when both exact lanes return
//     ResolvedNotFound.
//
// When kindHint is non-empty the query is narrowed to entities of that kind
// (EntityFilter.Kind), bypassing the partition logic and returning a simple
// unique-vs-ambiguous result within that kind. The alias lane is not consulted
// when kindHint is set (kind-scoped callers are precise enough that an alias
// fallback would introduce surprising results).
//
// The mode parameter controls the caller's policy on ambiguity:
//   - ResolveStrict: callers (MCP tools) want candidates listed so they can
//     surface an E_VALIDATION error.
//   - ResolveDangle: callers (wikilink resolver) want to leave the link dangling;
//     the returned ResolveResult still carries ResolvedAmbiguous so the caller
//     can act on it, but no error is signalled.
//
// Resolution rules (when kindHint is empty):
//  1. Overflow (>exactNameResolveCap same-name entities): ResolvedAmbiguous.
//  2. Exactly one non-document entity: ResolvedOK; ShadowedDocument is true when
//     any document namesake was present.
//  3. Multiple non-document entities of different priorities: the
//     highest-priority kind wins (preferByKind). ShadowedDocument is set if any
//     doc was also present.
//  4. Multiple non-document entities tied at the same priority: ResolvedAmbiguous.
//  5. No non-document entities, exactly one document: ResolvedOK (the document
//     entity wins).
//  6. No non-document entities, multiple documents: ResolvedAmbiguous.
//  7. No candidates at all (exact lane): try alias lane, same rules.
//  8. No candidates in either lane: ResolvedNotFound.
func (n *Navigator) ResolveDisplayName(ctx context.Context, name string, _ ResolveMode, kindHint string) ResolveResult {
	name = strings.TrimSpace(name)
	if name == "" {
		return ResolveResult{Status: ResolvedNotFound}
	}

	// When a kind hint is given, narrow the query to that kind. No selfUID
	// exclusion applies here (MCP callers supply no selfUID); just check
	// uniqueness within the requested kind. The alias lane is skipped for
	// kind-hinted calls (the caller is precise enough).
	if kindHint != "" {
		ents, err := n.ListEntities(ctx, EntityFilter{
			NameExact: name,
			Kind:      kindHint,
			Limit:     exactNameResolveCap + 1,
		})
		if err != nil {
			return ResolveResult{Status: ResolvedError, Err: err}
		}
		overflowed := len(ents) > exactNameResolveCap
		if overflowed {
			ents = ents[:exactNameResolveCap]
		}
		return PartitionAndResolve(ents, "", overflowed)
	}

	// Exact display-name lane: fetch cap+1 so overflow is detectable, then
	// delegate all partition + tie-break logic to PartitionAndResolve.
	ents, err := n.ListEntities(ctx, EntityFilter{
		NameExact: name,
		Limit:     exactNameResolveCap + 1,
	})
	if err != nil {
		return ResolveResult{Status: ResolvedError, Err: err}
	}
	overflowed := len(ents) > exactNameResolveCap
	if overflowed {
		ents = ents[:exactNameResolveCap]
	}
	if result := PartitionAndResolve(ents, "", overflowed); result.Status != ResolvedNotFound {
		return result
	}

	// Alias lane: exact display-name found nothing; try entity_aliases.
	// Same cap + PartitionAndResolve logic so ambiguity semantics are uniform.
	aliasEnts, aerr := n.ListEntities(ctx, EntityFilter{
		AliasExact: name,
		Limit:      exactNameResolveCap + 1,
	})
	if aerr != nil {
		return ResolveResult{Status: ResolvedError, Err: aerr}
	}
	aliasOverflowed := len(aliasEnts) > exactNameResolveCap
	if aliasOverflowed {
		aliasEnts = aliasEnts[:exactNameResolveCap]
	}
	return PartitionAndResolve(aliasEnts, "", aliasOverflowed)
}

// PartitionAndResolve applies the kind-partition + priority tie-break to an
// already-fetched candidate slice, returning the same ResolveResult as
// ResolveDisplayName.
//
// The selfUID argument is excluded before partitioning — the document wikilink
// resolver uses it to exclude the importing document from its own exact-name
// results. Pass "" when no exclusion is needed (MCP callers). The overflowed
// flag must be true when the caller fetched cap+1 rows and the slice is full,
// which signals that uniqueness cannot be proved.
//
// This is the shared kernel for both ResolveDisplayName (which fetches
// candidates itself) and the document/resolve.go wikilink resolver (which
// pre-fetches into a batchReader-overlaid page and calls this directly, since
// it works through the Reader interface rather than *Navigator).
func PartitionAndResolve(ents []Entity, selfUID string, overflowed bool) ResolveResult {
	if overflowed {
		return ResolveResult{Status: ResolvedAmbiguous, Candidates: ents}
	}
	if len(ents) == 0 {
		return ResolveResult{Status: ResolvedNotFound}
	}

	var canonical, docs []Entity
	for _, e := range ents {
		switch {
		case e.UID == selfUID:
			// exclude the importing document from its own resolution
		case e.Kind == documentKind:
			docs = append(docs, e)
		default:
			canonical = append(canonical, e)
		}
	}

	// If everything was selfUID we have no candidates at all — not-found so the
	// caller falls through to the vault-path lane.
	if len(canonical) == 0 && len(docs) == 0 {
		return ResolveResult{Status: ResolvedNotFound}
	}

	// G5: a document that declares canonical:true in its frontmatter wins the
	// display-name race over any non-document entities of the same name. This
	// lets an intentionally-authored document opt out of being shadowed by a
	// later connector-imported entity. Non-canonical documents keep the existing
	// default (non-document entities win). filterCanonicalDocs carries the loop
	// so this function stays within the gocyclo threshold.
	if canonicalDocs := filterCanonicalDocs(docs); len(canonicalDocs) > 0 {
		if len(canonicalDocs) == 1 {
			return ResolveResult{Status: ResolvedOK, UID: canonicalDocs[0].UID}
		}
		// Multiple canonical documents share the name — rare but genuinely
		// ambiguous; surface all of them so the caller can report the conflict.
		return ResolveResult{Status: ResolvedAmbiguous, Candidates: canonicalDocs}
	}

	hasDocs := len(docs) > 0

	switch len(canonical) {
	case 0:
		switch len(docs) {
		case 1:
			return ResolveResult{Status: ResolvedOK, UID: docs[0].UID}
		default:
			return ResolveResult{Status: ResolvedAmbiguous, Candidates: docs}
		}
	case 1:
		return ResolveResult{Status: ResolvedOK, UID: canonical[0].UID, ShadowedDocument: hasDocs, ShadowedDocuments: docs}
	default:
		winner, unique := preferByKind(canonical)
		if unique {
			return ResolveResult{Status: ResolvedOK, UID: winner.UID, ShadowedDocument: hasDocs, ShadowedDocuments: docs}
		}
		return ResolveResult{Status: ResolvedAmbiguous, Candidates: canonical}
	}
}

// CandidateRefs renders up to five entities as a human-readable disambiguation
// string for error messages: "DisplayName (entities/{uid}, kind)". Exported so
// both navigate_common.go and external callers (document/resolve.go is in a
// different package) can produce the same format. Appends "(+N more)" when
// truncated.
func CandidateRefs(es []Entity) string {
	const maxShown = 5
	parts := make([]string, 0, maxShown+1)
	for i, e := range es {
		if i == maxShown {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(es)-maxShown))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (entities/%s, %s)", e.DisplayName, e.UID, e.Kind))
	}
	return strings.Join(parts, "; ")
}
