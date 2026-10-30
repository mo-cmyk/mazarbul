// Package foaf provides the embedded FOAF vocabulary snapshot and
// an [Importer] seeded from it.
//
// The snapshot is a curated subset of 10 FOAF classes stored in
// types.json alongside a manifest.json that records the snapshot version and
// date for provenance.  Canonical namespace is http://xmlns.com/foaf/0.1/.
package foaf

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
	// Vocabulary is the vocabulary name, e.g. "foaf".
	Vocabulary string `json:"vocabulary"`

	// SnapshotVersion is the FOAF release version, e.g. "0.1".
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
		return nil, fmt.Errorf("foaf: parse manifest: %w", err)
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
		return nil, fmt.Errorf("foaf: load entries: %w", err)
	}
	return vocab.NewFoafImporter(entries), nil
}
