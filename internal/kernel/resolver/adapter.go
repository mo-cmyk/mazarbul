package resolver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Ensure Resolver satisfies kernel.ResolverIface at compile time.
var _ kernel.ResolverIface = (*Resolver)(nil)

// ResolveFrom implements kernel.ResolverIface. It scans the prepared batch for
// a RelationCandidatesProposed event, decodes the candidates and the
// accompanying Mention events from the same batch, and dispatches them
// through resolveInScope. Because resolveInScope appends entity / relation
// events through deps.EventLog directly, ResolveFrom returns nil events; the
// file_eventlog hook treats a nil return as a no-op.
//
// The scope of the RelationCandidatesProposed event is forwarded to
// resolveInScope so that entities and relations minted from a draft episode
// remain draft-scoped rather than defaulting to personal.
//
// Batches that do not contain a relationCandidatesProposed event are
// ignored. A batch with multiple such events is rare (the extractor emits at
// most one per Episode) but each is processed in order.
func (r *Resolver) ResolveFrom(ctx context.Context, batch []kernel.Event) ([]kernel.Event, error) {
	for _, ev := range batch {
		if ev.Kind != kernel.EventKind(ontology.KindRelationCandidatesProposed) {
			continue
		}
		var payload ontology.RelationCandidatesProposedEvent
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return nil, fmt.Errorf("resolver: decode candidates payload: %w", err)
		}

		// Propagate the scope from the candidate event (set by the extractor
		// from the originating episode's scope). An empty scope defaults to
		// personal via kernel.DefaultScope, preserving pre-fix behaviour.
		scope := ev.Scope
		if scope == "" {
			scope = kernel.DefaultScope
		}

		mentions := collectMentionsForEpisode(batch, payload.EpisodeUID)
		if err := r.resolveInScope(ctx, payload.EpisodeUID, scope, mentions, payload.Candidates); err != nil {
			return nil, fmt.Errorf("resolver: resolve episode %s: %w", payload.EpisodeUID, err)
		}
	}
	return nil, nil
}

// collectMentionsForEpisode walks batch in order and decodes every Mention
// event whose SourceEventUID matches episodeUID. The returned slice preserves
// batch order, which the extractor guarantees mirrors RelationCandidate's
// subject / object mention indices: the candidates reference positions in the
// mentions slice the extractor emitted, so any reordering during collection
// would silently scramble subject and object on every relation.
//
// The Mention's UID is carried over from the event UID so downstream entity
// updates can record the mention reference.
func collectMentionsForEpisode(batch []kernel.Event, episodeUID string) []ontology.Mention {
	mentions := make([]ontology.Mention, 0, len(batch))
	for _, ev := range batch {
		if ev.Kind != kernel.EventKind(ontology.KindMention) {
			continue
		}
		var m ontology.Mention
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			continue
		}
		if string(m.SourceEventUID) != episodeUID {
			continue
		}
		m.UID = ev.UID
		mentions = append(mentions, m)
	}
	return mentions
}
