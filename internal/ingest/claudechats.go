package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// connectorClaudeChats is the ClaudeChatsSource name; it tags Event.Source as
// "connector:claude-chats".
const connectorClaudeChats = "claude-chats"

// streamConversations is the single stream ClaudeChatsSource pulls.
const streamConversations = "conversations"

// sourceTypeClaudeConversation is the open-taxonomy SourceType stamped on every
// ingested chat turn's SourceRef.
const sourceTypeClaudeConversation = "claude_conversation"

// Hardening caps for the untrusted export archive. The Anthropic account-data
// export is opened as an attacker-controlled file (it arrives over email), so
// every size is bounded BEFORE it is materialised in memory: a compressed-archive
// ceiling, an uncompressed conversations.json ceiling enforced by streaming (the
// zip header's declared size is not trusted), and an entry-count ceiling against
// fan-out zip bombs.
const (
	maxExportArchiveBytes     = 1 << 30   // 1 GiB compressed archive ceiling
	maxConversationsJSONBytes = 512 << 20 // 512 MiB uncompressed conversations.json ceiling
	maxExportZipEntries       = 100000    // refuse absurd entry counts (zip-bomb fan-out)
)

// conversationsFileName is the entry the export bundle stores the chat history
// in. Anthropic publishes the export process but NOT the archive schema, so this
// is an observed convention, not a contract — hence the loud schema-drift guards.
const conversationsFileName = "conversations.json"

// ClaudeChatsSource ingests a Claude account-data export (Settings -> Privacy ->
// Export data): a ZIP containing conversations.json, or a bare conversations.json.
// It is exported-file import ONLY — no network, no session token, no scrape. The
// export is a full snapshot every time, so dedup is carried by the per-turn
// SourceRef (content hash); the watermark only records which snapshot was last
// seen so an unchanged re-import is a no-op.
//
// EXPERIMENTAL: the export schema is observed, not contracted, and can change
// without notice. The parser is forward-compatible (unknown fields tolerated)
// but fails LOUDLY on structural drift rather than silently importing nothing.
type ClaudeChatsSource struct {
	path string
	now  func() time.Time

	// Loaded lazily by load() and reused across Pull calls within one run (the
	// orchestrator drives a single source sequentially). snapshot is the content
	// hash of the raw conversations.json; items is the flattened turn list.
	loaded   bool
	snapshot string
	items    []Item
}

// NewClaudeChatsSource builds a ClaudeChatsSource reading the export at path
// (a .zip bundle or a bare conversations.json). Now may be nil (defaults to
// time.Now), overridable for deterministic tests.
func NewClaudeChatsSource(path string, now func() time.Time) *ClaudeChatsSource {
	if now == nil {
		now = time.Now
	}
	return &ClaudeChatsSource{path: path, now: now}
}

// Name implements Source.
func (s *ClaudeChatsSource) Name() string { return connectorClaudeChats }

// Streams implements Source: a single "conversations" stream.
func (s *ClaudeChatsSource) Streams() []string { return []string{streamConversations} }

// Validate implements Source: open + parse the export and fail loudly on
// structural drift, WITHOUT advancing any watermark. The parse is cached so Pull
// does not redo it.
func (s *ClaudeChatsSource) Validate(_ context.Context) error {
	return s.load()
}

// Pull implements Source. The export is a full snapshot, so the cursor is
// {"snapshot": <conversations.json hash>, "offset": N}: offset paginates within
// one snapshot to honour limit; when the persisted cursor's snapshot no longer
// matches the file (a fresh export), offset resets to 0 and the whole snapshot is
// re-scanned — the orchestrator's content-hash dedup then skips every turn it has
// already ingested, so only genuinely new turns are appended.
func (s *ClaudeChatsSource) Pull(_ context.Context, stream string, wm Watermark, limit int) (Batch, error) {
	if stream != streamConversations {
		return Batch{}, fmt.Errorf("ingest: unknown claude-chats stream %q", stream)
	}
	if err := s.load(); err != nil {
		return Batch{}, err
	}

	cur, err := decodeChatCursor(wm.Cursor)
	if err != nil {
		return Batch{}, err
	}
	offset := 0
	if cur.Snapshot == s.snapshot {
		offset = cur.Offset // same snapshot: resume; a different snapshot resets to 0
	}
	if offset > len(s.items) {
		offset = len(s.items) // stale offset past EOF (shrunk export) => drained
	}

	end := len(s.items)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	items := s.items[offset:end]
	cursor, err := json.Marshal(chatCursor{Snapshot: s.snapshot, Offset: end})
	if err != nil {
		return Batch{}, fmt.Errorf("ingest: encode claude-chats cursor: %w", err)
	}
	return Batch{
		Items:        items,
		NextCursor:   cursor,
		NextSequence: wm.Sequence + uint64(len(items)),
		Done:         end >= len(s.items),
	}, nil
}

// chatCursor is the opaque resume cursor for the conversations stream. Offset is
// the index into the flattened turn list reached within Snapshot; a snapshot
// change resets offset to 0 (full re-scan, dedup carries idempotency).
type chatCursor struct {
	Snapshot string `json:"snapshot"`
	Offset   int    `json:"offset"`
}

func decodeChatCursor(raw json.RawMessage) (chatCursor, error) {
	var c chatCursor
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("ingest: decode claude-chats cursor: %w", err)
	}
	return c, nil
}

// load reads and parses the export once, caching the snapshot hash and the
// flattened turn list. Idempotent: safe to call from both Validate and Pull.
func (s *ClaudeChatsSource) load() error {
	if s.loaded {
		return nil
	}
	data, err := s.readConversationsJSON()
	if err != nil {
		return err
	}
	items, err := s.parseConversations(data)
	if err != nil {
		return err
	}
	s.snapshot = contentHashBytes(data)
	s.items = items
	s.loaded = true
	return nil
}

// readConversationsJSON returns the raw conversations.json bytes from either a
// .zip export bundle or a bare conversations.json, applying the hardening caps.
func (s *ClaudeChatsSource) readConversationsJSON() ([]byte, error) {
	if strings.EqualFold(filepath.Ext(s.path), ".zip") {
		return s.readFromZip()
	}
	if strings.EqualFold(filepath.Base(s.path), conversationsFileName) {
		return s.readFromFile()
	}
	return nil, fmt.Errorf("ingest: %q is neither a .zip export nor %s", s.path, conversationsFileName)
}

// readFromFile reads a bare conversations.json with a streaming size cap.
func (s *ClaudeChatsSource) readFromFile() ([]byte, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("ingest: open export: %w", err)
	}
	defer f.Close()
	data, err := readCapped(f, maxConversationsJSONBytes)
	if err != nil {
		return nil, fmt.Errorf("ingest: read %s: %w", conversationsFileName, err)
	}
	return data, nil
}

// readFromZip locates conversations.json inside the export bundle and reads it
// with full untrusted-archive hardening: an archive-size ceiling, an entry-count
// ceiling, per-entry path-traversal (zip-slip) rejection, and a streamed
// uncompressed-size cap that does not trust the zip header.
func (s *ClaudeChatsSource) readFromZip() ([]byte, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, fmt.Errorf("ingest: stat export archive: %w", err)
	}
	if info.Size() > maxExportArchiveBytes {
		return nil, fmt.Errorf("ingest: export archive is %d bytes, exceeds cap %d", info.Size(), maxExportArchiveBytes)
	}
	zr, err := zip.OpenReader(s.path)
	if err != nil {
		return nil, fmt.Errorf("ingest: open export archive: %w", err)
	}
	defer zr.Close() //nolint:errcheck // read-only handle
	if len(zr.File) > maxExportZipEntries {
		return nil, fmt.Errorf("ingest: export archive has %d entries, exceeds cap %d (possible zip bomb)", len(zr.File), maxExportZipEntries)
	}
	for _, f := range zr.File {
		if !safeZipEntryName(f.Name) {
			return nil, fmt.Errorf("ingest: refusing unsafe archive entry path %q (zip slip)", f.Name)
		}
		if filepath.Base(f.Name) != conversationsFileName {
			continue
		}
		rc, oerr := f.Open()
		if oerr != nil {
			return nil, fmt.Errorf("ingest: open archive entry %q: %w", f.Name, oerr)
		}
		data, rerr := readCapped(rc, maxConversationsJSONBytes)
		rc.Close() //nolint:errcheck // read-only handle
		if rerr != nil {
			return nil, fmt.Errorf("ingest: read archive entry %q: %w", f.Name, rerr)
		}
		return data, nil
	}
	return nil, fmt.Errorf("ingest: %s not found in export archive %q", conversationsFileName, s.path)
}

// readCapped reads from r up to maxBytes, returning an error if the stream
// exceeds it. It reads maxBytes+1 so the overflow is detected from the stream
// itself rather than a (attacker-controlled) declared size.
func readCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("content exceeds %d bytes (possible decompression bomb)", maxBytes)
	}
	return data, nil
}

// safeZipEntryName rejects path-traversal and absolute entry names. The reader
// only ever reads a named entry into memory (it never extracts to disk), so this
// is defence-in-depth, but it keeps the connector safe if extraction is ever
// added and satisfies the untrusted-input requirement.
func safeZipEntryName(name string) bool {
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	return clean != ".." && !strings.HasPrefix(clean, "../") && !strings.Contains(clean, "/../")
}

// chatConversation / chatMessage / chatContent model the observed export schema.
// Unknown fields are tolerated (forward-compatible); structural breakage is
// caught by parseConversations.
type chatConversation struct {
	UUID         string        `json:"uuid"`
	Name         string        `json:"name"`
	ChatMessages []chatMessage `json:"chat_messages"`
}

type chatMessage struct {
	UUID    string        `json:"uuid"`
	Sender  string        `json:"sender"`
	Text    string        `json:"text"`
	Content []chatContent `json:"content"`
}

type chatContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseConversations flattens the export into one Item per human/assistant turn.
// It fails LOUDLY on structural drift — a top level that is not a JSON array, or
// a non-empty conversation list that yields no messages at all (the message field
// names almost certainly changed) — rather than silently importing nothing.
func (s *ClaudeChatsSource) parseConversations(data []byte) ([]Item, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("ingest: claude-chats export is empty")
	}
	if trimmed[0] != '[' {
		return nil, errors.New("ingest: claude-chats export must be a JSON array of conversations (schema drift or wrong file)")
	}
	var convs []chatConversation
	if err := json.Unmarshal(trimmed, &convs); err != nil {
		return nil, fmt.Errorf("ingest: decode claude-chats conversations (schema drift?): %w", err)
	}

	fetch := s.now().UTC()
	items := make([]Item, 0)
	// sawMessagesKey distinguishes a real schema break (the chat_messages field
	// was renamed/removed, so every conversation decodes to a nil slice) from a
	// benign export where conversations legitimately carry zero messages (an empty
	// [] decodes to a non-nil empty slice). encoding/json leaves an absent/null
	// key as a nil slice and an explicit [] as a non-nil empty slice, so nil-ness
	// is the key-presence signal — only the former is loud drift. Without this, an
	// account whose conversations all have "chat_messages": [] would fail to import.
	sawMessagesKey := false
	for ci := range convs {
		c := &convs[ci]
		if c.ChatMessages != nil {
			sawMessagesKey = true
		}
		for mi := range c.ChatMessages {
			m := &c.ChatMessages[mi]
			text := messageText(m)
			if strings.TrimSpace(text) == "" {
				continue // non-text turn (image / tool-only) — text-only in v1
			}
			body := senderRole(m.Sender) + ": " + strings.TrimSpace(text)
			items = append(items, Item{
				DedupKey:    chatDedupKey(c.UUID, ci, m.UUID, mi),
				ExternalID:  m.UUID,
				SourceType:  sourceTypeClaudeConversation,
				ContentHash: contentHash(body),
				Body:        body,
				FetchTime:   fetch,
			})
		}
	}
	if len(convs) > 0 && !sawMessagesKey {
		return nil, fmt.Errorf("ingest: parsed %d claude-chats conversations but none carried a chat_messages field — the export schema has likely changed (failing loud rather than importing nothing)", len(convs))
	}
	return items, nil
}

// messageText prefers the typed content[] text blocks (the current export shape)
// and falls back to the flat text field (older exports), so both shapes parse.
func messageText(m *chatMessage) string {
	var b strings.Builder
	for i := range m.Content {
		if m.Content[i].Type == "text" && m.Content[i].Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(m.Content[i].Text)
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	return m.Text
}

// senderRole normalises the export's sender into a human-readable role prefix.
func senderRole(sender string) string {
	switch strings.ToLower(strings.TrimSpace(sender)) {
	case "human":
		return "Human"
	case "assistant":
		return "Assistant"
	case "":
		return "Unknown"
	default:
		return sender
	}
}

// chatDedupKey builds the stable idempotency key for a turn. UUIDs are present in
// real exports; the positional fallbacks keep the key total (and an unchanged
// re-export a no-op) when a uuid is ever absent.
func chatDedupKey(convUUID string, ci int, msgUUID string, mi int) string {
	cv := convUUID
	if cv == "" {
		cv = fmt.Sprintf("c%d", ci)
	}
	mv := msgUUID
	if mv == "" {
		mv = fmt.Sprintf("m%d", mi)
	}
	return "claude-chats:" + cv + ":" + mv
}
