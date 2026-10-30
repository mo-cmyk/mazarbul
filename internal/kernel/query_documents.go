package kernel

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/geo"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// docEventKind is the EventKind carried by document hits — the same kind as
// the documentRevision events that source them. Mirrors
// ontology.KindDocumentRevision without importing ontology (same convention
// as classEpisodic).
const docEventKind = EventKind("documentRevision")

// classDocument is the typeWeights key for document hits. Unlike episodes —
// where an unknown class falls back to the episodic multiplier — an absent
// document key applies NO multiplier: documents are durable curated context,
// not episodic chatter, so they must not inherit the episodic down-weight.
const classDocument = "document"

// docPredicateInList is the SQL IN-clause membership fragment over the full set
// of wikilink relation predicates a document edge can carry: the two default
// predicates (mentions/links_to) plus the typed wikilink set
// (relates_to/contradicts/supports/blocks/depends_on/part_of). It mirrors
// document.documentPredicates (resolve.go) without importing the document
// package — the same convention docEventKind uses for ontology.
//
// Every document graph-traversal query (graph lane, spatial geofence,
// seed-and-pivot anchor attach) MUST gate on this full set, not just the
// default two: a typed wikilink such as `[supports] [[Rosie]]` is stored under
// its typed predicate and would otherwise be invisible to the wikilink graph,
// breaking unified recall and dropping spatially-geofenced typed links.
const docPredicateInList = `('mentions', 'links_to', 'relates_to', 'contradicts', 'supports', 'blocks', 'depends_on', 'part_of')`

// maxDocEntityRefs caps the entity anchors attached per document hit. A hub
// note can carry hundreds of wikilinks; the recall shortlist needs pivot
// anchors, not the whole adjacency list (getBacklinks returns that).
const maxDocEntityRefs = 16

// docBodyTextCap bounds Hit.Text for document hits that did not win through
// the chunk-vec lane (FTS/graph-only): the body is truncated to this many
// bytes (at a rune boundary) so a recall response never drags a 100 KB
// document through the iterator. Chunk-lane winners carry the winning chunk's
// text instead. Matches the extractor's 6000-byte chunk threshold.
const docBodyTextCap = 6000

// documentFTSSearcher is the optional FTS capability backing the document
// lexical lane (SQLite: documents_fts, migration 0016). Probed via type
// assertion so FTS fakes without document support run recall with the
// document FTS lane disabled rather than failing.
type documentFTSSearcher interface {
	SearchDocuments(ctx context.Context, query string, k int) ([]FTSHit, error)
}

// documentLaneEnabled reports whether document candidates participate in this
// recall: by default (no Kinds filter) documents co-rank with episodes; an
// explicit Kinds filter includes them only when it names documentRevision.
func documentLaneEnabled(kinds []EventKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == docEventKind {
			return true
		}
	}
	return false
}

// docCandidate accumulates one document's per-lane evidence before scoring.
type docCandidate struct {
	uid        string
	vecRank    int     // 1-based doc rank in the chunk-vec lane; 0 = absent
	rawSem     float32 // cosine of the BEST (closest) chunk
	chunkIdx   int     // winning chunk index; -1 when the vec lane is absent
	ftsRank    int     // 1-based rank in the document FTS lane; 0 = absent
	rawBM25    float32 // sigmoid-normalized BM25; 0 when absent
	ftsSnippet string
	graphRank  int // 1-based rank in the wikilink-graph lane; 0 = absent
}

// docRow is a hydrated documents projection row.
type docRow struct {
	uid         string
	path        string
	title       string
	body        string
	revisionUID string
	updateTime  time.Time
}

// docChunkMeta is one documents_vec_ids sidecar row (heading + byte offset of
// a chunk), used to anchor citations to a section without re-chunking.
type docChunkMeta struct {
	index      int
	heading    string
	byteOffset int
}

// collectDocumentHits runs the document lanes — chunk-vec (documents_vec),
// lexical (documents_fts), and wikilink-graph (relations touching the query's
// seed entities) — and projects scored Hits that co-rank with episode hits in
// the caller's fusion pipeline. The scoring math mirrors the episode path
// exactly (same WeightedComponents / rrfComponents / semantic gate), so a
// document and an episode with equal evidence fuse to equal scores.
//
// The whole lane is gated on the documents projection actually existing in
// the wired reader (migration 0016): test harnesses and embedders with a
// minimal episode-only schema run recall unchanged instead of erroring.
//
// VecHits/ftsHits are the EPISODE lanes' results: the entities mentioned by
// the top episode candidates seed the document graph lane, so a note about
// Rosie surfaces for a Rosie-anchored query even without lexical overlap.
func (h *HybridQuery) collectDocumentHits(ctx context.Context, qvec []float32, req RecallRequest, vecHits []VectorHit, ftsHits []FTSHit) ([]Hit, error) {
	if !h.documentsProjectionAvailable(ctx) {
		return nil, nil
	}
	cands := make(map[string]*docCandidate)

	if err := h.docVecLane(ctx, qvec, cands); err != nil {
		return nil, err
	}
	if err := h.docFTSLane(ctx, req.Query, cands); err != nil {
		return nil, err
	}
	if !req.NoGraph {
		seeds, serr := h.entityUIDsForEpisodes(ctx, collectEpisodeUIDs(vecHits, ftsHits, h.candidateK))
		if serr != nil {
			return nil, fmt.Errorf("kernel: hybrid: document graph seeds: %w", serr)
		}
		if len(seeds) > 0 {
			if err := h.docGraphLane(ctx, seeds, req.At, req.AsOf, cands); err != nil {
				return nil, err
			}
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// Spatial filter: when Near is set, episode rows are narrowed to those
	// citing an entity within the radius (query_hybrid). Documents must obey
	// the same geofence — a note about an unrelated place must not slip into a
	// "near Berlin" recall and become answer context — so we drop every
	// candidate whose wikilink edges touch no in-radius entity.
	if req.Near != nil {
		if err := h.filterDocCandidatesByNear(ctx, cands, req.Near, req.At, req.AsOf); err != nil {
			return nil, err
		}
		if len(cands) == 0 {
			return nil, nil
		}
	}

	rows, err := h.hydrateDocRows(ctx, cands, req)
	if err != nil {
		return nil, err
	}
	chunkMeta, err := h.docChunkMetaFor(ctx, cands, rows)
	if err != nil {
		return nil, err
	}
	hits := h.scoreDocCandidates(cands, rows, chunkMeta)
	h.reinforceDocHits(ctx, req, hits)
	return hits, nil
}

// reinforceDocHits applies the passive-reinforcement multiplier (issue #231) to
// document hits over the shared memory_strength accumulator, so a frequently-
// recalled document outranks a stale equal-relevance peer — the same treatment
// episodes, bare entities, and derived memories get. A strength-fetch failure
// degrades to neutral multipliers (warn, never fail recall), matching the other
// lanes. Mutates hits in place.
func (h *HybridQuery) reinforceDocHits(ctx context.Context, req RecallRequest, hits []Hit) {
	if len(hits) == 0 {
		return
	}
	docUIDs := make([]string, len(hits))
	for i := range hits {
		docUIDs[i] = hits[i].UID
	}
	strengthByUID, serr := h.fetchStrength(ctx, docUIDs)
	if serr != nil {
		logging.FromContext(ctx).Warn("kernel: hybrid: document strength fetch failed; ranking without reinforcement", zap.Error(serr))
		return
	}
	anchor := recallAnchor(req)
	for i := range hits {
		mult, strength := h.strengthMultiplier(strengthByUID[hits[i].UID], anchor)
		hits[i].FusedScore *= mult
		hits[i].Components.Strength = strength
	}
}

// filterDocCandidatesByNear drops every document candidate that does not cite —
// through any wikilink edge (the full document predicate set; see
// docPredicateInList) valid at the requested time — at least one entity inside
// the Near radius. It is the document-side mirror of
// applySpatialFilter: place resolution is identical (haversine when the place
// entity has coordinates; a display-name substring fallback when it does not; an
// empty result when no place entity matches), only the citation surface differs
// (wikilink edges in the relations projection vs the mentions table).
//
// The at / asOf bounds thread the SAME bi-temporal window the document graph
// lane uses, so a historical Near recall geofences on the wikilink graph as it
// was valid then — a [[Berlin]] edge that was live at `at` but has since been
// superseded still keeps its document in-region.
func (h *HybridQuery) filterDocCandidatesByNear(ctx context.Context, cands map[string]*docCandidate, near *NearFilter, at, asOf *time.Time) error {
	docUIDs := make([]string, 0, len(cands))
	for uid := range cands {
		docUIDs = append(docUIDs, uid)
	}

	keep, err := h.docsCitingNearbyEntities(ctx, docUIDs, near, at, asOf)
	if err != nil {
		return err
	}
	for uid := range cands {
		if _, ok := keep[uid]; !ok {
			delete(cands, uid)
		}
	}
	return nil
}

// docsCitingNearbyEntities returns the subset of docUIDs whose wikilink edges
// touch an entity satisfying the Near filter. Resolution branches exactly like
// applySpatialFilter so episode and document geofencing agree.
func (h *HybridQuery) docsCitingNearbyEntities(ctx context.Context, docUIDs []string, near *NearFilter, at, asOf *time.Time) (map[string]struct{}, error) {
	if len(docUIDs) == 0 {
		return map[string]struct{}{}, nil
	}

	lat, lon, res, err := h.resolvePlaceCoords(ctx, near.PlaceQuery)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: resolve place %q: %w", near.PlaceQuery, err)
	}

	switch res {
	case placeUnknown:
		// No matching place entity: the geofence cannot resolve, so no
		// document qualifies (matches the episode path's empty result).
		return map[string]struct{}{}, nil
	case placeWithoutCoord:
		// Degrade to a display-name substring match on the cited entities.
		return h.docsCitingNamedEntities(ctx, docUIDs, near.PlaceQuery, at, asOf)
	case placeWithCoord:
		// fall through to the haversine path below
	}

	radiusKm := near.RadiusKm
	if radiusKm <= 0 {
		radiusKm = spatialDefaultRadiusKm
	}
	// Scoped to the read span: an out-of-scope entity's coordinates must not
	// keep an in-scope document via the spatial fallback (#206).
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	nearby, err := geo.NearbyEntities(ctx, h.reader, lat, lon, radiusKm, scopePred, scopeArgs)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: document nearby entities: %w", err)
	}
	if len(nearby) == 0 {
		return map[string]struct{}{}, nil
	}
	nearbyUIDs := make([]string, len(nearby))
	for i, ec := range nearby {
		nearbyUIDs[i] = ec.UID
	}
	return h.docsCitingEntityUIDs(ctx, docUIDs, nearbyUIDs, at, asOf)
}

// docsCitingEntityUIDs returns the subset of docUIDs with a wikilink edge
// (any predicate in docPredicateInList, including the typed wikilink set) to
// any entity in entityUIDs, valid at the requested time (relationTemporalClause).
func (h *HybridQuery) docsCitingEntityUIDs(ctx context.Context, docUIDs, entityUIDs []string, at, asOf *time.Time) (map[string]struct{}, error) {
	if len(docUIDs) == 0 || len(entityUIDs) == 0 {
		return map[string]struct{}{}, nil
	}
	docPH, docArgs := inClauseStrings(docUIDs)
	entPH, entArgs := inClauseStrings(entityUIDs)
	args := append(docArgs, entArgs...)
	clause, clauseArgs := relationTemporalClause("r", at, asOf)
	args = append(args, clauseArgs...)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("r")
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT DISTINCT r.subject_uid
		FROM relations r
		WHERE r.predicate IN %s
		  AND r.subject_uid IN (%s)
		  AND r.object_uid IN (%s)%s
		  AND %s`, docPredicateInList, docPH, entPH, clause, scopePred)
	return h.scanDocUIDSet(ctx, q, args)
}

// docsCitingNamedEntities returns the subset of docUIDs whose wikilink edges
// (valid at the requested time) touch an entity whose display_name contains
// placeQuery as a substring — the coordinate-less spatial fallback, mirroring
// nameMatchEpisodes.
func (h *HybridQuery) docsCitingNamedEntities(ctx context.Context, docUIDs []string, placeQuery string, at, asOf *time.Time) (map[string]struct{}, error) {
	docPH, args := inClauseStrings(docUIDs)
	args = append(args, "%"+placeQuery+"%")
	clause, clauseArgs := relationTemporalClause("r", at, asOf)
	args = append(args, clauseArgs...)
	// Scope BOTH the wikilink edge (r) and the named entity (e) the LIKE matches:
	// otherwise a default-scope Near recall could keep a document because of an
	// out-of-scope (e.g. team) entity name the caller must not read.
	ss := scopesFromContext(ctx)
	scopePredR, scopeArgsR := ss.Predicate("r")
	scopePredE, scopeArgsE := ss.Predicate("e")
	args = append(args, scopeArgsR...)
	args = append(args, scopeArgsE...)
	q := fmt.Sprintf(`
		SELECT DISTINCT r.subject_uid
		FROM relations r
		JOIN entities e ON e.uid = r.object_uid
		WHERE r.predicate IN %s
		  AND r.subject_uid IN (%s)
		  AND e.display_name LIKE ?%s
		  AND %s
		  AND %s`, docPredicateInList, docPH, clause, scopePredR, scopePredE)
	return h.scanDocUIDSet(ctx, q, args)
}

// scanDocUIDSet runs a single-column document-UID query and collects the
// results into a set.
func (h *HybridQuery) scanDocUIDSet(ctx context.Context, q string, args []any) (map[string]struct{}, error) {
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: document spatial filter: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("kernel: hybrid: scan document spatial hit: %w", err)
		}
		out[uid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kernel: hybrid: document spatial filter: %w", err)
	}
	return out, nil
}

// documentsProjectionAvailable reports whether the wired SQLite reader carries
// the FULL documents projection (migration 0016). Probed once per HybridQuery
// and cached: the schema cannot change under a live reader.
//
// Every table the lanes touch must be present — not just `documents`. A
// partially-migrated or custom schema that has `documents` but is missing
// `documents_vec`, `documents_vec_ids`, or `documents_fts` would otherwise
// pass the probe and then turn ordinary episode recall into a runtime SQL /
// vector error. Requiring the whole set keeps the lane fail-safe: anything
// short of the complete projection disables it and leaves episode recall
// byte-identical.
func (h *HybridQuery) documentsProjectionAvailable(ctx context.Context) bool {
	h.docProbeOnce.Do(func() {
		// documents_vec is a virtual table (vec0); it lives in sqlite_master
		// as type 'table' alongside the ordinary tables, so a single IN scan
		// covers all four. The lane needs every one of them.
		const required = 4
		var n int
		err := h.reader.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master
			 WHERE type = 'table'
			   AND name IN ('documents', 'documents_vec', 'documents_vec_ids', 'documents_fts')`).Scan(&n)
		h.docAvailable = err == nil && n == required
	})
	return h.docAvailable
}

// docVecLane fills chunk-vec evidence: KNN over documents_vec returns chunk
// IDs ("<docUid>#<idx>") distance-ordered; the first chunk seen per document
// is its best, and doc rank increments only on new documents.
//
// Under degraded recall (qvec == nil; the embedder was unavailable) this lane is
// skipped — there is no query embedding to search documents_vec with, and the
// vector index refuses a zero-length probe. Document hits then come from the
// doc-FTS and doc-graph lanes only (#236).
func (h *HybridQuery) docVecLane(ctx context.Context, qvec []float32, cands map[string]*docCandidate) error {
	if qvec == nil {
		return nil
	}
	// Scoped like fetchVecAndFTS: the sidecar filter is defense-in-depth on
	// top of the authoritative scoped SQLite join-back.
	hits, err := h.vec.Search(ctx, qvec, h.candidateK, VectorFilter{
		Kinds:  []EventKind{docEventKind},
		Scopes: scopesFromContext(ctx),
	})
	if err != nil {
		return fmt.Errorf("kernel: hybrid: document vec search: %w", err)
	}
	rank := 0
	for _, vh := range hits {
		uid, idx, ok := splitDocChunkID(vh.ID)
		if !ok {
			continue
		}
		if _, seen := cands[uid]; seen {
			continue // a closer chunk of this document already won
		}
		rank++
		cands[uid] = &docCandidate{
			uid:      uid,
			vecRank:  rank,
			rawSem:   CosineSimilarityFromDistance(vh.Score),
			chunkIdx: idx,
		}
	}
	return nil
}

// docFTSLane fills lexical evidence from documents_fts when the wired FTSConn
// supports it.
func (h *HybridQuery) docFTSLane(ctx context.Context, query string, cands map[string]*docCandidate) error {
	ds, ok := h.fts.(documentFTSSearcher)
	if !ok {
		return nil
	}
	ftsQuery := escapeFTSQuery(query)
	if ftsQuery == "" {
		return nil
	}
	hits, err := ds.SearchDocuments(ctx, ftsQuery, h.candidateK)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: document fts search: %w", err)
	}
	queryTokens := countQueryTokens(query)
	for i, fh := range hits {
		c := cands[fh.ID]
		if c == nil {
			c = &docCandidate{uid: fh.ID, chunkIdx: -1}
			cands[fh.ID] = c
		}
		if c.ftsRank == 0 {
			c.ftsRank = i + 1
			c.rawBM25 = BM25Sigmoid(fh.BM25, queryTokens)
			c.ftsSnippet = fh.Snippet
		}
	}
	return nil
}

// relationTemporalClause builds the bi-temporal WHERE fragment shared by every
// document wikilink query (graph lane + spatial geofence), so they all see the
// relations projection through the SAME time window the episode graph
// traversal uses. `alias` is the relations-table alias in the caller's query.
//
//   - valid-time (`at`, defaulting to now): the edge's [valid_from, valid_until)
//     window must contain the requested instant. Mirrors the Kuzu episode lane's
//     (validFrom IS NULL OR validFrom <= now) AND (validUntil IS NULL OR now < validUntil).
//   - transaction-time (`asOf`): only edges recorded by AsOf participate.
//
// Both bounds compare RFC3339Nano UTC strings, which sort chronologically and
// match how the dispatcher persists valid_from / valid_until / create_time.
func relationTemporalClause(alias string, at, asOf *time.Time) (clause string, args []any) {
	now := time.Now().UTC()
	if at != nil {
		now = at.UTC()
	}
	nowStr := now.Format(time.RFC3339Nano)
	clause = fmt.Sprintf(
		" AND (%[1]s.valid_from IS NULL OR %[1]s.valid_from <= ?) AND (%[1]s.valid_until IS NULL OR ? < %[1]s.valid_until)",
		alias)
	args = append(args, nowStr, nowStr)
	if asOf != nil {
		clause += fmt.Sprintf(" AND %s.create_time <= ?", alias)
		args = append(args, asOf.UTC().Format(time.RFC3339Nano))
	}
	return clause, args
}

// docGraphLane fills graph evidence: documents whose wikilink edges
// (any predicate in docPredicateInList, including typed wikilinks like
// supports/contradicts) touch the seed entities,
// valid at the requested time, ranked by how many distinct seeds they touch.
// This is the doc-side mirror of the episode graph lane — a note about Rosie
// surfaces for a Rosie-anchored query even when its prose shares no tokens
// with it.
//
// Both temporal bounds match the episode graph traversal:
//   - valid-time (`At`, defaulting to now): the edge's [valid_from, valid_until)
//     window must contain the requested instant, so historical recall sees the
//     wikilink graph as it was valid then, not the current snapshot.
//   - transaction-time (`AsOf`): only edges recorded by AsOf participate.
func (h *HybridQuery) docGraphLane(ctx context.Context, seedEntityUIDs []string, at, asOf *time.Time, cands map[string]*docCandidate) error {
	placeholders, args := inClauseStrings(seedEntityUIDs)
	clause, clauseArgs := relationTemporalClause("r", at, asOf)
	args = append(args, clauseArgs...)
	// Scope BOTH the relation (the statement) and the document it ranks: an
	// out-of-scope wikilink edge must not influence in-scope document ranking.
	ss := scopesFromContext(ctx)
	scopePredR, scopeArgsR := ss.Predicate("r")
	scopePredD, scopeArgsD := ss.Predicate("d")
	args = append(args, scopeArgsR...)
	args = append(args, scopeArgsD...)
	args = append(args, h.candidateK)
	//nolint:gosec // G201: only "?" placeholders and constant fragments are spliced in
	q := fmt.Sprintf(`
		SELECT r.subject_uid, COUNT(DISTINCT r.object_uid) AS seeds
		FROM relations r
		JOIN documents d ON d.document_uid = r.subject_uid AND d.deleted = 0
		WHERE r.predicate IN %s
		  AND r.object_uid IN (%s)%s
		  AND %s
		  AND %s
		GROUP BY r.subject_uid
		ORDER BY seeds DESC, r.subject_uid
		LIMIT ?`, docPredicateInList, placeholders, clause, scopePredR, scopePredD)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: document graph lane: %w", err)
	}
	defer rows.Close()

	rank := 0
	for rows.Next() {
		var (
			uid   string
			seeds int
		)
		if err := rows.Scan(&uid, &seeds); err != nil {
			return fmt.Errorf("kernel: hybrid: scan document graph hit: %w", err)
		}
		c := cands[uid]
		if c == nil {
			c = &docCandidate{uid: uid, chunkIdx: -1}
			cands[uid] = c
		}
		if c.graphRank == 0 {
			rank++
			c.graphRank = rank
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: document graph lane: %w", err)
	}
	return nil
}

// hydrateDocRows fetches the live documents projection rows for the candidate
// set and applies the request's Since/Until window on update_time. Temporal
// document slicing is approximate (the projection holds only the latest
// revision); a document whose last update falls outside the window is
// excluded rather than rewound.
func (h *HybridQuery) hydrateDocRows(ctx context.Context, cands map[string]*docCandidate, req RecallRequest) (map[string]docRow, error) {
	uids := make([]string, 0, len(cands))
	for uid := range cands {
		uids = append(uids, uid)
	}
	placeholders, args := inClauseStrings(uids)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	args = append(args, scopeArgs...)
	//nolint:gosec // G201: only "?" placeholders and bound scope params are spliced in
	q := fmt.Sprintf(`
		SELECT document_uid, path, title, body, revision_uid, update_time
		FROM documents
		WHERE deleted = 0 AND document_uid IN (%s) AND %s`, placeholders, scopePred)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: hydrate documents: %w", err)
	}
	defer rows.Close()

	out := make(map[string]docRow, len(uids))
	for rows.Next() {
		var (
			r                      docRow
			title, rev, updateTime string
		)
		if err := rows.Scan(&r.uid, &r.path, &title, &r.body, &rev, &updateTime); err != nil {
			return nil, fmt.Errorf("kernel: hybrid: scan document row: %w", err)
		}
		r.title = title
		r.revisionUID = rev
		if t, perr := time.Parse(time.RFC3339Nano, updateTime); perr == nil {
			r.updateTime = t
		}
		if !req.Since.IsZero() && r.updateTime.Before(req.Since) {
			continue
		}
		if !req.Until.IsZero() && !r.updateTime.Before(req.Until) {
			continue
		}
		out[r.uid] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kernel: hybrid: hydrate documents: %w", err)
	}
	return out, nil
}

// docChunkMetaFor loads the documents_vec_ids sidecar rows (chunk index,
// heading, byte offset) for every candidate that won through the chunk-vec
// lane, keyed by document UID and ordered by chunk index. The sidecar was
// written by the dispatcher at apply time, so offsets always describe the
// CURRENT body.
func (h *HybridQuery) docChunkMetaFor(ctx context.Context, cands map[string]*docCandidate, rows map[string]docRow) (map[string][]docChunkMeta, error) {
	uids := make([]string, 0, len(cands))
	for uid, c := range cands {
		if c.chunkIdx < 0 {
			continue // no vec-lane winner; no section anchor needed
		}
		if _, ok := rows[uid]; !ok {
			continue
		}
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return map[string][]docChunkMeta{}, nil
	}
	placeholders, args := inClauseStrings(uids)
	//nolint:gosec // G201: only "?" placeholders are spliced in via inClauseStrings
	q := fmt.Sprintf(`
		SELECT document_uid, chunk_index, heading, byte_offset
		FROM documents_vec_ids
		WHERE document_uid IN (%s)
		ORDER BY document_uid, chunk_index`, placeholders)
	res, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: document chunk meta: %w", err)
	}
	defer res.Close()

	out := make(map[string][]docChunkMeta, len(uids))
	for res.Next() {
		var (
			uid string
			m   docChunkMeta
		)
		if err := res.Scan(&uid, &m.index, &m.heading, &m.byteOffset); err != nil {
			return nil, fmt.Errorf("kernel: hybrid: scan chunk meta: %w", err)
		}
		out[uid] = append(out[uid], m)
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("kernel: hybrid: document chunk meta: %w", err)
	}
	return out, nil
}

// scoreDocCandidates fuses each candidate's lane evidence with the same math
// as the episode path and projects Hits.
func (h *HybridQuery) scoreDocCandidates(cands map[string]*docCandidate, rows map[string]docRow, chunkMeta map[string][]docChunkMeta) []Hit {
	hits := make([]Hit, 0, len(cands))
	for uid, c := range cands {
		row, ok := rows[uid]
		if !ok {
			continue // deleted, or outside the Since/Until window
		}
		vc, fc, gc, nc := rrfComponents(c.vecRank, c.ftsRank, c.graphRank, 0, h.rrfK, h.graphWeight, h.nameEmbWeight)
		var fused float32
		if h.fusionMode == FusionModeWeighted {
			// Pre-fusion semantic gate, applied ONLY to candidates the
			// chunk-vec lane actually surfaced (vecRank > 0). The gate exists
			// to stop lexical/graph evidence from resurrecting a
			// semantically-unrelated VEC hit — it must not silently drop a
			// document that won purely on FTS or the wikilink graph, where
			// rawSem is structurally 0 (no chunk was ever scored). Gating
			// those would defeat the graph lane's whole purpose: the
			// zero-overlap "note about Rosie" match the design surfaces on a
			// Rosie-anchored query.
			if h.semanticGateThreshold > 0 && c.vecRank > 0 && float64(c.rawSem) < h.semanticGateThreshold {
				continue
			}
			fused = WeightedComponents(c.rawSem, c.rawBM25, c.graphRank, 0)
		} else {
			fused = vc + fc + gc + nc
		}
		if len(h.typeWeights) > 0 {
			if w, ok := h.typeWeights[classDocument]; ok {
				fused *= float32(w)
			}
		}

		text, source := docHitTextAndSource(&row, c, chunkMeta[uid])
		hits = append(hits, Hit{
			UID:  uid,
			Kind: docEventKind,
			Text: text,
			// Human/agent-authored documents carry full writer confidence.
			Snippet:    docHitSnippet(c.ftsSnippet, text, h.snippetRunes),
			Confidence: 1.0,
			CreateTime: row.updateTime,
			FusedScore: fused,
			Components: ScoreComponents{
				Vector:      vc,
				FTS:         fc,
				Graph:       gc,
				NameEmb:     nc,
				RawSemantic: c.rawSem,
				RawBM25:     c.rawBM25,
			},
			// Cite the document under its get-able entity name (the doc hit's own
			// UID is its document-entity uid, resolvable via get), mirroring how
			// the episode and bare-entity lanes self-cite a fetchable handle. A
			// bare events/{revisionUID} pointed at the revision event, which no
			// read tool accepts. The revision/source location stays on Source.
			Citations: []string{"entities/" + uid},
			Source:    source,
		})
	}
	return hits
}

// docHitTextAndSource picks the hit text (winning chunk when the vec lane
// fired, else the capped body) and builds the section-anchored source
// location.
func docHitTextAndSource(row *docRow, c *docCandidate, meta []docChunkMeta) (string, *SourceLocation) {
	src := &SourceLocation{Path: row.path, LineStart: 1}
	if c.chunkIdx < 0 || len(meta) == 0 {
		return truncateBytesAtRune(row.body, docBodyTextCap), src
	}
	// Find the winning chunk and its successor's offset; the slice
	// [offset, nextOffset) is the chunk minus the extractor's overlap tail —
	// coherent prose without re-chunking.
	start, end := 0, len(row.body)
	for i, m := range meta {
		if m.index != c.chunkIdx {
			continue
		}
		start = m.byteOffset
		if i+1 < len(meta) {
			end = meta[i+1].byteOffset
		}
		src.Heading = m.heading
		break
	}
	if start > len(row.body) {
		start = len(row.body)
	}
	if end > len(row.body) || end < start {
		end = len(row.body)
	}
	src.LineStart = 1 + strings.Count(row.body[:start], "\n")
	return row.body[start:end], src
}

// docHitSnippet prefers the FTS highlight excerpt, falling back to a
// rune-capped prefix of the hit text.
func docHitSnippet(ftsSnippet, text string, runesMax int) string {
	if ftsSnippet != "" {
		return ftsSnippet
	}
	runes := []rune(text)
	if len(runes) <= runesMax {
		return text
	}
	return string(runes[:runesMax])
}

// truncateBytesAtRune caps s at max bytes, backing up to a rune boundary.
func truncateBytesAtRune(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// splitDocChunkID parses a documents_vec sidecar ID "<docUid>#<chunkIdx>".
func splitDocChunkID(id string) (uid string, idx int, ok bool) {
	pos := strings.LastIndexByte(id, '#')
	if pos <= 0 || pos == len(id)-1 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[pos+1:])
	if err != nil || n < 0 {
		return "", 0, false
	}
	return id[:pos], n, true
}

// attachDocumentEntities fills Hit.Entities for document hits on the
// post-limit shortlist: the entities (and documents) the document's wikilink
// edges point at — the doc-side analogue of the episode mentions→entities
// join, and the same seed-then-traverse pivot.
//
// The wikilink edges are evaluated through the SAME bi-temporal window as the
// graph lane and Near filter (relationTemporalClause with the request's
// At/AsOf), so a document kept by historical recall returns the anchors that
// were valid THEN — not the current-live graph. Without this, a hit kept
// because its [[Berlin]] edge was valid in 2020 would pivot to an empty or
// present-day anchor set, breaking seed-and-pivot for the same evidence.
func (h *HybridQuery) attachDocumentEntities(ctx context.Context, hits []Hit, at, asOf *time.Time) error {
	uids := make([]string, 0, len(hits))
	for i := range hits {
		if hits[i].Kind == docEventKind {
			uids = append(uids, hits[i].UID)
		}
	}
	if len(uids) == 0 {
		return nil
	}
	placeholders, args := inClauseStrings(uids)
	clause, clauseArgs := relationTemporalClause("r", at, asOf)
	args = append(args, clauseArgs...)
	// Scope BOTH the wikilink edge (the statement) and the anchor entity it
	// resolves: an out-of-scope edge must not attach an anchor to an in-scope hit.
	ss := scopesFromContext(ctx)
	scopePredR, scopeArgsR := ss.Predicate("r")
	scopePredE, scopeArgsE := ss.Predicate("e")
	args = append(args, scopeArgsR...)
	args = append(args, scopeArgsE...)
	//nolint:gosec // G201: only "?" placeholders and constant fragments are spliced in
	q := fmt.Sprintf(`
		SELECT r.subject_uid, e.uid, e.kind, e.display_name
		FROM relations r
		JOIN entities e ON e.uid = r.object_uid
		WHERE r.predicate IN %s
		  AND r.subject_uid IN (%s)%s
		  AND %s
		  AND %s
		ORDER BY r.subject_uid, e.uid`, docPredicateInList, placeholders, clause, scopePredR, scopePredE)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: attach document entities: %w", err)
	}
	defer rows.Close()

	refs := make(map[string][]EntityRef, len(uids))
	for rows.Next() {
		var (
			docUID string
			ref    EntityRef
		)
		if err := rows.Scan(&docUID, &ref.UID, &ref.Kind, &ref.DisplayName); err != nil {
			return fmt.Errorf("kernel: hybrid: scan document entity: %w", err)
		}
		if len(refs[docUID]) >= maxDocEntityRefs {
			continue
		}
		refs[docUID] = append(refs[docUID], ref)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: attach document entities: %w", err)
	}
	for i := range hits {
		if hits[i].Kind == docEventKind {
			hits[i].Entities = refs[hits[i].UID]
		}
	}
	return nil
}

// sortHitsByScore is the shared deterministic ordering: fused score
// descending, UID ascending as the tiebreaker.
func sortHitsByScore(hits []Hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].FusedScore != hits[j].FusedScore {
			return hits[i].FusedScore > hits[j].FusedScore
		}
		return hits[i].UID < hits[j].UID
	})
}
