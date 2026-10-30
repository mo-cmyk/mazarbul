// Package skos provides the embedded SKOS (Simple Knowledge Organization
// System) vocabulary snapshot and an [Importer] seeded from it.
//
// The snapshot covers 4 SKOS core classes stored in types.json alongside a
// manifest.json that records the snapshot version and date for provenance.
// Canonical namespace is http://www.w3.org/2004/02/skos/core#.
package skos

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
	// Vocabulary is the vocabulary name, e.g. "skos".
	Vocabulary string `json:"vocabulary"`

	// SnapshotVersion is the SKOS release date used as version, e.g. "2009-08-18".
	SnapshotVersion string `json:"snapshotVersion"`

	// SnapshotDate is the date the snapshot was taken (YYYY-MM-DD).
	SnapshotDate string `json:"snapshotDate"`

	// SourceURL is the canonical URL of the full vocabulary specification.
	SourceURL string `json:"sourceURL"`

	// Notes is a free-text description of the snapshot scope.
	Notes string `json:"notes"`
}

// Manifest returns the parsed snapshot manifest for provenance reporting.
func Manifest() (*SnapshotManifest, error) {
	var m SnapshotManifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("skos: parse manifest: %w", err)
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
		return nil, fmt.Errorf("skos: load entries: %w", err)
	}
	return vocab.NewSkosImporter(entries), nil
}
