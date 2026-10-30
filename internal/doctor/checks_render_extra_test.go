package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePlaintextScopeConfigForExtra is a sibling of the helper in
// checks_render_size_test.go, scoped to this test file so we don't
// risk a future Edit collapsing them and breaking parallel-table
// guarantees. Same TOML shape on purpose: identical contract for
// the same loader.
func writePlaintextScopeConfigForExtra(t *testing.T, dataDir, scope, url string) {
	t.Helper()
	content := "[sync.remotes." + scope + "]\n" +
		"url = \"" + url + "\"\n" +
		"encrypted = false\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"),
		[]byte(content), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

// writeRelationsJSONLForScope materialises a relations.jsonl under
// dataDir/sync/<scope>/repo/rendered/ containing `live` rows of
// active relations and `tomb` rows of superseded ones, in
// alternating order so a buggy line-skip would skew the ratio.
func writeRelationsJSONLForScope(t *testing.T, dataDir, scope string, live, tomb int) {
	t.Helper()
	root := filepath.Join(dataDir, "sync", scope, "repo", "rendered")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	n := live
	if tomb > n {
		n = tomb
	}
	for i := range n {
		if i < live {
			b.WriteString(`{"uid":"L` + itoaTest(i) + `","subject_uid":"S","predicate":"p","object_uid":"O"}` + "\n")
		}
		if i < tomb {
			b.WriteString(`{"uid":"T` + itoaTest(i) + `","subject_uid":"S","predicate":"p","object_uid":"O","superseded_by":"X"}` + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "relations.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write relations.jsonl: %v", err)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

func TestRenderTombstoneRatioCheck_NoRemotesIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, detail, err := newRenderTombstoneRatioCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK", status)
	}
	if !strings.Contains(detail, "n/a") {
		t.Errorf("detail = %q, want 'n/a' hint", detail)
	}
}

func TestRenderTombstoneRatioCheck_NoRelationsFileIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	status, detail, err := newRenderTombstoneRatioCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK (no relations.jsonl yet)", status)
	}
	if !strings.Contains(detail, "no relations.jsonl") {
		t.Errorf("detail = %q, want 'no relations.jsonl' hint", detail)
	}
}

func TestRenderTombstoneRatioCheck_HealthyRatioIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// 95 live, 5 tombstones → 5% — well below the 50% warn threshold.
	writeRelationsJSONLForScope(t, dir, "personal", 95, 5)
	status, detail, err := newRenderTombstoneRatioCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK on 5%% tombstone ratio", status)
	}
	if !strings.Contains(detail, "within budget") {
		t.Errorf("detail = %q, want 'within budget'", detail)
	}
}

func TestRenderTombstoneRatioCheck_HighRatioWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// 10 live, 40 tombstones → 80% — over the 50% threshold.
	writeRelationsJSONLForScope(t, dir, "personal", 10, 40)
	status, detail, err := newRenderTombstoneRatioCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want Warn on 80%% tombstone ratio", status)
	}
	if !strings.Contains(detail, "tombstone") || !strings.Contains(detail, "personal") {
		t.Errorf("detail = %q, want tombstone+scope hint", detail)
	}
}

// writeCursorAndManifest builds the .cursor.json + MANIFEST.json
// pair the cursor-behind check reads. lastUlid pins the cursor;
// manifestHead pins the manifest's tail entry and event count.
func writeCursorAndManifest(t *testing.T, dataDir, scope, cursorLastUlid string, manifestEntries []manifestEntry) {
	t.Helper()
	repoDir := filepath.Join(dataDir, "sync", scope, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "rendered"), 0o750); err != nil {
		t.Fatalf("mkdir rendered: %v", err)
	}
	cursorBytes := []byte(`{"lastUlid":"` + cursorLastUlid + `"}`)
	if err := os.WriteFile(filepath.Join(repoDir, "rendered", ".cursor.json"), cursorBytes, 0o600); err != nil {
		t.Fatalf("write cursor: %v", err)
	}
	var sb strings.Builder
	sb.WriteString(`{"chunks":[`)
	for i, e := range manifestEntries {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"lastUlid":"` + e.lastUlid + `","eventCount":` + itoaTest(e.count) + `}`)
	}
	sb.WriteString(`]}`)
	if err := os.WriteFile(filepath.Join(repoDir, "MANIFEST.json"), []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

type manifestEntry struct {
	lastUlid string
	count    int
}

func TestRenderCursorBehindCheck_KeepingPaceIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// Cursor is at the manifest's tail → 0 events behind.
	writeCursorAndManifest(t, dir, "personal", "01HZ_TAIL",
		[]manifestEntry{
			{"01HZ_A", 100},
			{"01HZ_TAIL", 50},
		})
	status, detail, err := newRenderCursorBehindCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK when cursor matches manifest tail", status)
	}
	if !strings.Contains(detail, "keeping pace") {
		t.Errorf("detail = %q, want 'keeping pace'", detail)
	}
}

func TestRenderCursorBehindCheck_FarBehindWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// Cursor is at "01HZ_A"; manifest has 2 newer chunks summing 500 events.
	writeCursorAndManifest(t, dir, "personal", "01HZ_A",
		[]manifestEntry{
			{"01HZ_A", 100},
			{"01HZ_B", 200},
			{"01HZ_C", 300},
		})
	status, detail, err := newRenderCursorBehindCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want Warn when cursor lags by 500 > 100 events", status)
	}
	if !strings.Contains(detail, "behind") || !strings.Contains(detail, "personal") {
		t.Errorf("detail = %q, want lag+scope hint", detail)
	}
}

func TestRenderCursorBehindCheck_NoArtifactsIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	status, detail, err := newRenderCursorBehindCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK on fresh install", status)
	}
	if !strings.Contains(detail, "no rendered cursor") {
		t.Errorf("detail = %q, want 'no rendered cursor'", detail)
	}
}

// writeChunkJSONL writes a single plaintext chunk under
// <dataDir>/sync/<scope>/repo/chunks/. Each line is a kernel.Event-
// shaped row, but only the `uid` + `kind` fields the stuck-
// candidate scan inspects are written — keeps the helper minimal
// while exercising the same byte-scan path.
func writeChunkJSONL(t *testing.T, dataDir, scope, chunkID string, lines []chunkLine) {
	t.Helper()
	chunksDir := filepath.Join(dataDir, "sync", scope, "repo", "chunks")
	if err := os.MkdirAll(chunksDir, 0o750); err != nil {
		t.Fatalf("mkdir chunks: %v", err)
	}
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(`{"uid":"` + ln.uid + `","kind":"` + ln.kind + `"}` + "\n")
	}
	if err := os.WriteFile(filepath.Join(chunksDir, chunkID+".jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
}

type chunkLine struct {
	uid  string
	kind string
}

// TestRelationCandidatesStuckCheck_AllPromotedIsOK is the regression gate for
// Finding 4: the pre-fix implementation compared relation-event UIDs against
// candidate-event UIDs, which are unrelated (relation events carry freshly-
// minted UIDs; the candidate UID they resolve is in the payload, not the
// event's own UID). This made the check always report every store as "stuck".
//
// The correct heuristic: if ANY relation events exist anywhere in the scope,
// the resolver ran — even if the counts do not match 1:1. A fully-resolved
// store with N candidates and N relations must report OK.
func TestRelationCandidatesStuckCheck_AllPromotedIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// Two candidates followed by relation rows with DIFFERENT UIDs (as
	// the real resolver produces). This is the pattern that used to false-
	// positive as "stuck" because the old code compared event UIDs and
	// never found a match.
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HCAND1", "relationCandidatesProposed"},
			{"01HCAND2", "relationCandidatesProposed"},
			{"01HREL01", "relation"}, // resolver-emitted UID, differs from candidate UID
			{"01HREL02", "relation"},
		})
	status, detail, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK when candidates AND relation events both exist (resolver ran)", status)
	}
	if !strings.Contains(detail, "no stuck") {
		t.Errorf("detail = %q, want 'no stuck'", detail)
	}
}

// TestRelationCandidatesStuckCheck_PartiallyResolvedIsOK verifies that a scope
// with more candidates than relation events still reports OK: the resolver ran
// (some relations exist) and in-flight work is not the same as a stuck pipeline.
// Pre-fix, this case falsely warned because UID matching never found a match.
func TestRelationCandidatesStuckCheck_PartiallyResolvedIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HCAND1", "relationCandidatesProposed"},
			{"01HCAND2", "relationCandidatesProposed"},
			{"01HCAND3", "relationCandidatesProposed"},
			{"01HREL01", "relation"}, // resolver ran; only one relation emitted so far
		})
	status, _, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK; resolver ran (at least one relation exists)", status)
	}
}

// TestRelationCandidatesStuckCheck_StuckCandidatesWarn verifies that a scope
// with candidates but ZERO relation or relationForgotten events produces a
// WARN. This is the only case where the resolver is truly stuck: it never
// consumed any candidates at all.
func TestRelationCandidatesStuckCheck_StuckCandidatesWarn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// Three candidates; no relation events of any kind → resolver never ran.
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HCAND1", "relationCandidatesProposed"},
			{"01HCAND2", "relationCandidatesProposed"},
			{"01HCAND3", "relationCandidatesProposed"},
		})
	status, detail, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want Warn when candidates exist but resolver never ran", status)
	}
	if !strings.Contains(detail, "unresolved") || !strings.Contains(detail, "personal") {
		t.Errorf("detail = %q, want unresolved+scope hint", detail)
	}
}

// TestRelationCandidatesStuckCheck_WorkedBeforeStuckNowWarns is the regression
// gate for the "historical relation masks stall" bug. The resolver ran on an
// early chunk (chunk A has both candidates and a relation), but then new chunks
// arrived containing only candidates with no corresponding resolutions. The
// old global-hasResolved heuristic would see the old relation and report OK;
// the corrected implementation scopes the resolved signal to chunks at or after
// the most-recent candidates chunk and correctly warns.
func TestRelationCandidatesStuckCheck_WorkedBeforeStuckNowWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// Chunk A (older, lexicographically smaller): candidates + resolution — resolver ran.
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HCAND1", "relationCandidatesProposed"},
			{"01HREL01", "relation"},
		})
	// Chunk B (newer): new candidates only — resolver has stopped consuming them.
	writeChunkJSONL(t, dir, "personal", "01HZB",
		[]chunkLine{
			{"01HCAND2", "relationCandidatesProposed"},
			{"01HCAND3", "relationCandidatesProposed"},
		})
	status, detail, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want Warn when recent chunks have unresolved candidates despite earlier success", status)
	}
	if !strings.Contains(detail, "unresolved") || !strings.Contains(detail, "personal") {
		t.Errorf("detail = %q, want unresolved+scope hint", detail)
	}
}

// TestRelationCandidatesStuckCheck_SameChunkRelationBeforeCandidateWarns is the
// regression gate for the "same-chunk relation masks stall" bug. If the last
// candidate chunk contains a relation event BEFORE the final
// relationCandidatesProposed event, the old boolean heuristic would see
// hasResolved=true and incorrectly report OK. The corrected implementation
// checks whether a resolution appears after the last candidate, not just
// whether one exists anywhere in the chunk.
func TestRelationCandidatesStuckCheck_SameChunkRelationBeforeCandidateWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// Single chunk: a relation for earlier work appears first, then new
	// candidates with no following resolution.
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HREL01", "relation"},                   // older resolved work
			{"01HCAND2", "relationCandidatesProposed"}, // new unresolved candidates
			{"01HCAND3", "relationCandidatesProposed"},
		})
	status, detail, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Errorf("status = %v, want Warn when relation precedes new candidates in same chunk", status)
	}
	if !strings.Contains(detail, "unresolved") || !strings.Contains(detail, "personal") {
		t.Errorf("detail = %q, want unresolved+scope hint", detail)
	}
}

// TestRelationCandidatesStuckCheck_SameChunkRelationAfterCandidateIsOK verifies
// that a chunk shaped as [candidatesProposed, relation] (resolution follows the
// candidate) correctly reports OK — the resolver consumed the proposal.
func TestRelationCandidatesStuckCheck_SameChunkRelationAfterCandidateIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HCAND1", "relationCandidatesProposed"},
			{"01HREL01", "relation"}, // resolution follows candidate in same chunk
		})
	status, _, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK when resolution follows candidate in same chunk", status)
	}
}

// TestRelationCandidatesStuckCheck_ResolvedAfterCandidatesChunkIsOK verifies
// that when resolution events land in a chunk AFTER the candidates chunk, the
// check correctly reports OK (resolver is keeping up, just emitting to a
// different chunk).
func TestRelationCandidatesStuckCheck_ResolvedAfterCandidatesChunkIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	// Chunk A: candidates only (resolver hasn't emitted yet for this batch).
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HCAND1", "relationCandidatesProposed"},
		})
	// Chunk B: resolutions land in the next chunk (cross-chunk resolution).
	writeChunkJSONL(t, dir, "personal", "01HZB",
		[]chunkLine{
			{"01HREL01", "relation"},
		})
	status, _, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK when resolutions arrive in the chunk after candidates", status)
	}
}

func TestRelationCandidatesStuckCheck_NoChunksIsOK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePlaintextScopeConfigForExtra(t, dir, "personal", "git@example.com:o/personal.git")
	status, detail, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK on fresh install", status)
	}
	if !strings.Contains(detail, "no chunks") {
		t.Errorf("detail = %q, want 'no chunks'", detail)
	}
}

func TestRelationCandidatesStuckCheck_SkipsEncryptedScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Encrypted scope config — the check must not touch its chunks.
	content := `[sync.remotes.personal]
url = "git@example.com:o/p.git"
encrypted = true
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	// Drop a stuck-candidate chunk that WOULD warn if the check
	// scanned it. The encrypted-skip means this chunk is invisible.
	writeChunkJSONL(t, dir, "personal", "01HZA",
		[]chunkLine{
			{"01HCAND1", "relationCandidatesProposed"},
			{"01HCAND2", "relationCandidatesProposed"},
		})
	status, detail, err := newRelationCandidatesStuckCheck(dir).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v, want OK on encrypted-only setup", status)
	}
	if !strings.Contains(detail, "no chunks") && !strings.Contains(detail, "no stuck") {
		t.Errorf("detail = %q, want a 'no … on disk' verdict", detail)
	}
}
