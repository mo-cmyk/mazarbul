// Package navigate is the LLM-free structured read surface over Mazarbul's
// SQLite projection. It exists so an agent (or its own LLM) can fetch, filter,
// and traverse entities / relations / episodes at SQLite speed and do the
// reasoning itself — no embedder, no query rewrite, no answer composition.
//
// The hybrid recall lane (internal/kernel/query_hybrid.go) stays the entry
// point for fuzzy semantic/graph search and seeds a navigation: its hits carry
// entity anchors a caller pivots into Neighbors / Relations / Get from here.
// Everything in this package is a pure read against the reader pool; the graph
// (LadybugDB) is intentionally not consulted because the relations table is
// the same projection and avoids a second connection + the graph write lock.
package navigate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// ErrNotFound is returned (wrapped) when a Get* call targets a UID that does
// not exist in the projection. MCP/CLI callers map it to a not-found code.
var ErrNotFound = errors.New("navigate: not found")

// Navigator is the read surface. Construct with New; it borrows a read-only
// *sql.DB (the daemon's reader pool via Dispatcher.SQLiteReader, or a
// standalone sqlite.Store.Reader in the CLI). It holds no other state and is
// safe for concurrent use.
type Navigator struct {
	reader *sql.DB
	scopes kernel.ScopeSet
}

// New returns a Navigator backed by reader defaulting to DefaultReadSet
// {project, personal} — the safe per-request default for MCP tools, CLI, and
// contextbrief callers. Reader must be non-nil.
//
// Use NewAllScopes for maintenance and audit paths that legitimately span all
// user scopes (vault sync, daemon base navigators, hybrid-query anchor).
func New(reader *sql.DB) (*Navigator, error) {
	if reader == nil {
		return nil, errors.New("navigate: New requires a non-nil reader")
	}
	return &Navigator{reader: reader, scopes: kernel.DefaultReadSet()}, nil
}

// NewAllScopes returns a Navigator backed by reader with AllScopes
// {project, personal, team}. Use this only on maintenance and audit paths that
// legitimately read the full projection; per-request paths use New and narrow
// via WithScopes or navWithScopes.
func NewAllScopes(reader *sql.DB) (*Navigator, error) {
	if reader == nil {
		return nil, errors.New("navigate: NewAllScopes requires a non-nil reader")
	}
	return &Navigator{reader: reader, scopes: kernel.AllScopes()}, nil
}

// NewVaultScopes returns a Navigator spanning ALL FOUR scopes
// {project, personal, team, draft}. It is the read surface for the vault
// projector and the document importer, which must see and address every scope's
// documents — including draft, which AllScopes() deliberately excludes so recall
// and navigation keep speculative staging invisible. Do NOT widen AllScopes() to
// draft; this dedicated span is the only place draft documents are read.
func NewVaultScopes(reader *sql.DB) (*Navigator, error) {
	if reader == nil {
		return nil, errors.New("navigate: NewVaultScopes requires a non-nil reader")
	}
	return &Navigator{
		reader: reader,
		scopes: kernel.NewScopeSet(kernel.ScopeProject, kernel.ScopePersonal, kernel.ScopeTeam, kernel.ScopeDraft),
	}, nil
}

// WithScopes returns a sibling Navigator over the same reader pool narrowed to
// the given scope span. Called once per request so scope is a structural
// property of the navigator rather than a parameter on every read method.
func (n *Navigator) WithScopes(scopes kernel.ScopeSet) *Navigator {
	clone := *n
	clone.scopes = scopes
	return &clone
}

// scopeCond returns the navigator's scope predicate + bound args for the given
// table alias ("" for an unqualified `scope` column), to AND into a shared-table
// query's WHERE clause. Every read that touches a shared derived table
// (entities/relations/facts/episodes/mentions/documents) threads this in.
func (n *Navigator) scopeCond(alias string) (string, []any) {
	return n.scopes.Predicate(alias)
}

// Entity is a resolved entity row. Mirrors the entities table plus the
// AIP-shaped fields callers expect.
//
// Properties carries the typed attribute values validated against the kind's
// propertySchema (P3); it is the raw JSON object stored in entities.properties,
// surfaced verbatim so a read round-trips what createEntity wrote. Identifiers
// are deliberately NOT a field here (they would push the struct past the
// gocritic 128-byte rangeValCopy threshold for list scans); single-entity
// callers load them via Navigator.EntityIdentifiers instead.
type Entity struct {
	UID              string          `json:"uid"`
	Kind             string          `json:"kind"`
	DisplayName      string          `json:"displayName"`
	Confidence       float64         `json:"confidence"`
	ResolutionStatus string          `json:"resolutionStatus"`
	CreateTime       string          `json:"createTime,omitempty"`
	Properties       json.RawMessage `json:"properties,omitempty"`
	// Scope is the storage scope the entity lives in ("personal"|"project"|
	// "team"), read from the entities.scope column by GetEntity. It is NOT part
	// of any tool's wire output (json:"-") — entityWire embeds Entity, so a tag
	// here would otherwise leak the field into getEntity/listEntities and trip
	// the strict list schema. It exists so a write that REMOVES the entity —
	// deleteEntity / mergeEntities — can stamp the same scope on its tombstone
	// event, keeping the team write-permission gate honest (a non-member must not
	// delete a team entity via the personal lane). Populated only by GetEntity;
	// empty (→ personal at Append) elsewhere.
	Scope string `json:"-"`
	// Canonical is true for document-kind entities whose frontmatter declares
	// canonical:true, granting them name-resolution priority over non-document
	// entities of the same display name (guardrail G5). Always false for
	// non-document entity kinds and for documents that omit the key.
	Canonical bool `json:"canonical,omitempty"`
}

// EntityIdentifier is one strong/weak external identifier anchoring an entity
// (e.g. {type:"hrb", value:"HRB 12345 B"}). Type mirrors the entity_identifiers
// kind column; the column name "kind" is reused there for the identifier type,
// distinct from the entity's own kind.
type EntityIdentifier struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Relation is a typed edge. SubjectName/ObjectName are hydrated (LEFT JOIN on
// entities) when the Navigator method populates them, so an agent gets human
// labels without a second round-trip; they are omitted when empty.
type Relation struct {
	UID             string            `json:"uid"`
	SubjectUID      string            `json:"subjectUid"`
	SubjectName     string            `json:"subjectName,omitempty"`
	Predicate       string            `json:"predicate"`
	ObjectUID       string            `json:"objectUid"`
	ObjectName      string            `json:"objectName,omitempty"`
	Confidence      float64           `json:"confidence"`
	CreateTime      string            `json:"createTime"`
	SourceEventUID  string            `json:"sourceEventUid,omitempty"` // origin episode; walk back to it for provenance
	ValidFrom       string            `json:"validFrom,omitempty"`
	ValidUntil      string            `json:"validUntil,omitempty"`
	SupersededByUID string            `json:"supersededByUid,omitempty"`
	Provenance      string            `json:"provenance,omitempty"` // free-text origin label, e.g. "wikilink [[X]]" / "prose-extraction Y"
	Attributes      map[string]string `json:"attributes,omitempty"`
	// Status is the trust posture of the edge: "asserted" (authoritative; the
	// default for user/extraction writes) or "proposed" (extracted from an
	// untrusted connector source — excluded from supersession and predicate
	// minting until a promoteRelation flips it to asserted). Always populated on
	// reads so a consumer can tell a proposed external claim from a trusted fact
	// (#211, #212). A legacy/empty stored value reads back as "asserted".
	Status string `json:"status"`
	// Retracted is true when at least one relation_retractions row targets this
	// edge, meaning its valid_until was set by an explicit relationRetracted
	// tombstone (via unrelate or resolver supersession). False means the
	// valid_until, if any, was written at creation time (a born-bounded edge)
	// and no tombstone has ever targeted it. Always false for open edges. This
	// lets callers (e.g. timeline) distinguish a tombstone-closed edge from a
	// born-bounded one without re-querying the tombstone table.
	Retracted bool `json:"retracted,omitempty"`
}

// Provenance is one source/confidence/decided-at record for an event — the
// origin episode of a record. Mirrors the provenance table the dispatcher
// writes (migration 0001, one row per event since 0009). Derived records
// (relations, entities) have no provenance row of their own: they inherit it
// from their source episode(s), which EventProvenance / EntityProvenance walk
// back to.
type Provenance struct {
	EventUID   string  `json:"eventUid"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	DecidedAt  string  `json:"decidedAt,omitempty"`
}

// Episode is a stored memory unit. Text is the verbatim remember() payload.
type Episode struct {
	UID        string  `json:"uid"`
	Text       string  `json:"text"`
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence"`
	CreateTime string  `json:"createTime"`
}

// Neighbor is one hydrated hop in a neighborhood walk: the reachable entity,
// the relation that connected it toward the seed, the edge direction relative
// to the seed, and how many hops away it sits.
type Neighbor struct {
	Entity    Entity   `json:"entity"`
	Via       Relation `json:"via"`
	Direction string   `json:"direction"` // "outgoing" or "incoming" relative to the seed
	Hops      int      `json:"hops"`
}

// SubgraphNode is one node in a seed-rooted subgraph: the hydrated entity and
// its hop distance from the seed (0 = the seed itself).
type SubgraphNode struct {
	Entity Entity `json:"entity"`
	Hops   int    `json:"hops"`
}

// SubgraphResult is the node + edge set of a seed-rooted N-hop subgraph. Nodes
// includes the seed (Hops 0). Edges are every relation among Nodes (not only
// the edge each node was reached through), so sibling cross-links are present;
// they are deduped by UID, with self-loops and edges to an out-of-set endpoint
// dropped. Both slices are sorted deterministically (nodes by hops then uid,
// edges by uid) so a caller can serialize a stable, diffable artifact.
type SubgraphResult struct {
	Nodes []SubgraphNode `json:"nodes"`
	Edges []Relation     `json:"edges"`
}

// RelationFilter narrows EntityRelations / Neighbors.
//
// Validity semantics: by default only relations live at At (now when At is
// nil) and not superseded are returned — i.e. "current truth". At moves the
// valid-time anchor (time travel); AsOf bounds transaction time ("what did we
// know on X?"). IncludeInactive bypasses BOTH the valid-window gate and the
// superseded filter to return full history.
type RelationFilter struct {
	Predicates      []string
	At              *time.Time
	AsOf            *time.Time
	IncludeInactive bool
	Limit           int
}

// EntityFilter narrows ListEntities.
type EntityFilter struct {
	Kind      string
	NameLike  string // case-insensitive substring on display_name
	NameExact string // case-insensitive equality on display_name (takes precedence over NameLike)
	// AliasExact matches entities that carry the given string as an alternate
	// name in entity_aliases. Case-insensitive via COLLATE NOCASE. Mutually
	// exclusive with NameExact and NameLike: when NameExact is non-empty it
	// takes precedence; AliasExact is used only when both NameExact and
	// NameLike are empty.
	AliasExact    string
	MinConfidence float64
	Limit         int
	Offset        int
	// SortByMentions orders results by descending mention count (number of
	// distinct episodes that cite the entity by kind+display_name), with
	// create_time DESC as a deterministic tie-break. When false (default),
	// the result is newest-first (create_time DESC). Use this for sampling
	// the most-referenced entities, e.g. for the context brief's "Key entities"
	// section when no recall-stats ranker is available.
	SortByMentions bool
	// Filter is an optional AIP-160-subset expression (see navigate/filtersql)
	// AND-combined with the structured fields above. It can reference entity
	// columns (kind, displayName, confidence, resolutionStatus, createTime),
	// the short alias `status` for resolutionStatus (values: resolved,
	// confirmed, tentative, split_candidate, deleted), an entity's fact slots
	// by state_key (e.g. task_status = "open"), and
	// related("predicate", "target"). Empty disables it.
	Filter string
}

// defaultLimit caps unbounded list/traverse calls so an agent cannot pull the
// entire store into context with one call.
const defaultLimit = 50

// GetEntity returns the entity with uid, or ErrNotFound. If uid has been
// merged away (appears as source_uid in entity_redirects), the lookup
// transparently follows the redirect chain to the surviving entity, so stale
// UIDs held by agents continue to resolve after a merge.
func (n *Navigator) GetEntity(ctx context.Context, uid string) (Entity, error) {
	resolved, err := n.resolveEntityUID(ctx, uid)
	if err != nil {
		return Entity{}, err
	}
	uid = resolved

	scope, scopeArgs := n.scopeCond("")

	q := `SELECT uid, kind, display_name, confidence, resolution_status, create_time, properties, scope
	           FROM entities WHERE uid = ? AND ` + scope
	var e Entity
	var createTime, properties sql.NullString
	err = n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...).Scan(
		&e.UID, &e.Kind, &e.DisplayName, &e.Confidence, &e.ResolutionStatus, &createTime, &properties, &e.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return Entity{}, fmt.Errorf("entity %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return Entity{}, fmt.Errorf("get entity %q: %w", uid, err)
	}
	e.CreateTime = createTime.String
	// Surface the stored typed attributes verbatim, but only when the column
	// holds a valid JSON object — a NULL/empty/garbage value stays omitted
	// rather than emitting a broken `properties` field on the wire.
	if properties.Valid && json.Valid([]byte(properties.String)) {
		e.Properties = json.RawMessage(properties.String)
	}
	return e, nil
}

// resolveEntityUID follows the entity_redirects chain from uid to fixpoint
// and returns the canonical surviving UID. If uid has no row in
// entity_redirects it is returned unchanged. Chains resolve by following rows
// until no further redirect exists; cycles (concurrent A→B and B→A merges)
// are broken by returning the lexicographically smallest UID among cycle
// members, matching the dispatcher's MIN(event_uid) fold rule.
func (n *Navigator) resolveEntityUID(ctx context.Context, uid string) (string, error) {
	const maxHops = 64
	path := make([]string, 0, 8)
	cur := uid
	for range maxHops {
		var tgt string
		err := n.reader.QueryRowContext(ctx,
			`SELECT target_uid FROM entity_redirects WHERE source_uid = ?`, cur,
		).Scan(&tgt)
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil // fixpoint
		}
		if err != nil {
			return uid, fmt.Errorf("navigate: resolve entity uid %q: %w", uid, err)
		}
		path = append(path, cur)
		for j, v := range path {
			if v == tgt {
				// Cycle: return MIN uid among cycle members (path[j:]).
				min := path[j]
				for _, m := range path[j+1:] {
					if m < min {
						min = m
					}
				}
				return min, nil
			}
		}
		cur = tgt
	}
	return cur, nil
}

// EntityIdentifiers returns the external identifiers anchoring an entity
// (entity_identifiers rows), ordered by type then value for deterministic
// output. Empty (not an error) when the entity has none. Single-entity read
// path only — list callers omit identifiers to keep the per-row scan cheap.
func (n *Navigator) EntityIdentifiers(ctx context.Context, uid string) ([]EntityIdentifier, error) {
	// entity_identifiers carries no scope column of its own — identifiers are
	// entity-level metadata, so they ride the anchoring entity's visibility.
	// Join entities and apply the scope predicate there: an identifier surfaces
	// only when its entity is visible in the requested scope, matching the
	// entities.scope gate GetEntity uses (#206).
	scope, scopeArgs := n.scopeCond("e")
	//nolint:gosec // G202: scope is constant fragments + bound ? params; values are parameterized
	q := `SELECT ei.kind, ei.identifier FROM entity_identifiers ei
	            JOIN entities e ON e.uid = ei.entity_uid
	            WHERE ei.entity_uid = ? AND ` + scope + `
	            ORDER BY ei.kind ASC, ei.identifier ASC`
	rows, err := n.reader.QueryContext(ctx, q, append([]any{uid}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("get entity identifiers %q: %w", uid, err)
	}
	defer func() { _ = rows.Close() }()
	var out []EntityIdentifier
	for rows.Next() {
		var id EntityIdentifier
		if serr := rows.Scan(&id.Type, &id.Value); serr != nil {
			return nil, fmt.Errorf("scan entity identifier: %w", serr)
		}
		out = append(out, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate entity identifiers: %w", rerr)
	}
	return out, nil
}

// GetEpisode returns the episode with uid, or ErrNotFound. Text/source/
// confidence are parsed from the stored payload_json by scanEpisodeRow, shared
// with EpisodesCitingEntity so the payload shape has a single decode site.
func (n *Navigator) GetEpisode(ctx context.Context, uid string) (Episode, error) {
	scope, scopeArgs := n.scopeCond("")

	q := `SELECT uid, create_time, payload_json FROM episodes WHERE uid = ? AND ` + scope
	ep, err := scanEpisodeRow(n.reader.QueryRowContext(ctx, q, append([]any{uid}, scopeArgs...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return Episode{}, fmt.Errorf("episode %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return Episode{}, fmt.Errorf("get episode %q: %w", uid, err)
	}
	return ep, nil
}

// GetRelation returns the relation with uid (subject/object names hydrated), or
// ErrNotFound.
func (n *Navigator) GetRelation(ctx context.Context, uid string) (Relation, error) {
	scope, scopeArgs := n.scopeCond("r")
	// Gate endpoint-name hydration by the same scope span. An out-of-scope
	// endpoint must not disclose its display_name: the LEFT JOIN keeps the
	// relation row (its own scope already passed the WHERE guard) but resolves
	// the out-of-span name to NULL → "" in scanRelation, so only the UID surfaces.
	sScope, sArgs := n.scopeCond("s")
	oScope, oArgs := n.scopeCond("o")

	q := `SELECT r.uid, r.subject_uid, s.display_name, r.predicate,
	                  r.object_uid, o.display_name, r.confidence, r.create_time,
	                  r.valid_from, r.valid_until, r.superseded_by_uid, r.attributes,
	                  r.source_event_uid, r.provenance, r.status,
	                  EXISTS(SELECT 1 FROM relation_retractions rr WHERE rr.relation_uid = r.uid) AS retracted
	           FROM relations r
	           LEFT JOIN entities s ON s.uid = r.subject_uid AND ` + sScope + `
	           LEFT JOIN entities o ON o.uid = r.object_uid AND ` + oScope + `
	           WHERE r.uid = ? AND ` + scope
	args := make([]any, 0, len(sArgs)+len(oArgs)+1+len(scopeArgs))
	args = append(args, sArgs...)
	args = append(args, oArgs...)
	args = append(args, uid)
	args = append(args, scopeArgs...)
	rel, err := scanRelation(n.reader.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Relation{}, fmt.Errorf("relation %q: %w", uid, ErrNotFound)
	}
	if err != nil {
		return Relation{}, fmt.Errorf("get relation %q: %w", uid, err)
	}
	return rel, nil
}

// EntityRelations returns relations where uid is the subject or object,
// filtered per opts. Subject/object display names are hydrated.
func (n *Navigator) EntityRelations(ctx context.Context, uid string, opts RelationFilter) ([]Relation, error) {
	conds := make([]string, 0, 3+len(opts.Predicates))
	conds = append(conds, "(r.subject_uid = ? OR r.object_uid = ?)")
	whereArgs := make([]any, 0, 5+len(opts.Predicates))
	whereArgs = append(whereArgs, uid, uid)
	conds, whereArgs = appendRelationFilter(conds, whereArgs, opts)
	scope, scopeArgs := n.scopeCond("r")
	conds = append(conds, scope)
	whereArgs = append(whereArgs, scopeArgs...)

	// Gate endpoint-name hydration by the same scope span. An out-of-scope
	// endpoint must not disclose its display_name: the LEFT JOIN keeps the
	// relation row (its own scope already passed the WHERE guard) but resolves
	// the out-of-span name to NULL → "" in scanRelation, so only the UID surfaces.
	sScope, sArgs := n.scopeCond("s")
	oScope, oArgs := n.scopeCond("o")

	// limit semantics: 0 → defaultLimit; negative → no cap (the sentinel
	// Neighbors' per-hop fetch passes so a wide hop is not truncated before
	// the walk's overall cap applies); positive → that cap.
	limit := opts.Limit
	limitClause := " LIMIT ?"
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 0:
		limitClause = ""
	}
	// The clause fragments in conds and limitClause are all compile-time
	// constants; every runtime value is a bound ? parameter in args, so this
	// concatenation is not an injection vector.
	//nolint:gosec // G202: only constant clause fragments concatenated; values are parameterized
	q := `SELECT r.uid, r.subject_uid, s.display_name, r.predicate,
	             r.object_uid, o.display_name, r.confidence, r.create_time,
	             r.valid_from, r.valid_until, r.superseded_by_uid, r.attributes,
	             r.source_event_uid, r.provenance, r.status,
	             EXISTS(SELECT 1 FROM relation_retractions rr WHERE rr.relation_uid = r.uid) AS retracted
	      FROM relations r
	      LEFT JOIN entities s ON s.uid = r.subject_uid AND ` + sScope + `
	      LEFT JOIN entities o ON o.uid = r.object_uid AND ` + oScope + `
	      WHERE ` + strings.Join(conds, " AND ") + `
	      ORDER BY r.create_time DESC` + limitClause
	// Args follow query-text order: the JOIN scope predicates precede the WHERE
	// clause, so sArgs/oArgs come before the WHERE args, then the LIMIT.
	args := make([]any, 0, len(sArgs)+len(oArgs)+len(whereArgs)+1)
	args = append(args, sArgs...)
	args = append(args, oArgs...)
	args = append(args, whereArgs...)
	if limitClause != "" {
		args = append(args, limit)
	}

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query relations for %q: %w", uid, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Relation, 0, max(limit, 0))
	for rows.Next() {
		rel, serr := scanRelation(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan relation: %w", serr)
		}
		out = append(out, rel)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate relations: %w", rerr)
	}
	return out, nil
}

// Neighbors expands the neighborhood of seed within depth hops (clamped to
// 1..MaxHops), returning hydrated neighbor entities and the relation each was
// reached through. The seed is never returned; closer hops win on duplicates.
// Opts applies the same validity/predicate gating as EntityRelations.
func (n *Navigator) Neighbors(ctx context.Context, seed string, depth int, opts RelationFilter) ([]Neighbor, error) {
	depth = clampHops(depth)
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	seen := map[string]bool{seed: true}
	out := make([]Neighbor, 0, limit)
	frontier := []string{seed}

	for hop := 1; hop <= depth && len(out) < limit; hop++ {
		var next []string
		for _, node := range frontier {
			found, err := n.hopFrom(ctx, node, hop, opts, seen)
			if err != nil {
				return nil, err
			}
			for _, nb := range found {
				out = append(out, nb)
				next = append(next, nb.Entity.UID)
				if len(out) >= limit {
					return out, nil
				}
			}
		}
		frontier = next
	}
	return out, nil
}

// hopFrom returns the not-yet-seen hydrated neighbors reachable from node in a
// single step, marking each seen so a later node in the same hop does not
// re-emit it. Hop is recorded on every returned Neighbor. The relations fetch
// is unbounded (Limit 0) so a wide hop is not silently truncated before the
// caller's overall cap applies.
func (n *Navigator) hopFrom(ctx context.Context, node string, hop int, opts RelationFilter, seen map[string]bool) ([]Neighbor, error) {
	hopOpts := opts
	hopOpts.Limit = -1 // no cap: fetch every edge so the walk's overall limit, not a per-hop default, governs truncation
	rels, err := n.EntityRelations(ctx, node, hopOpts)
	if err != nil {
		return nil, err
	}
	found := make([]Neighbor, 0, len(rels))
	for i := range rels {
		rel := rels[i]
		otherUID, direction := otherEndpoint(rel, node)
		if otherUID == "" || seen[otherUID] {
			continue
		}
		seen[otherUID] = true
		ent, gerr := n.GetEntity(ctx, otherUID)
		if errors.Is(gerr, ErrNotFound) {
			// Edge points at an entity not in the projection (e.g. the "self"
			// sentinel or a forgotten node); skip rather than fail the walk.
			continue
		}
		if gerr != nil {
			return nil, gerr
		}
		found = append(found, Neighbor{Entity: ent, Via: rel, Direction: direction, Hops: hop})
	}
	return found, nil
}

// MaxHops bounds the traversal depth Neighbors and Subgraph will expand. It is a
// guardrail against a pathological deep walk on a hub entity; the per-call Limit
// (the node cap) is the primary bound on result size. The neighbors MCP tool
// schema and the `maz traverse` / `maz neighbors` CLI advertise the same ceiling.
const MaxHops = 6

// clampHops bounds a requested traversal depth to the supported 1..MaxHops range.
func clampHops(depth int) int {
	if depth < 1 {
		return 1
	}
	if depth > MaxHops {
		return MaxHops
	}
	return depth
}

// Subgraph walks an N-hop neighborhood from seed and returns its full node +
// edge set, suitable for rendering a seed-centered map. Node discovery rides
// Neighbors (depth clamped to 1..MaxHops, total nodes capped by opts.Limit) and
// the seed is added at hop 0. It then unions every edge among the discovered
// nodes via EntityRelations, so sibling cross-links appear (not just the tree
// edge each node was reached through), deduping by UID and dropping self-loops
// and edges whose endpoints are not both in the node set. The endpoint filter
// doubles as a scope-leak guard: an out-of-scope neighbor (kept out of the node
// set by the scoped navigator) never surfaces even as a dangling edge. The same
// validity/predicate/scope gating as Neighbors / EntityRelations applies. A
// missing seed returns ErrNotFound (wrapped).
func (n *Navigator) Subgraph(ctx context.Context, seed string, hops int, opts RelationFilter) (SubgraphResult, error) {
	seedEntity, err := n.GetEntity(ctx, seed)
	if err != nil {
		return SubgraphResult{}, err
	}

	nbrs, err := n.Neighbors(ctx, seed, hops, opts)
	if err != nil {
		return SubgraphResult{}, err
	}

	// Node set: seed at hop 0, then each reached neighbor (Neighbors already
	// deduped and recorded the closest hop). inSet drives the edge filter below.
	nodes := make([]SubgraphNode, 0, len(nbrs)+1)
	nodes = append(nodes, SubgraphNode{Entity: seedEntity, Hops: 0})
	inSet := map[string]bool{seedEntity.UID: true}
	for i := range nbrs {
		nodes = append(nodes, SubgraphNode{Entity: nbrs[i].Entity, Hops: nbrs[i].Hops})
		inSet[nbrs[i].Entity.UID] = true
	}

	// Edge union: every relation incident to a node, kept only when both
	// endpoints are in the set and it is not a self-loop. Limit -1 disables the
	// per-node cap (the node set already bounds the result); dedup by UID.
	edgeOpts := opts
	edgeOpts.Limit = -1
	seenEdge := make(map[string]bool)
	edges := make([]Relation, 0, len(nodes))
	for i := range nodes {
		rels, rerr := n.EntityRelations(ctx, nodes[i].Entity.UID, edgeOpts)
		if rerr != nil {
			return SubgraphResult{}, rerr
		}
		for j := range rels {
			r := rels[j]
			if r.SubjectUID == r.ObjectUID || !inSet[r.SubjectUID] || !inSet[r.ObjectUID] || seenEdge[r.UID] {
				continue
			}
			seenEdge[r.UID] = true
			edges = append(edges, r)
		}
	}

	// Deterministic ordering for a stable, diffable serialization.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Hops != nodes[j].Hops {
			return nodes[i].Hops < nodes[j].Hops
		}
		return nodes[i].Entity.UID < nodes[j].Entity.UID
	})
	sort.Slice(edges, func(i, j int) bool { return edges[i].UID < edges[j].UID })

	return SubgraphResult{Nodes: nodes, Edges: edges}, nil
}

// ListEntities returns entities matching opts.
//
// Default ordering is newest first (create_time DESC). When EntityFilter.SortByMentions
// is true, entities are ordered by descending mention count (the number of distinct
// episodes that cite the entity by kind+display_name) with create_time DESC as a
// deterministic tie-break. This surfaces the most-referenced entities first, which
// produces a more representative "Key entities" brief section than recency alone.
func (n *Navigator) ListEntities(ctx context.Context, opts EntityFilter) ([]Entity, error) {
	// buildEntityWhere produces the conditions over alias e, shared with
	// CountEntities / AggregateEntities so the structured fields + the AIP-160
	// Filter compile to identical predicates across list, count, and group-by.
	conds, args, err := buildEntityWhere(opts, n.scopes)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	q := buildListEntitiesQuery(conds, opts.SortByMentions)
	args = append(args, limit, max(opts.Offset, 0))

	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Entity, 0, limit)
	for rows.Next() {
		e, serr := scanEntity(rows, opts.SortByMentions)
		if serr != nil {
			return nil, serr
		}
		out = append(out, e)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate entities: %w", rerr)
	}
	return out, nil
}

// buildListEntitiesQuery constructs the SELECT for ListEntities. When
// sortByMentions is true it appends a correlated mention-count subquery and
// orders by mention_count DESC; otherwise it orders by create_time DESC.
// The WHERE clause is assembled from conds; two trailing bound parameters are
// reserved for LIMIT and OFFSET (appended by the caller).
//
// A LEFT JOIN to the documents table populates COALESCE(d.canonical, 0) for
// every row: document-kind entities get their stored canonical flag; all other
// entity kinds receive 0 because no documents row exists for them.
func buildListEntitiesQuery(conds []string, sortByMentions bool) string {
	const join = " LEFT JOIN documents d ON d.document_uid = e.uid"
	var q string
	if sortByMentions {
		// Correlated subquery counts mentions for each entity: a mention row
		// matches when its kind + display_name equal the entity's. Mention
		// frequency is a global signal (not scope-narrowed) — it reflects how
		// often the entity appeared in extracted episodes across all scopes.
		q = `SELECT e.uid, e.kind, e.display_name, e.confidence, e.resolution_status, e.create_time, e.properties,
		            (SELECT COUNT(*) FROM mentions m WHERE m.kind = e.kind AND m.display_name = e.display_name) AS mention_count,
		            COALESCE(d.canonical, 0)
		     FROM entities e` + join
		if len(conds) > 0 {
			q += " WHERE " + strings.Join(conds, " AND ")
		}
		return q + " ORDER BY mention_count DESC, e.create_time DESC LIMIT ? OFFSET ?"
	}
	q = `SELECT e.uid, e.kind, e.display_name, e.confidence, e.resolution_status, e.create_time, e.properties,
	            COALESCE(d.canonical, 0)
	     FROM entities e` + join
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	return q + " ORDER BY e.create_time DESC LIMIT ? OFFSET ?"
}

// scanEntity scans one entity row from rows. When withMentionCount is true,
// an extra mention_count integer column is scanned and discarded (it is
// selected only for ORDER BY, not surfaced on Entity). The final column is
// always COALESCE(d.canonical, 0) from the LEFT JOIN added by
// buildListEntitiesQuery; it maps to Entity.Canonical.
func scanEntity(rows *sql.Rows, withMentionCount bool) (Entity, error) {
	var e Entity
	var createTime, properties sql.NullString
	var canonicalInt int
	if withMentionCount {
		var mentionCount int64
		if err := rows.Scan(&e.UID, &e.Kind, &e.DisplayName, &e.Confidence, &e.ResolutionStatus, &createTime, &properties, &mentionCount, &canonicalInt); err != nil {
			return Entity{}, fmt.Errorf("scan entity: %w", err)
		}
	} else {
		if err := rows.Scan(&e.UID, &e.Kind, &e.DisplayName, &e.Confidence, &e.ResolutionStatus, &createTime, &properties, &canonicalInt); err != nil {
			return Entity{}, fmt.Errorf("scan entity: %w", err)
		}
	}
	e.CreateTime = createTime.String
	e.Canonical = canonicalInt != 0
	if properties.Valid && json.Valid([]byte(properties.String)) {
		e.Properties = json.RawMessage(properties.String)
	}
	return e, nil
}

// EpisodesCitingEntity returns episodes that mention the entity, newest first.
func (n *Navigator) EpisodesCitingEntity(ctx context.Context, entityUID string, limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	// Scope BOTH the outer episode (e) and the mention subquery (m) that proves
	// the citation: an out-of-scope mention must not surface an in-scope episode
	// as citing the entity.
	scopeE, scopeArgsE := n.scopeCond("e")
	scopeM, scopeArgsM := n.scopeCond("m")
	//nolint:gosec // G202: scope is constant fragments + bound ? params; all values parameterized
	q := `SELECT e.uid, e.create_time, e.payload_json
	           FROM episodes e
	           WHERE ` + scopeE + ` AND e.uid IN (
	               SELECT DISTINCT m.source_event_uid FROM mentions m
	               WHERE m.entity_uid = ? AND ` + scopeM + `
	           )
	           ORDER BY e.create_time DESC LIMIT ?`
	args := append([]any{}, scopeArgsE...)
	args = append(args, entityUID)
	args = append(args, scopeArgsM...)
	args = append(args, limit)
	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("episodes citing %q: %w", entityUID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Episode, 0, limit)
	for rows.Next() {
		ep, perr := scanEpisodeRow(rows)
		if perr != nil {
			return nil, perr
		}
		out = append(out, ep)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate episodes: %w", rerr)
	}
	return out, nil
}

// EpisodeFilter narrows ListEpisodes. Zero value lists recent episodes up to
// the default limit.
type EpisodeFilter struct {
	// Limit caps the number of returned episodes. 0 applies a safe default.
	Limit int
}

// ListEpisodes returns episodes within the navigator's current scope, newest
// first. It is the episode counterpart to ListEntities and is used by the
// draft-staging surface to enumerate staged episodes.
func (n *Navigator) ListEpisodes(ctx context.Context, opts EpisodeFilter) ([]Episode, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	scopeCond, scopeArgs := n.scopeCond("e")
	//nolint:gosec // G202: scope is constant fragments + bound ? params; all values parameterized
	q := `SELECT e.uid, e.create_time, e.payload_json
	           FROM episodes e
	           WHERE ` + scopeCond + `
	           ORDER BY e.create_time DESC LIMIT ?`
	args := append(scopeArgs, limit)
	rows, err := n.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list episodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Episode, 0, limit)
	for rows.Next() {
		ep, perr := scanEpisodeRow(rows)
		if perr != nil {
			return nil, perr
		}
		out = append(out, ep)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate episodes: %w", rerr)
	}
	return out, nil
}

// EventProvenance returns the provenance record(s) for an event UID — typically
// an Episode's own UID, or a relation's source_event_uid walked back one hop. An
// empty result (not ErrNotFound) is normal: provenance is supplementary lineage,
// and an event may have none recorded.
//
// Deliberately unscoped (#206): the provenance table is a pure lineage index
// keyed on event_uid with no scope column, and every caller reaches it only
// after a scope-filtered parent read (GetEpisode / GetRelation / GetFact), then
// passes that record's own event_uid. So this returns only the lineage of a
// record the caller can already see in scope — it cannot widen visibility. The
// scope-aware sibling EntityProvenance, which fans out across mentions, does
// filter, because there the entry point is an entity UID, not a vetted event.
func (n *Navigator) EventProvenance(ctx context.Context, eventUID string) ([]Provenance, error) {
	const q = `SELECT event_uid, source, confidence, decided_at
	           FROM provenance WHERE event_uid = ? ORDER BY decided_at, event_uid`
	return n.scanProvenance(ctx, q, eventUID)
}

// EntityProvenance aggregates provenance across every episode that mentions the
// entity. An entity has no provenance row of its own — the table is keyed on
// episode events — so its origin is the union of its mentions' source episodes.
func (n *Navigator) EntityProvenance(ctx context.Context, entityUID string) ([]Provenance, error) {
	scope, scopeArgs := n.scopeCond("m")

	q := `SELECT DISTINCT p.event_uid, p.source, p.confidence, p.decided_at
	           FROM provenance p
	           JOIN mentions m ON m.source_event_uid = p.event_uid
	           WHERE m.entity_uid = ? AND ` + scope + `
	           ORDER BY p.decided_at, p.event_uid`
	return n.scanProvenance(ctx, q, append([]any{entityUID}, scopeArgs...)...)
}

// scanProvenance runs a provenance query and collects the rows. Shared by
// EventProvenance and EntityProvenance; args binds the query's placeholders.
func (n *Navigator) scanProvenance(ctx context.Context, query string, args ...any) ([]Provenance, error) {
	rows, err := n.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("provenance query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Provenance
	for rows.Next() {
		var (
			p         Provenance
			decidedAt sql.NullString
		)
		if serr := rows.Scan(&p.EventUID, &p.Source, &p.Confidence, &decidedAt); serr != nil {
			return nil, fmt.Errorf("scan provenance: %w", serr)
		}
		p.DecidedAt = decidedAt.String
		out = append(out, p)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate provenance: %w", rerr)
	}
	return out, nil
}

// Predicate describes a registered relation predicate the agent can filter and
// traverse by. Mirrors the predicates table (migrations 0006 + 0017).
type Predicate struct {
	Predicate   string `json:"predicate"`
	Kind        string `json:"kind"` // "functional" | "non-functional"
	Description string `json:"description,omitempty"`
	// SubjectKinds / ObjectKinds are the predicate's declared domain/range
	// (P5 typed relations): the entity kinds allowed on each end. Empty means
	// any kind is accepted (open-world default).
	SubjectKinds []string `json:"subjectKinds,omitempty"`
	ObjectKinds  []string `json:"objectKinds,omitempty"`
	// SameAs lists alias IRIs to external vocabularies. Empty means none.
	SameAs []string `json:"sameAs,omitempty"`
}

// KindCount is an observed entity kind and the number of entities carrying it.
// Sourced from the live store (GROUP BY) rather than a static registry so the
// agent sees the kinds actually present, not a theoretical taxonomy.
type KindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// Predicates returns every registered predicate (seed + llm- + user-added),
// alphabetically, for the introspection surface.
func (n *Navigator) Predicates(ctx context.Context) ([]Predicate, error) {
	const q = `SELECT predicate, kind, description,
	                  COALESCE(subject_kinds,''), COALESCE(object_kinds,''), COALESCE(same_as,'')
	           FROM predicates ORDER BY predicate`
	rows, err := n.reader.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list predicates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Predicate{}
	for rows.Next() {
		var (
			p                          Predicate
			subjRaw, objRaw, sameAsRaw string
		)
		if serr := rows.Scan(&p.Predicate, &p.Kind, &p.Description, &subjRaw, &objRaw, &sameAsRaw); serr != nil {
			return nil, fmt.Errorf("scan predicate: %w", serr)
		}
		// P5 columns are JSON text arrays; malformed JSON degrades to nil
		// rather than failing the whole listing.
		if subjRaw != "" {
			if uerr := json.Unmarshal([]byte(subjRaw), &p.SubjectKinds); uerr != nil {
				p.SubjectKinds = nil
			}
		}
		if objRaw != "" {
			if uerr := json.Unmarshal([]byte(objRaw), &p.ObjectKinds); uerr != nil {
				p.ObjectKinds = nil
			}
		}
		if sameAsRaw != "" {
			if uerr := json.Unmarshal([]byte(sameAsRaw), &p.SameAs); uerr != nil {
				p.SameAs = nil
			}
		}
		out = append(out, p)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate predicates: %w", rerr)
	}
	return out, nil
}

// EntityKinds returns the distinct entity kinds present in the store with their
// counts, most populous first. Lets an agent discover what kinds exist before
// filtering ListEntities by one.
func (n *Navigator) EntityKinds(ctx context.Context) ([]KindCount, error) {
	scope, scopeArgs := n.scopeCond("")
	//nolint:gosec // G202: scope is constant fragments + bound ? params
	q := `SELECT kind, COUNT(*) AS n FROM entities WHERE ` + scope + ` GROUP BY kind ORDER BY n DESC, kind`
	rows, err := n.reader.QueryContext(ctx, q, scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("entity kinds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []KindCount{}
	for rows.Next() {
		var kc KindCount
		if serr := rows.Scan(&kc.Kind, &kc.Count); serr != nil {
			return nil, fmt.Errorf("scan kind count: %w", serr)
		}
		out = append(out, kc)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate kind counts: %w", rerr)
	}
	return out, nil
}

// RegisteredSchema is one row from the ontology_schemas projection table.
// Returned by RegisteredSchemas for the schema-preview surface so callers can
// detect a supersede without importing internal/ontology/dedup (which would
// create a circular dependency: dedup already imports navigate).
type RegisteredSchema struct {
	// SchemaKind is the schema kind identifier, e.g. "entityKind:vehicle".
	SchemaKind string `json:"schemaKind"`
	// Version is the currently registered semver string, e.g. "v1.0.0".
	Version string `json:"version"`
	// Definition is the current JSON Schema document.
	Definition json.RawMessage `json:"definition"`
}

// RegisteredSchemas returns every row in the ontology_schemas projection table,
// ordered by (schema_kind, version). This is the authoring read surface used by
// the defineSchema preview path to detect whether a new registration would
// supersede an existing one.
func (n *Navigator) RegisteredSchemas(ctx context.Context) ([]RegisteredSchema, error) {
	const q = `SELECT schema_kind, version, definition
	           FROM ontology_schemas ORDER BY schema_kind, version`
	rows, err := n.reader.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("registered schemas: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RegisteredSchema
	for rows.Next() {
		var s RegisteredSchema
		var defText string
		if serr := rows.Scan(&s.SchemaKind, &s.Version, &defText); serr != nil {
			return nil, fmt.Errorf("scan registered schema: %w", serr)
		}
		if json.Valid([]byte(defText)) {
			s.Definition = json.RawMessage(defText)
		}
		out = append(out, s)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate registered schemas: %w", rerr)
	}
	return out, nil
}

// RegisteredEntityKind is one row from the entity_kinds projection table.
// Returned by RegisteredEntityKinds for the introspect and describeType
// surfaces (wired in P2).
type RegisteredEntityKind struct {
	// Name is the kind identifier, e.g. "person" or "user:vehicle".
	Name string `json:"name"`
	// DisplayName is the human-readable label.
	DisplayName string `json:"displayName"`
	// Description is a one-sentence explanation of the kind.
	Description string `json:"description"`
	// Parent is the parent kind for the subtype hierarchy. Empty for root kinds.
	Parent string `json:"parent,omitempty"`
	// SameAs lists alias IRIs to canonical or external vocabularies, e.g.
	// ["https://schema.org/Person"]. Nil when absent.
	SameAs []string `json:"sameAs,omitempty"`
	// Provenance is the namespace lane: "core", "user", or "extension:<name>".
	Provenance string `json:"provenance"`
	// AddedBy identifies the agent that registered the kind.
	AddedBy string `json:"addedBy"`
	// PropertySchema is the kind's JSON Schema for typed properties (the
	// attr_schema column). Nil when the kind declares no typed properties.
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"`
}

// RegisteredEntityKinds returns every ACTIVE row in the entity_kinds
// projection table, alphabetically by name. This is the durable registry built
// from entityKindRegistered events; it differs from EntityKinds (which counts
// instances in the entities table) in that it always includes core kinds even
// when no entity of that kind has been ingested yet.
//
// Proposed and rejected kinds are excluded: this listing is the agent-facing
// active taxonomy (introspect, describeType, createEntity preflight, dedup),
// and in governed mode a freshly minted kind must stay out of that taxonomy
// until `maz schema review approve`. The governance queue reads proposed rows
// through its own status-scoped query, not through this method.
func (n *Navigator) RegisteredEntityKinds(ctx context.Context) ([]RegisteredEntityKind, error) {
	const q = `SELECT name, display_name, description, COALESCE(parent,''),
	                  COALESCE(same_as,''), provenance, added_by,
	                  COALESCE(attr_schema,'')
	           FROM entity_kinds
	          WHERE COALESCE(status,'active') = 'active'
	          ORDER BY name`
	rows, err := n.reader.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("registered entity kinds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []RegisteredEntityKind{}
	for rows.Next() {
		var k RegisteredEntityKind
		var sameAsJSON, attrSchemaRaw string
		if serr := rows.Scan(&k.Name, &k.DisplayName, &k.Description, &k.Parent, &sameAsJSON, &k.Provenance, &k.AddedBy, &attrSchemaRaw); serr != nil {
			return nil, fmt.Errorf("scan registered entity kind: %w", serr)
		}
		if sameAsJSON != "" {
			// Defensive unmarshal: a malformed same_as blob must not abort
			// the entire listing. On error SameAs stays nil and the row is
			// still returned with all other fields intact.
			var iris []string
			if err := json.Unmarshal([]byte(sameAsJSON), &iris); err == nil {
				k.SameAs = iris
			}
		}
		if attrSchemaRaw != "" {
			// Keep as raw JSON so callers can pass it through without
			// allocating an intermediate map. Malformed blobs are dropped.
			if json.Valid([]byte(attrSchemaRaw)) {
				k.PropertySchema = json.RawMessage(attrSchemaRaw)
			}
		}
		out = append(out, k)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate registered entity kinds: %w", rerr)
	}
	return out, nil
}

// --- helpers ---

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRelation reads one relation row in the canonical column order used by
// the Get/EntityRelations queries (with subject/object names LEFT JOINed).
// RelationStatusAssertedDefault is the authoritative trust posture a relation
// reads back as when its stored status is empty (a legacy row predating the
// status column). Mirrors ontology.RelationStatusAsserted as a local literal so
// the navigate package stays free of an ontology import.
const relationStatusAssertedDefault = "asserted"

func scanRelation(s rowScanner) (Relation, error) {
	var (
		rel                                          Relation
		subjName, objName                            sql.NullString
		validFrom, validUntil, supersededBy, attrRaw sql.NullString
		sourceEventUID, provenance, status           sql.NullString
		retracted                                    bool
	)
	if err := s.Scan(
		&rel.UID, &rel.SubjectUID, &subjName, &rel.Predicate,
		&rel.ObjectUID, &objName, &rel.Confidence, &rel.CreateTime,
		&validFrom, &validUntil, &supersededBy, &attrRaw,
		&sourceEventUID, &provenance, &status, &retracted,
	); err != nil {
		return Relation{}, err
	}
	rel.SubjectName = subjName.String
	rel.ObjectName = objName.String
	rel.SourceEventUID = sourceEventUID.String
	rel.ValidFrom = validFrom.String
	rel.ValidUntil = validUntil.String
	rel.SupersededByUID = supersededBy.String
	rel.Provenance = provenance.String
	// A legacy/empty stored status reads back as the authoritative default so
	// the trust posture is always explicit to a consumer (pre-status relation
	// rows predate the column).
	rel.Status = status.String
	if rel.Status == "" {
		rel.Status = relationStatusAssertedDefault
	}
	if attrRaw.Valid && attrRaw.String != "" && attrRaw.String != "{}" {
		attrs := map[string]string{}
		if err := json.Unmarshal([]byte(attrRaw.String), &attrs); err == nil && len(attrs) > 0 {
			rel.Attributes = attrs
		}
	}
	rel.Retracted = retracted
	return rel, nil
}

// scanEpisodeRow reads (uid, create_time, payload_json) and parses the
// payload for text/source/confidence.
func scanEpisodeRow(s rowScanner) (Episode, error) {
	var (
		ep      Episode
		payload string
	)
	if err := s.Scan(&ep.UID, &ep.CreateTime, &payload); err != nil {
		return Episode{}, fmt.Errorf("scan episode: %w", err)
	}
	var p struct {
		Text       string  `json:"text"`
		Source     string  `json:"source"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return Episode{}, fmt.Errorf("decode episode %q payload: %w", ep.UID, err)
	}
	ep.Text, ep.Source, ep.Confidence = p.Text, p.Source, p.Confidence
	return ep, nil
}

// appendRelationFilter adds predicate + validity conditions to a relations
// query. Timestamps compare via julianday() so RFC3339 values of differing
// sub-second precision still order correctly.
func appendRelationFilter(conds []string, args []any, opts RelationFilter) ([]string, []any) {
	if len(opts.Predicates) > 0 {
		ph := make([]string, len(opts.Predicates))
		for i, p := range opts.Predicates {
			ph[i] = "?"
			args = append(args, p)
		}
		conds = append(conds, "r.predicate IN ("+strings.Join(ph, ",")+")")
	}
	if !opts.IncludeInactive {
		now := time.Now().UTC()
		if opts.At != nil {
			now = opts.At.UTC()
		}
		nowStr := now.Format(time.RFC3339Nano)
		conds = append(conds,
			"(r.valid_from IS NULL OR r.valid_from = '' OR julianday(r.valid_from) <= julianday(?))",
			"(r.valid_until IS NULL OR r.valid_until = '' OR julianday(?) < julianday(r.valid_until))",
			"(r.superseded_by_uid IS NULL OR r.superseded_by_uid = '')",
		)
		args = append(args, nowStr, nowStr)
	}
	if opts.AsOf != nil {
		conds = append(conds, "julianday(r.create_time) <= julianday(?)")
		args = append(args, opts.AsOf.UTC().Format(time.RFC3339Nano))
	}
	return conds, args
}

// otherEndpoint returns the endpoint of rel that is not node, and the edge
// direction relative to node ("outgoing" when node is the subject).
func otherEndpoint(rel Relation, node string) (uid, direction string) {
	switch node {
	case rel.SubjectUID:
		return rel.ObjectUID, "outgoing"
	case rel.ObjectUID:
		return rel.SubjectUID, "incoming"
	default:
		return "", ""
	}
}

// escapeLike escapes LIKE wildcards in a user-supplied substring so it matches
// literally (paired with ESCAPE '\' in the query).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
