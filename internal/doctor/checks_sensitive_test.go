package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

func writeRemoteConfig(t *testing.T, dir string, remotes map[string]config.ScopeRemoteTable) {
	t.Helper()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Sync:          config.SyncTable{Remotes: remotes},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("WriteUserConfig: %v", err)
	}
}

// writeScopeChunk hand-builds a plaintext chunk on disk so the doctor
// scan has a fixture to find. It bypasses chunk.EncodePlaintext (which
// now refuses sensitive payloads at encode time) — the doctor check
// guards against exactly such a chunk arriving by a non-sealer path
// (pulled from a remote, or sealed before a marker existed).
func writeScopeChunk(t *testing.T, dir string, scope kernel.Scope, source string) {
	t.Helper()
	const chunkID = "01HQ8ZAAAAAAAAAAAAAAAAAAAA"
	uid := "01HQ8Z0000000000000000000F"
	e := kernel.Event{
		Name:       "events/" + uid,
		UID:        uid,
		Kind:       "episode",
		Source:     source,
		Confidence: 1,
		Scope:      scope,
		Payload:    []byte(`{"text":"body"}`),
	}
	meta := map[string]any{
		"_meta":      true,
		"chunkId":    chunkID,
		"scope":      string(scope),
		"count":      1,
		"firstUlid":  uid,
		"lastUlid":   uid,
		"createTime": "2026-06-15T00:00:00Z",
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(meta); err != nil {
		t.Fatalf("encode meta: %v", err)
	}
	if err := enc.Encode(e); err != nil {
		t.Fatalf("encode event: %v", err)
	}
	chunksDir := chunk.DirFor(dir, scope)
	if mkErr := os.MkdirAll(chunksDir, 0o750); mkErr != nil {
		t.Fatalf("MkdirAll: %v", mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(chunksDir, chunkID+chunk.ExtensionJSONL), buf.Bytes(), 0o600); wErr != nil {
		t.Fatalf("WriteFile: %v", wErr)
	}
}

// TestSensitivePayloadPosture_PlaintextSensitiveChunk_Fails pins
// acceptance #2: a plaintext scope holding a sensitive chunk FAILs the
// check, and ExitCode promotes that to a non-zero exit.
func TestSensitivePayloadPosture_PlaintextSensitiveChunk_Fails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"personal": {URL: "git@github.com:u/p.git"}, // Encrypted nil → plaintext
	})
	writeScopeChunk(t, dir, kernel.ScopePersonal, "connector:granola")

	status, detail, err := newSensitivePayloadPostureCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusFail {
		t.Errorf("status = %v, want FAIL", status)
	}
	if !strings.Contains(detail, "PLAINTEXT") {
		t.Errorf("detail = %q, want a PLAINTEXT explanation", detail)
	}
	if code := ExitCode([]Result{{Status: status}}); code != 1 {
		t.Errorf("ExitCode = %d, want 1", code)
	}
}

// TestSensitivePayloadPosture_EncryptedScope_OK confirms encrypted scopes
// are skipped: a sensitive chunk under an encrypted scope is fine.
func TestSensitivePayloadPosture_EncryptedScope_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	enc := true
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"personal": {URL: "git@github.com:u/p.git", Encrypted: &enc},
	})
	writeScopeChunk(t, dir, kernel.ScopePersonal, "connector:granola")

	status, _, err := newSensitivePayloadPostureCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (encrypted scope skipped)", status)
	}
}

// TestSensitivePayloadPosture_ProjectScope_OK confirms the project
// exemption at the doctor boundary.
func TestSensitivePayloadPosture_ProjectScope_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"project": {URL: "git@github.com:u/p.git"},
	})
	writeScopeChunk(t, dir, kernel.ScopeProject, "connector:granola")

	status, _, err := newSensitivePayloadPostureCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (project exempt)", status)
	}
}

// TestSensitivePayloadPosture_PlaintextNonSensitive_OK confirms a normal
// plaintext scope passes.
func TestSensitivePayloadPosture_PlaintextNonSensitive_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"personal": {URL: "git@github.com:u/p.git"},
	})
	writeScopeChunk(t, dir, kernel.ScopePersonal, "cli:remember")

	status, _, err := newSensitivePayloadPostureCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (no sensitive chunk)", status)
	}
}

// TestSensitivePayloadPosture_NoRemotes_OK confirms the single-device
// (no remote) case is a no-op.
func TestSensitivePayloadPosture_NoRemotes_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, detail, err := newSensitivePayloadPostureCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (no remotes)", status)
	}
	if !strings.Contains(detail, "no remotes") {
		t.Errorf("detail = %q, want 'no remotes' hint", detail)
	}
}

// TestSensitivePayloadPosture_InvalidScopeName_Skipped pins the scope
// validation guard: a remote whose name is not a recognized kernel scope
// is skipped before the name is used as a filesystem path, so a typo'd or
// hand-edited key cannot steer the scan outside the known
// sync/{project,personal,team}/chunks trees. (sync.remotes_configured is
// the check that reports the bad name.) A sensitive chunk planted under
// the bogus scope's derived dir would FAIL the pre-fix code; here it must
// be ignored.
func TestSensitivePayloadPosture_InvalidScopeName_Skipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeRemoteConfig(t, dir, map[string]config.ScopeRemoteTable{
		"bogus": {URL: "git@github.com:u/p.git"},
	})
	writeScopeChunk(t, dir, kernel.Scope("bogus"), "connector:granola")

	status, _, err := newSensitivePayloadPostureCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (invalid scope name skipped)", status)
	}
}
