package navigate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// Document is a row of the documents projection (migration 0016). It is the
// read shape for the markdown-vault surface: readDocument / listDocuments /
// `maz doc show|ls`. The document's stable identity (DocumentUID) is also the
// UID of its EntityKindDocument entity row, so a document's graph edges are
// fetched with the same EntityRelations / Neighbors calls used for any entity —
// there is no separate "document graph".
//
// Body and Frontmatter are populated by the single-document getters and left
// empty by ListDocuments so a listing never pulls every body into memory.
type Document struct {
	DocumentUID string          `json:"documentUid"`
	Path        string          `json:"path"`
	Slug        string          `json:"slug,omitempty"`
	Title       string          `json:"title,omitempty"`
	Body        string          `json:"body,omitempty"`
	Frontmatter json.RawMessage `json:"frontmatter,omitempty"`
	ContentHash string          `json:"contentHash,omitempty"`
	RevisionUID string          `json:"revisionUid,omitempty"`
	Pinned      bool            `json:"pinned"`
	Tags        []string        `json:"tags,omitempty"`
	CreateTime  string          `json:"createTime,omitempty"`
	UpdateTime  string          `json:"updateTime,omitempty"`
	// Scope is the document's storage/sync boundary ("personal"|"project"|
	// "team"|"draft"), read from the documents.scope column by every getter and
	// lister. It is json:"-" for the same reason as Entity.Scope: the read shape
	// is embedded/mapped by tool wire types with strict (additionalProperties:
	// false) schemas, so a wire tag would leak the field and trip the schema
	// tests. It exists so the vault projector can partition the on-disk file by
	// scope (<root>/<scope>/<path>), the importer's dedup can be scope-exact, and
	// a delete/move can stamp the same scope on its tombstone/revision event.
	Scope string `json:"-"`
}

// DocumentFilter narrows ListDocuments. Zero value lists recent live documents.
type DocumentFilter struct {
	PathPrefix  string // case-sensitive prefix on the vault-relative path
	Tag         string // matches a value in the frontmatter `tags` array
	PinnedOnly  bool   // restrict to pinned documents
	Limit       int    // 0 → defaultLimit; negative → no cap
	IncludeBody bool   // select bodies too (vault projector); default omits them
}

// GetDocumentByUID returns the live document with document_uid, or ErrNotFound.
// A soft-deleted (tombstoned) document is reported as not found.
func (n *Navigator) GetDocumentByUID(ctx context.Context, uid string) (Document, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT document_uid, path, slug, title, body, frontmatter_json,
	                  content_hash, revision_uid, pinned, create_time, update_time, scope
	           FROM documents WHERE document_uid = ? AND deleted = 0 AND ` + scope
	return scanDocumentRow(n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...), fmt.Sprintf("document %q", uid))
}

// GetDocumentByPath returns the live document at path, or ErrNotFound. Path is
// the primary human handle (`maz doc show notes/foo.md`).
//
// Since migration 0055 replaced global `path UNIQUE` with UNIQUE(path, scope), a
// path can match one row per readable scope, so this spans the navigator's scope
// set and resolves deterministically (most-recently-updated wins, document_uid
// breaks ties) — mirroring GetDocumentBySlug. A caller that must address ONE
// exact partition (edit/move/delete, the vault projector) uses
// GetDocumentByPathScope instead.
func (n *Navigator) GetDocumentByPath(ctx context.Context, path string) (Document, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT document_uid, path, slug, title, body, frontmatter_json,
	                  content_hash, revision_uid, pinned, create_time, update_time, scope
	           FROM documents WHERE path = ? AND deleted = 0 AND ` + scope + `
	           ORDER BY update_time DESC, document_uid DESC LIMIT 1`
	return scanDocumentRow(n.reader.QueryRowContext(ctx, q, append([]any{path}, scopeArgs...)...), fmt.Sprintf("document %q", path))
}

// GetDocumentByPathScope returns the live document at path in EXACTLY scope, or
// ErrNotFound. Unlike GetDocumentByPath (which spans the navigator's scope set
// and can match one row per scope once (path, scope) replaced path uniqueness),
// this pins the exact partition — the identity the write/edit spine and the
// vault projector need to address a single row/file. It is fail-closed: a scope
// the navigator is not allowed to read yields ErrNotFound without a query.
func (n *Navigator) GetDocumentByPathScope(ctx context.Context, path, scope string) (Document, error) {
	if !n.scopes.Contains(kernel.Scope(scope)) {
		return Document{}, fmt.Errorf("document %q@%s: %w", path, scope, ErrNotFound)
	}
	q := `SELECT document_uid, path, slug, title, body, frontmatter_json,
	                  content_hash, revision_uid, pinned, create_time, update_time, scope
	           FROM documents WHERE path = ? AND scope = ? AND deleted = 0`
	return scanDocumentRow(
		n.reader.QueryRowContext(ctx, q, path, scope),
		fmt.Sprintf("document %q@%s", path, scope))
}

// GetDocumentBySlug returns the live document with the given frozen slug, or
// ErrNotFound. The slug is the URL-safe identifier derived from the filename at
// first sight and used in the rendered git tree (rendered/documents/<slug>.md)
// and the mazarbul://documents/{slug} resource. Slugs are NOT guaranteed unique
// — two files sharing a basename in different folders can collide — so the
// most-recently-updated match wins deterministically (document_uid breaks ties).
func (n *Navigator) GetDocumentBySlug(ctx context.Context, slug string) (Document, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT document_uid, path, slug, title, body, frontmatter_json,
	                  content_hash, revision_uid, pinned, create_time, update_time, scope
	           FROM documents WHERE slug = ? AND deleted = 0 AND ` + scope + `
	           ORDER BY update_time DESC, document_uid DESC LIMIT 1`
	return scanDocumentRow(n.reader.QueryRowContext(ctx, q, append([]any{slug}, scopeArgs...)...), fmt.Sprintf("document slug %q", slug))
}

// ListDocuments returns live documents matching opts, ordered by most-recently
// updated. Body is not selected (listings stay lightweight); call a getter for
// the full body.
func (n *Navigator) ListDocuments(ctx context.Context, opts DocumentFilter) ([]Document, error) {
	scope, scopeArgs := n.scopeCond("")
	conds := []string{"deleted = 0", scope}
	args := append([]any{}, scopeArgs...)
	if opts.PathPrefix != "" {
		conds = append(conds, "path LIKE ? ESCAPE '\\'")
		args = append(args, escapeLike(opts.PathPrefix)+"%")
	}
	if opts.PinnedOnly {
		conds = append(conds, "pinned = 1")
	}
	if opts.Tag != "" {
		// Match a value in the frontmatter `tags` JSON array. json_each over a
		// non-array or absent path simply yields no rows, so a document without
		// frontmatter tags is correctly excluded rather than erroring.
		conds = append(conds, `EXISTS (SELECT 1 FROM json_each(frontmatter_json, '$.tags') WHERE json_each.value = ?)`)
		args = append(args, opts.Tag)
	}

	limit := opts.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	var limitClause string
	if limit > 0 {
		limitClause = " LIMIT ?"
	}
	// Listings omit bodies by default so they never pull the whole vault into
	// memory; the vault projector opts in via IncludeBody to materialize files
	// from one query.
	bodyCol := `'' AS body`
	if opts.IncludeBody {
		bodyCol = `body`
	}
	// The clause fragments in conds are all compile-time constants (bodyCol is
	// chosen between two literals above); every runtime value is a bound ?
	// parameter in args, so this concatenation is not an injection vector.
	//nolint:gosec // G202: only constant clause fragments concatenated; values are parameterized
	q := `SELECT document_uid, path, slug, title, ` + bodyCol + `, frontmatter_json,
	             content_hash, revision_uid, pinned, create_time, update_time, scope
	      FROM documents WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY update_time DESC, document_uid DESC` + limitClause
	if limitClause != "" {
		args = append(args, limit)
	}

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		doc, scanErr := scanDocumentRow(rows, "list documents")
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return out, nil
}

// scanDocumentRow decodes a documents row in the canonical column order. The
// frontmatter `tags` array is decoded into Document.Tags; a NULL/empty
// frontmatter or a non-array tags value yields no tags rather than an error.
// Label is used to phrase the ErrNotFound message.
func scanDocumentRow(row rowScanner, label string) (Document, error) {
	var (
		d                      Document
		frontmatter            sql.NullString
		title, slug, hash, rev sql.NullString
		createTime, updateTime sql.NullString
		pinned                 int
	)
	err := row.Scan(
		&d.DocumentUID, &d.Path, &slug, &title, &d.Body, &frontmatter,
		&hash, &rev, &pinned, &createTime, &updateTime, &d.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, fmt.Errorf("%s: %w", label, ErrNotFound)
	}
	if err != nil {
		return Document{}, fmt.Errorf("%s: %w", label, err)
	}
	d.Slug = slug.String
	d.Title = title.String
	d.ContentHash = hash.String
	d.RevisionUID = rev.String
	d.CreateTime = createTime.String
	d.UpdateTime = updateTime.String
	d.Pinned = pinned != 0
	if frontmatter.Valid && frontmatter.String != "" && frontmatter.String != "{}" {
		d.Frontmatter = json.RawMessage(frontmatter.String)
		d.Tags = decodeFrontmatterTags(frontmatter.String)
	}
	return d, nil
}

// UnresolvedLinkDoc is the minimal shape the import reconcile pass needs: a
// document that still carries dangling wikilinks (its write-time
// unresolved_links_json snapshot is non-empty) plus the raw snapshot to decode.
// RevisionUID is the latest documentRevision event UID — reconciled relation
// events use it as SourceEventUID so their provenance walks back to a real
// event exactly like edges emitted at import time.
type UnresolvedLinkDoc struct {
	DocumentUID    string
	Path           string
	Title          string
	RevisionUID    string
	UnresolvedJSON string
	// Scope is the reconciling document's own scope. The reconcile pass stamps
	// it on the relation events it emits so a personal document newly resolving a
	// link never produces a team-scoped edge (and vice versa) — the edge inherits
	// the DOCUMENT's scope, not the triggering write's.
	Scope string
}

// DocumentsWithUnresolvedLinks returns every live document whose write-time
// unresolved-link snapshot is non-empty. The import reconcile pass re-resolves
// these against the now-larger entity/document set and emits the edges that
// have since become resolvable (it cross-checks the relations table to avoid
// duplicate edges, so a stale snapshot entry is harmless).
func (n *Navigator) DocumentsWithUnresolvedLinks(ctx context.Context) ([]UnresolvedLinkDoc, error) {
	scope, scopeArgs := n.scopeCond("")
	//nolint:gosec // G202: scope is constant fragments + bound ? params
	q := `SELECT document_uid, path, title, revision_uid, unresolved_links_json, scope
	           FROM documents
	           WHERE deleted = 0 AND unresolved_links_json NOT IN ('[]', '', 'null') AND ` + scope
	rows, err := n.reader.QueryContext(ctx, q, scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("documents with unresolved links: %w", err)
	}
	defer rows.Close()

	var out []UnresolvedLinkDoc
	for rows.Next() {
		var d UnresolvedLinkDoc
		var title, rev sql.NullString
		if err := rows.Scan(&d.DocumentUID, &d.Path, &title, &rev, &d.UnresolvedJSON, &d.Scope); err != nil {
			return nil, fmt.Errorf("scan unresolved-link doc: %w", err)
		}
		d.Title = title.String
		d.RevisionUID = rev.String
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("documents with unresolved links: %w", err)
	}
	return out, nil
}

// DocumentUnresolvedLinkTargets returns the target strings of one document's
// write-time unresolved-link snapshot ([] when none). The snapshot is recorded
// at import time, so a target that resolved via an alias is already absent (it
// became a real edge and was never snapshotted) — this is why it is a safer
// dangling-link signal than diffing body wikilinks against resolved edges by
// name. The reconcile pass does NOT rewrite the snapshot, so a target that was
// dangling at write time but has since become resolvable can still appear here;
// callers that also hold the document's resolved edges should suppress any
// target now satisfied by an edge (see readDocument).
func (n *Navigator) DocumentUnresolvedLinkTargets(ctx context.Context, docUID string) ([]string, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT unresolved_links_json FROM documents
	          WHERE document_uid = ? AND deleted = 0 AND ` + scope
	var raw sql.NullString
	if err := n.reader.QueryRowContext(ctx, q, append([]any{docUID}, scopeArgs...)...).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("document unresolved links %q: %w", docUID, err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "[]" || raw.String == "null" {
		return nil, nil
	}
	// Decode only the target field; the snapshot is an array of WikiLink objects.
	var links []struct {
		Target string `json:"target"`
	}
	if uErr := json.Unmarshal([]byte(raw.String), &links); uErr != nil {
		return nil, nil //nolint:nilerr // a malformed snapshot yields no targets rather than failing the read
	}
	out := make([]string, 0, len(links))
	for _, l := range links {
		if t := strings.TrimSpace(l.Target); t != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// DeletedDocuments returns the tombstoned documents (deleted = 1) with path +
// content hash. The vault projector uses these to remove materialized files
// whose document was deleted — and only when the on-disk bytes still match the
// last known revision, so a human edit made after the tombstone survives.
func (n *Navigator) DeletedDocuments(ctx context.Context) ([]Document, error) {
	scope, scopeArgs := n.scopeCond("")
	//nolint:gosec // G202: scope is constant fragments + bound ? params
	q := `SELECT document_uid, path, content_hash, scope FROM documents WHERE deleted = 1 AND ` + scope
	rows, err := n.reader.QueryContext(ctx, q, scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("deleted documents: %w", err)
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.DocumentUID, &d.Path, &d.ContentHash, &d.Scope); err != nil {
			return nil, fmt.Errorf("scan deleted document: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deleted documents: %w", err)
	}
	return out, nil
}

// LintOrphanDoc is one document that has no active inbound or outbound
// relations edges in the graph -- no wikilinks referencing it, no wikilinks
// it emits. Useful for `maz doc lint` to surface isolated documents.
type LintOrphanDoc struct {
	DocumentUID string `json:"documentUid"`
	Path        string `json:"path"`
	Title       string `json:"title,omitempty"`
}

// LintContradictionDoc is a pair of documents connected by an active
// "contradicts" edge. Returned by DocumentContradictions for `maz doc lint`.
type LintContradictionDoc struct {
	SubjectUID  string `json:"subjectUid"`
	SubjectPath string `json:"subjectPath"`
	ObjectUID   string `json:"objectUid"`
	ObjectPath  string `json:"objectPath"`
}

// OrphanDocuments returns live documents that have no active relation edges
// (neither inbound nor outbound) in the relations table. A document is an
// orphan when it has never been linked from or to any other node -- it sits
// isolated in the graph. Deleted documents are excluded.
func (n *Navigator) OrphanDocuments(ctx context.Context) ([]LintOrphanDoc, error) {
	// A document entity has document_uid = entity.uid. We join documents with
	// entities and left-join relations (either direction) to find documents
	// whose entity UID appears in neither the subject nor object column of
	// any active (non-superseded) relation.
	scope, scopeArgs := n.scopeCond("d")
	// The NOT EXISTS relation probe is scoped too: an out-of-scope inbound edge
	// must not mark an in-scope document non-orphan.
	scopeR, scopeArgsR := n.scopeCond("r")
	//nolint:gosec // G202: scope is constant fragments + bound ? params
	q := `
		SELECT d.document_uid, d.path, d.title
		FROM documents d
		WHERE d.deleted = 0 AND ` + scope + `
		  AND NOT EXISTS (
			  SELECT 1 FROM relations r
			  WHERE (r.subject_uid = d.document_uid OR r.object_uid = d.document_uid)
			    AND r.superseded_by_uid IS NULL
			    AND ` + scopeR + `
		  )
		ORDER BY d.path`
	rows, err := n.reader.QueryContext(ctx, q, append(append([]any{}, scopeArgs...), scopeArgsR...)...)
	if err != nil {
		return nil, fmt.Errorf("orphan documents: %w", err)
	}
	defer rows.Close()

	var out []LintOrphanDoc
	for rows.Next() {
		var d LintOrphanDoc
		var title sql.NullString
		if err := rows.Scan(&d.DocumentUID, &d.Path, &title); err != nil {
			return nil, fmt.Errorf("scan orphan document: %w", err)
		}
		d.Title = title.String
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orphan documents: %w", err)
	}
	return out, nil
}

// DocumentContradictions returns pairs of documents connected by an active
// "contradicts" relation edge. Each pair is reported once (subject → object
// direction as stored). Used by `maz doc lint` to surface conflict signals
// that may need human review.
func (n *Navigator) DocumentContradictions(ctx context.Context) ([]LintContradictionDoc, error) {
	// Both endpoints must be documents (join back to the documents table).
	// Scope every table the row is built from: the contradicts edge (r), the
	// subject document (ds), and the object document (do).
	scopeR, scopeArgsR := n.scopeCond("r")
	scopeDS, scopeArgsDS := n.scopeCond("ds")
	scopeDO, scopeArgsDO := n.scopeCond("do")
	scopeArgs := append(append(append([]any{}, scopeArgsR...), scopeArgsDS...), scopeArgsDO...)
	//nolint:gosec // G202: scope is constant fragments + bound ? params
	q := `
		SELECT r.subject_uid, ds.path, r.object_uid, do.path
		FROM relations r
		JOIN documents ds ON ds.document_uid = r.subject_uid AND ds.deleted = 0
		JOIN documents do ON do.document_uid = r.object_uid  AND do.deleted = 0
		WHERE r.predicate = 'contradicts'
		  AND r.superseded_by_uid IS NULL
		  AND ` + scopeR + `
		  AND ` + scopeDS + `
		  AND ` + scopeDO + `
		ORDER BY ds.path, do.path`
	rows, err := n.reader.QueryContext(ctx, q, scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("document contradictions: %w", err)
	}
	defer rows.Close()

	var out []LintContradictionDoc
	for rows.Next() {
		var c LintContradictionDoc
		if err := rows.Scan(&c.SubjectUID, &c.SubjectPath, &c.ObjectUID, &c.ObjectPath); err != nil {
			return nil, fmt.Errorf("scan contradiction: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("document contradictions: %w", err)
	}
	return out, nil
}

// LintDanglingEdge is an active link from a LIVE document to a now-DELETED
// document. After `deleteDocument` tombstones a note, the backlinks pointing AT
// it are left intact (the author wrote them; the system does not silently
// rewrite references on delete) — so they become dangling edges that point at a
// document that no longer exists. Returned by DanglingDocumentEdges for
// `maz doc lint`, this is what makes the deleteDocument contract true: a
// backlink to a deleted note is surfaced as a dead link the agent can re-point
// or remove, instead of resolving silently to a tombstone.
type LintDanglingEdge struct {
	SubjectUID  string `json:"subjectUid"`
	SubjectPath string `json:"subjectPath"`
	Predicate   string `json:"predicate"`
	TargetUID   string `json:"targetUid"`
	TargetPath  string `json:"targetPath"`
	TargetTitle string `json:"targetTitle,omitempty"`
}

// DanglingDocumentEdges returns CURRENT document→document links whose target
// document has been soft-deleted. The subject is a live document (deleted = 0)
// and the object is a tombstoned document (deleted = 1). Edges whose object is
// a non-document entity (e.g. a person) are excluded by the documents join.
//
// Only currently-current edges are reported. An edge is current truth when it
// is not superseded (superseded_by_uid unset), not yet temporally closed
// (valid_until unset or still in the future), AND already active (valid_from
// unset or in the past) — the exact predicate the relation reads use (see
// queryRelations). Gating valid_from matters: a future-dated edge is hidden by
// every other current-relation reader, so reporting it as dangling here would
// make `doc lint` fail on a link nothing else considers live. A forgotten edge
// is hard-deleted (applyRelationForgotten) so it has no row at all. The
// empty-string guards match rows written before a column was populated.
func (n *Navigator) DanglingDocumentEdges(ctx context.Context) ([]LintDanglingEdge, error) {
	// The predicate list mirrors the closed document-predicate set
	// (mentions/links_to plus the typed extensions); a dangling typed edge
	// (e.g. depends_on a deleted note) is just as broken as a links_to.
	// Scope every table the row is built from: the edge (r), the live subject
	// document (ds), and the tombstoned target document (do).
	scopeR, scopeArgsR := n.scopeCond("r")
	scopeDS, scopeArgsDS := n.scopeCond("ds")
	scopeDO, scopeArgsDO := n.scopeCond("do")
	//nolint:gosec // G202: scope is constant fragments + bound ? params; timestamps are parameterized
	q := `
		SELECT r.subject_uid, ds.path, r.predicate, r.object_uid, do.path, do.title
		FROM relations r
		JOIN documents ds ON ds.document_uid = r.subject_uid AND ds.deleted = 0
		JOIN documents do ON do.document_uid = r.object_uid  AND do.deleted = 1
		WHERE (r.superseded_by_uid IS NULL OR r.superseded_by_uid = '')
		  AND (r.valid_from IS NULL OR r.valid_from = '' OR julianday(r.valid_from) <= julianday(?))
		  AND (r.valid_until IS NULL OR r.valid_until = '' OR julianday(?) < julianday(r.valid_until))
		  AND r.predicate IN ('links_to','mentions','relates_to','contradicts','supports','blocks','depends_on','part_of')
		  AND ` + scopeR + `
		  AND ` + scopeDS + `
		  AND ` + scopeDO + `
		ORDER BY ds.path, do.path`
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	args := append([]any{nowStr, nowStr}, scopeArgsR...)
	args = append(args, scopeArgsDS...)
	args = append(args, scopeArgsDO...)
	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("dangling document edges: %w", err)
	}
	defer rows.Close()

	var out []LintDanglingEdge
	for rows.Next() {
		var e LintDanglingEdge
		var title sql.NullString
		if err := rows.Scan(&e.SubjectUID, &e.SubjectPath, &e.Predicate, &e.TargetUID, &e.TargetPath, &title); err != nil {
			return nil, fmt.Errorf("scan dangling edge: %w", err)
		}
		e.TargetTitle = title.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dangling document edges: %w", err)
	}
	return out, nil
}

// LintNearDuplicateDoc is a pair of documents whose chunk embeddings are
// suspiciously similar (cosine similarity >= a configured threshold). Returned
// by NearDuplicateDocuments for `maz doc lint` / the lintDocuments MCP tool.
// Only one entry per pair (lower-UID first); the caller never sees (A,B)+(B,A).
type LintNearDuplicateDoc struct {
	SubjectUID  string  `json:"subjectUid"`
	SubjectPath string  `json:"subjectPath"`
	ObjectUID   string  `json:"objectUid"`
	ObjectPath  string  `json:"objectPath"`
	Similarity  float32 `json:"similarity"`
}

// NearDuplicateSimilarityThreshold is the default cosine-similarity floor for
// near-duplicate document detection. Two documents whose most-similar chunk
// pair exceeds this threshold are surfaced as potential duplicates.
// Chosen at 0.92: high enough to suppress clearly-related-but-distinct notes
// (0.7-0.85) while reliably catching actual duplicates and near-verbatim copies
// (which typically score > 0.95). The threshold is configurable at call sites.
const NearDuplicateSimilarityThreshold = 0.92

// NearDuplicateDocuments returns pairs of live documents whose most-similar
// chunk embeddings exceed the given cosine-similarity threshold.
//
// Strategy: for each document, query documents_vec for its nearest-neighbor
// chunks (own-document chunks excluded), growing the KNN window adaptively until
// every above-threshold neighbor is captured (see nearDupChunkNeighbors). Using
// any-chunk-pair is the simplest correct approach: if ANY two chunks from two
// different documents are near-duplicates, the documents themselves are
// near-duplicates. Symmetric pairs are deduplicated by keeping the pair where the
// subject UID is lexically smaller than the object UID.
//
// The query uses the sqlite-vec KNN search (mirroring the dispatcher's episode
// dedup path). If no embeddings have been written (e.g. tests without an
// embedder), the query returns nothing and the function returns an empty slice.
// Detection is complete for any realistic vault; the only bound is nearDupMaxK
// near-identical copies of a single passage (see that constant).
func (n *Navigator) NearDuplicateDocuments(ctx context.Context, threshold float32) ([]LintNearDuplicateDoc, error) {
	// sqlite-vec does not support correlated per-row KNN, so we use a two-pass
	// approach: (1) collect all docs with embeddings, (2) for each doc query its
	// nearest neighbors among OTHER docs, (3) deduplicate symmetric pairs.
	docs, pathByUID, err := n.listDocsWithVec(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[[2]string]bool)
	var out []LintNearDuplicateDoc
	for _, doc := range docs {
		pairs, ferr := n.findNearDupPairs(ctx, doc.uid, doc.path, threshold, pathByUID, seen)
		if ferr != nil {
			return nil, ferr
		}
		out = append(out, pairs...)
	}
	return out, nil
}

// nearDupEntry is the minimal shape needed for near-duplicate iteration.
type nearDupEntry struct {
	uid  string
	path string
}

// listDocsWithVec collects all live documents that have at least one chunk
// embedding, returning the list and a UID-to-path lookup map.
func (n *Navigator) listDocsWithVec(ctx context.Context) ([]nearDupEntry, map[string]string, error) {
	scope, scopeArgs := n.scopeCond("d")
	//nolint:gosec // G202: scope is constant fragments + bound ? params
	q := `
		SELECT DISTINCT d.document_uid, d.path
		FROM documents d
		JOIN documents_vec_ids i ON i.document_uid = d.document_uid
		WHERE d.deleted = 0 AND ` + scope + `
		ORDER BY d.document_uid`
	rows, err := n.reader.QueryContext(ctx, q, scopeArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("near-duplicate documents: list docs: %w", err)
	}
	defer rows.Close()

	var docs []nearDupEntry
	for rows.Next() {
		var e nearDupEntry
		if err := rows.Scan(&e.uid, &e.path); err != nil {
			return nil, nil, fmt.Errorf("near-duplicate documents: scan doc: %w", err)
		}
		docs = append(docs, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("near-duplicate documents: iterate docs: %w", err)
	}
	pathByUID := make(map[string]string, len(docs))
	for _, d := range docs {
		pathByUID[d.uid] = d.path
	}
	return docs, pathByUID, nil
}

// nearDupInitialK is the first KNN window used per source chunk. Most chunks in
// a realistic vault have far fewer than this many neighbors above the similarity
// threshold, so a single query suffices for them.
const nearDupInitialK = 50

// nearDupMaxK caps the adaptive KNN growth so a pathologically duplicated vault
// (thousands of byte-identical chunks) cannot trigger an unbounded scan. The
// window only grows — doubling — while its farthest chunk is still above the
// threshold, i.e. only for chunks that genuinely have many near-duplicates. If
// this cap is reached while the window is still saturated, neighbor detection
// for that one chunk is bounded to its nearest nearDupMaxK; completeness then
// holds only up to nearDupMaxK near-identical copies of a single passage, far
// beyond any real vault.
const nearDupMaxK = 2048

// nearDupChunkNeighbors returns, for the chunk at rowid, the minimum distance to
// each OTHER document among that chunk's near neighbors. It grows the KNN window
// adaptively: starting at nearDupInitialK and doubling (up to nearDupMaxK) until
// the window's farthest chunk falls below the similarity threshold — so no
// above-threshold neighbor can lie beyond the window — or the corpus is
// exhausted. This finds every above-threshold cross-document neighbor in any
// realistic vault instead of silently capping at a fixed window (which omitted
// pairs once a cluster exceeded the cap).
//
// Query failures (e.g. no embedding written yet) are silently skipped so a
// partially-embedded vault does not abort the lint run.
func (n *Navigator) nearDupChunkNeighbors(ctx context.Context, rowid int64, docUID string, threshold float32) (map[string]float32, error) {
	// sqlite-vec MATCH with a rowid sub-select as the query vector. Own-document
	// chunks are filtered in Go (not in SQL) so the window boundary — the k-th
	// nearest chunk OVERALL — is observable; that boundary, not the farthest
	// cross-document chunk, is what proves the window captured every
	// above-threshold neighbor. Same MATCH pattern as the episode dedup
	// (checkSemanticDuplicate) and time-range search in vector.go.
	const knnQ = `
		SELECT ids.document_uid, v.distance
		FROM documents_vec v
		JOIN documents_vec_ids ids ON ids.rowid = v.rowid
		WHERE v.embedding MATCH (
			SELECT embedding FROM documents_vec WHERE rowid = ?
		)
		  AND k = ?
		ORDER BY v.distance`

	// Above-threshold cosine similarity maps to an L2-distance ceiling: for
	// L2-normalised vectors sim = 1 - d²/2 ≥ threshold ⇔ d ≤ sqrt(2(1-threshold)).
	// Once the window's farthest chunk exceeds this distance, every chunk beyond
	// it (all farther) is below the threshold, so the window is complete.
	t := float64(threshold)
	if t > 1 {
		t = 1
	}
	maxDist := math.Sqrt(2 * (1 - t))

	best := make(map[string]float32)
	for k := nearDupInitialK; ; {
		window, count, farthest, unavailable, err := n.queryNearDupWindow(ctx, knnQ, rowid, k, docUID)
		if err != nil {
			return nil, err
		}
		if unavailable {
			// Background embedding has not written this chunk yet; skip quietly.
			return nil, nil //nolint:nilnil // (nil, nil) = "no neighbors available yet, not an error"
		}
		for uid, dist := range window {
			if prev, ok := best[uid]; !ok || dist < prev {
				best[uid] = dist
			}
		}
		// Stop when the corpus is exhausted (fewer rows than asked), the window
		// boundary fell below the threshold (all above-threshold seen), or the
		// hard cap is reached.
		if count < k || float64(farthest) > maxDist || k >= nearDupMaxK {
			break
		}
		if k *= 2; k > nearDupMaxK {
			k = nearDupMaxK
		}
	}
	return best, nil
}

// queryNearDupWindow runs one KNN window query for a source chunk. It returns the
// per-other-document minimum distance (own-document chunks excluded), the total
// number of rows the window returned (to detect corpus exhaustion when it is < k),
// and the farthest (largest) distance in the window (to detect the threshold
// boundary). The unavailable result is true when the chunk has no embedding yet
// and the query could not run — the caller skips it without failing the lint.
func (n *Navigator) queryNearDupWindow(ctx context.Context, query string, rowid int64, k int, docUID string) (best map[string]float32, count int, farthest float32, unavailable bool, err error) {
	rows, qerr := n.reader.QueryContext(ctx, query, rowid, k)
	if qerr != nil {
		// A missing embedding (chunk not written yet) is not a lint failure;
		// signal unavailable so the caller skips this chunk.
		return nil, 0, 0, true, nil
	}
	defer rows.Close()
	best = make(map[string]float32)
	for rows.Next() {
		count++
		var (
			uid  string
			dist float32
		)
		if serr := rows.Scan(&uid, &dist); serr != nil {
			continue
		}
		if dist > farthest {
			farthest = dist
		}
		if uid == docUID {
			continue // own-document chunk: counts toward the window, not a pair
		}
		if prev, ok := best[uid]; !ok || dist < prev {
			best[uid] = dist
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, 0, 0, false, fmt.Errorf("near-duplicate documents: knn rows: %w", rerr)
	}
	return best, count, farthest, false, nil
}

// findNearDupPairs finds near-duplicate pairs for docUID against all other
// documents, appending new (deduped) pairs to out and marking seen pairs.
func (n *Navigator) findNearDupPairs(ctx context.Context, docUID, docPath string, threshold float32, pathByUID map[string]string, seen map[[2]string]bool) ([]LintNearDuplicateDoc, error) {
	rowids, err := n.docChunkRowids(ctx, docUID)
	if err != nil {
		return nil, err
	}
	bestDistByUID, err := n.aggregateBestDistances(ctx, docUID, rowids, threshold)
	if err != nil {
		return nil, err
	}
	return nearDupPairsFromDists(docUID, docPath, threshold, pathByUID, seen, bestDistByUID), nil
}

// aggregateBestDistances returns a map of neighborUID -> minimum distance
// across all own-document chunk queries. This is the first pass of the
// two-pass near-duplicate algorithm: collect nearest-neighbor candidates.
func (n *Navigator) aggregateBestDistances(ctx context.Context, docUID string, rowids []int64, threshold float32) (map[string]float32, error) {
	bestDistByUID := make(map[string]float32)
	for _, rowid := range rowids {
		neighbors, nerr := n.nearDupChunkNeighbors(ctx, rowid, docUID, threshold)
		if nerr != nil {
			return nil, nerr
		}
		for uid, dist := range neighbors {
			if prev, ok := bestDistByUID[uid]; !ok || dist < prev {
				bestDistByUID[uid] = dist
			}
		}
	}
	return bestDistByUID, nil
}

// nearDupPairsFromDists converts a best-distance map into LintNearDuplicateDoc
// pairs, applying threshold filtering, path lookup, and symmetric deduplication.
func nearDupPairsFromDists(docUID, docPath string, threshold float32, pathByUID map[string]string, seen map[[2]string]bool, bestDistByUID map[string]float32) []LintNearDuplicateDoc {
	out := make([]LintNearDuplicateDoc, 0, len(bestDistByUID))
	for neighborUID, dist := range bestDistByUID {
		// CosineSimilarityFromDistance: sim = 1 - d²/2 for L2-normalised vectors.
		sim := float32(1.0 - float64(dist)*float64(dist)/2.0)
		if sim < 0 {
			sim = 0
		}
		if sim < threshold {
			continue
		}
		neighborPath := pathByUID[neighborUID]
		if neighborPath == "" {
			continue // deleted or not in live-doc set
		}
		lo, hi := docUID, neighborUID
		loPath, hiPath := docPath, neighborPath
		if lo > hi {
			lo, hi = hi, lo
			loPath, hiPath = hiPath, loPath
		}
		key := [2]string{lo, hi}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, LintNearDuplicateDoc{
			SubjectUID:  lo,
			SubjectPath: loPath,
			ObjectUID:   hi,
			ObjectPath:  hiPath,
			Similarity:  sim,
		})
	}
	return out
}

// docChunkRowids returns the vec0 rowids of docUID's chunks that HAVE a stored
// embedding. The JOIN to documents_vec is load-bearing: a chunk whose background
// embedding is still pending (or never ran — no embedder configured, or the
// worker short-circuited on shutdown) has a documents_vec_ids sidecar row but no
// documents_vec vector. Using such a rowid as the KNN query vector makes
// sqlite-vec reject a NULL query vector and abort the whole lint; filtering to
// embedded chunks keeps near-duplicate detection resilient on a partially
// embedded vault (the documented contract) instead. An empty slice means no
// chunk of this document is embedded yet.
func (n *Navigator) docChunkRowids(ctx context.Context, docUID string) ([]int64, error) {
	const q = `
		SELECT i.rowid
		FROM documents_vec_ids i
		JOIN documents_vec v ON v.rowid = i.rowid
		WHERE i.document_uid = ?`
	rows, err := n.reader.QueryContext(ctx, q, docUID)
	if err != nil {
		return nil, fmt.Errorf("near-duplicate documents: chunk rowids for %q: %w", docUID, err)
	}
	defer rows.Close()
	var rowids []int64
	for rows.Next() {
		var rowid int64
		if serr := rows.Scan(&rowid); serr != nil {
			return nil, fmt.Errorf("near-duplicate documents: scan rowid: %w", serr)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("near-duplicate documents: iterate rowids: %w", err)
	}
	return rowids, nil
}

// decodeFrontmatterTags pulls a string `tags` array out of decoded frontmatter
// JSON. Returns nil for any shape that is not a JSON array of strings.
func decodeFrontmatterTags(frontmatterJSON string) []string {
	var obj struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(frontmatterJSON), &obj); err != nil {
		return nil
	}
	return obj.Tags
}
