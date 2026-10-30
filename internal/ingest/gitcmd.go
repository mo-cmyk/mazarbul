package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// gitBin is the git executable. PATH lookup; no absolute path so the user's
// toolchain selection (mise, system) is honoured.
const gitBin = "git"

// runGit runs `git -C dir <args...>` with a scrubbed environment and returns
// stdout. It mirrors internal/cli/doc_git.go: the cleaned env drops any GIT_*
// variables a parent git hook injected, which would otherwise redirect git away
// from -C dir and corrupt the developer's real repository (the PR #146 class of
// bug).
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir}, args...)
	//nolint:gosec // gitBin is the constant "git"; args are built by the Source
	// from controlled cursor/repo inputs (no shell, no user-supplied executable).
	cmd := exec.CommandContext(ctx, gitBin, full...)
	cmd.Env = cleanGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// gitStream is a running `git` whose stdout is consumed incrementally, so a
// caller can stop after N records without buffering the whole output. This is
// the bound that keeps a fresh backfill over a huge repository from allocating
// O(repo) memory: the reader breaks once it has enough and Close kills git.
type gitStream struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *bytes.Buffer
}

// runGitStream starts `git -C dir <args...>` with a scrubbed environment and
// returns a handle whose Stdout the caller reads incrementally. The caller MUST
// call Close when done — including on the early-stop path — to reap the process.
func runGitStream(ctx context.Context, dir string, args ...string) (*gitStream, error) {
	sctx, cancel := context.WithCancel(ctx)
	full := append([]string{"-C", dir}, args...)
	//nolint:gosec // gitBin is the constant "git"; args are built by the Source
	// from controlled cursor/repo inputs (no shell, no user-supplied executable).
	cmd := exec.CommandContext(sctx, gitBin, full...)
	cmd.Env = cleanGitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("git %s: stdout pipe: %w", strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("git %s: start: %w", strings.Join(args, " "), err)
	}
	return &gitStream{cancel: cancel, cmd: cmd, stdout: stdout, stderr: &stderr}, nil
}

// Stdout is the live stdout pipe; read it incrementally.
func (g *gitStream) Stdout() io.Reader { return g.stdout }

// Close stops the git process and reaps it. StoppedEarly says whether the caller
// broke out before EOF.
//
// On the early-stop path git is killed via the cancelled context FIRST, so Wait
// reports a kill signal that is expected and suppressed. On the natural-EOF path
// git has emitted everything and is exiting on its own; Wait must run WITHOUT a
// preceding cancel, otherwise the SIGKILL races git's clean shutdown and turns a
// successful read into a spurious "signal: killed" — which would surface as an
// empty stream. There, a non-nil Wait error is a genuine git failure (e.g. a bad
// revision range) and is returned with the captured stderr for context.
func (g *gitStream) Close(stoppedEarly bool) error {
	if stoppedEarly {
		g.cancel() // we deliberately stopped reading; kill git before reaping
	}
	_ = g.stdout.Close() //nolint:errcheck // closing the read end is best-effort; Wait also closes it
	err := g.cmd.Wait()
	g.cancel() // release context resources; a no-op once the process has exited
	if err != nil && !stoppedEarly {
		return fmt.Errorf("git: %w: %s", err, strings.TrimSpace(g.stderr.String()))
	}
	return nil
}

// cleanGitEnv returns a scrubbed environment for git sub-processes, dropping the
// GIT_* redirection variables a parent hook may have exported. See
// internal/cli/doc_git.go for the rationale (PR #146).
//
// GIT_TERMINAL_PROMPT / GIT_ASKPASS are dropped here too, then re-appended with
// our non-interactive values below: when an env slice carries duplicate keys,
// git's getenv reads the FIRST occurrence, so a pre-existing prompt-enabling
// value in the parent environment would otherwise shadow our override and let an
// unauthenticated fetch hang on a credential prompt.
func cleanGitEnv() []string {
	drop := map[string]struct{}{
		"GIT_DIR":                          {},
		"GIT_WORK_TREE":                    {},
		"GIT_INDEX_FILE":                   {},
		"GIT_OBJECT_DIRECTORY":             {},
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
		"GIT_COMMON_DIR":                   {},
		"GIT_NAMESPACE":                    {},
		"GIT_PREFIX":                       {},
		"GIT_TERMINAL_PROMPT":              {},
		"GIT_ASKPASS":                      {},
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		key := kv
		if idx := strings.Index(kv, "="); idx >= 0 {
			key = kv[:idx]
		}
		if _, bad := drop[key]; !bad {
			out = append(out, kv)
		}
	}
	out = append(out, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/true")
	return out
}
