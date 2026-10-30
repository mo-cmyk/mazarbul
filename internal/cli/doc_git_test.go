package cli

// doc_git_test.go — tests for the git-tree document surface:
// resolveDocumentTarget, runDiffDocument, runShowDocument, runBlameDocument,
// and the github --document flag.
//
// HERMETICITY: every test that shells out to git MUST ensure GIT_DIR,
// GIT_INDEX_FILE, and GIT_WORK_TREE are absent from the environment,
// because a pre-commit hook exports those variables to its subprocesses.
// Without scrubbing them, a `go test` run inside a hook would redirect every
// git call to the developer's real working repository and corrupt it — the
// exact incident fixed in PR #146. We enforce this by setting the env on
// every cmd before Run(), mirroring cleanGitEnv().

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---- hermetic git helper ----

// hermeticGit runs a git command in dir with GIT_DIR/GIT_INDEX_FILE/
// GIT_WORK_TREE scrubbed from the environment so the test cannot accidentally
// corrupt the developer's real repository (PR #146 regression guard).
func hermeticGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	cmd.Env = cleanGitEnv(dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// buildDocumentRepo creates a minimal git mirror with rendered/entities.jsonl
// and rendered/documents/<slug>.md populated across two commits. It returns
// the repo dir and the frozen slug used for the body file.
//
// Commit structure:
//
//	initial: entities.jsonl with one document (uid ALPHA) + body file v1
//	second:  same document, body file v2 (one line changed)
func buildDocumentRepo(t *testing.T) (repoDir, slug, uid string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary required: %v", err)
	}

	repoDir = t.TempDir()
	uid = "01HZABCDEFGHIJKLMNOPQQGAEN" // 26-char synthetic ULID; last 6 = "QQGAEN"
	slug = "alpha-doc__QQGAEN"         // slugSuffixOf(uid) = uid[20:] = "QQGAEN"

	ctx := context.Background()
	if out, err := exec.CommandContext(ctx, "git", "init", "--quiet", "--initial-branch=main", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	hermeticGit(t, repoDir, "config", "user.email", "test@mazarbul")
	hermeticGit(t, repoDir, "config", "user.name", "test")

	// Build rendered/entities.jsonl with one document entity.
	entitiesContent := `{"uid":"01HZABCDEFGHIJKLMNOPQQGAEN","kind":"document","displayName":"Alpha Doc","aliases":[],"confidence":1,"resolutionStatus":"resolved","createTime":"2026-06-01T00:00:00Z","lastUpdated":"2026-06-01T00:00:00Z","mentionCount":0,"orphaned":false}`
	writeRepoFile(t, repoDir, "rendered/entities.jsonl", entitiesContent)

	bodyV1 := "# Alpha Doc\n\nThis is the first version.\n"
	writeRepoFile(t, repoDir, "rendered/documents/"+slug+".md", bodyV1)

	hermeticGit(t, repoDir, "add", "rendered/entities.jsonl", "rendered/documents/"+slug+".md")
	hermeticGit(t, repoDir, "commit", "-m", "initial: add alpha doc")

	// Second commit: update body.
	bodyV2 := "# Alpha Doc\n\nThis is the second version with more content.\n"
	writeRepoFile(t, repoDir, "rendered/documents/"+slug+".md", bodyV2)
	hermeticGit(t, repoDir, "add", "rendered/documents/"+slug+".md")
	hermeticGit(t, repoDir, "commit", "-m", "update: alpha doc revised")

	return repoDir, slug, uid
}

// writeRepoFile creates a file (and any parent directories) inside repoDir.
func writeRepoFile(t *testing.T, repoDir, relPath, content string) {
	t.Helper()
	full := filepath.Join(repoDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// ---- slug resolution ----

func TestSlugifyDocBase_MirrorsRendererRules(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 80) // 80 runes -> truncated to 60
	cases := []struct {
		name   string
		needle string
		want   string
	}{
		{"simple basename", "foo.md", "foo"},
		{"path with dir", "notes/foo.md", "foo"},
		{"spaces become hyphens", "Project Plan.md", "project-plan"},
		{"markdown extension stripped", "Project Plan.markdown", "project-plan"},
		{"punctuation collapses", "a---b__c.md", "a-b-c"},
		{"non-ascii sanitizes to unnamed", "🔥🔥.md", "unnamed"},
		{"empty basename no match", "/", ""},
		{"dot basename no match", ".", ""},
		{"over 60 runes truncated", long + ".md", strings.Repeat("a", 60)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := slugifyDocBase(tc.needle)
			if got != tc.want {
				t.Fatalf("slugifyDocBase(%q) = %q, want %q", tc.needle, got, tc.want)
			}
		})
	}
}

func TestSlugSuffixOf_ReturnsLast6Chars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		uid  string
		want string
	}{
		{"01HZABCDEFGHIJKLMNOPQQGAEN", "QQGAEN"},
		{"ABCDEF", "ABCDEF"}, // exactly 6
		{"ABC", "ABC"},       // shorter than 6
		{"", ""},             // empty
	}
	for _, tc := range cases {
		t.Run(tc.uid, func(t *testing.T) {
			t.Parallel()
			got := slugSuffixOf(tc.uid)
			if got != tc.want {
				t.Fatalf("slugSuffixOf(%q) = %q, want %q", tc.uid, got, tc.want)
			}
		})
	}
}

func TestDocNeedleMatches_VariousInputs(t *testing.T) {
	t.Parallel()
	uid := "01HZABCDEFGHIJKLMNOPQQGAEN"
	dn := "Alpha Doc"
	slug := "alpha-doc__qqgaen" // rendered filename without .md (lower-cased)
	cases := []struct {
		name   string
		needle string
		want   bool
	}{
		{"uid prefix matches", "01hzab", true},
		{"display name fragment", "alpha", true},
		{"display name full lowercase", "alpha doc", true},
		{"exact frozen slug", "alpha-doc__qqgaen", true},
		{"path target with dir and ext", "notes/alpha-doc.md", true},
		{"path target basename only", "alpha-doc.md", true},
		{"path target base no ext", "alpha-doc", true},
		{"path target with spaces normalizes", "notes/alpha doc.md", true},
		{"path target markdown extension", "alpha-doc.markdown", true},
		{"mistyped slug base rejected", "wrong-title__qqgaen", false},
		{"unrelated needle", "beta", false},
		{"empty needle matches nothing useful", "", true}, // empty matches uid prefix ""
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := docNeedleMatches(tc.needle, uid, dn, slug)
			if got != tc.want {
				t.Fatalf("docNeedleMatches(%q, %q, %q, %q) = %v, want %v",
					tc.needle, uid, dn, slug, got, tc.want)
			}
		})
	}
}

// ---- resolveDocumentTarget (requires git) ----

func TestResolveDocumentTarget_SlugResolvesCorrectly(t *testing.T) {
	t.Parallel()
	repoDir, slug, uid := buildDocumentRepo(t)
	ctx := context.Background()

	res, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:"+slug)
	if err != nil {
		t.Fatalf("resolveDocumentTarget by slug: %v", err)
	}
	if res.UID != uid {
		t.Errorf("UID = %q, want %q", res.UID, uid)
	}
	if res.BodyPath != "rendered/documents/"+slug+".md" {
		t.Errorf("BodyPath = %q, want %q", res.BodyPath, "rendered/documents/"+slug+".md")
	}
}

func TestResolveDocumentTarget_DisplayNameFragmentResolves(t *testing.T) {
	t.Parallel()
	repoDir, slug, uid := buildDocumentRepo(t)
	ctx := context.Background()

	res, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:alpha")
	if err != nil {
		t.Fatalf("resolveDocumentTarget by name fragment: %v", err)
	}
	if res.UID != uid {
		t.Errorf("UID = %q, want %q", res.UID, uid)
	}
	if res.BodyPath != "rendered/documents/"+slug+".md" {
		t.Errorf("BodyPath = %q, want %q", res.BodyPath, "rendered/documents/"+slug+".md")
	}
}

func TestResolveDocumentTarget_UIDPrefixResolves(t *testing.T) {
	t.Parallel()
	repoDir, slug, uid := buildDocumentRepo(t)
	ctx := context.Background()

	res, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:01HZAB")
	if err != nil {
		t.Fatalf("resolveDocumentTarget by uid prefix: %v", err)
	}
	if res.UID != uid {
		t.Errorf("UID = %q, want %q", res.UID, uid)
	}
	_ = slug
}

func TestResolveDocumentTarget_UnknownReturnsError(t *testing.T) {
	t.Parallel()
	repoDir, _, _ := buildDocumentRepo(t)
	ctx := context.Background()

	_, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:nonexistent-xyz")
	if err == nil {
		t.Fatal("expected error for unknown document, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestResolveDocumentTarget_AmbiguousReturnsError(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary required: %v", err)
	}

	repoDir := t.TempDir()
	ctx := context.Background()

	if out, err := exec.CommandContext(ctx, "git", "init", "--quiet", "--initial-branch=main", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	hermeticGit(t, repoDir, "config", "user.email", "test@mazarbul")
	hermeticGit(t, repoDir, "config", "user.name", "test")

	// Two documents both matching needle "alpha".
	uid1 := "01HZABCDEFGHIJKLMNOPQQGA01"
	uid2 := "01HZABCDEFGHIJKLMNOPQQGA02"
	entities := `{"uid":"01HZABCDEFGHIJKLMNOPQQGA01","kind":"document","displayName":"Alpha One","aliases":[],"confidence":1,"resolutionStatus":"resolved","createTime":"2026-06-01T00:00:00Z","lastUpdated":"2026-06-01T00:00:00Z","mentionCount":0,"orphaned":false}
{"uid":"01HZABCDEFGHIJKLMNOPQQGA02","kind":"document","displayName":"Alpha Two","aliases":[],"confidence":1,"resolutionStatus":"resolved","createTime":"2026-06-01T00:00:00Z","lastUpdated":"2026-06-01T00:00:00Z","mentionCount":0,"orphaned":false}`

	writeRepoFile(t, repoDir, "rendered/entities.jsonl", entities)
	writeRepoFile(t, repoDir, "rendered/documents/alpha-one__QQGA01.md", "# Alpha One\n")
	writeRepoFile(t, repoDir, "rendered/documents/alpha-two__QQGA02.md", "# Alpha Two\n")

	hermeticGit(t, repoDir, "add", "rendered/entities.jsonl",
		"rendered/documents/alpha-one__QQGA01.md",
		"rendered/documents/alpha-two__QQGA02.md")
	hermeticGit(t, repoDir, "commit", "-m", "two alpha docs")

	_ = uid1
	_ = uid2

	_, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:alpha")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "2 matches") {
		t.Errorf("expected candidate count in error, got: %v", err)
	}
}

func TestResolveDocumentTarget_PathTargetResolvesByBasename(t *testing.T) {
	t.Parallel()
	repoDir, slug, uid := buildDocumentRepo(t)
	ctx := context.Background()

	// The frozen slug base is "alpha-doc"; a vault-path target reduces to its
	// basename and must resolve the same document.
	res, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:notes/alpha-doc.md")
	if err != nil {
		t.Fatalf("resolveDocumentTarget by path: %v", err)
	}
	if res.UID != uid {
		t.Errorf("UID = %q, want %q", res.UID, uid)
	}
	if res.BodyPath != "rendered/documents/"+slug+".md" {
		t.Errorf("BodyPath = %q, want %q", res.BodyPath, "rendered/documents/"+slug+".md")
	}
}

func TestResolveDocumentTarget_PathWithSpacesResolves(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary required: %v", err)
	}
	repoDir := t.TempDir()
	ctx := context.Background()

	// A document whose vault path is "notes/Project Plan.md" renders to the
	// slugified filename "project-plan__QQGAEN.md".
	uid := "01HZABCDEFGHIJKLMNOPQQGAEN"
	slug := "project-plan__QQGAEN"
	if out, err := exec.CommandContext(ctx, "git", "init", "--quiet", "--initial-branch=main", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	hermeticGit(t, repoDir, "config", "user.email", "test@mazarbul")
	hermeticGit(t, repoDir, "config", "user.name", "test")
	entities := `{"uid":"01HZABCDEFGHIJKLMNOPQQGAEN","kind":"document","displayName":"Project Plan","aliases":[],"confidence":1,"resolutionStatus":"resolved","createTime":"2026-06-01T00:00:00Z","lastUpdated":"2026-06-01T00:00:00Z","mentionCount":0,"orphaned":false}`
	writeRepoFile(t, repoDir, "rendered/entities.jsonl", entities)
	writeRepoFile(t, repoDir, "rendered/documents/"+slug+".md", "# Project Plan\n")
	hermeticGit(t, repoDir, "add", "rendered/entities.jsonl", "rendered/documents/"+slug+".md")
	hermeticGit(t, repoDir, "commit", "-m", "add project plan")

	// The spaced vault path must resolve via renderer-aligned slugification.
	res, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:notes/Project Plan.md")
	if err != nil {
		t.Fatalf("resolveDocumentTarget spaced path: %v", err)
	}
	if res.UID != uid {
		t.Errorf("UID = %q, want %q", res.UID, uid)
	}
	if res.BodyPath != "rendered/documents/"+slug+".md" {
		t.Errorf("BodyPath = %q, want %q", res.BodyPath, "rendered/documents/"+slug+".md")
	}
}

func TestResolveDocumentTarget_MistypedSlugIsNotFound(t *testing.T) {
	t.Parallel()
	repoDir, _, _ := buildDocumentRepo(t)
	ctx := context.Background()

	// Correct UID suffix but wrong slug base must NOT resolve the document.
	_, err := resolveDocumentTarget(ctx, repoDir, "HEAD", "document:wrong-title__QQGAEN")
	if err == nil {
		t.Fatal("expected not-found for mistyped slug base, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

// ---- runDiffDocument ----

func TestRunDiffDocument_ShowsBodyDiff(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	// Symlink the repo into the expected location.
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &diffDocumentFlags{format: "text"}
	if err := runDiffDocument(ctx, &buf, dataDir, "document:"+slug, "HEAD~1..HEAD", flags); err != nil {
		t.Fatalf("runDiffDocument: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "second version") && !strings.Contains(out, "+") {
		t.Errorf("expected diff output with changed lines, got:\n%s", out)
	}
	// Must contain the file path labels.
	if !strings.Contains(out, slug) {
		t.Errorf("expected slug %q in diff output, got:\n%s", slug, out)
	}
}

func TestRunDiffDocument_JSONOutputIsParseable(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &diffDocumentFlags{format: "json"}
	if err := runDiffDocument(ctx, &buf, dataDir, "document:"+slug, "HEAD~1..HEAD", flags); err != nil {
		t.Fatalf("runDiffDocument json: %v", err)
	}

	var result diffDocumentJSONResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput: %s", err, buf.String())
	}
	if result.UID == "" {
		t.Errorf("JSON result missing uid; got: %+v", result)
	}
	if result.BodyPath == "" {
		t.Errorf("JSON result missing bodyPath; got: %+v", result)
	}
}

func TestRunDiffDocument_NoDiff_ReportsNoChanges(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &diffDocumentFlags{format: "text"}
	// Diff HEAD against itself — no changes.
	if err := runDiffDocument(ctx, &buf, dataDir, "document:"+slug, "HEAD..HEAD", flags); err != nil {
		t.Fatalf("runDiffDocument same-rev: %v", err)
	}
	if !strings.Contains(buf.String(), "no changes") {
		t.Errorf("expected 'no changes' for identical revs, got:\n%s", buf.String())
	}
}

// ---- runShowDocument ----

func TestRunShowDocument_PrintsBodyAtRevision(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &showDocumentFlags{format: "text", rev: "HEAD"}
	if err := runShowDocument(ctx, &buf, dataDir, "document:"+slug, flags); err != nil {
		t.Fatalf("runShowDocument: %v", err)
	}
	// HEAD has bodyV2 content.
	if !strings.Contains(buf.String(), "second version") {
		t.Errorf("expected v2 body at HEAD, got:\n%s", buf.String())
	}
}

func TestRunShowDocument_AtOlderRevision(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &showDocumentFlags{format: "text", rev: "HEAD~1"}
	if err := runShowDocument(ctx, &buf, dataDir, "document:"+slug, flags); err != nil {
		t.Fatalf("runShowDocument HEAD~1: %v", err)
	}
	// HEAD~1 has bodyV1 content.
	if !strings.Contains(buf.String(), "first version") {
		t.Errorf("expected v1 body at HEAD~1, got:\n%s", buf.String())
	}
}

func TestRunShowDocument_JSONOutputContainsBody(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &showDocumentFlags{format: "json", rev: "HEAD"}
	if err := runShowDocument(ctx, &buf, dataDir, "document:"+slug, flags); err != nil {
		t.Fatalf("runShowDocument json: %v", err)
	}

	var result showDocumentJSONResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput: %s", err, buf.String())
	}
	if result.Body == "" {
		t.Errorf("JSON result missing body; got: %+v", result)
	}
	if result.UID == "" {
		t.Errorf("JSON result missing uid; got: %+v", result)
	}
}

// ---- runBlameDocument ----

func TestRunBlameDocument_ReturnsHistory(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &blameDocumentFlags{format: "text", rev: "HEAD"}
	if err := runBlameDocument(ctx, &buf, dataDir, "document:"+slug, flags); err != nil {
		t.Fatalf("runBlameDocument: %v", err)
	}
	// Expect at least one commit entry in the output.
	out := buf.String()
	if !strings.Contains(out, slug) {
		t.Errorf("expected slug in blame header, got:\n%s", out)
	}
}

func TestRunBlameDocument_JSONOutputIsParseable(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &blameDocumentFlags{format: "json", rev: "HEAD"}
	if err := runBlameDocument(ctx, &buf, dataDir, "document:"+slug, flags); err != nil {
		t.Fatalf("runBlameDocument json: %v", err)
	}

	var result []blameEntry
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput: %s", err, buf.String())
	}
	if len(result) == 0 {
		t.Errorf("expected at least one blame entry, got empty array")
	}
}

// ---- github document link ----

func TestBuildGithubSubpath_DocumentFlagProducesBodyURL(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	baseURL := "https://github.com/example/repo"
	flags := &githubFlags{document: "document:" + slug}
	got := buildGithubSubpath(ctx, baseURL, repoDir, flags)

	want := baseURL + "/blob/main/rendered/documents/" + slug + ".md"
	if got != want {
		t.Errorf("buildGithubSubpath document = %q, want %q", got, want)
	}
}

func TestBuildGithubSubpath_DocumentFlagWithoutPrefix(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	// --document without the "document:" prefix — the command auto-prepends it.
	baseURL := "https://github.com/example/repo"
	flags := &githubFlags{document: slug}
	got := buildGithubSubpath(ctx, baseURL, repoDir, flags)

	want := baseURL + "/blob/main/rendered/documents/" + slug + ".md"
	if got != want {
		t.Errorf("buildGithubSubpath document (no prefix) = %q, want %q", got, want)
	}
}

func TestBuildGithubSubpath_UnknownDocumentFallsBackToBaseURL(t *testing.T) {
	t.Parallel()
	repoDir, _, _ := buildDocumentRepo(t)
	ctx := context.Background()

	baseURL := "https://github.com/example/repo"
	flags := &githubFlags{document: "document:nonexistent-xyz"}
	got := buildGithubSubpath(ctx, baseURL, repoDir, flags)

	// Should fall through to baseURL, not panic.
	if got != baseURL {
		t.Errorf("unknown document should fall back to base URL, got %q, want %q", got, baseURL)
	}
}

func TestRunDiffDocument_BadRevisionReturnsError(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &diffDocumentFlags{format: "text"}
	// A nonexistent revision must error, not silently render an empty diff.
	err := runDiffDocument(ctx, &buf, dataDir, "document:"+slug, "deadbeef..HEAD", flags)
	if err == nil {
		t.Fatalf("expected error for bad revision, got nil; output:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("expected revision-resolution error, got: %v", err)
	}
}

// buildStructuredDocumentRepo builds a repo whose second commit carries a
// sync-commit/v1 structured body enumerating the document UID with a "~"
// (revised) action, so blame can label the per-document action.
func buildStructuredDocumentRepo(t *testing.T) (repoDir, slug, uid string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary required: %v", err)
	}

	repoDir = t.TempDir()
	uid = "01HZABCDEFGHIJKLMNOPQQGAEN"
	slug = "alpha-doc__QQGAEN"

	ctx := context.Background()
	if out, err := exec.CommandContext(ctx, "git", "init", "--quiet", "--initial-branch=main", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	hermeticGit(t, repoDir, "config", "user.email", "test@mazarbul")
	hermeticGit(t, repoDir, "config", "user.name", "laptop-A")

	entities := `{"uid":"01HZABCDEFGHIJKLMNOPQQGAEN","kind":"document","displayName":"Alpha Doc","aliases":[],"confidence":1,"resolutionStatus":"resolved","createTime":"2026-06-01T00:00:00Z","lastUpdated":"2026-06-01T00:00:00Z","mentionCount":0,"orphaned":false}`
	writeRepoFile(t, repoDir, "rendered/entities.jsonl", entities)
	writeRepoFile(t, repoDir, "rendered/documents/"+slug+".md", "# Alpha Doc\n\nv1\n")
	hermeticGit(t, repoDir, "add", "rendered/entities.jsonl", "rendered/documents/"+slug+".md")
	// First commit: structured add.
	addMsg := "sync(personal): +1 document\n\nDocuments:\n  + documents/" + uid + "  \"notes/alpha.md\"\n\n" + commitSchemaV1Trailer + "\n"
	hermeticGit(t, repoDir, "commit", "-m", addMsg)

	// Second commit: structured revise.
	writeRepoFile(t, repoDir, "rendered/documents/"+slug+".md", "# Alpha Doc\n\nv2 revised\n")
	hermeticGit(t, repoDir, "add", "rendered/documents/"+slug+".md")
	reviseMsg := "sync(personal): ~1 document\n\nDocuments:\n  ~ documents/" + uid + "  \"notes/alpha.md\"\n\n" + commitSchemaV1Trailer + "\n"
	hermeticGit(t, repoDir, "commit", "-m", reviseMsg)

	return repoDir, slug, uid
}

func TestRunBlameDocument_StructuredCommitsCarryActions(t *testing.T) {
	t.Parallel()
	repoDir, slug, _ := buildStructuredDocumentRepo(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	writePersonalConfig(t, dataDir, "git@example.com:demo.git")
	scopeRepo := filepath.Join(dataDir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Dir(scopeRepo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repoDir, scopeRepo); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	flags := &blameDocumentFlags{format: "json", rev: "HEAD"}
	if err := runBlameDocument(ctx, &buf, dataDir, "document:"+slug, flags); err != nil {
		t.Fatalf("runBlameDocument: %v", err)
	}

	var entries []blameEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput: %s", err, buf.String())
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 blame entries, got %d: %+v", len(entries), entries)
	}
	// Oldest first: add then revise. With the UID threaded through, the actions
	// must be the structured "added"/"updated", not the neutral "modified".
	if entries[0].Action != diffActionAdded {
		t.Errorf("entry[0].Action = %q, want %q", entries[0].Action, diffActionAdded)
	}
	if entries[1].Action != commitActionUpdated {
		t.Errorf("entry[1].Action = %q, want %q", entries[1].Action, commitActionUpdated)
	}
}

// ---- help text coverage ----

func TestNewDiffDocumentCmd_HelpIsNonEmptyAndHasExamples(t *testing.T) {
	t.Parallel()
	cmd := newDiffDocumentCmd()
	if cmd.Long == "" {
		t.Fatal("diff document: Long help is empty")
	}
	if !strings.Contains(cmd.Long, "Examples:") {
		t.Error("diff document: Long help missing Examples section")
	}
}

func TestNewShowDocumentCmd_HelpIsNonEmptyAndHasExamples(t *testing.T) {
	t.Parallel()
	cmd := newShowDocumentCmd()
	if cmd.Long == "" {
		t.Fatal("show document: Long help is empty")
	}
	if !strings.Contains(cmd.Long, "Examples:") {
		t.Error("show document: Long help missing Examples section")
	}
}

func TestNewBlameDocumentCmd_HelpIsNonEmptyAndHasExamples(t *testing.T) {
	t.Parallel()
	cmd := newBlameDocumentCmd()
	if cmd.Long == "" {
		t.Fatal("blame document: Long help is empty")
	}
	if !strings.Contains(cmd.Long, "Examples:") {
		t.Error("blame document: Long help missing Examples section")
	}
}
