package kernel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/hlc"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// ErrInvalidEvent is returned when an event fails pre-write validation:
// malformed UID, future-dated CreateTime beyond the allowed skew, or a
// payload that the ontology rejects. Callers can use errors.Is to branch
// on the class of failure; the wrapped error names the offending field.
var ErrInvalidEvent = errors.New("kernel: invalid event")

// defaultMaxSkew is the tolerance for caller-supplied CreateTime values
// in the future. Tuned for NTP drift across providers.
const defaultMaxSkew = 5 * time.Minute

// defaultApplyTimeout bounds the post-fsync fanout into Storage.Apply.
// Append releases l.mu before invoking Apply, so this deadline does
// not gate the writer mutex — it gates the calling goroutine so a
// wedged dispatcher (slow embedder, hung SQLite write) eventually
// returns receipts and emits the warn-and-continue log. 30s matches
// Ollama's worst-case embed timeout (60s) minus a comfort margin so a
// degraded inner provider can still surface its own error rather than
// being preempted here. Issue #42's daemon may revisit this once load
// tests appear.
//
// The cache-singleflight race the previous bound reintroduced (Greptile
// #108 third pass) is resolved at the dispatcher/cache boundary:
// CachingEmbedder.Wait drains pending singleflight workers, and
// Dispatcher.Close calls it before tearing down SQLite pools. So an
// embed worker that outlives the deadline still completes against live
// pools.
const defaultApplyTimeout = 30 * time.Second

// defaultExtractionTimeout bounds each background extraction goroutine.
// A LongMemEval-S haystack bursts ~250 episodes through
// scheduleBackgroundEnrichment, all of which queue behind Ollama's
// per-model serializer: late-queued goroutines wait their turn, so the
// budget must absorb (queue_depth * per_call_latency). Sized for the
// worst case the managed daemon hits in practice: Ollama's default
// `num_parallel` is 1 when unset, so 250 events × ~5 s ≈ 1250 s of
// queue wait. 1800 s gives headroom for slower hardware and colder
// models. The prior 90 s caused silent extraction loss in the 15q
// bench — a subset of episodes' mentions/entities never landed because
// the goroutine's ctx expired while it sat in Ollama's queue, and
// recall@K varied run-to-run as the loss set shifted. Extraction
// failures are still warn-logged and never propagate to the original
// Append caller.
const defaultExtractionTimeout = 1800 * time.Second

// defaultResolutionTimeout bounds each background resolution goroutine.
// Resolution runs once per extraction batch but emits several LLM calls
// internally (naming + summary + relation classification), so under a
// 250-episode burst it queues ~750 LLM requests behind the same Ollama
// serializer as extraction — the same queue-depth envelope applies.
// Sized to match defaultExtractionTimeout for that reason. Kept as a
// separate field (not folded into extractionTimeout) so callers that
// can prove their resolver path is short-circuiting safely can tune it
// down independently via WithResolutionTimeout; tests use that path to
// pin the split contract. The trade-off vs a smaller default: a wedged
// resolver can pin Close / WaitForBackgroundWork / recall drain for up
// to defaultResolutionTimeout — accepted because silently dropping
// Entity / Relation events causes the recall@K instability this PR is
// addressing, and operators with a reliably fast resolver path can
// shrink the budget per-instance.
const defaultResolutionTimeout = 1800 * time.Second

// FileEventLog is the on-disk JSONL writer for the event log. Appends
// take a write lock on mu; Reads take a read lock. The RWMutex preserves
// two contract guarantees from issue #25: (1) concurrent Read calls do
// not block each other (multiple RLocks coexist), and (2) Reads see
// fully-committed events only (an RLock cannot be acquired while a
// writer holds the Lock, so a Read never observes the page-cache
// window between f.Write and a successful f.Sync). The file handle is
// opened and closed per Append so rotation, recovery, and rebuilds do
// not contend with a long-lived fd.
//
// See agent-docs/designs/event-log.md for the algorithm and decisions.
type FileEventLog struct {
	dataDir  string
	ontology Ontology

	clock      func() time.Time
	newULID    func() (string, error)
	fsyncDirFn func(string) error
	writeOneFn func(bucketPlan) (oldSize int64, observed bool, createdByUs bool, err error)
	maxSkew    time.Duration

	// hlcClock is the hybrid logical clock used to stamp each new event with a
	// convergent total-order key (see internal/kernel/hlc). It is initialised
	// lazily on the first Append or GraftSealed call via hlcInitOnce: the sidecar
	// may not exist yet at NewFileEventLog time, and the RLock path inside
	// openCommitted conflicts with the mu.Lock in appendUnderLock, so loading
	// must happen before appendUnderLock acquires mu.
	hlcClock    *hlc.Clock
	hlcInitOnce sync.Once

	mu sync.RWMutex

	// indexMu guards the lifecycle of the bbolt sidecar. It is held
	// for WRITE while opening or closing the handle, and for READ during
	// At and putIndex so Close cannot tear the handle down underneath
	// an in-flight bbolt transaction. The lock is independent of mu so
	// At never contends with Append on the writer mutex.
	indexMu sync.RWMutex
	index   *bolt.DB

	// storage receives every batch after the JSONL fsync + sidecar
	// update succeed (issue #55). Nil by default so isolated writer
	// tests stay independent of the four-layer dispatcher; the
	// daemon's orchestrator threads in a real *dispatcher.Dispatcher
	// via WithStorage. NoopStorage is deliberately NOT the default:
	// its Apply returns ErrNotImplemented, which would fire a warn-log
	// on every Append once a real logger is wired (the daemon will).
	storage Storage

	// extractor is called after storage.Apply succeeds for each Append
	// batch (see applyExtraction). Nil by default so existing tests that
	// do not inject a ChatProvider stay clean; WithExtractor installs the
	// real extractor in production. applyExtraction short-circuits on nil
	// so no warn path fires on a nil default.
	extractor ExtractorIface

	// resolver is called after applyExtraction has appended its derived
	// Mention and RelationCandidatesProposed events. The resolver consumes
	// those events and emits Entity / Relation / RelationRetracted /
	// PredicateRegistered events. Nil by default; WithResolver installs
	// the real resolver in production. applyResolution short-circuits on
	// nil so no warn path fires on the default.
	resolver ResolverIface

	// signer stamps the mandatory EventAuthor block onto every event in
	// prepare() (after UID/CreateTime/Scope are final). Never nil: the
	// constructor installs newLocalDefaultSigner() so existing callers
	// and tests produce signed events without configuration; production
	// overrides it with the Watchword-derived identity signer via
	// WithSigner / SetSigner. The pointer is read under mu.
	signer Signer

	// verifier checks team-scope event signatures during Rebuild. Nil by
	// default (single-user / no team verification); the daemon installs a
	// real verifier via WithVerifier / SetVerifier once the identity
	// store + device table are available. The Rebuild gate short-circuits
	// when verifier is nil or the event is not team-scoped, so personal /
	// project replay pays zero verification cost.
	verifier Verifier

	// teamAuth gates LIVE team-scope writes at the append boundary: a
	// team-scope event by a principal who is not an admitted member is
	// rejected with ErrPermission (the genesis self-grant is the one
	// bootstrap exception). Nil by default (single-user: no team writes
	// expected, and the local-default signer is rejected for team scope
	// regardless). The daemon installs the dispatcher's ACL-backed
	// authorizer via SetTeamWriteAuthority. This is the LIVE gate only;
	// replay/quarantine convergence is the membership fold's job, so this
	// is never consulted during Rebuild.
	teamAuth TeamWriteAuthority

	// teamRoleChecker queries a principal's effective role at a given
	// position in the canonical event order (a restricted re-fold of the
	// membership events below the verified event's UID), so verifyTeamEvent
	// reaches the same verdict on live graft and on Rebuild replay. Nil by
	// default; installed by SetTeamRoleChecker once the dispatcher is
	// available.
	teamRoleChecker TeamRoleChecker

	// deviceRevokeChecker queries whether a device has been revoked at a
	// given position in the canonical event order. Used by verifyTeamEvent to
	// drop events authored by revoked devices with a UID > the revocation UID.
	// Nil by default; installed by SetDeviceRevokeChecker once the dispatcher
	// is available.
	deviceRevokeChecker DeviceRevokeChecker

	// extractionTimeout bounds each background extraction goroutine's
	// ctx. Defaults to defaultExtractionTimeout; overridable via
	// WithExtractionTimeout. Sized for Ollama queue depth under burst.
	extractionTimeout time.Duration

	// resolutionTimeout bounds each background resolution goroutine's
	// ctx. Kept separate from extractionTimeout so a wedged resolver
	// does not extend Close / drain by the full extraction window.
	// Defaults to defaultResolutionTimeout; overridable via
	// WithResolutionTimeout.
	resolutionTimeout time.Duration

	// bgWork tracks extraction + resolution goroutines spawned by Append.
	// Extraction calls a remote LLM (qwen2.5:7b tool-use, ~17-24s per
	// episode in production); leaving it on the Append hot path blew the
	// LongMemEval feeder's 30s timeout and lost every Episode past the
	// first few. Append now spawns extraction asynchronously and Close
	// waits on bgWork before tearing the log down, so callers that need
	// post-extraction state (tests, the eval harness, shutdown) can
	// observe a quiescent log without spinning on disk state.
	bgWork sync.WaitGroup

	// bgWorkDepth mirrors bgWork's internal counter so external callers
	// (the idleStatus MCP tool, eval benchmarks) can poll progress
	// without the unexported WaitGroup state. Incremented next to every
	// bgWork.Add(1); decremented next to every bgWork.Done(). They MUST
	// stay in lockstep — diverging makes BackgroundWorkDepth lie about
	// whether the log is quiescent.
	bgWorkDepth atomic.Int64

	// Bounded extraction worker pool. When extractionWorkers > 0,
	// scheduleBackgroundEnrichment enqueues a job onto extractionQueue
	// instead of spawning a fresh goroutine; a fixed pool of
	// extractionWorkers worker goroutines drains the queue. This caps
	// memory + HTTP-connection fanout under burst load (the LongMemEval
	// feeder sends 500+ remembers back-to-back; the unbounded shape
	// spawned 500 goroutines that all stacked up behind Ollama's
	// model-level serialisation). Zero workers keeps the legacy
	// per-Append spawn (used by unit tests that never set the option).
	// extractionWG tracks the worker goroutines so Close can drain
	// before tearing the log down. extractionStop is closed by Close to
	// signal workers to exit after draining the queue.
	extractionWorkers int
	extractionQueue   chan extractionJob
	extractionWG      sync.WaitGroup
	extractionStop    chan struct{}

	// spillSlots bounds the one-shot enrichment goroutines spawned when the
	// worker pool's queue is already full, so a sustained ingestion flood
	// cannot grow an unbounded goroutine herd that all block on Ollama's
	// single chat slot until their deadlines expire (wasting memory and
	// losing the work anyway). Buffered to extractionWorkers*spillCapPerWorker;
	// left nil when the pool is disabled (extractionWorkers <= 0), where
	// spills stay unbounded so tests draining via WaitForBackgroundWork never
	// lose a scheduled job.
	spillSlots chan struct{}

	// scheduleMu guards extractionQueue against close-while-sending.
	// Producers (scheduleBackgroundEnrichment) take RLock; Close takes
	// Lock before closing the channel. Without this, the producer's
	// select can race a concurrent close() and panic on "send on closed
	// channel" when the inner Append from a worker (applyExtraction
	// -> Append -> scheduleBackgroundEnrichment) overlaps shutdown.
	scheduleMu   sync.RWMutex
	shuttingDown bool

	// statsStore receives IncEvent and IncBytes on every successful Append.
	// Defaults to stats.Noop() so callers that do not inject a store
	// incur no overhead and no nil-check is needed at the call site.
	statsStore stats.Store

	// strictAttributes, when true, causes prepareEvent to reject Entity
	// events whose properties field fails to validate against the kind's
	// composed propertySchema. When false (the default) validation runs
	// but violations are warn-logged and the event is accepted. Mirrors
	// the warn-accept policy used by the P2 entity-kind membership check.
	// Set via WithStrictAttributes; threaded from [ontology] strict_attributes
	// in config.toml.
	strictAttributes bool

	// strictDomainRange, when true, causes prepareEvent to reject Relation
	// events whose subject/object entity kinds fall outside the predicate's
	// declared domain/range, or whose Attributes fail propertySchema
	// validation. When false (the default) violations are warn-logged and
	// accepted. Set via WithStrictDomainRange; threaded from
	// [ontology] strict_domain_range in config.toml (P5 typed relations).
	// The predicate definitions and entity-kind lookups come from the
	// configured Ontology (SQLiteOntology.ValidateRelation); see
	// checkRelationDomainRange.
	strictDomainRange bool

	// jobStarter is the optional JobStarter the extraction worker pool
	// uses to surface every background-extraction batch in the Jobs
	// hub panel. Nil means jobs are not reported (the default for
	// tests / minimal-boot daemons).
	jobStarter JobStarter

	// extractedEpisodeUIDs records every episode UID for which we've
	// observed a downstream Mention in the current daemon process.
	// applyExtraction consults this LRU before invoking the
	// extractor so events arriving on a pull replay (Episode +
	// Mention together in one batch or in consecutive batches) do
	// not re-trigger non-deterministic LLM re-extraction. The set
	// is bounded (extractedEpisodeUIDsCap entries) and FIFO-evicts
	// the oldest entry when full; a fallback JSONL forward-scan
	// catches the cross-restart case where the receiving daemon was
	// restarted between the Mention arriving and the Episode
	// arriving fresh on a later pull.
	extractedMu              sync.Mutex
	extractedEpisodeUIDs     map[string]struct{}
	extractedEpisodeUIDOrder []string // FIFO insertion order for bounded eviction
}

// extractedEpisodeUIDsCap is the LRU bound for the in-memory dedup
// set. 1024 entries covers a daemon's working set of recently-
// observed Mentions; older entries fall back to the JSONL scan
// path (see hasDownstreamMentions). Sized to keep memory predictable
// (each entry is a 26-byte ULID + map overhead ≈ 100 B → ~100 KiB
// total at full capacity).
const extractedEpisodeUIDsCap = 1024

// extractedFallbackScanWindow caps how far back hasDownstreamMentions
// walks the JSONL day-bucket directory looking for a Mention that
// references the given episode UID. 7 days is the operationally
// useful window: a Mention older than that is, by construction,
// already in the dedup set if the daemon has been running, or
// belongs to an Episode whose re-extraction would be harmless cost
// (the receiver doesn't yet have a canonical entity for it).
const extractedFallbackScanWindow = 7 * 24 * time.Hour

// extractionJob bundles the prepared batch and the caller context for a
// single scheduled extraction unit. Carried over the bounded queue from
// scheduleBackgroundEnrichment to the worker pool. Ctx is detached from
// the caller (context.WithoutCancel) so a returned Append followed by a
// request cancel does not abort the queued extraction mid-call; the
// applyExtraction / applyResolution helpers apply their own per-call
// timeouts internally.
type extractionJob struct {
	ctx      context.Context
	prepared []Event
}

// Option configures a FileEventLog at construction time.
type Option func(*FileEventLog)

// WithClock injects a clock. Tests use this to assert CreateTime defaulting
// and skew rejection without depending on wall time.
func WithClock(fn func() time.Time) Option {
	return func(l *FileEventLog) { l.clock = fn }
}

// WithULID injects a ULID generator. Tests use this to make UIDs
// deterministic; production code relies on the default.
func WithULID(fn func() (string, error)) Option {
	return func(l *FileEventLog) { l.newULID = fn }
}

// WithMaxSkew sets the tolerance for caller-supplied CreateTime values
// in the future. The default is five minutes.
func WithMaxSkew(d time.Duration) Option {
	return func(l *FileEventLog) { l.maxSkew = d }
}

// WithStorage injects a Storage that receives every batch after the
// JSONL fsync + sidecar index update succeed. The default is
// NoopStorage so Append is bytes-only in unit-test setups; in
// production the daemon's orchestrator (issue #55) passes a
// *dispatcher.Dispatcher so the four derived stores stay coherent
// with the log. A nil argument is ignored: the existing handle
// (NoopStorage at construction) is preserved.
func WithStorage(s Storage) Option {
	return func(l *FileEventLog) {
		if s != nil {
			l.storage = s
		}
	}
}

// WithExtractionTimeout overrides the per-goroutine ctx budget used by
// applyExtraction. Default is defaultExtractionTimeout (30 minutes),
// sized to absorb a 250-episode LongMemEval burst at the managed
// Ollama default (num_parallel=1). Tests use this to demonstrate the
// loss path with sub-second budgets; production code should leave the
// default in place unless an operator is empirically tuning for a
// smaller queue (e.g., a daemon that pins OLLAMA_NUM_PARALLEL >= 4
// can safely shrink this).
func WithExtractionTimeout(d time.Duration) Option {
	return func(l *FileEventLog) {
		if d > 0 {
			l.extractionTimeout = d
		}
	}
}

// WithResolutionTimeout overrides the per-goroutine ctx budget used by
// applyResolution. Default is defaultResolutionTimeout (5 minutes),
// kept tighter than extraction so a wedged resolver does not pin
// Close / drain for the full extraction window. Resolution emits a
// bounded number of LLM calls per batch (naming + summary + relation
// classification), so its queue-depth exposure is fundamentally
// smaller than extraction's per-episode fanout.
func WithResolutionTimeout(d time.Duration) Option {
	return func(l *FileEventLog) {
		if d > 0 {
			l.resolutionTimeout = d
		}
	}
}

// WithExtractionWorkers caps the number of concurrent background
// extraction goroutines. N > 0 enables the bounded worker pool: a fixed
// pool of n workers drains a channel queue that scheduleBackgroundEnrichment
// enqueues onto. N <= 0 keeps the legacy per-Append goroutine spawn
// (default), which is fine for unit tests but blows up under haystack
// ingestion bursts where Ollama serialises at the model anyway. Production
// wiring should set this to a small number matching the embed provider's
// realistic concurrency (4 mirrors Ollama's default OLLAMA_NUM_PARALLEL).
func WithExtractionWorkers(n int) Option {
	return func(l *FileEventLog) { l.extractionWorkers = n }
}

// WithExtractor injects an ExtractorIface that is called after
// storage.Apply succeeds for each Append batch (Episode events only;
// the extractor impl is responsible for ignoring non-episode events).
// A nil argument is ignored. The default is nil (no-op: extraction is
// skipped and no warn is emitted).
func WithExtractor(e ExtractorIface) Option {
	return func(l *FileEventLog) {
		if e != nil {
			l.extractor = e
		}
	}
}

// WithResolver injects a ResolverIface that is called after applyExtraction
// has appended its derived Mention and RelationCandidatesProposed events.
// A nil argument is ignored. The default is nil (no-op: resolution is
// skipped and no warn is emitted).
func WithResolver(r ResolverIface) Option {
	return func(l *FileEventLog) {
		if r != nil {
			l.resolver = r
		}
	}
}

// WithStats injects a stats.Store that receives IncEvent and IncBytes on
// every successful Append. A nil argument is ignored; the default Noop store
// is preserved so existing callers incur no overhead.
func WithStats(s stats.Store) Option {
	return func(l *FileEventLog) {
		if s != nil {
			l.statsStore = s
		}
	}
}

// WithSigner installs the Signer that stamps the mandatory EventAuthor
// block onto every event. A nil argument is ignored so the constructor's
// local-default signer is preserved. Production wiring passes the
// Watchword-derived identity signer here; tests pass a deterministic
// static signer when they need to assert exact signed bytes.
func WithSigner(s Signer) Option {
	return func(l *FileEventLog) {
		if s != nil {
			l.signer = s
		}
	}
}

// WithVerifier installs the Verifier consulted for team-scope events
// during Rebuild. A nil argument is ignored (the default is no
// verification, which is correct for single-user installs). The daemon
// passes a verifier backed by the enrolled-device table once identity is
// wired.
func WithVerifier(v Verifier) Option {
	return func(l *FileEventLog) {
		if v != nil {
			l.verifier = v
		}
	}
}

// WithStrictAttributes enables strict property-schema enforcement for Entity
// events. When true, an Entity whose properties field fails to validate against
// the kind's composed propertySchema is rejected (ErrInvalidEvent). When false
// (the default), violations are warn-logged and the event is accepted. Mirrors
// the warn-accept policy used by the P2 entity-kind membership check: the flag
// is off by default so autonomous extraction is never blocked. Enabled via the
// [ontology] strict_attributes = true knob in config.toml.
func WithStrictAttributes(strict bool) Option {
	return func(l *FileEventLog) { l.strictAttributes = strict }
}

// WithStrictDomainRange enables strict domain/range enforcement for Relation
// events (P5 typed relations). When true, a Relation whose subject/object
// entity kinds fall outside the predicate's declared subjectKinds/objectKinds,
// or whose Attributes fail the predicate's propertySchema, is rejected
// (ErrInvalidEvent). When false (the default), violations are warn-logged and
// the event is accepted. Enabled via [ontology] strict_domain_range = true in
// config.toml.
func WithStrictDomainRange(strict bool) Option {
	return func(l *FileEventLog) { l.strictDomainRange = strict }
}

// SetResolver installs the ResolverIface after construction. This is the
// late-binding entry point the daemon needs: the resolver depends on the
// dispatcher's SQLite pools, which exist only after orchestrator.New runs,
// so the log itself must already have been constructed by the time the
// daemon can build a resolver. Safe to call exactly once before the first
// Append. A nil argument is ignored.
func (l *FileEventLog) SetResolver(r ResolverIface) {
	if r == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resolver = r
}

// SetExtractor mirrors SetResolver for the extractor hook. Used by the
// daemon to install the extractor after the orchestrator + prompts
// registry are wired. A nil argument is ignored.
func (l *FileEventLog) SetExtractor(e ExtractorIface) {
	if e == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.extractor = e
}

// SetSigner installs the Signer after construction, replacing the local
// default. This is the late-binding entry point the daemon needs: the
// Watchword-derived key is available only after the secret chain + keychain
// are wired, which happens after the log is constructed. Safe to call
// before the first Append. A nil argument is ignored, preserving the
// local default so the Author field is never stamped by a nil signer.
func (l *FileEventLog) SetSigner(s Signer) {
	if s == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.signer = s
}

// SetVerifier installs the team-scope Verifier after construction. Used by
// the daemon once the enrolled-device table is queryable. A nil argument
// is ignored. Until set, team-scope replay is not signature-verified
// (correct for single-user installs that have no team scope).
func (l *FileEventLog) SetVerifier(v Verifier) {
	if v == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verifier = v
}

// SetTeamWriteAuthority installs the live team-scope write gate after
// construction. Used by the daemon once the dispatcher (which holds the
// membership ACL) is available. A nil argument is ignored; until set,
// team-scope writes are gated only by the local-default-signer rejection
// (correct for single-user installs that have no team membership).
func (l *FileEventLog) SetTeamWriteAuthority(a TeamWriteAuthority) {
	if a == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.teamAuth = a
}

// SetTeamRoleChecker installs the role-at-position checker used by
// verifyTeamEvent on live graft and on Rebuild replay. The dispatcher
// satisfies this interface; nil is ignored (single-user installs have no
// team role enforcement). Must be set after the dispatcher's membership
// projection is queryable.
func (l *FileEventLog) SetTeamRoleChecker(c TeamRoleChecker) {
	if c == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.teamRoleChecker = c
}

// SetDeviceRevokeChecker installs the convergent device-revocation checker
// used by verifyTeamEvent on live graft and on Rebuild replay. The
// dispatcher satisfies this interface; nil is ignored (single-user installs
// have no device revocation). Must be set after the dispatcher's device
// projection is queryable.
func (l *FileEventLog) SetDeviceRevokeChecker(c DeviceRevokeChecker) {
	if c == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deviceRevokeChecker = c
}

// SetStats installs a stats.Store after construction. Used by the
// daemon to swap in the live counter sink after orchestrator
// construction (the orchestrator owns the FileEventLog, so options
// can't be threaded all the way down). Passing nil falls back to
// stats.Noop() so the hot path never panics.
func (l *FileEventLog) SetStats(s stats.Store) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s == nil {
		l.statsStore = stats.Noop()
		return
	}
	l.statsStore = s
}

// SetJobStarter installs a JobStarter after construction. When set,
// the extraction worker pool opens a tracked job per batch so the
// Jobs hub panel surfaces background extraction work alongside the
// rebuild job (the previous lone producer). Passing nil clears any
// previously installed starter.
func (l *FileEventLog) SetJobStarter(s JobStarter) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jobStarter = s
}

// NewFileEventLog returns a FileEventLog that writes to dataDir and
// validates payloads against ontology. DataDir must already exist or be
// creatable; Append will MkdirAll the day-bucket parent as needed.
//
// The default ULID generator captures per-instance monotonic entropy so
// receipts minted within the same millisecond sort in append order.
//
// Single-writer invariant: a given dataDir is owned by exactly one
// FileEventLog at a time. The bbolt sidecar (events/index.bolt) holds
// an exclusive OS file lock, so a second FileEventLog over the same
// directory will block on Append/At until the first one is closed.
// Daemon shutdown owns the close.
func NewFileEventLog(dataDir string, ontology Ontology, opts ...Option) *FileEventLog {
	entropy := ulid.Monotonic(rand.Reader, math.MaxUint32)
	l := &FileEventLog{
		dataDir:              dataDir,
		ontology:             ontology,
		clock:                func() time.Time { return time.Now().UTC() },
		maxSkew:              defaultMaxSkew,
		fsyncDirFn:           fsyncDir,
		statsStore:           stats.Noop(),
		signer:               newLocalDefaultSigner(),
		extractionTimeout:    defaultExtractionTimeout,
		resolutionTimeout:    defaultResolutionTimeout,
		extractedEpisodeUIDs: make(map[string]struct{}),
		hlcClock:             new(hlc.Clock),
		// storage stays nil by default; WithStorage installs the
		// real dispatcher in production. applyToStorage short-circuits
		// on nil so the warn path never fires on a no-op default.
		newULID: func() (string, error) {
			id, err := ulid.New(ulid.Now(), entropy)
			if err != nil {
				return "", fmt.Errorf("ulid.New: %w", err)
			}
			return id.String(), nil
		},
	}
	l.writeOneFn = l.writeOne
	for _, opt := range opts {
		opt(l)
	}
	l.startExtractionWorkers()
	return l
}

// extractionQueueCapPerWorker controls the buffered-channel sizing for the
// bounded extraction worker pool. A modest backlog per worker lets bursty
// Appends enqueue without producer blocking; queueing further than this
// just buffers latency without changing throughput because Ollama caps the
// actual model concurrency.
const extractionQueueCapPerWorker = 32

// spillCapPerWorker bounds the one-shot spill goroutines (per worker) spawned
// when the pool queue is full. Generous on purpose: an interactive burst never
// approaches it, so it only caps a pathological ingestion flood. Beyond the
// queue (extractionQueueCapPerWorker) plus this, further enrichment is dropped
// with a warn rather than spawned, since Ollama serialises the actual model
// concurrency anyway and the episode is durable regardless.
const spillCapPerWorker = 64

// startExtractionWorkers boots the bounded worker pool when
// extractionWorkers > 0. Called from NewFileEventLog after options have
// been applied. Each worker drains extractionQueue and runs
// applyExtraction + applyResolution per job. Workers exit when both
// extractionStop is closed AND the queue has drained.
func (l *FileEventLog) startExtractionWorkers() {
	if l.extractionWorkers <= 0 {
		return
	}
	l.extractionQueue = make(chan extractionJob, l.extractionWorkers*extractionQueueCapPerWorker)
	l.spillSlots = make(chan struct{}, l.extractionWorkers*spillCapPerWorker)
	l.extractionStop = make(chan struct{})
	for range l.extractionWorkers {
		l.extractionWG.Add(1)
		go l.extractionWorkerLoop()
	}
}

// extractionWorkerLoop is one worker goroutine in the pool. It drains jobs
// from extractionQueue and runs each via runEnrichmentJob until the queue is
// closed. RunEnrichmentJob owns the per-job bgWork.Done + bgWorkDepth release
// (and recovers a panicking batch), so a job that panics mid-extraction neither
// crashes the daemon nor leaks the in-flight counters — the bug the previous
// inline, non-deferred decrement here left open.
func (l *FileEventLog) extractionWorkerLoop() {
	defer l.extractionWG.Done()
	for job := range l.extractionQueue {
		l.runEnrichmentJob(job.ctx, job.prepared)
	}
}

// runEnrichmentJob runs one extraction+resolution batch and ALWAYS releases the
// in-flight accounting (bgWork.Done + bgWorkDepth--), even if the batch panics.
// Both invariants are load-bearing:
//
//   - Recover: an unrecovered panic in a background goroutine — a malformed LLM
//     tool-use response, a nil deref in the resolver — takes down the WHOLE
//     daemon process. Recovering keeps the daemon alive; the episode is already
//     durable in the JSONL and a failed enrichment is recoverable via Rebuild,
//     so no source data is lost.
//   - Guaranteed counter release: bgWork/bgWorkDepth drive WaitForBackgroundWork
//     and idleStatus. A panic that skipped the decrement would wedge them
//     forever, reporting in-flight work that no longer exists (isIdle never
//     returns true). Deferring the release fixes both the worker-loop path
//     (which decremented only on the success line) and the spill path.
func (l *FileEventLog) runEnrichmentJob(ctx context.Context, prepared []Event) {
	defer l.bgWork.Done()
	defer l.bgWorkDepth.Add(-1)
	defer func() {
		if r := recover(); r != nil {
			logging.FromContext(ctx).Error(
				"eventlog: background enrichment panicked (episode durable, recoverable via Rebuild)",
				zap.Any("panic", r),
				zap.Int("batch", len(prepared)),
				zap.ByteString("stack", debug.Stack()),
			)
		}
	}()
	l.runExtractionBatch(ctx, prepared)
}

// runExtractionBatch wraps applyExtraction + applyResolution in a
// tracked job when a JobStarter is installed. Without a starter the
// behaviour matches the pre-instrumentation loop: extract, resolve,
// done. The job's title carries the batch size so the Jobs panel can
// distinguish a single-episode remember from a burst-ingest batch.
func (l *FileEventLog) runExtractionBatch(ctx context.Context, prepared []Event) {
	l.mu.RLock()
	starter := l.jobStarter
	l.mu.RUnlock()

	if starter == nil {
		l.applyExtraction(ctx, prepared)
		l.applyResolution(ctx, prepared)
		return
	}
	rep, jobCtx := starter.Begin(ctx, "extraction", fmt.Sprintf("Extract %d event(s)", len(prepared)))
	// Close the job even if applyExtraction/applyResolution panics. Without
	// this, the panic unwinds past rep.Done() and the Jobs panel shows an
	// extraction that never completes. Re-raise the panic so runEnrichmentJob's
	// recover still logs the stack and drains the in-flight counters — the only
	// thing this defer adds is marking the job failed before the unwind
	// continues.
	done := false
	defer func() {
		if done {
			return
		}
		if r := recover(); r != nil {
			rep.Fail(fmt.Errorf("extraction panicked: %v", r))
			panic(r)
		}
		rep.Fail(errors.New("extraction returned before completion"))
	}()
	l.applyExtraction(jobCtx, prepared)
	rep.Update(0.5, "extraction complete, resolving")
	l.applyResolution(jobCtx, prepared)
	rep.Done()
	done = true
}

// Compile-time check that FileEventLog satisfies EventLog.
var _ EventLog = (*FileEventLog)(nil)

// ensureHLCInit seeds the hybrid logical clock exactly once from the bbolt
// sidecar (fast path) or by scanning the most recent day-file for the maximum
// EffectiveHLC (recovery path). It is called via sync.Once so it runs at most
// once regardless of concurrent callers. Errors during loading are silently
// swallowed: the worst outcome is a clock that starts at zero, which is safe
// (the clock will advance monotonically from the first Now() call).
//
// IMPORTANT: this must be called BEFORE appendUnderLock acquires l.mu, because
// the day-file fallback path calls openCommitted → l.mu.RLock(), which would
// deadlock if l.mu is already write-locked by the caller.
func (l *FileEventLog) ensureHLCInit(ctx context.Context) {
	l.hlcInitOnce.Do(func() {
		// Fast path: load from the bbolt sidecar written on previous runs.
		// Guard with an existence check so withIndex (which creates the file on
		// demand) is never called before any event has been appended — that would
		// create the events/ directory and the bolt file for a rejected Append,
		// breaking the invariant that a rejected-event Append leaves no files.
		if _, serr := os.Stat(l.indexPath()); serr == nil {
			if hlcStr, found, err := l.loadHLCFromIndex(); err == nil && found {
				l.hlcClock.Load(hlcStr)
				return
			}
		}
		// Recovery / fresh-start path: scan the last day file for max EffectiveHLC.
		// Mirrors the index-recovery posture for the UID index.
		files, ferr := listEventFiles(l.dataDir)
		if ferr != nil || len(files) == 0 {
			return
		}
		last := files[len(files)-1]
		noop := func(Event, error) bool { return true }
		recs, ok, _, scanErr := l.loadEventFile(ctx, last.path, noop)
		if !ok || scanErr != nil {
			// Hard read error or parse error; recs may be nil or partial.
			// Best-effort: clock stays at zero and advances from first Now().
			return
		}
		var maxHLC string
		for i := range recs {
			if h := recs[i].ev.EffectiveHLC(); h > maxHLC {
				maxHLC = h
			}
		}
		if maxHLC != "" {
			l.hlcClock.Load(maxHLC)
		}
	})
}

// Append validates, time-stamps, and writes events to events/YYYY/MM/DD.jsonl
// under the writer's data dir. Per touched file: a single content fsync,
// plus a directory fsync when the file was newly created so its directory
// entry is durable across a crash. Cross-day batches commit atomically: if
// any bucket fails after an earlier bucket has been fsynced, the earlier
// bucket is rolled back (truncated, or deleted if newly created) so the
// caller's retry is safe.
func (l *FileEventLog) Append(ctx context.Context, events []Event) ([]Receipt, error) {
	if len(events) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: context: %w", err)
	}

	// Seed the HLC from the sidecar before acquiring the write lock. The day-
	// file fallback calls openCommitted → l.mu.RLock(), which must not be
	// attempted while l.mu is already write-locked (deadlock). sync.Once
	// guarantees exactly-one execution regardless of concurrent callers.
	l.ensureHLCInit(ctx)

	prepared, err := l.appendUnderLock(ctx, events)
	if err != nil {
		return nil, err
	}

	// Persist the last HLC of this batch to the sidecar for crash recovery.
	// Best-effort: a failure here does not roll back the durable JSONL bytes;
	// the next open re-derives the clock from the day files instead.
	if n := len(prepared); n > 0 {
		if lastHLC := prepared[n-1].HLC; lastHLC != "" {
			if err := l.putHLCLast(lastHLC); err != nil {
				logging.FromContext(ctx).Debug("eventlog: persist hlc stamp failed (best-effort)",
					zap.Error(err))
			}
		}
	}

	// Fan out to the derived stores (issue #55) OUTSIDE l.mu. Two
	// invariants the placement preserves:
	//
	//  1. A stalled Apply (slow embedder, hung SQLite write) cannot
	//     pin the writer mutex: subsequent Append calls acquire l.mu
	//     freely, write their JSONL lines, and proceed.
	//  2. Apply runs under a context.WithoutCancel ctx — no deadline
	//     and no caller-cancel — so the dispatcher's cache layer,
	//     whose own singleflight worker survives on
	//     context.WithoutCancel, completes synchronously inside this
	//     call. That eliminates the race where a detached cache
	//     worker outlives Apply and writes to a dispatcher pool that
	//     Close has already torn down (Greptile #108 P1).
	//
	// The JSONL line is the source of truth: if Apply fails or stalls,
	// the log still holds every event and Rebuild (#27) reconciles
	// the indexes.
	l.applyToStorage(ctx, prepared)

	// Bump the runtime counters AFTER the JSONL is durable (the events
	// are committed even if applyToStorage stalls). Drives the Usage
	// panel's events-today card and the source-attribution buckets.
	for _, ev := range prepared {
		l.statsStore.IncEvent(string(ev.Kind))
		l.statsStore.IncBytes(int64(len(ev.Payload)))
	}

	receipts := make([]Receipt, len(prepared))
	for i, ev := range prepared {
		receipts[i] = Receipt{UID: ev.UID, CreateTime: ev.CreateTime}
	}
	return receipts, nil
}

// appendUnderLock executes the JSONL fsync + bbolt sidecar update
// under l.mu, then releases the lock before returning. Pulling these
// steps into a helper keeps Append's hot path linear (mu-acquire →
// helper → mu-release → fanout → receipts) and lets defer handle the
// unlock cleanly across every error path.
func (l *FileEventLog) appendUnderLock(ctx context.Context, events []Event) ([]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Re-check the context after acquiring the lock so a request that
	// timed out while blocked behind a slow fsync does not still become
	// durable.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: context: %w", err)
	}

	prepared := make([]Event, len(events))
	now := l.clock().UTC()
	for i, ev := range events {
		if err := l.prepare(ctx, &ev, now); err != nil {
			return nil, err
		}
		prepared[i] = ev
	}

	if err := l.checkUIDsUnique(ctx, prepared); err != nil {
		return nil, err
	}

	plans, err := l.planBuckets(prepared)
	if err != nil {
		return nil, err
	}

	entries, err := l.commitPlans(plans)
	if err != nil {
		return nil, err
	}

	if err := l.putIndex(entries); err != nil {
		// JSONL bytes are durable on disk and fsync'd. The index is the
		// derived sidecar, not the source of truth: surface the error
		// without rolling back the log. But the sidecar is now behind
		// the log, and the next Append's uniqueness check would trust
		// a stale index. Invalidate the sidecar so the next call
		// rebuilds from JSONL before accepting more events.
		if ierr := l.invalidateIndex(); ierr != nil {
			return nil, fmt.Errorf("eventlog: update index: %w (also failed to invalidate: %w)", err, ierr)
		}
		return nil, fmt.Errorf("eventlog: update index: %w", err)
	}

	return prepared, nil
}

// applyToStorage dispatches prepared into the derived four-layer
// store. Errors are warn-logged and dropped: the JSONL line is
// durable, and Rebuild (#27) reconciles the indexes from the log.
// Kept as a method rather than inlined so Append's hot path reads
// top-to-bottom.
//
// The Apply call runs under context.WithTimeout(WithoutCancel(ctx),
// defaultApplyTimeout): the ctx inherits the caller's values (logger,
// tracing) but is detached from caller cancellation and bounded by a
// finite deadline. Two failure modes the contract closes:
//
//  1. Caller-deadline pin-through. A caller whose ctx expires the
//     instant after fsync would otherwise see Apply return ctx.Err()
//     and the four derived stores silently skip the batch even
//     though receipts went out. WithoutCancel detaches that.
//  2. Stuck dispatcher. A wedged embedder, graph write, or SQLite
//     call must not leave Append blocking forever (it would never
//     return receipts) or pin Dispatcher.Close indefinitely. The
//     bounded deadline lets us warn-log and continue inside a known
//     wall-clock budget.
//
// The cache-singleflight race the bounded ctx would otherwise expose
// (a CachingEmbedder worker outliving the deadline and racing
// Dispatcher.Close on the SQLite pools) is owned at the cache's own
// boundary: Dispatcher.Close calls CachingEmbedder.Wait before tearing
// down pools, so any worker still pending after the deadline drains
// against live connections. See dispatcher.go:Close and
// cache/embedding.go:Wait.
//
// Append releases l.mu before calling this helper, so a stalled Apply
// blocks only the calling goroutine — concurrent Appends proceed.
func (l *FileEventLog) applyToStorage(ctx context.Context, prepared []Event) {
	if l.storage == nil {
		// No storage means no four-layer derived state; extraction +
		// resolution still need to fire so a test that wires only an
		// extractor can observe its side effects.
		l.scheduleBackgroundEnrichment(ctx, prepared)
		return
	}
	applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultApplyTimeout)
	defer cancel()
	if err := l.storage.Apply(applyCtx, prepared); err != nil {
		logging.FromContext(ctx).Warn(
			"eventlog: storage apply failed (log still durable)",
			zap.Error(err),
			zap.Int("events", len(prepared)),
		)
		return
	}
	l.scheduleBackgroundEnrichment(ctx, prepared)
}

// BackgroundQueueDepth returns the current number of extraction jobs sitting
// on the bounded worker pool's queue (jobs that scheduleBackgroundEnrichment
// enqueued but no worker has dequeued yet). Returns 0 when the worker pool is
// disabled (extractionWorkers <= 0, no queue exists) and when the queue is
// empty. Read-only; safe to call from any goroutine.
//
// Operators use this to observe backlog under ingestion bursts: a value
// climbing toward extractionWorkers * extractionQueueCapPerWorker means
// Append is enqueuing faster than the workers can drain Ollama, and the next
// Append will block on the send to the channel rather than returning fast.
// The metric is sampled state, not a counter, so two consecutive reads
// taken without a synchronization barrier may differ by races with workers.
func (l *FileEventLog) BackgroundQueueDepth() int {
	if l.extractionQueue == nil {
		return 0
	}
	return len(l.extractionQueue)
}

// BackgroundWorkDepth returns the current count of enqueued-or-running
// background enrichment jobs (extraction + resolution). Read from an
// atomic counter held in lockstep with bgWork.Add/Done, so the value
// is consistent across goroutines without holding a lock. A return
// value of zero implies the log is quiescent on the enrichment side
// (it does NOT cover dispatcher.bgVec — embedding upserts are a
// separate drain primitive, see Dispatcher.BackgroundVecDepth).
//
// Use for progress polling, not for tight termination checks: under
// rapid Append followed by Done, the counter can race down to zero
// for a tick between batches and back up. Callers that need a
// stable "fully idle" signal should use WaitForBackgroundWork.
func (l *FileEventLog) BackgroundWorkDepth() int64 {
	return l.bgWorkDepth.Load()
}

// ExtractionQueueDepth returns the number of enrichment jobs sitting
// in the bounded worker pool's queue (not yet picked up by a worker).
// Always <= BackgroundWorkDepth; the difference is the number of jobs
// currently in flight inside a worker. Returns 0 when the bounded
// pool is disabled (extractionWorkers <= 0, the unit-test default).
func (l *FileEventLog) ExtractionQueueDepth() int {
	if l.extractionQueue == nil {
		return 0
	}
	return len(l.extractionQueue)
}

// WaitForBackgroundWork blocks until every extraction + resolution goroutine
// spawned by a prior Append has returned, or ctx is canceled. The recall
// surface uses this to observe a quiescent four-layer store after a burst
// of remembers; without it, a feeder loop that finishes ingesting and
// immediately searches would hit empty entity / mention / relation tables
// because the LLM extractor was still mid-call.
//
// Returns ctx.Err() when the caller's context expires while work is still
// in flight; callers that prefer to search partial state should pass a
// bounded context and treat the error as a soft signal. Cheap to call when
// no work is in flight (the WaitGroup counter is zero). Tests use this as
// a deterministic alternative to sleeping.
func (l *FileEventLog) WaitForBackgroundWork(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		l.bgWork.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("eventlog: wait for background work: %w", ctx.Err())
	}
}

// passiveCaptureSourcePrefix marks Episode events produced by passive,
// always-on client lifecycle hooks — `maz hook stop` writes
// "hook:claude-code/stop" on every conversation turn (see
// internal/cli/hook_capture.go). These ambient per-turn captures are
// stored durably (searchable via recall/FTS/vector) but are deliberately
// EXCLUDED from automatic entity/relation extraction. The convention is a
// source prefix rather than the exact string so any future client's Stop
// hook ("hook:<client>/stop") inherits the same passive treatment;
// deliberate writes use other sources ("mcp:remember", "cli:remember",
// connector pulls) and keep full extraction.
const passiveCaptureSourcePrefix = "hook:"

// isPassiveCaptureSource reports whether an Episode source denotes a
// passive ambient capture that must not trigger background enrichment.
func isPassiveCaptureSource(source string) bool {
	return strings.HasPrefix(source, passiveCaptureSourcePrefix)
}

// dropPassiveCaptureEpisodes returns prepared with passive-capture Episode
// events removed; every other event (Mentions, Entities, Relations, and
// deliberately-sourced Episodes) passes through untouched. The common case
// — a batch with no passive captures — returns the input slice unchanged
// after a single cheap scan, so non-hook Appends pay no allocation.
func dropPassiveCaptureEpisodes(prepared []Event) []Event {
	drop := 0
	for i := range prepared {
		if string(prepared[i].Kind) == kindEpisode && isPassiveCaptureSource(prepared[i].Source) {
			drop++
		}
	}
	if drop == 0 {
		return prepared
	}
	kept := make([]Event, 0, len(prepared)-drop)
	for i := range prepared {
		if string(prepared[i].Kind) == kindEpisode && isPassiveCaptureSource(prepared[i].Source) {
			continue
		}
		kept = append(kept, prepared[i])
	}
	return kept
}

// scheduleBackgroundEnrichment runs applyExtraction + applyResolution on a
// detached goroutine so Append returns as soon as the JSONL is durable and
// storage.Apply has fanned out the four-layer derived state. Extraction
// calls a remote LLM whose tool-use latency routinely exceeds the eval
// feeder's 30-second timeout; pushing it off the hot path keeps remember()
// fast enough for the harness while leaving the work in the same process.
// Close drains bgWork before tearing the log down, so callers that need to
// observe post-extraction state can do so deterministically.
func (l *FileEventLog) scheduleBackgroundEnrichment(ctx context.Context, prepared []Event) {
	if l.extractor == nil && l.resolver == nil {
		return
	}
	// Passive ambient captures (a client lifecycle Stop hook fires every
	// conversation turn; see internal/cli/hook_capture.go) are already
	// durable and searchable via storage.Apply, but must NOT enter the
	// extraction/resolution pipeline: auto-extracting every turn floods the
	// graph with mis-typed, low-signal entities. Drop them before scheduling
	// any work — this is the single gate that distinguishes passive capture
	// from a deliberate `remember`, and it gates both the Append path and the
	// boot-time DrainUnextractedEpisodes re-enqueue (the source is persisted,
	// so the gate survives restart). If nothing else remains, there is no
	// enrichment to do.
	prepared = dropPassiveCaptureEpisodes(prepared)
	if len(prepared) == 0 {
		return
	}
	// Increment under the caller's goroutine so a subsequent Close
	// observes the new work even if the goroutine has not started yet.
	l.bgWork.Add(1)
	l.bgWorkDepth.Add(1)
	bgCtx := context.WithoutCancel(ctx)

	// Bounded worker pool: try to enqueue the job NON-BLOCKING so Append's hot
	// path stays sub-millisecond regardless of LLM latency. The deferred
	// bgWork.Done lives in runEnrichmentJob (worker loop and spill alike), so
	// the WaitGroup tracks "queued or running" as one notion of in-flight work.
	if l.extractionQueue != nil {
		// Take the schedule read-lock so Close cannot close the queue
		// while we are mid-send. Close acquires the write-lock before
		// flipping shuttingDown + closing the channels, so once we
		// observe shuttingDown=false under RLock the send is race-free.
		l.scheduleMu.RLock()
		if !l.shuttingDown {
			select {
			case l.extractionQueue <- extractionJob{ctx: bgCtx, prepared: prepared}:
				l.scheduleMu.RUnlock()
				return
			default:
				// Queue full; fall through to the bounded spill below.
			}
		}
		l.scheduleMu.RUnlock()

		// The pool queue is saturated (extractionWorkers*32 slots full, or the
		// enqueue lost a shutdown race). Spill onto a BOUNDED set of one-shot
		// goroutines: a sustained ingestion flood must NOT spawn an unbounded
		// herd that all block on Ollama's single chat slot until their deadlines
		// expire — that wastes memory and loses the work anyway. Acquire a spill
		// slot NON-BLOCKING (Append must stay off the critical path); if even the
		// spill is full, drop the enrichment with a loud warn. The episode is
		// already durable in the JSONL and a dropped enrichment is recoverable
		// via Rebuild, so this trades a silent late-timeout loss for an explicit,
		// bounded, logged one.
		select {
		case l.spillSlots <- struct{}{}:
			go func() {
				defer func() { <-l.spillSlots }()
				l.runEnrichmentJob(bgCtx, prepared)
			}()
		default:
			l.bgWork.Done()
			l.bgWorkDepth.Add(-1)
			logging.FromContext(bgCtx).Warn(
				"eventlog: enrichment backlog saturated; dropping batch (episode durable, recoverable via Rebuild)",
				zap.Int("batch", len(prepared)),
				zap.Int("queueCap", cap(l.extractionQueue)),
				zap.Int("spillCap", cap(l.spillSlots)),
			)
		}
		return
	}

	// No worker pool (extractionWorkers <= 0, the unit-test / legacy default):
	// run each enrichment on its own one-shot goroutine. Unbounded by design —
	// without a pool there is no backlog to bound, and tests rely on
	// WaitForBackgroundWork draining every scheduled job.
	go l.runEnrichmentJob(bgCtx, prepared)
}

// applyExtraction calls the injected extractor for each event in prepared and
// appends the resulting events (Mention + RelationCandidatesProposed) to the
// log. It runs under a detached context so extraction failures never propagate
// to the original Append caller; failures are warn-logged with the episode UID
// so the eval harness can count them.
//
// Calling l.Append from within this method is safe: Append's writer mutex
// (l.mu) is not held when applyExtraction runs (applyToStorage is called
// outside l.mu), so the inner Append acquires the lock normally. The inner
// Append triggers applyToStorage again, but because the extractor returns nil
// for non-episode events the recursion is always one level deep.
func (l *FileEventLog) applyExtraction(ctx context.Context, prepared []Event) {
	if l.extractor == nil {
		return
	}
	extractCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.extractionTimeout)
	defer cancel()

	l.recordEpisodeUIDsForMentions(prepared)

	for i := range prepared {
		ev := prepared[i]
		if l.alreadyExtracted(ev.UID) {
			logging.FromContext(ctx).Debug(
				"eventlog: extraction skipped, downstream mention present",
				zap.String("episodeUID", ev.UID),
			)
			continue
		}
		if string(ev.Kind) == kindEpisode && l.hasDownstreamMentions(extractCtx, ev) {
			logging.FromContext(ctx).Debug(
				"eventlog: extraction skipped, downstream mention found in storage scan",
				zap.String("episodeUID", ev.UID),
			)
			continue
		}
		evts, err := l.extractor.ExtractFrom(extractCtx, ev)
		if err != nil {
			logging.FromContext(ctx).Warn(
				"eventlog: extraction failed (episode still durable)",
				zap.String("episodeUID", ev.UID),
				zap.String("kind", string(ev.Kind)),
				zap.Error(err),
			)
			continue
		}
		if len(evts) == 0 {
			continue
		}
		if _, appendErr := l.Append(extractCtx, evts); appendErr != nil {
			logging.FromContext(ctx).Warn(
				"eventlog: append extracted events failed",
				zap.String("episodeUID", ev.UID),
				zap.Int("extractedEvents", len(evts)),
				zap.Error(appendErr),
			)
		}
	}
}

// kindMention / kindEpisode / kindEntity are the ontology Kind strings
// for the event kinds this file inspects inline. We copy the literals
// rather than importing the ontology package to avoid a kernel →
// ontology dependency loop; the constants live at
// ontology.KindMention / ontology.KindEpisode / ontology.KindEntity
// and the two must stay in lockstep (guarded by the package-level
// extraction-skip tests).
const (
	kindMention  = "mention"
	kindEpisode  = "episode"
	kindEntity   = "entity"
	kindRelation = "relation"
)

// recordEpisodeUIDsForMentions scans prepared for Mention events and
// records the episode UID each Mention references in l.extractedEpisodeUIDs.
// This is the seam that lets pull-time replay short-circuit non-
// deterministic LLM re-extraction: a chunk replay surfaces both
// Episode and its derived Mentions in the same Append batch (or in
// consecutive batches inside the same daemon process), so the first
// time we see a Mention we mark its source Episode as "already
// extracted on the originating device" and the for-loop in
// applyExtraction skips ExtractFrom for it. Cheap O(n) scan over the
// batch; the map is in-memory only and bounded by the number of
// distinct episodes the daemon process has seen.
func (l *FileEventLog) recordEpisodeUIDsForMentions(prepared []Event) {
	uids := make([]string, 0, len(prepared))
	needed := false
	for i := range prepared {
		if string(prepared[i].Kind) != kindMention {
			continue
		}
		uid := extractSourceEpisodeUID(prepared[i].Payload)
		if uid == "" {
			continue
		}
		uids = append(uids, uid)
		needed = true
	}
	if !needed {
		return
	}
	l.extractedMu.Lock()
	for _, uid := range uids {
		l.insertExtractedLocked(uid)
	}
	l.extractedMu.Unlock()
}

// insertExtractedLocked adds uid to the bounded dedup set, evicting
// the oldest entry on overflow. Caller must hold l.extractedMu.
// FIFO eviction is deliberate (not LRU access-order) — the dominant
// use case is pull-replay arriving in burst, so the head of the
// queue is also the events least likely to be seen again, and the
// extra bookkeeping a true LRU would need (doubly-linked list) buys
// nothing measurable.
func (l *FileEventLog) insertExtractedLocked(uid string) {
	if _, ok := l.extractedEpisodeUIDs[uid]; ok {
		return
	}
	if len(l.extractedEpisodeUIDOrder) >= extractedEpisodeUIDsCap {
		evict := l.extractedEpisodeUIDOrder[0]
		l.extractedEpisodeUIDOrder = l.extractedEpisodeUIDOrder[1:]
		delete(l.extractedEpisodeUIDs, evict)
	}
	l.extractedEpisodeUIDs[uid] = struct{}{}
	l.extractedEpisodeUIDOrder = append(l.extractedEpisodeUIDOrder, uid)
}

// alreadyExtracted reports whether the given episode UID has been
// seen referenced by at least one Mention in the current daemon's
// lifetime. Mentions are extractor output, so a positive answer means
// extraction has happened upstream and we MUST NOT redo it
// (extractor calls are LLM-backed and non-deterministic — repeating
// them on every pull produces divergent canonical Entities and
// silently bloats the event log).
func (l *FileEventLog) alreadyExtracted(uid string) bool {
	if uid == "" {
		return false
	}
	l.extractedMu.Lock()
	_, ok := l.extractedEpisodeUIDs[uid]
	l.extractedMu.Unlock()
	return ok
}

// extractSourceEpisodeUID parses a Mention payload's sourceEventUid
// field without unmarshaling the entire struct. Returns "" if the
// field is missing or malformed; callers treat that as a no-op
// signal (the Mention is still durable, we just can't credit its
// source Episode for skip purposes).
func extractSourceEpisodeUID(payload []byte) string {
	var probe struct {
		SourceEventUID string `json:"sourceEventUid"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return ""
	}
	return probe.SourceEventUID
}

// hasDownstreamMentions scans the persisted event log for any
// Mention whose sourceEventUid points at the given Episode's UID.
// This is the cross-restart correctness path: a daemon that ran,
// pulled chunks containing only Mentions for some Episode (which
// arrived earlier on another device), then restarted, would have
// an empty extractedEpisodeUIDs LRU at boot. When the Episode
// itself later lands on a subsequent pull, the LRU would miss and
// the in-memory same-batch check would also miss; this fallback
// reads the JSONL day-bucket directory and refuses extraction if
// a downstream Mention already exists on disk.
//
// Bounded by extractedFallbackScanWindow (7 days back from the
// Episode's CreateTime) to keep the scan cheap even for years-old
// logs. A Mention older than that is, by construction, in the
// dedup set if the daemon has been running steady-state; older
// stale Mentions for never-extracted Episodes are not a correctness
// problem (the LLM call still runs; the cost is one duplicated
// extraction, not a divergent graph).
//
// On any scan error we conservatively return false (don't skip).
// The extractor's existing failure handling will warn-log the LLM
// cost; that's strictly better than silently dropping an Episode.
//
// Caches the result in the LRU on hit so later batches in the same
// daemon process don't re-scan.
func (l *FileEventLog) hasDownstreamMentions(ctx context.Context, ev Event) bool {
	since := ev.CreateTime.Add(-time.Hour) // small overlap to absorb clock skew at boundaries
	until := ev.CreateTime.Add(extractedFallbackScanWindow)
	opts := ReadOpts{
		Since: since,
		Until: until,
		Kinds: []EventKind{kindMention},
	}
	for got, err := range l.Read(ctx, opts) {
		if err != nil {
			// Scan failure: bias to "we don't know"; extractor will
			// run, which is the safe default for correctness even
			// though it costs an extra LLM call.
			return false
		}
		if string(got.Kind) != kindMention {
			continue
		}
		if extractSourceEpisodeUID(got.Payload) != ev.UID {
			continue
		}
		l.extractedMu.Lock()
		l.insertExtractedLocked(ev.UID)
		l.extractedMu.Unlock()
		return true
	}
	return false
}

// DrainUnextractedEpisodes scans the event log for Episode events that have
// no downstream Mention and re-enqueues them through the existing
// scheduleBackgroundEnrichment path so the extraction worker pool (or the
// fallback per-Append goroutine) processes them. This is the boot-time
// catch-up pass for episodes that were appended while no extractor was active:
// an in-process one-shot ingest (no daemon), a crash mid-extraction, or any
// path that appended episodes before the extractor was wired all leave the log
// with un-extracted episodes that will never be processed otherwise, because
// scheduleBackgroundEnrichment is only called at Append time.
//
// The method is a no-op (returns 0, nil) when no extractor is installed
// (l.extractor == nil) — there is nothing to drain into.
//
// Re-enqueueing goes through scheduleBackgroundEnrichment, not the extractor
// directly, so the existing skip logic in applyExtraction applies:
// alreadyExtracted (in-memory LRU) and hasDownstreamMentions (persistent
// JSONL scan) both gate the call, so genuinely un-extracted episodes are
// processed while already-extracted ones are silently skipped. The returned
// count reflects how many episodes were passed to scheduleBackgroundEnrichment;
// some of those may be skipped inside applyExtraction if their mention is
// found during the drain scan.
//
// Passive ambient captures (hook:-prefixed sources, e.g. the per-turn
// `maz hook stop` capture) are excluded from the drain entirely: they are
// never enrichment candidates (see scheduleBackgroundEnrichment), so they
// are neither counted in queued nor passed downstream — otherwise a
// passive-heavy log would re-schedule a no-op enrichment for every capture
// on every boot.
//
// Known tradeoff: an episode that legitimately yields NO entities will have
// no downstream mention, so hasDownstreamMentions returns false and it will
// be re-enqueued on every boot. This is bounded and self-correcting (the LLM
// re-confirms empty extraction, which is cheap relative to a live episode).
// A persistent per-episode "extraction attempted" marker would eliminate the
// repeated work but requires a new event kind, which is deliberately out of
// scope here.
//
// Call this once after the extractor is installed and before the first user
// request. WaitForBackgroundWork blocks until the enqueued batch completes.
func (l *FileEventLog) DrainUnextractedEpisodes(ctx context.Context) (queued int, err error) {
	l.mu.RLock()
	ext := l.extractor
	l.mu.RUnlock()
	if ext == nil {
		return 0, nil
	}

	opts := ReadOpts{Kinds: []EventKind{kindEpisode}}
	for ev, readErr := range l.Read(ctx, opts) {
		if readErr != nil {
			return queued, fmt.Errorf("eventlog: drain backlog scan: %w", readErr)
		}
		if string(ev.Kind) != kindEpisode {
			continue
		}
		// Passive ambient captures are never enrichment candidates
		// (scheduleBackgroundEnrichment drops them; see
		// dropPassiveCaptureEpisodes). Skip them here too so a passive-heavy
		// log neither schedules a no-op enrichment per boot nor inflates the
		// returned queued count with work that is never performed.
		if isPassiveCaptureSource(ev.Source) {
			continue
		}
		l.scheduleBackgroundEnrichment(ctx, []Event{ev})
		queued++
	}
	return queued, nil
}

// applyResolution calls the injected resolver with the current Append batch
// and re-Appends the resulting Entity / Relation / RelationRetracted /
// PredicateRegistered events. It runs under a detached context so resolver
// failures never propagate to the original Append caller; failures are warn-
// logged for observability.
//
// Calling l.Append from within this method is safe: Append's writer mutex is
// not held here (applyToStorage runs outside l.mu), so the inner Append
// acquires the lock normally. The resolver returns nil for batches that
// contain only resolver-produced events (Entity, Relation, ...), so the
// recursion is bounded.
func (l *FileEventLog) applyResolution(ctx context.Context, prepared []Event) {
	if l.resolver == nil {
		return
	}
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.resolutionTimeout)
	defer cancel()

	evts, err := l.resolver.ResolveFrom(resolveCtx, prepared)
	if err != nil {
		logging.FromContext(ctx).Warn(
			"eventlog: resolution failed (batch still durable)",
			zap.Int("batchSize", len(prepared)),
			zap.Error(err),
		)
		return
	}
	if len(evts) == 0 {
		return
	}
	if _, appendErr := l.Append(resolveCtx, evts); appendErr != nil {
		logging.FromContext(ctx).Warn(
			"eventlog: append resolver events failed",
			zap.Int("resolverEvents", len(evts)),
			zap.Error(appendErr),
		)
	}
}

// prepare assigns UID, normalizes CreateTime, sets Name, and validates the
// payload. It mutates ev in place via the pointer receiver.
func (l *FileEventLog) prepare(ctx context.Context, ev *Event, now time.Time) error {
	if ev.UID == "" {
		uid, err := l.newULID()
		if err != nil {
			return fmt.Errorf("eventlog: generate uid: %w", err)
		}
		// Validate the generated UID just like a caller-supplied one. A
		// misconfigured or test-injected generator that returns a malformed
		// string with a nil error must not corrupt the log.
		if _, perr := ulid.Parse(uid); perr != nil {
			return fmt.Errorf("eventlog: ulid generator returned invalid value %q: %w", uid, perr)
		}
		ev.UID = uid
	} else if _, err := ulid.Parse(ev.UID); err != nil {
		return fmt.Errorf("%w: uid %q: %w", ErrInvalidEvent, ev.UID, err)
	}

	if ev.CreateTime.IsZero() {
		ev.CreateTime = now
	} else {
		ev.CreateTime = ev.CreateTime.UTC()
		if ev.CreateTime.After(now.Add(l.maxSkew)) {
			return fmt.Errorf("%w: createTime %s is more than %s in the future",
				ErrInvalidEvent, ev.CreateTime.Format(time.RFC3339Nano), l.maxSkew)
		}
	}

	ev.Name = "events/" + ev.UID

	// Scope defaulting + validation. Empty Scope is the on-disk shape
	// of pre-scope events read back from JSONL; coerce to the safe
	// default (personal) so legacy events never accidentally write
	// into a shared scope. A caller-supplied Scope must be in the
	// known enum; the writer refuses unknown values.
	if ev.Scope == "" {
		ev.Scope = DefaultScope
	} else if !ev.Scope.Valid() {
		return fmt.Errorf("%w: scope %q is not in {project|personal|team|draft}", ErrInvalidEvent, ev.Scope)
	}

	if err := l.ontology.Validate(ctx, string(ev.Kind), ev.Payload); err != nil {
		return fmt.Errorf("%w: kind %q: %w", ErrInvalidEvent, ev.Kind, err)
	}

	// Entity-kind membership check (P2 dynamic-ontology). See
	// checkEntityKindMembership for the warn-accept policy rationale.
	l.checkEntityKindMembership(ctx, ev)

	// Property-schema validation (P3 typed properties). See
	// checkEntityProperties for the strict-vs-warn-accept policy rationale.
	if err := l.checkEntityProperties(ctx, ev); err != nil {
		return err
	}

	// Relation domain/range + attribute-schema validation (P5 typed relations).
	// See checkRelationDomainRange for the strict-vs-warn-accept policy.
	if err := l.checkRelationDomainRange(ctx, ev); err != nil {
		return err
	}

	if err := l.enforceTeamWriteAuthority(ctx, ev); err != nil {
		return err
	}

	// Stamp the HLC before signing: CanonicalBytes v2 includes the hlc field,
	// so it must be final before the signature is computed. Graft events arrive
	// with a pre-set HLC (preserved verbatim by graftPersistOne, which bypasses
	// prepare) — the guard keeps re-entrant inner Appends from re-stamping them.
	if ev.HLC == "" {
		ev.HLC = l.hlcClock.Now(now)
	}

	// Stamp the mandatory signed authorship block. Done last, after every
	// field that feeds CanonicalBytes is final (UID, CreateTime, Scope,
	// Kind, Source, Confidence, Payload, HLC) and after all validation passes,
	// so a rejected event is never signed. See stampAuthor.
	if err := l.stampAuthor(ev); err != nil {
		return err
	}
	return nil
}

// enforceTeamWriteAuthority gates a LIVE team-scope write: it must be authored
// by an admitted member (the genesis self-grant is the one bootstrap exception,
// decided by teamAuth). Checked before signing so an unauthorized event is
// neither signed nor appended. Personal/project writes and single-user installs
// (teamAuth nil) skip this entirely. Extracted to keep prepare within the
// gocyclo gate.
func (l *FileEventLog) enforceTeamWriteAuthority(ctx context.Context, ev *Event) error {
	if ev.Scope != ScopeTeam || l.teamAuth == nil {
		return nil
	}
	// Mirror stampAuthor's guard: l.signer is never nil on the production wiring
	// path, but a construction path that installs teamAuth without a signer must
	// fail loudly here rather than panic on the Author() call below.
	if l.signer == nil {
		return fmt.Errorf("%w: no signer configured (team-scope write requires a provisioned identity)", ErrInvalidEvent)
	}
	authorPrincipalUID, _, _ := l.signer.Author()
	return l.teamAuth.AuthorizeTeamWrite(ctx, ev, authorPrincipalUID)
}

// stampAuthor populates ev.Author via the injected Signer. It runs the
// two-phase signing flow: read the signer's identity (principalUID,
// deviceID, sigKeyID) into the Author block, compute the canonical bytes
// over the now-complete identity + payload, then sign them. Called from
// prepare() under l.mu, so l.signer is read without an additional lock.
//
// L.signer is never nil — NewFileEventLog installs newLocalDefaultSigner()
// and the With/Set mutators ignore nil — but the guard is kept so a
// future construction path that forgets the default fails loudly here
// rather than writing an unsigned (and thus unverifiable) event.
func (l *FileEventLog) stampAuthor(ev *Event) error {
	if l.signer == nil {
		return fmt.Errorf("%w: no signer configured (every event must be signed)", ErrInvalidEvent)
	}
	// A team-scope event crosses the writer boundary, so it must be authored by
	// a provisioned identity — never the deterministic local-default signer,
	// whose seed is public (anyone could forge a matching signature). Genesis
	// installs the real Watchword-derived signer before any team write; this
	// guard fails closed if some path tries to write team scope before then.
	if ev.Scope == ScopeTeam {
		if principalUID, _, _ := l.signer.Author(); principalUID == localDefaultPrincipalUID {
			return fmt.Errorf("%w: team-scope event requires a provisioned identity (local default signer rejected)", ErrInvalidEvent)
		}
	}
	if err := SignEventAuthor(ev, l.signer); err != nil {
		return fmt.Errorf("%w: sign event: %w", ErrInvalidEvent, err)
	}
	return nil
}

// SignEventAuthor populates ev.Author from signer and signs the canonical
// bytes in place: it sets the (principalUID, deviceID, sigKeyID) triple, leaves
// Sig empty while computing CanonicalBytes (which excludes Sig), then fills Sig
// with the signature. Exported so the re-stamp migration can sign legacy events
// with a standalone signer without an event-log instance.
func SignEventAuthor(ev *Event, signer Signer) error {
	principalUID, deviceID, sigKeyID := signer.Author()
	ev.Author = EventAuthor{
		PrincipalUID: principalUID,
		DeviceID:     deviceID,
		SigKeyID:     sigKeyID,
		// Sig left empty: it is excluded from CanonicalBytes and filled below.
	}
	sig, err := signer.SignCanonical(ev.CanonicalBytes())
	if err != nil {
		return err
	}
	ev.Author.Sig = sig
	return nil
}

// checkRelationDomainRange validates a relation event against its predicate's
// declared domain/range and attribute schema (P5 typed relations). It fires
// only for relation events, and only when the configured ontology implements
// the relationValidator capability (SQLiteOntology with a live store).
// MemoryOntology does not satisfy the interface, so this is a no-op in tests and
// early-boot paths.
//
// Policy is config-gated, mirroring checkEntityProperties:
//   - strict_domain_range = true  → reject a real constraint violation
//     (ErrRelationValidation) with ErrInvalidEvent.
//   - strict_domain_range = false → warn-and-accept (default; autonomous
//     extraction is never blocked).
//
// Infrastructure failures (DB query, schema compile) always warn-and-accept so
// a transient or a misconfigured predicate schema does not gate writes.
func (l *FileEventLog) checkRelationDomainRange(ctx context.Context, ev *Event) error {
	if string(ev.Kind) != kindRelation {
		return nil
	}
	rv, ok := l.ontology.(interface {
		ValidateRelation(ctx context.Context, predicate, subjectUID, objectUID string, attributes json.RawMessage) error
	})
	if !ok {
		return nil
	}

	var rel struct {
		Predicate  string            `json:"predicate"`
		SubjectUID string            `json:"subjectUid"`
		ObjectUID  string            `json:"objectUid"`
		Attributes map[string]string `json:"attributes"`
	}
	if err := json.Unmarshal(ev.Payload, &rel); err != nil {
		// Malformed payloads were already rejected by ontology.Validate above;
		// skipping here is deliberate, not error swallowing.
		return nil //nolint:nilerr // skip relation check, never block on decode
	}
	if rel.Predicate == "" {
		return nil
	}

	var attrsJSON json.RawMessage
	if len(rel.Attributes) > 0 {
		raw, merr := json.Marshal(rel.Attributes)
		if merr == nil {
			attrsJSON = raw
		}
	}

	verr := rv.ValidateRelation(ctx, rel.Predicate, rel.SubjectUID, rel.ObjectUID, attrsJSON)
	if verr == nil {
		return nil
	}
	// Only reject in strict mode when the failure is a real domain/range or
	// attribute-constraint violation. Query/compile errors warn-and-accept.
	if l.strictDomainRange && errors.Is(verr, ErrRelationValidation) {
		return fmt.Errorf("%w: relation predicate %q: %w", ErrInvalidEvent, rel.Predicate, verr)
	}
	logging.FromContext(ctx).Warn(
		"eventlog: relation failed domain/range validation (warn-accept)",
		zap.String("predicate", rel.Predicate),
		zap.String("uid", ev.UID),
		zap.Bool("strictDomainRange", l.strictDomainRange),
		zap.Error(verr),
	)
	return nil
}

// checkEntityProperties validates an entity event's typed "properties" bag
// against the kind's composed property schema (P3 typed properties). It only
// fires for entity events whose payload contains a non-empty "properties"
// key, and only when the configured ontology implements the propertyValidator
// capability (SQLiteOntology with a live store). MemoryOntology does not
// satisfy the interface, so this is a no-op in tests and early-boot paths.
//
// Policy is config-gated:
//   - strict_attributes = true  → reject violation (ErrInvalidEvent)
//   - strict_attributes = false → warn-and-accept (default; autonomous
//     extraction is never blocked)
//
// DB/compile errors always warn-and-accept so infrastructure hiccups do not
// gate writes.
func (l *FileEventLog) checkEntityProperties(ctx context.Context, ev *Event) error {
	if string(ev.Kind) != kindEntity {
		return nil
	}
	pv, ok := l.ontology.(interface {
		ValidateEntityProperties(ctx context.Context, kind string, properties json.RawMessage) error
	})
	if !ok {
		return nil
	}
	var entityPayload struct {
		Kind       string          `json:"kind"`
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(ev.Payload, &entityPayload); err != nil {
		// Malformed payloads were already rejected by ontology.Validate
		// above; skipping here is deliberate, not error swallowing.
		return nil //nolint:nilerr // skip property check, never block on decode
	}
	if len(entityPayload.Properties) == 0 || entityPayload.Kind == "" {
		return nil
	}
	verr := pv.ValidateEntityProperties(ctx, entityPayload.Kind, entityPayload.Properties)
	if verr == nil {
		return nil
	}
	// Only reject in strict mode when the failure is a real property-constraint
	// violation (ErrPropertyValidation). DB query errors, schema compile errors,
	// extends-cycle errors, and other infrastructure failures always
	// warn-and-accept so transient or misconfigured storage does not gate writes.
	if l.strictAttributes && errors.Is(verr, ErrPropertyValidation) {
		return fmt.Errorf("%w: entity properties invalid for kind %q: %w",
			ErrInvalidEvent, entityPayload.Kind, verr)
	}
	// Demoted to Debug: this is an advisory note on a successful write.
	// At the default log level (info) it must be invisible so a one-shot
	// CLI command does not print internal paths or mem:// URIs to stderr
	// on an otherwise successful operation. Use --log-level=debug to see
	// these. Operators who want hard enforcement should enable
	// [ontology] strict_attributes = true in config.toml.
	logging.FromContext(ctx).Debug(
		"eventlog: entity properties failed property schema (warn-accept; enable strict_attributes to reject)",
		zap.String("entityKind", entityPayload.Kind),
		zap.String("uid", ev.UID),
		zap.Bool("strictAttributes", l.strictAttributes),
		zap.Error(verr),
	)
	return nil
}

// entityKindValidator is the optional interface that SQLiteOntology
// implements. MemoryOntology does not, so the check is a no-op in tests and
// early-boot paths.
type entityKindValidator interface {
	ValidateEntityKind(ctx context.Context, kind string) error
}

// checkEntityKindMembership fires for entity and mention events when the
// configured ontology satisfies entityKindValidator. The policy is
// warn-and-accept:
//   - The resolver registers the kind first; the entity write follows. A
//     race or a replayed event from a remote peer may arrive before the
//     local registry catches up.
//   - Rejecting would silently lose data.
//   - Auto-registering here would recurse back into the writer.
//
// DB errors also warn-and-accept (availability over strictness).
func (l *FileEventLog) checkEntityKindMembership(ctx context.Context, ev *Event) {
	ekv, ok := l.ontology.(entityKindValidator)
	if !ok {
		return
	}
	if string(ev.Kind) != kindEntity && string(ev.Kind) != kindMention {
		return
	}
	var raw struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(ev.Payload, &raw); err != nil || raw.Kind == "" {
		return
	}
	if err := ekv.ValidateEntityKind(ctx, raw.Kind); err != nil {
		logging.FromContext(ctx).Warn(
			"eventlog: entity kind not registered (warn-accept)",
			zap.String("entityKind", raw.Kind),
			zap.String("eventKind", string(ev.Kind)),
			zap.String("uid", ev.UID),
			zap.Error(err),
		)
	}
}

// dateKey is a tuple used as a map key when grouping events by UTC date.
type dateKey struct {
	year  int
	month time.Month
	day   int
}

func dateKeyOf(t time.Time) dateKey {
	y, m, d := t.UTC().Date()
	return dateKey{y, m, d}
}

func (k dateKey) path(root string) string {
	return filepath.Join(root, "events",
		strconv.Itoa(k.year),
		fmt.Sprintf("%02d", int(k.month)),
		fmt.Sprintf("%02d.jsonl", k.day))
}

// bucketByDate groups events by their UTC CreateTime date and returns the
// keys sorted chronologically. Sorting matters on two axes:
//
//   - **Cross-day**: callers may submit a batch whose events are not in
//     time order (replay, backfill). The rebuild path needs cross-day
//     ordering to be deterministic.
//   - **Within a day**: same-day events are stable-sorted by (CreateTime,
//     UID) for deterministic, compression-friendly disk layout. NOTE: this
//     sort governs WRITE order only — sortDayEvents in the read path uses
//     (EffectiveHLC, UID) instead. The two orderings can differ for
//     same-batch backfill (e.g. batch [11:00, 10:00] lands on disk as
//     [10:00, 11:00] for locality, but Read yields [11:00, 10:00] because
//     11:00 received HLC logical=0 and 10:00 received logical=1).
//
// Sorting is per-batch. Across batches, the on-disk order is the order
// in which Append calls completed; a same-day event in a later batch is
// appended after an earlier batch's events, even if its CreateTime is
// older. Cross-batch chronological ordering would require buffering whole
// files or a sorted index, neither of which fits an append-only
// single-pass log (see agent-docs/designs/event-log.md).
func bucketByDate(events []Event) (map[dateKey][]Event, []dateKey) {
	buckets := make(map[dateKey][]Event)
	for _, ev := range events {
		k := dateKeyOf(ev.CreateTime)
		buckets[k] = append(buckets[k], ev)
	}
	for k, batch := range buckets {
		sort.SliceStable(batch, func(i, j int) bool {
			if !batch[i].CreateTime.Equal(batch[j].CreateTime) {
				return batch[i].CreateTime.Before(batch[j].CreateTime)
			}
			return batch[i].UID < batch[j].UID
		})
		buckets[k] = batch
	}
	order := make([]dateKey, 0, len(buckets))
	for k := range buckets {
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.year != b.year {
			return a.year < b.year
		}
		if a.month != b.month {
			return a.month < b.month
		}
		return a.day < b.day
	})
	return buckets, order
}

// bucketPlan is a fully-encoded bucket ready to be written. Encoding
// happens up front so a marshal failure in any bucket aborts the whole
// Append before any I/O hits the disk.
//
// IsNew is a best-effort plan-time hint sourced from os.Stat, exposed
// to test simulators that swap writeOne via SetWriteOneFn. It is NOT
// authoritative for rollback decisions: between planBuckets and
// writeOne another writer may create or delete the file. The runtime
// signals returned by writeOne (createdByUs, observed) are the only
// sources commitPlans consults when deciding how to undo a bucket.
//
// Uids and offsets are aligned slices: offsets[i] is the byte offset of
// the i-th event's line within buf, and uids[i] is its UID. Together
// with the file's pre-batch size (returned by writeOne) they let
// commitPlans build the UID -> (file, offset) index entries without a
// second pass over the encoded buffer.
type bucketPlan struct {
	day     dateKey
	path    string
	buf     []byte
	uids    []string
	offsets []int64
	isNew   bool
}

// planBuckets pre-encodes every bucket and records a plan-time isNew
// hint by stat-ing each path. Returns an error if marshaling any bucket
// fails or if stat fails with anything other than "not exist".
func (l *FileEventLog) planBuckets(prepared []Event) ([]bucketPlan, error) {
	buckets, order := bucketByDate(prepared)
	plans := make([]bucketPlan, 0, len(order))
	for _, day := range order {
		path := day.path(l.dataDir)
		batch := buckets[day]
		buf, offsets, err := encodeBatch(batch)
		if err != nil {
			return nil, fmt.Errorf("eventlog: marshal day %04d-%02d-%02d: %w",
				day.year, day.month, day.day, err)
		}
		uids := make([]string, len(batch))
		for i, ev := range batch {
			uids[i] = ev.UID
		}
		isNew := false
		_, statErr := os.Stat(path)
		switch {
		case statErr == nil:
			isNew = false
		case errors.Is(statErr, os.ErrNotExist):
			isNew = true
		default:
			return nil, fmt.Errorf("eventlog: stat %s: %w", path, statErr)
		}
		plans = append(plans, bucketPlan{
			day:     day,
			path:    path,
			buf:     buf,
			uids:    uids,
			offsets: offsets,
			isNew:   isNew,
		})
	}
	return plans, nil
}

// commitMark remembers what to undo on a later failure during the same
// Append. Used to roll back earlier buckets when a later bucket fails to
// commit, preserving atomicity for cross-day batches.
type commitMark struct {
	path    string
	oldSize int64
	isNew   bool
}

// commitPlans writes each plan in chronological order. On any failure it
// rolls back every plan it already committed: files this writer created
// are removed entirely; pre-existing files are truncated back to the
// size observed by writeOne. Best-effort: rollback errors are silent
// because the caller already has a primary error in flight.
//
// A post-write failure (parent-dir fsync or file close after a successful
// content sync) leaves durable content on disk even though writeOne
// returned an error. When that happens, this bucket is added to the
// rollback set so undoCommits can restore the file. Otherwise a caller
// retrying the same Append would duplicate events on disk.
//
// Two runtime signals from writeOne drive the rollback decision, and
// the plan-time isNew hint is deliberately not consulted:
//
//   - createdByUs: proven by O_EXCL succeeding. Only when this is true
//     is the file ours to remove. The plan-time isNew flag would be
//     wrong when another writer creates the file between planBuckets
//     and writeOne's open; trusting it there causes undoCommits to
//     delete another writer's durable JSONL file.
//   - observed: writeOne saw the on-disk size before any append. Only
//     when this is true is the truncate-back target trustworthy. The
//     observed size already incorporates any bytes another writer
//     interleaved between planBuckets and writeOne.
//
// When neither flag is set, writeOne failed before it could either
// claim or observe the file. There is nothing to undo, so the bucket
// is intentionally not added to the rollback set; a pre-existing file
// owned by another writer stays untouched.
func (l *FileEventLog) commitPlans(plans []bucketPlan) ([]indexEntry, error) {
	commits := make([]commitMark, 0, len(plans))
	total := 0
	for _, p := range plans {
		total += len(p.uids)
	}
	entries := make([]indexEntry, 0, total)
	for _, p := range plans {
		if err := os.MkdirAll(filepath.Dir(p.path), 0o750); err != nil {
			undoCommits(commits)
			return nil, fmt.Errorf("eventlog: mkdir %s: %w", filepath.Dir(p.path), err)
		}
		oldSize, observed, createdByUs, err := l.writeOneFn(p)
		if err != nil {
			if createdByUs || observed {
				commits = append(commits, commitMark{
					path:    p.path,
					oldSize: oldSize,
					isNew:   createdByUs,
				})
			}
			undoCommits(commits)
			return nil, err
		}
		commits = append(commits, commitMark{
			path:    p.path,
			oldSize: oldSize,
			isNew:   createdByUs,
		})
		for i, off := range p.offsets {
			entries = append(entries, indexEntry{
				uid:    p.uids[i],
				day:    p.day,
				offset: oldSize + off,
			})
		}
	}
	return entries, nil
}

// writeOne opens the bucket file, writes the encoded batch, fsyncs the
// file, and (if this writer created the file) fsyncs the parent
// directory so the directory entry is durable across a crash.
//
// Returns three runtime signals along with any error:
//
//   - oldSize is the pre-batch file size as observed by this writer's
//     Stat. Meaningful only when observed is true.
//   - observed is true once Stat has succeeded; commitPlans uses it
//     to decide whether oldSize is a trustworthy truncate target.
//   - createdByUs is true when THIS writer was the one that created
//     the day file, as proven by O_EXCL succeeding. It is the only
//     runtime signal authoritative enough to drive the rollback's
//     remove-file decision: another writer may have created the file
//     between planBuckets's stat and this writer's open, so the
//     plan-time isNew hint cannot be trusted to mean "we own this
//     file". A pre-existing file is opened by retrying without
//     O_CREATE so the writer can still append.
//
// On early failure paths (OpenFile error before O_EXCL retry resolves,
// or initial Stat error), observed is false. CreatedByUs may still be
// true if O_EXCL succeeded before Stat failed; in that pathological
// case undoCommits will remove the empty file we just created.
func (l *FileEventLog) writeOne(p bucketPlan) (oldSize int64, observed bool, createdByUs bool, retErr error) {
	// #nosec G304 -- path is composed of the writer's internal dataDir
	// and the UTC date, never of caller-controlled input.
	f, err := os.OpenFile(p.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		createdByUs = true
	case errors.Is(err, os.ErrExist):
		// File already exists. Reopen without O_CREATE so we append
		// to the existing content; createdByUs stays false so any
		// later rollback truncates back instead of removing another
		// writer's durable file.
		// #nosec G304 -- same constraints apply on retry.
		f, err = os.OpenFile(p.path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, false, false, fmt.Errorf("eventlog: open %s: %w", p.path, err)
		}
	default:
		return 0, false, false, fmt.Errorf("eventlog: open %s: %w", p.path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("eventlog: close %s: %w", p.path, cerr)
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return 0, false, createdByUs, fmt.Errorf("eventlog: stat %s: %w", p.path, err)
	}
	oldSize = info.Size()
	observed = true

	n, werr := f.Write(p.buf)
	if werr != nil || n < len(p.buf) {
		if werr == nil {
			werr = fmt.Errorf("short write: %d of %d bytes", n, len(p.buf))
		}
		return oldSize, true, createdByUs, joinErr(
			fmt.Errorf("eventlog: write %s: %w", p.path, werr),
			rollback(f, oldSize))
	}

	if err := f.Sync(); err != nil {
		return oldSize, true, createdByUs, joinErr(
			fmt.Errorf("eventlog: fsync %s: %w", p.path, err),
			rollback(f, oldSize))
	}

	if createdByUs {
		if err := l.fsyncDirFn(filepath.Dir(p.path)); err != nil {
			return oldSize, true, createdByUs, fmt.Errorf("eventlog: fsync dir %s: %w",
				filepath.Dir(p.path), err)
		}
	}
	return oldSize, true, createdByUs, nil
}

// undoCommits walks committed buckets in reverse and undoes their writes.
// For newly-created files it removes the file; for pre-existing files it
// truncates back to the pre-batch size. Errors are intentionally swallowed:
// the caller already has a primary error in flight and a partial rollback
// is still better than none. Future M2 fault injection will tighten this.
func undoCommits(commits []commitMark) {
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		if c.isNew {
			if err := os.Remove(c.path); err != nil {
				_ = err // best-effort: caller already has a primary error
			}
			continue
		}
		// #nosec G304 -- path was just written by this writer.
		f, err := os.OpenFile(c.path, os.O_WRONLY, 0o600)
		if err != nil {
			continue
		}
		if err := f.Truncate(c.oldSize); err != nil {
			_ = err // best-effort
		}
		if err := f.Sync(); err != nil {
			_ = err // best-effort
		}
		if err := f.Close(); err != nil {
			_ = err // best-effort
		}
	}
}

// fsyncDir opens dir read-only and fsyncs it. On POSIX this persists the
// directory's metadata, including newly-added child entries, so a crash
// after Append returns cannot lose a brand-new day's JSONL file.
func fsyncDir(dir string) (retErr error) {
	// #nosec G304 -- dir is composed of the writer's internal dataDir
	// and the UTC date.
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() {
		if cerr := d.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close: %w", cerr)
		}
	}()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// encodeBatch marshals events back-to-back, one JSON object per line.
// Returns the full byte buffer ready for a single Write call along with
// the per-event byte offset of each line within the buffer; callers add
// the bucket's pre-batch file size to recover the absolute offset for
// the index. The initial capacity is a rough overestimate so most
// batches do not realloc.
func encodeBatch(batch []Event) ([]byte, []int64, error) {
	buf := make([]byte, 0, len(batch)*256)
	offsets := make([]int64, len(batch))
	for i, ev := range batch {
		offsets[i] = int64(len(buf))
		line, err := json.Marshal(ev)
		if err != nil {
			return nil, nil, fmt.Errorf("event %d: %w", i, err)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	return buf, offsets, nil
}

// rollback shrinks f back to oldSize, then fsyncs again. Both steps are
// best-effort but their errors are surfaced so a caller who sees a write
// failure also learns whether the file is actually torn-free or not.
func rollback(f *os.File, oldSize int64) error {
	var errs []error
	if err := f.Truncate(oldSize); err != nil {
		errs = append(errs, fmt.Errorf("eventlog: rollback truncate: %w", err))
	}
	if err := f.Sync(); err != nil {
		errs = append(errs, fmt.Errorf("eventlog: rollback fsync: %w", err))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("eventlog: rollback: %w", errors.Join(errs...))
}

// joinErr merges a primary error with an optional secondary, returning the
// primary alone when secondary is nil. The result behaves correctly under
// errors.Is and errors.As against the primary's chain.
func joinErr(primary, secondary error) error {
	if secondary == nil {
		return primary
	}
	return fmt.Errorf("eventlog: combined: %w", errors.Join(primary, secondary))
}

// Provenance delegates to the configured Storage so callers can look up
// the recorded source/confidence/decided-at for an Episode event. A nil
// storage (the test-only minimal log without a four-layer dispatcher)
// reports (nil, nil) , the same "no rows" signal as the interface.
func (l *FileEventLog) Provenance(ctx context.Context, eventUID string) ([]ProvenanceRecord, error) {
	if l.storage == nil {
		return nil, nil
	}
	return l.storage.Provenance(ctx, eventUID)
}

// Rebuild replays every event in the log via Storage.Apply,
// reconstructing the four derived stores from the JSONL source of truth.
// Use it on a fresh checkout (no derived stores yet), after a corruption
// (rm -rf the SQLite/LadybugDB files), or to reconcile a soft-
// inconsistency window (Append fanout failed; the log is durable, the
// indexes drifted).
//
// Semantics:
//   - Events are read via Read(ctx, ReadOpts{}), so they arrive in
//     chronological order with ULID monotonic tie-breaking (issue #25).
//   - Idempotent. Before calling Apply, Rebuild asks into.HasEvent(uid).
//     A true answer skips the event; a false answer dispatches it. The
//     dispatcher's own ON CONFLICT(uid) DO UPDATE upsert (#32) is the
//     second line of defence if HasEvent returns a false negative.
//   - Best-effort by default. Per-event Apply failures are collected in
//     the returned []ApplyError, warn-logged, and Rebuild continues. Pass
//     WithStrict to abort on the first failure instead. In strict mode
//     the offending ApplyError is wrapped in the returned error so
//     errors.As recovers the UID.
//   - Progress is streamed to the io.Writer installed via WithProgress
//     (default cadence: every defaultProgressEvery events). A terminal
//     summary line is always emitted when progress is non-nil, even for
//     short rebuilds. Format: "rebuilt {n} events  skipped={s}  elapsed=
//     {d}  rate={r}/s\n". Total event count is not known in advance (the
//     iterator is streaming), so the line reports counts and rate, not
//     ETA, satisfying the issue's "event count + ELT" intent without
//     fabricating an estimate.
//   - Single-pass over the iterator. No buffering.
//
// Rebuild does NOT acquire l.mu: it only reads from disk via Read, which
// takes its own RLock. Append calls can interleave; new events appended
// during a rebuild are picked up by the iterator if they land before
// Read's cursor and reconciled by a subsequent Rebuild otherwise.
// Operationally, the daemon pauses Append before invoking Rebuild on a
// corrupted store, but the contract does not require it. Future writer
// ops that mutate the on-disk log (compact, truncate, segment rotation)
// MUST coordinate with in-flight Rebuilds; today no such API exists.
func (l *FileEventLog) Rebuild(ctx context.Context, into Storage, opts ...RebuildOption) ([]ApplyError, error) { //nolint:gocognit // job-reporter integration adds one branch above the threshold; refactoring would obscure the linear progression.
	if into == nil {
		return nil, errors.New("eventlog: rebuild: storage is nil")
	}
	cfg := rebuildConfig{progressEvery: defaultProgressEvery}
	for _, opt := range opts {
		opt(&cfg)
	}

	logger := logging.FromContext(ctx)
	var applyErrs []ApplyError
	var applied, skipped, failed int
	start := time.Now()

	// Wire the optional job reporter. When cfg.jobs is non-nil, Begin
	// opens a tracked job and the deferred closure marks it Done or Fail.
	var jobRep JobReporter
	if cfg.jobs != nil {
		var jobCtx context.Context
		jobRep, jobCtx = cfg.jobs.Begin(ctx, "rebuild", "Rebuild event log")
		ctx = jobCtx
	}

	// Terminal progress summary fires on every exit path — normal
	// completion, ctx cancel, reader I/O error, HasEvent error, strict
	// abort. Without the defer, early returns would leave the operator
	// staring at a stale cadence line that under-counts the work done.
	defer func() {
		flushProgress(&cfg, applied, skipped, failed, start)
		if jobRep != nil {
			if len(applyErrs) > 0 || failed > 0 {
				jobRep.Fail(fmt.Errorf("rebuild: %d events failed to apply", failed))
			} else {
				jobRep.Done()
			}
		}
	}()

	for ev, rerr := range l.Read(ctx, ReadOpts{}) {
		// Honour ctx cancellation before reporting reader I/O so a
		// caller-cancelled rebuild always returns context.Canceled in
		// its error chain regardless of which side observed the
		// cancellation first.
		if err := ctx.Err(); err != nil {
			return applyErrs, fmt.Errorf("eventlog: rebuild: %w", err)
		}
		if rerr != nil {
			return applyErrs, fmt.Errorf("eventlog: rebuild: read: %w", rerr)
		}

		has, herr := into.HasEvent(ctx, ev.UID)
		if herr != nil {
			return applyErrs, fmt.Errorf("eventlog: rebuild: hasEvent uid=%s: %w", ev.UID, herr)
		}
		if has {
			skipped++
			maybeProgress(&cfg, applied, skipped, failed, start)
			continue
		}

		// One event flows through the team-scope verification gate and then
		// Apply. A failure in EITHER step is recorded identically — an
		// ApplyError at exact UID granularity — so a tampered team event is
		// excluded from the derived stores exactly like an apply fault. The
		// gate runs first so a bad signature never reaches Apply.
		evErr := l.verifyTeamEvent(ctx, &ev)
		if evErr == nil {
			// Apply runs under a detached context so a tight caller deadline
			// (an operator running `maz rebuild` with --timeout, or the daemon
			// shutting down) cannot rip a half-finished tx out of the
			// dispatcher. Same pattern as Append.applyToStorage. Outer rebuild
			// cancellation is still honoured at the top of each loop iteration.
			//
			// Apply per-event (single-element slice) so a failure surfaces at
			// exact event granularity rather than as an unattributed batch.
			applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultApplyTimeout)
			evErr = into.Apply(applyCtx, []Event{ev})
			cancel()
		}
		if evErr != nil {
			ae := ApplyError{UID: ev.UID, Err: evErr}
			applyErrs = append(applyErrs, ae)
			// Count failure before branching on strict so the deferred
			// terminal progress line reflects the abort in both modes.
			failed++
			if cfg.strict {
				return applyErrs, fmt.Errorf("eventlog: rebuild: strict abort: %w", ae)
			}
			logger.Warn("eventlog: rebuild event failed",
				zap.String("event_uid", ev.UID),
				zap.Error(evErr),
			)
			maybeProgress(&cfg, applied, skipped, failed, start)
			continue
		}
		applied++
		maybeProgress(&cfg, applied, skipped, failed, start)
	}

	return applyErrs, nil
}

// verifyTeamEvent runs the signature gate and, when a role checker is wired,
// the role-at-position gate for one event — the same function on live graft
// (GraftSealed) and on Rebuild replay. Only team-scope events with a
// configured verifier pay the ed25519-verify cost; the scope/nil-verifier
// short-circuit keeps personal/project replay unchanged.
//
// Role-at-position gate: reader-authored content events are dropped to
// Rejections. Every peer reaches the same verdict because the checker
// re-folds ONLY the membership events with a UID strictly below ev.UID
// (TeamRoleChecker) — a pure function of the membership set, so a demote or
// revoke that post-dates the event can never flip the verdict, whatever the
// arrival order. Membership and role lifecycle kinds are fold-governed and
// are exempt from this gate; only the signature gate applies to them.
func (l *FileEventLog) verifyTeamEvent(ctx context.Context, ev *Event) error {
	if ev.Scope != ScopeTeam || l.verifier == nil {
		return nil
	}
	if err := l.verifier.Verify(ctx, ev); err != nil {
		return err
	}

	// Device-revocation gate (position-pure): drop events authored by a
	// revoked device when an honored revocation's UID is strictly less than
	// the event's UID. This gate applies to ALL team-scope kinds — membership,
	// role, and device lifecycle included: a revoked (stolen) owner/admin
	// device must not be able to author grants, role changes, regrant
	// requests, or revenge-revocations. Bootstrap flows need no exemption: a
	// fresh enrollment or a revocation authored by a non-revoked device has no
	// earlier honored revocation against it, and a self-revocation sits AT its
	// own UID, not strictly before it. Concurrent cross-revocations resolve
	// first-wins by UID — the later one is authored by an already-revoked
	// device and drops, on every peer alike.
	if l.deviceRevokeChecker != nil {
		revoked, err := l.deviceRevokeChecker.DeviceRevokedAt(
			ctx, ev.Author.DeviceID, ev.Author.SigKeyID, ev.UID)
		if err != nil {
			return fmt.Errorf("device revocation check: %w", err)
		}
		if revoked {
			return fmt.Errorf("%w: device %q (key %q) was revoked before event %s",
				ErrPermission, ev.Author.DeviceID, ev.Author.SigKeyID, ev.UID)
		}
	}

	// Role-at-position: skip the check for membership and role lifecycle events —
	// the fold handles authority there. Only content events authored by a reader
	// are dropped.
	if l.teamRoleChecker == nil || isMembershipKind(ev.Kind) {
		return nil
	}
	role, ok, err := l.teamRoleChecker.TeamMemberRoleAt(ctx, ev.Author.PrincipalUID, ev.UID)
	if err != nil {
		return fmt.Errorf("team role check: %w", err)
	}
	if ok && role == "reader" {
		return fmt.Errorf("%w: reader may not author content events", ErrPermission)
	}
	return nil
}

// isMembershipKind reports whether k is a team membership, role lifecycle, or
// device identity lifecycle event kind. These events are fold-governed and
// exempt from the reader-content gate in verifyTeamEvent — the membership and
// revocation folds evaluate authority for them at their own position. They
// are NOT exempt from the device-revocation gate: a revoked device may not
// author membership or device lifecycle events either.
func isMembershipKind(k EventKind) bool {
	switch k {
	case "teamMemberGranted", "teamMemberRevoked", "teamRoleChanged", "teamRegrantRequested",
		"deviceEnrolled", "deviceRevoked", "principalRegistered":
		return true
	default:
		return false
	}
}

// maybeProgress emits a progress line when cadence is hit. The cadence
// counts applied, skipped, and failed events so a partial rebuild that
// skips heavily or errors often still emits regular updates instead of
// looking stuck. Package-level (not a method) because it does not touch
// FileEventLog state; the only inputs are the per-call config and
// counters.
func maybeProgress(cfg *rebuildConfig, applied, skipped, failed int, start time.Time) {
	if cfg.progress == nil {
		return
	}
	total := applied + skipped + failed
	if total == 0 || total%cfg.progressEvery != 0 {
		return
	}
	writeProgressLine(cfg.progress, applied, skipped, failed, start)
}

// flushProgress emits a terminal summary so the caller always sees a
// final count, even if the rebuild applied fewer than progressEvery
// events or exited via an early return path. No-op when progress is nil.
func flushProgress(cfg *rebuildConfig, applied, skipped, failed int, start time.Time) {
	if cfg.progress == nil {
		return
	}
	writeProgressLine(cfg.progress, applied, skipped, failed, start)
}

// writeProgressLine formats a single progress line. Rate uses elapsed
// wall-clock seconds; for sub-second rebuilds it still reports a rate
// using the float-second denominator so the line is informative.
//
// Write errors on the progress sink are deliberately discarded: progress
// is best-effort observability, not part of the rebuild contract. A
// closed pipe (operator hit Ctrl-C, daemon shutdown) must not abort an
// in-flight rebuild. Ctx cancellation, which is the right way to stop
// a rebuild, is honoured in the main loop.
func writeProgressLine(w io.Writer, applied, skipped, failed int, start time.Time) {
	elapsed := time.Since(start)
	var rate float64
	secs := elapsed.Seconds()
	if secs > 0 {
		rate = float64(applied) / secs
	}
	// errcheck: writer errors are intentionally ignored. See doc comment
	// above for the rationale (best-effort observability sink).
	//nolint:errcheck
	fmt.Fprintf(w, "rebuilt %d events  skipped=%d  failed=%d  elapsed=%s  rate=%.1f/s\n",
		applied, skipped, failed, elapsed.Truncate(time.Millisecond), rate)
}
