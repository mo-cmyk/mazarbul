// Package wikidata provides the embedded Wikidata vocabulary snapshot and
// an [Importer] seeded from it.
//
// The snapshot is a curated subset of 25 commonly-referenced Wikidata entity
// classes stored in types.json alongside a manifest.json that records the
// snapshot version and date for provenance.  Canonical IRI pattern is
// http://www.wikidata.org/entity/Q<id>.  The full Wikidata corpus can be
// imported via --from-file with a SPARQL CONSTRUCT export.
package wikidata

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
)

//go:embed types.json
var typesJSON []byte

//go:embed manifest.json
var manifestJSON []byte

// SnapshotManifest records provenance metadata for the embedded snapshot.
type SnapshotManifest struct {
	// Vocabulary is the vocabulary name, e.g. "wikidata".
	Vocabulary string `json:"vocabulary"`

	// SnapshotVersion is the snapshot creation date used as version.
	SnapshotVersion string `json:"snapshotVersion"`

	// SnapshotDate is the date the snapshot was taken (YYYY-MM-DD).
	SnapshotDate string `json:"snapshotDate"`

	// SourceURL is the canonical URL of the Wikidata project.
	SourceURL string `json:"sourceURL"`

	// Notes is a free-text description of the snapshot scope.
	Notes string `json:"notes"`
}

// Manifest returns the parsed snapshot manifest for provenance reporting.
func Manifest() (*SnapshotManifest, error) {
	var m SnapshotManifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("wikidata: parse manifest: %w", err)
	}
	return &m, nil
}

// Entries parses and returns the type entries from the embedded snapshot.
func Entries() ([]vocab.TypeEntry, error) {
	return vocab.ParseTypeEntries(typesJSON)
}

// NewImporter returns a [vocab.Importer] loaded from the embedded snapshot.
func NewImporter() (*vocab.Importer, error) {
	entries, err := Entries()
	if err != nil {
		return nil, fmt.Errorf("wikidata: load entries: %w", err)
	}
	return vocab.NewWikidataImporter(entries), nil
}
