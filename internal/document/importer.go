package document

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// allDocumentPredicates is the full set of predicates managed by the importer
// (both the default wikilink predicates and the typed document-relation set).
// ForgottenEdgeEvents and existingEdgeTargets filter by this set so that typed
// edges are forgotten when their wikilink is removed and are never double-emitted
// on re-import -- the same invariants the original mentions/links_to pair had.
//
//nolint:gochecknoglobals // process-lifetime predicate slice, derived from documentPredicates map once at init
var allDocumentPredicates = func() []string {
	out := make([]string, 0, len(documentPredicates))
	for p := range documentPredicates {
		out = append(out, p)
	}
	return out
}()

// observationStateKey builds the Fact.StateKey for one observation. The key has
// three dot-separated segments: "timeless" (one of the seven mem0 categories;
// vault observations are durable, author-curated notes that do not expire), the
// sanitized category from the `- [category] text` bullet (lowercased,
// space-stripped, filtered to [a-z0-9_]; empty falls back to "uncategorized"),
// and a short content discriminator derived from the value.
//
// The discriminator is essential: an observation is a SET-valued statement — a
// note routinely carries many "[fact]" lines — so every line of the same
// category must land in its OWN fact slot. Sharing one timeless.<category> slot
// would make the supersede pipeline (reconcileFactSlot) close all but the last,
// silently dropping every earlier observation. Hashing the value also keeps the
// projection idempotent: an identical re-assert maps to the same slot, while a
// distinct value maps to a distinct slot, independent of source order.
//
// Examples: ("Decision","ship Friday") -> "timeless.decision.<8hex>".
func observationStateKey(category, value string) string {
	var b strings.Builder
	b.WriteString("timeless.")
	b.WriteString(categorySegment(category))
	// Each StateKey segment must start with a letter ([a-z][a-z0-9_]*), so the
	// hex discriminator is prefixed with a constant letter "h". The discriminator
	// hashes the NORMALIZED category alongside the value. Hashing the value alone
	// would not separate two distinct categories that sanitize to the same lossy
	// segment (e.g. "a-b" and "ab" both -> "ab", or "123" and "c123" both ->
	// "c123"): same segment + same value would share one slot and the supersede
	// pipeline would silently drop one observation. Folding the normalized
	// category in keeps such slots distinct while an identical re-assert (same
	// category modulo case/whitespace, same value) still maps to one slot.
	//
	// NOTE: this discriminator is a deliberate, breaking change to the observation
	// StateKey scheme. Facts written under an older value-only discriminator have
	// different keys and are not superseded by a re-import; per the pre-prod
	// "clean cuts, no backcompat" policy there is no migration — existing vaults
	// are re-imported into a fresh store rather than re-keyed in place.
	b.WriteString(".h")
	norm := strings.ToLower(strings.TrimSpace(category))
	sum := sha256.Sum256([]byte(norm + "\x00" + value))
	b.WriteString(hex.EncodeToString(sum[:4])) // 8 hex chars: ample to separate one note's observations
	return b.String()
}

// categorySegment sanitizes an author-supplied bullet category into a single
// StateKey segment: lowercased, filtered to [a-z0-9_], and guaranteed to start
// with a letter so it matches the schema pattern ^[a-z][a-z0-9_]*. A category
// that is empty after filtering falls back to "uncategorized"; one that starts
// with a digit or underscore (e.g. an index line like "- [0035 migration](...)")
// is prefixed with "c" so the whole import batch is not rejected.
//
// The filter is intentionally lossy: distinct categories can map to the same
// segment (e.g. "a-b" and "ab", or "123" and "c123"). The human-readable
// segment is therefore not relied on for slot identity; observationStateKey
// folds the normalized category into the discriminator so two categories that
// collide on one segment never share a slot.
func categorySegment(category string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(category)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "uncategorized"
	}
	if s[0] < 'a' || s[0] > 'z' {
		return "c" + s
	}
	return s
}

// observationContentHash computes the dedup key for a Fact derived from an
// observation. It delegates to ontology.ObservationContentHash, the single
// source of truth shared with the dispatcher's factRehomed handler so that a
// re-imported observation fact and a re-homed one hash identically and dedup
// against the same open slot.
func observationContentHash(subjectUID, stateKey, value string) string {
	return ontology.ObservationContentHash(subjectUID, stateKey, value)
}

// Appender is the write surface the importer needs: append events to the log.
// Satisfied by *kernel.FileEventLog (and the orchestrator's Log).
type Appender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// ProseExtractor is the optional LLM-backed extraction surface the importer
// uses to pull named entities from document bodies. Satisfied by
// *extractor.Extractor (via ProseExtractorAdapter). Nil is the documented
// "extraction disabled" sentinel: import works without it (only wikilinks and
// frontmatter links produce edges).
//
// Implementations must:
//   - Be safe for concurrent use (the vault watcher may drive multiple
//     documents concurrently).
//   - Respect the chat-concurrency semaphore inside the ChatProvider so
//     simultaneous calls queue behind the provider's real limit instead of
//     spawning unbounded goroutines that pile up in Ollama's input buffer.
//   - Return quickly when ctx is cancelled: the goroutine budget tracks these
//     calls and the importer does not join them before returning to the caller.
type ProseExtractor interface {
	// ExtractProse runs LLM extraction on text and returns grounded entity
	// mentions. docUID is the document's own entity UID; implementations may
	// use it as a stable "episode-like" dedup key.
	ExtractProse(ctx context.Context, docUID, text string) ([]ontology.Mention, error)
}

// Reader is the read surface the importer needs for content-hash dedup,
// kind-agnostic wikilink resolution, duplicate-edge avoidance, and the
// cross-import reconcile pass. Satisfied by *navigate.Navigator.
type Reader interface {
	GetDocumentByPath(ctx context.Context, path string) (navigate.Document, error)
	GetDocumentByPathScope(ctx context.Context, path, scope string) (navigate.Document, error)
	GetDocumentByUID(ctx context.Context, uid string) (navigate.Document, error)
	ListEntities(ctx context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error)
	EntityRelations(ctx context.Context, uid string, opts navigate.RelationFilter) ([]navigate.Relation, error)
	DocumentsWithUnresolvedLinks(ctx context.Context) ([]navigate.UnresolvedLinkDoc, error)
}

// Importer turns markdown vault files into event-sourced documents intertwined
// with the structured graph. One import is a four-stage batch:
//
//  1. dedup    — skip files whose content hash already matches the projection.
//  2. entities — build one EntityKindDocument event per new/changed file, so
//     every document node exists (in batch order) before any edge cites it.
//  3. revisions+edges — resolve each file's wikilinks and frontmatter links
//     against the projection overlaid with the staged batch (batchReader); build
//     a documentRevision (carrying the dangling remainder) plus
//     mentions/links_to relation edges. Stages 2+3 are appended in ONE atomic
//     batch — see importBatch.
//  4. reconcile — re-resolve every still-dangling link across the whole vault,
//     emitting edges that this import's new nodes just made resolvable.
//  5. prose extraction (async, non-fatal) — when a ProseExtractor is wired,
//     each changed document's body is fed to the LLM extractor in a detached
//     goroutine. Resolved mentions produce additional "mentions" edges deduped
//     against the wikilink/frontmatter edges already in the log. The extraction
//     goroutine never blocks ImportContent/ImportDir from returning.
//
// Resolution uses four lanes (see resolve.go):
//   - Exact case-insensitive display-name match (always active).
//   - Vault path match (always active).
//   - Unique whole-word partial-name match (always active; [[Rosie]] →
//     "Rosie Cotton", ambiguous matches stay dangling).
//   - Semantic name-embedding via SemanticNameResolver (optional; wired by
//     WithSemanticNameResolver when the embedder+vector layer are available).
type Importer struct {
	appender Appender
	reader   Reader
	// resolveReader is the read view used for wikilink / frontmatter NAME and
	// PATH resolution. It is draft-blind (AllScopes: project/personal/team) so a
	// [[Link]] in a non-draft document can never resolve to a draft-scoped entity
	// or document — which would leak a private draft into a shared scope's graph
	// as a cross-scope edge. Dedup (GetDocumentByPathScope), reconcile discovery
	// (DocumentsWithUnresolvedLinks) and edge/relation reads keep using the full
	// four-scope reader. Import batches are single-scope (one scope per
	// ImportContent/ImportDir), so the batchReader overlay never stacks a
	// different scope's staged docs on top of this view.
	resolveReader Reader
	log           *zap.Logger
	newUID        func() (string, error)
	now           func() time.Time
	proseExtract  ProseExtractor       // nil = extraction disabled
	snr           SemanticNameResolver // nil = semantic lane disabled
	binder        SelfBinder           // nil = no self->principal rebinding

	// proseLocks serializes the per-document prose-extraction read-then-append
	// so two concurrent extractions of the SAME document (watcher + MCP edit,
	// or back-to-back edits) cannot both observe the pre-append edge set and
	// both emit the same mentions edge. Keyed by document UID; entries are
	// reaped when their refcount drops to zero so the map does not grow with
	// every document ever imported.
	proseLockMu sync.Mutex
	proseLocks  map[string]*proseLock

	// pathLocks serializes the whole stage→append of ImportContent per vault
	// path. Two concurrent ingests of the SAME new path (the watcher's fsnotify
	// delivery racing a direct ReconcilePath, or a writeback materialization
	// racing a user edit) would otherwise both observe "no document at this
	// path", both mint a FRESH document UID — so the per-UID proseLock can't
	// coalesce them — and both append, colliding on the documents.path UNIQUE
	// constraint. Keyed by clean vault path; reaped at refcount zero like
	// proseLocks so the map does not grow with every path ever imported.
	pathLockMu sync.Mutex
	pathLocks  map[string]*proseLock
}

// proseLock is a per-document mutex with a refcount so idle entries can be
// reaped from Importer.proseLocks once no goroutine holds or waits on them.
type proseLock struct {
	mu   sync.Mutex
	refs int
}

// NewImporter builds an Importer. Log may be nil (a no-op logger is used).
func NewImporter(appender Appender, reader Reader, log *zap.Logger) *Importer {
	if log == nil {
		log = zap.NewNop()
	}
	// ulid.Monotonic is NOT safe for concurrent use. ImportContent can be
	// driven concurrently (the vault watcher fires a reconcile for the same
	// new path on both the fsnotify Run goroutine and a direct ReconcilePath
	// caller), and two new-document imports both reach newUID before the
	// event-log writer lock serializes them. Guard the entropy source so UID
	// minting is race-free.
	entropy := ulid.Monotonic(rand.Reader, 0)
	var entropyMu sync.Mutex
	// Derive the draft-blind resolution view. A *navigate.Navigator can be cheaply
	// rescoped; a test fake that cannot is used as-is (its own scope span governs).
	resolveReader := reader
	if rs, ok := reader.(interface {
		WithScopes(kernel.ScopeSet) *navigate.Navigator
	}); ok {
		resolveReader = rs.WithScopes(kernel.AllScopes())
	}
	return &Importer{
		appender:      appender,
		reader:        reader,
		resolveReader: resolveReader,
		log:           log,
		newUID: func() (string, error) {
			entropyMu.Lock()
			id, err := ulid.New(ulid.Now(), entropy)
			entropyMu.Unlock()
			if err != nil {
				return "", fmt.Errorf("mint ulid: %w", err)
			}
			return id.String(), nil
		},
		now:        func() time.Time { return time.Now().UTC() },
		proseLocks: make(map[string]*proseLock),
		pathLocks:  make(map[string]*proseLock),
	}
}

// acquirePathLock returns the per-path mutex (creating it if absent) with its
// refcount bumped. The caller must call releasePathLock with the same path
// after unlocking. This serializes the stage→append of ImportContent per path
// so concurrent ingests of the same path cannot double-create the document.
func (im *Importer) acquirePathLock(rel string) *proseLock {
	im.pathLockMu.Lock()
	defer im.pathLockMu.Unlock()
	pl := im.pathLocks[rel]
	if pl == nil {
		pl = &proseLock{}
		im.pathLocks[rel] = pl
	}
	pl.refs++
	return pl
}

// releasePathLock drops the refcount for rel's lock and reaps the map entry
// when no goroutine is holding or waiting on it.
func (im *Importer) releasePathLock(rel string) {
	im.pathLockMu.Lock()
	defer im.pathLockMu.Unlock()
	pl := im.pathLocks[rel]
	if pl == nil {
		return
	}
	pl.refs--
	if pl.refs <= 0 {
		delete(im.pathLocks, rel)
	}
}

// acquireProseLock returns the per-document mutex (creating it if absent) with
// its refcount bumped. The caller must call releaseProseLock with the same UID
// after unlocking. This serializes the read-then-append in runProseExtraction
// per document so concurrent extractions of the same doc cannot both emit a
// duplicate mentions edge.
func (im *Importer) acquireProseLock(docUID string) *proseLock {
	im.proseLockMu.Lock()
	defer im.proseLockMu.Unlock()
	pl := im.proseLocks[docUID]
	if pl == nil {
		pl = &proseLock{}
		im.proseLocks[docUID] = pl
	}
	pl.refs++
	return pl
}

// releaseProseLock drops the refcount for docUID's lock and reaps the map entry
// when no goroutine is holding or waiting on it.
func (im *Importer) releaseProseLock(docUID string) {
	im.proseLockMu.Lock()
	defer im.proseLockMu.Unlock()
	pl := im.proseLocks[docUID]
	if pl == nil {
		return
	}
	pl.refs--
	if pl.refs <= 0 {
		delete(im.proseLocks, docUID)
	}
}

// WithProseExtractor attaches an optional LLM-backed extractor. When set, each
// changed document's body is run through the extractor asynchronously after the
// deterministic import batch commits; resolved mentions produce additional
// "mentions" edges deduped against wikilink/frontmatter edges. Returns im so
// calls can be chained:
//
//	im := document.NewImporter(log, nav, logger).WithProseExtractor(ext)
func (im *Importer) WithProseExtractor(e ProseExtractor) *Importer {
	im.proseExtract = e
	return im
}

// WithSemanticNameResolver attaches an optional semantic name resolver. When
// set, wikilink resolution gains a third lane (after exact display-name match
// and vault path match): the target string is embedded and the nearest-neighbour
// search over entities_vec WHERE vector_type='name' is used to resolve partial
// or informal names such as [[Rosie]] → "Rosie Cotton". The lane is conservative:
// it only fires when the top hit exceeds the similarity threshold AND the margin
// over the runner-up exceeds the margin threshold; otherwise the link remains
// dangling (reconcile resolves it when a later import satisfies it exactly).
//
// The semantic lane runs synchronously inside the import batch so resolution
// outcome is included in the atomic entity+revision+edge append. This preserves
// the atomicity guarantee and keeps the import idempotent: unchanged content
// hash → file skipped → no embedding call. Embedder errors are non-fatal and
// leave the link dangling.
//
// The reconcile pass also benefits: when reconcile calls resolveWikilinks for
// a previously-dangling link, the semantic lane is active and can match an
// entity that arrived since the original import.
//
// Returns im so calls can be chained:
//
//	im := document.NewImporter(log, nav, logger).WithSemanticNameResolver(snr)
func (im *Importer) WithSemanticNameResolver(snr SemanticNameResolver) *Importer {
	im.snr = snr
	return im
}

// SemanticResolver returns the attached semantic name resolver, or nil when the
// lane is disabled. The daemon wires the resolver onto the importer after the
// navigation tools are registered, so read-side consumers (e.g. the lint tool)
// must read it lazily at invoke time rather than capturing it at construction.
//
//nolint:ireturn // returns the SemanticNameResolver interface; nil is the documented disabled sentinel.
func (im *Importer) SemanticResolver() SemanticNameResolver { return im.snr }

// SelfBinder resolves the device-local self->principal binding so a document's
// "entity: self" frontmatter attribution resolves to the concrete principal
// UID — shareable across a team — rather than the local-only "self" literal.
// Satisfied by *config.SelfBinder; a nil binder disables rebinding.
type SelfBinder interface {
	SelfPrincipal(ctx context.Context) (string, bool)
}

// WithSelfBinder attaches the self->principal resolver. Returns im so calls can
// be chained. The daemon wires *config.SelfBinder here; tests omit it (nil),
// leaving the "self" frontmatter target verbatim.
func (im *Importer) WithSelfBinder(b SelfBinder) *Importer {
	im.binder = b
	return im
}

// Result summarises an import run.
type Result struct {
	Imported   int           `json:"imported"`   // new or changed documents written
	Skipped    int           `json:"skipped"`    // unchanged (content-hash match)
	Edges      int           `json:"edges"`      // resolved wikilink edges emitted
	Unresolved int           `json:"unresolved"` // links left dangling this run
	Reconciled int           `json:"reconciled"` // dangling links resolved across imports
	Documents  []ImportedDoc `json:"documents,omitempty"`
	// UnresolvedTargets lists the wikilink targets left dangling this run (the
	// text inside [[…]] that matched no entity or document). Surfacing the names
	// — not just the count — lets a caller see WHICH links failed to wire so it
	// can fix a typo or create the target; they reconcile automatically when the
	// target later appears.
	UnresolvedTargets []string `json:"unresolvedTargets,omitempty"`
}

// ImportedDoc records one document touched by an import.
type ImportedDoc struct {
	Path        string `json:"path"`
	DocumentUID string `json:"documentUid"`
	RevisionUID string `json:"revisionUid"`
	Changed     bool   `json:"changed"` // false when skipped as unchanged
}

// isMarkdownExt reports whether a file extension marks a markdown document.
func isMarkdownExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

// ImportDir imports every markdown file under root (recursively) into scope.
// Root may also be a single file. Vault-relative paths use forward slashes
// regardless of OS. An empty scope coerces to kernel.DefaultScope (personal),
// matching the event-log write default.
func (im *Importer) ImportDir(ctx context.Context, scope kernel.Scope, root string) (Result, error) {
	if scope == "" {
		scope = kernel.DefaultScope
	}
	info, err := os.Stat(root)
	if err != nil {
		return Result{}, fmt.Errorf("stat %q: %w", root, err)
	}

	var files []string
	base := root
	if info.IsDir() {
		files, err = collectMarkdownFiles(root)
		if err != nil {
			return Result{}, err
		}
	} else {
		// Single-file imports get the same contract as the directory walk:
		// markdown only.
		if !isMarkdownExt(filepath.Ext(root)) {
			return Result{}, fmt.Errorf("import %q: not a markdown file (want .md or .markdown)", root)
		}
		files = []string{root}
		base = filepath.Dir(root)
	}

	pending, err := im.loadPending(ctx, scope, base, files)
	if err != nil {
		return Result{}, err
	}

	// A single-file import addresses the vault by the file's basename. If a
	// different document already occupies that path, this import becomes a new
	// revision of it — filesystem copy-into-vault semantics, with the previous
	// content preserved in the event log. Surface the overwrite so an operator
	// importing two unrelated files that share a basename sees what happened.
	if !info.IsDir() && len(pending) == 1 && pending[0].existed && pending[0].changed {
		im.log.Warn("document: single-file import updates the existing document at this vault path (new revision; prior content stays in the event log)",
			zap.String("path", pending[0].path),
			zap.String("documentUID", pending[0].documentUID))
	}
	return im.importBatch(ctx, pending)
}

// collectMarkdownFiles walks root recursively and returns every markdown file
// path, skipping hidden directories (.git, .obsidian, ...).
func collectMarkdownFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if isHiddenDir(d.Name()) && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if isMarkdownExt(filepath.Ext(p)) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %q: %w", root, err)
	}
	return files, nil
}

// pendingDoc is one file staged for import: its vault-relative path, parsed
// view, the stable document UID (reused on re-import, minted on first sight),
// whether its content actually changed, whether a document already existed at
// the path before this import, and (when it did) the revision it supersedes.
type pendingDoc struct {
	path            string
	parsed          Parsed
	documentUID     string
	changed         bool
	existed         bool
	prevRevisionUID string
	// scope is the storage scope every event this document emits is stamped with
	// (entity, revision, edges, observation facts, forgets). Set by stage from
	// the caller's scope; because dedup is scope-exact (GetDocumentByPathScope), a
	// document's scope is fixed at creation — the same relative path in a
	// different scope is a different document.
	scope kernel.Scope
}

// loadPending reads + parses each file, resolves its stable document UID by
// path (reusing the existing UID on re-import), and flags whether the content
// changed since the last revision.
func (im *Importer) loadPending(ctx context.Context, scope kernel.Scope, base string, files []string) ([]pendingDoc, error) {
	out := make([]pendingDoc, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // operator-supplied vault path
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", f, err)
		}
		rel, err := vaultRelPath(base, f)
		if err != nil {
			return nil, err
		}
		pd, sErr := im.stage(ctx, scope, rel, raw)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, pd)
	}
	return out, nil
}

// stage parses raw content destined for vault path rel in scope, resolves its
// stable document UID (reusing the existing UID when the path is already
// occupied IN THAT SCOPE), and records change + lineage bookkeeping. Shared by
// the file import path (loadPending) and the in-memory write path
// (ImportContent). Dedup is scope-exact (GetDocumentByPathScope): the same
// relative path in a different scope stages a NEW document, so a document's
// scope is fixed at creation and can never silently flip.
func (im *Importer) stage(ctx context.Context, scope kernel.Scope, rel string, raw []byte) (pendingDoc, error) {
	parsed, perr := Parse(raw)
	if perr != nil {
		// Body + wikilinks still parsed; ingest the body and drop metadata.
		im.log.Warn("document: metadata parse failed, importing body only",
			zap.String("path", rel), zap.Error(perr))
	}

	pd := pendingDoc{path: rel, parsed: parsed, changed: true, scope: scope}
	existing, gerr := im.reader.GetDocumentByPathScope(ctx, rel, string(scope))
	switch {
	case gerr == nil:
		pd.documentUID = existing.DocumentUID
		pd.changed = existing.ContentHash != parsed.ContentHash
		pd.existed = true
		pd.prevRevisionUID = existing.RevisionUID
	case errors.Is(gerr, navigate.ErrNotFound):
		uid, uErr := im.newUID()
		if uErr != nil {
			return pendingDoc{}, uErr
		}
		pd.documentUID = uid
	default:
		return pendingDoc{}, fmt.Errorf("lookup document %q: %w", rel, gerr)
	}
	return pd, nil
}

// ImportContent ingests one in-memory document at the given vault-relative
// path into scope. This is the write surface (writeDocument / editDocument /
// $EDITOR re-ingest): it runs through the exact same import pipeline as a file
// import, so agent-written documents get identical content-hash dedup, entity
// creation, wikilink edge resolution, removed-link forgetting, and
// cross-document reconcile semantics. The path is normalised via
// CleanVaultPath (".md" default extension; vault-escape rejected). An empty
// scope coerces to kernel.DefaultScope (personal), matching the event-log
// write default.
func (im *Importer) ImportContent(ctx context.Context, scope kernel.Scope, path string, raw []byte) (Result, error) {
	if scope == "" {
		scope = kernel.DefaultScope
	}
	rel, err := CleanVaultPath(path)
	if err != nil {
		return Result{}, err
	}
	// Serialize stage→append per path: the staging read ("is there already a
	// document at this path?") and the append must be atomic against a
	// concurrent ingest of the same path, or both calls mint a fresh document
	// UID and collide on the documents.path UNIQUE constraint. Different paths
	// take different locks and never block each other.
	pl := im.acquirePathLock(rel)
	pl.mu.Lock()
	defer func() {
		pl.mu.Unlock()
		im.releasePathLock(rel)
	}()
	pd, err := im.stage(ctx, scope, rel, raw)
	if err != nil {
		return Result{}, err
	}
	return im.importBatch(ctx, []pendingDoc{pd})
}

// importBatch runs the import over the staged documents. Stages 2 and 3
// (entities, revisions+edges) are BUILT separately but appended in ONE atomic
// batch: appending entities first in their own call would leave an orphaned
// document entity behind when the later revision append failed — a retry
// would miss the path (no documents row), mint a fresh document UID, and
// strand a duplicate EntityKindDocument whose display name makes exact-name
// wikilinks ambiguous forever. Intra-batch links still resolve because
// resolution runs against a batchReader overlay that exposes the staged docs
// as if the batch had already applied.
func (im *Importer) importBatch(ctx context.Context, pending []pendingDoc) (Result, error) {
	res := Result{}

	changed := make([]pendingDoc, 0, len(pending))
	for i := range pending {
		pd := &pending[i]
		if !pd.changed {
			res.Skipped++
			res.Documents = append(res.Documents, ImportedDoc{Path: pd.path, DocumentUID: pd.documentUID})
			continue
		}
		changed = append(changed, *pd)
	}

	// Stage 2: one EntityKindDocument event per changed document, so every
	// document node exists (in batch order) before the edges that cite it.
	events, err := im.entityEvents(changed)
	if err != nil {
		return Result{}, err
	}

	// Stage 3: resolve links against the overlay, then build revisions and
	// their resolved edges. Appended together with the entities below. The
	// overlay wraps the draft-blind resolveReader so a non-draft doc's [[links]]
	// never resolve to a draft target (the batch is single-scope, so the staged
	// overlay adds only same-scope docs on top).
	revAndEdges, err := im.revisionAndEdgeEvents(ctx, newBatchReader(im.resolveReader, changed), changed, &res)
	if err != nil {
		return Result{}, err
	}
	events = append(events, revAndEdges...)

	if len(events) > 0 {
		if _, err := im.appender.Append(ctx, events); err != nil {
			return Result{}, fmt.Errorf("append document import batch: %w", err)
		}
	}

	// Stage 4: pick up links that this import's new nodes made resolvable.
	reconciled, err := im.reconcile(ctx, batchUIDs(changed))
	if err != nil {
		return Result{}, err
	}
	res.Reconciled = reconciled

	// Stage 5 (async, non-fatal): prose extraction via LLM. Fires only for
	// changed documents when a ProseExtractor is wired. Each document gets its
	// own goroutine so a slow extraction does not block the others; the
	// chat-concurrency semaphore inside the ChatProvider limits simultaneous
	// Ollama calls at the provider level. The goroutines use a detached context
	// (context.WithoutCancel) so a client cancel does not abort an in-progress
	// LLM call mid-extraction — the same discipline the FileEventLog uses for
	// its applyExtraction path.
	if im.proseExtract != nil {
		im.scheduleProseExtraction(ctx, changed, res.Documents)
	}

	return res, nil
}

// scheduleProseExtraction launches one background goroutine per changed
// document. Each goroutine extracts entity mentions from the document body,
// resolves them to canonical entity UIDs, and appends "mentions" relation
// events deduped against existing edges from wikilinks and frontmatter links.
// Goroutine errors are warn-logged; they never fail the import.
func (im *Importer) scheduleProseExtraction(ctx context.Context, changed []pendingDoc, imported []ImportedDoc) {
	// Build a revUID index so each goroutine can anchor provenance to the
	// revision event that was just appended. ImportedDoc carries the revision UID.
	revByDoc := make(map[string]string, len(imported))
	for _, d := range imported {
		if d.RevisionUID != "" {
			revByDoc[d.DocumentUID] = d.RevisionUID
		}
	}
	bgCtx := context.WithoutCancel(ctx)
	for i := range changed {
		pd := changed[i]
		revUID := revByDoc[pd.documentUID]
		if revUID == "" {
			continue
		}
		go func() {
			if err := im.runProseExtraction(bgCtx, &pd, revUID); err != nil {
				im.log.Warn("Document: prose extraction failed (document still durable)",
					zap.String("path", pd.path),
					zap.String("documentUID", pd.documentUID),
					zap.Error(err),
				)
			}
		}()
	}
}

// runProseExtraction is the per-document extraction body run inside the
// goroutine launched by scheduleProseExtraction. It calls the ProseExtractor,
// resolves each returned mention to a canonical entity UID, and appends
// "mentions" relation events for new edges (deduped against existing ones).
func (im *Importer) runProseExtraction(ctx context.Context, pd *pendingDoc, revUID string) error {
	mentions, err := im.proseExtract.ExtractProse(ctx, pd.documentUID, pd.parsed.Body)
	if err != nil {
		return fmt.Errorf("prose extract %q: %w", pd.path, err)
	}
	if len(mentions) == 0 {
		return nil
	}

	// Serialize the read-then-append per document. Without this, two concurrent
	// extractions of the same document (watcher + MCP edit, back-to-back edits)
	// can both read the pre-append edge set and both append the same mentions
	// edge. The LLM call above runs OUTSIDE the lock so the (slow) extraction of
	// different documents still proceeds concurrently; only the short
	// dedup-and-append critical section is serialized per doc.
	pl := im.acquireProseLock(pd.documentUID)
	pl.mu.Lock()
	defer func() {
		pl.mu.Unlock()
		im.releaseProseLock(pd.documentUID)
	}()

	// Abandon the extraction if the document was deleted while the (slow,
	// out-of-lock) LLM call ran. The delete path takes this same per-document
	// lock and tombstones the document + drops its entity node, so once we hold
	// the lock the deleted state is stable. Appending now would resurrect a
	// prose mentions edge from a non-existent document (the create-then-delete
	// race): the edge would dangle off a tombstoned subject with no future
	// extraction pass to reconcile it. GetDocumentByUID filters deleted = 0, so
	// ErrNotFound here means "deleted (or never projected)" — skip the append.
	// A non-not-found read error means liveness is UNKNOWN: do not append (that
	// could resurrect an edge against a since-deleted doc); surface the error so
	// the caller can retry rather than silently proceeding on a failed check.
	if _, err := im.reader.GetDocumentByUID(ctx, pd.documentUID); err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("liveness check before prose append %q: %w", pd.path, err)
	}

	// Load existing edges once (wikilink + frontmatter + prior extractions) so we
	// can skip any mention that already has a mentions edge, plus the set of names
	// an explicit (non-prose) wikilink already claims so we defer to those.
	existing, explicitNames, err := im.existingDocEdges(ctx, pd.documentUID)
	if err != nil {
		return fmt.Errorf("load existing edges for prose extraction %q: %w", pd.path, err)
	}

	events := make([]kernel.Event, 0, len(mentions))
	seen := make(map[string]struct{})
	for i := range mentions {
		m := mentions[i]
		// Defer to an explicit, user-authored wikilink: if the document already
		// links this name deterministically (e.g. [[Mithrilmail]] resolved to the
		// person), do not let this async LLM mention add a competing edge to a
		// different same-named entity (the project). The user's link is
		// authoritative for that name; the prose mention is dropped.
		if _, claimed := explicitNames[nameKey(m.DisplayName)]; claimed {
			continue
		}
		// Resolve the mention display name to a canonical entity UID via the
		// same resolveTarget lane body wikilinks use. Kind-gated: only resolve
		// to entities whose kind matches the mention's kind to prevent
		// cross-type collisions (e.g. a person named "Berlin" not merging with
		// the place "Berlin").
		uid, _, ok := resolveTargetKindGated(ctx, im.reader, pd.documentUID, m.DisplayName, string(m.Kind))
		if !ok {
			continue
		}
		key := predicateMentions + "\x00" + uid
		if _, dup := existing[key]; dup {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		ev, eErr := im.relationEvent(pd.scope, pd.documentUID, resolvedLink{
			objectUID:  uid,
			predicate:  predicateMentions,
			raw:        m.DisplayName,
			provenance: proseProvenancePrefix + m.DisplayName,
		}, revUID)
		if eErr != nil {
			return eErr
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		return nil
	}
	if _, err := im.appender.Append(ctx, events); err != nil {
		return fmt.Errorf("append prose extraction edges for %q: %w", pd.path, err)
	}
	return nil
}

// entityEvents builds one EntityKindDocument event per changed document. The
// entity UID is the document UID, unifying the document's structured-graph node
// with its revision stream. Re-emitting an existing entity is an idempotent
// merge in the dispatcher. The caller appends these together with the revision
// and edge events in one atomic batch.
func (im *Importer) entityEvents(changed []pendingDoc) ([]kernel.Event, error) {
	if len(changed) == 0 {
		return nil, nil
	}
	now := im.now()
	events := make([]kernel.Event, 0, len(changed))
	for i := range changed {
		pd := &changed[i]
		entity := ontology.Entity{
			Name:             "entities/" + pd.documentUID,
			UID:              pd.documentUID,
			Kind:             ontology.EntityKindDocument,
			DisplayName:      displayName(pd),
			Confidence:       1.0,
			ResolutionStatus: ontology.ResolutionConfirmed,
			CreateTime:       now,
		}
		payload, err := json.Marshal(entity)
		if err != nil {
			return nil, fmt.Errorf("marshal document entity %q: %w", pd.path, err)
		}
		events = append(events, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEntity),
			Scope:      pd.scope,
			Source:     "document:import",
			Confidence: 1.0,
			Payload:    payload,
		})
	}
	return events, nil
}

// appendRevisionsAndEdges resolves each changed document's wikilinks, emits the
// documentRevision events (carrying the unresolved remainder), and emits the
// resolved mentions/links_to edges with each revision's UID as provenance.
// Edges whose wikilink was REMOVED from the body are forgotten in the same
// batch (relationForgotten), so backlinks and reads always mirror the latest
// body rather than accumulating every link the document ever had.
//
// The caller appends these events together with the batch's entity events in
// ONE Append call. A batch commits atomically (file_eventlog rolls earlier
// buckets back when a later one fails), so the entities, revisions, and edges
// become durable together or not at all. Appending any of them separately
// would open permanent consistency holes: a durable revision (content_hash)
// with lost edges makes a retry skip the file as unchanged; a durable entity
// without its revision makes a retry mint a fresh document UID and strand a
// duplicate document entity. Revision event UIDs are minted up front (the log
// honours caller-supplied ULIDs) so the edge events can reference their
// revision as provenance inside the same batch.
//
// Resolution runs against the caller-supplied reader — a batchReader overlay
// that exposes the staged documents as if the batch had already applied, so
// intra-batch [[links]] resolve without the entities being durable yet.
func (im *Importer) revisionAndEdgeEvents(ctx context.Context, r Reader, changed []pendingDoc, res *Result) ([]kernel.Event, error) {
	if len(changed) == 0 {
		return nil, nil
	}

	events := make([]kernel.Event, 0, len(changed))
	for i := range changed {
		pd := &changed[i]

		// Log unknown relTypes before resolution so the operator can see which
		// typed-link prefixes in their vault are not in the registered predicate
		// set and will fall back to the default mentions/links_to edge.
		for _, wl := range pd.parsed.Wikilinks {
			if wl.RelType != "" && !isKnownDocumentPredicate(wl.RelType) {
				im.log.Debug("document: unknown wikilink relType; falling back to default predicate",
					zap.String("path", pd.path),
					zap.String("relType", wl.RelType),
					zap.String("target", wl.Target),
				)
			}
		}

		resolved, unresolved := resolveWikilinks(ctx, r, im.snr, pd.documentUID, pd.parsed.Wikilinks)
		// Resolve frontmatter typed links (about: / entity: self) and merge with
		// body-wikilink results. Unresolved frontmatter links join the dangling
		// snapshot for the reconcile pass.
		fmResolved, fmUnresolved := resolveFrontmatterLinks(ctx, r, im.binder, pd.documentUID, pd.parsed.FrontmatterLinks)
		resolved = append(resolved, fmResolved...)
		unresolved = append(unresolved, fmUnresolved...)
		// Dedup merged resolved set so a frontmatter link duplicating a body
		// wikilink (same predicate + objectUID) does not emit two relation events.
		resolved = dedupResolvedLinks(resolved)
		// Edges counts the TOTAL resolved wikilinks in the current revision body
		// (whole-document snapshot, not a delta vs. the previous revision). Counting
		// here — before docEdgeEvents deduplicates against already-active edges —
		// ensures re-writing a document with N resolved links always reports Edges=N
		// regardless of how many were already in the event log.
		res.Edges += len(resolved)
		res.Unresolved += len(unresolved)
		for _, wl := range unresolved {
			res.UnresolvedTargets = append(res.UnresolvedTargets, wl.Target)
		}

		revUID, err := im.newUID()
		if err != nil {
			return nil, err
		}

		rev := ontology.DocumentRevision{
			DocumentUID:         pd.documentUID,
			Path:                pd.path,
			Title:               displayName(pd),
			Body:                pd.parsed.Body,
			Frontmatter:         pd.parsed.Frontmatter,
			FrontmatterRaw:      pd.parsed.FrontmatterRaw,
			ContentHash:         pd.parsed.ContentHash,
			Wikilinks:           unresolved, // payload carries the dangling remainder
			Observations:        pd.parsed.Observations,
			PreviousRevisionUID: prevRevisionPtr(pd.prevRevisionUID),
			Pinned:              pd.parsed.Pinned,
			Tags:                pd.parsed.Tags,
		}
		payload, err := json.Marshal(rev)
		if err != nil {
			return nil, fmt.Errorf("marshal documentRevision %q: %w", pd.path, err)
		}
		events = append(events, kernel.Event{
			UID:        revUID,
			Kind:       kernel.EventKind(ontology.KindDocumentRevision),
			Scope:      pd.scope,
			Source:     "document:import",
			Confidence: 1.0,
			Payload:    payload,
		})

		// The document's edge events follow its revision in the same batch, so
		// the dispatcher applies the revision before the edges that cite it. Every
		// companion event (edges, observation facts) carries the document's scope
		// so a document IS an entity, and its edges/observations never split across
		// scopes.
		edgeEvents, eErr := im.docEdgeEvents(ctx, pd.scope, pd.documentUID, resolved, revUID)
		if eErr != nil {
			return nil, eErr
		}
		events = append(events, edgeEvents...)

		// Observation facts: one Fact event per observation line. Emitted after
		// the revision event so dispatcher ordering matches the edge events.
		// aboutSubjectsOf filters the resolved links for predicateDescribes edges
		// and returns the about: subject UIDs; observations home on those instead
		// of the document (Part A).
		factEvents, fErr := im.observationFactEvents(ctx, pd.scope, pd.documentUID, revUID, pd.parsed.Observations, aboutSubjectsOf(resolved))
		if fErr != nil {
			return nil, fErr
		}
		events = append(events, factEvents...)

		res.Documents = append(res.Documents, ImportedDoc{
			Path:        pd.path,
			DocumentUID: pd.documentUID,
			RevisionUID: revUID,
			Changed:     true,
		})
		res.Imported++
	}
	return events, nil
}

// aboutSubjectsOf returns the object UIDs from links whose predicate is
// predicateDescribes (the about: frontmatter relation). These become the
// subject candidates for observation facts emitted by observationFactEvents.
func aboutSubjectsOf(links []resolvedLink) []string {
	var out []string
	for _, l := range links {
		if l.predicate == predicateDescribes {
			out = append(out, l.objectUID)
		}
	}
	return out
}

// observationInlineSubject scans one observation text line for [[Target]]
// wikilinks and returns the resolved entity UID only when EXACTLY ONE distinct
// target resolves (the rule-1 contract). It returns "" when no wikilink is
// present, none resolves, or two or more distinct targets resolve — the last
// case is ambiguous (e.g. "- [[Alice]] thanked [[MIT]]"), so the caller falls
// through to the about: frontmatter subjects rather than silently picking the
// first link. Repeated links to the same entity count as one subject. The
// selfUID guard (the document's own UID) prevents a self-edge being used as the
// override subject, matching the body-wikilink resolver's convention.
func observationInlineSubject(ctx context.Context, r Reader, selfUID, text string) string {
	masked := inlineCodeRe.ReplaceAllStringFunc(text, maskRun)
	subject := ""
	for _, m := range typedLinkRe.FindAllStringSubmatch(masked, -1) {
		// m[1]=prefix relType, m[2]=prefix confidence, m[3]=inner link text.
		wl, ok := parseWikilink(m[3], m[1], m[2])
		if !ok {
			continue
		}
		uid, _, ok := resolveTarget(ctx, r, selfUID, wl.Target)
		if !ok {
			continue
		}
		if subject != "" && uid != subject {
			// A second, distinct subject resolved — ambiguous, no inline override.
			return ""
		}
		subject = uid
	}
	return subject
}

// observationFactEvents builds Fact events for the parsed observations in one
// document revision. Subject selection follows these rules (evaluated in order):
//
//  1. Inline [[X]] wikilink inside the observation text: if exactly one target
//     resolves, that entity becomes the subject (overrides about: frontmatter).
//  2. aboutSubjectUIDs from the document's about: frontmatter (predicateDescribes
//     edges resolved by the importer):
//     - exactly one subject → SubjectUID = that subject's UID.
//     - multiple subjects → one Fact event is emitted per subject so the
//     observation attaches to every described entity.
//  3. Fallback: SubjectUID = docUID (current document entity; original behavior).
//
// observationContentHash includes the subjectUID so the same observation text
// homed to two different subjects yields two distinct, non-colliding fact rows.
// SourceEventUID always stays revUID to preserve documentRevision provenance.
func (im *Importer) observationFactEvents(ctx context.Context, scope kernel.Scope, docUID, revUID string, obs []ontology.Observation, aboutSubjectUIDs []string) ([]kernel.Event, error) {
	if len(obs) == 0 {
		return nil, nil
	}
	now := im.now()
	events := make([]kernel.Event, 0, len(obs))
	for _, ob := range obs {
		// Value carries the full observation text plus any trailing context, so a
		// consumer reading Fact.Value gets the complete picture without re-parsing
		// the DocumentRevision body. ob.Text is the verbatim observation line and
		// already contains its inline #tags (ob.Tags is parsed FROM that text by
		// observationTags), so we must NOT re-append ob.Tags — doing so produced a
		// duplicated "ship Friday #launch #launch".
		value := ob.Text
		if ob.Context != "" {
			value += " (" + ob.Context + ")"
		}
		// StateKey carries a value discriminator so multiple observations of the
		// same category each occupy their own fact slot (see observationStateKey).
		stateKey := observationStateKey(ob.Category, value)

		// Determine which subject UID(s) this observation attaches to.
		var subjects []string
		if inlineSub := observationInlineSubject(ctx, im.reader, docUID, ob.Text); inlineSub != "" {
			// Rule 1: inline [[X]] wikilink in the observation line overrides all.
			subjects = []string{inlineSub}
		} else if len(aboutSubjectUIDs) > 0 {
			// Rule 2: about: frontmatter subjects; one fact per subject.
			subjects = aboutSubjectUIDs
		} else {
			// Rule 3: fallback to the document itself.
			subjects = []string{docUID}
		}

		for _, subjectUID := range subjects {
			uid, err := im.newUID()
			if err != nil {
				return nil, err
			}
			f := ontology.Fact{
				Name:           "facts/" + uid,
				UID:            uid,
				SubjectUID:     subjectUID,
				StateKey:       stateKey,
				Value:          value,
				CreateTime:     now,
				SourceEventUID: revUID,
				ContentHash:    observationContentHash(subjectUID, stateKey, value),
			}
			payload, err := json.Marshal(f)
			if err != nil {
				return nil, fmt.Errorf("marshal observation fact: %w", err)
			}
			events = append(events, kernel.Event{
				Kind:       kernel.EventKind(ontology.KindFact),
				Scope:      scope,
				Source:     "document:import",
				Confidence: 1.0,
				Payload:    payload,
			})
		}
	}
	return events, nil
}

// reconcile re-resolves every still-dangling wikilink across the whole vault,
// emitting the edges this import's new nodes just made resolvable. Documents in
// skipUIDs (this batch) are skipped — their links were just resolved. Duplicate
// edges are avoided by cross-checking the relations table. Returns the count of
// reconciled edges emitted.
func (im *Importer) reconcile(ctx context.Context, skipUIDs map[string]struct{}) (int, error) {
	docs, err := im.reader.DocumentsWithUnresolvedLinks(ctx)
	if err != nil {
		return 0, fmt.Errorf("reconcile: list dangling docs: %w", err)
	}

	var edgeEvents []kernel.Event
	for _, doc := range docs {
		if _, skip := skipUIDs[doc.DocumentUID]; skip {
			continue
		}
		var links []ontology.WikiLink
		if uErr := json.Unmarshal([]byte(doc.UnresolvedJSON), &links); uErr != nil {
			im.log.Warn("document: reconcile decode unresolved links",
				zap.String("path", doc.Path), zap.Error(uErr))
			continue
		}
		// Draft-blind resolution (resolveReader), same as the initial import pass:
		// a reconciled edge for a non-draft doc must never point at a draft target.
		resolved, _ := resolveWikilinks(ctx, im.resolveReader, im.snr, doc.DocumentUID, links)
		if len(resolved) == 0 {
			continue
		}
		existing, lerr := im.existingEdgeTargets(ctx, doc.DocumentUID)
		if lerr != nil {
			return 0, lerr
		}
		for _, link := range resolved {
			if _, dup := existing[link.predicate+"\x00"+link.objectUID]; dup {
				continue
			}
			existing[link.predicate+"\x00"+link.objectUID] = struct{}{}
			// Anchor provenance to the document's latest revision EVENT (not the
			// document UID) so reconciled edges walk back to a real event exactly
			// like the edges emitted at import time. The reconciled edge carries
			// the RECONCILED document's own scope (not the batch's), so a personal
			// doc made resolvable by a team import still emits a personal edge.
			ev, eErr := im.relationEvent(kernel.Scope(doc.Scope), doc.DocumentUID, link, doc.RevisionUID)
			if eErr != nil {
				return 0, eErr
			}
			edgeEvents = append(edgeEvents, ev)
		}
	}
	if len(edgeEvents) == 0 {
		return 0, nil
	}
	if _, err := im.appender.Append(ctx, edgeEvents); err != nil {
		return 0, fmt.Errorf("reconcile: append edges: %w", err)
	}
	return len(edgeEvents), nil
}

// docEdgeEvents builds the relation events for one freshly revised document:
// new mentions/links_to edges for resolved wikilinks not already present
// (duplicate edges avoided via the relations table), plus relationForgotten
// events for active edges whose wikilink no longer appears in the body.
// The caller (revisionAndEdgeEvents) accumulates res.Edges before this call,
// so this function only builds events; it does not touch res.
func (im *Importer) docEdgeEvents(ctx context.Context, scope kernel.Scope, docUID string, resolved []resolvedLink, revisionUID string) ([]kernel.Event, error) {
	existing, err := im.existingEdgeTargets(ctx, docUID)
	if err != nil {
		return nil, err
	}
	events := make([]kernel.Event, 0, len(resolved))
	for _, link := range resolved {
		if _, dup := existing[link.predicate+"\x00"+link.objectUID]; dup {
			continue
		}
		existing[link.predicate+"\x00"+link.objectUID] = struct{}{}
		ev, eErr := im.relationEvent(scope, docUID, link, revisionUID)
		if eErr != nil {
			return nil, eErr
		}
		events = append(events, ev)
	}

	// Forget edges whose wikilink no longer appears in this revision's body,
	// so the doc's graph mirrors its latest content. Prose edges are preserved
	// (includeProse=false): the async extractor reconciles them, not this pass.
	cause := "wikilink removed in document revision " + revisionUID
	forgets, fErr := im.forgottenEdgeEvents(ctx, scope, docUID, resolved, cause, false)
	if fErr != nil {
		return nil, fErr
	}
	return append(events, forgets...), nil
}

// forgottenEdgeEvents emits relationForgotten events for the document's ACTIVE
// wikilink edges whose (predicate, target) no longer appears in the latest
// body's resolution — a link removed from the body must stop being reported by
// backlinks and reads. Only the wikilink predicates (mentions/links_to) with
// this document as subject are considered, so edges from other pipelines stay
// untouched; an edge whose link merely became ambiguous is also forgotten,
// consistent with the resolver's never-guess rule (reconcile re-adds it if the
// ambiguity later clears). The forgotten relation events stay in the JSONL
// log; the dispatcher's relationForgotten apply removes the derived row and
// graph edge.
//
// Cause is written verbatim into the RelationForgottenEvent.Reason field:
//   - on a wikilink reconcile pass: "wikilink removed in document revision <uid>"
//   - on a document delete (delete_move.go): "document deleted"
//
// Parameterizing cause here keeps the call sites honest — a delete-triggered
// forget must not claim the link was merely removed from a revision.
//
// The includeProse flag controls prose-extracted edges (provenance prefixed
// with proseProvenancePrefix). The re-import/revision path passes false: prose
// edges are reconciled by the async extractor, not the deterministic pass, so
// they must survive a body edit even when this revision resolved no wikilinks.
// Deletion passes true: a tombstoned document has no future extraction pass to
// reconcile its edges, so ALL outgoing edges (wikilink AND prose) must be
// forgotten or they would linger as dead edges from a non-existent document.
func (im *Importer) forgottenEdgeEvents(ctx context.Context, scope kernel.Scope, docUID string, resolved []resolvedLink, cause string, includeProse bool) ([]kernel.Event, error) {
	active, err := im.reader.EntityRelations(ctx, docUID, navigate.RelationFilter{
		Predicates: allDocumentPredicates,
		Limit:      -1,
	})
	if err != nil {
		return nil, fmt.Errorf("load active edges for %q: %w", docUID, err)
	}
	desired := make(map[string]struct{}, len(resolved))
	for _, l := range resolved {
		desired[l.predicate+"\x00"+l.objectUID] = struct{}{}
	}
	events := make([]kernel.Event, 0, len(active))
	for i := range active {
		r := &active[i]
		if r.SubjectUID != docUID {
			continue
		}
		// Never forget a prose-extracted edge in the deterministic pass UNLESS
		// the caller is deleting the document. On a re-import the desired set is
		// built only from wikilinks + frontmatter, so a prose-only "mentions"
		// edge would otherwise be deleted here and lost for good whenever the
		// async re-extraction returns nothing (a transient LLM hiccup, or
		// extraction disabled on this run); prose edges are reconciled by the
		// async extractor itself, not this pass. On a DELETE there is no future
		// extraction pass, so prose edges must be forgotten along with the rest.
		if !includeProse && strings.HasPrefix(r.Provenance, proseProvenancePrefix) {
			continue
		}
		if _, keep := desired[r.Predicate+"\x00"+r.ObjectUID]; keep {
			continue
		}
		payload, mErr := json.Marshal(ontology.RelationForgottenEvent{
			RelationUID: r.UID,
			Reason:      cause,
			ForgottenAt: im.now(),
		})
		if mErr != nil {
			return nil, fmt.Errorf("marshal relationForgotten: %w", mErr)
		}
		events = append(events, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindRelationForgotten),
			Scope:      scope,
			Source:     "document:import",
			Confidence: 1.0,
			Payload:    payload,
		})
	}
	return events, nil
}

// existingEdgeTargets returns the set of "predicate\x00objectUID" keys for the
// document's current outgoing wikilink edges (all document predicates: mentions,
// links_to, plus the typed set), so the importer never emits a duplicate edge
// on re-import or reconcile. The full predicate set is checked (not only the
// default two) so that typed edges from a previous import are deduped correctly.
func (im *Importer) existingEdgeTargets(ctx context.Context, docUID string) (map[string]struct{}, error) {
	keys, _, err := im.existingDocEdges(ctx, docUID)
	return keys, err
}

// existingDocEdges loads a document's current outgoing edges in one read and
// returns two views of them:
//
//   - keys: the "predicate\x00objectUID" dedup set over EVERY edge (active or
//     not) so a re-import never double-emits an edge that already exists.
//   - explicitNames: the nameKey-folded display names linked by ACTIVE, EXPLICIT
//     (non-prose) edges. The prose-extraction pass consults this so a
//     user-authored wikilink wins: when the user wrote [[Mithrilmail]] and it
//     resolved to one "Mithrilmail" entity, an async LLM mention of "Mithrilmail" must
//     not add a second, competing edge to a DIFFERENT same-named entity (the
//     project vs. the person). Explicit links are authoritative.
//
// Forgotten edges are deleted from the relations table (relationForgotten
// removes the derived row), so a wikilink the user REMOVED no longer appears
// here and a later prose mention of that name is free to stand. Superseded
// edges are skipped from explicitNames for the same reason — they no longer
// reflect the user's intent.
func (im *Importer) existingDocEdges(ctx context.Context, docUID string) (keys, explicitNames map[string]struct{}, err error) {
	rels, err := im.reader.EntityRelations(ctx, docUID, navigate.RelationFilter{
		Predicates:      allDocumentPredicates,
		IncludeInactive: true,
		Limit:           -1,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("load existing edges for %q: %w", docUID, err)
	}
	keys = make(map[string]struct{}, len(rels))
	explicitNames = make(map[string]struct{}, len(rels))
	for i := range rels {
		r := &rels[i]
		if r.SubjectUID != docUID {
			continue
		}
		keys[r.Predicate+"\x00"+r.ObjectUID] = struct{}{}
		if r.SupersededByUID == "" && r.ObjectName != "" && !strings.HasPrefix(r.Provenance, proseProvenancePrefix) {
			explicitNames[nameKey(r.ObjectName)] = struct{}{}
		}
	}
	return keys, explicitNames, nil
}

// relationEvent builds a wikilink relation event from a resolved link.
// SourceEventUID anchors provenance to the document revision that produced it.
// Confidence uses the stated value from the typed-link prefix (e.g.
// [contradicts:0.8] [[Y]]) or defaults to 1.0 when no confidence was stated
// (zero value from WikiLink.Confidence).
func (im *Importer) relationEvent(scope kernel.Scope, subjectUID string, link resolvedLink, sourceEventUID string) (kernel.Event, error) {
	uid, err := im.newUID()
	if err != nil {
		return kernel.Event{}, err
	}
	confidence := link.confidence
	if confidence == 0 {
		confidence = 1.0
	}
	// Deterministic wikilink/frontmatter edges carry no explicit provenance and
	// get the "wikilink <raw>" label. Prose-extracted edges set link.provenance
	// explicitly so they are both labelled honestly and excluded from the
	// deterministic forget pass.
	provenance := link.provenance
	if provenance == "" {
		provenance = "wikilink " + link.raw
	}
	rel := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     subjectUID,
		Predicate:      link.predicate,
		ObjectUID:      link.objectUID,
		Confidence:     confidence,
		SourceEventUID: sourceEventUID,
		CreateTime:     im.now(),
		Provenance:     provenance,
	}
	payload, err := json.Marshal(rel)
	if err != nil {
		return kernel.Event{}, fmt.Errorf("marshal wikilink relation: %w", err)
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindRelation),
		Scope:      scope,
		Source:     "document:import",
		Confidence: 1.0,
		Payload:    payload,
	}, nil
}

// prevRevisionPtr returns a pointer to uid for the optional
// DocumentRevision.PreviousRevisionUID field, or nil for a first revision.
func prevRevisionPtr(uid string) *string {
	if uid == "" {
		return nil
	}
	return &uid
}

// batchUIDs collects the document UIDs touched by this import.
func batchUIDs(changed []pendingDoc) map[string]struct{} {
	set := make(map[string]struct{}, len(changed))
	for i := range changed {
		set[changed[i].documentUID] = struct{}{}
	}
	return set
}

// displayName is the document's entity display name: the parsed title, else a
// humanised filename (never empty — the entity schema requires a non-empty
// displayName).
func displayName(pd *pendingDoc) string {
	if t := strings.TrimSpace(pd.parsed.Title); t != "" {
		return t
	}
	return humaniseFilename(pd.path)
}

// humaniseFilename turns "notes/project-alpha.md" into "Project Alpha".
func humaniseFilename(p string) string {
	baseName := filepath.Base(filepath.FromSlash(p))
	baseName = strings.TrimSuffix(baseName, filepath.Ext(baseName))
	baseName = strings.NewReplacer("-", " ", "_", " ").Replace(baseName)
	fields := strings.Fields(baseName)
	for i, f := range fields {
		fields[i] = strings.ToUpper(f[:1]) + f[1:]
	}
	if len(fields) == 0 {
		return "Untitled"
	}
	return strings.Join(fields, " ")
}

// vaultRelPath returns the forward-slash path of file relative to base.
func vaultRelPath(base, file string) (string, error) {
	rel, err := filepath.Rel(base, file)
	if err != nil {
		return "", fmt.Errorf("relativise %q under %q: %w", file, base, err)
	}
	return filepath.ToSlash(rel), nil
}

// isHiddenDir reports whether a directory name is a dotfile dir (e.g. .git,
// .obsidian) that should be skipped during a vault walk.
func isHiddenDir(name string) bool {
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}
