package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/mod/semver"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// statusOrphaned is the resolution_status string written when an entity has
// lost all its mention provenance via episodeForgotten cascades.
const statusOrphaned = "orphaned"

// provenanceCore is the namespace lane for built-in taxonomy rows. A non-core
// event cannot overwrite a core row in the rendered state (mirrors the
// dispatcher's core-provenance guard).
const provenanceCore = "core"

// EntityRow is the renderer's in-memory view of one entity.
type EntityRow struct {
	UID              string
	Kind             ontology.EntityKind
	DisplayName      string
	Aliases          []string
	Identifiers      []ontology.Identifier
	Confidence       float64
	ResolutionStatus ontology.ResolutionStatus
	CreateTime       time.Time
	LastUpdated      time.Time
	MentionUIDs      []string
	EpisodeUIDs      []string
	Orphaned         bool
	// Properties holds the entity's typed attributes (P3) as a compacted
	// JSON object, replaced wholesale by the latest entity event that
	// carries one. Compacted at apply time so the rendered bytes stay
	// stable regardless of source whitespace.
	Properties json.RawMessage
}

// RelationRow is the renderer's in-memory view of one relation edge.
type RelationRow struct {
	UID             string
	SubjectUID      string
	Predicate       string
	ObjectUID       string
	Confidence      float64
	ValidFrom       *time.Time
	ValidUntil      *time.Time
	Attributes      map[string]string
	SupersededByUID string
	// Retractions is this relation's monotonic tombstone set, keyed by the
	// relationRetracted event UID so set-union is idempotent. SupersededByUID
	// and ValidUntil are a deterministic fold (foldRetractions →
	// ontology.ReduceRetractions) over it, so the rendered close converges
	// across paired devices regardless of replay order.
	Retractions     map[string]ontology.Tombstone
	Forgotten       bool
	ForgottenAt     *time.Time
	ForgottenReason string
	ConflictWith    string
}

// PredicateRow holds a registered predicate, including the v0.2 typed-relation
// metadata (domain/range, external-vocabulary aliases, attribute schema) so
// the git-rendered ontology/predicates.yaml carries the same typing the
// SQLite projection enforces at runtime.
type PredicateRow struct {
	Predicate      string
	Kind           ontology.PredicateKind
	Description    string
	SubjectKinds   []string
	ObjectKinds    []string
	SameAs         []string
	PropertySchema json.RawMessage
	AddedBy        ontology.PredicateAddedBy
	AddedAt        time.Time
}

// clonePredicateRow returns an independent copy of the row with its
// reference-type fields (kind slices, sameAs, propertySchema) deep-copied.
func clonePredicateRow(v *PredicateRow) *PredicateRow {
	cp := *v
	if len(v.SubjectKinds) > 0 {
		cp.SubjectKinds = append([]string(nil), v.SubjectKinds...)
	}
	if len(v.ObjectKinds) > 0 {
		cp.ObjectKinds = append([]string(nil), v.ObjectKinds...)
	}
	if len(v.SameAs) > 0 {
		cp.SameAs = append([]string(nil), v.SameAs...)
	}
	if len(v.PropertySchema) > 0 {
		cp.PropertySchema = append(json.RawMessage(nil), v.PropertySchema...)
	}
	return &cp
}

// equal reports whether two predicate rows carry identical rendered state.
// PredicateRow is no longer comparable with == (it holds slices), so the
// repeat-registration no-change check in applyPredicateRegistered uses this.
func (p *PredicateRow) equal(o *PredicateRow) bool {
	return p.Predicate == o.Predicate &&
		p.Kind == o.Kind &&
		p.Description == o.Description &&
		slices.Equal(p.SubjectKinds, o.SubjectKinds) &&
		slices.Equal(p.ObjectKinds, o.ObjectKinds) &&
		slices.Equal(p.SameAs, o.SameAs) &&
		bytes.Equal(p.PropertySchema, o.PropertySchema) &&
		p.AddedBy == o.AddedBy &&
		p.AddedAt.Equal(o.AddedAt)
}

// FactRow holds a free-text fact.
type FactRow struct {
	UID            string
	SubjectUID     string
	StateKey       string
	Value          string
	CreateTime     time.Time
	SourceEventUID string
}

// EntityKindRow holds a registered entity kind from an entityKindRegistered
// event. Used to render the ontology/kinds/ tree in the git-synced repo.
type EntityKindRow struct {
	Name           string
	DisplayName    string
	Description    string
	Extends        string
	SameAs         []string
	PropertySchema json.RawMessage
	Provenance     string
	AddedBy        string
	AddedAt        time.Time
	// Status is the governance status: "active", "proposed", or "rejected".
	// Empty is treated as "active" for backward compatibility with pre-P7 events.
	Status string
}

// SchemaRow holds a runtime-registered schema from a schemaRegistered event.
// Used to render the ontology/schemas/ tree so dynamic schemas travel through
// the git transport alongside the SQLite ontology_schemas projection.
type SchemaRow struct {
	SchemaKind string
	Version    string
	Definition json.RawMessage
	Provenance string
	AddedBy    string
	AddedAt    time.Time
}

// PresetRow is the rendered lockfile metadata for one active preset (#210). It
// carries ONLY name/version/description — there is deliberately NO Instructions
// field. The verbatim instructions body is render-private: its structural
// absence here is the guarantee that an adversarial body can never reach the
// plaintext git tree, on any scope. The file's mere existence under
// ontology/presets/ marks the preset active; a presetDeactivated event deletes
// the row, and the diff drops the file.
type PresetRow struct {
	Name        string
	Version     string
	Description string
}

// DocumentRow is the renderer's head-state view of one vault document. Only the
// latest revision's verbatim body is retained; the full revision chain lives in
// the event log (surfaced by `maz doc log`). A document is also an entity, so a
// companion EntityRow (Kind=document) carries its mentions/links_to edges; this
// row carries only what the entity stub cannot: the verbatim body that renders
// to rendered/documents/<slug>.md byte-identically to the editable vault.
type DocumentRow struct {
	UID         string
	Path        string
	Title       string
	Body        string // verbatim full file (frontmatter included)
	ContentHash string
	RevisionUID string // event UID of the latest revision
	Pinned      bool
	Tags        []string
	CreateTime  time.Time
	LastUpdated time.Time
	Deleted     bool
}

// GraphState is the accumulated in-memory graph built by applying events.
// It is exported so callers can cache it between incremental renders.
type GraphState struct {
	// Entities maps entity UID to its current row.
	Entities map[string]*EntityRow
	// Relations maps relation UID to its row (includes superseded/forgotten).
	Relations map[string]*RelationRow
	// Slugs maps entity UID to its frozen filename slug.
	Slugs map[string]string
	// Predicates maps predicate string to its row.
	Predicates map[string]*PredicateRow
	// EntityKinds maps kind name to its registered row.
	EntityKinds map[string]*EntityKindRow
	// Schemas maps schemaKind to its runtime-registered schema row.
	Schemas map[string]*SchemaRow
	// Presets maps preset name to its active lockfile-metadata row (#210). Holds
	// only active presets: presetInstalled adds, presetDeactivated removes.
	Presets map[string]*PresetRow
	// Facts holds free-text facts in append order.
	Facts []*FactRow
	// SourceRefs maps SourceRef UID to its row (#207). Keyed (not a slice) so a
	// re-recorded dedup_key — which derives the same UID — collapses to one row
	// with last-write-wins values, mirroring the source_refs UNIQUE(dedup_key)
	// upsert. A slice would emit duplicate-uid lines and, since the uid sort is
	// not stable, render non-deterministically.
	SourceRefs map[string]*SourceRefRow
	// Documents maps document UID to its head-state row (latest revision).
	Documents map[string]*DocumentRow
	// DocSlugs maps document UID to its frozen filename slug. Frozen at the
	// first revision so a moveDocument (path change) never moves the rendered
	// file and breaks git history.
	DocSlugs map[string]string
	// Cursor is the last applied event ULID.
	Cursor string
	// episodeMentions maps episodeUID -> set of entity UIDs that have mentions from it.
	episodeMentions map[string]map[string]struct{}
	// pendingRetractions buffers relationRetracted tombstones whose target
	// relation row has not been applied yet (a retraction replayed before its
	// relation event). Keyed by relation UID -> retraction event UID ->
	// tombstone; drained into the row by applyRelation when it lands.
	pendingRetractions map[string]map[string]ontology.Tombstone

	// relBySubject is a per-render scratch index built lazily by
	// relationsForSubject. It maps subject entity UID -> relation
	// rows sorted by UID. Without it, every entity stub does a
	// full O(R log R) scan over state.Relations, four times (once
	// per supersede/forgotten/conflict/current section), making
	// the total cost O(E * R log R) -- 425 s for 10k entities x
	// 50k relations. Lazy + per-state lets clone() invalidate
	// trivially (the field zero-values, next access rebuilds).
	relBySubject     map[string][]*RelationRow
	relBySubjectOnce sync.Once
}

// relationsForSubject returns every RelationRow whose SubjectUID
// equals subjectUID, sorted by relation UID. The result is the same
// the old per-entity scan produced; the difference is that we build
// the bucketed index once and reuse it across every entity stub
// emission, cutting the dominant cost from O(E * R log R) to
// O(R log R + E + sum(matches)).
//
// The index is lazily built and per-state; cloneGraphState yields
// a fresh zero-value Once, so a clone (used as PriorState) rebuilds
// independently the first time it's queried.
func (g *GraphState) relationsForSubject(subjectUID string) []*RelationRow {
	g.relBySubjectOnce.Do(func() {
		idx := make(map[string][]*RelationRow, len(g.Entities))
		for _, uid := range sortedKeys(g.Relations) {
			r := g.Relations[uid]
			idx[r.SubjectUID] = append(idx[r.SubjectUID], r)
		}
		g.relBySubject = idx
	})
	return g.relBySubject[subjectUID]
}

// newGraphState creates an empty GraphState ready for apply calls.
func newGraphState() *GraphState {
	return &GraphState{
		Entities:        make(map[string]*EntityRow),
		Relations:       make(map[string]*RelationRow),
		Slugs:           make(map[string]string),
		Predicates:      make(map[string]*PredicateRow),
		EntityKinds:     make(map[string]*EntityKindRow),
		Schemas:         make(map[string]*SchemaRow),
		Presets:         make(map[string]*PresetRow),
		SourceRefs:      make(map[string]*SourceRefRow),
		Documents:       make(map[string]*DocumentRow),
		DocSlugs:        make(map[string]string),
		episodeMentions: make(map[string]map[string]struct{}),
	}
}

// cloneGraphState produces a shallow-enough copy for prior-state diffing.
func cloneGraphState(src *GraphState) *GraphState {
	if src == nil {
		return nil
	}
	dst := &GraphState{
		Entities:        make(map[string]*EntityRow, len(src.Entities)),
		Relations:       make(map[string]*RelationRow, len(src.Relations)),
		Slugs:           make(map[string]string, len(src.Slugs)),
		Predicates:      make(map[string]*PredicateRow, len(src.Predicates)),
		EntityKinds:     make(map[string]*EntityKindRow, len(src.EntityKinds)),
		Schemas:         make(map[string]*SchemaRow, len(src.Schemas)),
		Presets:         make(map[string]*PresetRow, len(src.Presets)),
		Facts:           make([]*FactRow, len(src.Facts)),
		SourceRefs:      make(map[string]*SourceRefRow, len(src.SourceRefs)),
		Documents:       make(map[string]*DocumentRow, len(src.Documents)),
		DocSlugs:        make(map[string]string, len(src.DocSlugs)),
		Cursor:          src.Cursor,
		episodeMentions: make(map[string]map[string]struct{}, len(src.episodeMentions)),
	}
	for k, v := range src.Entities {
		cp := *v
		dst.Entities[k] = &cp
	}
	for k, v := range src.Relations {
		dst.Relations[k] = cloneRelationRow(v)
	}
	for k, v := range src.Slugs {
		dst.Slugs[k] = v
	}
	for k, v := range src.Predicates {
		dst.Predicates[k] = clonePredicateRow(v)
	}
	for k, v := range src.EntityKinds {
		dst.EntityKinds[k] = cloneEntityKindRow(v)
	}
	for k, v := range src.Schemas {
		cp := *v
		cp.Definition = append(json.RawMessage(nil), v.Definition...)
		dst.Schemas[k] = &cp
	}
	for k, v := range src.Presets {
		cp := *v
		dst.Presets[k] = &cp
	}
	copy(dst.Facts, src.Facts)
	for k, v := range src.SourceRefs {
		cp := *v
		dst.SourceRefs[k] = &cp
	}
	for k, v := range src.Documents {
		cp := *v
		dst.Documents[k] = &cp
	}
	for k, v := range src.DocSlugs {
		dst.DocSlugs[k] = v
	}
	for k, v := range src.episodeMentions {
		set := make(map[string]struct{}, len(v))
		for eid := range v {
			set[eid] = struct{}{}
		}
		dst.episodeMentions[k] = set
	}
	dst.pendingRetractions = clonePendingRetractions(src.pendingRetractions)
	return dst
}

// cloneRelationRow deep-copies a RelationRow, including its tombstone set.
// ApplyRelationRetracted mutates Retractions in place, so a shared map would
// leak new tombstones into the prior-state clone and corrupt diffing.
func cloneRelationRow(v *RelationRow) *RelationRow {
	cp := *v
	if len(v.Retractions) > 0 {
		cp.Retractions = make(map[string]ontology.Tombstone, len(v.Retractions))
		for rk, rv := range v.Retractions {
			cp.Retractions[rk] = rv
		}
	}
	return &cp
}

// cloneEntityKindRow deep-copies an EntityKindRow, including its slice/raw-JSON
// reference fields, so the clone (used as PriorState) is independent of the
// source.
func cloneEntityKindRow(v *EntityKindRow) *EntityKindRow {
	cp := *v
	if len(v.SameAs) > 0 {
		cp.SameAs = append([]string(nil), v.SameAs...)
	}
	if len(v.PropertySchema) > 0 {
		cp.PropertySchema = append(json.RawMessage(nil), v.PropertySchema...)
	}
	return &cp
}

// clonePendingRetractions deep-copies the retracted-before-relation buffer so
// the clone is independent of the source's nested maps. Returns nil for an
// empty source to preserve the lazy-init contract.
func clonePendingRetractions(src map[string]map[string]ontology.Tombstone) map[string]map[string]ontology.Tombstone {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]map[string]ontology.Tombstone, len(src))
	for relUID, pending := range src {
		cp := make(map[string]ontology.Tombstone, len(pending))
		for evUID, t := range pending {
			cp[evUID] = t
		}
		dst[relUID] = cp
	}
	return dst
}

// apply processes one event and updates the state. Data events (episodes,
// mentions, entities, relations, facts) dispatch here; ontology-registry
// events (the type system itself) dispatch via applyOntologyEvent so each
// switch stays small as the ontology grows.
func (g *GraphState) apply(ev kernel.Event) (bool, error) {
	switch ev.Kind {
	case ontology.KindEpisode:
		return g.applyEpisode(ev)
	case ontology.KindEpisodeForgotten:
		return g.applyEpisodeForgotten(ev)
	case ontology.KindMention:
		return g.applyMention(ev)
	case ontology.KindEntity:
		return g.applyEntity(ev)
	case ontology.KindEntityIdentifierAdded:
		return g.applyEntityIdentifierAdded(ev)
	case ontology.KindRelation:
		return g.applyRelation(ev)
	case ontology.KindRelationRetracted:
		return g.applyRelationRetracted(ev)
	case ontology.KindRelationForgotten:
		return g.applyRelationForgotten(ev)
	case ontology.KindRelationSupersedeConflict:
		return g.applyRelationSupersedeConflict(ev)
	case ontology.KindFact:
		return g.applyFact(ev)
	case ontology.KindSourceRefRecorded:
		return g.applySourceRefRecorded(ev)
	case ontology.KindDocumentRevision:
		return g.applyDocumentRevision(ev)
	case ontology.KindDocumentDeleted:
		return g.applyDocumentDeleted(ev)
	default:
		return g.applyOntologyEvent(ev)
	}
}

// applyOntologyEvent dispatches the ontology-registry event kinds — the type
// system itself: predicates, entity kinds, reviews, custom schemas. Unhandled
// kinds (including relationCandidatesProposed) advance the cursor without
// changing rendered state.
func (g *GraphState) applyOntologyEvent(ev kernel.Event) (bool, error) {
	switch ev.Kind {
	case ontology.KindPredicateRegistered:
		return g.applyPredicateRegistered(ev)
	case ontology.KindEntityKindRegistered:
		return g.applyEntityKindRegistered(ev)
	case ontology.KindEntityKindReviewed:
		return g.applyEntityKindReviewed(ev)
	case ontology.KindSchemaRegistered:
		return g.applySchemaRegistered(ev)
	case ontology.KindPresetInstalled:
		return g.applyPresetInstalled(ev)
	case ontology.KindPresetDeactivated:
		return g.applyPresetDeactivated(ev)
	default:
		g.Cursor = ev.UID
		return false, nil
	}
}

// applyPresetInstalled records an active preset's lockfile metadata. The
// verbatim Instructions body is DELIBERATELY NOT copied into the row — it never
// enters rendered state, so it can never leak to the plaintext git tree.
func (g *GraphState) applyPresetInstalled(ev kernel.Event) (bool, error) {
	var payload ontology.PresetInstalledEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal presetInstalled: %w", err)
	}
	g.Presets[payload.Name] = &PresetRow{
		Name:        payload.Name,
		Version:     payload.Version,
		Description: payload.Description,
	}
	g.Cursor = ev.UID
	return true, nil
}

// applyPresetDeactivated drops the preset's row so the diff emits a delete for
// its ontology/presets/<name>.yaml file (clean cut). Idempotent: deactivating an
// absent preset changes nothing.
func (g *GraphState) applyPresetDeactivated(ev kernel.Event) (bool, error) {
	var payload ontology.PresetDeactivatedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal presetDeactivated: %w", err)
	}
	_, existed := g.Presets[payload.Name]
	delete(g.Presets, payload.Name)
	g.Cursor = ev.UID
	return existed, nil
}

func (g *GraphState) applyEpisode(ev kernel.Event) (bool, error) {
	g.Cursor = ev.UID
	return false, nil
}

func (g *GraphState) applyEpisodeForgotten(ev kernel.Event) (bool, error) {
	var payload ontology.EpisodeForgottenEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal episodeForgotten: %w", err)
	}

	epUID := payload.EpisodeUID
	entitySet, ok := g.episodeMentions[epUID]
	if !ok {
		g.Cursor = ev.UID
		return false, nil
	}

	changed := false
	for entityUID := range entitySet {
		row, exists := g.Entities[entityUID]
		if !exists {
			continue
		}
		row.EpisodeUIDs = removeString(row.EpisodeUIDs, epUID)
		if len(row.EpisodeUIDs) == 0 && len(row.MentionUIDs) > 0 {
			row.MentionUIDs = nil
		}
		row.Orphaned = len(row.MentionUIDs) == 0 && len(row.EpisodeUIDs) == 0
		changed = true
	}
	delete(g.episodeMentions, epUID)
	g.Cursor = ev.UID
	return changed, nil
}

func (g *GraphState) applyMention(ev kernel.Event) (bool, error) {
	var payload ontology.Mention
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal mention: %w", err)
	}

	epUID := string(payload.SourceEventUID)
	mentionUID := ev.UID

	if g.episodeMentions[epUID] == nil {
		g.episodeMentions[epUID] = make(map[string]struct{})
	}
	g.episodeMentions[epUID][mentionUID] = struct{}{}
	g.Cursor = ev.UID
	return false, nil
}

func (g *GraphState) applyEntity(ev kernel.Event) (bool, error) {
	var payload ontology.Entity
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal entity: %w", err)
	}

	// Key entities by the payload-carried entity UID, not the
	// kernel event UID. Other events (relation.subjectUid,
	// entityIdentifierAdded.entityUid, relationRetracted.relationUid)
	// reference the payload UID; mis-keying here would break every
	// downstream cross-reference and silently empty supersede /
	// forgotten / identifier outputs. ev.UID is preserved on the
	// Cursor below to track event-log position.
	uid := payload.UID
	if uid == "" {
		uid = ev.UID
	}
	existing, exists := g.Entities[uid]
	if !exists {
		if _, hasSlug := g.Slugs[uid]; !hasSlug {
			g.Slugs[uid] = slugify(payload.DisplayName, uid)
		}
		row := &EntityRow{
			UID:              uid,
			Kind:             payload.Kind,
			DisplayName:      payload.DisplayName,
			Confidence:       payload.Confidence,
			ResolutionStatus: payload.ResolutionStatus,
			CreateTime:       payload.CreateTime.UTC(),
			LastUpdated:      ev.CreateTime.UTC(),
			Properties:       compactRawJSON(payload.Properties),
		}
		for _, m := range payload.MentionUIDs {
			row.MentionUIDs = appendUnique(row.MentionUIDs, string(m))
			g.registerMentionEpisode(string(m), uid)
		}
		for _, alias := range payload.AlternateNames {
			row.Aliases = appendUnique(row.Aliases, alias)
		}
		row.Identifiers = append(row.Identifiers, payload.Identifiers...)
		g.Entities[uid] = row
		g.Cursor = ev.UID
		return true, nil
	}

	return g.updateEntity(existing, payload, ev)
}

func (g *GraphState) updateEntity(existing *EntityRow, payload ontology.Entity, ev kernel.Event) (bool, error) {
	changed := false
	if existing.DisplayName != payload.DisplayName {
		existing.DisplayName = payload.DisplayName
		changed = true
	}
	if existing.Confidence != payload.Confidence {
		existing.Confidence = payload.Confidence
		changed = true
	}
	if existing.ResolutionStatus != payload.ResolutionStatus {
		existing.ResolutionStatus = payload.ResolutionStatus
		changed = true
	}
	for _, alias := range payload.AlternateNames {
		if appendUniqueChanged(&existing.Aliases, alias) {
			changed = true
		}
	}
	for _, m := range payload.MentionUIDs {
		if appendUniqueChanged(&existing.MentionUIDs, string(m)) {
			g.registerMentionEpisode(string(m), ev.UID)
			changed = true
		}
	}
	// Typed properties replace wholesale when the incoming event carries
	// them; an event without properties keeps the stored object (mirrors
	// the dispatcher's COALESCE upsert semantics).
	if props := compactRawJSON(payload.Properties); len(props) > 0 && !bytes.Equal(existing.Properties, props) {
		existing.Properties = props
		changed = true
	}
	if changed {
		existing.LastUpdated = ev.CreateTime.UTC()
	}
	g.Cursor = ev.UID
	return changed, nil
}

// registerMentionEpisode wires a mention UID to an entity in the episode index.
func (g *GraphState) registerMentionEpisode(mentionUID, entityUID string) {
	for epUID, set := range g.episodeMentions {
		if _, ok := set[mentionUID]; ok {
			delete(set, mentionUID)
			set[entityUID] = struct{}{}
			g.episodeMentions[epUID] = set
			if row, exists := g.Entities[entityUID]; exists {
				row.EpisodeUIDs = appendUnique(row.EpisodeUIDs, epUID)
			}
			return
		}
	}
}

func (g *GraphState) applyEntityIdentifierAdded(ev kernel.Event) (bool, error) {
	var payload ontology.EntityIdentifierAddedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal entityIdentifierAdded: %w", err)
	}

	row, exists := g.Entities[payload.EntityUID]
	if !exists {
		g.Cursor = ev.UID
		return false, nil
	}

	if payload.Kind == ontology.IdentifierKindAlias {
		if appendUniqueChanged(&row.Aliases, payload.Identifier) {
			row.LastUpdated = ev.CreateTime.UTC()
			g.Cursor = ev.UID
			return true, nil
		}
		g.Cursor = ev.UID
		return false, nil
	}

	for _, id := range row.Identifiers {
		if id.Type == string(payload.Kind) && id.Value == payload.Identifier {
			g.Cursor = ev.UID
			return false, nil
		}
	}
	row.Identifiers = append(row.Identifiers, ontology.Identifier{
		Type:  string(payload.Kind),
		Value: payload.Identifier,
	})
	row.LastUpdated = ev.CreateTime.UTC()
	g.Cursor = ev.UID
	return true, nil
}

func (g *GraphState) applyRelation(ev kernel.Event) (bool, error) {
	var payload ontology.Relation
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal relation: %w", err)
	}

	// Key relations by payload UID so relationRetracted /
	// relationForgotten lookups (which reference payload.RelationUID)
	// hit the same row this branch created.
	uid := payload.UID
	if uid == "" {
		uid = ev.UID
	}
	if _, exists := g.Relations[uid]; exists {
		g.Cursor = ev.UID
		return false, nil
	}

	row := &RelationRow{
		UID:        uid,
		SubjectUID: payload.SubjectUID,
		Predicate:  payload.Predicate,
		ObjectUID:  payload.ObjectUID,
		Confidence: payload.Confidence,
		ValidFrom:  payload.ValidFrom,
		ValidUntil: payload.ValidUntil,
		Attributes: payload.Attributes,
	}
	if payload.SupersededByUID != nil {
		row.SupersededByUID = string(*payload.SupersededByUID)
	}
	g.Relations[uid] = row

	// Fold in any retraction tombstones that arrived before this relation
	// event (retracted-before-relation replay order). Drains the pending
	// buffer so the close materializes deterministically once the row lands.
	if pending := g.pendingRetractions[uid]; len(pending) > 0 {
		if row.Retractions == nil {
			row.Retractions = make(map[string]ontology.Tombstone, len(pending))
		}
		for evUID, t := range pending {
			row.Retractions[evUID] = t
		}
		delete(g.pendingRetractions, uid)
		foldRetractions(row)
	}

	g.Cursor = ev.UID
	return true, nil
}

func (g *GraphState) applyRelationRetracted(ev kernel.Event) (bool, error) {
	var payload ontology.RelationRetractedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal relationRetracted: %w", err)
	}

	relUID := string(payload.RelationUID)
	tombstone := ontology.Tombstone{
		SupersededByUID: string(payload.SupersededByUID),
		RetractedAt:     payload.RetractedAt.UTC(),
	}

	row, exists := g.Relations[relUID]
	if !exists {
		// Retraction replayed before its relation event: buffer it, keyed by
		// retraction event UID for idempotent set-union, and let applyRelation
		// fold it in when the row lands.
		if g.pendingRetractions == nil {
			g.pendingRetractions = make(map[string]map[string]ontology.Tombstone)
		}
		pending := g.pendingRetractions[relUID]
		if pending == nil {
			pending = make(map[string]ontology.Tombstone)
			g.pendingRetractions[relUID] = pending
		}
		pending[ev.UID] = tombstone
		g.Cursor = ev.UID
		return false, nil
	}

	if row.Retractions == nil {
		row.Retractions = make(map[string]ontology.Tombstone)
	}
	row.Retractions[ev.UID] = tombstone
	foldRetractions(row)
	g.Cursor = ev.UID
	return true, nil
}

// foldRetractions re-derives a relation row's SupersededByUID and ValidUntil
// from its full tombstone set via the shared ontology.ReduceRetractions fold,
// the same deterministic reduction the kernel dispatcher applies to the
// relations table. Row.ValidUntil is passed as the extractor-set bound floor so
// a legitimate bi-temporal bound survives; the fold is commutative and
// idempotent, so the rendered close converges regardless of replay order.
func foldRetractions(row *RelationRow) {
	if len(row.Retractions) == 0 {
		return
	}
	tombstones := make([]ontology.Tombstone, 0, len(row.Retractions))
	for _, t := range row.Retractions {
		tombstones = append(tombstones, t)
	}
	superseder, validUntil := ontology.ReduceRetractions(row.ValidUntil, tombstones)
	if superseder != nil {
		row.SupersededByUID = *superseder
	}
	row.ValidUntil = validUntil
}

func (g *GraphState) applyRelationForgotten(ev kernel.Event) (bool, error) {
	var payload ontology.RelationForgottenEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal relationForgotten: %w", err)
	}

	row, exists := g.Relations[payload.RelationUID]
	if !exists {
		g.Cursor = ev.UID
		return false, nil
	}

	row.Forgotten = true
	t := payload.ForgottenAt.UTC()
	row.ForgottenAt = &t
	row.ForgottenReason = payload.Reason
	g.Cursor = ev.UID
	return true, nil
}

func (g *GraphState) applyPredicateRegistered(ev kernel.Event) (bool, error) {
	var payload ontology.PredicateRegisteredEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal predicateRegistered: %w", err)
	}

	row := &PredicateRow{
		Predicate:      payload.Predicate,
		Kind:           payload.Kind,
		Description:    payload.Description,
		SubjectKinds:   payload.SubjectKinds,
		ObjectKinds:    payload.ObjectKinds,
		SameAs:         payload.SameAs,
		PropertySchema: compactRawJSON(payload.PropertySchema),
		AddedBy:        payload.AddedBy,
		AddedAt:        payload.AddedAt.UTC(),
	}

	// Mirror the dispatcher's UPSERT: a repeat registration overwrites the
	// stored row so the rendered predicates.yaml matches the replayed
	// projection table. Report no change when the row is byte-identical to
	// avoid spurious re-renders.
	if existing, exists := g.Predicates[payload.Predicate]; exists && existing.equal(row) {
		g.Cursor = ev.UID
		return false, nil
	}

	g.Predicates[payload.Predicate] = row
	g.Cursor = ev.UID
	return true, nil
}

// compactRawJSON returns raw compacted to its minimal byte form so stored
// rows and rendered output are byte-stable regardless of source whitespace.
// Nil/empty input stays nil; input that fails to compact (malformed JSON that
// slipped past upstream validation) is passed through unchanged rather than
// dropped.
func compactRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return raw
	}
	return json.RawMessage(compact.String())
}

func (g *GraphState) applyRelationSupersedeConflict(ev kernel.Event) (bool, error) {
	var payload ontology.RelationSupersedeConflictEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal relationSupersedeConflict: %w", err)
	}

	changed := false
	if row, ok := g.Relations[string(payload.PrimaryRelationUID)]; ok {
		if row.ConflictWith == "" {
			row.ConflictWith = string(payload.ConflictingRelationUID)
			changed = true
		}
	}
	if row, ok := g.Relations[string(payload.ConflictingRelationUID)]; ok {
		if row.ConflictWith == "" {
			row.ConflictWith = string(payload.PrimaryRelationUID)
			changed = true
		}
	}
	g.Cursor = ev.UID
	return changed, nil
}

func (g *GraphState) applyFact(ev kernel.Event) (bool, error) {
	var payload ontology.Fact
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal fact: %w", err)
	}

	g.Facts = append(g.Facts, &FactRow{
		UID:            ev.UID,
		SubjectUID:     payload.SubjectUID,
		StateKey:       payload.StateKey,
		Value:          payload.Value,
		CreateTime:     payload.CreateTime.UTC(),
		SourceEventUID: payload.SourceEventUID,
	})
	g.Cursor = ev.UID
	return true, nil
}

func (g *GraphState) applyEntityKindRegistered(ev kernel.Event) (bool, error) {
	var payload ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal entityKindRegistered: %w", err)
	}

	// Only carry PropertySchema into rendered state when it passes the same
	// JSON Schema compile check that the dispatcher runs before materializing
	// attr_schema. An invalid schema (e.g. {"type":12}) would otherwise reach
	// the git render while the runtime materialization dropped it, causing
	// synced ontology files to disagree with the live ontology.
	var compiledPropertySchema json.RawMessage
	if len(payload.PropertySchema) > 0 && isValidPropertySchema(payload.PropertySchema) {
		compiledPropertySchema = payload.PropertySchema
	}

	// Mirror the dispatcher's extends cycle/depth guard: if the proposed
	// parent would form a cycle or exceed the depth limit in the current
	// in-memory state, clear Extends so the rendered YAML matches what
	// SQLite materialization stored (dispatcher stores NULL in that case).
	extendsToRender := payload.Extends
	if extendsToRender != "" && !isSafeExtends(payload.Name, extendsToRender, g.EntityKinds) {
		extendsToRender = ""
	}

	status := payload.Status
	if status == "" {
		status = ontology.EntityKindStatusActive
	}

	row := &EntityKindRow{
		Name:           payload.Name,
		DisplayName:    payload.DisplayName,
		Description:    payload.Description,
		Extends:        extendsToRender,
		SameAs:         payload.SameAs,
		PropertySchema: compiledPropertySchema,
		Provenance:     payload.Provenance,
		AddedBy:        string(payload.AddedBy),
		AddedAt:        payload.AddedAt.UTC(),
		Status:         status,
	}

	// Guard: a non-core event must not overwrite an existing core row.
	if existing, ok := g.EntityKinds[payload.Name]; ok {
		if existing.Provenance == provenanceCore && payload.Provenance != provenanceCore {
			g.Cursor = ev.UID
			return false, nil
		}
	}

	g.EntityKinds[payload.Name] = row
	g.Cursor = ev.UID
	return true, nil
}

// applyEntityKindReviewed updates the Status of an existing EntityKindRow in
// the render state to reflect a review decision. Mirrors the dispatcher's
// applyEntityKindReviewed contract guards so the rendered ontology/kinds/<name>.yaml
// never disagrees with the replayed SQLite projection: core rows are
// write-protected, unknown names are silent no-ops, and a review decision only
// transitions a row that is still proposed (a replayed or synced review must not
// re-flip an already-active or already-rejected kind).
func (g *GraphState) applyEntityKindReviewed(ev kernel.Event) (bool, error) {
	var payload ontology.EntityKindReviewedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal entityKindReviewed: %w", err)
	}
	if payload.Name == "" {
		g.Cursor = ev.UID
		return false, nil
	}

	existing, ok := g.EntityKinds[payload.Name]
	if !ok {
		g.Cursor = ev.UID
		return false, nil // unknown kind: silent no-op
	}
	if existing.Provenance == provenanceCore {
		g.Cursor = ev.UID
		return false, nil // core rows are immutable
	}
	// Lifecycle invariant (mirrors the dispatcher): a review decision only
	// transitions a still-proposed row. Empty Status is treated as active
	// (pre-P7 back-compat), so it correctly fails this guard. Without it, a
	// replayed or out-of-order synced review could render a kind as rejected
	// while SQLite keeps it active.
	if existing.Status != ontology.EntityKindStatusProposed {
		g.Cursor = ev.UID
		return false, nil // only proposed rows transition
	}

	switch payload.Decision {
	case ontology.EntityKindReviewApproved:
		existing.Status = ontology.EntityKindStatusActive
	case ontology.EntityKindReviewRejected:
		existing.Status = ontology.EntityKindStatusRejected
	default:
		g.Cursor = ev.UID
		return false, nil // unknown decision: silent no-op
	}
	g.Cursor = ev.UID
	return true, nil
}

// applySchemaRegistered materializes a schemaRegistered event into the render
// state so dynamic schemas travel through the git transport at
// ontology/schemas/<schemaKind>.json. It mirrors the dispatcher's
// applySchemaRegistered contract guards so the rendered tree matches the
// replayed ontology_schemas projection: a non-core event cannot overwrite a
// core row, and a row is only replaced by a strictly newer semver version
// (same/lower versions are replay no-ops).
func (g *GraphState) applySchemaRegistered(ev kernel.Event) (bool, error) {
	var payload ontology.SchemaRegisteredEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return false, fmt.Errorf("render: unmarshal schemaRegistered: %w", err)
	}

	if existing, ok := g.Schemas[payload.SchemaKind]; ok {
		if existing.Provenance == provenanceCore && payload.Provenance != provenanceCore {
			g.Cursor = ev.UID
			return false, nil
		}
		if semver.Compare(payload.Version, existing.Version) <= 0 {
			g.Cursor = ev.UID
			return false, nil
		}
	}

	g.Schemas[payload.SchemaKind] = &SchemaRow{
		SchemaKind: payload.SchemaKind,
		Version:    payload.Version,
		Definition: append(json.RawMessage(nil), payload.Definition...),
		Provenance: payload.Provenance,
		AddedBy:    string(payload.AddedBy),
		AddedAt:    payload.AddedAt.UTC(),
	}
	g.Cursor = ev.UID
	return true, nil
}

// sortedKeys returns the keys of any map[string]V sorted lexicographically.
// All map iteration in the renderer MUST go through this helper.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// appendUnique appends s to slice only when not already present.
func appendUnique(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}

// appendUniqueChanged appends s to *slice only when not already present.
// Returns true when s was added.
func appendUniqueChanged(slice *[]string, s string) bool {
	for _, existing := range *slice {
		if existing == s {
			return false
		}
	}
	*slice = append(*slice, s)
	return true
}

// removeString returns a new slice with all occurrences of s removed.
func removeString(slice []string, s string) []string {
	out := slice[:0:len(slice)]
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// encodeJSONL encodes v as a JSON line into enc. Encoding a plain Go struct
// into a bytes.Buffer cannot fail; if it does, the data directory is
// corrupted beyond repair and a panic is appropriate.
func encodeJSONL(enc interface{ Encode(any) error }, v any) {
	if err := enc.Encode(v); err != nil {
		panic(fmt.Sprintf("render: encodeJSONL: %v", err))
	}
}

// renderMaxExtendsDepth is the maximum parent-chain depth the render
// cycle guard will walk. Kept in sync with kernel.maxExtendsDepth (16)
// and dispatcher.dispatcherMaxExtendsDepth (16).
const renderMaxExtendsDepth = 16

// isSafeExtends mirrors the dispatcher's checkExtendsCycle logic over the
// in-memory EntityKinds map. Returns true when proposedParent would NOT
// create a cycle or exceed renderMaxExtendsDepth hops. A missing ancestor
// (not yet registered in the current render state) is treated as a safe
// chain end, matching the dispatcher's sql.ErrNoRows path.
func isSafeExtends(kindName, proposedParent string, kinds map[string]*EntityKindRow) bool {
	visited := make(map[string]struct{}, renderMaxExtendsDepth+1)
	visited[kindName] = struct{}{} // self-reference sentinel
	current := proposedParent
	for range renderMaxExtendsDepth + 1 {
		if current == "" {
			return true // reached a root
		}
		if _, seen := visited[current]; seen {
			return false // cycle detected
		}
		visited[current] = struct{}{}
		row, ok := kinds[current]
		if !ok {
			return true // ancestor not yet in render state; chain ends here
		}
		current = row.Extends
	}
	return false // exceeded depth
}

// isValidPropertySchema reports whether raw is a syntactically and
// semantically valid JSON Schema document using the same santhosh-tekuri
// compiler settings the dispatcher's compile-before-materialize guard uses.
// Only PropertySchema values that pass this check are carried into rendered
// state so the git-synced ontology/kinds/*.yaml files never export a schema
// that the dispatcher's attr_schema materialization would have dropped.
func isValidPropertySchema(raw json.RawMessage) bool {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	const resource = "mem://render/property-schema.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(resource, doc); err != nil {
		return false
	}
	_, err := c.Compile(resource)
	return err == nil
}
