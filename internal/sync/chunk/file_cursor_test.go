package chunk_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

func TestFileCursor_PersistsAcrossInstances(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := chunk.NewFileCursor(dir)
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	want := []string{
		"01J0000000000000000000AAAA",
		"01J0000000000000000000AAAB",
		"01J0000000000000000000AAAC",
	}
	if err := first.MarkSealed(context.Background(), kernel.ScopePersonal, want); err != nil {
		t.Fatalf("first MarkSealed: %v", err)
	}

	// Fresh instance reads the file from scratch — proves persistence.
	second, err := chunk.NewFileCursor(dir)
	if err != nil {
		t.Fatalf("NewFileCursor (second): %v", err)
	}
	got, err := second.Sealed(context.Background(), kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("Sealed: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("len(Sealed)=%d, want %d (got=%v)", len(got), len(want), got)
	}
	for _, uid := range want {
		if _, ok := got[uid]; !ok {
			t.Errorf("Sealed missing uid %q", uid)
		}
	}
}

func TestFileCursor_AppendsAcrossCalls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	c, err := chunk.NewFileCursor(dir)
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	if err := c.MarkSealed(context.Background(), kernel.ScopePersonal,
		[]string{"01J0000000000000000000AAAA"}); err != nil {
		t.Fatalf("first MarkSealed: %v", err)
	}
	if err := c.MarkSealed(context.Background(), kernel.ScopePersonal,
		[]string{"01J0000000000000000000AAAB"}); err != nil {
		t.Fatalf("second MarkSealed: %v", err)
	}
	got, err := c.Sealed(context.Background(), kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("Sealed: %v", err)
	}
	if _, ok := got["01J0000000000000000000AAAA"]; !ok {
		t.Errorf("first uid missing after append")
	}
	if _, ok := got["01J0000000000000000000AAAB"]; !ok {
		t.Errorf("second uid missing after append")
	}
}

func TestFileCursor_TolatesTornLastLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a corrupt sealed.jsonl by hand: one good UID then a torn tail.
	syncDir := filepath.Join(dir, "sync", "personal")
	if err := os.MkdirAll(syncDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(syncDir, "sealed.jsonl")
	if err := os.WriteFile(path,
		[]byte("01J0000000000000000000AAAA\nNOTAULID\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, err := chunk.NewFileCursor(dir)
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	got, err := c.Sealed(context.Background(), kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("Sealed: %v", err)
	}
	if _, ok := got["01J0000000000000000000AAAA"]; !ok {
		t.Errorf("valid uid lost after torn-line tolerance")
	}
	if _, ok := got["NOTAULID"]; ok {
		t.Errorf("non-ULID line should be ignored, was %v", got)
	}
}

func TestFileCursor_MissingFileIsEmptySet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := chunk.NewFileCursor(dir)
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	got, err := c.Sealed(context.Background(), kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("Sealed missing-file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file should yield empty set, got %v", got)
	}
}

func TestFileCursor_RejectsInvalidScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := chunk.NewFileCursor(dir)
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	if err := c.MarkSealed(context.Background(), kernel.Scope("bogus"),
		[]string{"01J0000000000000000000AAAA"}); err == nil {
		t.Errorf("MarkSealed accepted invalid scope")
	}
}
