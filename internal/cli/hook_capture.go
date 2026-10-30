package cli

// hook_capture.go — the passive-capture mechanics behind `maz hook stop`:
// decode the Stop payload, read only the transcript turns added since the last
// fire (a per-session byte cursor), distill them within a budget, and shape the
// `remember` request. All parsing is defensive: Claude Code's transcript JSONL
// schema is not a stable public API, so an unparseable line is skipped, never
// fatal.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Episode source string for Claude Code Stop captures.
	hookCaptureSource = "hook:claude-code/stop"
	// Writer confidence for passively captured episodes — below an explicit
	// `maz remember` (1.0) because the text is auto-distilled from a transcript
	// rather than asserted by a user.
	passiveCaptureConfidence = 0.6
	// Cap on the hook payload read from stdin. The payload is small (it
	// references the transcript by path), so the cap only guards a malformed
	// unbounded stream.
	maxStdinBytes = 1 << 20 // 1 MiB
	// Cap on the bytes read from the transcript per capture. A backlog larger
	// than this keeps only the most recent tail.
	maxTranscriptScanBytes = 256 << 10 // 256 KiB
	// Cap on the distilled episode text. The tail (most recent turns) is kept
	// when the delta exceeds it.
	maxEpisodeBytes = 16 << 10 // 16 KiB
)

// stopPayload is the subset of Claude Code's Stop-hook stdin JSON we use.
type stopPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// decodeStopPayload reads and decodes the Stop payload from stdin (bounded).
func decodeStopPayload(r io.Reader) (stopPayload, error) {
	var p stopPayload
	dec := json.NewDecoder(io.LimitReader(r, maxStdinBytes))
	if err := dec.Decode(&p); err != nil {
		return stopPayload{}, fmt.Errorf("decode stop payload: %w", err)
	}
	return p, nil
}

// buildHookRememberRequest shapes the `remember` tool input for a passive
// capture: actor=assistant routes the text through the assistant extraction
// branch, and the hook source marks its provenance.
func buildHookRememberRequest(text string) (json.RawMessage, error) {
	in := map[string]any{
		"text":       text,
		"source":     hookCaptureSource,
		"actor":      "assistant",
		"confidence": passiveCaptureConfidence,
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal remember request: %w", err)
	}
	return body, nil
}

// captureDelta reads the transcript turns added since the last fire for this
// session and returns the distilled text plus the new cursor offset (bytes
// consumed). When the delta exceeds maxTranscriptScanBytes only the most recent
// tail is read; the cursor still advances past the skipped bytes.
func captureDelta(dataDir, sessionID, transcriptPath string) (text string, newOffset int64, err error) {
	offset := readCursor(dataDir, sessionID)
	// transcriptPath is supplied by the local Claude Code process via its own
	// Stop-hook payload and points at a file that process already owns; the hook
	// runs as the same user, so opening it grants no access that user lacks and
	// the read is best-effort + fail-open. Not constrained to a fixed directory
	// because Claude Code's transcript location varies (per-project, per-session).
	f, err := os.Open(filepath.Clean(transcriptPath)) // #nosec G304 -- path is the trusted hook payload's own transcript; same-user, read-only, fail-open.
	if err != nil {
		return "", offset, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", offset, fmt.Errorf("stat transcript: %w", err)
	}
	size := info.Size()
	if offset > size {
		offset = 0 // transcript truncated / rotated (session id reused) — re-scan
	}

	start := offset
	skipFirstPartial := false
	if size-start > maxTranscriptScanBytes {
		start = size - maxTranscriptScanBytes
		skipFirstPartial = true // we seek into the middle of a record
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", offset, fmt.Errorf("seek transcript: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTranscriptScanBytes+1))
	if err != nil {
		return "", offset, fmt.Errorf("read transcript: %w", err)
	}

	// Consume only through the last complete line; a trailing partial line
	// (mid-write) is left for the next fire.
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		// No complete line in this window: the transcript's tail is an
		// unterminated record still being written. Do NOT advance the cursor —
		// re-scan next fire once the line completes, rather than dropping
		// in-flight data.
		return "", offset, nil
	}
	newOffset = start + int64(lastNL) + 1

	lines := bytes.Split(data[:lastNL+1], []byte{'\n'})
	if skipFirstPartial && len(lines) > 0 {
		lines = lines[1:] // first line is a fragment from a mid-record seek
	}
	turns := make([]string, 0, len(lines))
	for _, ln := range lines {
		if role, txt, ok := extractTurn(ln); ok {
			turns = append(turns, role+": "+txt+"\n")
		}
	}
	return distillTurns(turns, maxEpisodeBytes), newOffset, nil
}

// extractTurn pulls a (role, text) pair from one transcript JSONL line, or
// reports ok=false for non-message lines (tool results, metadata) and turns
// with no text. Only user and assistant roles are captured.
func extractTurn(line []byte) (role, text string, ok bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return "", "", false
	}
	var rec map[string]any
	if json.Unmarshal(line, &rec) != nil {
		return "", "", false
	}
	// Role comes only from message.role. The top-level record `type` is NOT a
	// fallback: tool-result records carry type="user" with no conversational
	// text, and reading role from `type` would misclassify them. A record with
	// no message.role is metadata and is skipped.
	msg, _ := rec["message"].(map[string]any) //nolint:errcheck // absent/!map → no role below
	role, _ = msg["role"].(string)            //nolint:errcheck // absent → filtered out below
	if role != "user" && role != "assistant" {
		return "", "", false
	}
	text = strings.TrimSpace(extractContent(msg["content"]))
	if text == "" {
		return "", "", false
	}
	return role, text, true
}

// extractContent renders a transcript message's content to plain text. Content
// is either a string or an array of blocks; only text blocks are kept (tool_use
// / tool_result / thinking blocks are dropped as capture noise).
func extractContent(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, blk := range v {
			bm, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			t, _ := bm["type"].(string) //nolint:errcheck // non-text blocks are skipped
			if t != "text" {
				continue
			}
			if s, ok := bm["text"].(string); ok && s != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// distillTurns joins turns oldest-to-newest, keeping the most recent tail
// within budget bytes. At least one turn is always kept (even if it alone
// exceeds the budget) so a single large final turn is never dropped entirely.
func distillTurns(turns []string, budget int) string {
	if len(turns) == 0 {
		return ""
	}
	var picked []string
	total := 0
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if total+len(t) > budget && len(picked) > 0 {
			break
		}
		picked = append(picked, t)
		total += len(t)
	}
	// picked is newest-first; reverse to chronological order.
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return strings.Join(picked, "")
}

// ---- per-session delta cursor ----

// cursorState is the on-disk capture cursor: the transcript byte offset already
// consumed for a session.
type cursorState struct {
	Offset int64 `json:"offset"`
}

// cursorPath is <dataDir>/hooks/sessions/<sanitized session id>.json.
func cursorPath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "hooks", "sessions", sanitizeSessionID(sessionID)+".json")
}

// sanitizeSessionID maps a session id to a safe filename component. A real
// Claude Code session id is a lowercase-hex UUID and passes through verbatim.
// Anything else — an id with a path separator, a dot-only name, or any
// character outside [A-Za-z0-9._-] — is replaced by a SHA-256 hash, so a
// hostile id cannot escape the cursor directory AND two distinct ids can never
// collapse to the same cursor file (which in-place character replacement would
// allow, e.g. "a/b" and "a_b").
func sanitizeSessionID(id string) string {
	if isSafeFilenameComponent(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "h_" + hex.EncodeToString(sum[:16])
}

// isSafeFilenameComponent reports whether id is a non-empty string of
// [A-Za-z0-9._-] that is not a dot-reference ("." / "..").
func isSafeFilenameComponent(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// readCursor returns the stored offset for a session, or 0 when absent /
// unreadable / no session id (treat every fire as fresh).
func readCursor(dataDir, sessionID string) int64 {
	if sessionID == "" {
		return 0
	}
	data, err := os.ReadFile(cursorPath(dataDir, sessionID))
	if err != nil {
		return 0
	}
	var c cursorState
	if json.Unmarshal(data, &c) != nil {
		return 0
	}
	if c.Offset < 0 {
		return 0
	}
	return c.Offset
}

// writeCursor persists the offset for a session. A missing session id is a
// no-op (we cannot key a cursor without one).
func writeCursor(dataDir, sessionID string, offset int64) error {
	if sessionID == "" {
		return nil
	}
	p := cursorPath(dataDir, sessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mkdir cursor dir: %w", err)
	}
	data, err := json.Marshal(cursorState{Offset: offset})
	if err != nil {
		return fmt.Errorf("marshal cursor: %w", err)
	}
	// Write atomically (tmp + rename) so a second Stop fire racing this one
	// cannot observe a torn cursor file.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write cursor tmp: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("rename cursor: %w", err)
	}
	return nil
}
