// Package capacity walks Mazarbul's data directory and totals the
// on-disk bytes per storage layer (event log, sqlite + fts, vector
// index, graph, embed cache). The Usage hub panel renders the result
// as stacked bars, and the getCapacity MCP tool returns the same shape.
package capacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Report is the rendered shape consumed by the Usage panel.
type Report struct {
	OnDisk   OnDisk
	Tokens   Tokens
	Computed time.Time
}

// OnDisk is the per-storage-layer byte totals.
type OnDisk struct {
	EventLog   int64
	SQLiteFTS  int64
	Graph      int64
	Vector     int64
	EmbedCache int64
	Total      int64
	FreeDisk   int64
}

// Tokens is the token-charge totals over Last30Days, as displayed in M5.
type Tokens struct {
	EmbedIn        uint64
	EmbedOut       uint64
	ChatIn         uint64
	ChatOut        uint64
	EstimatedUSD   float64
	PriceTableHash string
}

// Reporter is the contract the Usage panel + MCP tool talk to.
type Reporter interface {
	Report(ctx context.Context) (Report, error)
}

// NewReporter returns a Reporter scoped to dataDir. Capacity walks are
// cached for 60 s — the panel refresh ticks at 2 s, so re-walking on
// every tick is wasteful for what amounts to a directory enumeration.
func NewReporter(dataDir string) Reporter {
	return &dirReporter{dataDir: dataDir, ttl: 60 * time.Second}
}

type dirReporter struct {
	dataDir string
	ttl     time.Duration

	mu       sync.Mutex
	cachedAt time.Time
	cache    Report
}

func (r *dirReporter) Report(ctx context.Context) (Report, error) {
	r.mu.Lock()
	if !r.cachedAt.IsZero() && time.Since(r.cachedAt) < r.ttl {
		out := r.cache
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	out := Report{Computed: time.Now()}
	if r.dataDir == "" {
		return out, errors.New("capacity: no data dir configured")
	}

	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("capacity: %w", err)
	}

	// The daemon's real on-disk layout (confirmed by lsof against
	// a running daemon): event log is a subdir, but SQLite + the
	// vector index share a single index.db + WAL + SHM at the
	// top level, and Ladybug writes graph.ladybug + .wal at the
	// top level. The original "sqlite/" / "graph/" / "vector/"
	// subdir paths matched no real files and reported 0 B for
	// every layer except the event log — visible as the Usage
	// panel storage block showing ladybug=0 / sqlite=0 / vec=0
	// while the files were several MB on disk.
	out.OnDisk.EventLog = dirBytes(filepath.Join(r.dataDir, "events"))
	out.OnDisk.SQLiteFTS = sumFilesByPrefix(r.dataDir, "index.db")
	out.OnDisk.Graph = sumFilesByPrefix(r.dataDir, "graph.ladybug")
	// Vector lives in the same index.db file as the rest of
	// SQLite (sqlite-vec is a SQLite virtual table). Reporting
	// it separately would double-count; keep the field for
	// schema stability but leave it at zero until the daemon
	// splits storage layers.
	out.OnDisk.Vector = 0
	out.OnDisk.EmbedCache = dirBytes(filepath.Join(r.dataDir, "cache"))
	out.OnDisk.Total = out.OnDisk.EventLog + out.OnDisk.SQLiteFTS +
		out.OnDisk.Graph + out.OnDisk.Vector + out.OnDisk.EmbedCache
	out.OnDisk.FreeDisk = freeDisk(r.dataDir)

	r.mu.Lock()
	r.cache = out
	r.cachedAt = time.Now()
	r.mu.Unlock()
	return out, nil
}

// sumFilesByPrefix sums the byte counts of every top-level regular
// file under dir whose name starts with prefix. Used for stores
// like SQLite (index.db + index.db-wal + index.db-shm) and Ladybug
// (graph.ladybug + graph.ladybug.wal) that scatter related files
// at the top of the data dir rather than in a subdirectory.
func sumFilesByPrefix(dir, prefix string) int64 {
	if dir == "" || prefix == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < len(prefix) || name[:len(prefix)] != prefix {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// dirBytes returns the total byte count of every regular file under
// path. Unreadable directories contribute 0; the panel surfaces
// per-layer totals so a single unreadable subtree degrades gracefully.
func dirBytes(path string) int64 {
	if path == "" {
		return 0
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		full := filepath.Join(path, e.Name())
		if e.IsDir() {
			total += dirBytes(full)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}
