package git

import (
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
)

// renderedDirName is the working-tree directory that holds the human-
// readable view of the scope's entity graph. Sibling of chunks/ so a
// reader landing on github.com sees both the canonical JSONL stream
// (chunks/) and the rendered semantic snapshot (rendered/) without
// having to install Mazarbul locally.
const renderedDirName = "rendered"

// ontologyDirName is the root-level renderer-owned subtree holding the
// git-rendered taxonomy (ontology/kinds/, ontology/schemas/,
// ontology/predicates.yaml). It is a sibling of rendered/, not nested inside
// it, so reconciliation must walk it independently. Like rendered/, every
// file under it is renderer-owned (no transport-owned files), so a path not
// in the renderer's view is stale and must be pruned — otherwise a removed or
// renamed kind/schema/predicate (or a change to the schema-filename mapping)
// would leak its old artifact and keep a Windows-unsafe or duplicate file
// tracked in the synced repo.
const ontologyDirName = "ontology"

// writeRenderedView regenerates rendered/ from the current chunks/
// working tree. Called after writeRepoMetadata's manifest write
// completes; runs only for plaintext-only pushes (encrypted scopes
// skip the renderer because rendered files would defeat the
// encryption). Idempotent: same chunk set always produces the same
// bytes (the renderer is pure).
//
// Implementation note: we re-render from the FULL chunk set in the
// working tree, not the delta. The renderer's PriorState path is
// available but plumbing it through Transport would require a per-
// scope state cache; we ship the full-rebuild shape first because
// the renderer's bench budget (~30s for 100k entities + 500k
// relations) is fine for push latency and the determinism is
// trivially auditable. Future work can wire PriorState through once
// scale data justifies the complexity.
func (t *Transport) writeRenderedView(scope kernel.Scope, blobs []transport.Blob, repoDir, chunksDir string) error {
	if repoHasEncryptedChunks(blobs) {
		// Scope just flipped (or has always been) encrypted. If a
		// previous plaintext push left a rendered/ tree on disk the
		// next `git add .` would re-commit it under the encrypted
		// scope, leaking the old human-readable graph at HEAD. Prune
		// the directory so the encrypted snapshot is genuinely
		// rendered-free — matches the documented invariant in
		// agent-docs/privacy.md.
		return pruneRenderedTree(repoDir)
	}
	events, headULID, devices, err := collectPlaintextEvents(scope, chunksDir)
	if err != nil {
		return fmt.Errorf("git transport: collect events for render: %w", err)
	}
	if len(events) == 0 {
		return nil
	}
	view, err := render.Render(render.Input{
		Scope:      scope,
		Events:     sliceSeq(events),
		HeadULID:   headULID,
		Device:     t.deviceID,
		Devices:    devices,
		RenderedAt: headULIDTime(headULID),
	})
	if err != nil {
		return fmt.Errorf("git transport: render: %w", err)
	}
	// The renderer's File.Path values are already repo-root-relative
	// ("rendered/entities.jsonl", "CHANGELOG/2026/06/..."): writing at
	// repoDir directly preserves the plan's sibling layout and avoids
	// a second `rendered/` prefix collapsing rendered/CHANGELOG/ inside
	// rendered/.
	if err := reconcileRenderedDir(repoDir, view.Files); err != nil {
		return err
	}
	// Persist the render cursor so the next push (and the
	// sync.render_cursor_behind doctor check) sees what was last
	// rendered. Written outside reconcileRenderedDir so the reconciler
	// stays narrowly scoped to renderer-emitted File entries.
	cursorPath := filepath.Join(repoDir, renderedDirName, ".cursor.json")
	return render.WriteCursor(cursorPath, render.Cursor{
		ULID:          view.Cursor,
		RenderedAt:    headULIDTime(headULID),
		SchemaVersion: render.CursorSchemaVersion,
	})
}

// headULIDTime extracts the embedded timestamp from a ULID so the
// render cursor's RenderedAt stays deterministic (a wall-clock time.Now
// would break the byte-identical-across-runs invariant the renderer
// promises). The ULID's first 48 bits encode a Unix-millisecond
// timestamp; on a parse error we fall back to the zero time, which is
// honest about the missing signal and still serialises cleanly.
func headULIDTime(headULID string) time.Time {
	if headULID == "" {
		return time.Time{}
	}
	id, err := ulid.Parse(strings.ToUpper(headULID))
	if err != nil {
		return time.Time{}
	}
	return ulid.Time(id.Time()).UTC()
}

// errSensitiveRenderPlaintext mirrors the seal-time and writeBlob gates
// at the render boundary: collectPlaintextEvents refuses to render a
// plaintext chunk that carries a sensitive event (a chunk pulled from
// the remote, or sealed before a marker existed) so the payload never
// reaches the human-readable rendered/ tree. Abort, not skip — skipping
// would partially render and reveal the sensitive event's absence.
// ErrTransport-wrapped so callers classify it with the other transport
// failures. Project scope is exempt (it is intentionally public).
var errSensitiveRenderPlaintext = fmt.Errorf(
	"%w: refuse to render sensitive payload from a plaintext chunk", transport.ErrTransport)

// refuseSensitivePlaintext returns an error if any event is sensitive and
// scope is not exempt — the render mirror of the seal-time gate. Project
// is exempt (intentionally public). Extracted so collectPlaintextEvents
// stays under the cognitive-complexity gate.
func refuseSensitivePlaintext(scope kernel.Scope, chunkName string, events []kernel.Event) error {
	if scope == kernel.ScopeProject {
		return nil
	}
	for i := range events {
		if privacy.IsSensitivePayload(events[i]) {
			return fmt.Errorf("%w: chunk %s event uid=%s scope=%q",
				errSensitiveRenderPlaintext, chunkName, events[i].UID, scope)
		}
	}
	return nil
}

// collectPlaintextEvents reads every plaintext chunk in chunksDir and
// returns the union of their events, sorted by ULID. Encrypted chunks
// in the directory are skipped (the renderer is plaintext-only) —
// they are still pushed via the normal materializeBlobs path.
//
// The returned headULID is the highest ULID across the union. The
// renderer's changelog fragment uses it as the file name so per-push
// fragments sort chronologically without a separate timestamp.
//
// Devices is the distinct, sorted set of sealing devices read from the chunk
// headers, fed to the renderer's DASHBOARD.md "synced from N devices" banner.
// Headers with a blank Device (legacy plaintext chunks predate the device tag)
// contribute nothing.
func collectPlaintextEvents(scope kernel.Scope, chunksDir string) ([]kernel.Event, string, []string, error) {
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil, nil
		}
		return nil, "", nil, fmt.Errorf("readdir chunks: %w", err)
	}
	var (
		all      []kernel.Event
		headULID string
		devSet   = make(map[string]struct{})
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isPlaintextChunkName(name) {
			continue
		}
		path := filepath.Join(chunksDir, name)
		blob, rerr := os.ReadFile(path) //nolint:gosec // bounded by ReadDir of internal dir
		if rerr != nil {
			return nil, "", nil, fmt.Errorf("read %s: %w", path, rerr)
		}
		hdr, events, derr := chunk.DecodePlaintext(blob, scope)
		if derr != nil {
			return nil, "", nil, fmt.Errorf("decode %s: %w", path, derr)
		}
		if serr := refuseSensitivePlaintext(scope, name, events); serr != nil {
			return nil, "", nil, serr
		}
		all = append(all, events...)
		if hdr.Device != "" {
			devSet[hdr.Device] = struct{}{}
		}
		if hdr.LastULID > headULID {
			headULID = hdr.LastULID
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].UID < all[j].UID })
	return all, headULID, sortedSetKeys(devSet), nil
}

// sortedSetKeys returns the keys of a string set in lexical order (nil for an
// empty set, so the renderer's "synced from N devices" banner is suppressed
// rather than printing "0 devices").
func sortedSetKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// refuseSensitiveWorkingTree is the Push pre-flight mirror of the render
// guard: it scans the plaintext chunks already present in chunksDir and
// returns the sensitive sentinel if any carries a sensitive event, so
// Push can refuse BEFORE materializing any blob (see
// prepareScopeWorkingTree for why the ordering is load-bearing). It
// reuses collectPlaintextEvents — which already aborts on a sensitive
// chunk — and discards the events; that is a second decode pass over the
// working tree, but push is a cold path and the reuse keeps the guard
// logic in exactly one place.
//
// It runs for every non-project push, INCLUDING an encrypted-mode push.
// A sensitive plaintext chunk sitting in the working tree is staged by
// commitAndPush's `git add .` regardless of whether the new blobs are
// encrypted, so enabling encryption does not by itself neutralise it: the
// cleartext chunk is still committed (and, on a scope rotation, freshly
// pushed to the new remote). Enabling encryption stops *new* plaintext
// seals; an existing sensitive plaintext chunk must be removed, and until
// it is, every push to that scope is refused. Project scope alone is
// exempt — it is intentionally public.
func refuseSensitiveWorkingTree(scope kernel.Scope, chunksDir string) error {
	if scope == kernel.ScopeProject {
		return nil
	}
	_, _, _, err := collectPlaintextEvents(scope, chunksDir)
	return err
}

// isPlaintextChunkName matches the .jsonl extension used by the
// plaintext sealer. The .maz extension is encrypted; we skip
// those because we can't decode them without the scope key, and the
// renderer is plaintext-only by design.
func isPlaintextChunkName(name string) bool {
	return len(name) > len(".jsonl") && name[len(name)-len(".jsonl"):] == ".jsonl"
}

// changelogDirName is the top-level CHANGELOG tree the renderer writes
// alongside rendered/. Sibling of rendered/, never nested inside it (see
// TestRenderedLayout_PathsMatchPlan), so the encrypted-scope prune must
// remove both subtrees independently.
const changelogDirName = "CHANGELOG"

// dashboardFileName is the repo-root DASHBOARD.md the renderer emits on every
// plaintext push (render.dashboardPath). Like rendered/, it is a plaintext-only
// artifact, so an encrypted flip must delete it to keep the encrypted snapshot
// rendered-free.
const dashboardFileName = "DASHBOARD.md"

// pruneRenderedTree removes every renderer-owned subtree from repoDir.
// Called when a scope is pushed in encrypted mode: a previous plaintext
// push may have left a human-readable snapshot in place under rendered/,
// CHANGELOG/, or the root-level ontology/ taxonomy tree, and `git add .`
// would otherwise re-commit those under the encrypted scope, leaking the
// historical graph (and plaintext taxonomy) at HEAD. All three subtrees are
// pruned so the encrypted snapshot is genuinely rendered-free (matches
// agent-docs/privacy.md). Returning without error when a directory is absent
// keeps the call idempotent across the always-encrypted case (nothing to
// prune) and the switched-from-plaintext case (real prune).
func pruneRenderedTree(repoDir string) error {
	// rendered/, CHANGELOG/, ontology/ are directories; DASHBOARD.md is a
	// root-level file. os.RemoveAll handles both, and a missing path is a no-op,
	// so the call stays idempotent across the always-encrypted and
	// switched-from-plaintext cases.
	for _, sub := range []string{renderedDirName, changelogDirName, ontologyDirName, dashboardFileName} {
		path := filepath.Join(repoDir, sub)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("git transport: prune %s: %w", sub, err)
		}
	}
	return nil
}

// sliceSeq turns a slice into an iter.Seq for the renderer's Input.
// Renderer-facing helper kept private to the transport; the renderer
// already exports a Render entry-point so we just adapt the input shape.
func sliceSeq[T any](xs []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, x := range xs {
			if !yield(x) {
				return
			}
		}
	}
}

// reconcileRenderedDir reconciles the renderer-owned subtrees inside
// repoDir against the renderer's view: writes/updates files at the
// planned paths and prunes only the snapshot tree under rendered/.
// CHANGELOG/<YYYY>/<MM>/<ULID>__<device>.md fragments are append-only
// per-push artefacts and are never pruned (the plan requires the
// per-push audit trail to survive future pushes). The chunks/,
// MANIFEST.json, README.md and .gitattributes files belong to the
// transport, not the renderer, and are left untouched.
func reconcileRenderedDir(repoDir string, files []render.File) error {
	want := make(map[string]struct{}, len(files))
	for _, f := range files {
		want[f.Path] = struct{}{}
	}
	if err := pruneStaleRendered(repoDir, want); err != nil {
		return err
	}
	for _, f := range files {
		if f.Action == render.ActionDelete {
			continue
		}
		full := filepath.Join(repoDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := writeFileAtomic(full, f.Content); err != nil {
			return err
		}
	}
	return nil
}

// pruneStaleRendered walks the renderer-owned subtrees under repoDir
// (rendered/ and ontology/) and removes any file whose repo-relative POSIX
// path is not in want. CHANGELOG/ is intentionally outside the prune scope:
// per-push fragments are append-only history. A missing subtree is a no-op
// (first push).
func pruneStaleRendered(repoDir string, want map[string]struct{}) error {
	for _, root := range []string{renderedDirName, ontologyDirName} {
		if err := pruneStaleSubtree(repoDir, root, want); err != nil {
			return err
		}
	}
	return nil
}

// pruneStaleSubtree walks one renderer-owned subtree (rootName) under repoDir
// and removes every file whose repo-relative POSIX path is not in want.
func pruneStaleSubtree(repoDir, rootName string, want map[string]struct{}) error {
	root := filepath.Join(repoDir, rootName)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	var stale []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(repoDir, path)
		if rerr != nil {
			return fmt.Errorf("rel %s: %w", path, rerr)
		}
		posix := filepath.ToSlash(rel)
		// The render cursor lives inside rendered/ but is written by
		// the transport, not the renderer, so it never appears in
		// `want`. Keep it across reconciles.
		if posix == renderedDirName+"/.cursor.json" {
			return nil
		}
		if _, ok := want[posix]; !ok {
			stale = append(stale, path)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", rootName, walkErr)
	}
	for _, s := range stale {
		if err := os.Remove(s); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale %s: %w", rootName, err)
		}
	}
	return nil
}

// writeFileAtomic writes content to path via temp+rename so a
// concurrent reader never sees a half-written file. Same idiom used
// by writeManifest in this package.
func writeFileAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
			// Best-effort: a leftover temp file is harmless and the
			// next run's CreateTemp picks a fresh suffix anyway.
			_ = rmErr
		}
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
