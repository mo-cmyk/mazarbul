package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// PredicateKind classifies how many current values a predicate may hold per
// subject. Functional predicates allow at most one live value; non-functional
// predicates allow many.
type PredicateKind string

const (
	// Functional predicates allow at most one live relation per (subject,
	// predicate) pair. A new relation auto-supersedes the previous one.
	Functional PredicateKind = "functional"

	// NonFunctional predicates allow multiple live relations per (subject,
	// predicate) pair. An LLM contradiction check gates supersede decisions.
	NonFunctional PredicateKind = "non-functional"
)

// AddedBy constants for the Predicate.AddedBy field. Defined here so the
// WritePredicate switch statement and callers in resolver.go satisfy the
// goconst linter rule (3+ occurrences must be a named constant).
const (
	addedBySeed = "seed"
	addedByUser = "user"
	addedByLLM  = "llm"
)

// Predicate describes a single entry in the predicates vocabulary table.
type Predicate struct {
	// Name is the predicate identifier, e.g. "works_at". Required.
	Name string

	// Kind classifies the predicate as functional or non-functional. Required.
	Kind PredicateKind

	// Description is a short human-readable explanation of the predicate.
	Description string

	// AddedAt is the time the predicate was added to the registry. Required.
	AddedAt time.Time

	// AddedBy identifies the agent that registered the predicate.
	// One of: "seed", "llm", "user".
	AddedBy string

	// SubjectKinds lists the entity-kind names valid as subjects for this
	// predicate (domain constraint). Nil/empty means any kind is accepted
	// (open-world default). Carried onto the predicateRegistered event;
	// persisted to the predicates table by the dispatcher (P5 typed relations).
	SubjectKinds []string

	// ObjectKinds lists the entity-kind names valid as objects for this
	// predicate (range constraint). Nil/empty means any kind is accepted.
	ObjectKinds []string

	// SameAs lists alias/sameAs IRIs to canonical or external vocabularies
	// (schema.org, Wikidata, FOAF). Nil/empty means no external alignment.
	SameAs []string

	// PropertySchema is the optional JSON Schema (Draft 2020-12) for the
	// relation's Attributes map. Nil means the attributes bag is untyped.
	PropertySchema json.RawMessage
}

// Registry caches the predicates table for the process lifetime. Updates are
// reflected via Register; concurrent reads are safe.
type Registry struct {
	mu    sync.RWMutex
	cache map[string]PredicateKind
	db    *sql.DB // read-only pool
}

// NewRegistry opens the cache and loads all predicates from db.
func NewRegistry(ctx context.Context, db *sql.DB) (*Registry, error) {
	r := &Registry{
		cache: make(map[string]PredicateKind),
		db:    db,
	}
	if err := r.load(ctx); err != nil {
		return nil, fmt.Errorf("predicates: load: %w", err)
	}
	return r, nil
}

// Kind returns the cached kind for predicate. Returns ("", false) if unknown.
func (r *Registry) Kind(predicate string) (PredicateKind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.cache[predicate]
	return k, ok
}

// IsFunctional returns true when the predicate is registered as functional.
// Unknown predicates default to non-functional (conservative; no auto-supersede).
func (r *Registry) IsFunctional(predicate string) bool {
	k, ok := r.Kind(predicate)
	if !ok {
		return false
	}
	return k == Functional
}

// Register stores a new predicate. Idempotent on (predicate, kind) tuple:
// if the exact (predicate, kind) pair already exists in the cache the call
// returns nil without touching the database.
//
// The caller must supply a tx from the writer pool so the resolver can append
// a PredicateRegistered event and the INSERT in the same transaction.
func (r *Registry) Register(ctx context.Context, tx *sql.Tx, p Predicate) error {
	// Fast path: if the predicate is already cached with the same kind, skip
	// the write. We must hold the write lock for the full operation to avoid a
	// race between the cache check and the cache update.
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.cache[p.Name]; ok && existing == p.Kind {
		return nil
	}

	addedAt := p.AddedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO predicates (predicate, kind, description, added_at, added_by)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(predicate) DO UPDATE SET
		     kind        = excluded.kind,
		     description = excluded.description,
		     added_at    = excluded.added_at,
		     added_by    = excluded.added_by`,
		p.Name, string(p.Kind), p.Description, addedAt, p.AddedBy,
	)
	if err != nil {
		return fmt.Errorf("predicates: register %q: %w", p.Name, err)
	}

	r.cache[p.Name] = p.Kind
	return nil
}

// lexiconEntry is one curated classification in predicateLexicon.
type lexiconEntry struct {
	kind PredicateKind
	desc string
}

// predicateLexicon maps predicates the extractor commonly emits — but the
// 0006/0013 migrations do not seed — to curated classifications.
// AutoRegisterPredicate consults it BEFORE the LLM, so these classify
// deterministically with zero LLM round-trips.
//
// Kind rationale: functional = at most one LIVE value per subject (a new
// relation auto-supersedes the old one), so past-tense predicates are
// always non-functional — a person accumulates many former employers and
// homes, and auto-superseding one past fact with another would erase
// history. Sourced from live extraction dumps over the gold set plus the
// obvious tense/synonym families of the seeded canon.
//
//nolint:gochecknoglobals // small static catalog, mirrors the 0006 seed list
var predicateLexicon = map[string]lexiconEntry{
	// Employment & education — present tense holds one live value.
	"works_for":      {Functional, "Subject is employed by the object organisation."},
	"employed_by":    {Functional, "Subject is employed by the object organisation."},
	"attends":        {Functional, "Subject is currently enrolled at the object institution."},
	"teaches_at":     {Functional, "Subject currently teaches at the object institution."},
	"worked_at":      {NonFunctional, "Subject was formerly employed at the object organisation."},
	"studied_at":     {NonFunctional, "Subject formerly studied at the object institution."},
	"attended":       {NonFunctional, "Subject attended the object institution or event in the past."},
	"graduated_from": {NonFunctional, "Subject graduated from the object institution."},

	// Residence & location.
	"lived_in":         {NonFunctional, "Subject formerly resided in the object location."},
	"based_in":         {Functional, "Subject is currently based in the object location."},
	"headquartered_in": {Functional, "Subject organisation has its headquarters in the object location."},
	"visited":          {NonFunctional, "Subject visited the object location."},
	"traveled_to":      {NonFunctional, "Subject traveled to the object location."},

	// Family & social — symmetric/many-valued, never auto-supersede.
	"sibling_of":   {NonFunctional, "Subject is a sibling of the object person."},
	"brother_of":   {NonFunctional, "Subject is a brother of the object person."},
	"sister_of":    {NonFunctional, "Subject is a sister of the object person."},
	"cousin_of":    {NonFunctional, "Subject is a cousin of the object person."},
	"father_of":    {NonFunctional, "Subject is the father of the object person."},
	"mother_of":    {NonFunctional, "Subject is the mother of the object person."},
	"friend_of":    {NonFunctional, "Subject is a friend of the object person."},
	"friends_with": {NonFunctional, "Subject is a friend of the object person."},
	"colleague_of": {NonFunctional, "Subject is a colleague of the object person."},
	"knows":        {NonFunctional, "Subject knows the object person."},

	// Romance — one current partner; new value supersedes (knowledge update).
	"dating":     {Functional, "Subject is in a romantic relationship with the object person."},
	"engaged_to": {Functional, "Subject is engaged to the object person."},
	"partner_of": {Functional, "Subject is the romantic partner of the object person."},

	// Work structure & creation.
	"reports_to":      {Functional, "Subject reports to the object person as direct manager."},
	"manages":         {NonFunctional, "Subject manages the object person or team."},
	"leads":           {NonFunctional, "Subject leads the object team, project, or group."},
	"lead_of":         {NonFunctional, "Subject is the lead of the object team or project."},
	"works_on":        {NonFunctional, "Subject works on the object project."},
	"works_with":      {NonFunctional, "Subject works with the object person."},
	"founded":         {NonFunctional, "Subject founded the object organisation."},
	"co_founded":      {NonFunctional, "Subject co-founded the object organisation."},
	"created":         {NonFunctional, "Subject created the object."},
	"acquired":        {NonFunctional, "Subject organisation acquired the object organisation."},
	"affiliated_with": {NonFunctional, "Subject is affiliated with the object organisation."},
	"participated_in": {NonFunctional, "Subject participated in the object event or activity."},

	// Mentorship — many mentees/mentors over time.
	"mentors":   {NonFunctional, "Subject mentors the object person."},
	"mentored":  {NonFunctional, "Subject formerly mentored the object person."},
	"mentor_of": {NonFunctional, "Subject is a mentor of the object person."},
	"mentee_of": {NonFunctional, "Subject is a mentee of the object person."},

	// Attributes, health & preferences.
	"speaks":         {NonFunctional, "Subject speaks the object language."},
	"plays":          {NonFunctional, "Subject plays the object instrument, sport, or game."},
	"enjoys":         {NonFunctional, "Subject enjoys the object activity."},
	"likes":          {NonFunctional, "Subject likes the object."},
	"dislikes":       {NonFunctional, "Subject dislikes the object."},
	"allergic_to":    {NonFunctional, "Subject is allergic to the object."},
	"diagnosed_with": {NonFunctional, "Subject has been diagnosed with the object condition."},
	"has_pet":        {NonFunctional, "Subject has the object animal as a pet."},
}

// lexiconLookup returns the curated classification for predicate, if any.
// Lookup is case-insensitive on the trimmed predicate name.
func lexiconLookup(predicate string) (PredicateKind, string, bool) {
	e, ok := predicateLexicon[strings.ToLower(strings.TrimSpace(predicate))]
	if !ok {
		return "", "", false
	}
	return e.kind, e.desc, true
}

// load queries all rows from the predicates table and populates the cache.
// Must be called with the lock not held; called only from NewRegistry.
func (r *Registry) load(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx,
		`SELECT predicate, kind FROM predicates`,
	)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var pred, kind string
		if err := rows.Scan(&pred, &kind); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		r.cache[pred] = PredicateKind(kind)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	return nil
}

// eventAppender is the minimum interface WritePredicate requires from an event
// log: the ability to append events. Both *kernel.FileEventLog and the MCP
// tools' Appender satisfy this interface, so WritePredicate need not import the
// full kernel.EventLog (which also carries Read, At, Rebuild, Provenance).
type eventAppender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// WritePredicate is the shared predicate-registration writer callable outside
// the resolver's own pipeline. It persists p in the predicates table via a
// SQLite transaction on writer, appends a PredicateRegistered event to
// eventLog, updates the in-memory registry cache, and logs. This is the same
// logic the resolver's internal registerPredicate method executes; it is
// extracted here so the definePredicate MCP tool can call it without importing
// the Resolver type. The resolver's method delegates here unchanged.
//
// Writer is the single-writer SQLite pool. EventLog is any append-capable event
// log (satisfying eventAppender: both *kernel.FileEventLog and mcp tools'
// Appender qualify). Reg is the in-memory predicate registry. Logger may be nil
// (nop substituted). Source is the event source string recorded on the
// PredicateRegistered event (e.g. "mcp/definePredicate").
func WritePredicate(
	ctx context.Context,
	writer *sql.DB,
	eventLog eventAppender,
	reg *Registry,
	logger *zap.Logger,
	p Predicate,
	source string,
) error {
	if logger == nil {
		logger = zap.NewNop()
	}

	// Append the predicateRegistered event FIRST. The event log is the source of
	// truth and the dispatcher materialises it into the predicates table; writing
	// the local SQLite row + in-memory cache only after the append succeeds means
	// a failed append never leaves a predicate in the derived store that a replay
	// (`maz rebuild`), which sees only the log, would silently drop.
	ontKind := ontology.PredicateKindFunctional
	if p.Kind == NonFunctional {
		ontKind = ontology.PredicateKindNonFunctional
	}
	var addedBy ontology.PredicateAddedBy
	switch p.AddedBy {
	case addedBySeed:
		addedBy = ontology.PredicateAddedBySeed
	case addedByUser:
		addedBy = ontology.PredicateAddedByUser
	default:
		addedBy = ontology.PredicateAddedByLLM
	}
	evPayload := ontology.PredicateRegisteredEvent{
		Predicate:      p.Name,
		Kind:           ontKind,
		Description:    p.Description,
		AddedBy:        addedBy,
		AddedAt:        p.AddedAt,
		SubjectKinds:   p.SubjectKinds,
		ObjectKinds:    p.ObjectKinds,
		SameAs:         p.SameAs,
		PropertySchema: p.PropertySchema,
	}
	raw, err := json.Marshal(evPayload)
	if err != nil {
		return fmt.Errorf("marshal predicate registered event: %w", err)
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindPredicateRegistered),
		Source:     source,
		Confidence: 1.0,
		Payload:    raw,
	}
	if _, err := eventLog.Append(ctx, []kernel.Event{ev}); err != nil {
		return err
	}

	// Refresh the in-memory registry cache (and idempotently upsert the row) so
	// the predicate is live for the resolver immediately; the dispatcher's
	// applyPredicateRegistered has already written the same row from the event.
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for predicate register: %w", err)
	}
	if err := reg.Register(ctx, tx, p); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("register predicate: %w (rollback: %w)", err, rbErr)
		}
		return fmt.Errorf("register predicate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit predicate register: %w", err)
	}

	logger.Info("predicate registered",
		zap.String("predicate", p.Name),
		zap.String("kind", string(p.Kind)),
		zap.String("addedBy", p.AddedBy),
		zap.String("description", p.Description),
		zap.String("source", source),
	)
	return nil
}
