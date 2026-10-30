package daemon

// teamtest_helpers_test.go provides the in-package test helpers shared by all
// team-scope E2E tests: device abstraction, supervisor builders, canonical
// dump for state comparison, and general assertion helpers.
//
// Every function here is defined in a _test.go file so nothing leaks into
// production binaries. New E2E tests compose these helpers by importing them
// implicitly (same package, same build, test-only).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// ontologyFactToolName is the fact tool's registered name.
// Captured as a const-by-lookup so tests do not import the ontology package
// just for the string literal; the registered tool's Name() is authoritative.
const ontologyFactToolName = "fact"

// device bundles one fully-wired daemon for a single device identity: its data
// dir, its isolated secret provider (separate keychain namespace), and the
// *Wired the team lifecycle and reads run through.
type device struct {
	name    string
	dataDir string
	secrets *memSecrets
	wired   *Wired
}

// newDevice opens a fresh fully-wired daemon with a unique auto-generated
// Watchword so each call yields a genuinely distinct identity.
func newDevice(t *testing.T, name string) *device {
	t.Helper()
	mnemonic, err := watchword.Generate()
	if err != nil {
		t.Fatalf("%s: generate watchword: %v", name, err)
	}
	return newDeviceWithMnemonic(t, name, mnemonic)
}

// newDeviceWithMnemonic is like newDevice but uses the supplied BIP-39
// mnemonic. Two devices seeded with the same mnemonic share the same signing
// key and derive the same principal UID — the correct setup for a paired
// two-device personal-scope sync test (C1). They still hold separate data
// dirs and separate memSecrets instances (and thus different random device IDs).
func newDeviceWithMnemonic(t *testing.T, name, mnemonic string) *device {
	t.Helper()
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: embedProviderNone},
	}
	if err := config.WriteUserConfig(dataDir, cfg); err != nil {
		t.Fatalf("%s: write config: %v", name, err)
	}
	secrets := newMemSecrets()
	if serr := secrets.Set(context.Background(), watchword.KeychainKey, mnemonic); serr != nil {
		t.Fatalf("%s: seed watchword: %v", name, serr)
	}
	w, err := OpenWiring(context.Background(), WiringConfig{
		DataDir: dataDir,
		Logger:  zap.NewNop(),
		Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("%s: OpenWiring: %v", name, err)
	}
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	d := &device{name: name, dataDir: dataDir, secrets: secrets, wired: w}
	if uid := d.principalUID(t); uid == "" {
		t.Fatalf("%s: self→principal binding absent after OpenWiring", name)
	}
	return d
}

// principalUID returns this device's bound principal UID (genesis-minted).
func (d *device) principalUID(t *testing.T) string {
	t.Helper()
	uid, err := config.SelfPrincipalUID(d.dataDir)
	if err != nil {
		t.Fatalf("%s: read self principal: %v", d.name, err)
	}
	return uid
}

// tool finds a registered MCP tool by name.
func (d *device) tool(t *testing.T, name string) mcp.Tool {
	t.Helper()
	for _, tl := range d.wired.Tools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("%s: tool %q not registered (registered: %v)", d.name, name, d.toolNames())
	return nil
}

func (d *device) toolNames() []string {
	names := make([]string, 0, len(d.wired.Tools))
	for _, tl := range d.wired.Tools {
		names = append(names, tl.Name())
	}
	return names
}

// invoke calls a registered tool with a JSON args object and returns the raw
// response, failing the test on any tool error.
func (d *device) invoke(t *testing.T, name string, args map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("%s: marshal %s args: %v", d.name, name, err)
	}
	resp, err := d.tool(t, name).Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("%s: invoke %s: %v", d.name, name, err)
	}
	return resp
}

// invokeMayError calls a registered tool and returns both the response and any
// error without failing the test. The caller decides how to interpret the error.
func (d *device) invokeMayError(t *testing.T, name string, args map[string]any) (json.RawMessage, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("%s: marshal %s args: %v", d.name, name, err)
	}
	return d.tool(t, name).Invoke(context.Background(), raw)
}

// dispatcher returns the production dispatcher backing this device's storage.
func (d *device) dispatcher(t *testing.T) *dispatcher.Dispatcher {
	t.Helper()
	disp, ok := d.wired.Orchestrator.Storage.(*dispatcher.Dispatcher)
	if !ok {
		t.Fatalf("%s: storage is %T, want *dispatcher.Dispatcher", d.name, d.wired.Orchestrator.Storage)
	}
	return disp
}

// teamNavigator returns a navigator narrowed to the {team} scope so reads see
// only team-scoped projections (the #206 scope boundary).
func (d *device) teamNavigator(t *testing.T) *navigate.Navigator {
	t.Helper()
	reader, err := d.dispatcher(t).SQLiteReader()
	if err != nil {
		t.Fatalf("%s: sqlite reader: %v", d.name, err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("%s: build navigator: %v", d.name, err)
	}
	return nav.WithScopes(kernel.NewScopeSet(kernel.ScopeTeam))
}

// newSupervisor builds (does not Start) a syncSupervisor for this device over
// the shared remote in team envelope mode. Requires the device's keychain to
// already hold a team DEK (call after team_create or after bootstrapping via
// consumeOnce). We drive sealOnce/pushOnce/consumeOnce by hand for
// determinism.
func (d *device) newSupervisor(t *testing.T, remote string) *syncSupervisor {
	t.Helper()
	orch := d.wired.Orchestrator
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:     d.dataDir,
		DeviceID:    resolveDeviceTag(context.Background(), d.secrets, zap.NewNop()),
		Source:      orch.Log,
		Sync:        envelopeTeamSync(remote),
		SecretChain: d.secrets,
		Recipients:  teamRecipientSourceFor(orch),
		Grafter:     orch.Log,
		Members:     teamMembershipReaderFor(orch),
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("%s: newSyncSupervisor: %v", d.name, err)
	}
	if sv == nil {
		t.Fatalf("%s: newSyncSupervisor returned nil (DEK or remote missing)", d.name)
	}
	t.Cleanup(func() { _ = sv.Close() })
	return sv
}

// newConsumeSupervisor builds a supervisor for a device that holds NO team DEK
// yet (a fresh joiner or never-granted device). Constructs the struct directly
// with an empty scopeKeys map — the genuine code path before DEK bootstrap.
func (d *device) newConsumeSupervisor(t *testing.T, remote string) *syncSupervisor {
	t.Helper()
	store, err := chunk.NewFSStore(d.dataDir)
	if err != nil {
		t.Fatalf("%s: open chunk store: %v", d.name, err)
	}
	trans, err := gittransport.New(gittransport.Options{
		DataDir:    d.dataDir,
		RemoteURLs: map[kernel.Scope]string{kernel.ScopeTeam: remote},
		DeviceID:   resolveDeviceTag(context.Background(), d.secrets, zap.NewNop()),
		AuthorName: "Mazarbul",
		AuthorMail: "mazarbul@" + d.name,
	})
	if err != nil {
		t.Fatalf("%s: git transport: %v", d.name, err)
	}
	orch := d.wired.Orchestrator
	return &syncSupervisor{
		cfg: &syncSupervisorConfig{
			DataDir:     d.dataDir,
			DeviceID:    resolveDeviceTag(context.Background(), d.secrets, zap.NewNop()),
			Source:      orch.Log,
			Sync:        envelopeTeamSync(remote),
			SecretChain: d.secrets,
			Recipients:  teamRecipientSourceFor(orch),
			Grafter:     orch.Log,
			Members:     teamMembershipReaderFor(orch),
			GenesisKeys: d.dispatcher(t), // enables maybeImportTeamJSONPin after first graft
			Logger:      zap.NewNop(),
		},
		store:     store,
		scopeKeys: map[kernel.Scope][]byte{},
		trans:     trans,
		logger:    zap.NewNop(),
	}
}

// newPlaintextPersonalSupervisor builds a fully-functional supervisor for the
// personal scope in plaintext mode via newSyncSupervisor. Unlike the manual
// consume-only variants above, this path runs buildScopeKeyMap which includes a
// cursor — so the returned supervisor can sealOnce+pushOnce+consumeOnce. Used
// by the C1 personal-scope two-device test.
func (d *device) newPlaintextPersonalSupervisor(t *testing.T, remote string) *syncSupervisor {
	t.Helper()
	orch := d.wired.Orchestrator
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:     d.dataDir,
		DeviceID:    resolveDeviceTag(context.Background(), d.secrets, zap.NewNop()),
		Source:      orch.Log,
		Sync:        plaintextPersonalSync(remote),
		SecretChain: d.secrets,
		Grafter:     orch.Log,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("%s: newSyncSupervisor (personal): %v", d.name, err)
	}
	if sv == nil {
		t.Fatalf("%s: newSyncSupervisor (personal) returned nil — no remote configured?", d.name)
	}
	t.Cleanup(func() { _ = sv.Close() })
	return sv
}

// envelopeTeamSync returns a SyncTable that puts the team scope on remote in
// encrypted envelope mode.
func envelopeTeamSync(remote string) config.SyncTable {
	enc := true
	return config.SyncTable{
		Remotes: map[string]config.ScopeRemoteTable{
			string(kernel.ScopeTeam): {URL: remote, Encrypted: &enc, KeyMode: config.KeyModeEnvelope},
		},
	}
}

// plaintextPersonalSync returns a SyncTable for an unencrypted personal-scope
// remote. No Encrypted flag means chunk.Decode reads JSONL verbatim.
func plaintextPersonalSync(remote string) config.SyncTable {
	return config.SyncTable{
		Remotes: map[string]config.ScopeRemoteTable{
			string(kernel.ScopePersonal): {URL: remote},
		},
	}
}

// configureJoinerTeamRemote points device B's team scope at the shared remote
// in envelope mode WITHOUT minting a competing genesis.
func configureJoinerTeamRemote(t *testing.T, d *device, remote string) {
	t.Helper()
	cfg, err := config.LoadUserConfig(d.dataDir)
	if err != nil {
		t.Fatalf("%s: load config: %v", d.name, err)
	}
	if cfg.Sync.Remotes == nil {
		cfg.Sync.Remotes = map[string]config.ScopeRemoteTable{}
	}
	enc := true
	cfg.Sync.Remotes[string(kernel.ScopeTeam)] = config.ScopeRemoteTable{
		URL:       remote,
		Encrypted: &enc,
		KeyMode:   config.KeyModeEnvelope,
	}
	if werr := config.WriteUserConfig(d.dataDir, cfg); werr != nil {
		t.Fatalf("%s: write joiner team config: %v", d.name, werr)
	}
}

// newBareTeamRemote initialises an empty bare git repo and returns a file://
// URL the git transport accepts verbatim.
func newBareTeamRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not in PATH: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "team-remote.git")
	cmd := exec.CommandContext(t.Context(), "git", "init", "--bare", "--quiet", "--initial-branch=main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	return "file://" + dir
}

// joinKeyBlob invokes the team_key tool and returns the opaque join-key blob.
func joinKeyBlob(t *testing.T, d *device) string {
	t.Helper()
	resp := d.invoke(t, "team_key", map[string]any{})
	blob := jsonField(t, resp, "joinKey")
	if blob == "" {
		t.Fatalf("%s: team_key returned empty joinKey: %s", d.name, resp)
	}
	return blob
}

// requireMember asserts IsTeamMember(uid) == want with a labelled failure.
func requireMember(t *testing.T, disp *dispatcher.Dispatcher, uid string, want bool, label string) {
	t.Helper()
	got, err := disp.IsTeamMember(context.Background(), uid)
	if err != nil {
		t.Fatalf("%s: IsTeamMember(%q): %v", label, uid, err)
	}
	if got != want {
		t.Fatalf("%s: IsTeamMember(%q) = %v, want %v", label, uid, got, want)
	}
}

// factsForSubject reads the open facts for subjectUID through nav.
func factsForSubject(t *testing.T, nav *navigate.Navigator, subjectUID string) []navigate.Fact {
	t.Helper()
	facts, err := nav.FactsForSubject(context.Background(), subjectUID, navigate.FactFilter{})
	if err != nil {
		t.Fatalf("FactsForSubject(%q): %v", subjectUID, err)
	}
	return facts
}

// hasFact reports whether facts contains an open slot key=val.
func hasFact(facts []navigate.Fact, key, val string) bool {
	for i := range facts {
		if facts[i].StateKey == key && facts[i].Value == val {
			return true
		}
	}
	return false
}

// assertRemoteHasTeamChunk verifies pushOnce actually landed a team chunk on
// the shared remote.
func assertRemoteHasTeamChunk(ctx context.Context, t *testing.T, sv *syncSupervisor) {
	t.Helper()
	listing, err := sv.trans.List(ctx, kernel.ScopeTeam)
	if err != nil {
		t.Fatalf("list remote team chunks: %v", err)
	}
	if len(listing.Chunks) == 0 {
		t.Fatalf("remote carries no team chunks after pushOnce — seal/push produced nothing")
	}
}

// assertRecipientsRoster reads A's staged recipients.jsonl and asserts both
// principals appear with a wrapped DEK row.
func assertRecipientsRoster(t *testing.T, aDataDir, aPrincipal, bPrincipal string) {
	t.Helper()
	path := filepath.Join(aDataDir, "sync", string(kernel.ScopeTeam), recipients.Filename)
	rows, err := recipients.Read(path)
	if err != nil {
		t.Fatalf("read recipients.jsonl: %v", err)
	}
	seen := map[string]bool{}
	for i := range rows {
		if rows[i].WrappedDEK == "" {
			t.Errorf("recipients row for %q has empty WrappedDEK", rows[i].PrincipalUID)
		}
		seen[rows[i].PrincipalUID] = true
	}
	if !seen[aPrincipal] {
		t.Errorf("recipients.jsonl missing genesis A %q: %+v", aPrincipal, rows)
	}
	if !seen[bPrincipal] {
		t.Errorf("recipients.jsonl missing grantee B %q: %+v", bPrincipal, rows)
	}
}

// jsonField extracts a string field from a tool's JSON response.
func jsonField(t *testing.T, raw json.RawMessage, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal tool response: %v (%s)", err, raw)
	}
	s, _ := m[field].(string)
	return s
}

// jsonIntField extracts an integer field from a tool's JSON response.
func jsonIntField(t *testing.T, raw json.RawMessage, field string) int {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal tool response: %v (%s)", err, raw)
	}
	v, ok := m[field].(float64)
	if !ok {
		t.Fatalf("field %q is %T, want numeric (got %v)", field, m[field], m[field])
	}
	return int(v)
}

// assertJSONField asserts a string field equals want.
func assertJSONField(t *testing.T, tool string, raw json.RawMessage, field, want string) {
	t.Helper()
	if got := jsonField(t, raw, field); got != want {
		t.Fatalf("%s response %s = %q, want %q (%s)", tool, field, got, want, raw)
	}
}

// canonicalDump queries a dispatcher's SQLite store and returns a
// deterministic, human-readable string of the scope-filtered derived tables.
// The dump is byte-identical when two dispatchers hold the same SHARED-scope
// event set applied in any order (the convergence guarantee). scope filters
// facts/relations; team_membership_acl and entity_redirects are unscoped by
// design.
//
// Entities need a subtler contract than a plain scope filter. A principal's
// person entity is fed by TWO writers: the personal-scope principalRegistered
// applier (self device only — personal events never leave the device) and the
// team-grant applier projectAdmittedMember (every device, scope='team',
// ON CONFLICT DO NOTHING so the self device keeps its personal tag and the
// entity stays visible in default personal reads). The row's scope TAG is
// therefore device-local by design; what converges is the row's EXISTENCE and
// identity. The dump accordingly selects entities tagged with the scope OR
// referenced by a same-scope fact/relation, comparing uid/kind/display_name
// without the tag.
//
// All rows are ordered by uid/principal_uid/source_uid so the result is
// stable regardless of insertion order. Facts include closed rows so the full
// slot history is compared — ordering divergence would be invisible if only
// open rows were checked.
func canonicalDump(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, scope string) string {
	t.Helper()
	db, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("canonicalDump: SQLiteReader: %v", err)
	}
	var sb strings.Builder

	dumpSection(t, ctx, &sb, db, fmt.Sprintf("=entities scope=%s\n", scope),
		`SELECT uid, kind, display_name FROM entities
		  WHERE scope = ?
		     OR uid IN (SELECT subject_uid FROM facts WHERE scope = ?)
		     OR uid IN (SELECT subject_uid FROM relations WHERE scope = ?)
		     OR uid IN (SELECT object_uid FROM relations WHERE scope = ?)
		  ORDER BY uid`, scope, scope, scope, scope)

	dumpSection(t, ctx, &sb, db, fmt.Sprintf("=facts scope=%s\n", scope),
		`SELECT uid, subject_uid, state_key, value, COALESCE(event_end,''), COALESCE(superseded_by_uid,'')
		   FROM facts WHERE scope = ? ORDER BY uid`, scope)

	dumpSection(t, ctx, &sb, db, fmt.Sprintf("=relations scope=%s\n", scope),
		`SELECT uid, subject_uid, predicate, object_uid, COALESCE(valid_until,''), COALESCE(superseded_by_uid,'')
		   FROM relations WHERE scope = ? ORDER BY uid`, scope)

	dumpSection(t, ctx, &sb, db, "=team_membership_acl\n",
		`SELECT principal_uid, admitted_by_uid, dek_epoch, is_genesis, role
		   FROM team_membership_acl ORDER BY principal_uid`)

	dumpSection(t, ctx, &sb, db, "=entity_redirects\n",
		`SELECT source_uid, target_uid FROM entity_redirects ORDER BY source_uid`)

	return sb.String()
}

// dumpSection writes one table section into sb: a header line followed by
// tab-delimited rows. query must ORDER BY a stable key. args are bound to
// any ? placeholders in query.
func dumpSection(t *testing.T, ctx context.Context, sb *strings.Builder, db *sql.DB, header, query string, args ...any) {
	t.Helper()
	sb.WriteString(header)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("canonicalDump query: %v\nSQL: %s", err, query)
	}
	defer func() { _ = rows.Close() }()
	appendRowLines(t, sb, rows)
}

// appendRowLines scans rows and appends each as a tab-delimited line to sb.
func appendRowLines(t *testing.T, sb *strings.Builder, rows *sql.Rows) {
	t.Helper()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("canonicalDump columns: %v", err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if serr := rows.Scan(ptrs...); serr != nil {
			t.Fatalf("canonicalDump scan: %v", serr)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%v", v)
		}
		fmt.Fprintln(sb, strings.Join(parts, "\t"))
	}
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("canonicalDump rows: %v", rerr)
	}
}

// isErrPermission reports whether err wraps kernel.ErrPermission. Used to
// assert that a reader's team write was denied at the append boundary.
func isErrPermission(err error) bool {
	return errors.Is(err, kernel.ErrPermission)
}
