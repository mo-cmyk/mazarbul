package ingest

import (
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// BuildConnectorEpisodeEvents builds the atomic (episode, sourceRef) event pair
// for one untrusted external item. It is the single point that stamps the trust
// posture, scrubs secret-shaped strings, and prompt-fences the body, so the pull
// orchestrator and the agent-mediated captureExternal tool never diverge on how
// untrusted external content is recorded.
//
// EpUID is the caller-minted episode ULID the SourceRef's SubjectUID references
// (the caller mints it so it can also assert a has_primary_source edge from the
// same episode in the same Append batch). The body is scrubbed, capped, and
// fenced; the episode is tagged connector:<connector> at full confidence (trust
// rides the source prefix and the resolver's proposed-status gate, not a
// depressed score); the SourceRef is keyed by item.DedupKey and carries
// item.FreshnessToken. The two events are returned (not appended): the caller
// composes them with any sibling events (external_ref create, has_primary_source
// relate) into one atomic Append.
func BuildConnectorEpisodeEvents(scrubber *Scrubber, connector, stream string, scope kernel.Scope, epUID string, item *Item) ([]kernel.Event, error) {
	body := capBody(scrubber.Scrub(item.Body))
	connectorSource := ontology.ConnectorSourceColonPrefix + connector

	epPayload, err := json.Marshal(ontology.Episode{
		Text:   WrapUntrusted(body),
		Source: connectorSource,
		Actor:  ontology.ActorSystem,
		Class:  ontology.ClassEpisodic,
		// Trust is carried by the connector:<name> Source prefix, which drives the
		// leak gate and the resolver's untrusted gate (status=proposed), not by a
		// depressed confidence. A verbatim capture is recorded at full confidence so
		// it ranks on relevance; its provenance, not its score, marks it untrusted.
		Confidence: 1.0,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal episode: %w", err)
	}

	refPayload, err := json.Marshal(ontology.SourceRefRecordedEvent{
		UID:            ontology.SourceRefUID(item.DedupKey),
		SubjectUID:     ontology.ULID(epUID),
		SourceType:     item.SourceType,
		Connector:      connector,
		Stream:         stream,
		ExternalID:     item.ExternalID,
		ExternalURL:    item.ExternalURL,
		ContentHash:    item.ContentHash,
		FetchTime:      item.FetchTime,
		DedupKey:       item.DedupKey,
		FreshnessToken: item.FreshnessToken,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal source ref: %w", err)
	}

	return []kernel.Event{
		{
			UID:        epUID,
			Kind:       kernel.EventKind(ontology.KindEpisode),
			Source:     connectorSource,
			Confidence: 1.0,
			Scope:      scope,
			Payload:    epPayload,
		},
		{
			Kind:       kernel.EventKind(ontology.KindSourceRefRecorded),
			Source:     connectorSource,
			Confidence: 1.0,
			Scope:      scope,
			Payload:    refPayload,
		},
	}, nil
}
