package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// TestComputePullDigest_GroupsCountsByAction folds a synthetic diff-entry set
// into the grouped digest and asserts each bucket lands in the right counter.
func TestComputePullDigest_GroupsCountsByAction(t *testing.T) {
	t.Parallel()

	entries := []diffEntry{
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Frodo"},
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "person/Sam"},
		{Action: diffActionAdded, Kind: diffKindEntity, Target: "organization/Shire"},
		{Action: diffActionChanged, Kind: diffKindEntity, Target: "person/Bilbo"},
		{Action: diffActionOrphaned, Kind: diffKindEntity, Target: "person/Gollum"},
		{Action: diffActionAdded, Kind: diffKindRelation, Target: "a knows b"},
		{Action: diffActionAdded, Kind: diffKindRelation, Target: "a lives_in c"},
		{Action: diffActionSuperseded, Kind: diffKindRelation, Target: "a role_at d"},
		{Action: diffActionForgotten, Kind: diffKindRelation, Target: "a knows e"},
	}
	docs := digestDocCounts{Added: 2, Changed: 1}

	d := computePullDigest([]string{"personal"}, entries, docs)

	if d.Entities.Added != 3 || d.Entities.Changed != 1 || d.Entities.Orphaned != 1 {
		t.Errorf("entity counts = %+v, want added=3 changed=1 orphaned=1", d.Entities)
	}
	if d.Relations.Added != 2 || d.Relations.Superseded != 1 || d.Relations.Forgotten != 1 {
		t.Errorf("relation counts = %+v, want added=2 superseded=1 forgotten=1", d.Relations)
	}
	if d.Documents != docs {
		t.Errorf("document counts = %+v, want %+v", d.Documents, docs)
	}
	if len(d.Entries) != len(entries) {
		t.Errorf("entries length = %d, want %d", len(d.Entries), len(entries))
	}
	if !d.hasChanges() {
		t.Error("hasChanges() = false, want true")
	}
}

// TestDigestSegments_MatchesBriefExample pins the human one-liner against the
// canonical example, exercising pluralization, the proposed nudge, the closed
// glyph, and document counts in one shot.
func TestDigestSegments_MatchesBriefExample(t *testing.T) {
	t.Parallel()

	d := pullDigest{
		Entities:  digestEntityCounts{Added: 3, Changed: 1},
		Relations: digestRelationCounts{Added: 5, Forgotten: 1, Proposed: 1},
		Documents: digestDocCounts{Added: 2},
	}
	got := strings.Join(digestSegments(&d), " · ")
	want := "+3 entities · ~1 entity · +5 relations (1 proposed → run maz review proposed) · ⊗1 relation closed · +2 documents"
	if got != want {
		t.Errorf("digest line mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestDigestSegments_SingularAndNoProposed checks singular nouns and that the
// proposed nudge is omitted when no relation is awaiting review.
func TestDigestSegments_SingularAndNoProposed(t *testing.T) {
	t.Parallel()

	d := pullDigest{
		Entities:  digestEntityCounts{Added: 1},
		Relations: digestRelationCounts{Added: 1, Superseded: 1},
		Documents: digestDocCounts{Removed: 1},
	}
	got := strings.Join(digestSegments(&d), " · ")
	want := "+1 entity · +1 relation · ⤳1 relation superseded · -1 document"
	if got != want {
		t.Errorf("digest line mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestWritePullDigestText_EmptyWritesNothing verifies a no-change digest stays
// silent (the "no-op pull prints nothing noisy" contract).
func TestWritePullDigestText_EmptyWritesNothing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	d := pullDigest{}
	if err := writePullDigestText(&buf, &d); err != nil {
		t.Fatalf("writePullDigestText: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty digest, got %q", buf.String())
	}
	if d.hasChanges() {
		t.Error("empty digest hasChanges() = true, want false")
	}
}

// TestPullDigest_SaveLoadRoundTrip pins the cache `maz changed` re-reads.
func TestPullDigest_SaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	want := pullDigest{
		Entities:          digestEntityCounts{Added: 2, Changed: 1},
		Relations:         digestRelationCounts{Added: 3},
		Documents:         digestDocCounts{Added: 1},
		Scopes:            []string{"personal"},
		Entries:           []diffEntry{{Action: diffActionAdded, Kind: diffKindRelation, Target: "a knows b"}},
		AddedRelationUIDs: []string{"rel-1", "rel-2", "rel-3"},
		PullTime:          time.Now().UTC().Format(time.RFC3339),
	}
	if err := savePullDigest(dir, &want); err != nil {
		t.Fatalf("savePullDigest: %v", err)
	}
	if _, err := os.Stat(pullDigestPath(dir)); err != nil {
		t.Fatalf("digest cache not written: %v", err)
	}
	got, err := loadPullDigest(dir)
	if err != nil {
		t.Fatalf("loadPullDigest: %v", err)
	}
	if got.Entities != want.Entities || got.Relations != want.Relations || got.Documents != want.Documents {
		t.Errorf("counts round-trip mismatch: got %+v, want %+v", got, want)
	}
	if len(got.AddedRelationUIDs) != 3 {
		t.Errorf("AddedRelationUIDs round-trip = %v, want 3 entries", got.AddedRelationUIDs)
	}
}

// TestNewChangedCmd_HelpHasExamples asserts the help is non-empty and teaches
// with example invocations.
func TestNewChangedCmd_HelpHasExamples(t *testing.T) {
	t.Parallel()

	cmd := newChangedCmd()
	if cmd.Long == "" {
		t.Fatal("changed --help Long is empty")
	}
	for _, want := range []string{"Examples:", "maz changed", "maz changed --json"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("changed --help should contain %q; Long:\n%s", want, cmd.Long)
		}
	}
}

// TestChanged_NoDigestStableOutput covers the "nothing recorded yet" path in
// both modes and asserts a clean exit (code 0).
func TestChanged_NoDigestStableOutput(t *testing.T) {
	t.Parallel()

	t.Run("json emits parseable empty digest", func(t *testing.T) {
		t.Parallel()
		cmd, out, _ := newChangedTestCmd(t.TempDir(), "")
		if err := runChanged(cmd); err != nil {
			t.Fatalf("runChanged json: %v", err)
		}
		var d pullDigest
		if err := json.Unmarshal(out.Bytes(), &d); err != nil {
			t.Fatalf("stdout %q is not parseable JSON: %v", out.String(), err)
		}
		if d.hasChanges() {
			t.Errorf("empty digest should report no changes, got %+v", d)
		}
	})

	t.Run("text reports nothing recorded", func(t *testing.T) {
		t.Parallel()
		cmd, out, _ := newChangedTestCmd(t.TempDir(), "text")
		if err := runChanged(cmd); err != nil {
			t.Fatalf("runChanged text: %v", err)
		}
		if !strings.Contains(out.String(), "no pull digest recorded yet") {
			t.Errorf("unexpected text output: %q", out.String())
		}
	})
}

// TestChanged_CorruptCacheExitsTwo verifies the exit-code contract: an
// unexpected failure reading the cache returns silentError code 2.
func TestChanged_CorruptCacheExitsTwo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(pullDigestPath(dir)), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pullDigestPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt cache: %v", err)
	}
	cmd, _, _ := newChangedTestCmd(dir, "")
	err := runChanged(cmd)
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("runChanged returned %T, want silentError", err)
	}
	if se.code != 2 {
		t.Errorf("exit code = %d, want 2", se.code)
	}
}

// TestPull_DigestAfterRealPull is the end-to-end acceptance: device A seals a
// chunk with two entities + one relation and pushes it (which renders the
// graph); device B pulls and the digest reports the grouped counts; `maz
// changed` re-prints them; and --json carries the structured shape.
func TestPull_DigestAfterRealPull(t *testing.T) {
	t.Parallel()
	skipIfShortLiveBackend(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary required: %v", err)
	}

	ctx := context.Background()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare", "--quiet",
		"--initial-branch=main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	remoteURL := "file://" + remoteDir

	// Device A: seal a chunk carrying two entities and a relation, then push
	// (which regenerates the rendered/ tree the digest diffs).
	dirA := t.TempDir()
	writePersonalConfigToPath(t, dirA, remoteURL)
	storeA, err := chunk.NewFSStore(dirA)
	if err != nil {
		t.Fatalf("NewFSStore A: %v", err)
	}
	const (
		frodoUID = "01HZA0000000000000000FRD12"
		shireUID = "01HZA0000000000000000SHR34"
		relUID   = "01HZA0000000000000000RNK56"
	)
	events := []kernel.Event{
		entityEvent(t, frodoUID, "Frodo", ontology.EntityKindPerson),
		entityEvent(t, shireUID, "Shire", ontology.EntityKindOrganization),
		relationEvent(t, relUID, frodoUID, "lives_in", shireUID),
	}
	blob, hdr, err := chunk.EncodePlaintext(events, kernel.ScopePersonal, "device-a")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	if err := storeA.Put(ctx, kernel.ScopePersonal, hdr, blob); err != nil {
		t.Fatalf("storeA.Put: %v", err)
	}
	if _, err := pushScope(ctx, dirA, "personal", remoteURL, storeA, nil); err != nil {
		t.Fatalf("pushScope: %v", err)
	}

	// Device B: pull through the post-pull hook (a non-TTY buffer => JSON).
	dirB := t.TempDir()
	writePersonalConfigToPath(t, dirB, remoteURL)
	pullCmd, pullOut, _ := newChangedTestCmd(dirB, "")
	if err := runPullWithDigest(pullCmd, "personal", false); err != nil {
		t.Fatalf("runPullWithDigest: %v", err)
	}
	var res pullJSONResult
	if err := json.Unmarshal(pullOut.Bytes(), &res); err != nil {
		t.Fatalf("pull stdout %q is not parseable JSON: %v", pullOut.String(), err)
	}
	if res.Digest == nil {
		t.Fatalf("pull result carried no digest: %s", pullOut.String())
	}
	if res.Digest.Entities.Added != 2 {
		t.Errorf("digest entities added = %d, want 2 (%s)", res.Digest.Entities.Added, pullOut.String())
	}
	if res.Digest.Relations.Added != 1 {
		t.Errorf("digest relations added = %d, want 1 (%s)", res.Digest.Relations.Added, pullOut.String())
	}

	// The digest must have been cached for `maz changed`.
	if _, err := os.Stat(pullDigestPath(dirB)); err != nil {
		t.Fatalf("pull did not cache the digest: %v", err)
	}

	// `maz changed` re-prints the same grouped counts.
	changedCmd, changedOut, _ := newChangedTestCmd(dirB, "")
	if err := runChanged(changedCmd); err != nil {
		t.Fatalf("runChanged: %v", err)
	}
	var reprinted pullDigest
	if err := json.Unmarshal(changedOut.Bytes(), &reprinted); err != nil {
		t.Fatalf("changed stdout %q is not parseable JSON: %v", changedOut.String(), err)
	}
	if reprinted.Entities.Added != 2 || reprinted.Relations.Added != 1 {
		t.Errorf("maz changed re-print mismatch: %+v", reprinted)
	}

	// The human one-liner reflects the same counts.
	var text bytes.Buffer
	if err := writePullDigestText(&text, &reprinted); err != nil {
		t.Fatalf("writePullDigestText: %v", err)
	}
	if !strings.Contains(text.String(), "+2 entities") || !strings.Contains(text.String(), "+1 relation") {
		t.Errorf("text digest missing expected segments: %q", text.String())
	}

	// A second pull is a no-op: nothing new, so the cached digest is unchanged.
	noopCmd, noopOut, _ := newChangedTestCmd(dirB, "")
	if err := runPullWithDigest(noopCmd, "personal", false); err != nil {
		t.Fatalf("second runPullWithDigest: %v", err)
	}
	var noop pullJSONResult
	if err := json.Unmarshal(noopOut.Bytes(), &noop); err != nil {
		t.Fatalf("noop pull stdout %q is not parseable JSON: %v", noopOut.String(), err)
	}
	if noop.Digest != nil && noop.Digest.hasChanges() {
		t.Errorf("second pull reported changes, want none: %+v", noop.Digest)
	}
}

// newChangedTestCmd builds a lone command carrying the persistent flags the
// pull/changed code reads (data-dir, output, json) plus a context. An empty
// outputFmt leaves format resolution to default (JSON off a non-TTY buffer).
func newChangedTestCmd(dataDir, outputFmt string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.PersistentFlags().String("data-dir", dataDir, "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.PersistentFlags().Bool("json", false, "")
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetContext(context.Background())
	// Parse so the persistent flags merge into cmd.Flags(), which is how
	// output.From reads --output at runtime. An empty outputFmt leaves format
	// resolution to default (JSON off a non-TTY buffer).
	args := []string{}
	if outputFmt != "" {
		args = []string{"--output", outputFmt}
	}
	if err := cmd.ParseFlags(args); err != nil {
		panic("newChangedTestCmd: parse flags: " + err.Error())
	}
	return cmd, &out, &errBuf
}

// entityEvent builds a minimal entity event for the rendered-tree fixtures.
func entityEvent(t *testing.T, uid, displayName string, kind ontology.EntityKind) kernel.Event {
	t.Helper()
	payload := ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             kind,
		DisplayName:      displayName,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Date(2026, 6, 3, 14, 10, 0, 0, time.UTC),
	}
	return mkEvent(t, uid, ontology.KindEntity, payload)
}

// relationEvent builds a minimal relation event for the rendered-tree fixtures.
func relationEvent(t *testing.T, uid, subjectUID, predicate, objectUID string) kernel.Event {
	t.Helper()
	payload := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     subjectUID,
		Predicate:      predicate,
		ObjectUID:      objectUID,
		Confidence:     1.0,
		SourceEventUID: uid,
		CreateTime:     time.Date(2026, 6, 3, 14, 11, 0, 0, time.UTC),
	}
	return mkEvent(t, uid, ontology.KindRelation, payload)
}

func mkEvent(t *testing.T, uid string, kind kernel.EventKind, payload any) kernel.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", kind, err)
	}
	return kernel.Event{
		UID:        uid,
		Name:       "events/" + uid,
		Kind:       kind,
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 3, 14, 10, 0, 0, time.UTC),
		Payload:    raw,
	}
}
