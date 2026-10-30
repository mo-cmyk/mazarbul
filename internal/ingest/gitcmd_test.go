package ingest

import (
	"strings"
	"testing"
)

// TestCleanGitEnv_OverridesPreexistingPromptVars pins the env-shadowing fix: when
// the parent environment already sets GIT_TERMINAL_PROMPT / GIT_ASKPASS (a
// developer who exported them, or a CI runner that does), cleanGitEnv must DROP
// those copies and emit exactly one occurrence of each with our non-interactive
// values. git's getenv reads the FIRST occurrence in the env block, so a
// surviving parent copy would shadow our override and let an unauthenticated
// fetch hang on a credential prompt.
func TestCleanGitEnv_OverridesPreexistingPromptVars(t *testing.T) {
	// Not parallel: t.Setenv mutates the process environment.
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_ASKPASS", "/usr/bin/ssh-askpass")
	t.Setenv("GIT_DIR", "/somewhere/.git")

	env := cleanGitEnv()

	if got := valuesFor(env, "GIT_TERMINAL_PROMPT"); len(got) != 1 || got[0] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT occurrences = %v, want exactly [\"0\"]", got)
	}
	if got := valuesFor(env, "GIT_ASKPASS"); len(got) != 1 || got[0] != "/bin/true" {
		t.Errorf("GIT_ASKPASS occurrences = %v, want exactly [\"/bin/true\"]", got)
	}
	// The pure redirection var is dropped entirely (never re-appended).
	if got := valuesFor(env, "GIT_DIR"); len(got) != 0 {
		t.Errorf("GIT_DIR survived scrubbing: %v", got)
	}
}

// valuesFor returns every value assigned to key in a KEY=VALUE env slice, in
// order, so a test can prove there is exactly one (no shadowed duplicate).
func valuesFor(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			out = append(out, v)
		}
	}
	return out
}
