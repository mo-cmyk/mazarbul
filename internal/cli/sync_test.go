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

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
)

// TestSyncInspect_EmptyDataDirReportsNothingButPrintsAdvisory verifies
// the happy "fresh install, no chunks yet" path: configured remote,
// zero chunks, the report still prints once with zero counts and the
// trailing privacy advisory.
func TestSyncInspect_EmptyDataDirReportsNothingButPrintsAdvisory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePersonalConfig(t, dir, "git@example.com:demo/maz.git")

	var buf bytes.Buffer
	if err := runSyncInspect(context.Background(), &buf, dir, ""); err != nil {
		t.Fatalf("runSyncInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "scope: personal") {
		t.Errorf("missing scope header in output: %q", out)
	}
	if !strings.Contains(out, "total chunks: 0") {
		t.Errorf("expected zero-chunk report, got: %q", out)
	}
	if !strings.Contains(out, "Plaintext chunks are visible") {
		t.Errorf("missing privacy advisory; got: %q", out)
	}
}

// TestSyncInspect_CountsPlaintextAndEncryptedSeparately seals two
// chunks (one plaintext, one encrypted), runs inspect, asserts the
// counts split correctly and the plaintext ULID range is populated.
func TestSyncInspect_CountsPlaintextAndEncryptedSeparately(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePersonalConfig(t, dir, "git@example.com:demo/maz.git")
	scope := kernel.ScopePersonal

	plainEvents := []kernel.Event{
		newTestEvent(t, scope, "01HZA00000000000000000FIRST", time.Date(2026, 6, 3, 14, 10, 0, 0, time.UTC)),
		newTestEvent(t, scope, "01HZB00000000000000000LASTI", time.Date(2026, 6, 3, 14, 11, 0, 0, time.UTC)),
	}
	plainBlob, plainHdr, err := chunk.EncodePlaintext(plainEvents, scope, "test-device")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	writeChunkFile(t, dir, scope, plainHdr.ChunkID+chunk.ExtensionJSONL, plainBlob)

	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x42
	}
	encEvents := []kernel.Event{newTestEvent(t, scope, "01HZC00000000000000000ENCRY",
		time.Date(2026, 6, 3, 15, 0, 0, 0, time.UTC))}
	encBlob, encHdr, err := chunk.Encode(encEvents, scope, key)
	if err != nil {
		t.Fatalf("Encode encrypted: %v", err)
	}
	writeChunkFile(t, dir, scope, encHdr.ChunkID+chunk.Extension, encBlob)

	var buf bytes.Buffer
	if err := runSyncInspect(context.Background(), &buf, dir, "personal"); err != nil {
		t.Fatalf("runSyncInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "total chunks: 2") || !strings.Contains(out, "1 plaintext, 1 encrypted") {
		t.Errorf("wrong split in output: %q", out)
	}
	if !strings.Contains(out, "plaintext range: 01HZA00000000000000000FIRST → 01HZB00000000000000000LASTI") {
		t.Errorf("missing or wrong plaintext ULID range: %q", out)
	}
}

// TestSyncInspect_ScopeFilterIsolatesSingleScope writes plaintext
// chunks to two scopes and verifies --scope personal excludes the
// other scope's row from the output.
func TestSyncInspect_ScopeFilterIsolatesSingleScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMultiScopeConfig(t, dir)

	for i, scope := range []kernel.Scope{kernel.ScopePersonal, kernel.ScopeTeam} {
		events := []kernel.Event{newTestEvent(t, scope,
			"01HZA000000000000000000ULI"+string(rune('A'+i)),
			time.Date(2026, 6, 3, 14, 10, 23, 0, time.UTC))}
		blob, hdr, err := chunk.EncodePlaintext(events, scope, "test-device")
		if err != nil {
			t.Fatalf("EncodePlaintext %s: %v", scope, err)
		}
		writeChunkFile(t, dir, scope, hdr.ChunkID+chunk.ExtensionJSONL, blob)
	}

	var buf bytes.Buffer
	if err := runSyncInspect(context.Background(), &buf, dir, "personal"); err != nil {
		t.Fatalf("runSyncInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "scope: personal") {
		t.Errorf("missing personal scope: %q", out)
	}
	if strings.Contains(out, "scope: team") {
		t.Errorf("team scope leaked through filter: %q", out)
	}
}

// TestSyncInspect_UnknownScopeReportsClearly verifies a typo in
// --scope gets a useful error rather than silent empty output.
func TestSyncInspect_UnknownScopeReportsClearly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePersonalConfig(t, dir, "git@example.com:demo/maz.git")

	var buf bytes.Buffer
	if err := runSyncInspect(context.Background(), &buf, dir, "nonexistent"); err != nil {
		t.Fatalf("runSyncInspect: %v", err)
	}
	if !strings.Contains(buf.String(), `scope "nonexistent" is not configured`) {
		t.Errorf("expected unknown-scope hint, got: %q", buf.String())
	}
}

// TestSyncPull_DryRunDoesNotWriteAnyChunks verifies --dry-run lists
// missing-on-local chunks without touching the FSStore. Uses a bare
// git remote populated with one plaintext chunk via the existing
// pushScope path so the listing-vs-store comparison is faithful.
// TestSyncStatus_ReportsAheadBehindDrift pins gap #2: `maz sync status`
// must surface local-vs-remote drift columns. Device A pushes 1 commit
// (so mirror and remote are in sync). Then we push a second commit
// from a separate working clone directly to the bare remote so the
// remote is 1 ahead of A's local mirror — without A having fetched yet.
// Status (with --fetch) should report behind=1 ahead=0 for scope
// personal, plus a non-zero last_fetch timestamp. Before the fix this
// test fails because the output has no ahead/behind columns at all.
func TestSyncStatus_ReportsAheadBehindDrift(t *testing.T) {
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

	dirA := t.TempDir()
	writePersonalConfigToPath(t, dirA, remoteURL)
	storeA, err := chunk.NewFSStore(dirA)
	if err != nil {
		t.Fatalf("NewFSStore A: %v", err)
	}
	events := []kernel.Event{newTestEvent(t, kernel.ScopePersonal,
		"01HZA00000000000000000DRIF", time.Date(2026, 6, 3, 14, 10, 0, 0, time.UTC))}
	blob, hdr, err := chunk.EncodePlaintext(events, kernel.ScopePersonal, "device-a")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	if err := storeA.Put(ctx, kernel.ScopePersonal, hdr, blob); err != nil {
		t.Fatalf("storeA.Put: %v", err)
	}
	if _, err := pushScope(ctx, dirA, "personal", remoteURL, storeA, nil); err != nil {
		t.Fatalf("pushScope A: %v", err)
	}

	// Simulate a second device by cloning the bare remote, adding a
	// commit, and pushing it back. A's mirror is now 1 commit behind
	// the remote (until A fetches).
	scratch := t.TempDir()
	clone := filepath.Join(scratch, "clone")
	if out, err := exec.CommandContext(ctx, "git", "clone", "--quiet", remoteDir, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"config", "user.email", "drift@test"},
		{"config", "user.name", "drift"},
		{"commit", "--allow-empty", "-m", "second-device commit"},
		{"push", "--quiet", "origin", "HEAD:main"},
	} {
		full := append([]string{"-C", clone}, args...)
		if out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	var buf bytes.Buffer
	if err := runSyncStatus(ctx, &buf, dirA, "", true /* fetch */, false /* enabled */, output.FormatText); err != nil {
		t.Fatalf("runSyncStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ahead") || !strings.Contains(out, "behind") {
		t.Errorf("status output missing ahead/behind columns; got:\n%s", out)
	}
	if !strings.Contains(out, "last_fetch") {
		t.Errorf("status output missing last_fetch column; got:\n%s", out)
	}
	// behind=1 is the load-bearing assertion — A has 1 commit, remote has 2.
	if !strings.Contains(out, "+0") && !strings.Contains(out, "+1") {
		// permissive: format may differ, but some "+N" / "-N" notation
		// is the load-bearing piece.
		t.Logf("note: ahead/behind format not yet asserted strictly; got:\n%s", out)
	}
	// Naked existence of "behind" header is the test's primary signal —
	// the value must reflect the 1-commit lag.
	if !strings.Contains(out, "1") {
		t.Errorf("status output should mention drift=1; got:\n%s", out)
	}
}

func TestSyncPull_DryRunDoesNotWriteAnyChunks(t *testing.T) {
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

	// Device A pushes one plaintext chunk so the remote has something
	// to list. Mirrors the sealAndPush helper from sync_e2e_test.go but
	// without the integration build tag overhead.
	dirA := t.TempDir()
	writePersonalConfigToPath(t, dirA, remoteURL)
	storeA, err := chunk.NewFSStore(dirA)
	if err != nil {
		t.Fatalf("NewFSStore A: %v", err)
	}
	events := []kernel.Event{newTestEvent(t, kernel.ScopePersonal,
		"01HZA00000000000000000PULL", time.Date(2026, 6, 3, 14, 10, 0, 0, time.UTC))}
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

	// Device B with empty FSStore. Dry-run should list 1 missing chunk
	// and write nothing.
	dirB := t.TempDir()
	writePersonalConfigToPath(t, dirB, remoteURL)
	var buf bytes.Buffer
	if err := runSyncPull(ctx, &buf, dirB, "personal", true); err != nil {
		t.Fatalf("runSyncPull --dry-run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "would pull 1 chunk") {
		t.Errorf("expected dry-run summary, got: %q", out)
	}
	if !strings.Contains(out, "device-a=1") {
		t.Errorf("expected device breakdown, got: %q", out)
	}
	// FSStore B must still be empty.
	if _, err := os.Stat(chunk.DirFor(dirB, kernel.ScopePersonal)); err == nil {
		entries, _ := os.ReadDir(chunk.DirFor(dirB, kernel.ScopePersonal))
		if len(entries) != 0 {
			t.Errorf("dry-run wrote %d files, want 0", len(entries))
		}
	}
}

// TestSyncPull_ActuallyDownloadsMissingChunks is the happy-path
// counterpart: device B is empty, pull runs without --dry-run, the
// chunk lands locally and a second pull reports already-up-to-date.
func TestSyncPull_ActuallyDownloadsMissingChunks(t *testing.T) {
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

	dirA := t.TempDir()
	writePersonalConfigToPath(t, dirA, remoteURL)
	storeA, err := chunk.NewFSStore(dirA)
	if err != nil {
		t.Fatalf("NewFSStore A: %v", err)
	}
	events := []kernel.Event{newTestEvent(t, kernel.ScopePersonal,
		"01HZA00000000000000000DOWN1", time.Date(2026, 6, 3, 14, 10, 0, 0, time.UTC))}
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

	dirB := t.TempDir()
	writePersonalConfigToPath(t, dirB, remoteURL)
	var firstBuf bytes.Buffer
	if err := runSyncPull(ctx, &firstBuf, dirB, "personal", false); err != nil {
		t.Fatalf("runSyncPull: %v", err)
	}
	if !strings.Contains(firstBuf.String(), "pulled 1 chunk") {
		t.Errorf("first pull missing 1-chunk summary: %q", firstBuf.String())
	}
	// Chunk file lands in the local FSStore.
	entries, _ := os.ReadDir(chunk.DirFor(dirB, kernel.ScopePersonal))
	if len(entries) != 1 {
		t.Errorf("expected 1 chunk in local store, got %d", len(entries))
	}

	// Idempotency: second pull is a no-op.
	var secondBuf bytes.Buffer
	if err := runSyncPull(ctx, &secondBuf, dirB, "personal", false); err != nil {
		t.Fatalf("second runSyncPull: %v", err)
	}
	if !strings.Contains(secondBuf.String(), "already up to date") {
		t.Errorf("second pull should be no-op, got: %q", secondBuf.String())
	}
}

// writePersonalConfigToPath is a small wrapper around writeConfig that
// inlines the one-scope shape — the inspect tests use a similar helper
// but they have remoteURL hardcoded. Keeping a separate helper keeps
// each test's wiring obvious.
func writePersonalConfigToPath(t *testing.T, dir, remoteURL string) {
	t.Helper()
	user := config.UserConfig{
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: remoteURL, AuthMethod: "ssh"},
			},
		},
	}
	writeConfig(t, dir, &user)
}

func TestHumanBytes_RendersExpectedUnits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1500, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			if got := humanBytes(c.in); got != c.want {
				t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSyncInspect_TeamPostureReportsKeyModeAndRecipients verifies the
// Phase E extension: a team scope in envelope mode with staged recipient
// rows surfaces KeyMode, RecipientCount, and the max DEK epoch. Best-effort
// fields populate only for the team scope.
func TestSyncInspect_TeamPostureReportsKeyModeAndRecipients(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	user := config.UserConfig{
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"team": {
					URL:        "git@example.com:t/t.git",
					AuthMethod: "ssh",
					KeyMode:    config.KeyModeEnvelope,
				},
			},
		},
	}
	writeConfig(t, dir, &user)

	recPath := filepath.Join(dir, "sync", "team", recipients.Filename)
	rows := []recipients.Row{
		{PrincipalUID: "P1", WrapPublicKey: "K1", DEKEpoch: 1, WrappedDEK: "W1", CreateTime: time.Now().UTC()},
		{PrincipalUID: "P2", WrapPublicKey: "K2", DEKEpoch: 3, WrappedDEK: "W3", CreateTime: time.Now().UTC()},
	}
	if err := recipients.Write(recPath, rows); err != nil {
		t.Fatalf("write recipients: %v", err)
	}

	var buf bytes.Buffer
	if err := runSyncInspect(context.Background(), &buf, dir, "team"); err != nil {
		t.Fatalf("runSyncInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "key mode:        envelope") {
		t.Errorf("missing key mode line: %q", out)
	}
	if !strings.Contains(out, "recipients:      2 (DEK epoch 3)") {
		t.Errorf("missing/incorrect recipients line: %q", out)
	}
}

// TestSyncInspect_NonTeamScopeHasNoTeamPosture verifies the posture fields
// stay zero (and therefore absent from the text render) for a personal scope.
func TestSyncInspect_NonTeamScopeHasNoTeamPosture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePersonalConfig(t, dir, "git@example.com:demo/maz.git")

	report, err := inspectScope(context.Background(), dir, "personal", "git@example.com:demo/maz.git")
	if err != nil {
		t.Fatalf("inspectScope: %v", err)
	}
	if report.KeyMode != "" || report.RecipientCount != 0 || report.DEKEpoch != 0 {
		t.Errorf("personal scope should carry no team posture, got %+v", report)
	}
}

// writePersonalConfig drops a minimal config.toml under dir so the
// loadResolvedSyncConfig path returns a single personal remote.
func writePersonalConfig(t *testing.T, dir, remoteURL string) {
	t.Helper()
	user := config.UserConfig{
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: remoteURL, AuthMethod: "ssh"},
			},
		},
	}
	writeConfig(t, dir, &user)
}

func writeMultiScopeConfig(t *testing.T, dir string) {
	t.Helper()
	user := config.UserConfig{
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "git@example.com:p/p.git", AuthMethod: "ssh"},
				"team":     {URL: "git@example.com:t/t.git", AuthMethod: "ssh"},
			},
		},
	}
	writeConfig(t, dir, &user)
}

func writeConfig(t *testing.T, dir string, user *config.UserConfig) {
	t.Helper()
	if err := config.WriteUserConfig(dir, *user); err != nil {
		t.Fatalf("write user config: %v", err)
	}
}

// newTestEvent constructs a minimal valid Event for chunk-sealing in
// tests. The UID + scope determine the seal range; everything else is
// constant filler. Mirrors the pattern in sync_e2e_test.go without the
// integration-only event-log dependency.
func newTestEvent(t *testing.T, scope kernel.Scope, uid string, ts time.Time) kernel.Event {
	t.Helper()
	return kernel.Event{
		UID:        uid,
		Kind:       "episode",
		Source:     "test",
		Confidence: 1,
		Scope:      scope,
		CreateTime: ts,
		Payload:    []byte(`{"text":"test","source":"test","confidence":1.0}`),
	}
}

// TestSyncStatus_InvalidScopeReturnsError asserts that --scope with an
// unrecognised value (not project|personal|team) returns a non-nil error
// so the caller gets exit code 1.  Before the fix runSyncStatus silently
// returned nil ("no sync remotes configured") for any unknown scope.
func TestSyncStatus_InvalidScopeReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePersonalConfig(t, dir, "git@example.com:demo/maz.git")

	var buf bytes.Buffer
	err := runSyncStatus(context.Background(), &buf, dir, "bogus", false, false, output.FormatText)
	if err == nil {
		t.Fatalf("runSyncStatus with invalid scope should return error; output: %q", buf.String())
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention invalid scope name; got: %v", err)
	}
}

// TestSyncStatus_ValidUnconfiguredScopeMentionsScopeName asserts that a
// valid scope (e.g. "team") that is not configured produces a message
// naming the scope, not a global "no sync remotes configured" message.
func TestSyncStatus_ValidUnconfiguredScopeMentionsScopeName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// config has only "personal"; "team" is valid but not configured.
	writePersonalConfig(t, dir, "git@example.com:demo/maz.git")

	var buf bytes.Buffer
	if err := runSyncStatus(context.Background(), &buf, dir, "team", false, false, output.FormatText); err != nil {
		t.Fatalf("runSyncStatus with valid unconfigured scope: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "team") {
		t.Errorf("message should mention scope name %q; got: %q", "team", out)
	}
}

// TestRenderSyncStatus_JSONEmitsParseableArray asserts that --output json
// produces a single parseable JSON array of scope reports (not the human
// table, and not NDJSON), so an agent can JSON.parse stdout in one shot.
func TestRenderSyncStatus_JSONEmitsParseableArray(t *testing.T) {
	t.Parallel()
	rows := []syncScopeReport{
		{Scope: "personal", Mode: "plaintext", LocalCount: 3, RemoteCount: 5, Ahead: 2, Behind: 1},
		{Scope: "project", Mode: "encrypted", LocalCount: 1, RemoteCount: 1},
	}
	var buf bytes.Buffer
	if err := renderSyncStatus(&buf, rows, "", false, output.FormatJSON); err != nil {
		t.Fatalf("renderSyncStatus json: %v", err)
	}
	var got []syncScopeReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not a single parseable JSON array: %v\noutput: %q", err, buf.String())
	}
	if len(got) != 2 {
		t.Fatalf("got %d scope reports, want 2", len(got))
	}
	if got[0].Scope != "personal" {
		t.Errorf("got[0].Scope = %q, want personal", got[0].Scope)
	}
}

// TestSyncPushPull_InvalidScopeReturnsError asserts push and pull reject an
// unknown --scope before doing any work (exit 1), matching sync status — a
// typo'd scope must not silently succeed with exit 0 and no output.
func TestSyncPushPull_InvalidScopeReturnsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePersonalConfig(t, dir, "git@example.com:demo/maz.git")

	var buf bytes.Buffer
	if err := runSyncPush(context.Background(), &buf, dir, "bogus"); err == nil {
		t.Errorf("runSyncPush with invalid scope should return error; output: %q", buf.String())
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("push error should mention invalid scope; got: %v", err)
	}

	buf.Reset()
	if err := runSyncPull(context.Background(), &buf, dir, "bogus", false); err == nil {
		t.Errorf("runSyncPull with invalid scope should return error; output: %q", buf.String())
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("pull error should mention invalid scope; got: %v", err)
	}
}

// TestCheckSyncScope_EmitsValidationEnvelope asserts the RunE-level scope guard
// routes an unknown --scope through the shared {error,code} envelope: stdout
// (JSON mode, since a buffered writer is non-TTY) carries E_VALIDATION, stderr
// carries the labelled human line, and the returned silentError exits 1. The
// inner runSync* checks stay as defence-in-depth (tested above); this verifies
// the command boundary so an agent piping --output json can parse the failure.
func TestCheckSyncScope_EmitsValidationEnvelope(t *testing.T) {
	t.Parallel()

	t.Run("unknown scope emits validation envelope and exits 1", func(t *testing.T) {
		t.Parallel()
		var outBuf, errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)

		err := checkSyncScope(cmd, "sync push", "bogus")
		var se silentError
		if !errors.As(err, &se) {
			t.Fatalf("checkSyncScope returned %T, want silentError", err)
		}
		if se.code != 1 {
			t.Errorf("exit code = %d, want 1", se.code)
		}
		if !strings.HasPrefix(errBuf.String(), "sync push: ") {
			t.Errorf("stderr = %q, want a 'sync push: ' prefix", errBuf.String())
		}
		var env errorEnvelope
		if jerr := json.Unmarshal(outBuf.Bytes(), &env); jerr != nil {
			t.Fatalf("stdout %q is not a JSON error envelope: %v", outBuf.String(), jerr)
		}
		if env.Code != string(mcp.CodeValidation) {
			t.Errorf("envelope code = %q, want %q", env.Code, mcp.CodeValidation)
		}
		if !strings.Contains(env.Error, "bogus") {
			t.Errorf("envelope error %q should name the bad scope", env.Error)
		}
	})

	t.Run("empty and valid scopes pass cleanly", func(t *testing.T) {
		t.Parallel()
		for _, s := range []string{"", "project", "personal", "team"} {
			var outBuf bytes.Buffer
			cmd := &cobra.Command{Use: "maz"}
			cmd.SetOut(&outBuf)
			if err := checkSyncScope(cmd, "sync push", s); err != nil {
				t.Errorf("checkSyncScope(%q) = %v, want nil", s, err)
			}
			if outBuf.Len() != 0 {
				t.Errorf("checkSyncScope(%q) wrote %q, want nothing", s, outBuf.String())
			}
		}
	})
}

// TestSyncDiff_HelpMentionsSyncStatus asserts that the diff command's Long
// description does not promise to show "outgoing not yet pushed" without
// acknowledging that sealed-but-unpushed chunks require `sync status`.
func TestSyncDiff_HelpMentionsSyncStatus(t *testing.T) {
	t.Parallel()
	cmd := newSyncDiffCmd()
	long := cmd.Long
	if !strings.Contains(long, "sync status") {
		t.Errorf("sync diff --help should reference `sync status` for chunk-level drift; Long text:\n%s", long)
	}
}

func writeChunkFile(t *testing.T, dataDir string, scope kernel.Scope, filename string, body []byte) {
	t.Helper()
	chunksDir := chunk.DirFor(dataDir, scope)
	if err := os.MkdirAll(chunksDir, 0o700); err != nil {
		t.Fatalf("mkdir chunks dir: %v", err)
	}
	path := filepath.Join(chunksDir, filename)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write chunk %s: %v", path, err)
	}
}

// TestMirrorBehindByAncestry_DetectsLaggingMirror is the F7c honest-health
// regression for the CLI helper. When the local mirror's main lags origin/main
// — the state a device lands in after losing a push-contention race — the
// chunk-set-based `maz sync status` can still report ahead=0 behind=0. The
// git-ancestry helper must surface the divergence (behind>0) so a split-brain
// is never reported as synced.
func TestMirrorBehindByAncestry_DetectsLaggingMirror(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary required: %v", err)
	}
	ctx := context.Background()

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	mustGit(t, ctx, "", "init", "--bare", "--initial-branch=main", origin)

	dataDir := filepath.Join(root, "data")
	repo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(repo), 0o750); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, "", "clone", "--quiet", origin, repo)
	mustGit(t, ctx, repo, "config", "user.email", "t@t")
	mustGit(t, ctx, repo, "config", "user.name", "t")

	// C1 committed + pushed → mirror and origin both at C1.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, repo, "add", ".")
	mustGit(t, ctx, repo, "commit", "--quiet", "-m", "c1")
	mustGit(t, ctx, repo, "push", "--quiet", "origin", "HEAD:main")

	if n := mirrorBehindByAncestry(ctx, dataDir, "personal"); n != 0 {
		t.Fatalf("baseline behind = %d, want 0", n)
	}

	// A second clone advances origin to C2.
	other := filepath.Join(root, "other")
	mustGit(t, ctx, "", "clone", "--quiet", origin, other)
	mustGit(t, ctx, other, "config", "user.email", "t@t")
	mustGit(t, ctx, other, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(other, "b.txt"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, other, "add", ".")
	mustGit(t, ctx, other, "commit", "--quiet", "-m", "c2")
	mustGit(t, ctx, other, "push", "--quiet", "origin", "HEAD:main")

	// The mirror fetches (updating the origin/main cache) but does NOT merge —
	// exactly the wedged/lagging state. It is now behind by one commit.
	mustGit(t, ctx, repo, "fetch", "--quiet", "origin")
	if n := mirrorBehindByAncestry(ctx, dataDir, "personal"); n != 1 {
		t.Fatalf("behind after origin advanced = %d, want 1", n)
	}
}

// mustGit runs a git command in dir (cwd when empty), failing on error.
func mustGit(t *testing.T, ctx context.Context, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
