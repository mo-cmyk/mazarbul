package document

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode"

	"go.uber.org/zap"
	"golang.org/x/text/unicode/norm"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Predicate names for document-relation edge types.
//
// Body wikilinks default to one of two predicates based on the resolved
// entity's kind: a link whose target resolves to a document-kind entity
// becomes a links_to edge (doc to doc); a link to any other entity kind
// (person, organization, place, project, event) becomes a mentions edge
// (doc to entity). Both are non-functional: a document may link to or
// mention many targets.
//
// Frontmatter keys produce a separate predicate: the `about:` key expresses
// that this document describes its subject (a high-confidence authorial
// statement), while the `entity:` key anchors the document to a reference
// entity (for example "entity: self") and produces a mentions edge. The
// describes predicate is never emitted by body wikilinks or prose extraction.
const (
	predicateMentions  = "mentions"
	predicateLinksTo   = "links_to"
	predicateDescribes = "describes"
)

// selfEntityUID is the UID of the canonical seeded self-entity. Migration
// 0012_self_entity_seed.up.sql inserts it as the literal string "self". The
// frontmatter "entity: self" key and prose extractions that resolve to the
// self display name ("Me") both anchor here without a lookup.
//
// Intentionally duplicated from resolver.SelfEntityUID: importing the resolver
// package would add a heavy transitive dependency to the document layer.
// The MCP tool layer (internal/mcp/tools/relate.go) mirrors the same pattern.
const selfEntityUID = "self"

// documentPredicates is the closed set of relation predicates valid in typed
// wikilink syntax (`[relType] [[Target]]` / `[[relType::Target]]`) and for
// frontmatter keys. Only these strings are accepted as relType values. Any
// unknown relType in a document body falls back to the default
// mentions/links_to predicate (logged at debug by the importer). The core set
// (mentions, links_to, relates_to, contradicts, supports, blocks, depends_on,
// part_of) is seeded in migration 0019; describes is added in migration 0041.
//
//nolint:gochecknoglobals // process-lifetime constant set; content is fixed at compile time
var documentPredicates = map[string]struct{}{
	"mentions":    {},
	"links_to":    {},
	"relates_to":  {},
	"contradicts": {},
	"supports":    {},
	"blocks":      {},
	"depends_on":  {},
	"part_of":     {},
	"describes":   {},
}

// isKnownDocumentPredicate reports whether relType is in the closed set of
// registered document-relation predicates.
func isKnownDocumentPredicate(relType string) bool {
	_, ok := documentPredicates[relType]
	return ok
}

// resolvedLink is one wikilink the importer resolved to a canonical target.
type resolvedLink struct {
	objectUID  string
	predicate  string
	confidence float64 // stated confidence; 0 means "use default 1.0"
	raw        string  // the source [[...]] literal, for relation provenance
	// provenance is the full free-text origin label stamped on the relation
	// row. Empty means "derive from raw" (a deterministic wikilink/frontmatter
	// edge → "wikilink <raw>"). Prose-extracted edges set this explicitly to
	// proseProvenancePrefix+name so the deterministic forget pass can tell them
	// apart and never delete them (see forgottenEdgeEvents).
	provenance string
}

// proseProvenancePrefix marks a relation row as produced by async LLM prose
// extraction rather than a deterministic body wikilink or frontmatter link.
// The deterministic forget pass keys off this prefix so it never deletes a
// prose-derived edge (which it cannot re-derive synchronously). It is also the
// honest provenance label: a prose edge is NOT a wikilink.
const proseProvenancePrefix = "prose-extraction "

// nameKey folds a display name for case- and encoding-insensitive comparison:
// NFC unifies decomposed graphemes (the same normalization resolveTargetStatus
// applies before matching) and lowercasing makes it case-insensitive. Used to
// decide whether an async prose mention names the same entity an explicit
// wikilink has already claimed in a document, so the explicit link wins.
func nameKey(s string) string {
	return strings.ToLower(norm.NFC.String(strings.TrimSpace(s)))
}

// resolveWikilinks splits a document's wikilinks into the edges that resolve to
// a canonical entity/document and the remainder that stay dangling. Resolution
// runs four lanes in order:
//
//  1. Exact case-insensitive display-name match (deterministic, no I/O beyond
//     the SQLite reader pool).
//  2. Vault path match — [[notes/foo]] style links that name a file.
//  3. Unique whole-word partial-name match — [[Rosie]] → "Rosie Cotton" when
//     exactly one entity has the target as a contiguous whole-word run in its
//     display name (deterministic, embedder-free; ambiguous matches stay
//     dangling rather than guessing). See uniquePartialNameMatch.
//  4. Semantic name-embedding (optional, requires a SemanticNameResolver wired
//     via Importer.WithSemanticNameResolver): embeds the target string and
//     searches entities_vec WHERE vector_type='name'. Only fires when snr is
//     non-nil and lanes 1-3 all missed.
//
// The semantic lane is kind-agnostic — a wikilink may name any entity kind.
// It is conservative: only resolves when the top candidate's cosine similarity
// is above the threshold AND it clearly beats the runner-up by a margin (both
// gates are enforced inside the SemanticNameResolver implementation). When
// neither gate passes the link remains in the unresolved slice.
//
// SelfUID is the importing document's own UID; a self-referential link resolves
// to nothing rather than a self-edge. Resolved links are deduped by
// (objectUID, predicate) so repeated mentions of the same target yield one edge.
//
// When a WikiLink carries a RelType from the typed prefix form
// (`[relType:conf] [[Target]]` or `[[relType::Target]]`) AND the relType is in
// the closed set of registered document predicates, the resolved edge uses that
// predicate and the stated confidence. Unknown relTypes fall back to the
// default mentions/links_to predicate; the caller logs the fallback at debug.
func resolveWikilinks(ctx context.Context, r Reader, snr SemanticNameResolver, selfUID string, links []ontology.WikiLink) (resolved []resolvedLink, unresolved []ontology.WikiLink) {
	seen := make(map[string]struct{})
	for _, wl := range links {
		uid, defaultPredicate, ok, isSemanticHit := resolveTargetWithSemantic(ctx, r, snr, selfUID, wl.Target)
		if !ok {
			unresolved = append(unresolved, wl)
			continue
		}
		predicate := defaultPredicate
		if wl.RelType != "" && isKnownDocumentPredicate(wl.RelType) {
			predicate = wl.RelType
		}
		key := predicate + "\x00" + uid
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		provenance := ""
		if isSemanticHit {
			provenance = formatSemanticProvenance(wl.Raw)
		}
		resolved = append(resolved, resolvedLink{
			objectUID:  uid,
			predicate:  predicate,
			confidence: wl.Confidence,
			raw:        wl.Raw,
			provenance: provenance,
		})
	}
	return resolved, unresolved
}

// dedupResolvedLinks removes duplicate (predicate, objectUID) pairs from a
// slice of resolved links, preserving the first occurrence. Used after merging
// wikilink and frontmatter-link resolution results so a target named in both
// the body and frontmatter produces exactly one edge.
func dedupResolvedLinks(links []resolvedLink) []resolvedLink {
	if len(links) == 0 {
		return links
	}
	seen := make(map[string]struct{}, len(links))
	out := links[:0]
	for _, l := range links {
		key := l.predicate + "\x00" + l.objectUID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, l)
	}
	return out
}

// resolveTargetWithSemantic is resolveTarget extended with an optional semantic
// lane. It returns (uid, predicate, ok, isSemanticHit). The fourth return value
// is true only when the match came from the SemanticNameResolver lane (not from
// any deterministic lane), so the caller can stamp a distinct provenance on the
// resulting edge.
//
// Resolution order:
//  1. Exact case-insensitive display-name match — from resolveTarget.
//  2. Vault path match — from resolveTarget.
//  3. Unique whole-word partial-name match — from resolveTarget.
//  4. Semantic name-embedding via snr — fires only when snr is non-nil and the
//     deterministic lanes (1-3, all inside resolveTarget) missed.
//
// An AMBIGUOUS deterministic match (multiple exact-name or whole-word
// candidates) blocks the semantic lane entirely: the deterministic lanes
// already proved the target names more than one entity, so a confident semantic
// top hit must not silently pick one. Only an outright miss (no deterministic
// candidate at all) falls through to the semantic lane.
//
// When snr is nil the function is identical to resolveTarget.
func resolveTargetWithSemantic(ctx context.Context, r Reader, snr SemanticNameResolver, selfUID, target string) (uid, predicate string, ok, isSemanticHit bool) {
	u, p, resolved, status := resolveTargetStatus(ctx, r, selfUID, target)
	if resolved {
		return u, p, true, false
	}
	// A deterministic lane proved the target is ambiguous; never let the
	// semantic lane override that "stay dangling" decision with a guess.
	if status == partialAmbiguous {
		return "", "", false, false
	}
	// Semantic lane: only fires as a fallback.
	if snr == nil {
		return "", "", false, false
	}
	semUID, semKind, semOK := snr.ResolveNameSemantically(ctx, target, selfUID)
	if !semOK {
		return "", "", false, false
	}
	return semUID, predicateForKind(semKind), true, true
}

// resolveFrontmatterLinks resolves the WikiLinks produced by parseFrontmatterLinks
// into concrete edges. It differs from resolveWikilinks in two ways:
//
//  1. The special target "self" resolves directly to selfEntityUID without a
//     lookup — it is the seeded canonical self-entity that the first-person
//     self-statement detector also targets.
//  2. The predicate for each frontmatter link is taken from wl.RelType (set by
//     parseFrontmatterLinks): `about:` links carry predicateDescribes and
//     `entity:` links carry predicateMentions. Frontmatter links never produce
//     links_to edges regardless of the resolved entity's kind.
//
// Unresolvable targets are returned in the unresolved slice so they can join the
// dangling-link snapshot and the reconcile pass exactly like body wikilinks.
// Self-links (the document targeting itself via "entity: self" is not a
// self-link in the structural sense — "self" is the user entity, not the doc)
// are never suppressed here; the selfUID guard only suppresses doc-to-doc self-loops.
// RebindFrontmatterSelf maps the "self" frontmatter target to the concrete
// principal UID when the device is bound, else to the seeded self-entity
// literal (pre-identity behavior).
func rebindFrontmatterSelf(ctx context.Context, binder SelfBinder) string {
	if binder != nil {
		if principalUID, ok := binder.SelfPrincipal(ctx); ok {
			return principalUID
		}
	}
	return selfEntityUID
}

func resolveFrontmatterLinks(ctx context.Context, r Reader, binder SelfBinder, selfUID string, links []ontology.WikiLink) (resolved []resolvedLink, unresolved []ontology.WikiLink) {
	seen := make(map[string]struct{})
	for _, wl := range links {
		var uid string
		// The literal target "self" maps to the seeded self-entity, then rebinds
		// to the concrete principal UID when the device is bound so the synced
		// attribution references the shareable principal.
		if strings.EqualFold(wl.Target, "self") {
			uid = rebindFrontmatterSelf(ctx, binder)
		} else {
			resolvedUID, _, ok := resolveTarget(ctx, r, selfUID, wl.Target)
			if !ok {
				unresolved = append(unresolved, wl)
				continue
			}
			uid = resolvedUID
		}
		// Suppress a document linking to itself (e.g. a document with UID ==
		// selfEntityUID, which cannot happen today but is worth guarding).
		if uid == selfUID {
			continue
		}
		// Use the predicate recorded by parseFrontmatterLinks: describes for
		// about: links, mentions for entity: links.
		predicate := wl.RelType
		if predicate == "" {
			predicate = predicateMentions
		}
		key := predicate + "\x00" + uid
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		resolved = append(resolved, resolvedLink{
			objectUID:  uid,
			predicate:  predicate,
			confidence: wl.Confidence,
			raw:        wl.Raw,
		})
	}
	return resolved, unresolved
}

// LiveDeadLinkDocs re-resolves the write-time unresolved-link snapshots against
// the CURRENT projection and returns only the documents that still carry at
// least one genuinely-unresolvable wikilink.
//
// The documents table's unresolved_links_json is a write-time snapshot that the
// reconcile pass never prunes: once a later import makes a dangling link
// resolvable, reconcile emits the edge but leaves the stale snapshot in place
// (harmless for reconcile, which dedups against the relations table). Lint must
// not treat that stale snapshot as the live dead-link state, or `maz doc lint`
// would report a reconciled link as dead forever. This re-resolves each
// snapshot exactly like reconcile does, so a link that now resolves is dropped
// and only the truly-still-dangling links are reported.
//
// Resolution is read-only (no events emitted). When snr is non-nil,
// the semantic name-embedding lane is also consulted, so a link that was
// unresolvable at import time but is now satisfied by a semantically close
// entity is correctly excluded from the dead-link report.
//
// A doc whose snapshot fails to decode is reported as dead-linked (its raw
// snapshot is preserved) and logged, matching the conservative stance that an
// undecodable snapshot is not "clean".
//
// Each reported doc carries the specific link targets that remain dead after
// live re-resolution, so lint can name which [[wikilink]] to fix instead of
// only the file that contains it.
func LiveDeadLinkDocs(ctx context.Context, r Reader, snr SemanticNameResolver, log *zap.Logger) ([]DeadLinkDoc, error) {
	if log == nil {
		log = zap.NewNop()
	}
	docs, err := r.DocumentsWithUnresolvedLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("live dead-link docs: %w", err)
	}
	var out []DeadLinkDoc
	for _, doc := range docs {
		var links []ontology.WikiLink
		if uErr := json.Unmarshal([]byte(doc.UnresolvedJSON), &links); uErr != nil {
			log.Warn("document: lint decode unresolved links",
				zap.String("path", doc.Path), zap.Error(uErr))
			out = append(out, DeadLinkDoc{UnresolvedLinkDoc: doc})
			continue
		}
		if _, unresolved := resolveWikilinks(ctx, r, snr, doc.DocumentUID, links); len(unresolved) > 0 {
			targets := make([]string, 0, len(unresolved))
			for _, link := range unresolved {
				targets = append(targets, link.Target)
			}
			out = append(out, DeadLinkDoc{UnresolvedLinkDoc: doc, Unresolved: targets})
		}
	}
	return out, nil
}

// DeadLinkDoc is a document that still carries at least one genuinely
// unresolvable wikilink after live re-resolution, together with the specific
// link targets that remain dead. The targets are what make a lint report
// actionable — "doc X has dead link [[Ghost]]" — rather than only naming the
// file. Unresolved is empty when the doc's snapshot could not be decoded (the
// doc is still reported dead, but the offending targets are unknown).
type DeadLinkDoc struct {
	navigate.UnresolvedLinkDoc
	Unresolved []string
}

// resolveTarget maps one wikilink target string to (objectUID, predicate, ok).
// Order:
//
//  1. Exact case-insensitive display-name match across all entity kinds. A
//     unique match wins; an ambiguous match (>1 entity) is treated as
//     unresolved so the importer never silently picks the wrong node.
//  2. Vault path match: the target read as a document path (with and without a
//     trailing .md), for [[notes/foo]] style links that name a file.
//
// A document is itself an entity (display name = title), so a [[Title]] link to
// another note resolves in step 1 and is classified links_to by its kind.
func resolveTarget(ctx context.Context, r Reader, selfUID, target string) (string, string, bool) {
	uid, pred, ok, _ := resolveTargetStatus(ctx, r, selfUID, target)
	return uid, pred, ok
}

// resolveTargetStatus is resolveTarget with the partial-name lane's tri-state
// exposed. The fourth return value is the whole-word partial-name lane's
// status, which is only meaningful when ok is false: partialAmbiguous means the
// lane found multiple whole-word candidates (or could not prove uniqueness) and
// deliberately stayed dangling, so the semantic fallback must NOT override that
// decision. Deterministic hits (exact name, vault path, unique partial) report
// ok=true; in that case partialStatus carries no extra signal for the caller.
func resolveTargetStatus(ctx context.Context, r Reader, selfUID, target string) (uid, predicate string, ok bool, partialStatus partialNameStatus) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", false, partialMiss
	}
	// Normalise to NFC so a wikilink written with a decomposed grapheme (e.g.
	// "u"+combining-diaeresis) matches an entity stored in the composed form
	// ("ü"). This unifies invisible encoding variants only; it does NOT fold
	// accents away ("Wübben" stays distinct from "Wubben" — those are merged,
	// when wrong, via the updateEntity merge op, not silently here).
	target = norm.NFC.String(target)

	// The query asks for cap+1 rows so an overflow is detectable: a display
	// name shared by more entities than fit the page cannot be decided exactly
	// (a second canonical namesake could sit beyond the page), so it dangles
	// rather than risk a wrong guess. cap is generous — more than one entity
	// sharing a verbatim display name is already rare; more than cap is
	// pathological and an exact [[path]] link serves it better.
	ents, err := r.ListEntities(ctx, navigate.EntityFilter{NameExact: target, Limit: exactNameCandidateCap + 1})
	if err == nil {
		if uid, pred, ok, status, decided := resolveExactNameMatch(ents, selfUID); decided {
			return uid, pred, ok, status
		}
	}

	for _, candidate := range pathCandidates(target) {
		doc, derr := r.GetDocumentByPath(ctx, candidate)
		if derr == nil && doc.DocumentUID != selfUID {
			return doc.DocumentUID, predicateLinksTo, true, partialMiss
		}
	}

	// Unique whole-word partial-name match: a short-form link like [[Rosie]]
	// resolves to "Rosie Cotton" when exactly one entity has the target as a
	// contiguous run of whole words in its display name (case-insensitive).
	// This is deterministic and safe — it NEVER guesses between two candidates
	// (an ambiguous match stays dangling) and never matches a sub-word ("Sara"
	// does not match "Rosie") — so it handles first-names/nicknames that the
	// embedding lane's conservative cosine threshold (0.92) rejects, without
	// the false positives of simply lowering that threshold.
	uid, pred, status := uniquePartialNameMatch(ctx, r, selfUID, target)
	if status == partialUnique {
		return uid, pred, true, partialUnique
	}
	return "", "", false, status
}

// resolveExactNameMatch decides the exact-display-name lane from the candidate
// page (already fetched with Limit cap+1). Decided reports whether the lane
// reached a terminal verdict — resolved (ok=true) or deliberately dangling
// (ok=false, status=partialAmbiguous so the semantic lane stays blocked); when
// decided is false the caller falls through to the vault-path and partial-name
// lanes. SelfUID (the importing document) is excluded from the candidates.
//
// Delegates to navigate.PartitionAndResolve so document wikilinks and MCP
// tools share one kind-partition + priority tie-break policy. The
// wikilink-specific predicate assignment (mentions vs links_to) is applied here
// after the shared resolver returns its winner.
func resolveExactNameMatch(ents []navigate.Entity, selfUID string) (uid, predicate string, ok bool, status partialNameStatus, decided bool) {
	overflowed := len(ents) > exactNameCandidateCap
	if overflowed {
		return "", "", false, partialAmbiguous, true
	}
	r := navigate.PartitionAndResolve(ents, selfUID, false)
	switch r.Status {
	case navigate.ResolvedOK:
		// Assign the wikilink predicate based on the winning entity's kind.
		// predicateForKind takes a kind string — look it up from the fetched slice.
		pred := predicateMentions // safe default; overwritten below
		for i := range ents {
			if ents[i].UID == r.UID {
				pred = predicateForKind(ents[i].Kind)
				break
			}
		}
		return r.UID, pred, true, partialMiss, true
	case navigate.ResolvedAmbiguous:
		return "", "", false, partialAmbiguous, true
	default: // ResolvedNotFound — only self or empty; fall through to other lanes
		return "", "", false, partialMiss, false
	}
}

// partialNameStatus is the tri-state result of the whole-word partial-name
// lane. The caller distinguishes an outright miss (the semantic lane may try)
// from an ambiguous match (the lane found multiple whole-word candidates and
// the semantic lane must NOT override the lane's "stay dangling" decision).
type partialNameStatus int

const (
	partialMiss      partialNameStatus = iota // no whole-word candidate
	partialUnique                             // exactly one whole-word candidate
	partialAmbiguous                          // >1 whole-word candidate, or uniqueness unprovable
)

// exactNameCandidateCap bounds the exact-display-name candidate page. Resolving
// a bare name partitions the page into canonical (non-document) entities and
// document namesakes, so the page must hold every entity sharing the verbatim
// name to decide uniqueness; an overflow (more than cap rows) dangles rather
// than guess. 50 is far above the realistic count of entities with an identical
// display name.
const exactNameCandidateCap = 50

// partialCandidateCap bounds the leading-token candidate page. The query asks
// for cap+1 rows so a full page is detectable: when the prefilter itself
// overflows, a whole-word match (or an ambiguity-proving second match) could
// sit beyond the page, so uniqueness cannot be proven and the lane reports
// partialAmbiguous (stay dangling) rather than guessing. 200 is generous for a
// single leading token; legitimate vaults that exceed it are better served by
// an exact [[Full Name]] link than a bare first name.
const partialCandidateCap = 200

// uniquePartialNameMatch returns the single entity whose display name contains
// target as a contiguous whole-word run. It returns partialUnique with that
// entity, partialAmbiguous when more than one entity qualifies (or uniqueness
// cannot be proven because the candidate page overflowed), or partialMiss when
// none do. The importing document itself (selfUID) is excluded.
func uniquePartialNameMatch(ctx context.Context, r Reader, selfUID, target string) (string, string, partialNameStatus) {
	targetTokens := tokenizeName(target)
	if len(targetTokens) == 0 {
		return "", "", partialMiss
	}
	// Prefilter on the FIRST target token, not the raw target string. A token
	// is alphanumeric, so its substring appears in any display name that
	// tokenizes to include it regardless of the punctuation the name uses to
	// separate words — [[Apollo Launch]] must still retrieve "Apollo-Launch
	// Project". Filtering on the raw target (with its literal space/punctuation)
	// would miss token-equivalent names before containsWholeWordRun could judge
	// them. The whole-word check below is the precise filter; this only narrows
	// the candidate set.
	//
	// cap+1 rows make a full page detectable: if the leading-token prefilter
	// overflows, a qualifying match could sit beyond the page, so uniqueness is
	// unprovable and we conservatively report ambiguous (dangle, never guess).
	ents, err := r.ListEntities(ctx, navigate.EntityFilter{
		NameLike: targetTokens[0],
		Limit:    partialCandidateCap + 1,
	})
	if err != nil {
		return "", "", partialMiss
	}
	if len(ents) > partialCandidateCap {
		return "", "", partialAmbiguous // overflowed: cannot prove global uniqueness
	}
	var match navigate.Entity
	count := 0
	for i := range ents {
		if ents[i].UID == selfUID {
			continue
		}
		if !containsWholeWordRun(ents[i].DisplayName, targetTokens) {
			continue
		}
		count++
		if count > 1 {
			return "", "", partialAmbiguous // ambiguous — leave dangling rather than guess
		}
		match = ents[i]
	}
	if count == 1 {
		return match.UID, predicateForKind(match.Kind), partialUnique
	}
	return "", "", partialMiss
}

// containsWholeWordRun reports whether targetTokens appears as a contiguous run
// of whole words within name (case-insensitive). Tokenisation folds case and
// splits on any non-alphanumeric rune, so "Rosie" matches "Rosie Cotton" but not
// "Sarahs"; "apollo launch" matches "Apollo Launch Project".
func containsWholeWordRun(name string, targetTokens []string) bool {
	nameTokens := tokenizeName(name)
	if len(targetTokens) == 0 || len(targetTokens) > len(nameTokens) {
		return false
	}
	for i := 0; i+len(targetTokens) <= len(nameTokens); i++ {
		matched := true
		for j := range targetTokens {
			if nameTokens[i+j] != targetTokens[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// tokenizeName lowercases s and splits it into alphanumeric tokens, discarding
// punctuation and whitespace.
func tokenizeName(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// resolveTargetKindGated is resolveTarget with an additional kind constraint:
// the entity lookup is narrowed to entities whose kind equals the given kind
// string. This prevents a prose-extraction mention of "Berlin" (person) from
// resolving to a "Berlin" (place) and producing a spurious mentions edge.
//
// When kind is empty the call degrades to the kind-agnostic resolveTarget.
// The vault-path lane is skipped when kind is non-empty (a path match always
// resolves to a document entity, so it would only fire for an empty kind).
func resolveTargetKindGated(ctx context.Context, r Reader, selfUID, target, kind string) (string, string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", false
	}
	if kind == "" {
		return resolveTarget(ctx, r, selfUID, target)
	}
	ents, err := r.ListEntities(ctx, navigate.EntityFilter{NameExact: target, Kind: kind, Limit: 3})
	if err == nil {
		matches := ents[:0]
		for i := range ents {
			if ents[i].UID != selfUID {
				matches = append(matches, ents[i])
			}
		}
		if len(matches) == 1 {
			return matches[0].UID, predicateForKind(matches[0].Kind), true
		}
	}
	return "", "", false
}

// predicateForKind classifies an edge by the resolved entity's kind: a
// document target is a links_to edge, anything else is a mentions edge.
func predicateForKind(kind string) string {
	if kind == string(ontology.EntityKindDocument) {
		return predicateLinksTo
	}
	return predicateMentions
}

// pathCandidates returns the vault paths a target might name, normalised to
// forward slashes: the target verbatim and the target with a .md extension.
func pathCandidates(target string) []string {
	clean := strings.TrimSpace(strings.ReplaceAll(target, "\\", "/"))
	if clean == "" {
		return nil
	}
	cands := []string{clean}
	if path.Ext(clean) == "" {
		cands = append(cands, clean+".md")
	}
	return cands
}

// batchReader overlays an import batch's staged documents on the projected
// Reader so wikilink resolution sees the post-batch world before anything is
// appended: every staged doc is visible as a document entity (by display
// name) and as a document (by path). This is what lets the importer append
// entities, revisions, and edges in ONE atomic batch — resolution no longer
// depends on the batch's entity events being durable first.
//
// Only the two methods resolution uses are overlaid; EntityRelations and
// DocumentsWithUnresolvedLinks pass through via the embedded Reader (the
// staged batch carries no relations of its own).
type batchReader struct {
	Reader
	staged map[string]*pendingDoc   // document UID → staged doc
	byName map[string][]*pendingDoc // asciiLower(displayName) → staged docs
	byPath map[string]*pendingDoc   // vault-relative path → staged doc
}

// newBatchReader indexes the changed docs by UID, display name, and path.
func newBatchReader(r Reader, changed []pendingDoc) *batchReader {
	b := &batchReader{
		Reader: r,
		staged: make(map[string]*pendingDoc, len(changed)),
		byName: make(map[string][]*pendingDoc, len(changed)),
		byPath: make(map[string]*pendingDoc, len(changed)),
	}
	for i := range changed {
		pd := &changed[i]
		b.staged[pd.documentUID] = pd
		key := asciiLower(displayName(pd))
		b.byName[key] = append(b.byName[key], pd)
		b.byPath[pd.path] = pd
	}
	return b
}

// ListEntities merges the staged documents into a NameExact or NameLike page.
// Projected rows superseded by a staged doc are dropped first (the staged
// revision may retitle its document, so the projected display name is about to
// be stale), then staged docs whose display name matches the query are
// appended. Matching is ASCII case-insensitive, mirroring the navigator's
// COLLATE NOCASE / LIKE: NameExact requires equality, NameLike requires the
// query as a substring. The underlying query asks for opts.Limit + len(staged)
// rows so dropped rows cannot hide projected matches beyond the page, and the
// merged result is clamped back to opts.Limit so the caller's
// uniqueness/ambiguity arithmetic stays exact.
//
// Overlaying NameLike (not just NameExact) is what lets the partial-name
// wikilink lane see staged docs: an intra-batch [[Rosie]] -> staged "Rosie
// Jones" resolves against the post-batch world like every other lane.
//
//nolint:gocritic // hugeParam: EntityFilter is passed by value to satisfy the navigate.Reader interface (mirrors *Navigator.ListEntities).
func (b *batchReader) ListEntities(ctx context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	if opts.NameExact == "" && opts.NameLike == "" {
		return b.Reader.ListEntities(ctx, opts)
	}
	inner := opts
	if inner.Limit > 0 {
		inner.Limit += len(b.staged)
	}
	ents, err := b.Reader.ListEntities(ctx, inner)
	if err != nil {
		return nil, err
	}
	merged := make([]navigate.Entity, 0, len(ents))
	for _, e := range ents {
		if _, isStaged := b.staged[e.UID]; !isStaged {
			merged = append(merged, e)
		}
	}
	merged = append(merged, b.matchingStaged(opts)...)
	if opts.Limit > 0 && len(merged) > opts.Limit {
		merged = merged[:opts.Limit]
	}
	return merged, nil
}

// matchingStaged returns the staged docs whose display name satisfies opts,
// each as a document-kind entity. NameExact uses the byName index for an O(1)
// equality lookup; NameLike scans the staged set for an ASCII case-insensitive
// substring (the lane that drives partial-name resolution). NameExact takes
// precedence over NameLike, mirroring buildEntityWhere's switch.
//
//nolint:gocritic // hugeParam: mirrors the value-receiver EntityFilter contract.
func (b *batchReader) matchingStaged(opts navigate.EntityFilter) []navigate.Entity {
	stagedEntity := func(pd *pendingDoc) navigate.Entity {
		return navigate.Entity{
			UID:         pd.documentUID,
			Kind:        string(ontology.EntityKindDocument),
			DisplayName: displayName(pd),
		}
	}
	if opts.NameExact != "" {
		hits := b.byName[asciiLower(opts.NameExact)]
		out := make([]navigate.Entity, 0, len(hits))
		for _, pd := range hits {
			out = append(out, stagedEntity(pd))
		}
		return out
	}
	needle := asciiLower(opts.NameLike)
	var out []navigate.Entity
	for _, pds := range b.byName {
		for _, pd := range pds {
			if strings.Contains(asciiLower(displayName(pd)), needle) {
				out = append(out, stagedEntity(pd))
			}
		}
	}
	return out
}

// GetDocumentByPath serves staged documents first so [[path]]-style links to
// files in the same batch resolve; other paths fall through to the projection.
func (b *batchReader) GetDocumentByPath(ctx context.Context, path string) (navigate.Document, error) {
	if pd, ok := b.byPath[path]; ok {
		return navigate.Document{
			DocumentUID: pd.documentUID,
			Path:        pd.path,
			Title:       displayName(pd),
			ContentHash: pd.parsed.ContentHash,
		}, nil
	}
	return b.Reader.GetDocumentByPath(ctx, path)
}

// GetDocumentByUID serves staged documents first so intra-batch UID lookups
// resolve without a database round-trip; other UIDs fall through to the
// underlying Reader (the production Navigator or the test fake).
func (b *batchReader) GetDocumentByUID(ctx context.Context, uid string) (navigate.Document, error) {
	if pd, ok := b.staged[uid]; ok {
		return navigate.Document{
			DocumentUID: pd.documentUID,
			Path:        pd.path,
			Title:       displayName(pd),
			ContentHash: pd.parsed.ContentHash,
		}, nil
	}
	return b.Reader.GetDocumentByUID(ctx, uid)
}

// asciiLower folds A-Z to a-z and leaves every other byte untouched,
// mirroring SQLite's COLLATE NOCASE (which case-folds ASCII exclusively) so
// overlay name matching agrees byte-for-byte with the navigator's
// display_name comparison.
func asciiLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}
