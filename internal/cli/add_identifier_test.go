package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// runEntityAddIdentifierCmd runs `maz entity add-identifier <uid> --identifier X`
// with --output text and returns the printed entities/{uid} name (or fatals).
func runEntityAddIdentifierCmd(t *testing.T, dataDir, uid, identifier string, extra ...string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	args := append(
		[]string{"--data-dir", dataDir, "--output", "text", "entity", "add-identifier", uid, "--identifier", identifier},
		extra...,
	)
	root.SetArgs(args)
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity add-identifier: %v (stderr=%q)", err, errBuf.String())
	}
	return strings.TrimSpace(outBuf.String())
}

// TestEntityAddIdentifierResolvesByAlias is the #1 end-to-end: after attaching a
// prior surface form as an alias to an existing entity, the resolver matches the
// entity by that alias — the rename-orphan case the feedback reported.
func TestEntityAddIdentifierResolvesByAlias(t *testing.T) {
	dir := t.TempDir()
	seedEntityConfig(t, dir) // embed=none; skips under -short

	name := runEntityCreateCmd(t, dir, "organization", "Erebor Trading Company")
	uid := strings.TrimPrefix(name, "entities/")
	if uid == "" || uid == name {
		t.Fatalf("unexpected create output %q", name)
	}

	// Attach the prior surface form as an alias to the existing entity.
	if got := runEntityAddIdentifierCmd(t, dir, uid, "Erebor Global"); got != name {
		t.Fatalf("add-identifier printed %q, want %q", got, name)
	}

	// The alias now resolves to the same entity through the resolver.
	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "get", "Erebor Global"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("get by alias: %v (stderr=%q)", err, errBuf.String())
	}
	if got := outBuf.String(); !strings.Contains(got, uid) {
		t.Fatalf("get by alias did not resolve to %s; output=%q", uid, got)
	}
}

// TestEntityAddIdentifierRequiresIdentifier verifies the flag is required: a
// missing --identifier is a clean bad-input exit, not a tool round-trip.
func TestEntityAddIdentifierRequiresIdentifier(t *testing.T) {
	t.Parallel()
	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", t.TempDir(), "entity", "add-identifier", "01HZZZZZZZZZZZZZZZZZZZEXMP"})
	root.SetContext(context.Background())
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when --identifier is omitted")
	}
}
