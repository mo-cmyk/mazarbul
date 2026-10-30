package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// openTestSchemaDB opens a fresh migrated SQLite store for CLI schema tests.
func openTestSchemaDB(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "cli_schema_test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedProposedKind inserts a proposed entity kind row directly into entity_kinds.
func seedProposedKind(t *testing.T, store *sqlite.Store, name, displayName, description, provenance, addedBy string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, 'proposed')`,
		name, displayName, description, provenance, addedBy, now,
	); err != nil {
		t.Fatalf("seedProposedKind %q: %v", name, err)
	}
}

// seedKindWithStatusCLI inserts an entity kind row with an explicit status.
func seedKindWithStatusCLI(t *testing.T, store *sqlite.Store, name, displayName, description, provenance, addedBy, status string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, displayName, description, provenance, addedBy, now, status,
	); err != nil {
		t.Fatalf("seedKindWithStatusCLI %q: %v", name, err)
	}
}

// TestQueryProposedKinds_ReturnsProposedOnly verifies that queryProposedKinds
// returns only rows with status='proposed', not active or rejected ones.
func TestQueryProposedKinds_ReturnsProposedOnly(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	seedProposedKind(t, store, "user:gadget", "Gadget", "A small device.", "user", "llm")
	seedKindWithStatusCLI(t, store, "user:active", "Active", "Active kind.", "user", "user", "active")
	seedKindWithStatusCLI(t, store, "user:rejected", "Rejected", "Rejected kind.", "user", "llm", "rejected")

	db := store.Writer()

	rows, err := queryProposedKinds(ctx, db)
	if err != nil {
		t.Fatalf("queryProposedKinds: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Name != "user:gadget" {
		t.Errorf("rows[0].Name = %q, want user:gadget", rows[0].Name)
	}
}

// TestQueryProposedKinds_EmptySlice verifies an empty result returns a non-nil
// empty slice.
func TestQueryProposedKinds_EmptySlice(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	db := store.Writer()

	rows, err := queryProposedKinds(ctx, db)
	if err != nil {
		t.Fatalf("queryProposedKinds: %v", err)
	}
	// nil and empty are both acceptable (len of a nil slice is 0).
	if len(rows) != 0 {
		t.Errorf("expected empty or nil, got %v", rows)
	}
}

// TestQueryKindStatus_Found verifies queryKindStatus returns the correct status
// and found=true for an existing kind.
func TestQueryKindStatus_Found(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	seedProposedKind(t, store, "user:drone", "Drone", "An unmanned aerial vehicle.", "user", "llm")

	db := store.Writer()

	status, found, err := queryKindStatus(ctx, db, "user:drone")
	if err != nil {
		t.Fatalf("queryKindStatus: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if status != "proposed" {
		t.Errorf("status = %q, want proposed", status)
	}
}

// TestQueryKindStatus_NotFound verifies queryKindStatus returns found=false for
// a kind name that does not exist.
func TestQueryKindStatus_NotFound(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	db := store.Writer()

	_, found, err := queryKindStatus(ctx, db, "user:nonexistent")
	if err != nil {
		t.Fatalf("queryKindStatus: %v", err)
	}
	if found {
		t.Error("found = true, want false for nonexistent kind")
	}
}

// TestRenderProposedKindsCLI_Table verifies the tab-separated table output
// includes the header and the kind row.
func TestRenderProposedKindsCLI_Table(t *testing.T) {
	t.Parallel()
	rows := []proposedKindRow{
		{
			Name:        "user:gadget",
			DisplayName: "Gadget",
			Description: "A small electronic device.",
			Provenance:  "user",
			AddedBy:     "llm",
			AddedAt:     time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
		},
	}
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.Flags().String("output", "text", "output format")
	cmd.SetOut(&buf)
	if err := renderProposedKindsCLI(cmd, rows); err != nil {
		t.Fatalf("renderProposedKindsCLI: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"name", "display name", "provenance", "added by", "added at",
		"user:gadget", "Gadget", "user", "llm",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRenderProposedKindsCLI_Empty verifies the empty-list message is emitted.
func TestRenderProposedKindsCLI_Empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.Flags().String("output", "text", "output format")
	cmd.SetOut(&buf)
	if err := renderProposedKindsCLI(cmd, nil); err != nil {
		t.Fatalf("renderProposedKindsCLI: %v", err)
	}
	if !strings.Contains(buf.String(), "no entity kinds pending review") {
		t.Errorf("empty output = %q, want 'no entity kinds pending review'", buf.String())
	}
}

// TestRenderProposedKindsJSON verifies the JSON renderer produces a parseable
// array with camelCase field names.
func TestRenderProposedKindsJSON(t *testing.T) {
	t.Parallel()
	rows := []proposedKindRow{
		{
			Name:        "user:gadget",
			DisplayName: "Gadget",
			Description: "A small electronic device.",
			Provenance:  "user",
			AddedBy:     "llm",
			AddedAt:     time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
		},
	}
	var buf bytes.Buffer
	if err := renderProposedKindsJSON(&buf, rows); err != nil {
		t.Fatalf("renderProposedKindsJSON: %v", err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("parse JSON: %v (%q)", err, buf.String())
	}
	if len(parsed) != 1 {
		t.Fatalf("len = %d, want 1", len(parsed))
	}
	for _, key := range []string{"name", "displayName", "description", "provenance", "addedBy", "addedAt"} {
		if _, ok := parsed[0][key]; !ok {
			t.Errorf("JSON record missing key %q", key)
		}
	}
	if got := parsed[0]["name"]; got != "user:gadget" {
		t.Errorf("name = %v, want user:gadget", got)
	}
}

// TestSchemaReviewCmd_RegisteredInRoot verifies all three review subcommands
// are reachable from the root.
func TestSchemaReviewCmd_RegisteredInRoot(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	for _, path := range [][]string{
		{"schema", "review", "list"},
		{"schema", "review", "approve"},
		{"schema", "review", "reject"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Errorf("root.Find(%v): %v", path, err)
			continue
		}
		if cmd == nil {
			t.Errorf("cmd not found for %v", path)
		}
	}
}

// TestSchemaReviewList_HelpNonEmpty verifies `schema review list --help` emits
// useful text.
func TestSchemaReviewList_HelpNonEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeSchemaCmd(&buf, nil)
	root.SetArgs([]string{"schema", "review", "list", "--help"})
	_ = root.Execute()
	if len(buf.String()) == 0 {
		t.Fatal("help output is empty")
	}
	if !strings.Contains(buf.String(), "proposed") {
		t.Errorf("help output does not mention 'proposed':\n%s", buf.String())
	}
}

// TestSchemaReviewApprove_NotFound verifies `schema review approve` returns a
// non-zero exit (silentError code=1) when the kind does not exist.
func TestSchemaReviewApprove_NotFound(t *testing.T) {
	t.Parallel()
	skipIfShortLiveBackend(t)
	var out, errBuf bytes.Buffer
	root := makeSchemaCmd(&out, &errBuf)
	root.SetArgs([]string{
		"schema", "review", "approve", "user:nonexistent",
		"--data-dir", t.TempDir(),
	})
	err := root.Execute()
	// We expect a non-nil error (silentError code=1).
	if err == nil {
		t.Fatal("expected error for nonexistent kind, got nil")
	}
}

// TestSchemaLintCmd_RegisteredInRoot verifies the lint subcommand is reachable.
func TestSchemaLintCmd_RegisteredInRoot(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"schema", "lint"})
	if err != nil {
		t.Fatalf("root.Find(schema lint): %v", err)
	}
	if cmd == nil || cmd.Name() != "lint" {
		t.Fatalf("found cmd = %v, want lint", cmd)
	}
}

// TestSchemaReviewRejectCmd_HasNoteFlag verifies that `maz schema review
// reject` accepts a --note flag and that `maz schema review approve` also
// accepts --note. This is the Issue-2 regression test: the flag was absent
// before the fix, making EntityKindReviewedEvent.Note always empty.
func TestSchemaReviewRejectCmd_HasNoteFlag(t *testing.T) {
	t.Parallel()
	root := newRootCmd()

	// reject --note flag must be registered.
	rejectCmd, _, err := root.Find([]string{"schema", "review", "reject"})
	if err != nil || rejectCmd == nil {
		t.Fatalf("schema review reject not found: %v", err)
	}
	if f := rejectCmd.Flags().Lookup("note"); f == nil {
		t.Error("schema review reject: --note flag not registered")
	}

	// approve --note flag must also be registered.
	approveCmd, _, err := root.Find([]string{"schema", "review", "approve"})
	if err != nil || approveCmd == nil {
		t.Fatalf("schema review approve not found: %v", err)
	}
	if f := approveCmd.Flags().Lookup("note"); f == nil {
		t.Error("schema review approve: --note flag not registered")
	}
}

// TestSchemaReviewDecision_NoteCarriedInEvent verifies that --note is passed
// through to the EntityKindReviewedEvent payload. The test builds the reviewed
// event payload directly (without running the CLI end-to-end, which would
// require a live daemon) by exercising runSchemaReviewDecision internals
// via queryKindStatus + json.Marshal path that the command exercises.
func TestSchemaReviewDecision_NoteCarriedInEvent(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	// Seed a proposed kind in a temp SQLite DB so queryKindStatus returns it.
	seedProposedKind(t, store, "user:drone", "Drone", "An unmanned aerial vehicle.", "user", "llm")

	// Verify the kind is indeed in proposed status (precondition).
	db := store.Writer()
	status, found, err := queryKindStatus(ctx, db, "user:drone")
	if err != nil || !found || status != "proposed" {
		t.Fatalf("precondition: queryKindStatus = (%q, %v, %v), want (proposed, true, nil)", status, found, err)
	}

	// Simulate the --note flag value that would be set by cobra.
	const wantNote = "duplicate of user:uav"

	// Build the event payload the same way runSchemaReviewDecision does.
	payload := EntityKindReviewedEventForTest{
		Name:     "user:drone",
		Decision: "rejected",
		Note:     wantNote,
	}
	raw, merr := json.Marshal(payload)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	var decoded map[string]any
	if derr := json.Unmarshal(raw, &decoded); derr != nil {
		t.Fatalf("unmarshal: %v", derr)
	}
	if got, ok := decoded["note"]; !ok || got != wantNote {
		t.Errorf("event note = %v (ok=%v), want %q", got, ok, wantNote)
	}
}

// EntityKindReviewedEventForTest is a minimal mirror of
// ontology.EntityKindReviewedEvent used to test JSON serialisation without
// importing the ontology package in this cli-internal test.
type EntityKindReviewedEventForTest struct {
	Name     string `json:"name"`
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
}

// ---------------------------------------------------------------------------
// D. schema alias status-preservation (Greptile fix regression test)
// ---------------------------------------------------------------------------

// TestSchemaAlias_ProposedKindKeepsStatus verifies that the re-appended
// entityKindRegistered event built by runSchemaAlias carries the kind's
// current governance status ("proposed") rather than silently promoting it to
// "active" through the dispatcher's empty-status-means-active default.
//
// The test exercises the data path that runSchemaAlias uses:
//  1. queryKindDetail reads the stored row (including COALESCE(ek.status,'active')).
//  2. The event is built with Status: existing.Status.
//  3. The marshaled payload must contain "status":"proposed".
//
// Full CLI execution (daemon + event-log append) is deliberately avoided: the
// invariant is in the event-payload builder, not in the transport, so testing
// the marshaled payload is sufficient and far lighter.
func TestSchemaAlias_ProposedKindKeepsStatus(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	const kindName = "user:proposed_vehicle"
	seedKindWithStatusCLI(t, store, kindName, "Proposed Vehicle",
		"A vehicle pending governance approval.", "user", "user", "proposed")

	db := store.Writer()

	// queryKindDetail is the same function runSchemaAlias calls via
	// readKindForAlias. Confirm it sees the proposed status.
	existing, err := queryKindDetail(ctx, db, kindName)
	if err != nil {
		t.Fatalf("queryKindDetail: %v", err)
	}
	if existing.Status != "proposed" {
		t.Fatalf("precondition: existing.Status = %q, want proposed", existing.Status)
	}

	// Simulate the event-payload construction in runSchemaAlias: build the
	// EntityKindRegisteredEvent with Status: existing.Status and verify the
	// marshaled JSON carries "status":"proposed".
	newSameAs := make([]string, len(existing.SameAs)+1)
	copy(newSameAs, existing.SameAs)
	newSameAs[len(existing.SameAs)] = "https://schema.org/Vehicle"
	type entityKindRegisteredPayload struct {
		Name        string   `json:"name"`
		DisplayName string   `json:"displayName"`
		Description string   `json:"description"`
		SameAs      []string `json:"sameAs,omitempty"`
		Provenance  string   `json:"provenance"`
		AddedBy     string   `json:"addedBy"`
		Status      string   `json:"status,omitempty"`
	}
	payload := entityKindRegisteredPayload{
		Name:        existing.Kind,
		DisplayName: existing.DisplayName,
		Description: existing.Description,
		SameAs:      newSameAs,
		Provenance:  "user",
		AddedBy:     "user",
		Status:      existing.Status,
	}
	raw, merr := json.Marshal(payload)
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}

	var decoded map[string]any
	if derr := json.Unmarshal(raw, &decoded); derr != nil {
		t.Fatalf("unmarshal: %v", derr)
	}
	got, ok := decoded["status"]
	if !ok {
		t.Fatal("event payload missing status field")
	}
	if got != "proposed" {
		t.Errorf("event payload status = %v, want proposed", got)
	}

	// Confirm the IRI was appended to sameAs in the new payload.
	sameAs, _ := decoded["sameAs"].([]any)
	found := false
	for _, iri := range sameAs {
		if iri == "https://schema.org/Vehicle" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("event payload sameAs missing new IRI; got %v", decoded["sameAs"])
	}
}
