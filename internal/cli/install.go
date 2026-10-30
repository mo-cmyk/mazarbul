package cli

// install.go implements "maz install <pack>" — installs a schema pack by
// appending entityKindRegistered and predicateRegistered events to the event
// log with provenance "extension:<pack-name>".
//
// Pack resolution order:
//  1. If the argument is a directory path on disk, parse it directly.
//  2. If the argument matches a builtin pack name ("schema-org-core",
//     "business-de"), load the embedded pack.
//
// The write pathway is identical to schema_kinds.go: openMaterializingLog →
// Installer.Install → event batch append. The dispatcher materialises the
// events synchronously, so the kinds/predicates are queryable the instant the
// command returns — no `maz rebuild` needed.
//
// Exit codes:
//
//	0   pack installed (events appended)
//	1   expected failure: pack not found, validation error, bad flags
//	2   unexpected failure: storage or wiring error

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/plugin"
	"github.com/mo-cmyk/mazarbul/internal/plugin/fetch"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// installFlags holds parsed flags for "maz install".
type installFlags struct {
	dryRun   bool
	force    bool
	list     bool
	yes      bool
	checksum string
}

// newInstallCmd returns the "maz install <pack>" cobra command.
func newInstallCmd() *cobra.Command {
	flags := &installFlags{}
	cmd := &cobra.Command{
		Use:   "install [<pack>]",
		Short: "Install a schema pack",
		Long: `Install a schema pack by appending entityKindRegistered and
predicateRegistered events to the event log.

<pack> can be:
  - the name of a builtin pack: schema-org-core, business-de
  - a path to a local pack directory containing manifest.toml
  - a remote git source: github.com/user/repo[@ref][#subdir] or a full git
    URL (https://, ssh://, git@host:owner/repo). @ref pins a branch, tag, or
    commit; #subdir selects a bundle nested in the repo. The clone is pinned to
    the resolved commit SHA and recorded as provenance.

Use --list to see the bundled packs without installing anything.

Remote sources require git on PATH and reuse your existing git credentials
(SSH / credential helper). Pass --checksum sha256:... to refuse a fetched
bundle whose canonical contents do not match (a tamper gate, verified before
anything is parsed or installed); compute the expected value with
'maz preset package <dir>'.

The events are appended and materialised into the entity_kinds and predicates
tables in the same step, so the kinds and predicates are queryable via
'maz schema kinds list' / 'maz schema predicates' the instant this command
returns — no 'maz rebuild' needed. If a daemon already serves this data dir it
holds the store lock, so install refuses rather than racing it; stop the daemon
('maz daemon stop') and retry.

Installation is idempotent for pack-vs-pack and pack-vs-core collisions.
When a pack defines a name that already belongs to a USER-authored predicate
or entity kind (added_by = "user"), install skips that item and prints a
warning rather than overwriting the user's definition. Pass --force to
overwrite user definitions (DATA LOSS: the user's semantics will be replaced).

Provenance is set to "extension:<pack-name>" for all registered types.

A manifest may declare two OPTIONAL fields that gate install (both absent-safe):
  - api_version: the host plugin-API version the pack targets. An incompatible
    version is refused with a clear host-vs-required message; a malformed one is
    a validation error.
  - scopes: the read scopes the pack requests (project|personal|team). An
    unknown token is a validation error. When scopes are declared, install
    prompts to grant them; pass --yes to grant the declared set without
    prompting (a non-interactive run without --yes is refused).

Exit codes:
  0   pack installed (events appended), or --list printed
  1   expected failure: pack not found, validation error, no argument given,
      or a declined/unconfirmed scope grant
  2   unexpected failure: storage or wiring error

Examples:
  maz install --list
  maz install schema-org-core
  maz install business-de
  maz install ./my-pack/
  maz install ./my-scoped-pack/ --yes
  maz install github.com/acme/crm-pack
  maz install github.com/acme/packs@v1.2.0#crm --checksum sha256:abc123...
  maz install business-de --dry-run
  maz install business-de --force
  maz install business-de --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.list {
				if len(args) > 0 {
					return fmt.Errorf("install --list: accepts no arguments, got %d", len(args))
				}
				return runInstallList(cmd)
			}
			if len(args) == 0 {
				//nolint:errcheck // best-effort stderr
				fmt.Fprintf(cmd.ErrOrStderr(), "install: <pack> argument required (use --list to see available packs)\n")
				return silentError{code: 1, err: errors.New("install: <pack> argument required")}
			}
			if len(args) > 1 {
				return fmt.Errorf("install: accepts 1 argument, got %d", len(args))
			}
			return runInstall(cmd, args[0], flags)
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "list what would be registered without appending events")
	cmd.Flags().BoolVar(&flags.force, "force", false, "overwrite user-authored predicates/kinds on name collision (data loss)")
	cmd.Flags().BoolVar(&flags.list, "list", false, "list available builtin packs without installing")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "grant the scopes the manifest declares without prompting")
	cmd.Flags().StringVar(&flags.checksum, "checksum", "", "require a fetched remote bundle to match this sha256:... checksum (tamper gate)")
	return cmd
}

// installErrorResult is the JSON output shape for an expected failure in
// "maz install --output json". It mirrors presetUninstallErrorResult so that
// scripted consumers get a consistent error envelope across all install-family
// commands.
type installErrorResult struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// installResult is the JSON output shape for maz install.
type installResult struct {
	PackName             string   `json:"packName"`
	PackVersion          string   `json:"packVersion"`
	KindsRegistered      []string `json:"kindsRegistered"`
	PredicatesRegistered []string `json:"predicatesRegistered"`
	// KindsSkipped lists kind names that were NOT installed because a
	// user-authored definition with the same name already exists. Pass
	// --force to overwrite.
	KindsSkipped []string `json:"kindsSkipped,omitempty"`
	// PredicatesSkipped lists predicate names that were NOT installed because
	// a user-authored definition with the same name already exists.
	PredicatesSkipped []string `json:"predicatesSkipped,omitempty"`
	// GrantedScopes lists the scopes the manifest declared and the user granted
	// at install (the full declared set; either confirmed interactively or via
	// --yes). Empty/omitted when the manifest declares no scopes — the record of
	// what this plugin was authorised to read.
	GrantedScopes []string  `json:"grantedScopes,omitempty"`
	DryRun        bool      `json:"dryRun"`
	InstalledAt   time.Time `json:"installedAt,omitempty"`
}

// runInstall is the main logic for maz install.
func runInstall(cmd *cobra.Command, packArg string, flags *installFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// ---- resolve pack (local dir, builtin, or remote git source) ----
	pack, err := resolvePack(ctx, packArg, dataDir, flags.checksum)
	if err != nil {
		return emitInstallResolveError(cmd, err)
	}

	// ---- install-time manifest gate: apiVersion compatibility + scope tokens ----
	// Runs before any state is touched (and before dry-run), so an incompatible
	// or malformed manifest is refused with a clear E_VALIDATION message rather
	// than half-installing. Both fields are optional: an absent apiVersion / empty
	// scopes list skips its check, so pre-existing manifests gate to a no-op.
	if gerr := gateManifestCompat(cmd, &pack.Manifest); gerr != nil {
		return gerr
	}

	// ---- dry-run: no event log needed ----
	if flags.dryRun {
		// GrantedScopes echoes the declared set so a dry-run previews exactly what
		// an install would grant.
		kinds, predicates := packRegisteredNames(pack)
		return renderInstallResult(cmd, &installResult{
			PackName:             pack.Manifest.Name,
			PackVersion:          pack.Manifest.Version,
			KindsRegistered:      kinds,
			PredicatesRegistered: predicates,
			GrantedScopes:        pack.Manifest.Scopes,
			DryRun:               true,
		})
	}

	// ---- scope-grant consent: prompt to grant the declared scopes ----
	// A manifest with no declared scopes proceeds silently (behaves exactly as
	// before this feature). With scopes declared: --yes auto-grants; an
	// interactive TTY prompts [y/N]; a non-interactive run without --yes refuses.
	proceed, cerr := confirmScopeGrant(cmd, pack.Manifest.Name, pack.Manifest.Scopes, flags.yes)
	if cerr != nil {
		return cerr
	}
	if !proceed {
		//nolint:errcheck // best-effort stdout
		fmt.Fprintln(cmd.OutOrStdout(), "install cancelled")
		// Exit 1, not 0: a declined scope grant is a non-success. The help text
		// documents exit 1 for "a declined/unconfirmed scope grant" and the
		// non-interactive refusal path already returns silentError{code: 1}, so
		// automation checking the exit code reads an interactive decline as
		// "consent not granted" rather than silently misreading it as success.
		return silentError{code: 1, err: errors.New("install: scope grant declined")}
	}

	now := time.Now().UTC()

	// When a daemon is live, we cannot open the store in-process. Build the
	// batch without appending it, then route through the admin endpoint.
	// Collision detection (filterPackAgainstDB) is skipped: the dispatcher's
	// UPSERT is idempotent — re-registering an existing kind or predicate is a
	// no-op (A.6 documented behavior change). skippedKinds/Preds are empty.
	if preflightDaemon("", dataDir) != nil {
		return installViaDaemon(ctx, cmd, dataDir, logger, pack, now)
	}

	// ---- open event log and install (no daemon) ----
	wired, closeFn, err := openMaterializingLog(ctx, cmd, dataDir, logger, "install")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("install: close wiring", zap.Error(cerr))
		}
	}()

	// ---- collision detection: protect user-authored definitions ----
	// Without --force, skip any pack predicate or entity kind whose name
	// already exists with added_by = 'user' in the query projection. The
	// UPSERT would otherwise silently overwrite the user's semantics (kind,
	// description, domain/range, sameAs) — a data-loss regression.
	// Pack-vs-pack and pack-vs-core collisions keep the current idempotent
	// UPSERT behaviour; only user definitions are protected.
	db, qerr := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if qerr != nil {
		return wrapInstallFatal(cmd, fmt.Errorf("open sqlite for collision check: %w", qerr))
	}
	filteredPack, skippedKinds, skippedPreds, cerr := filterPackAgainstDB(ctx, cmd, db, pack, flags.force)
	if cerr != nil {
		return wrapInstallFatal(cmd, cerr)
	}

	installer := &schemapack.Installer{Log: wired.Orchestrator.Log}
	ir, err := installer.Install(ctx, filteredPack, false)
	if err != nil {
		return wrapInstallFatal(cmd, fmt.Errorf("install pack: %w", err))
	}

	// Confirm every registered type materialised into the query projection.
	// Append keeps the JSONL durable but swallows storage.Apply failures, so
	// without this the pack could report "installed" while its kinds/predicates
	// are absent until `maz rebuild` — the silent-until-rebuild mode this path
	// closes.
	checks := kindPredicateMatChecks(ir)
	if len(checks) > 0 {
		if mcerr := confirmMaterialised(ctx, cmd, wired, "install", checks...); mcerr != nil {
			return mcerr
		}
	}

	res := &installResult{
		PackName:             ir.PackName,
		PackVersion:          pack.Manifest.Version,
		KindsRegistered:      ir.KindsRegistered,
		PredicatesRegistered: ir.PredicatesRegistered,
		KindsSkipped:         skippedKinds,
		PredicatesSkipped:    skippedPreds,
		GrantedScopes:        pack.Manifest.Scopes,
		DryRun:               false,
		InstalledAt:          now,
	}
	return renderInstallResult(cmd, res)
}

// installViaDaemon builds the pack's install batch and routes it through the
// live daemon's admin endpoint (the daemon holds the exclusive store lock, so
// an in-process open would fail). Collision detection (filterPackAgainstDB) is
// skipped: the dispatcher's UPSERT is idempotent — re-registering an existing
// kind or predicate is a no-op (A.6 documented behaviour change).
func installViaDaemon(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, pack *schemapack.Pack, now time.Time) error {
	installer := &schemapack.Installer{}
	events, ir, berr := installer.BuildEvents(pack, now)
	if berr != nil {
		return wrapInstallFatal(cmd, fmt.Errorf("build install events: %w", berr))
	}
	checks := kindPredicateMatChecks(ir)
	if aerr := appendBatchViaDaemonOrInProcess(ctx, cmd, dataDir, logger, "install", events, nil, checks); aerr != nil {
		return aerr
	}
	return renderInstallResult(cmd, &installResult{
		PackName:             ir.PackName,
		PackVersion:          pack.Manifest.Version,
		KindsRegistered:      ir.KindsRegistered,
		PredicatesRegistered: ir.PredicatesRegistered,
		GrantedScopes:        pack.Manifest.Scopes,
		DryRun:               false,
		InstalledAt:          now,
	})
}

// packRegisteredNames returns the entity-kind and predicate names a pack would
// register, in declaration order. Shared by the install and preset dry-run
// paths so both preview the same lists.
func packRegisteredNames(pack *schemapack.Pack) (kinds, predicates []string) {
	for i := range pack.Kinds {
		kinds = append(kinds, pack.Kinds[i].Name)
	}
	for i := range pack.Predicates {
		predicates = append(predicates, pack.Predicates[i].Predicate)
	}
	return kinds, predicates
}

// kindPredicateMatChecks builds the post-install materialisation probes for an
// install result: one COUNT query per registered entity kind and predicate.
func kindPredicateMatChecks(ir *schemapack.InstallResult) []matCheck {
	checks := make([]matCheck, 0, len(ir.KindsRegistered)+len(ir.PredicatesRegistered))
	for _, k := range ir.KindsRegistered {
		checks = append(checks, matCheck{query: "SELECT COUNT(1) FROM entity_kinds WHERE name = ?", args: []any{k}, label: k})
	}
	for _, p := range ir.PredicatesRegistered {
		checks = append(checks, matCheck{query: "SELECT COUNT(1) FROM predicates WHERE predicate = ?", args: []any{p}, label: p})
	}
	return checks
}

// emitInstallResolveError reports a pack-resolution failure: it prints to
// stderr and, in JSON/NDJSON mode, emits an error envelope to stdout so a
// piped consumer can read it without parsing stderr. Returns exit code 1
// (an expected failure: not found, not a pack, bad checksum).
func emitInstallResolveError(cmd *cobra.Command, err error) error {
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(cmd.ErrOrStderr(), "install: %v\n", err)
	if wr := output.From(cmd); wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		//nolint:errcheck // best-effort JSON error envelope
		_ = wr.Emit(installErrorResult{Error: err.Error(), Code: "E_NOT_FOUND"})
	}
	return silentError{code: 1, err: err}
}

// validateManifestCompat checks a pack manifest's OPTIONAL apiVersion and scopes
// fields with no I/O. It returns a clear error when the declared apiVersion is
// incompatible with this host (a host-vs-required message) or a declared scope
// token is outside the kernel scope enum (project|personal|team), and nil when
// both fields are absent or valid. Shared by `maz install` and `maz preset
// install` so both gate identically; each command emits the error with its own
// E_VALIDATION envelope.
func validateManifestCompat(m *schemapack.PackManifest) error {
	if err := plugin.CheckAPICompatibility(m.APIVersion); err != nil {
		return err
	}
	// ParseScopeSet rejects any token outside {project,personal,team}; the parsed
	// set is discarded here (we only need the all-tokens-valid verdict) — the
	// grant set is recorded from the manifest's declared list verbatim.
	if _, err := kernel.ParseScopeSet(m.Scopes); err != nil {
		return fmt.Errorf("manifest declares an invalid scope: %w", err)
	}
	return nil
}

// gateManifestCompat is the `maz install` manifest gate: it runs
// validateManifestCompat and, on failure, prints to stderr, emits an
// E_VALIDATION envelope in JSON mode, and returns silentError{code:1}. Both
// manifest fields are optional, so a manifest with neither gates to a no-op and
// installs exactly as before this feature.
func gateManifestCompat(cmd *cobra.Command, m *schemapack.PackManifest) error {
	if err := validateManifestCompat(m); err != nil {
		return emitInstallValidationError(cmd, err)
	}
	return nil
}

// emitInstallValidationError reports an expected manifest-validation failure
// (incompatible apiVersion, unknown scope token). It prints to stderr and, in
// JSON/NDJSON mode, emits an E_VALIDATION envelope to stdout so a piped consumer
// reads the failure without scraping stderr. Returns silentError{code:1}.
func emitInstallValidationError(cmd *cobra.Command, err error) error {
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(cmd.ErrOrStderr(), "install: %v\n", err)
	if wr := output.From(cmd); wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		//nolint:errcheck // best-effort JSON error envelope
		_ = wr.Emit(installErrorResult{Error: err.Error(), Code: errCodeValidation})
	}
	return silentError{code: 1, err: err}
}

// confirmScopeGrant obtains the user's consent to grant the scopes a manifest
// declares. It returns (true, nil) to proceed and (false, nil) when the user
// declines on a TTY (the caller prints "install cancelled" and exits 1, matching
// the non-interactive refusal below and the documented exit code).
//
//   - No declared scopes: returns true immediately (no prompt) — the manifest
//     requests nothing, so install proceeds exactly as before this feature.
//   - --yes: pre-consented, returns true without prompting.
//   - Interactive TTY: lists the requested scopes on stderr (so --json stdout
//     stays clean) and prompts [y/N] via the huh confirm.
//   - Non-interactive (JSON mode, non-TTY) without --yes: REFUSES with a clear
//     "re-run with --yes" message and silentError{code:1}; scopes must never be
//     granted silently in a scripted run.
func confirmScopeGrant(cmd *cobra.Command, name string, scopes []string, yes bool) (bool, error) {
	if len(scopes) == 0 {
		return true, nil
	}
	if yes {
		return true, nil
	}
	errOut := cmd.ErrOrStderr()
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(errOut, "install: %q requests scopes %v; re-run with --yes to grant them\n", name, scopes)
		return false, silentError{code: 1, err: fmt.Errorf("install: scope grant for %q not confirmed", name)}
	}
	//nolint:errcheck // best-effort stderr review block (interactive only)
	fmt.Fprintf(errOut, "\nPlugin %q requests read access to these scopes:\n", name)
	for _, s := range scopes {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(errOut, "  + %s\n", s)
	}
	return uiform.PromptYN(errOut, fmt.Sprintf("Grant %q access to these scopes?", name), false)
}

// filterPackAgainstDB returns a shallow copy of pack with user-owned collisions
// removed, plus the lists of skipped kind and predicate names. Each collision
// prints a warning. When force is true the original pack is returned unchanged
// with empty skip lists and a per-item warning that the user definition will be
// overwritten.
func filterPackAgainstDB(
	ctx context.Context,
	cmd *cobra.Command,
	db *sql.DB,
	pack *schemapack.Pack,
	force bool,
) (*schemapack.Pack, []string, []string, error) {
	userKinds, err := queryUserNames(ctx, db,
		"SELECT name FROM entity_kinds WHERE added_by = 'user'")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("query user kinds: %w", err)
	}
	userPreds, err := queryUserNames(ctx, db,
		"SELECT predicate FROM predicates WHERE added_by = 'user'")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("query user predicates: %w", err)
	}

	filtered := &schemapack.Pack{
		Manifest:   pack.Manifest,
		Kinds:      make([]schemapack.KindDef, 0, len(pack.Kinds)),
		Predicates: make([]schemapack.PredicateDef, 0, len(pack.Predicates)),
	}
	var skippedKinds, skippedPreds []string

	for i := range pack.Kinds {
		kd := pack.Kinds[i]
		if _, clash := userKinds[kd.Name]; clash {
			if force {
				//nolint:errcheck // best-effort stderr warning
				fmt.Fprintf(cmd.ErrOrStderr(),
					"install: WARNING: overwriting user-authored entity kind %q with --force\n", kd.Name)
				filtered.Kinds = append(filtered.Kinds, kd)
			} else {
				//nolint:errcheck // best-effort stderr warning
				fmt.Fprintf(cmd.ErrOrStderr(),
					"install: skipping entity kind %q — user-authored definition exists (use --force to overwrite)\n", kd.Name)
				skippedKinds = append(skippedKinds, kd.Name)
			}
			continue
		}
		filtered.Kinds = append(filtered.Kinds, kd)
	}

	for i := range pack.Predicates {
		pd := pack.Predicates[i]
		if _, clash := userPreds[pd.Predicate]; clash {
			if force {
				//nolint:errcheck // best-effort stderr warning
				fmt.Fprintf(cmd.ErrOrStderr(),
					"install: WARNING: overwriting user-authored predicate %q with --force\n", pd.Predicate)
				filtered.Predicates = append(filtered.Predicates, pd)
			} else {
				//nolint:errcheck // best-effort stderr warning
				fmt.Fprintf(cmd.ErrOrStderr(),
					"install: skipping predicate %q — user-authored definition exists (use --force to overwrite)\n", pd.Predicate)
				skippedPreds = append(skippedPreds, pd.Predicate)
			}
			continue
		}
		filtered.Predicates = append(filtered.Predicates, pd)
	}

	return filtered, skippedKinds, skippedPreds, nil
}

// queryUserNames runs query (which must SELECT a single string column) and
// returns the results as a set. Used to find user-authored kinds/predicates
// before an install overwrites them.
func queryUserNames(ctx context.Context, db *sql.DB, query string) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = struct{}{}
	}
	return names, rows.Err()
}

// runInstallList prints the builtin pack catalogue to stdout.
func runInstallList(cmd *cobra.Command) error {
	packs, err := schemapack.LoadAllBuiltins()
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "install --list: %v\n", err)
		return silentError{code: 2, err: err}
	}
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		type packInfo struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			Description string `json:"description"`
		}
		if wr.Format() == output.FormatNDJSON {
			// NDJSON: one compact JSON object per line, not a pretty array.
			for _, p := range packs {
				if err := wr.Emit(packInfo{
					Name:        p.Manifest.Name,
					Version:     p.Manifest.Version,
					Description: p.Manifest.Description,
				}); err != nil {
					return err
				}
			}
			return nil
		}
		// FormatJSON: a single pretty-printed array.
		w := cmd.OutOrStdout()
		out := make([]packInfo, 0, len(packs))
		for _, p := range packs {
			out = append(out, packInfo{
				Name:        p.Manifest.Name,
				Version:     p.Manifest.Version,
				Description: p.Manifest.Description,
			})
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "name"},
		{Title: "version"},
		{Title: "description"},
	})
	for _, p := range packs {
		tbl.Row(p.Manifest.Name, p.Manifest.Version, p.Manifest.Description)
	}
	return tbl.Render()
}

// packSource records where a resolved pack came from: a local directory (an
// untrusted, human-authored bundle), a builtin embedded in the binary (trusted,
// reviewed in-tree), or a remote git source (untrusted, third-party, fetched +
// integrity-pinned). The preset install gate uses it to decide whether a
// non-builtin instructions body must be human-confirmed before it can steer an
// agent.
type packSource int

const (
	// SourceUnknown is the zero value: a pack that did not resolve (the error
	// path). Keeping it at iota 0 means an unset packSource never masquerades as
	// a real provenance, so a future caller that inspects the source on the error
	// path is not misled into treating an unresolved pack as a trusted builtin.
	sourceUnknown packSource = iota
	sourceLocalDir
	sourceBuiltin
	// SourceRemote is a bundle fetched from a git remote (#224): third-party and
	// untrusted, so it is human-gated exactly like a local directory.
	sourceRemote
)

// resolved bundles a parsed pack with where it came from and (remote only) its
// fetch provenance. Prov is nil for local-directory and builtin packs.
type resolved struct {
	pack   *schemapack.Pack
	source packSource
	prov   *fetch.Provenance
}

// resolvePack resolves the pack argument to a parsed Pack, discarding the source
// and provenance. Callers that need the source (the preset install gate) use
// resolvePackWithSource directly.
func resolvePack(ctx context.Context, arg, dataDir, wantChecksum string) (*schemapack.Pack, error) {
	r, err := resolvePackWithSource(ctx, arg, dataDir, wantChecksum)
	return r.pack, err
}

// resolvePackWithSource resolves the pack argument in order: remote (when arg
// lexically looks like a git URL/handle), then a local directory (so a dir named
// like a builtin shadows it), then the builtins. Remote detection is lexical, so
// a path or builtin name never triggers a network fetch. WantChecksum, when
// non-empty, gates a remote fetch: a bundle whose checksum differs is refused
// before it is parsed. Passing --checksum for a local-directory or builtin
// source is an error: the checksum gate is a remote-integrity mechanism and
// silently ignoring it for local sources would give false confidence.
func resolvePackWithSource(ctx context.Context, arg, dataDir, wantChecksum string) (resolved, error) {
	if fetch.IsRemoteSpec(arg) {
		localDir, prov, ferr := fetch.Fetch(ctx, arg, fetch.Options{DataDir: dataDir, WantChecksum: wantChecksum})
		if ferr != nil {
			return resolved{source: sourceUnknown}, ferr
		}
		p, perr := schemapack.ParseDir(localDir)
		if perr != nil {
			return resolved{source: sourceUnknown}, perr
		}
		return resolved{pack: p, source: sourceRemote, prov: &prov}, nil
	}
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		if wantChecksum != "" {
			return resolved{source: sourceUnknown}, fmt.Errorf(
				"--checksum only applies to remote sources; local directory %q is read directly without fetching", arg)
		}
		p, perr := schemapack.ParseDir(arg)
		return resolved{pack: p, source: sourceLocalDir}, perr
	}
	if wantChecksum != "" {
		// arg is neither a remote spec nor a local directory (it could be a builtin
		// name). Reject early so the user is not misled into thinking the builtin
		// binary was verified against their checksum.
		return resolved{source: sourceUnknown}, fmt.Errorf(
			"--checksum only applies to remote sources; %q is not a remote git spec or local directory", arg)
	}
	p, err := schemapack.LoadBuiltin(arg)
	if err != nil {
		return resolved{source: sourceUnknown}, fmt.Errorf("pack %q not found as a remote spec, local directory, or builtin pack; "+
			"builtin packs: %v", arg, schemapack.BuiltinPackNames)
	}
	return resolved{pack: p, source: sourceBuiltin}, nil
}

// renderInstallResult renders the install result as JSON or CLI text.
func renderInstallResult(cmd *cobra.Command, res *installResult) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	return renderInstallCLI(w, res)
}

// renderInstallCLI prints a human-readable install summary.
func renderInstallCLI(w io.Writer, res *installResult) error {
	prefix := "Installed"
	if res.DryRun {
		prefix = "Would install"
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "Pack\t%s@%s\n", res.PackName, res.PackVersion); err != nil {
		return err
	}
	if res.DryRun {
		if _, err := fmt.Fprintf(tw, "Mode\tdry-run (no events appended)\n"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(tw, "Installed at\t%s\n", res.InstalledAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// writeBulletList no-ops on an empty slice, so the skip / granted-scopes
	// sections need no guard: an empty section prints nothing.
	scopeHeader := "\nGranted scopes:\n"
	if res.DryRun {
		scopeHeader = "\nWould grant scopes:\n"
	}
	for _, sec := range []bulletSection{
		{fmt.Sprintf("\n%s %d entity kind(s):\n", prefix, len(res.KindsRegistered)), res.KindsRegistered},
		{fmt.Sprintf("\n%s %d predicate(s):\n", prefix, len(res.PredicatesRegistered)), res.PredicatesRegistered},
		{fmt.Sprintf("\nSkipped %d entity kind(s) (user-owned; use --force to overwrite):\n", len(res.KindsSkipped)), res.KindsSkipped},
		{fmt.Sprintf("\nSkipped %d predicate(s) (user-owned; use --force to overwrite):\n", len(res.PredicatesSkipped)), res.PredicatesSkipped},
		{scopeHeader, res.GrantedScopes},
	} {
		if err := writeBulletList(w, sec.header, sec.items); err != nil {
			return err
		}
	}
	if !res.DryRun {
		if _, err := fmt.Fprintf(w, "\nNext steps:\n  maz schema kinds list\n  maz schema predicates\n"); err != nil {
			return err
		}
	}
	return nil
}

// bulletSection pairs a header with its items for the data-driven render loops
// in renderInstallCLI / renderPresetInstallCLI. An empty items slice renders
// nothing (writeBulletList no-ops), so optional sections need no guard.
type bulletSection struct {
	header string
	items  []string
}

// writeBulletList prints header followed by each item as "  + item\n".
// When items is empty, nothing is written.
func writeBulletList(w io.Writer, header string, items []string) error {
	if len(items) == 0 {
		return nil
	}
	if _, err := fmt.Fprint(w, header); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(w, "  + %s\n", item); err != nil {
			return err
		}
	}
	return nil
}

// wrapInstallFatal prints the error to stderr and returns silentError(2).
func wrapInstallFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "install", err)
}

// appendExtensionKindEvents marshals every event in evs to a kernel.Event
// batch and appends them in a single log.Append call. Using one batch write
// avoids partial-import state: either all kinds are durable or none are.
func appendExtensionKindEvents(
	ctx context.Context,
	log *kernel.FileEventLog,
	evs []*ontology.EntityKindRegisteredEvent,
	source string,
) error {
	batch := make([]kernel.Event, 0, len(evs))
	for _, ev := range evs {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("marshal kind event %q: %w", ev.Name, err)
		}
		batch = append(batch, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindEntityKindRegistered),
			Source:     source,
			Confidence: 1.0,
			Payload:    payload,
		})
	}
	if len(batch) == 0 {
		return nil
	}
	if _, err := log.Append(ctx, batch); err != nil {
		return fmt.Errorf("append kind events: %w", err)
	}
	return nil
}
