package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// ── WS1: RegisterDynamicCompletions tests ─────────────────────────────────────

// TestCompletionScopeFlag verifies that --scope completions return the three
// canonical scope values and no file completions.
func TestCompletionScopeFlag(t *testing.T) {
	t.Parallel()
	completions, directive := completeScopeFlag(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; want ShellCompDirectiveNoFileComp", directive)
	}
	wantScopes := []string{"project", "personal", "team"}
	for _, want := range wantScopes {
		found := false
		for _, c := range completions {
			if strings.HasPrefix(c, want+"\t") || c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scope %q not found in completions %v", want, completions)
		}
	}
}

// TestCompletionOutputFlag verifies that --output completions return the three
// format values.
func TestCompletionOutputFlag(t *testing.T) {
	t.Parallel()
	completions, directive := completeOutputFlag(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; want ShellCompDirectiveNoFileComp", directive)
	}
	for _, want := range []string{"text", "json", "ndjson"} {
		found := false
		for _, c := range completions {
			if strings.HasPrefix(c, want+"\t") || c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("output format %q not found in completions %v", want, completions)
		}
	}
}

// TestCompletionSortByFlag verifies --sort-by completions.
func TestCompletionSortByFlag(t *testing.T) {
	t.Parallel()
	completions, directive := completeSortByFlag(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; want ShellCompDirectiveNoFileComp", directive)
	}
	for _, want := range []string{"recent", "recallCount"} {
		found := false
		for _, c := range completions {
			if strings.HasPrefix(c, want+"\t") || c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sort-by value %q not found in completions %v", want, completions)
		}
	}
}

// TestCompletionDaemonDown verifies that daemon-backed completions return
// (nil, NoFileComp) within the hard 300 ms deadline when the daemon is not
// running, rather than blocking or returning file completions.
func TestCompletionDaemonDown(t *testing.T) {
	t.Parallel()

	// Use a throwaway data dir that has no daemon socket.
	root := newRootCmd()
	if err := root.PersistentFlags().Set("data-dir", t.TempDir()); err != nil {
		t.Fatalf("set data-dir: %v", err)
	}

	start := time.Now()
	completions, directive := completeDaemonPredicatesFlag(root, nil, "")
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("daemon completion took %v; want ≤500ms", elapsed)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; want ShellCompDirectiveNoFileComp when daemon is down", directive)
	}
	if len(completions) != 0 {
		t.Errorf("completions = %v; want nil when daemon is down", completions)
	}
}

// TestRegisterDynamicCompletions_NoPanic verifies that RegisterDynamicCompletions
// can be called on the full command tree without panicking. It also verifies
// that the root command was constructed without errors (newRootCmd already
// calls RegisterDynamicCompletions internally).
func TestRegisterDynamicCompletions_NoPanic(t *testing.T) {
	t.Parallel()
	// Should not panic even when called a second time (idempotent-ish: errors
	// from double-register are silently ignored by tryRegisterFlagCompletion).
	root := newRootCmd()
	RegisterDynamicCompletions(root)
}

// TestCompletionSourceLine pins the per-shell rc line. fish has no process
// substitution, so it must use the pipe form; bash/zsh use `source <(…)`. The
// installer, the printed hint, and the doctor setup.completion check all share
// this helper, so a regression here would silently break fish completion.
func TestCompletionSourceLine(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"bash": "source <(maz completion bash)",
		"zsh":  "source <(maz completion zsh)",
		"fish": "maz completion fish | source",
	}
	for shell, want := range cases {
		if got := completionSourceLine(shell); got != want {
			t.Errorf("completionSourceLine(%q) = %q, want %q", shell, got, want)
		}
	}
	// fish must never emit bash/zsh process substitution.
	if strings.Contains(completionSourceLine("fish"), "<(") {
		t.Errorf("fish line must not use process substitution: %q", completionSourceLine("fish"))
	}
}
