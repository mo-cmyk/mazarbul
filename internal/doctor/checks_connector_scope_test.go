package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// seedConnectorWatermark opens (and migrates) the index.db under dir and
// inserts one connector_watermarks row so the config-load check has the
// durable "ingestion has run in scope X" signal to read.
func seedConnectorWatermark(t *testing.T, dir string, connector, stream string, scope kernel.Scope) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqliteIndexPath(dir))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO connector_watermarks (connector, stream, cursor, sequence, advance_time, scope)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		connector, stream, `{"lastSHA":"abc"}`, 1, "2026-06-17T00:00:00Z", string(scope),
	); err != nil {
		t.Fatalf("insert watermark: %v", err)
	}
}

func newSharedFor(dir string) *sharedSQLite {
	return &sharedSQLite{path: sqliteIndexPath(dir)}
}

// TestConnectorScopeEncryption_PlaintextScopeWithIngest_Fails is the AC-3
// config-load guard: connector content ingested into a scope that is
// configured PLAINTEXT FAILs the check — the case the runtime scan cannot see
// (the seal gate refuses the chunk, so it never lands in plaintext to be found).
func TestConnectorScopeEncryption_PlaintextScopeWithIngest_Fails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"personal": {URL: "git@github.com:u/p.git"}, // Encrypted nil → plaintext
	})
	seedConnectorWatermark(t, dir, "local-git", "commits", kernel.ScopePersonal)

	status, detail, err := newConnectorScopeEncryptionCheck(dir, newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusFail {
		t.Errorf("status = %v, want FAIL", status)
	}
	if !strings.Contains(detail, "PLAINTEXT") || !strings.Contains(detail, "personal") {
		t.Errorf("detail = %q, want a PLAINTEXT explanation naming the scope", detail)
	}
	if code := ExitCode([]Result{{Status: status}}); code != 1 {
		t.Errorf("ExitCode = %d, want 1", code)
	}
}

// TestConnectorScopeEncryption_EncryptedScope_OK confirms an encrypted scope
// holding connector content is safe.
func TestConnectorScopeEncryption_EncryptedScope_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	enc := true
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"personal": {URL: "git@github.com:u/p.git", Encrypted: &enc},
	})
	seedConnectorWatermark(t, dir, "local-git", "commits", kernel.ScopePersonal)

	status, _, err := newConnectorScopeEncryptionCheck(dir, newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (encrypted scope)", status)
	}
}

// TestConnectorScopeEncryption_NoIngest_OK confirms a plaintext remote with no
// connector ingestion is fine — the check is scoped to connector content.
func TestConnectorScopeEncryption_NoIngest_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"personal": {URL: "git@github.com:u/p.git"},
	})
	// Open+migrate the store but seed NO watermark.
	seedConnectorWatermark(t, dir, "local-git", "commits", kernel.ScopeTeam) // team, not personal
	// Reconfigure: only a personal plaintext remote; the team watermark has no
	// configured remote, so it cannot leak.
	status, detail, err := newConnectorScopeEncryptionCheck(dir, newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v (%s), want OK (ingested scope has no configured remote)", status, detail)
	}
}

// TestConnectorScopeEncryption_NoRemotes_OK confirms the single-device case is
// a no-op even with connector content present.
func TestConnectorScopeEncryption_NoRemotes_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedConnectorWatermark(t, dir, "local-git", "commits", kernel.ScopePersonal)

	status, detail, err := newConnectorScopeEncryptionCheck(dir, newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (no remotes)", status)
	}
	if !strings.Contains(detail, "no remotes") {
		t.Errorf("detail = %q, want a 'no remotes' hint", detail)
	}
}
