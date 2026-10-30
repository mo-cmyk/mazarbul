package git

// readme.go — idempotent first-push README writer for sync repos.
//
// When a Mazarbul daemon pushes to a fresh remote, the repo arrives
// at a host (GitHub, Gitea, self-hosted) as a directory of opaque
// chunk files. Without context, the next human who lands there has
// no idea what they're looking at: a backup? a config? a leak? The
// README solves that.
//
// Design constraints:
//
//   - Written ONCE on first push per scope. Never rewritten so any
//     hand-edits (badges, repo description, contact links) survive.
//   - Idempotent: a second call on a populated repo is a no-op.
//   - Plaintext metadata only: never the events themselves.
//   - Forward-only friendly: works for plaintext-default repos AND
//     for encrypted-mode repos, with mode called out explicitly.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// readmeFilename is the canonical name written at scope repo root.
const readmeFilename = "README.md"

// repoHasEncryptedChunks reports whether any blob in a push is
// encrypted. Used by the README + .gitattributes writers to decide
// which mode-flavoured prose / rules to drop into the repo root on
// the first commit.
func repoHasEncryptedChunks(blobs []transport.Blob) bool {
	for i := range blobs {
		if blobs[i].Header.Encrypted {
			return true
		}
	}
	return false
}

// readmeWriter writes a Mazarbul sync-repo README to a directory at
// most once. Caller-supplied so tests can stub time / device.
type readmeWriter struct {
	now      func() time.Time
	deviceID string
}

func newReadmeWriter(deviceID string) *readmeWriter {
	return &readmeWriter{now: time.Now, deviceID: deviceID}
}

// ensureREADME writes README.md to repoDir if absent. Returns
// (created, error). Created=false means the file already existed
// (idempotent path); error is reserved for write failures.
func (w *readmeWriter) ensureREADME(repoDir string, scope kernel.Scope, encrypted bool) (bool, error) {
	path := filepath.Join(repoDir, readmeFilename)
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("git transport: stat README: %w", err)
	}
	body := w.renderREADME(scope, encrypted)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // committed plaintext doc, world-readable is fine
		return false, fmt.Errorf("git transport: write README: %w", err)
	}
	return true, nil
}

func (w *readmeWriter) renderREADME(scope kernel.Scope, encrypted bool) string {
	var b strings.Builder
	w.writeHeader(&b, scope, encrypted)
	writeEditability(&b, encrypted)
	if !encrypted {
		writeAtAGlance(&b)
		writeHowItWorks(&b)
	}
	writeLayout(&b, encrypted)
	if encrypted {
		w.writeEncryptedSection(&b)
	} else {
		w.writePlaintextSection(&b)
		writeBrowse(&b)
	}
	writeUsing(&b, scope, encrypted)
	w.writeReadingOnNewDevice(&b, scope, encrypted)
	writeMore(&b)
	return b.String()
}

// writeHeader stamps the branded title, tagline, and a one-line statement of
// what the repo is and which privacy mode it is in.
func (w *readmeWriter) writeHeader(b *strings.Builder, scope kernel.Scope, encrypted bool) {
	fmt.Fprintf(b, "# Your Mazarbul memory — `%s` scope\n\n", scope)
	b.WriteString("> A local-first, cryptographically signed, **git-native** memory you own. ")
	b.WriteString("This repository is the synced mirror of your AI agent's memory.\n\n")
	b.WriteString("## What this is\n\n")
	b.WriteString("The source of truth lives on your device as an append-only, signed event log ")
	b.WriteString("([Mazarbul](https://github.com/mo-cmyk/mazarbul)). Mazarbul seals batches of those events and ")
	b.WriteString("pushes them here, so your other devices — and, for a team scope, your teammates — can pull and ")
	b.WriteString("replay the same memory. Each commit is one batch of sealed events.\n\n")
	if encrypted {
		b.WriteString("This scope is **encrypted**: events ship as XChaCha20-Poly1305-sealed ciphertext, so the git ")
		b.WriteString("host sees only opaque blobs — no event text, no entity names.\n\n")
	} else {
		b.WriteString("This scope is **plaintext**: events ship as line-delimited JSON, so `git log`, `git blame`, ")
		b.WriteString("and the GitHub web UI all work directly on your memory.\n\n")
	}
	fmt.Fprintf(b, "_Initialised by `%s` on %s._\n\n", w.deviceID, w.now().UTC().Format(time.RFC3339))
}

// writeEditability is the load-bearing "what you may edit" section, placed high
// so a human cannot miss it. The contract is mode-aware: an encrypted scope has
// no rendered/ontology/DASHBOARD trees.
func writeEditability(b *strings.Builder, encrypted bool) {
	b.WriteString("## ⚠ What you may edit\n\n")
	b.WriteString("This repository is **mostly machine-managed**. To change your memory, edit it on the device that ")
	b.WriteString("owns it (your local `vault/`, or `maz remember` / `maz relate` / …) and let Mazarbul sync — ")
	b.WriteString("**do not hand-edit files here.** This repo is a mirror, not the steering wheel.\n\n")
	b.WriteString("| Path | Edit here? | What it is |\n")
	b.WriteString("|------|-----------|------------|\n")
	if encrypted {
		b.WriteString("| `chunks/*.maz` | ⛔ Never | The sealed event stream — the source of truth. Tampering fails the cipher's auth tag on pull. |\n")
	} else {
		b.WriteString("| `chunks/*.jsonl` | ⛔ Never | The signed event stream — the source of truth. A hand-edit is detected and **refused on the next pull**. |\n")
		b.WriteString("| `rendered/` | ⛔ Never | A human-readable view of your graph, **regenerated and pruned every push**. Edits here are deleted. |\n")
		b.WriteString("| `ontology/` | ⛔ Never | Your entity-kind & predicate definitions, regenerated every push. |\n")
		b.WriteString("| `DASHBOARD.md`, `CHANGELOG/` | ⛔ Never | Machine-generated summaries, overwritten every push. |\n")
	}
	b.WriteString("| `MANIFEST.json` | ⛔ Never | The chunk index, regenerated every push. |\n")
	b.WriteString("| `README.md`, `.gitattributes` | ✅ Yes | Written once; your edits are preserved. Add a description, badges, or notes. |\n\n")
}

// writeAtAGlance points the reader at the live dashboard (plaintext only).
func writeAtAGlance(b *strings.Builder) {
	b.WriteString("## At a glance\n\n")
	b.WriteString("See **[DASHBOARD.md](DASHBOARD.md)** for live counts, recent activity, and a graph of your ")
	b.WriteString("memory — regenerated on every push.\n\n")
}

// writeHowItWorks embeds a compact Mermaid diagram of the log→mirror→replay
// flow. GitHub renders ```mermaid fences natively, so no image asset is needed.
func writeHowItWorks(b *strings.Builder) {
	b.WriteString("## How it works\n\n")
	b.WriteString("```mermaid\nflowchart LR\n")
	b.WriteString("  A[\"maz remember /<br/>relate / fact\"] --> B[\"signed event log<br/>(source of truth,<br/>on your device)\"]\n")
	b.WriteString("  B --> C{{\"seal + push\"}}\n")
	b.WriteString("  C --> D[(\"this git repo<br/>chunks/ + rendered/\")]\n")
	b.WriteString("  D --> E[\"pull + replay<br/>(your other devices)\"]\n")
	b.WriteString("```\n\n")
}

// writeLayout describes the on-disk tree, tagging each entry with its tier.
func writeLayout(b *strings.Builder, encrypted bool) {
	b.WriteString("## Layout\n\n```\n")
	if encrypted {
		b.WriteString("chunks/*.maz       # sealed event batches (SOURCE — do not edit)\n")
	} else {
		b.WriteString("chunks/*.jsonl     # signed event batches, one event per line (SOURCE — do not edit)\n")
		b.WriteString("rendered/          # human-readable graph view (DERIVED — regenerated each push)\n")
		b.WriteString("ontology/          # entity-kind & predicate definitions (DERIVED)\n")
		b.WriteString("DASHBOARD.md       # counts + activity + graph (DERIVED)\n")
		b.WriteString("CHANGELOG/         # per-push change summaries (DERIVED)\n")
	}
	b.WriteString("MANIFEST.json      # chunk index: count, time range, device, content hash (DERIVED)\n")
	b.WriteString("README.md          # this file (AUTHORED — yours to edit)\n")
	b.WriteString(".gitattributes     # marks generated trees + *.maz binary (AUTHORED)\n")
	b.WriteString("```\n\n")
}

// writeBrowse lists the navigable views of the memory (plaintext only).
func writeBrowse(b *strings.Builder) {
	b.WriteString("## Browse your memory\n\n")
	b.WriteString("- **[DASHBOARD.md](DASHBOARD.md)** — counts and the graph, at a glance.\n")
	b.WriteString("- `rendered/by-kind/<kind>/INDEX.md` — every entity of a kind, in a table.\n")
	b.WriteString("- `rendered/by-uid/…` — one page per entity, with its relations.\n")
	b.WriteString("- `chunks/` — the raw event stream; `git log -p` shows every change.\n\n")
}

// writeUsing shows a few real commands so a human knows how to actually work
// with the memory, both via maz and via plain git.
func writeUsing(b *strings.Builder, scope kernel.Scope, encrypted bool) {
	b.WriteString("## Using it\n\n```bash\n")
	b.WriteString("# On a device paired to this repo:\n")
	b.WriteString("maz recall \"what do I know about X\"  # hybrid search over this memory\n")
	b.WriteString("maz get \"Some Entity\"  # fetch one entity and its relations\n")
	fmt.Fprintf(b, "maz sync pull --scope %s  # replay the latest from this remote\n", scope)
	if !encrypted {
		b.WriteString("\n# Or with plain git — it's just JSONL + markdown:\n")
		b.WriteString("git log --stat\n")
		b.WriteString("git blame rendered/by-uid/<uid>.md  # line-level provenance\n")
	}
	b.WriteString("```\n\n")
}

// writeReadingOnNewDevice explains how to bring the memory up on a fresh device.
func (w *readmeWriter) writeReadingOnNewDevice(b *strings.Builder, scope kernel.Scope, encrypted bool) {
	b.WriteString("## Reading on a new device\n\n")
	b.WriteString("Pair the new device against this remote, then have its daemon pull on its own cadence:\n\n")
	b.WriteString("```bash\n")
	if encrypted {
		b.WriteString("maz init --restore --watchword \"<24 words from your existing device>\"\n")
	}
	fmt.Fprintf(b, "maz sync pull --scope %s\n```\n\n", scope)
}

// writeMore links the design docs and the project.
func writeMore(b *strings.Builder) {
	b.WriteString("## More\n\n")
	b.WriteString("- Project: <https://github.com/mo-cmyk/mazarbul>\n")
	b.WriteString("- Sync protocol: <https://github.com/mo-cmyk/mazarbul/blob/main/agent-docs/sync.md>\n")
	b.WriteString("- Privacy model: <https://github.com/mo-cmyk/mazarbul/blob/main/agent-docs/privacy.md>\n")
}

func (w *readmeWriter) writePlaintextSection(b *strings.Builder) {
	b.WriteString("## Mode: plaintext (default)\n\n")
	b.WriteString("Events ship as line-delimited JSON. Anyone with read access to this repo can read every event in plain text. ")
	b.WriteString("In exchange, you get every GitHub-native feature:\n\n")
	b.WriteString("- `git log` shows every push; `git blame` attributes each event to the commit (and device) that produced it.\n")
	b.WriteString("- GitHub's web UI renders chunks with syntax highlighting and line-level diffs.\n")
	b.WriteString("- `git log -S \"some text\"` searches the full event history.\n\n")
	b.WriteString("If you want privacy from your git host, switch to encrypted mode by setting `encryption = \"encrypted\"` under `[sync.<scope>]` in your `config.toml` (run `maz config edit` to open it). If you haven't paired a Watchword yet, `maz mellon pair` will generate one. The toggle is forward-only: past plaintext events stay in the git history.\n\n")
}

func (w *readmeWriter) writeEncryptedSection(b *strings.Builder) {
	b.WriteString("## Mode: encrypted (opt-in)\n\n")
	b.WriteString("Events are sealed with XChaCha20-Poly1305 under a Watchword-derived scope key. The git host sees only ciphertext — no event payloads, no entity names, no timestamps inside events (though the chunk metadata in MANIFEST.json is plaintext: count, time range, device per chunk).\n\n")
	b.WriteString("Tradeoff: GitHub's web UI shows opaque binary blobs; `git diff` and `git blame` only operate at the chunk-file level. The `*.maz binary` line in `.gitattributes` tells git not to try semantic merges of ciphertext.\n\n")
	b.WriteString("To decrypt locally: clone the repo, pair the device with your Watchword, run `maz sync pull --scope <name>`.\n\n")
}
