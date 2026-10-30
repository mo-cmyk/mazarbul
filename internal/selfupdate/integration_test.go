//go:build integration

package selfupdate

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEndToEndFileMirror exercises the full pipeline (resolve -> download ->
// verify -> extract -> smoke -> swap) against a file:// mirror and a temp
// install prefix, with no network and no real GitHub release. The real
// GitHub end-to-end is deferred until #243 publishes releases; this proves the
// mechanism offline. Run with: go test -tags='sqlite_fts5 integration' ./...
func TestEndToEndFileMirror(t *testing.T) {
	mirror := t.TempDir()
	writeFixtureArchive(t, mirror, "v9.9.9", runtime.GOOS, runtime.GOARCH)

	opts := &Options{BaseURL: "file://" + mirror, Version: "v9.9.9"}
	rel, err := ResolveRelease(context.Background(), opts)
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	dl := t.TempDir()
	archive, sums, err := Download(context.Background(), opts, rel, dl)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if err := VerifyChecksum(archive, sums, rel.ArchiveName); err != nil {
		t.Fatalf("VerifyChecksum: %v", err)
	}
	stage := t.TempDir()
	if err := Extract(archive, stage); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := Smoke(context.Background(), stage); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	loc := newLiveFixture(t)
	if err := Swap(stage, loc); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := readFile(t, filepath.Join(loc.BinDir, "maz")); got != fixtureMazScript {
		t.Errorf("installed binary content = %q, want the fixture script", got)
	}
}
