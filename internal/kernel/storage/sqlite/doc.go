// Package sqlite implements the relational layer of Mazarbul's four-layer
// storage. It owns the on-disk SQLite database, the production pragma set
// (WAL, foreign_keys=ON, busy_timeout=5000, synchronous=NORMAL), the
// embedded forward-only migration runner, and two pooled *sql.DB handles
// (a single-writer pool and a read-only multi-reader pool).
//
// The eventual storage dispatcher wraps a *Store and exposes
// kernel.Storage.OpenSQLite by returning Store.Writer; this package itself
// does not implement the kernel.Storage interface. See
// agent-docs/designs/storage.md for the four-layer rationale.
package sqlite
