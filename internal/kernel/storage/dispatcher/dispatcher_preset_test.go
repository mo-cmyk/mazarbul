package dispatcher_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// applyPresetInstall applies a presetInstalled event for the given name/version.
func applyPresetInstall(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, name, version, instructions string) {
	t.Helper()
	payload, err := json.Marshal(ontology.PresetInstalledEvent{
		Name:         name,
		Version:      version,
		Description:  "Test preset " + name,
		Instructions: instructions,
		AddedAt:      time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal presetInstalled: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindPresetInstalled),
		Source:     "preset:" + name,
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply presetInstalled %q: %v", name, err)
	}
}

// applyPresetDeactivate applies a presetDeactivated event for the given name.
func applyPresetDeactivate(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, name string) {
	t.Helper()
	payload, err := json.Marshal(ontology.PresetDeactivatedEvent{
		Name:      name,
		RemovedAt: time.Date(2026, 6, 16, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal presetDeactivated: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindPresetDeactivated),
		Source:     "preset:" + name,
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 16, 13, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply presetDeactivated %q: %v", name, err)
	}
}

// queryPreset returns version + instructions for name, or sql.ErrNoRows.
func queryPreset(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, name string) (version, instructions string, err error) {
	t.Helper()
	db, derr := d.OpenSQLite(ctx)
	if derr != nil {
		t.Fatalf("OpenSQLite: %v", derr)
	}
	err = db.QueryRowContext(ctx, `SELECT version, instructions FROM presets WHERE name = ?`, name).
		Scan(&version, &instructions)
	return version, instructions, err
}

func presetCount(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher) int {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM presets`).Scan(&n); err != nil {
		t.Fatalf("count presets: %v", err)
	}
	return n
}

func TestApplyPresetInstalled_UpsertsRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	applyPresetInstall(t, ctx, d, "crm", "0.1.0", "# CRM\nFile deals under crm:Account.\n")
	ver, instr, err := queryPreset(t, ctx, d, "crm")
	if err != nil {
		t.Fatalf("query crm: %v", err)
	}
	if ver != "0.1.0" {
		t.Errorf("version = %q, want 0.1.0", ver)
	}
	if instr != "# CRM\nFile deals under crm:Account.\n" {
		t.Errorf("instructions not stored verbatim: %q", instr)
	}
}

// TestApplyPresetInstalled_Idempotent re-applies install (same name, new
// version) and asserts a single updated row — the replay-safety that makes
// rebuild deterministic.
func TestApplyPresetInstalled_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	applyPresetInstall(t, ctx, d, "crm", "0.1.0", "v1 body")
	applyPresetInstall(t, ctx, d, "crm", "0.2.0", "v2 body")

	if got := presetCount(t, ctx, d); got != 1 {
		t.Fatalf("preset count = %d, want 1 (upsert, not duplicate)", got)
	}
	ver, instr, err := queryPreset(t, ctx, d, "crm")
	if err != nil {
		t.Fatalf("query crm: %v", err)
	}
	if ver != "0.2.0" || instr != "v2 body" {
		t.Errorf("after re-install: version=%q instructions=%q, want 0.2.0 / v2 body", ver, instr)
	}
}

func TestApplyPresetDeactivated_DeletesRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	applyPresetInstall(t, ctx, d, "crm", "0.1.0", "body")
	applyPresetDeactivate(t, ctx, d, "crm")

	if _, _, err := queryPreset(t, ctx, d, "crm"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("after deactivate: err = %v, want sql.ErrNoRows (row gone)", err)
	}
}

// applyPresetInstallProv applies a presetInstalled event carrying #224
// provenance fields (source, pinned commit, checksum).
func applyPresetInstallProv(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, name, sourceURL, pinnedRef, checksum string) {
	t.Helper()
	payload, err := json.Marshal(ontology.PresetInstalledEvent{
		Name:         name,
		Version:      "0.1.0",
		Description:  "Remote preset " + name,
		Instructions: "# body\n",
		AddedAt:      time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC),
		SourceURL:    sourceURL,
		PinnedRef:    pinnedRef,
		Checksum:     checksum,
	})
	if err != nil {
		t.Fatalf("marshal presetInstalled: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindPresetInstalled),
		Source:     "preset:" + name,
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply presetInstalled %q: %v", name, err)
	}
}

// queryPresetProvenance returns the source_url, pinned_ref, and checksum columns.
func queryPresetProvenance(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, name string) (sourceURL, pinnedRef, checksum string) {
	t.Helper()
	db, derr := d.OpenSQLite(ctx)
	if derr != nil {
		t.Fatalf("OpenSQLite: %v", derr)
	}
	if err := db.QueryRowContext(ctx, `SELECT source_url, pinned_ref, checksum FROM presets WHERE name = ?`, name).
		Scan(&sourceURL, &pinnedRef, &checksum); err != nil {
		t.Fatalf("query provenance %q: %v", name, err)
	}
	return sourceURL, pinnedRef, checksum
}

// TestApplyPresetInstalled_RecordsProvenance projects the #224 provenance fields
// for a remote install and leaves them empty for a local/builtin install.
func TestApplyPresetInstalled_RecordsProvenance(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const wantRef = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	applyPresetInstallProv(t, ctx, d, "remote-crm", "github.com/acme/crm@v1", wantRef, "sha256:abc123")
	src, ref, sum := queryPresetProvenance(t, ctx, d, "remote-crm")
	if src != "github.com/acme/crm@v1" || ref != wantRef || sum != "sha256:abc123" {
		t.Errorf("remote provenance = (%q,%q,%q), want the recorded values", src, ref, sum)
	}

	// A local/builtin install carries no provenance: the columns stay empty.
	applyPresetInstall(t, ctx, d, "local-crm", "0.1.0", "body")
	src, ref, sum = queryPresetProvenance(t, ctx, d, "local-crm")
	if src != "" || ref != "" || sum != "" {
		t.Errorf("local install provenance = (%q,%q,%q), want all empty", src, ref, sum)
	}
}

// TestApplyPresetDeactivated_UnknownIsNoop asserts deactivating an absent preset
// is a no-op (idempotent), so a replayed or duplicate deactivate converges.
func TestApplyPresetDeactivated_UnknownIsNoop(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	applyPresetDeactivate(t, ctx, d, "never-installed")
	if got := presetCount(t, ctx, d); got != 0 {
		t.Errorf("preset count = %d, want 0", got)
	}
}

// TestApplyPresetDeactivated_CleanCut confirms uninstalling a preset does NOT
// retract the entity kinds it seeded: an entity kind registered alongside the
// preset survives deactivation.
func TestApplyPresetDeactivated_CleanCut(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// Seed an extension kind the way `maz preset install` would (provenance =
	// extension:crm), then mark the preset active.
	kindPayload, err := json.Marshal(ontology.EntityKindRegisteredEvent{
		Name:        "extension:crm:account",
		DisplayName: "Account",
		Description: "A CRM account.",
		Provenance:  "extension:crm",
		AddedBy:     ontology.EntityKindAddedByExtension,
		AddedAt:     time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal kind: %v", err)
	}
	kindEv := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
		Source:     "schemapack:crm",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
		Payload:    kindPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{kindEv}); err != nil {
		t.Fatalf("apply entityKindRegistered: %v", err)
	}
	applyPresetInstall(t, ctx, d, "crm", "0.1.0", "body")

	applyPresetDeactivate(t, ctx, d, "crm")

	// The preset row is gone...
	if _, _, qerr := queryPreset(t, ctx, d, "crm"); !errors.Is(qerr, sql.ErrNoRows) {
		t.Fatalf("preset row: err = %v, want sql.ErrNoRows", qerr)
	}
	// ...but the seeded kind remains resolvable.
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var kindCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entity_kinds WHERE name = ?`, "extension:crm:account").Scan(&kindCount); err != nil {
		t.Fatalf("count entity_kinds: %v", err)
	}
	if kindCount != 1 {
		t.Errorf("seeded kind count after uninstall = %d, want 1 (clean cut, not retracted)", kindCount)
	}
}
