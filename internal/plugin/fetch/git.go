package fetch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// requireGit reports an actionable error when the system git binary is not on
// PATH. Remote install shells out to git; without it there is nothing to do.
func requireGit() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is required to install a remote preset; install git or use a local directory: %w", err)
	}
	return nil
}

// runGit runs `git <args>` in dir with a hardened, non-interactive environment
// and a per-invocation timeout. It returns stdout and stderr.
//
// The env discipline mirrors the sync transport (internal/sync/transport/git):
// credential prompting is disabled so a missing credential errors instead of
// hanging, global/system config is neutralized, and every repo-location
// variable that could redirect git away from dir is stripped. GIT_INDEX_FILE is
// the load-bearing omission: `go test` invoked from a git hook inherits it, so
// without the strip a clone here would write into the developer's real index
// and corrupt the working repository.
func runGit(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CEILING_DIRECTORIES="+dir,
	)
	cmd.Env = filterGitEnv(env)

	if err := cmd.Run(); err != nil {
		// Report only the git SUBCOMMAND (args[0]) plus git's own stderr, never the
		// full argv: the clone argv embeds the absolute staging path under the data
		// dir, which is noise to the operator and a needless path disclosure in
		// surfaced errors. git's stderr already names the offending URL/ref.
		sub := "git"
		if len(args) > 0 {
			sub = args[0]
		}
		return stdout.String(), stderr.String(),
			fmt.Errorf("git %s: %w: %s", sub, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}

// gitLocationVars returns the environment variables that can redirect git away
// from cmd.Dir. They are stripped so cmd.Dir is the sole source of truth.
func gitLocationVars() []string {
	return []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX",
	}
}

// gitConfigInjectionPrefixes returns the prefixes of the GIT_CONFIG_COUNT family
// (since git 2.31). These inject arbitrary git config at a HIGHER priority than
// GIT_CONFIG_GLOBAL=/dev/null, so an inherited GIT_CONFIG_COUNT=1 +
// GIT_CONFIG_KEY_0=protocol.allow + GIT_CONFIG_VALUE_0=always from a parent
// process would silently re-enable exotic transports. Stripped by prefix because
// the key/value vars are indexed (GIT_CONFIG_KEY_0, GIT_CONFIG_KEY_1, ...).
func gitConfigInjectionPrefixes() []string {
	return []string{"GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}
}

// filterGitEnv returns env with every gitLocationVars entry (matched as KEY=) and
// every gitConfigInjectionPrefixes entry (matched by prefix) removed.
func filterGitEnv(env []string) []string {
	out := env[:0]
	for _, kv := range env {
		drop := false
		for _, k := range gitLocationVars() {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			for _, p := range gitConfigInjectionPrefixes() {
				if strings.HasPrefix(kv, p) {
					drop = true
					break
				}
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
