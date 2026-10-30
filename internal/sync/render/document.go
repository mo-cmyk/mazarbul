package render

// document.go renders vault documents into the git tree. A document is also an
// entity, so its mentions/links_to edges already render via the entity stub and
// entities.jsonl (kind=document). What only the document layer can produce is
// the VERBATIM body file, rendered/documents/<slug>.md, which is byte-identical
// to the editable vault's materialization of the same revision event — both are
// projections of DocumentRevision.Body, never normalized. That byte-identity is
// the contract that lets `maz diff document:<path>` show a real body diff.

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// writeDocumentDiffs appends per-document changelog bullets: a new document, a
// new revision of an existing one, or a tombstone.
func writeDocumentDiffs(buf *strings.Builder, state *GraphState, prior *GraphState) {
	for _, uid := range sortedKeys(state.Documents) {
		row := state.Documents[uid]
		if prior == nil {
			writeNewDocumentLine(buf, row)
			continue
		}
		priorRow, existed := prior.Documents[uid]
		if !existed {
			writeNewDocumentLine(buf, row)
			continue
		}
		writeDocumentDiffLine(buf, row, priorRow)
	}
}

func writeNewDocumentLine(buf *strings.Builder, row *DocumentRow) {
	if row.Deleted {
		fmt.Fprintf(buf, "- ⊗ document %s  %q (deleted)\n", row.UID, row.Path)
		return
	}
	fmt.Fprintf(buf, "- + document %s  %q\n", row.UID, row.Path)
}

func writeDocumentDiffLine(buf *strings.Builder, row, priorRow *DocumentRow) {
	if row.Deleted && !priorRow.Deleted {
		fmt.Fprintf(buf, "- ⊗ document %s  %q (deleted)\n", row.UID, row.Path)
		return
	}
	if !row.Deleted && row.RevisionUID != priorRow.RevisionUID {
		fmt.Fprintf(buf, "- ~ document %s  %q\n", row.UID, row.Path)
	}
}

// computeDocumentStats counts added / changed / deleted documents for the push
// summary, diffing head state against the prior render.
func computeDocumentStats(s *Stats, state *GraphState, prior *GraphState) {
	for _, uid := range sortedKeys(state.Documents) {
		row := state.Documents[uid]
		priorRow, existed := documentPriorRow(prior, uid)
		if !existed {
			if row.Deleted {
				s.DocumentsDeleted++
			} else {
				s.DocumentsAdded++
			}
			continue
		}
		switch {
		case row.Deleted && !priorRow.Deleted:
			s.DocumentsDeleted++
		case !row.Deleted && row.RevisionUID != priorRow.RevisionUID:
			s.DocumentsChanged++
		}
	}
}

// documentPriorRow returns the prior render's row for uid, or (nil, false) when
// there was no prior state or the document is new this pass.
func documentPriorRow(prior *GraphState, uid string) (*DocumentRow, bool) {
	if prior == nil {
		return nil, false
	}
	row, ok := prior.Documents[uid]
	return row, ok
}

// documentsDir is the rendered subdirectory holding verbatim document bodies.
const documentsDir = "documents"

// applyDocumentRevision folds one documentRevision event into head state. Events
// arrive ULID-ordered, so the latest revision for a document wins; an earlier
// tombstone is reversed when a newer revision re-creates the document.
func (g *GraphState) applyDocumentRevision(ev kernel.Event) (bool, error) {
	var payload ontology.DocumentRevision
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal documentRevision: %w", err)
	}

	uid := payload.DocumentUID
	if uid == "" {
		uid = ev.UID
	}
	// Freeze the slug at first sight so a later path change (moveDocument)
	// never relocates the rendered file and orphans its git history.
	if _, ok := g.DocSlugs[uid]; !ok {
		g.DocSlugs[uid] = slugify(docSlugBase(payload.Path, payload.Title), uid)
	}

	existing, exists := g.Documents[uid]
	if !exists {
		g.Documents[uid] = &DocumentRow{
			UID:         uid,
			Path:        payload.Path,
			Title:       payload.Title,
			Body:        payload.Body,
			ContentHash: payload.ContentHash,
			RevisionUID: ev.UID,
			Pinned:      payload.Pinned,
			Tags:        payload.Tags,
			CreateTime:  ev.CreateTime.UTC(),
			LastUpdated: ev.CreateTime.UTC(),
		}
		g.Cursor = ev.UID
		return true, nil
	}

	existing.Path = payload.Path
	existing.Title = payload.Title
	existing.Body = payload.Body
	existing.ContentHash = payload.ContentHash
	existing.RevisionUID = ev.UID
	existing.Pinned = payload.Pinned
	existing.Tags = payload.Tags
	existing.LastUpdated = ev.CreateTime.UTC()
	existing.Deleted = false // a fresh revision un-tombstones the document
	g.Cursor = ev.UID
	return true, nil
}

// applyDocumentDeleted folds a soft-delete tombstone into head state. The row is
// retained (Deleted=true) so a later revision can deterministically revive it on
// replay; the rendered body file is dropped by collectDocumentFiles.
func (g *GraphState) applyDocumentDeleted(ev kernel.Event) (bool, error) {
	var payload ontology.DocumentDeleted
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal documentDeleted: %w", err)
	}
	row, exists := g.Documents[payload.DocumentUID]
	if !exists || row.Deleted {
		g.Cursor = ev.UID
		return false, nil
	}
	row.Deleted = true
	row.LastUpdated = ev.CreateTime.UTC()
	g.Cursor = ev.UID
	return true, nil
}

// docSlugBase chooses the human-readable slug source for a document: the path's
// base name without its markdown extension, falling back to the title when the
// path has no usable base. Slugify appends the UID suffix, so collisions across
// two documents with the same base name are impossible.
func docSlugBase(docPath, title string) string {
	base := path.Base(docPath)
	base = strings.TrimSuffix(base, ".md")
	base = strings.TrimSuffix(base, ".markdown")
	if b := strings.TrimSpace(base); b != "" && b != "." && b != "/" {
		return base
	}
	return title
}

// documentFilePath returns the rendered path for a document body file.
func documentFilePath(slug string) string {
	return renderedPath(documentsDir, slug+".md")
}

// collectDocumentFiles populates current with one verbatim body file per live
// document. Tombstoned documents are skipped, so a previously-rendered body is
// diffed to an ActionDelete and removed from the tree.
func collectDocumentFiles(current map[string][]byte, state *GraphState) {
	for _, uid := range sortedKeys(state.Documents) {
		row := state.Documents[uid]
		if row.Deleted {
			continue
		}
		slug := state.DocSlugs[uid]
		if slug == "" {
			slug = slugify(docSlugBase(row.Path, row.Title), uid)
		}
		current[documentFilePath(slug)] = []byte(row.Body)
	}
}
