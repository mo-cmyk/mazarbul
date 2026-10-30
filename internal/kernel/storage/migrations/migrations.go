// Package migrations holds the embedded SQL and Cypher migration files for
// the four-layer storage. Each layer (sqlite, ladybug, etc.) lives in its
// own subdirectory; this package exposes one embed.FS per layer so the
// runners in internal/kernel/storage/<layer>/ can consume the files
// without nesting them inside the package directory.
package migrations

import "embed"

// SQLite is the embedded migration set for the relational layer. Files
// live under sqlite/NNNN_<name>.up.sql and are applied in lexicographic
// order by the runner at internal/kernel/storage/sqlite/migrate.go.
//
//go:embed sqlite/*.up.sql
var SQLite embed.FS
