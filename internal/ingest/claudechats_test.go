package ingest

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// sampleExport is a minimal but representative Claude account-data export: two
// conversations exercising BOTH message shapes — the typed content[] blocks (the
// current export) and the flat text field (older exports) — plus a non-text
// (image-only) turn that must be skipped.
const sampleExport = `[
  {
    "uuid": "conv-1",
    "name": "Planning chat",
    "chat_messages": [
      {"uuid": "m-1", "sender": "human", "content": [{"type": "text", "text": "What is the budget?"}]},
      {"uuid": "m-2", "sender": "assistant", "content": [{"type": "text", "text": "The budget is fixed at ten."}]}
    ]
  },
  {
    "uuid": "conv-2",
    "name": "Old format chat",
    "chat_messages": [
      {"uuid": "m-3", "sender": "human", "text": "Legacy flat-text message."},
      {"uuid": "m-4", "sender": "assistant", "content": [{"type": "image", "source": "x"}]}
    ]
  }
]`

func fixedNow() func() time.Time {
	t := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// writeExportZip writes an export bundle containing the given files and returns
// its path. Each entry is name->content.
func writeExportZip(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range files {
		w, werr := zw.Create(name)
		if werr != nil {
			t.Fatalf("zip create %q: %v", name, werr)
		}
		if _, werr := w.Write([]byte(content)); werr != nil {
			t.Fatalf("zip write %q: %v", name, werr)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return path
}

func TestClaudeChatsSource_ParsesBothMessageShapes(t *testing.T) {
	t.Parallel()
	path := writeExportZip(t, map[string]string{"conversations.json": sampleExport})
	src := NewClaudeChatsSource(path, fixedNow())
	if err := src.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	batch, err := src.Pull(context.Background(), streamConversations, Watermark{}, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	// 3 text turns (the image-only m-4 is skipped).
	if len(batch.Items) != 3 {
		t.Fatalf("items = %d, want 3 (image-only turn skipped)", len(batch.Items))
	}
	if !batch.Done {
		t.Error("single full Pull should report Done")
	}
	wantKeys := []string{"claude-chats:conv-1:m-1", "claude-chats:conv-1:m-2", "claude-chats:conv-2:m-3"}
	for i, want := range wantKeys {
		if batch.Items[i].DedupKey != want {
			t.Errorf("item %d DedupKey = %q, want %q", i, batch.Items[i].DedupKey, want)
		}
		if batch.Items[i].SourceType != sourceTypeClaudeConversation {
			t.Errorf("item %d SourceType = %q", i, batch.Items[i].SourceType)
		}
	}
	if !strings.HasPrefix(batch.Items[0].Body, "Human: ") {
		t.Errorf("human turn body = %q, want Human: prefix", batch.Items[0].Body)
	}
	if !strings.HasPrefix(batch.Items[1].Body, "Assistant: ") {
		t.Errorf("assistant turn body = %q, want Assistant: prefix", batch.Items[1].Body)
	}
	if !strings.Contains(batch.Items[2].Body, "Legacy flat-text message") {
		t.Errorf("flat-text turn not parsed: %q", batch.Items[2].Body)
	}
}

func TestClaudeChatsSource_ImportsBareJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "conversations.json")
	if err := os.WriteFile(path, []byte(sampleExport), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	src := NewClaudeChatsSource(path, fixedNow())
	batch, err := src.Pull(context.Background(), streamConversations, Watermark{}, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(batch.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(batch.Items))
	}
}

// TestClaudeChatsSource_OrchestratorTagsUntrusted proves the connector content
// engages the trust gate + leak gate: every episode is tagged connector:claude-
// chats (so status=proposed / no-supersede / no-mint and the plaintext-seal
// refusal all engage downstream) and is recorded at full confidence — trust is
// the source prefix + proposed status, not a depressed score (#212). This is the
// #212 fold-in of the trust-gate regression for the claude-chats source type.
func TestClaudeChatsSource_OrchestratorTagsUntrusted(t *testing.T) {
	t.Parallel()
	path := writeExportZip(t, map[string]string{"conversations.json": sampleExport})
	app := &fakeAppender{}
	o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
	report, err := o.Run(context.Background(), NewClaudeChatsSource(path, fixedNow()), kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != 3 {
		t.Fatalf("ItemsApplied = %d, want 3", report.ItemsApplied)
	}
	eps := app.eventsOfKind(ontology.KindEpisode)
	if len(eps) != 3 {
		t.Fatalf("episodes = %d, want 3", len(eps))
	}
	want := ontology.ConnectorSourceColonPrefix + connectorClaudeChats
	for i := range eps {
		if eps[i].Source != want {
			t.Errorf("episode %d Source = %q, want %q", i, eps[i].Source, want)
		}
		if eps[i].Confidence != 1.0 {
			t.Errorf("episode %d Confidence = %v, want 1.0 (trust via source prefix, not a cap)", i, eps[i].Confidence)
		}
	}
}

// TestClaudeChatsSource_ReimportIsNoop proves idempotency through the
// orchestrator: a second run whose dedup probe already knows every turn's content
// hash applies nothing.
func TestClaudeChatsSource_ReimportIsNoop(t *testing.T) {
	t.Parallel()
	path := writeExportZip(t, map[string]string{"conversations.json": sampleExport})

	app1 := &fakeAppender{}
	o1 := NewOrchestrator(app1, newFakeReader(), jobs.NewRegistry(), nil)
	r1, err := o1.Run(context.Background(), NewClaudeChatsSource(path, fixedNow()), kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Seed run 2's reader with the (dedupKey -> contentHash) the first run recorded.
	reader2 := newFakeReader()
	for _, ev := range app1.eventsOfKind(ontology.KindSourceRefRecorded) {
		var sr ontology.SourceRefRecordedEvent
		if uerr := json.Unmarshal(ev.Payload, &sr); uerr != nil {
			t.Fatalf("unmarshal sourceRef: %v", uerr)
		}
		reader2.hashes[sr.DedupKey] = sr.ContentHash
	}

	app2 := &fakeAppender{}
	o2 := NewOrchestrator(app2, reader2, jobs.NewRegistry(), nil)
	r2, err := o2.Run(context.Background(), NewClaudeChatsSource(path, fixedNow()), kernel.ScopePersonal, Bounds{}, false)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if r2.ItemsApplied != 0 || r2.ItemsSkipped != r1.ItemsApplied {
		t.Errorf("re-import not a no-op: applied=%d skipped=%d (want applied=0 skipped=%d)",
			r2.ItemsApplied, r2.ItemsSkipped, r1.ItemsApplied)
	}
	if len(app2.eventsOfKind(ontology.KindEpisode)) != 0 {
		t.Error("re-import appended episodes; expected none")
	}
}

func TestClaudeChatsSource_RejectsZipSlip(t *testing.T) {
	t.Parallel()
	path := writeExportZip(t, map[string]string{"../../etc/conversations.json": sampleExport})
	src := NewClaudeChatsSource(path, fixedNow())
	err := src.Validate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "zip slip") {
		t.Fatalf("want zip-slip refusal, got %v", err)
	}
}

func TestClaudeChatsSource_FailsLoudOnSchemaDrift(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, body, wantSub string }{
		{"top level object", `{"conversations": []}`, "JSON array"},
		{"renamed messages field", `[{"uuid":"c","messages":[{"sender":"human","text":"hi"}]}]`, "schema has likely changed"},
		{"empty file", ``, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeExportZip(t, map[string]string{"conversations.json": tc.body})
			src := NewClaudeChatsSource(path, fixedNow())
			err := src.Validate(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

func TestClaudeChatsSource_EmptyMessagesImportsWithoutDrift(t *testing.T) {
	t.Parallel()
	// A real export can contain conversations that carry zero messages
	// ("chat_messages": []). That is a present-but-empty key, NOT schema drift —
	// it must import as a no-op rather than triggering the loud drift guard.
	path := writeExportZip(t, map[string]string{
		"conversations.json": `[{"uuid":"c1","name":"Empty","chat_messages":[]}]`,
	})
	src := NewClaudeChatsSource(path, fixedNow())
	if err := src.Validate(context.Background()); err != nil {
		t.Fatalf("Validate rejected a valid empty-messages export: %v", err)
	}
	batch, err := src.Pull(context.Background(), streamConversations, Watermark{}, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(batch.Items) != 0 {
		t.Errorf("items = %d, want 0 (an empty conversation yields no turns)", len(batch.Items))
	}
}

func TestClaudeChatsSource_MissingConversationsEntry(t *testing.T) {
	t.Parallel()
	path := writeExportZip(t, map[string]string{"users.json": `{}`})
	src := NewClaudeChatsSource(path, fixedNow())
	err := src.Validate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

func TestReadCapped_RejectsOversize(t *testing.T) {
	t.Parallel()
	if _, err := readCapped(strings.NewReader("0123456789"), 4); err == nil {
		t.Fatal("want oversize error, got nil")
	}
	got, err := readCapped(strings.NewReader("ok"), 4)
	if err != nil || string(got) != "ok" {
		t.Fatalf("under-cap read failed: %q %v", got, err)
	}
}

// TestClaudeChatsSource_DrainedSnapshotRepullIsTerminal re-pulls a fully-drained
// snapshot: it must yield zero items, stay Done, and leave the sequence
// unchanged — the cursor the orchestrator validates relies on ValidateCursor's
// done && nextSeq==prevSeq allowance, so this pins that path.
func TestClaudeChatsSource_DrainedSnapshotRepullIsTerminal(t *testing.T) {
	t.Parallel()
	path := writeExportZip(t, map[string]string{"conversations.json": sampleExport})
	src := NewClaudeChatsSource(path, fixedNow())
	first, err := src.Pull(context.Background(), streamConversations, Watermark{}, 0)
	if err != nil {
		t.Fatalf("first pull: %v", err)
	}
	if !first.Done {
		t.Fatalf("first pull not done; cannot test the drained re-pull")
	}
	wm := Watermark{Cursor: first.NextCursor, Sequence: first.NextSequence}
	again, err := src.Pull(context.Background(), streamConversations, wm, 0)
	if err != nil {
		t.Fatalf("re-pull: %v", err)
	}
	if len(again.Items) != 0 || !again.Done {
		t.Fatalf("re-pull items=%d done=%v, want 0 + done", len(again.Items), again.Done)
	}
	if again.NextSequence != wm.Sequence {
		t.Errorf("NextSequence = %d, want %d (no progress on a drained snapshot)", again.NextSequence, wm.Sequence)
	}
	if verr := ontology.ValidateCursor(wm.Sequence, again.NextSequence, again.NextCursor, again.Done); verr != nil {
		t.Errorf("ValidateCursor rejected the drained re-pull: %v", verr)
	}
}

func TestClaudeChatsSource_SnapshotChangeResetsOffset(t *testing.T) {
	t.Parallel()
	path := writeExportZip(t, map[string]string{"conversations.json": sampleExport})
	src := NewClaudeChatsSource(path, fixedNow())
	// A persisted cursor from a DIFFERENT snapshot must not skip this file's turns.
	stale, err := json.Marshal(chatCursor{Snapshot: "deadbeef", Offset: 999})
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	batch, err := src.Pull(context.Background(), streamConversations, Watermark{Cursor: stale, Sequence: 999}, 0)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(batch.Items) != 3 {
		t.Fatalf("stale-snapshot cursor skipped turns: items = %d, want 3", len(batch.Items))
	}
	if batch.NextSequence <= 999 {
		t.Errorf("NextSequence = %d, want > 999 (monotonic)", batch.NextSequence)
	}
}
