package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRenderTreeForScope materialises a tiny rendered/ tree under
// dataDir/sync/<scope>/repo/rendered/ with two files: entities.jsonl
// (entitiesBytes long) and other.md (padBytes long). The doctor walk
// hits filepath.Walk, so the directory shape mirrors what the
// transport produces on push.
func writeRenderTreeForScope(t *testing.T, dataDir, scope string, entitiesBytes, padBytes int) {
	t.Helper()
	root := filepath.Join(dataDir, "sync", scope, "repo", "rendered")
	if err := os.MkdirAll(filepath.Join(root, "by-uid", "01H", "AB"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "entities.jsonl"),
		bytesOfSize(entitiesBytes), 0o600); err != nil {
		t.Fatalf("write entities.jsonl: %v", err)
	}
	if padBytes > 0 {
		if err := os.WriteFile(filepath.Join(root, "by-uid", "01H", "AB", "stub.md"),
			bytesOfSize(padBytes), 0o600); err != nil {
			t.Fatalf("write stub: %v", err)
		}
	}
}

func bytesOfSize(n int) []byte {
	if n <= 0 {
		return nil
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}

// writePlaintextScopeConfig drops a minimal config.toml under dataDir
// declaring one plaintext scope so loadResolvedSync produces an
// entry the render-size check can iterate. Keeps the file shape in
// sync with config.UserConfig's TOML mapping; if a future refactor
// reshapes [sync.<scope>], update this helper.
func writePlaintextScopeConfig(t *testing.T, dataDir, scope, url string) {
	t.Helper()
	content := "[sync.remotes." + scope + "]\n" +
		"url = \"" + url + "\"\n" +
		"encrypted = false\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"),
		[]byte(content), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

func TestRenderSizeCheck_NoRemotesIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, detail, err := newRenderSizeCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK", status)
	}
	if !strings.Contains(detail, "n/a") {
		t.Errorf("detail = %q, want n/a hint", detail)
	}
}

func TestRenderSizeCheck_RemoteConfiguredNoTreeIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfig(t, dir, "personal", "git@example.com:o/personal.git")
	status, detail, err := newRenderSizeCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK on fresh install with no pushes yet", status)
	}
	if !strings.Contains(detail, "no rendered trees") {
		t.Errorf("detail = %q, want 'no rendered trees' hint", detail)
	}
}

func TestRenderSizeCheck_SmallTreeIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfig(t, dir, "personal", "git@example.com:o/personal.git")
	writeRenderTreeForScope(t, dir, "personal", 256, 512)

	status, detail, err := newRenderSizeCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK on small tree", status)
	}
	if !strings.Contains(detail, "within budget") {
		t.Errorf("detail = %q, want 'within budget' hint", detail)
	}
}

func TestRenderSizeCheck_EntitiesJSONLOverThresholdWarns(t *testing.T) {
	// Not t.Parallel(): mutates package-level renderEntitiesWarnBytes/
	// renderDirWarnBytes. A parallel run with TotalDirOverThreshold
	// races the cleanup and produces flaky "wrong message" failures.
	dir := t.TempDir()
	writePlaintextScopeConfig(t, dir, "personal", "git@example.com:o/personal.git")

	// Lower the entities threshold to a tiny value so we can trip the
	// warn path without writing 200 MiB on disk in a unit test.
	origEntities := renderEntitiesWarnBytes
	origDir := renderDirWarnBytes
	t.Cleanup(func() {
		renderEntitiesWarnBytes = origEntities
		renderDirWarnBytes = origDir
	})
	renderEntitiesWarnBytes = 1024
	renderDirWarnBytes = 1 << 30 // keep dir threshold high so we isolate the entities-jsonl signal

	writeRenderTreeForScope(t, dir, "personal", 4096, 0)

	status, detail, err := newRenderSizeCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want Warn when entities.jsonl exceeds threshold", status)
	}
	if !strings.Contains(detail, "entities.jsonl") {
		t.Errorf("detail = %q, want entities.jsonl mention", detail)
	}
}

func TestRenderSizeCheck_TotalDirOverThresholdWarns(t *testing.T) {
	// Not t.Parallel() — see EntitiesJSONLOverThreshold for rationale
	// (shared package-level thresholds, parallel mutation flakes).
	dir := t.TempDir()
	writePlaintextScopeConfig(t, dir, "personal", "git@example.com:o/personal.git")

	origEntities := renderEntitiesWarnBytes
	origDir := renderDirWarnBytes
	t.Cleanup(func() {
		renderEntitiesWarnBytes = origEntities
		renderDirWarnBytes = origDir
	})
	// Set dir threshold low enough to trigger; keep entities threshold
	// high so we isolate the dir-total signal.
	renderDirWarnBytes = 2048
	renderEntitiesWarnBytes = 1 << 30

	writeRenderTreeForScope(t, dir, "personal", 256, 4096)

	status, detail, err := newRenderSizeCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want Warn when total rendered tree exceeds threshold", status)
	}
	if !strings.Contains(detail, "rendered tree") || !strings.Contains(detail, "personal") {
		t.Errorf("detail = %q, want rendered-tree warn citing scope name", detail)
	}
}

func TestHumanBytesRender_FormatsExpectedUnits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{5 << 20, "5.0 MiB"},
		{int64(3) << 30, "3.0 GiB"},
	}
	for _, c := range cases {
		if got := humanBytesRender(c.in); got != c.want {
			t.Errorf("humanBytesRender(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
