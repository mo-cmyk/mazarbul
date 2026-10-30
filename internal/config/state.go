package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// stateFile is the canonical filename inside the data directory for
// TUI-managed configuration overrides. State.toml is sibling to
// config.toml: the user owns config.toml (hand-edited, comments survive);
// the Hub Settings panel owns state.toml (regenerated on save). When
// both exist, state.toml takes precedence per-field via [Merge].
const stateFile = "state.toml"

// CurrentUserStateSchemaVersion is the schema version this build writes
// to state.toml. Bumped independently of CurrentUserConfigSchemaVersion:
// a config.toml migration does not always require a state.toml migration.
const CurrentUserStateSchemaVersion = 1

// ErrUserStateNotFound is returned by LoadUserState when no state.toml
// exists. Callers should treat a missing state file as "no overrides",
// not as a corrupt config.
var ErrUserStateNotFound = errors.New("config: user state not found")

// UserState mirrors UserConfig with override semantics: every field uses
// a zero value that means "inherit from UserConfig". The Hub Settings
// panel writes UserState; loaders call [Merge] to obtain the resolved
// configuration.
//
// Why split state from config:
//   - config.toml stays comment-friendly because the TUI never writes it.
//   - state.toml is machine-managed and accepts comment loss on save.
//   - The merge boundary is the only place to look up resolved knobs at
//     runtime; the Settings panel does not need to read config.toml to
//     compute the post-write state.
type UserState struct {
	SchemaVersion int `toml:"schema_version"`

	// Defaults overrides UserConfig.Defaults. Empty strings inherit.
	Defaults DefaultsStateTable `toml:"defaults,omitempty"`

	// Providers overrides specific provider fields by name. Keys absent
	// from this map inherit their corresponding UserConfig entry whole.
	// Keys present override field-by-field: a zero field still inherits.
	Providers map[string]ProviderStateTable `toml:"providers,omitempty"`

	// Recall overrides UserConfig.Recall field-by-field. Zero fields
	// inherit. Mirrors the existing zero-means-default discipline.
	Recall RecallTable `toml:"recall,omitempty"`

	// Composer overrides UserConfig.Composer field-by-field.
	Composer ComposerTable `toml:"composer,omitempty"`

	// Ingestion overrides UserConfig.Ingestion field-by-field.
	Ingestion IngestionTable `toml:"ingestion,omitempty"`

	// Ontology overrides UserConfig.Ontology field-by-field (P3 typed
	// properties). Zero values inherit from config.toml.
	Ontology OntologyTable `toml:"ontology,omitempty"`

	// Log overrides the daemon log level/format. Hot-reloadable: the
	// daemon's fsnotify watcher picks up changes without a restart.
	Log LogStateTable `toml:"log,omitempty"`

	// Sync overrides the file-resident sync block field-by-field.
	// The TUI writes here; config.toml stays comment-friendly.
	Sync SyncStateTable `toml:"sync,omitempty"`

	// Identity records what the local "self" singleton means on THIS
	// device: the concrete principal UID first-person facts bind to, plus
	// the device id and signing-key fingerprint. It is device-local truth,
	// NOT a config override (so it is deliberately absent from Merge), is
	// never rendered or synced (only <scope>/ repos sync), and survives a
	// Rebuild (which only wipes event-derived stores). See #209.
	Identity IdentityStateTable `toml:"identity,omitempty"`

	// Team holds team-scope device-local truth: genesis pin, etc.
	// Absent from Merge for the same reasons as Identity — it is not a
	// UserConfig override; it is device-local state written at team create
	// or join and read by the fold and doctor.
	Team TeamStateTable `toml:"team,omitempty"`
}

// TeamStateTable holds team-scope device-local state. It is NOT a UserConfig
// override (absent from Merge); it is written at team create/join and read by
// the membership fold and doctor checks at runtime.
type TeamStateTable struct {
	// GenesisPin pins the team genesis principal UID and signing-key
	// fingerprint. Written at team CREATE (pin = self) and at team JOIN
	// (pin from TEAM.json or JoinKey). Empty fields mean "unpinned":
	// the fold falls back to lowest-ULID TOFU selection.
	GenesisPin GenesisPinStateTable `toml:"genesis_pin,omitempty"`
}

// GenesisPinStateTable is the on-disk representation of the genesis trust
// anchor inside state.toml. Empty fields mean "not yet pinned".
type GenesisPinStateTable struct {
	// PrincipalUID is the genesis principal's ULID.
	PrincipalUID string `toml:"principal_uid,omitempty"`
	// SigKeyID is the fingerprint ("kid:…") of the genesis signing key.
	SigKeyID string `toml:"sig_key_id,omitempty"`
}

// IdentityStateTable is the local self->principal binding. Empty fields
// mean "not yet bound" (pre-genesis / fresh data dir).
type IdentityStateTable struct {
	// PrincipalUID is the concrete principal the local "self" resolves to.
	PrincipalUID string `toml:"principal_uid,omitempty"`
	// DeviceID is this device's stable identifier.
	DeviceID string `toml:"device_id,omitempty"`
	// SigKeyID is the fingerprint of this device's ed25519 signing key.
	SigKeyID string `toml:"sig_key_id,omitempty"`
}

// SyncStateTable mirrors SyncTable with override semantics. Empty
// values inherit; the AutoPush bool uses a pointer because false is
// a meaningful override against an existing auto_push=true.
// CoalesceSeconds is a pointer so an explicit 0 (no coalescing) is
// distinguishable from an absent key (inherit).
type SyncStateTable struct {
	Remotes                map[string]ScopeRemoteStateTable `toml:"remotes,omitempty"`
	AutoPush               *bool                            `toml:"auto_push,omitempty"`
	CoalesceSeconds        *int                             `toml:"coalesce_seconds,omitempty"`
	SealMaxEvents          int                              `toml:"seal_max_events,omitempty"`
	SealMaxEventsPlaintext int                              `toml:"seal_max_events_plaintext,omitempty"`
	SealIntervalSeconds    int                              `toml:"seal_interval_seconds,omitempty"`

	// Identity overrides the commit author identity (the Hub Settings → Sync
	// tab writes it here). Empty fields inherit from config.toml's
	// [sync.identity].
	Identity SyncIdentityStateTable `toml:"identity,omitempty"`

	// LastPush records the latest successful push per scope as
	// RFC3339 strings. Hot-update target: the Sync settings tab
	// reads this for the "Last push" status row. Empty until the
	// daemon has pushed at least once.
	LastPush map[string]string `toml:"last_push,omitempty"`
}

// SyncIdentityStateTable mirrors SyncIdentityTable as per-field overrides.
// Empty strings inherit from config.toml's [sync.identity].
type SyncIdentityStateTable struct {
	CommitIdentity string `toml:"commit_identity,omitempty"`
	Name           string `toml:"name,omitempty"`
	Email          string `toml:"email,omitempty"`
}

// ScopeRemoteStateTable mirrors ScopeRemoteTable as per-field
// overrides. Empty strings inherit; Encrypted is a pointer so a
// state.toml that sets it explicitly (true OR false) overrides the
// config.toml value, while an unset pointer leaves the config value
// in place. Cleared=true is a hard tombstone: the resolved scope
// loses every config.toml field even when state.toml's URL is empty
// (this is the only way to disable a remote that was inherited from
// config.toml without editing config.toml by hand).
type ScopeRemoteStateTable struct {
	URL        string `toml:"url,omitempty"`
	AuthMethod string `toml:"auth_method,omitempty"`
	Encrypted  *bool  `toml:"encrypted,omitempty"`
	Cleared    bool   `toml:"cleared,omitempty"`
}

// DefaultsStateTable mirrors DefaultsTable with omitempty. Empty strings
// mean "inherit from UserConfig.Defaults".
type DefaultsStateTable struct {
	Chat  string `toml:"chat,omitempty"`
	Embed string `toml:"embed,omitempty"`
}

// ProviderStateTable mirrors ProviderTable as per-field overrides. The
// Managed bool uses a pointer because false is a meaningful override
// against an existing managed=true; the other fields treat zero as
// "inherit" by convention.
type ProviderStateTable struct {
	Model       string `toml:"model,omitempty"`
	Secret      string `toml:"secret,omitempty"`
	BaseURL     string `toml:"base_url,omitempty"`
	ChatModel   string `toml:"chat_model,omitempty"`
	EmbedModel  string `toml:"embed_model,omitempty"`
	Managed     *bool  `toml:"managed,omitempty"`
	NumParallel int    `toml:"num_parallel,omitempty"`
}

// LogStateTable holds hot-reloadable log overrides. Empty Level / Format
// inherits the Config-level value.
type LogStateTable struct {
	Level  string `toml:"level,omitempty"`
	Format string `toml:"format,omitempty"`
}

// UserStatePath returns the canonical path to state.toml inside dataDir.
func UserStatePath(dataDir string) string {
	return filepath.Join(dataDir, stateFile)
}

// WriteUserState persists state to ${dataDir}/state.toml atomically.
// DataDir must already exist. The final file ends up at mode 0o600.
//
//nolint:gocritic // UserState copied by value mirrors WriteUserConfig.
func WriteUserState(dataDir string, state UserState) error {
	if dataDir == "" {
		return errors.New("config: data dir is required")
	}
	if state.SchemaVersion == 0 {
		state.SchemaVersion = CurrentUserStateSchemaVersion
	}
	return WriteTOMLFileAtomic(UserStatePath(dataDir), state)
}

// LoadUserState reads ${dataDir}/state.toml. A missing file returns
// ErrUserStateNotFound so callers can distinguish "no overrides" from
// "corrupt overrides". An unrecognised schema_version is an error;
// callers should treat it as a "newer maz wrote this" signal.
func LoadUserState(dataDir string) (UserState, error) {
	path := UserStatePath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return UserState{}, ErrUserStateNotFound
		}
		return UserState{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var st UserState
	if err := toml.Unmarshal(data, &st); err != nil {
		return UserState{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if st.SchemaVersion > CurrentUserStateSchemaVersion {
		return UserState{}, fmt.Errorf(
			"config: %s has schema_version=%d, this build supports up to %d",
			path, st.SchemaVersion, CurrentUserStateSchemaVersion,
		)
	}
	return st, nil
}

// SelfPrincipalUID returns the principal UID the local "self" is bound to
// on this device, or "" when unbound (no state file, or identity not yet
// seeded). A missing state file is not an error here: an unbound device is
// a valid pre-genesis state. Other read/parse errors are surfaced.
func SelfPrincipalUID(dataDir string) (string, error) {
	st, err := LoadUserState(dataDir)
	if err != nil {
		if errors.Is(err, ErrUserStateNotFound) {
			return "", nil
		}
		return "", err
	}
	return st.Identity.PrincipalUID, nil
}

// BindSelfPrincipal persists the local self->principal binding to
// state.toml via an atomic read-modify-write, preserving every other
// state field. Idempotent: re-binding the same principal is a no-op write.
func BindSelfPrincipal(dataDir, principalUID, deviceID, sigKeyID string) error {
	st, err := LoadUserState(dataDir)
	if err != nil && !errors.Is(err, ErrUserStateNotFound) {
		return err
	}
	st.Identity = IdentityStateTable{
		PrincipalUID: principalUID,
		DeviceID:     deviceID,
		SigKeyID:     sigKeyID,
	}
	return WriteUserState(dataDir, st)
}

// SetTeamGenesisPin persists the team genesis pin to state.toml via an atomic
// read-modify-write, preserving every other state field. Called at team CREATE
// (pin = self) and at team JOIN (pin from TEAM.json or JoinKey).
// Both principalUID and sigKeyID must be non-empty; empty values are rejected.
func SetTeamGenesisPin(dataDir, principalUID, sigKeyID string) error {
	if principalUID == "" || sigKeyID == "" {
		return errors.New("config: genesis pin requires non-empty principal UID and sig key id")
	}
	st, err := LoadUserState(dataDir)
	if err != nil && !errors.Is(err, ErrUserStateNotFound) {
		return err
	}
	st.Team.GenesisPin = GenesisPinStateTable{
		PrincipalUID: principalUID,
		SigKeyID:     sigKeyID,
	}
	return WriteUserState(dataDir, st)
}

// ClearTeamGenesisPin removes the genesis pin from state.toml via an atomic
// read-modify-write, preserving every other state field. Clearing an already
// absent pin (or a missing state file) is a no-op. This is the recovery path
// for a poisoned or stale pin: once cleared, the fold falls back to TOFU and
// the daemon's consume loop re-imports a VERIFIED TEAM.json anchor on the
// next team pull.
func ClearTeamGenesisPin(dataDir string) error {
	st, err := LoadUserState(dataDir)
	if err != nil {
		if errors.Is(err, ErrUserStateNotFound) {
			return nil
		}
		return err
	}
	st.Team.GenesisPin = GenesisPinStateTable{}
	return WriteUserState(dataDir, st)
}

// LoadTeamGenesisPin returns the genesis pin stored in state.toml, with
// ok=false when no pin is present (state absent, or pin fields empty).
// A missing state file is not an error: an unpinned team is valid.
func LoadTeamGenesisPin(dataDir string) (principalUID, sigKeyID string, ok bool, err error) {
	st, lerr := LoadUserState(dataDir)
	if lerr != nil {
		if errors.Is(lerr, ErrUserStateNotFound) {
			return "", "", false, nil
		}
		return "", "", false, lerr
	}
	p := st.Team.GenesisPin.PrincipalUID
	k := st.Team.GenesisPin.SigKeyID
	if p == "" || k == "" {
		return "", "", false, nil
	}
	return p, k, true, nil
}

// Merge applies a UserState's overrides onto a UserConfig. Zero state
// fields inherit; non-zero state fields replace the matching cfg field.
// Provider overrides are key-wise and field-by-field: a partial
// override (e.g. only state.Providers["anthropic"].Model set) leaves
// every other field of cfg.Providers["anthropic"] intact.
//
//nolint:gocritic // cfg, state copied by value to keep call sites natural.
func Merge(cfg UserConfig, state UserState) UserConfig {
	out := cfg
	if state.Defaults.Chat != "" {
		out.Defaults.Chat = state.Defaults.Chat
	}
	if state.Defaults.Embed != "" {
		out.Defaults.Embed = state.Defaults.Embed
	}
	if len(state.Providers) > 0 {
		if out.Providers == nil {
			out.Providers = map[string]ProviderTable{}
		}
		for name, override := range state.Providers {
			base := out.Providers[name]
			applyProviderState(&base, &override)
			out.Providers[name] = base
		}
	}
	out.Recall = mergeRecall(&out.Recall, &state.Recall)
	out.Composer = mergeComposer(out.Composer, state.Composer)
	out.Ingestion = mergeIngestion(out.Ingestion, state.Ingestion)
	out.Ontology = mergeOntology(out.Ontology, state.Ontology)
	out.Log = mergeLog(out.Log, state.Log)
	out.Sync = mergeSync(out.Sync, &state.Sync)
	return out
}

// mergeOntology applies state-side ontology overrides on top of the base.
// Each bool follows true-wins: if either base or override is true, the merged
// result is true.
func mergeOntology(base, override OntologyTable) OntologyTable {
	out := base
	if override.StrictAttributes {
		out.StrictAttributes = true
	}
	if override.StrictDomainRange {
		out.StrictDomainRange = true
	}
	return out
}

// mergeSync applies the state-side sync overrides on top of the
// file-resident sync block. Per-scope remote entries are key-wise:
// state.toml may add new scopes or override URL/AuthMethod for an
// existing scope without disturbing other scopes. AutoPush uses a
// pointer-bool so the operator can disable an enabled config.toml
// auto_push without editing config.toml.
//

//nolint:gocognit // merge is intentionally per-field; nested loops keep the SyncTable / SyncStateTable mapping readable in one place.
func mergeSync(base SyncTable, override *SyncStateTable) SyncTable {
	out := base
	if len(override.Remotes) > 0 {
		if out.Remotes == nil {
			out.Remotes = map[string]ScopeRemoteTable{}
		}
		for scope, sr := range override.Remotes {
			if sr.Cleared {
				// Hard tombstone: forget any inherited config.toml entry.
				delete(out.Remotes, scope)
				continue
			}
			base := out.Remotes[scope]
			if sr.URL != "" {
				base.URL = sr.URL
			}
			if sr.AuthMethod != "" {
				base.AuthMethod = sr.AuthMethod
			}
			if sr.Encrypted != nil {
				v := *sr.Encrypted
				base.Encrypted = &v
			}
			out.Remotes[scope] = base
		}
	}
	if override.AutoPush != nil {
		out.AutoPush = *override.AutoPush
	}
	if override.CoalesceSeconds != nil {
		out.CoalesceSeconds = override.CoalesceSeconds
	}
	if override.SealMaxEvents != 0 {
		out.SealMaxEvents = override.SealMaxEvents
	}
	if override.SealMaxEventsPlaintext != 0 {
		out.SealMaxEventsPlaintext = override.SealMaxEventsPlaintext
	}
	if override.SealIntervalSeconds != 0 {
		out.SealIntervalSeconds = override.SealIntervalSeconds
	}
	out.Identity = mergeSyncIdentity(out.Identity, override.Identity)
	return out
}

// mergeSyncIdentity applies the state-side [sync.identity] overrides onto the
// config-side block. Empty override fields inherit; a non-empty field on an
// absent base block allocates one. Returns nil only when both are empty, so the
// resolved SyncTable carries no spurious empty identity.
func mergeSyncIdentity(base *SyncIdentityTable, override SyncIdentityStateTable) *SyncIdentityTable {
	if override.CommitIdentity == "" && override.Name == "" && override.Email == "" {
		return base
	}
	out := SyncIdentityTable{}
	if base != nil {
		out = *base
	}
	if override.CommitIdentity != "" {
		out.CommitIdentity = override.CommitIdentity
	}
	if override.Name != "" {
		out.Name = override.Name
	}
	if override.Email != "" {
		out.Email = override.Email
	}
	return &out
}

// mergeLog applies the hot-reloadable log overrides from state on
// top of the file-resident config. Empty state fields inherit; the
// daemon's watcher callback consumes the resolved values to call
// zap.AtomicLevel.SetLevel at runtime.
func mergeLog(base UserLogTable, override LogStateTable) UserLogTable {
	if override.Level != "" {
		base.Level = override.Level
	}
	if override.Format != "" {
		base.Format = override.Format
	}
	return base
}

func applyProviderState(base *ProviderTable, override *ProviderStateTable) {
	if override.Model != "" {
		base.Model = override.Model
	}
	if override.Secret != "" {
		base.Secret = override.Secret
	}
	if override.BaseURL != "" {
		base.BaseURL = override.BaseURL
	}
	if override.ChatModel != "" {
		base.ChatModel = override.ChatModel
	}
	if override.EmbedModel != "" {
		base.EmbedModel = override.EmbedModel
	}
	if override.Managed != nil {
		base.Managed = *override.Managed
	}
	if override.NumParallel != 0 {
		base.NumParallel = override.NumParallel
	}
}

func mergeRecall(base, override *RecallTable) RecallTable {
	out := *base
	mergeRecallLanes(&out, override)
	mergeRecallFusion(&out, override)
	mergeRecallRecency(&out, override)
	return out
}

func mergeRecallLanes(out, override *RecallTable) {
	// TwoHopFloor/GraphWeight/NameEmbWeight are pointer knobs so an explicit 0
	// in a scope override is honored (nil == unset, not "fall back to base").
	if override.TwoHopFloor != nil {
		out.TwoHopFloor = override.TwoHopFloor
	}
	if override.GraphWeight != nil {
		out.GraphWeight = override.GraphWeight
	}
	if override.NameEmbWeight != nil {
		out.NameEmbWeight = override.NameEmbWeight
	}
	if len(override.EdgeAllowlist) > 0 {
		out.EdgeAllowlist = override.EdgeAllowlist
	}
}

func mergeRecallFusion(out, override *RecallTable) {
	if override.FusionMode != "" {
		out.FusionMode = override.FusionMode
	}
	if override.RRFMinScore != nil {
		out.RRFMinScore = override.RRFMinScore
	}
	if override.RerankEnabled {
		out.RerankEnabled = true
	}
	if override.MMRLambda != nil {
		out.MMRLambda = override.MMRLambda
	}
	if override.SemanticWeight != 0 {
		out.SemanticWeight = override.SemanticWeight
	}
	if override.BM25Weight != 0 {
		out.BM25Weight = override.BM25Weight
	}
	if override.EntityWeight != 0 {
		out.EntityWeight = override.EntityWeight
	}
	if override.SemanticGateThreshold != 0 {
		out.SemanticGateThreshold = override.SemanticGateThreshold
	}
	if len(override.TypeWeights) > 0 {
		if out.TypeWeights == nil {
			out.TypeWeights = map[string]float64{}
		}
		for k, v := range override.TypeWeights {
			out.TypeWeights[k] = v
		}
	}
}

func mergeRecallRecency(out, override *RecallTable) {
	if override.RecencyBoost != nil {
		out.RecencyBoost = override.RecencyBoost
	}
	if override.RecencyHalflifeDays != 0 {
		out.RecencyHalflifeDays = override.RecencyHalflifeDays
	}
	if override.StrengthBoost != 0 {
		out.StrengthBoost = override.StrengthBoost
	}
	if override.StrengthHalflifeDays != 0 {
		out.StrengthHalflifeDays = override.StrengthHalflifeDays
	}
	if override.SalienceBase != 0 {
		out.SalienceBase = override.SalienceBase
	}
}

func mergeComposer(base, override ComposerTable) ComposerTable {
	out := base
	if override.AbstainThreshold != 0 {
		out.AbstainThreshold = override.AbstainThreshold
	}
	return out
}

func mergeIngestion(base, override IngestionTable) IngestionTable {
	out := base
	if override.DedupSemanticThreshold != 0 {
		out.DedupSemanticThreshold = override.DedupSemanticThreshold
	}
	return out
}
