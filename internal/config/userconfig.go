package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// userConfigFile is the canonical filename inside the data directory.
const userConfigFile = "config.toml"

// userConfigMode is the permission set for the persisted file. The data dir
// itself is 0700 (per #46); the file is 0600 to match the owner-only stance.
const userConfigMode os.FileMode = 0o600

// CurrentUserConfigSchemaVersion is the schema version this build writes.
// Bump when the on-disk shape changes incompatibly; LoadUserConfig refuses
// versions it does not understand.
const CurrentUserConfigSchemaVersion = 1

// SecretURIScheme is the prefix used in TOML to reference a secret stored
// in the OS keychain (or any other secret.Provider implementation). The
// wizard writes "secrets://anthropic.api_key"; the plugin loader resolves
// it by trimming the prefix and calling secret.Provider.Get.
const SecretURIScheme = "secrets://"

// ErrUserConfigNotFound is returned by LoadUserConfig when no file exists
// at the resolved path. Callers can use errors.Is to distinguish first-run
// from a corrupt file.
var ErrUserConfigNotFound = errors.New("config: user config not found")

// UserConfig is the on-disk shape of ${data_dir}/config.toml. It is distinct
// from runtime [Config] (config.go), which loads process-level YAML at
// ~/.config/mazarbul/config.yaml. UserConfig is the per-data-dir surface
// `maz init` writes after the wizard's provider step; the M3 plugin loader
// will read it to decide which provider fills each slot.
//
// Schema discipline:
//   - snake_case TOML keys, per agent-docs/lifecycle-theming.md.
//   - Table per provider (not per slot); [defaults] picks which provider
//     fills each slot. A future Voyage embed plugin lands as
//     [providers.voyage] without disturbing the existing tables.
//   - Secrets are referenced, not stored: secret = "secrets://<name>".
//     The wizard writes the URI; the plugin loader resolves it via
//     secret.Provider.Get(strings.TrimPrefix(uri, "secrets://")).
//   - schema_version is the migration anchor. Bump on incompatible changes.
type UserConfig struct {
	SchemaVersion int                      `json:"schema_version" toml:"schema_version"`
	Providers     map[string]ProviderTable `json:"providers" toml:"providers"`
	Defaults      DefaultsTable            `json:"defaults" toml:"defaults"`
	Recall        RecallTable              `json:"recall,omitempty" toml:"recall,omitempty"`
	Composer      ComposerTable            `json:"composer,omitempty" toml:"composer,omitempty"`
	Ingestion     IngestionTable           `json:"ingestion,omitempty" toml:"ingestion,omitempty"`
	Connectors    ConnectorsTable          `json:"connectors,omitempty" toml:"connectors,omitempty"`
	Ontology      OntologyTable            `json:"ontology,omitempty" toml:"ontology,omitempty"`
	Log           UserLogTable             `json:"log,omitempty" toml:"log,omitempty"`
	Sync          SyncTable                `json:"sync,omitempty" toml:"sync,omitempty"`
	Vault         VaultTable               `json:"vault,omitempty" toml:"vault,omitempty"`
	WatchFS       WatchFSTable             `json:"watch_fs,omitempty" toml:"watch_fs,omitempty"`
	Output        OutputTable              `json:"output,omitempty" toml:"output,omitempty"`
	SavedRecalls  SavedRecallsTable        `json:"saved_recalls,omitempty" toml:"saved_recalls,omitempty"`
}

// WatchFSTable configures the agent-agnostic filesystem watcher (maz
// watch-fs). It captures every text-file change under Roots as a signed
// remember() episode, the only passive-capture path for hookless agents.
// Flags on the command override these values; an empty table leaves the
// watcher disabled until roots are supplied on the command line.
type WatchFSTable struct {
	// Roots is the set of directory trees to watch. Used as the default
	// when `maz watch-fs` is invoked without positional arguments and as
	// the watched set for the launchd-managed service.
	Roots []string `json:"roots,omitempty" toml:"roots,omitempty"`

	// Scope is the sync/encryption scope captured episodes land in:
	// personal, project, or team. Empty ⇒ project (file changes are
	// repo-bound), set by the command's default.
	Scope string `json:"scope,omitempty" toml:"scope,omitempty"`

	// SizeCapBytes bounds the bytes of file content captured per change.
	// Zero ⇒ watchfs.DefaultSizeCap (4 KB).
	SizeCapBytes int `json:"size_cap_bytes,omitempty" toml:"size_cap_bytes,omitempty"`

	// DebounceMs is the per-path debounce window in milliseconds. Zero ⇒
	// watchfs.DefaultDebounce (500 ms).
	DebounceMs int `json:"debounce_ms,omitempty" toml:"debounce_ms,omitempty"`

	// Ignore is an optional set of extra .gitignore-syntax patterns
	// applied across every root, on top of the default denylist and each
	// root's own .gitignore.
	Ignore []string `json:"ignore,omitempty" toml:"ignore,omitempty"`

	// Confidence is the writer confidence stamped on captured episodes,
	// range 0..1. Zero ⇒ the command's default (0.6): a file change is a
	// weaker signal than an explicit remember().
	Confidence float64 `json:"confidence,omitempty" toml:"confidence,omitempty"`
}

// OutputTable holds user preferences for CLI output rendering. Settings here
// apply as the third precedence layer (after --output flag and MAZ_OUTPUT env),
// so the user can make `maz recall` default to csv without touching every call
// site. The output package reads this via output.SetDefaultFormat, injected from
// root.go's PersistentPreRun after config load.
type OutputTable struct {
	// DefaultFormat is the fallback format when neither --output nor MAZ_OUTPUT
	// is set. Accepts the same values as --output: text|json|ndjson|csv|kv.
	// Empty string means "apply the TTY heuristic" (text on TTY, json off TTY).
	DefaultFormat string `json:"default_format,omitempty" toml:"default_format,omitempty"`
}

// SavedRecall stores a named recall query with its parameters.
// Used by `maz saved add` / `maz saved recall`.
type SavedRecall struct {
	// Query is the recall query string, required.
	Query string `json:"query" toml:"query"`
	// Limit is the maximum number of hits to return. Zero uses the recall default.
	Limit int `json:"limit,omitempty" toml:"limit,omitempty"`
	// MinScore drops hits below this fused score floor. Zero disables the floor.
	MinScore float64 `json:"min_score,omitempty" toml:"min_score,omitempty"`
	// Scopes restricts recall to the given scope names. Empty uses the recall
	// default (project + personal).
	Scopes []string `json:"scopes,omitempty" toml:"scopes,omitempty"`
}

// SavedRecallsTable maps user-defined names to their stored recall queries.
// The TOML encoding is a table-of-inline-tables, matching the [saved_recalls]
// section. The zero value (nil map) is valid and treated as empty.
type SavedRecallsTable map[string]SavedRecall

// VaultTable configures the editable markdown vault — the local
// materialization of event-sourced documents (documentRevision events) that
// humans edit with their editor of choice. The vault is DERIVED state,
// rebuildable from the event log at any time, so repointing Path never loses
// data.
type VaultTable struct {
	// Path is the vault directory. Empty ⇒ <dataDir>/vault. A relative path
	// resolves against the data dir. May point at an existing vault (e.g.
	// Obsidian): the projector only writes documents it owns (keyed by
	// vault-relative path) and deletes a file only when its document was
	// tombstoned AND the on-disk bytes still match the last known revision.
	Path string `json:"path,omitempty" toml:"path,omitempty"`

	// Watch enables the bidirectional file-watcher (P7). When true the daemon
	// starts an fsnotify watcher on the vault directory: editing a file appends
	// exactly one documentRevision, and a revision appended by an MCP tool or
	// sync re-materializes the file — with loop prevention via content-hash
	// idempotency (I1). Disabled by default because a misfiring watcher must
	// never corrupt the vault or loop. Enable only after verifying the vault
	// path is correctly configured.
	Watch bool `json:"watch,omitempty" toml:"watch,omitempty"`
}

// VaultPath resolves the vault directory for dataDir: the configured
// [vault] path when set (relative paths resolve against dataDir), else
// <dataDir>/vault.
func VaultPath(dataDir string, u *UserConfig) string {
	if u != nil && u.Vault.Path != "" {
		if filepath.IsAbs(u.Vault.Path) {
			return u.Vault.Path
		}
		return filepath.Join(dataDir, u.Vault.Path)
	}
	return filepath.Join(dataDir, "vault")
}

// SyncTable holds the per-scope Moria-sync configuration. A scope with
// no remote URL is locally-tracked-only: the sealer still rotates open
// chunks (so a future remote configuration can replay them) but
// Transport.Push is never called.
//
// AutoPush defaults false — operators must opt in to the network side
// of sync. Hand-edited config.toml is the source of truth; the Hub
// Settings → Sync tab writes to state.toml and overlays here.
type SyncTable struct {
	// Remotes maps scope name (project|personal|team) to its remote
	// configuration. Absent scopes are local-only.
	Remotes map[string]ScopeRemoteTable `json:"remotes,omitempty" toml:"remotes,omitempty"`

	// AutoPush gates the daemon's background push goroutine. False ⇒
	// sealer still seals on cadence but pushes are operator-triggered
	// only (`maz sync push`).
	AutoPush bool `json:"auto_push,omitempty" toml:"auto_push,omitempty"`

	// CoalesceSeconds is the minimum interval between consecutive
	// pushes per scope. A pointer so an explicit 0 (no coalescing)
	// is distinguishable from an absent key; nil leaves the daemon
	// default (30s). GitHub's free tier accepts ~6 pushes/min; 30s
	// sits comfortably inside that.
	CoalesceSeconds *int `json:"coalesce_seconds,omitempty" toml:"coalesce_seconds,omitempty"`

	// SealMaxEvents caps the per-chunk batch size for ENCRYPTED scopes
	// (`.maz` chunks). Zero ⇒ daemon default (10_000). Smaller chunks
	// are cheaper to fail-recover but pay more git-object overhead per
	// event. Encrypted chunks gzip-compress well, so 10k is a sensible
	// default — the on-disk size still lands in the 0.5–2 MiB range.
	SealMaxEvents int `json:"seal_max_events,omitempty" toml:"seal_max_events,omitempty"`

	// SealMaxEventsPlaintext caps the per-chunk batch size for
	// PLAINTEXT scopes (`.jsonl` chunks). Zero ⇒ daemon default
	// (2_000). Lower than the encrypted default because plaintext
	// chunks don't compress on disk, and github.com refuses to render
	// any file larger than ~1 MiB inline — keeping plaintext chunks
	// small preserves the "browse your memory on github" UX.
	SealMaxEventsPlaintext int `json:"seal_max_events_plaintext,omitempty" toml:"seal_max_events_plaintext,omitempty"`

	// SealIntervalSeconds is how often the daemon attempts a seal
	// rotation. Zero ⇒ daemon default (3600s, 1 hour). The seal
	// pass is idempotent so a tight interval costs only the disk
	// walk when no new events are present.
	SealIntervalSeconds int `json:"seal_interval_seconds,omitempty" toml:"seal_interval_seconds,omitempty"`

	// Identity controls how the daemon authors the git commits it pushes to a
	// scope's remote — the "bring your own git" knob that makes `git blame`,
	// `git log --author`, `git shortlog`, and GitHub contribution graphs
	// attribute the commit to the real user instead of a generic bot. A pointer
	// so an absent [sync.identity] block is nil (and so SyncTable stays small
	// enough to pass by value through the daemon's hot paths).
	Identity *SyncIdentityTable `json:"identity,omitempty" toml:"identity,omitempty"`
}

// SyncIdentityTable configures the git author/committer identity stamped on
// sync commits. It is independent of encryption: turning a scope encrypted
// never changes the author line (least surprise).
//
// CommitIdentity selects how the bound principal is rendered:
//
//   - "noreply" (default): a synthesized GitHub no-reply email
//     ("<id>+<login>@users.noreply.github.com"). The commit renders AS the
//     user on GitHub — avatar, profile, contribution graph — without exposing
//     the real email to clones or scrapers.
//   - "real": the principal's display name plus the operator's real
//     git-config email. Use only when you are comfortable publishing that
//     address to the remote host.
//   - "anonymous": the generic "Mazarbul" sentinel; no personal attribution.
//
// Name/Email are a verbatim escape hatch: when BOTH are set they override the
// resolved identity entirely (highest precedence), letting an operator pin any
// author string regardless of mode.
type SyncIdentityTable struct {
	CommitIdentity string `json:"commit_identity,omitempty" toml:"commit_identity,omitempty"`
	Name           string `json:"name,omitempty" toml:"name,omitempty"`
	Email          string `json:"email,omitempty" toml:"email,omitempty"`
}

// Commit-identity modes for SyncIdentityTable.CommitIdentity.
const (
	// CommitIdentityNoreply renders sync commits with a GitHub no-reply email
	// so they attribute to the user without leaking the real address. Default.
	CommitIdentityNoreply = "noreply"
	// CommitIdentityReal uses the operator's real git-config email.
	CommitIdentityReal = "real"
	// CommitIdentityAnonymous uses the generic "Mazarbul" sentinel author.
	CommitIdentityAnonymous = "anonymous"
)

// ScopeRemoteTable is the per-scope remote target. AuthMethod ∈
// {"ssh","https_pat","none"}; "" inherits from the SSH-default
// resolution path (~/.ssh/id_ed25519). The HTTPS PAT secret itself
// lives in the keychain under `mazarbul:sync:<scope>:pat` — never
// in the on-disk config.
//
// Encrypted is the per-scope privacy switch. Absent ≡ false (the
// plaintext default: events ship one-per-line to git so the
// host's diff/blame/history features work). Explicit true opts
// into XChaCha20-Poly1305 sealed chunks; the operator MUST have a
// Watchword in the keychain or sealing will skip that scope and
// `maz doctor` warns about the misconfiguration. Pointer so absent
// and explicit-false are wire-distinguishable for migrations.
type ScopeRemoteTable struct {
	URL        string `json:"url,omitempty" toml:"url,omitempty"`
	AuthMethod string `json:"auth_method,omitempty" toml:"auth_method,omitempty"`
	Encrypted  *bool  `json:"encrypted,omitempty" toml:"encrypted,omitempty"`

	// KeyMode selects how the scope's symmetric seal key is obtained.
	// "" / "watchword" ≡ per-scope HKDF derivation from the Watchword
	// root (watchword.DeriveScopeKey) — the single-writer default.
	// "envelope" selects per-team envelope encryption: a random DEK
	// (wrapped to each member, distributed via recipients.jsonl)
	// replaces HKDF derivation for the team scope. Set by `maz team
	// create`; only meaningful for the team scope.
	KeyMode string `json:"key_mode,omitempty" toml:"key_mode,omitempty"`
}

// Key-mode values for ScopeRemoteTable.KeyMode.
const (
	// KeyModeWatchword derives the scope key from the Watchword root via
	// per-scope HKDF. The default for personal/project (and team unless
	// envelope mode is opted into).
	KeyModeWatchword = "watchword"
	// KeyModeEnvelope seals the scope under a random per-epoch DEK wrapped
	// to each member's x25519 key — the team multi-writer path.
	KeyModeEnvelope = "envelope"
)

// EncryptedOrDefault returns the resolved encryption mode for a
// scope: explicit true/false from config when set, otherwise the
// plaintext default (false ≡ plaintext). Centralises the absent-pointer
// → false default so config readers do not each repeat the
// nil-deref dance.
func (r ScopeRemoteTable) EncryptedOrDefault() bool {
	if r.Encrypted == nil {
		return false
	}
	return *r.Encrypted
}

// Envelope reports whether the scope uses envelope (per-team DEK)
// encryption rather than Watchword-HKDF key derivation.
func (r ScopeRemoteTable) Envelope() bool {
	return r.KeyMode == KeyModeEnvelope
}

// UserLogTable is the user-editable subset of the runtime Config.Log
// struct: level and format are the two knobs operators flip from
// state.toml or the Settings TUI. Empty values inherit the runtime
// default (resolved from CLI flags + env + config.toml).
type UserLogTable struct {
	Level  string `json:"level,omitempty" toml:"level,omitempty"`
	Format string `json:"format,omitempty" toml:"format,omitempty"`
}

// OntologyTable holds optional knobs that govern the dynamic-ontology write
// path (P3 typed properties) and the P7 governance surface. Zero values
// preserve the pre-knob behaviour: a missing [ontology] block has no effect.
//
// The StrictAttributes toggle mirrors the warn-accept / reject gate on the
// entity write path described in agent-docs/designs/dynamic-ontology.md. It is
// intentionally a single coarse-grained bool for P3; the P7 governance phase
// layers per-kind governance on top via Mode.
type OntologyTable struct {
	// StrictAttributes, when true, causes the event log to reject Entity
	// events whose properties field fails to validate against the kind's
	// composed propertySchema (kernel.FileEventLog WithStrictAttributes).
	// When false (the default, zero value), violations are warn-logged and
	// the event is accepted — autonomous extraction is never blocked by
	// default. AIP-140 adjective form: no "is" prefix.
	StrictAttributes bool `json:"strict_attributes,omitempty" toml:"strict_attributes,omitempty"`

	// StrictDomainRange, when true, causes the event log to reject Relation
	// events whose subject/object entity kinds fall outside the predicate's
	// declared subjectKinds/objectKinds, or whose Attributes fail the
	// predicate's propertySchema (kernel.FileEventLog WithStrictDomainRange,
	// P5 typed relations). When false (the default, zero value), violations are
	// warn-logged and the event is accepted — autonomous extraction is never
	// blocked by default. AIP-140 adjective form: no "is" prefix.
	StrictDomainRange bool `json:"strict_domain_range,omitempty" toml:"strict_domain_range,omitempty"`

	// Mode selects the ontology governance level. Empty string or
	// "permissive" (the default) preserves the autonomy-first behaviour:
	// the resolver and defineEntityKind MCP tool register new entity kinds
	// as active immediately. "governed" routes all auto-minted and
	// explicitly defined kinds through a proposal queue: they are persisted
	// with status="proposed" and must be approved via `maz schema review
	// approve` before becoming active in the taxonomy.
	Mode string `json:"mode,omitempty" toml:"mode,omitempty"`

	// DedupHighThreshold is the cosine-similarity floor above which
	// defineEntityKind automatically reuses an existing kind instead of
	// minting a new one (the "reuse" path). Recommended value: 0.92.
	// Zero uses the dedup package default (ReuseThreshold = 0.90).
	DedupHighThreshold float64 `json:"dedup_high_threshold,omitempty" toml:"dedup_high_threshold,omitempty"`

	// DedupMediumThreshold is the cosine-similarity floor above which
	// defineEntityKind mints a new kind and automatically records a SKOS
	// closeMatch sameAs IRI pointing at the near-duplicate. Recommended
	// value: 0.82. Zero uses the dedup package default
	// (MintAliasThreshold = 0.75).
	DedupMediumThreshold float64 `json:"dedup_medium_threshold,omitempty" toml:"dedup_medium_threshold,omitempty"`
}

// Governed reports whether the ontology is in governed mode. In governed mode
// auto-minted and explicitly defined entity kinds are persisted as proposed and
// require explicit approval via `maz schema review approve` before becoming
// active in the taxonomy.
func (o OntologyTable) Governed() bool { return o.Mode == "governed" }

// IngestionTable holds optional knobs that apply during the write path
// (remember → extractor → dispatcher). Zero values preserve the
// pre-knob behaviour: a missing [ingestion] block has no effect.
//
// Phase 2 of the mem0-parity sweep introduced DedupSemanticThreshold so
// near-duplicate restatements ("I love oat milk" then "Oat milk is my
// favorite") collapse into one Episode instead of polluting recall with
// two near-identical hits.
type IngestionTable struct {
	// DedupSemanticThreshold is the cosine-similarity floor used to
	// suppress new Episode writes when an existing Episode is already
	// >= threshold similar to the candidate. Matches mem0 v3's NOOP
	// path. Zero (the default) disables the check; the recommended
	// production starting value is 0.85. Only fires when the dispatcher
	// has an embedder + vector connection wired.
	DedupSemanticThreshold float64 `json:"dedup_semantic_threshold,omitempty" toml:"dedup_semantic_threshold,omitempty"`
}

// ComposerTable holds the optional composer tuning knobs. Zero values
// preserve kernel defaults: the daemon only applies a knob when the
// field is non-zero, so an absent [composer] block has no effect.
//
// The Mazarbul LongMemEval harness writes its own [composer] block
// before running so operators can A/B-test thresholds without
// recompiling. Phase 2 of the mem0-parity plan introduced
// AbstainThreshold as the hard-floor refusal gate.
type ComposerTable struct {
	// AbstainThreshold is the FusedScore hard floor below which Compose
	// returns the abstain sentinel without calling the LLM. Zero (the
	// default) disables the floor and preserves v11 behaviour. Tunable
	// per the recall-score histogram from a 500q LongMemEval baseline:
	// 0.18 is a reasonable starting value.
	AbstainThreshold float32 `json:"abstain_threshold,omitempty" toml:"abstain_threshold,omitempty"`
}

// RecallTable holds the optional hybrid-recall tuning knobs. Zero values
// preserve kernel defaults: the daemon only applies a knob when the field
// is non-zero, so an absent [recall] block has no effect. The keys mirror
// the kernel.HybridOption setters one-to-one so the docs can point at a
// single canonical source.
//
// The Mazarbul LongMemEval harness writes its own [recall] block before
// running so operators can A/B-test thresholds without recompiling. The
// defaults below match the kernel's compile-time defaults; document the
// units inline so a TOML editor does not have to chase Go source.
type RecallTable struct {
	// TwoHopFloor is the minimum 1-hop entity count below which the graph
	// lane expands to 2-hop. A pointer so an explicit 0 (disable 2-hop
	// expansion) is distinguishable from an absent key; nil leaves the
	// kernel default (3). Feeds the entity lane in both RRF and weighted
	// fusion.
	TwoHopFloor *int `json:"two_hop_floor,omitempty" toml:"two_hop_floor,omitempty"`

	// GraphWeight is the RRF weight applied to the graph lane (relative to
	// 1.0 for vector and FTS). A pointer so an explicit 0.0 (disable the
	// graph lane's RRF contribution) takes effect instead of being read as
	// "unset" — the footgun that silently pinned the kernel default (1.0)
	// no matter what the operator wrote. nil leaves the default. RRF-only:
	// the weighted fusion mode (the kernel default) consumes the graph rank
	// directly, not this weight.
	GraphWeight *float64 `json:"graph_weight,omitempty" toml:"graph_weight,omitempty"`

	// NameEmbWeight is the RRF weight applied to the entity-name-embedding
	// lane. A pointer so an explicit 0.0 (disable the lane) is honored; nil
	// leaves the kernel default (1.0). RRF-only (see GraphWeight).
	NameEmbWeight *float64 `json:"name_emb_weight,omitempty" toml:"name_emb_weight,omitempty"`

	// RRFMinScore drops fused hits below this score. A pointer so an
	// explicit 0 (disable the floor) is distinguishable from an absent
	// key; nil leaves the kernel default (no floor). A small positive
	// value (e.g. 0.005) prunes the long-tail noise the reranker would
	// otherwise have to weed out.
	RRFMinScore *float32 `json:"rrf_min_score,omitempty" toml:"rrf_min_score,omitempty"`

	// EdgeAllowlist restricts 2-hop expansion to relations whose predicate
	// is in this list. Empty means the 2-hop lane uses every predicate;
	// non-empty narrows it (e.g. ["works_at", "located_in", "knows"]).
	EdgeAllowlist []string `json:"edge_allowlist,omitempty" toml:"edge_allowlist,omitempty"`

	// RerankEnabled opts in to LLM reranking after RRF fusion. Default
	// false: recall returns hits in deterministic FusedScore-descending
	// order regardless of whether a chat model is configured. Set true to
	// activate the post-fusion LLM reranker (requires a chat model).
	// Consistent with MMR (mmr_lambda > 0) and recency (recency_boost > 0)
	// both being opt-in.
	RerankEnabled bool `json:"rerank,omitempty" toml:"rerank,omitempty"`

	// MMRLambda controls Maximal Marginal Relevance after RRF fusion. A
	// pointer so an explicit 0 (disable MMR) is distinguishable from an
	// absent key; nil leaves MMR disabled (kernel default). 1.0 reduces
	// to pure relevance (FusedScore order); 0 < λ < 1 trades relevance
	// off against diversity. Carbonell & Goldstein 1998 cite 0.7 as the
	// production-consensus default for near-duplicate suppression.
	MMRLambda *float64 `json:"mmr_lambda,omitempty" toml:"mmr_lambda,omitempty"`

	// RecencyBoost scales the Gaussian recency multiplier applied after
	// RRF fusion: FusedScore *= (1 + boost · exp(-Δd²/(2σ²))). A pointer
	// so an explicit 0 (disable recency boost) is distinguishable from an
	// absent key; nil leaves the kernel default (disabled). 0.15 is the
	// production-consensus value. Clamped to [0, 1].
	RecencyBoost *float64 `json:"recency_boost,omitempty" toml:"recency_boost,omitempty"`

	// RecencyHalflifeDays is σ (in days) for the Gaussian recency
	// decay. Zero means kernel default (30 days, matching Elastic /
	// OpenSearch function_score gauss scale=30d, decay≈0.5). Only
	// consulted when RecencyBoost > 0.
	RecencyHalflifeDays float64 `json:"recency_halflife_days,omitempty" toml:"recency_halflife_days,omitempty"`

	// StrengthBoost scales the passive-reinforcement multiplier applied
	// during fusion: FusedScore *= (1 + boost · reinforcement(accessCount)
	// · decay). Zero leaves the kernel default (0.15, reinforcement as a
	// tiebreaker). Set explicitly via the pointer path to disable; see the
	// StrengthBoost override merge in state.go. Clamped to [0, 1].
	// Cognitive memory lifecycle, issue #231.
	StrengthBoost float64 `json:"strength_boost,omitempty" toml:"strength_boost,omitempty"`

	// StrengthHalflifeDays is σ (in days) for the Gaussian access-recency
	// decay of the reinforcement multiplier. Zero means kernel default
	// (30 days, matching recency_halflife_days). Only consulted when the
	// effective StrengthBoost > 0.
	StrengthHalflifeDays float64 `json:"strength_halflife_days,omitempty" toml:"strength_halflife_days,omitempty"`

	// SalienceBase is the reinforcement curve base r0 in
	// reinforcement(n) = 1 − (1−r0)·0.9^n. Zero means kernel default (0.5).
	// A never-accessed memory's reinforcement equals this base.
	SalienceBase float64 `json:"salience_base,omitempty" toml:"salience_base,omitempty"`

	// FusionMode selects the fusion strategy applied to per-lane
	// candidate sets. Empty leaves the kernel default, "weighted":
	// mem0 v3's score-based blend, (semantic + BM25 + entity) /
	// maxPossible over per-lane raw scores normalised to [0,1], tuned
	// so a sole strong-lane match is not buried beneath weakly-multi-
	// lane hits. Set "rrf" to opt back into the additive reciprocal-
	// rank fusion the kernel shipped through M1 (fused = vc+fc+gc+nc).
	// Validated at the config-parse boundary so TOML/code drift
	// surfaces there rather than on the recall hot path.
	FusionMode string `json:"fusion_mode,omitempty" toml:"fusion_mode,omitempty"`

	// SemanticWeight, BM25Weight, EntityWeight are retained in the
	// schema for backward compatibility with configs that set them
	// against the earlier (rank-based) WeightedComponents signature.
	// They are no longer wired: the current WeightedComponents derives
	// blend weights from mem0 v3's adaptive sum/max_possible rule and
	// per-lane raw scores. Reading these knobs is a no-op; set
	// fusion_mode = "weighted" to activate the new algorithm.
	SemanticWeight float64 `json:"semantic_weight,omitempty" toml:"semantic_weight,omitempty"`
	BM25Weight     float64 `json:"bm25_weight,omitempty" toml:"bm25_weight,omitempty"`
	EntityWeight   float64 `json:"entity_weight,omitempty" toml:"entity_weight,omitempty"`

	// SemanticGateThreshold is the pre-fusion semantic floor: candidates
	// whose raw cosine similarity falls below this value are dropped
	// from the weighted-fusion result set before fusion runs. Matches
	// mem0/utils/scoring.py:102-103. Zero (the default) disables the
	// gate; production tuning starts around 0.2-0.3. Only consulted
	// when FusionMode == "weighted".
	SemanticGateThreshold float64 `json:"semantic_gate_threshold,omitempty" toml:"semantic_gate_threshold,omitempty"`

	// TypeWeights maps EpisodeClass strings ("semantic", "episodic",
	// "procedural") to the multiplier the retrieval lane applies to
	// the fused score (Phase 3 of the mem0-parity sweep). mem0 v3's
	// documented defaults are 0.6 / 0.3 / 0.1: context-stable facts
	// outrank session turns which outrank step-by-step instructions
	// on fact-seeking queries.
	//
	// Empty (the default) disables the multiplier so every class
	// scores at its raw fused value. To enable mem0's defaults
	// explicitly, write:
	//
	//   [recall.type_weights]
	//     semantic   = 0.6
	//     episodic   = 0.3
	//     procedural = 0.1
	TypeWeights map[string]float64 `json:"type_weights,omitempty" toml:"type_weights,omitempty"`

	// EmbedDeadlineMs is the per-recall query-embedding deadline in
	// milliseconds. When the embedder does not respond within this window
	// the call degrades to FTS-only and surfaces MAZ-RECALL-001 in the
	// recall report. Zero (the default) means no deadline beyond the
	// caller's context. Applies globally to every recall call; individual
	// calls can tighten further via the per-call embedDeadlineMs field.
	EmbedDeadlineMs int `json:"embed_deadline_ms,omitempty" toml:"embed_deadline_ms,omitempty"`
}

// ProviderTable is the union of every per-provider field across the
// providers Mazarbul understands today. Using one struct (rather than
// map[string]any) keeps the schema discoverable; omitempty keeps the
// emitted TOML tight when a provider only needs a subset.
type ProviderTable struct {
	// Model is the chat-or-default model identifier for providers that
	// expose a single model slot (e.g. Anthropic's claude-opus-4-7).
	Model string `json:"model,omitempty" toml:"model,omitempty"`

	// Secret is the reference URI under which the provider's API key is
	// stored. Format: "secrets://<canonical-name>", e.g.
	// "secrets://anthropic.api_key". Local providers omit this.
	Secret string `json:"secret,omitempty" toml:"secret,omitempty"`

	// BaseURL overrides the provider endpoint (e.g. a non-default Ollama
	// daemon URL). Empty means the provider's compiled-in default.
	BaseURL string `json:"base_url,omitempty" toml:"base_url,omitempty"`

	// ChatModel is the chat model for providers that distinguish chat from
	// embedding slots (Ollama, OpenAI). Empty means the slot is unused for
	// this provider.
	ChatModel string `json:"chat_model,omitempty" toml:"chat_model,omitempty"`

	// EmbedModel is the embedding model for providers that distinguish chat
	// from embedding slots. Empty means the slot is unused.
	EmbedModel string `json:"embed_model,omitempty" toml:"embed_model,omitempty"`

	// Managed indicates the Mazarbul daemon owns the lifecycle of the
	// provider's subprocess (e.g. an `ollama serve` child started by the
	// init wizard). When true, the supervisor starts an OllamaSupervisor
	// goroutine that monitors liveness and restarts the child on crash.
	// AIP-140 adjective form: no "is" prefix.
	Managed bool `json:"managed,omitempty" toml:"managed,omitempty"`

	// NumParallel sets OLLAMA_NUM_PARALLEL in the env of the managed
	// `ollama serve` child. Zero (the default) leaves the env unset so
	// Ollama applies its built-in default of 1 parallel slot. Values >= 2
	// let multiple chat or embed calls run concurrently against a single
	// model runner, which helps on >= 16 GB hosts where the model fits
	// comfortably in GPU memory. Opt-in: users set it explicitly in
	// config.toml under [providers.ollama]; doctor surfaces the value as
	// an INFO row. There is no auto-detect: the user knows their GPU
	// memory better than we do, and a wrong default thrashes the
	// runner.
	NumParallel int `json:"num_parallel,omitempty" toml:"num_parallel,omitempty"`
}

// DefaultsTable picks the active provider for each slot. Values are the
// provider names that appear as keys in UserConfig.Providers. An empty
// Embed string means "no embed provider wired"; recall degrades to
// FTS-only and `maz remember` writes raw events without embedding.
type DefaultsTable struct {
	Chat  string `json:"chat" toml:"chat"`
	Embed string `json:"embed,omitempty" toml:"embed,omitempty"`
}

// UserConfigPath returns the canonical path to the user config inside the
// resolved data directory.
func UserConfigPath(dataDir string) string {
	return filepath.Join(dataDir, userConfigFile)
}

// WriteUserConfig persists cfg to ${dataDir}/config.toml atomically.
//
// The write goes to a sibling tempfile and is renamed into place; readers
// see either the previous or new contents, never a partial write. The
// final file ends up with mode 0o600. DataDir must already exist; the
// caller (maz init) creates it earlier in the wizard.
//
//nolint:gocritic // UserConfig copied by value to keep the call sites natural (`config.WriteUserConfig(dir, cfg)`) and let the compiler keep cfg on the stack; pointer would force every caller to take an address and complicate test fixtures.
func WriteUserConfig(dataDir string, cfg UserConfig) error {
	if dataDir == "" {
		return errors.New("config: data dir is required")
	}
	final := UserConfigPath(dataDir)

	tmp, err := os.CreateTemp(dataDir, userConfigFile+".tmp.*")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Belt-and-braces: if anything below this point fails before Rename,
	// remove the orphan tempfile so the data dir does not accumulate
	// .tmp.* leftovers across crashed init runs. We deliberately ignore
	// any error from os.Remove because the rename has already failed and
	// the leftover (mode 0600, owner-only) is harmless.
	cleanup := func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			// Nothing actionable to do; the rename error returned to
			// the caller already conveys the meaningful failure.
			_ = rmErr
		}
	}

	if encErr := toml.NewEncoder(tmp).Encode(cfg); encErr != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: encode: %w", encErr)
	}
	if chmodErr := tmp.Chmod(userConfigMode); chmodErr != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config: chmod temp: %w", chmodErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		cleanup()
		return fmt.Errorf("config: close temp: %w", closeErr)
	}
	if renameErr := os.Rename(tmpPath, final); renameErr != nil {
		cleanup()
		return fmt.Errorf("config: rename: %w", renameErr)
	}
	return nil
}

// LoadResolvedConfig loads ${dataDir}/config.toml and overlays any
// ${dataDir}/state.toml overrides via [Merge], returning the resolved
// view callers should consult at runtime. A missing config.toml
// surfaces ErrUserConfigNotFound; a missing state.toml is treated as
// "no overrides" and the bare UserConfig is returned. Any other read
// or parse failure on either file is returned as-is.
//
// This is the canonical loader for daemon boot, the Settings TUI's
// view-after-save, and any consumer that needs the post-override
// configuration. Direct LoadUserConfig calls remain for code paths
// that must round-trip the user-curated file verbatim (e.g.
// schema-migration tooling).
func LoadResolvedConfig(dataDir string) (UserConfig, error) {
	cfg, err := LoadUserConfig(dataDir)
	if err != nil {
		return UserConfig{}, err
	}
	state, err := LoadUserState(dataDir)
	if err != nil {
		if errors.Is(err, ErrUserStateNotFound) {
			return cfg, nil
		}
		return UserConfig{}, err
	}
	return Merge(cfg, state), nil
}

// UserConfigDefaults returns the compiled defaults for UserConfig.
// Used by `maz config get` / `maz config list` so values fall back to
// the application's defaults instead of the zero value when
// config.toml is absent. Mirrors the shape baked into init's "smart
// defaults" path so `config list` on a fresh data-dir reports what
// the daemon would actually use at boot.
func UserConfigDefaults() UserConfig {
	return UserConfig{
		SchemaVersion: CurrentUserConfigSchemaVersion,
		Defaults: DefaultsTable{
			Chat:  "anthropic",
			Embed: "ollama",
		},
		Sync: SyncTable{
			CoalesceSeconds:        intPtr(30),
			SealMaxEvents:          10_000,
			SealMaxEventsPlaintext: 2_000,
			SealIntervalSeconds:    3600,
		},
		Log: UserLogTable{Level: "info", Format: "json"},
	}
}

// LoadEffectiveConfig returns the fully-merged UserConfig the
// application would use at runtime: compiled defaults <- config.toml
// <- state.toml <- project overlay. ABSENT layers are skipped without
// error (so a fresh data-dir still produces a sensible view rather
// than zero values); PARSE errors and schema-version rejections from
// any layer are returned to the caller so `maz config get` cannot
// report a clean-looking effective config while the daemon would
// reject the same file at boot. Used by `maz config get` and `maz
// config list` so what the operator sees is what the daemon honours.
func LoadEffectiveConfig(dataDir, projectDir string) (UserConfig, error) {
	resolved := UserConfigDefaults()
	cfg, err := LoadUserConfig(dataDir)
	switch {
	case err == nil:
		resolved = mergeUserConfig(resolved, cfg)
	case errors.Is(err, ErrUserConfigNotFound):
		// fresh data-dir; the compiled defaults stand in for the missing layer.
	default:
		return UserConfig{}, fmt.Errorf("config: load user config: %w", err)
	}
	state, err := LoadUserState(dataDir)
	switch {
	case err == nil:
		resolved = Merge(resolved, state)
	case errors.Is(err, ErrUserStateNotFound):
		// fresh data-dir; no state overrides yet.
	default:
		return UserConfig{}, fmt.Errorf("config: load user state: %w", err)
	}
	if projectDir != "" {
		overlay, perr := LoadProjectConfig(projectDir)
		switch {
		case perr == nil:
			resolved = MergeProjectConfig(resolved, overlay)
		case errors.Is(perr, ErrProjectConfigNotFound):
			// no per-project overlay in this directory; not an error.
		default:
			return UserConfig{}, fmt.Errorf("config: load project config: %w", perr)
		}
	}
	return resolved, nil
}

// intPtr returns a pointer to v. Used to initialise *int config fields in
// UserConfigDefaults so the compiled default (e.g. CoalesceSeconds=30) is
// stored as a non-nil pointer, keeping the nil == "unset" semantic correct.
func intPtr(v int) *int { return &v }

// recallNonZero / composerNonZero are JSON-roundtrip nil checks for
// the slice-bearing tables. A struct that marshals to the same bytes
// as the zero value carries no overlay-worthy data. Pointer receivers
// keep the heavy RecallTable / ComposerTable values off the stack.
func recallNonZero(r *RecallTable) bool     { return !zeroJSON(*r) }
func composerNonZero(c *ComposerTable) bool { return !zeroJSON(*c) }
func zeroJSON[T any](v T) bool {
	var zero T
	a, _ := json.Marshal(v)    //nolint:errcheck // marshaling a config struct never errs at runtime; the bytes-equal check tolerates the nil case
	b, _ := json.Marshal(zero) //nolint:errcheck // marshaling a zero-value struct never errs at runtime
	return bytes.Equal(a, b)
}

// mergeUserConfig overlays src on top of dst for the small set of
// fields config.toml typically overrides. The state-merge path
// handles the rest via Merge(cfg, state); this helper only exists so
// the defaults-then-file ordering produces the expected union without
// turning the whole layering into a reflective merge.
//
//nolint:gocognit,gocyclo,gocritic // the function is a flat sequence of independent field overlays; splitting it would hide the precedence order behind helpers; value receivers keep the call sites copy-on-write
func mergeUserConfig(dst, src UserConfig) UserConfig {
	if src.SchemaVersion != 0 {
		dst.SchemaVersion = src.SchemaVersion
	}
	if src.Defaults.Chat != "" {
		dst.Defaults.Chat = src.Defaults.Chat
	}
	if src.Defaults.Embed != "" {
		dst.Defaults.Embed = src.Defaults.Embed
	}
	if len(src.Providers) > 0 {
		if dst.Providers == nil {
			dst.Providers = make(map[string]ProviderTable, len(src.Providers))
		}
		for name, pt := range src.Providers {
			dst.Providers[name] = pt
		}
	}
	if src.Sync.CoalesceSeconds != nil {
		dst.Sync.CoalesceSeconds = src.Sync.CoalesceSeconds
	}
	if src.Sync.SealMaxEvents != 0 {
		dst.Sync.SealMaxEvents = src.Sync.SealMaxEvents
	}
	if src.Sync.SealMaxEventsPlaintext != 0 {
		dst.Sync.SealMaxEventsPlaintext = src.Sync.SealMaxEventsPlaintext
	}
	if src.Sync.SealIntervalSeconds != 0 {
		dst.Sync.SealIntervalSeconds = src.Sync.SealIntervalSeconds
	}
	if src.Sync.AutoPush {
		dst.Sync.AutoPush = true
	}
	if len(src.Sync.Remotes) > 0 {
		if dst.Sync.Remotes == nil {
			dst.Sync.Remotes = make(map[string]ScopeRemoteTable, len(src.Sync.Remotes))
		}
		for k, v := range src.Sync.Remotes {
			dst.Sync.Remotes[k] = v
		}
	}
	if src.Sync.Identity != nil {
		id := *src.Sync.Identity
		dst.Sync.Identity = &id
	}
	// Recall / Composer contain slices, so they're not comparable; we
	// overlay them wholesale when src specifies anything (best-effort
	// "if any field is set, take it" heuristic via JSON round-trip).
	if recallNonZero(&src.Recall) {
		dst.Recall = src.Recall
	}
	if composerNonZero(&src.Composer) {
		dst.Composer = src.Composer
	}
	if src.Log.Level != "" {
		dst.Log.Level = src.Log.Level
	}
	if src.Log.Format != "" {
		dst.Log.Format = src.Log.Format
	}
	// OntologyTable: bool fields; any true value in src wins.
	if src.Ontology.StrictAttributes {
		dst.Ontology.StrictAttributes = true
	}
	if src.Ontology.StrictDomainRange {
		dst.Ontology.StrictDomainRange = true
	}
	// OutputTable: overlay individual string fields when non-empty.
	if src.Output.DefaultFormat != "" {
		dst.Output.DefaultFormat = src.Output.DefaultFormat
	}
	// SavedRecallsTable: merge map entries (src wins on key collision).
	if len(src.SavedRecalls) > 0 {
		if dst.SavedRecalls == nil {
			dst.SavedRecalls = make(SavedRecallsTable, len(src.SavedRecalls))
		}
		for k, v := range src.SavedRecalls {
			dst.SavedRecalls[k] = v
		}
	}
	return dst
}

// LoadUserConfig reads and parses ${dataDir}/config.toml. A missing file
// returns ErrUserConfigNotFound so callers can distinguish first-run from
// an explicit empty config. An unrecognised schema version is an error;
// callers should treat it as a "newer maz wrote this" signal.
func LoadUserConfig(dataDir string) (UserConfig, error) {
	path := UserConfigPath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return UserConfig{}, ErrUserConfigNotFound
		}
		return UserConfig{}, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg UserConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return UserConfig{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.SchemaVersion > CurrentUserConfigSchemaVersion {
		return UserConfig{}, fmt.Errorf(
			"config: %s has schema_version=%d, this build supports up to %d",
			path, cfg.SchemaVersion, CurrentUserConfigSchemaVersion,
		)
	}
	return cfg, nil
}
