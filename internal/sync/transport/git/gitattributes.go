package git

// gitattributes.go — idempotent first-push .gitattributes writer.
//
// Marks *.maz as binary so git does not try to compute line diffs
// or three-way merges on ciphertext. Leaves *.jsonl to git's
// defaults (it correctly auto-detects line-delimited JSON, applies
// syntax highlighting in the GitHub web UI, and produces useful
// per-event diffs on commit). Plaintext is short; written once on
// first push per scope and never rewritten so operator-added rules
// (binary patterns for other tracked assets, EOL normalisation,
// merge drivers) survive.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const gitattributesFilename = ".gitattributes"

// gitattributesBody is the verbatim content written on first push.
// The trailing newline keeps git from complaining about an
// "incomplete final line" if a downstream tool prints the file.
//
// The linguist-generated rules mark the machine-managed, regenerated
// trees so GitHub collapses them in diffs and drops them from the
// repo's language stats — a human reading the repo on github.com sees
// the signal (chunks + README + DASHBOARD), not the derived noise.
// Chunks/ is deliberately NOT marked: plaintext chunks' line-level
// diff / blame / web-UI search is the whole point of plaintext mode,
// and *.maz binary already stops textual diffs on encrypted chunks.
const gitattributesBody = `# Mazarbul sync repo — managed by maz sync push.
# Mark encrypted chunks as binary so git does not attempt textual
# diffs / merges on XChaCha20-Poly1305 ciphertext.
*.maz binary

# Machine-managed, regenerated every push: collapse in diffs and exclude
# from language stats. Do not hand-edit — your changes are overwritten
# or pruned on the next sync. (chunks/ is intentionally left visible.)
rendered/**   linguist-generated=true
ontology/**   linguist-generated=true
CHANGELOG/**  linguist-generated=true
MANIFEST.json linguist-generated=true
DASHBOARD.md  linguist-generated=true
`

// ensureGitattributes writes .gitattributes to repoDir if absent.
// Returns (created, error); created=false means the file already
// existed and was preserved.
func ensureGitattributes(repoDir string) (bool, error) {
	path := filepath.Join(repoDir, gitattributesFilename)
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("git transport: stat .gitattributes: %w", err)
	}
	if err := os.WriteFile(path, []byte(gitattributesBody), 0o644); err != nil { //nolint:gosec // committed plaintext rules, world-readable is fine
		return false, fmt.Errorf("git transport: write .gitattributes: %w", err)
	}
	return true, nil
}
