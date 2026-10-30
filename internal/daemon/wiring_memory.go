package daemon

import (
	"context"
	"database/sql"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/cognitive"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/reranker"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/rewriter"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// registerMemoryPrompts registers the extractor, resolver, composer, and
// rewriter prompt templates into reg. Extracted from wireMemoryLayer to keep
// that function's cyclomatic complexity within the lint budget once the
// ontology (extractor vocab / kind registry) and document-prose paths joined it.
func registerMemoryPrompts(ctx context.Context, reg *prompts.MemoryRegistry) error {
	if err := extractor.RegisterExtractPrompt(ctx, reg); err != nil {
		return fmt.Errorf("register extract prompt: %w", err)
	}
	if err := resolver.RegisterPrompts(ctx, reg); err != nil {
		return fmt.Errorf("register resolver prompts: %w", err)
	}
	if err := composer.RegisterComposePrompt(ctx, reg); err != nil {
		return fmt.Errorf("register compose prompt: %w", err)
	}
	if err := rewriter.RegisterPrompt(ctx, reg); err != nil {
		return fmt.Errorf("register rewriter prompt: %w", err)
	}
	return nil
}

// wireMemoryLayer plugs the extractor, resolver, composer, and ask MCP tool
// into a Wired struct that already has the orchestrator + HybridQuery online.
// Called only when a non-nil ChatProvider is configured; the no-chat path
// returns early in OpenWiring so this helper never runs with a nil chat arg.
//
// Build order:
//  1. prompts.Registry (nil log, audit disabled for M1 to avoid a
//     construction cycle with the FileEventLog that owns the log).
//  2. Register every M1 prompt template: extract-v1, the four resolver
//     templates (disambiguate / contradiction / summary / predicate-canon),
//     and compose-v1.
//  3. predicates.Registry from the dispatcher's read pool.
//  4. Extractor + Resolver against chat + prompts + predicates.
//  5. Install extractor + resolver on the FileEventLog via the late-binding
//     SetExtractor / SetResolver methods.
//  6. Composer + Ask MCP tool against chat + prompts + HybridQuery.
//
//nolint:gocritic // user passed by value at startup-only callsite; copy cost negligible.
func wireMemoryLayer(
	ctx context.Context,
	w *Wired,
	orch *orchestrator.Orchestrator,
	hybrid *kernel.HybridQuery,
	chat kernel.ChatProvider,
	user config.UserConfig,
	dataDir string,
	logger *zap.Logger,
) error {
	reg := prompts.New(nil)
	if err := registerMemoryPrompts(ctx, reg); err != nil {
		return err
	}

	chatTable := user.Providers[user.Defaults.Chat]
	chatModel := chatTable.ChatModel
	if chatModel == "" {
		chatModel = chatTable.Model
	}

	writer, err := orch.Storage.OpenSQLite(ctx)
	if err != nil {
		return fmt.Errorf("acquire sqlite writer: %w", err)
	}
	vec, err := orch.Storage.OpenVector(ctx)
	if err != nil {
		return fmt.Errorf("acquire vector conn: %w", err)
	}

	predicates, err := resolver.NewRegistry(ctx, writer)
	if err != nil {
		return fmt.Errorf("load predicates registry: %w", err)
	}
	kindRegistry, err := resolver.NewKindRegistry(ctx, writer)
	if err != nil {
		return fmt.Errorf("load entity-kind registry: %w", err)
	}

	ext := extractor.New(chat, reg, extractor.Config{
		Model:          chatModel,
		RecentEpisodes: extractor.NewSQLiteRecentEpisodes(writer),
		Vocab:          extractor.NewSQLiteVocabReader(writer),
	})

	res, err := resolver.New(resolver.Config{Model: chatModel, Governed: user.Ontology.Governed()}, resolver.Deps{
		EventLog:     orch.Log,
		Embedder:     orch.Embedder,
		Chat:         chat,
		Vec:          vec,
		Reader:       writer,
		Writer:       writer,
		Prompts:      reg,
		Predicates:   predicates,
		KindRegistry: kindRegistry,
		Logger:       logger,
		// Rebind a first-person ("self") extracted subject to the concrete
		// principal UID so self-statements sync as the shareable principal.
		SelfBinder: config.NewSelfBinder(dataDir),
	})
	if err != nil {
		return fmt.Errorf("build resolver: %w", err)
	}

	orch.Log.SetExtractor(ext)
	orch.Log.SetResolver(res)

	// NOTE: the boot-time un-extracted-episode backlog drain is intentionally
	// NOT here. wireMemoryLayer runs on EVERY OpenWiring — including one-shot
	// in-process CLI commands (maz get / recall / source list, ~10 callsites)
	// whose deferred drainAllBackground would then block until the entire
	// backlog extracts. The drain is a long-lived-daemon concern, so it lives
	// in the supervisor's wireAndBuildServer (drainExtractionBacklog) instead.

	// Expose the extractor as a ProseExtractor so startVaultWatcher can wire
	// document-prose extraction into the vault Importer. The adapter feeds
	// through the same chat-concurrency semaphore as episode extraction.
	adapter := document.NewExtractorAdapter(ext)
	w.ProseExtractor = adapter
	// Late-bind onto the shared Importer that registerNavigationTools already
	// built. WithProseExtractor mutates the struct in place so all MCP tools
	// (writeDocument, editDocument, importDocuments) holding the same pointer
	// automatically see prose extraction without re-wiring.
	if w.Importer != nil {
		w.Importer.WithProseExtractor(adapter)
	}

	wireSemanticNameResolver(ctx, w, orch, vec, writer, logger)

	comp, err := composer.New(composer.Config{
		Model:            chatModel,
		AbstainThreshold: user.Composer.AbstainThreshold,
	}, chat, reg, logger)
	if err != nil {
		return fmt.Errorf("build composer: %w", err)
	}

	if chatModel != "" {
		rw, rwErr := rewriter.New(chat, reg, chatModel)
		if rwErr != nil {
			return fmt.Errorf("build rewriter: %w", rwErr)
		}
		hybrid.SetRewriter(rw)

		rr, rrErr := reranker.New(reranker.Config{Model: chatModel}, chat, logger)
		if rrErr != nil {
			return fmt.Errorf("build reranker: %w", rrErr)
		}
		hybrid.SetReranker(rr)
	} else {
		logger.Warn("daemon: chat model unset; query rewriter + reranker not wired (RewriteQuery + LLM rerank will be no-ops)")
	}

	w.Tools = append(w.Tools, tools.NewAsk(hybrid, comp))

	// definePredicate (P4) lives here rather than in registerNavigationTools
	// because it writes through the same predicates registry the resolver uses
	// for auto-classification, so a new predicate is live for extraction
	// immediately — not just persisted to SQLite. It shares the resolver's
	// writer pool and registry instance to keep the in-memory cache coherent.
	w.Tools = append(w.Tools, tools.NewDefinePredicate(orch.Log, writer, predicates, logger))

	// Cognitive lifecycle (#231): autonomous reflection + consolidation jobs,
	// the decay/demote sweep, and the manual-trigger MCP tools. Chat-required,
	// so it lives on this path; degrades to a warn + skip if storage is not a
	// dispatcher (the jobs need the SQLite reader/writer + the vec deduper).
	if err := wireCognitiveLayer(ctx, w, orch, chat, chatModel, logger); err != nil {
		return err
	}

	logger.Info("daemon: memory layer wired (extractor + resolver + composer + ask + rewriter + reranker)")
	return nil
}

// wireCognitiveLayer constructs the reflection + consolidation jobs and the
// decay/demote sweep supervisor (issue #231), starts the idle-gated ticker on
// ctx, registers its Close on w.closers, and exposes the manual-trigger MCP
// tools. Extracted from wireMemoryLayer to keep that function's complexity
// within budget. A non-dispatcher storage backend (e.g. a test fake) disables
// the cognitive layer with a warn rather than failing boot.
func wireCognitiveLayer(
	ctx context.Context,
	w *Wired,
	orch *orchestrator.Orchestrator,
	chat kernel.ChatProvider,
	chatModel string,
	logger *zap.Logger,
) error {
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		logger.Warn("daemon: storage is not a dispatcher; cognitive jobs (reflection/consolidation/decay) disabled")
		return nil
	}
	reader, err := disp.SQLiteReader()
	if err != nil {
		return fmt.Errorf("cognitive: acquire sqlite reader: %w", err)
	}
	writer, err := orch.Storage.OpenSQLite(ctx)
	if err != nil {
		return fmt.Errorf("cognitive: acquire sqlite writer: %w", err)
	}
	// DefaultReadSet: cognitive jobs (reflection/consolidation) synthesize from the
	// standard user scope span. New() defaults to DefaultReadSet so no override needed.
	nav, err := navigate.New(reader)
	if err != nil {
		return fmt.Errorf("cognitive: build navigator: %w", err)
	}

	// disp satisfies cognitive.Deduper via CheckDuplicate for both jobs:
	// reflection dedups candidate insights against the "insights" vec domain,
	// consolidation dedups candidate summaries against the "consolidated" domain.
	reflector := cognitive.NewReflector(chat, nav, orch.Log, disp, chatModel, logger)
	consolidator := cognitive.NewConsolidator(chat, nav, orch.Log, disp, chatModel, logger)

	// Idle gate: a tick runs only when both background lanes are quiescent, so
	// the LLM jobs never compete with active ingestion (mirrors waitForIdle).
	idle := func() bool {
		return orch.Log.BackgroundWorkDepth() == 0 && disp.BackgroundVecDepth() == 0
	}

	sv := newCognitiveSupervisor(reflector, consolidator, writer, idle, w.Jobs, logger)
	sv.Start(ctx)
	w.closers = append(w.closers, sv.Close)

	w.Tools = append(w.Tools,
		tools.NewTriggerReflection(sv),
		tools.NewTriggerConsolidation(sv),
	)

	logger.Info("daemon: cognitive layer wired (reflection + consolidation jobs + decay sweep + triggers)")
	return nil
}

// wireSemanticNameResolver wires the optional semantic wikilink resolver onto
// the shared Importer. It reuses the same embedder + entities_vec name vectors
// the entity resolver uses in its nameEmbMatch step. The kindForUID closure
// reads the entities table via the SQLite reader (a single SELECT by primary
// key, negligible cost).
//
// The vec parameter must satisfy document.nameEmbeddingSearcher; the production
// *sqlite.Vector satisfies it via SearchNameVectors. If the type assertion fails
// (e.g. a test fake that does not implement SearchNameVectors), the semantic lane
// is skipped rather than failing boot.
//
// Extracted from wireMemoryLayer to keep gocognit complexity inside the limit.
func wireSemanticNameResolver(
	_ context.Context,
	w *Wired,
	orch *orchestrator.Orchestrator,
	vec kernel.VectorConn,
	writer *sql.DB,
	logger *zap.Logger,
) {
	type nameSearcher interface {
		SearchNameVectors(ctx context.Context, query []float32, k int) ([]kernel.VectorHit, error)
	}
	snrSearcher, ok := vec.(nameSearcher)
	if !ok {
		logger.Warn("daemon: vector conn does not implement SearchNameVectors; semantic wikilink lane disabled")
		return
	}

	kindForUID := func(ctx context.Context, uid string) string {
		var kind string
		if err := writer.QueryRowContext(ctx,
			`SELECT kind FROM entities WHERE uid = ?`, uid,
		).Scan(&kind); err != nil {
			// Entity not found or DB error: degrade to empty kind (predicateForKind
			// then falls back to "mentions", which is the safe default).
			return ""
		}
		return kind
	}

	snr := document.NewNameEmbeddingResolver(orch.Embedder, snrSearcher, kindForUID, logger)
	if w.Importer != nil {
		w.Importer.WithSemanticNameResolver(snr)
	}
	logger.Info("daemon: semantic wikilink resolver wired (name-embedding lane active)")
}

// registerDefinePredicateDegraded wires the definePredicate authoring tool in
// the degraded daemon modes (no embedder, or no chat provider) where
// wireMemoryLayer does not run. In full mode wireMemoryLayer registers
// definePredicate against the resolver's shared in-memory registry so a new
// predicate is live for extraction immediately; in degraded mode there is no
// resolver/extractor to stay cache-coherent with, so a standalone registry over
// the dispatcher's writer is correct. This mirrors defineEntityKind, which is
// always registered (in registerNavigationTools) and likewise does not share
// the resolver's kind registry. Failures degrade to a warn + skip rather than
// failing boot: the read surface still works; only `schema predicates add`
// against the daemon is unavailable.
func registerDefinePredicateDegraded(ctx context.Context, w *Wired, orch *orchestrator.Orchestrator, logger *zap.Logger) {
	writer, err := orch.Storage.OpenSQLite(ctx)
	if err != nil {
		logger.Warn("daemon: definePredicate skipped (sqlite writer unavailable)", zap.Error(err))
		return
	}
	predicates, err := resolver.NewRegistry(ctx, writer)
	if err != nil {
		logger.Warn("daemon: definePredicate skipped (predicate registry load failed)", zap.Error(err))
		return
	}
	w.Tools = append(w.Tools, tools.NewDefinePredicate(orch.Log, writer, predicates, logger))
}
