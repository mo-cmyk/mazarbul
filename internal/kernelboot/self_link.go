package kernelboot

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// seededSelfUID is the local-only symbolic self anchor seeded by migration
// 0012_self_entity_seed. It is never emitted as an event, so it never syncs and
// never collides across a team. Mirrors resolver.SelfEntityUID and the literal
// "self" sentinel the Relation schema accepts as subject/object (see
// ontology.Relation.SubjectUID pattern ^([0-9A-HJKMNP-TV-Z]{26}|self)$). Kept as
// a local constant rather than importing internal/kernel/resolver, matching the
// established pattern in internal/mcp/tools/relate.go (selfEntityUID).
const seededSelfUID = "self"

// selfLinkPredicate is the predicate that links the local self anchor to the
// bound principal identity. Owl:sameAs / schema.org/sameAs semantics: the two
// nodes denote the same individual.
const selfLinkPredicate = "same_as"

// genesisSelfLinkTime is the addedAt timestamp for the same_as identity-link
// predicate registration emitted by EnsureSelfPrincipalLink on existing
// installs. Dated after the #208 seed predicates (genesisTimeLate) to keep the
// genesis provenance timeline monotonic and replay-stable.
//
//nolint:gochecknoglobals // process-lifetime constant; time.Time cannot be declared const
var genesisSelfLinkTime = time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)

// EnsureSelfPrincipalLink links the local-only self anchor (uid="self", display
// name "Me", seeded by migration 0012) to the bound principal's person entity
// (uid=<principalULID>, the portable per-user identity materialized by
// applyPrincipalRegistered and synced at personal scope). Without this link the
// graph shows an isolated "Me" stub next to the real identity node.
//
// The link is a raw genesis-style `same_as` relation event written at the
// kernelboot layer. This deliberately bypasses the two-place self-rebind
// (resolver.resolveSelfSubject + mcp/tools.rebindSelf) that maps the "self"
// sentinel to the principal UID on every resolver/MCP write surface: a rebound
// relate(subject="self") collapses into a self-loop on the principal and cannot
// create the link. The dispatcher's applyRelation projects the literal
// subject/object UIDs as-is (no rebind), so a KindRelation event with
// subjectUid="self", objectUid=principalULID lands as a real self->principal row.
//
// It is idempotent and best-effort, meant to run on every daemon boot right
// after SeedIdentity + UpgradeGitHubAnchor: a no-op when there is no bound
// principal, the principal IS the seeded self, the principal has not been
// registered yet, or the link already exists. On a missing/failed step the
// reconciler returns an error the caller logs as non-fatal and retries next boot.
//
// The link is a structural identity invariant — like the principal's person
// entity and the seeded self anchor themselves — not user content. The
// reconciler re-establishes it on boot whenever it is absent, so retracting or
// `maz forget`-ing it only makes it reappear on the next boot (bounded: once the
// row is present again the check below short-circuits). That is by design: the
// self<->principal identity is maintained, not user-discardable. Collapsing the
// two nodes into one is a merge (entityMerged), not a forget.
//
// Db is the SQLite writer pool (same dataDir as log).
func EnsureSelfPrincipalLink(ctx context.Context, log kernel.EventLog, db *sql.DB, dataDir string) error {
	if db == nil {
		return nil
	}
	principalUID, err := config.SelfPrincipalUID(dataDir)
	if err != nil {
		return fmt.Errorf("kernelboot: read self binding: %w", err)
	}
	if principalUID == "" {
		return nil // genesis has not run yet; nothing to link
	}
	if principalUID == seededSelfUID {
		// The principal IS the seeded self anchor: there is no separate portable
		// node to link, and a self->self edge would be a degenerate self-loop.
		return nil
	}
	if !principalRegistered(ctx, db, principalUID) {
		// The principals row (and thus the principal's projected person entity)
		// is not present yet, so there is no real node to link to. Retry next boot.
		return nil
	}

	linked, err := selfPrincipalLinkExists(ctx, db, principalUID)
	if err != nil {
		return err
	}
	if linked {
		return nil // already linked; nothing to do
	}

	events, err := selfPrincipalLinkEvents(principalUID)
	if err != nil {
		return err
	}
	// makeGenesisEvent leaves Event.Scope empty, so kernel.DefaultScope (personal)
	// applies on Append. Personal scope is REQUIRED here: the link references the
	// local-only "self" sentinel and must never land in a team scope.
	if _, err := log.Append(ctx, events); err != nil {
		return fmt.Errorf("kernelboot: append self-principal link: %w", err)
	}
	return nil
}

// selfPrincipalLinkExists reports whether a live same_as edge already connects
// the seeded self anchor to principalUID. "Live" follows the canonical
// relations-table predicate used by the navigate layer (appendRelationFilter):
// not retracted/superseded (superseded_by_uid IS NULL OR empty). The link is
// never validity-bounded, so valid_from/valid_until are not consulted.
func selfPrincipalLinkExists(ctx context.Context, db *sql.DB, principalUID string) (bool, error) {
	const q = `
		SELECT COUNT(*) FROM relations
		WHERE subject_uid = ?
		  AND predicate = ?
		  AND object_uid = ?
		  AND (superseded_by_uid IS NULL OR superseded_by_uid = '')`
	var n int
	err := db.QueryRowContext(ctx, q, seededSelfUID, selfLinkPredicate, principalUID).Scan(&n)
	if err != nil {
		// A missing relations table (pre-migration) means there cannot be a link
		// yet; treat it as "not linked" so the reconciler retries once migrated
		// rather than bricking boot.
		if isNoSuchTableErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("kernelboot: query self-principal link: %w", err)
	}
	return n > 0, nil
}

// selfPrincipalLinkEvents builds the two genesis events the reconciler appends:
//
//  1. A predicateRegistered for same_as. SeedOntology short-circuits on an
//     already-seeded store, so existing installs would never get same_as from
//     the seed list; emitting it here guarantees registration on every install.
//     The dispatcher's ON CONFLICT(predicate) upsert makes re-registration
//     idempotent (fresh installs also seed it via genesis.go seedPredicates).
//  2. The same_as Relation event itself, subject=self, object=principalUID. The
//     UID and SourceEventUID are the SAME minted ULID, mirroring the direct
//     assertion path in internal/mcp/tools/relate.go buildRelation
//     (SourceEventUID: uid): a raw genesis assertion has no upstream episode, so
//     it is its own provenance anchor.
func selfPrincipalLinkEvents(principalUID string) ([]kernel.Event, error) {
	predicate := selfLinkPredicateEvent()
	predicateEv, err := makeGenesisEvent(ontology.KindPredicateRegistered, predicate)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: marshal same_as predicate: %w", err)
	}

	uid, err := newGenesisULID()
	if err != nil {
		return nil, err
	}
	rel := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     seededSelfUID,
		Predicate:      selfLinkPredicate,
		ObjectUID:      principalUID,
		Confidence:     1.0,
		SourceEventUID: uid,
		CreateTime:     time.Now().UTC(),
		Status:         ontology.RelationStatusAsserted,
		Provenance:     "genesis: links the local self anchor to the bound principal identity",
	}
	relEv, err := makeGenesisEvent(ontology.KindRelation, &rel)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: marshal self-principal relation: %w", err)
	}
	return []kernel.Event{predicateEv, relEv}, nil
}

// selfLinkPredicateEvent is the same_as PredicateRegisteredEvent payload, shared
// by EnsureSelfPrincipalLink (existing installs) and SeedOntology's seed list
// (fresh installs) so both paths register identical vocabulary. Mirrors the
// genesis.go seedPredicates entries exactly.
func selfLinkPredicateEvent() *ontology.PredicateRegisteredEvent {
	return &ontology.PredicateRegisteredEvent{
		Predicate:   selfLinkPredicate,
		Kind:        ontology.PredicateKindNonFunctional,
		Description: "Subject denotes the same individual as the object (identity link, e.g. the local self anchor to the bound principal).",
		SameAs:      []string{"http://www.w3.org/2002/07/owl#sameAs", "https://schema.org/sameAs"},
		AddedBy:     ontology.PredicateAddedBySeed,
		AddedAt:     genesisSelfLinkTime,
	}
}
