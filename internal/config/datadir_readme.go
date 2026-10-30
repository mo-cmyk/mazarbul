package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// dataDirReadmeFile is the orientation file written once at the data-dir root.
const dataDirReadmeFile = "README.md"

// dataDirReadmeBody explains the editability model to a human who opens the
// data directory in a file browser. It mirrors the synced-repo README's
// "what you may edit" contract but for the local on-disk layout: the vault is
// the one hand-edit surface, everything else is source-of-truth or derived.
const dataDirReadmeBody = `# Your Mazarbul data directory

This folder holds your AI agent's memory on this machine. Almost all of it is
**machine-managed** — the one thing you edit by hand is the ` + "`vault/`" + `.

## What you may edit

| Path | Edit by hand? | What it is |
|------|---------------|------------|
| ` + "`vault/*.md`" + ` | ✅ Yes | Your notes / documents. Saved edits are read back into memory automatically. **This is the steering wheel.** |
| ` + "`config.toml`" + ` | ✅ Yes | Your settings (` + "`maz config set <key> <value>`" + ` is the safe way). |
| ` + "`events/`" + ` | ⛔ Never | The append-only event log — the source of truth. Editing or deleting it loses or corrupts memory. |
| ` + "`index.db`, `graph.ladybug/`, `vectors.db`" + ` | ⛔ Never | Search indexes, rebuilt from ` + "`events/`" + ` by ` + "`maz rebuild`" + `. Hand-edits are overwritten. |
| ` + "`ontology/`, `state.toml`" + ` | ⛔ Never | Machine-managed projections. |
| ` + "`sync/`" + ` | ⛔ Never | Git mirror internals for ` + "`maz sync`" + `. |

To change your memory, edit files under ` + "`vault/`" + ` — or use ` + "`maz remember`, `maz relate`, `maz fact set`" + `,
and the other commands. Everything outside the vault is derived from ` + "`events/`" + `
and regenerated on demand, so editing it directly does nothing useful (and may
corrupt state).

Learn more: https://github.com/mo-cmyk/mazarbul
`

// EnsureDataDirReadme writes README.md at the data-dir root if it is absent.
// Idempotent: a populated file (including operator hand-edits) is preserved,
// matching the synced-repo README's write-once contract. A write failure is
// returned but is non-fatal to callers — the README is an orientation aid, not
// load-bearing state.
func EnsureDataDirReadme(dataDir string) error {
	if dataDir == "" {
		return errors.New("config: data dir is required")
	}
	path := filepath.Join(dataDir, dataDirReadmeFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: stat data-dir README: %w", err)
	}
	if err := os.WriteFile(path, []byte(dataDirReadmeBody), 0o644); err != nil {
		return fmt.Errorf("config: write data-dir README: %w", err)
	}
	return nil
}
