package ingest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// ingestOneBody runs the orchestrator over a single commit whose message is body
// and returns the resulting episode payload (unmarshalled) plus the raw event.
func ingestOneBody(t *testing.T, body string) ontology.Episode {
	t.Helper()
	dir := initRepo(t)
	_ = commit(t, dir, body)
	app := &fakeAppender{}
	o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
	if _, err := o.Run(context.Background(), NewLocalGitSource(dir, nil, nil), kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	eps := app.eventsOfKind(ontology.KindEpisode)
	if len(eps) != 1 {
		t.Fatalf("episodes = %d, want 1", len(eps))
	}
	if want := ontology.ConnectorSourceColonPrefix + connectorLocalGit; eps[0].Source != want {
		t.Errorf("episode Source = %q, want %q (leak gate would not engage)", eps[0].Source, want)
	}
	if eps[0].Confidence != 1.0 {
		t.Errorf("episode Confidence = %v, want 1.0 (trust via source prefix, not a cap)", eps[0].Confidence)
	}
	var ep ontology.Episode
	if err := json.Unmarshal(eps[0].Payload, &ep); err != nil {
		t.Fatalf("unmarshal episode: %v", err)
	}
	return ep
}

// TestInjectionCorpus_HardensEveryBody runs every injection-corpus fixture
// through ingest and asserts the ingest-layer defences: the body is fenced as
// UNTRUSTED data and the episode is connector-tagged (so the resolver's
// structural trust gate — status=proposed, no supersede, no mint, exercised in
// untrusted_gate_test.go — engages downstream). The episode is recorded at full
// confidence: trust is the connector source prefix + proposed status, not a cap.
func TestInjectionCorpus_HardensEveryBody(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "03_injection_corpus")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("injection corpus is empty")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, rerr := os.ReadFile(filepath.Join(dir, name))
			if rerr != nil {
				t.Fatalf("read %s: %v", name, rerr)
			}
			ep := ingestOneBody(t, string(raw))
			if !strings.Contains(ep.Text, "UNTRUSTED") {
				t.Errorf("%s: episode text not fenced as untrusted:\n%s", name, ep.Text)
			}
			if ep.Confidence != 1.0 {
				t.Errorf("%s: confidence %v, want 1.0 (trust via source prefix, not a cap)", name, ep.Confidence)
			}
		})
	}
}

// TestInjectionCorpus_SecretsScrubbed pins that the secret-shaped strings in the
// 02_secret_leak fixture never survive into the episode text.
func TestInjectionCorpus_SecretsScrubbed(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "03_injection_corpus", "02_secret_leak.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ep := ingestOneBody(t, string(raw))
	leaks := []string{
		"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"AKIAIOSFODNN7EXAMPLE",
		"correct-horse-battery-staple-42",
	}
	for _, secret := range leaks {
		if strings.Contains(ep.Text, secret) {
			t.Errorf("secret survived into episode text: %q\n%s", secret, ep.Text)
		}
	}
	if !strings.Contains(ep.Text, secretRedaction) {
		t.Errorf("expected redaction marker in episode text:\n%s", ep.Text)
	}
}

// TestInjectionCorpus_RemoteKeyShapesScrubbed pins that the remote-body secret
// shapes #212 added to the scrub corpus (Bearer / grn_ Granola keys / padded
// base64) never survive into the episode text. These are the realities a meeting
// transcript or chat export can carry that a git commit usually does not.
func TestInjectionCorpus_RemoteKeyShapesScrubbed(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "03_injection_corpus", "07_remote_key_escape.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ep := ingestOneBody(t, string(raw))
	leaks := []string{
		"grn_abcdefghijklmnopqrstuvwxyz0123456789",
		"grn_zyxwvutsrqponmlkjihgfedcba9876543210",
		"aGVsbG8gd29ybGQgdGhpcyBpcyBhIHNlY3JldA==",
	}
	for _, secret := range leaks {
		if strings.Contains(ep.Text, secret) {
			t.Errorf("remote-body secret survived into episode text: %q\n%s", secret, ep.Text)
		}
	}
	if !strings.Contains(ep.Text, secretRedaction) {
		t.Errorf("expected redaction marker in episode text:\n%s", ep.Text)
	}
}

// TestIngest_CapsOversizedBody pins the robustness fix the red-team found: a
// pathologically large commit body is capped before becoming an episode, so it
// cannot exceed the embedder's context window and silently drop out of vector
// recall. The cap leaves a visible truncation marker.
func TestIngest_CapsOversizedBody(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("A", maxEpisodeBodyBytes*2)
	ep := ingestOneBody(t, huge)
	// Episode text = fence preamble + capped body + marker + postamble. Bound it
	// generously above the cap; the point is it is NOT ~2x the cap.
	if len(ep.Text) > maxEpisodeBodyBytes+1024 {
		t.Errorf("episode text not capped: %d bytes (cap %d)", len(ep.Text), maxEpisodeBodyBytes)
	}
	if !strings.Contains(ep.Text, "truncated") {
		t.Errorf("missing truncation marker in capped body")
	}
}

// TestInjectionCorpus_ConnectionStringsScrubbed pins that connection-string
// passwords in the 04 fixture never survive into the episode (the red-team leak).
func TestInjectionCorpus_ConnectionStringsScrubbed(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "03_injection_corpus", "04_connection_strings.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ep := ingestOneBody(t, string(raw))
	for _, secret := range []string{"SuperSecretPassword123", "cacheP4ssw0rdValue", "M0ngoSecretValue"} {
		if strings.Contains(ep.Text, secret) {
			t.Errorf("connection-string password survived: %q\n%s", secret, ep.Text)
		}
	}
}

// TestInjectionCorpus_FenceEscapeNeutralized pins that the 05 fixture's embedded
// fence delimiters are defanged so the body cannot forge a fence boundary.
func TestInjectionCorpus_FenceEscapeNeutralized(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "03_injection_corpus", "05_fence_escape.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ep := ingestOneBody(t, string(raw))
	inner := strings.TrimSuffix(strings.TrimPrefix(ep.Text, untrustedPreamble), untrustedPostamble)
	if strings.Contains(inner, "UNTRUSTED>>>") || strings.Contains(inner, "<<<UNTRUSTED") {
		t.Errorf("embedded fence delimiter not neutralized in body:\n%s", inner)
	}
}

// TestCommitCorpus_CleanBodyUntouched confirms a clean commit body is preserved
// verbatim (within the fence) — scrubbing must not damage legitimate content.
func TestCommitCorpus_CleanBodyUntouched(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "01_commit_corpus", "01_clean.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ep := ingestOneBody(t, string(raw))
	if strings.Contains(ep.Text, secretRedaction) {
		t.Errorf("clean body was redacted:\n%s", ep.Text)
	}
	for _, want := range []string{"idempotency", "Carol", "Dave"} {
		if !strings.Contains(ep.Text, want) {
			t.Errorf("clean body lost content %q:\n%s", want, ep.Text)
		}
	}
}
