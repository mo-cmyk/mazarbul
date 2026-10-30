package document

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// ReconcileForEntity is a targeted, cheap reconcile pass triggered when a new
// entity is created. It scans every live document whose write-time
// unresolved-link snapshot contains at least one [[Target]] or about: frontmatter
// target that case-insensitively matches one of the supplied names, then runs
// the full wikilink resolver on those documents only. Any link that now resolves
// produces a new relation event; duplicate edges are blocked by
// existingEdgeTargets so the call is idempotent.
//
// Names must include the entity's display name and all alternate names the caller
// wants to match. EntityUID is used solely for logging; the object of new edges
// is determined by the resolver. The relations table dedup handles duplicate
// prevention against any edge that was already committed.
//
// The call is CHEAP and BOUNDED:
//   - It fetches only documents with a non-empty unresolved-link snapshot.
//   - The in-memory name filter skips docs whose snapshot does not mention any of
//     the new names, so the resolver only runs on the relevant subset.
//   - It never does a full vault rescan or re-imports any document.
//
// It is dedup-safe: existingEdgeTargets cross-checks the relations table, so a
// reconcile edge is never appended twice even when the unresolved snapshot is
// stale (the snapshot is a write-time artifact that is intentionally never pruned
// -- see the LiveDeadLinkDocs comment for the rationale).
//
// Returns the number of new relation events emitted.
func (im *Importer) ReconcileForEntity(ctx context.Context, entityUID string, names []string) (int, error) {
	if len(names) == 0 {
		return 0, nil
	}

	nameSet := buildNameSet(names)

	docs, err := im.reader.DocumentsWithUnresolvedLinks(ctx)
	if err != nil {
		return 0, fmt.Errorf("reconcile for entity %q: list dangling docs: %w", entityUID, err)
	}

	var edgeEvents []kernel.Event
	for i := range docs {
		evs, err := im.reconcileDocForEntity(ctx, &docs[i], nameSet)
		if err != nil {
			return 0, err
		}
		edgeEvents = append(edgeEvents, evs...)
	}

	if len(edgeEvents) == 0 {
		return 0, nil
	}
	if _, err := im.appender.Append(ctx, edgeEvents); err != nil {
		return 0, fmt.Errorf("reconcile for entity %q: append edges: %w", entityUID, err)
	}
	return len(edgeEvents), nil
}

// reconcileDocForEntity processes one dangling-link document for
// ReconcileForEntity. It decodes the snapshot, skips the document when no
// target matches the name set, resolves the remaining links, and returns edge
// events for any resolved link not already present in the relations table.
func (im *Importer) reconcileDocForEntity(ctx context.Context, doc *navigate.UnresolvedLinkDoc, nameSet map[string]struct{}) ([]kernel.Event, error) {
	var links []ontology.WikiLink
	if uErr := json.Unmarshal([]byte(doc.UnresolvedJSON), &links); uErr != nil {
		im.log.Warn("Document: reconcile for entity decode unresolved links",
			zap.String("path", doc.Path), zap.Error(uErr))
		return nil, nil
	}

	// Pre-filter: skip this document unless at least one dangling target
	// name-matches one of the new entity's names. This makes the reconcile
	// pass O(matching docs) rather than O(all docs with any dangling link).
	if !anyLinkTargetMatches(links, nameSet) {
		return nil, nil
	}

	resolved, _ := resolveWikilinks(ctx, im.reader, im.snr, doc.DocumentUID, links)
	if len(resolved) == 0 {
		return nil, nil
	}

	existing, lerr := im.existingEdgeTargets(ctx, doc.DocumentUID)
	if lerr != nil {
		return nil, lerr
	}

	evs := make([]kernel.Event, 0, len(resolved))
	for _, link := range resolved {
		if _, dup := existing[link.predicate+"\x00"+link.objectUID]; dup {
			continue
		}
		existing[link.predicate+"\x00"+link.objectUID] = struct{}{}
		// The edge carries the reconciled document's OWN scope (read from its
		// row), not the newly-created entity's, so a personal doc made resolvable
		// by a team entity still emits a personal edge.
		ev, eErr := im.relationEvent(kernel.Scope(doc.Scope), doc.DocumentUID, link, doc.RevisionUID)
		if eErr != nil {
			return nil, eErr
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// buildNameSet folds each name with nameKey (lowercase NFC) into a set for
// O(1) target matching in ReconcileForEntity.
func buildNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[nameKey(n)] = struct{}{}
	}
	return set
}

// anyLinkTargetMatches reports whether at least one WikiLink target in links
// matches any key in the nameSet (case-insensitive NFC fold via nameKey).
func anyLinkTargetMatches(links []ontology.WikiLink, nameSet map[string]struct{}) bool {
	for _, l := range links {
		if _, ok := nameSet[nameKey(l.Target)]; ok {
			return true
		}
	}
	return false
}
