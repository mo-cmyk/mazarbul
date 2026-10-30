package cli

// preset.go implements the "maz preset" command group: install, uninstall,
// list, show. A preset is a schema pack (manifest.toml + kinds/ + predicates.yaml)
// bundled with an instructions.md agent-steering body. Installing one seeds the
// ontology via the SAME entityKindRegistered/predicateRegistered events as
// `maz install`, AND appends a presetInstalled event — in one atomic batch — so
// the body steers agents via the getContext "Operating instructions" section.
//
// Security: the instructions body is untrusted input flowing into the highest-
// priority agent-control surface. A NON-builtin preset (a local directory) is
// surfaced for human review and requires explicit confirmation (interactive
// prompt, or --yes) before install; a non-interactive install without --yes is
// refused. There is deliberately NO agent-callable install — presets are
// CLI-only; the listPresets MCP tool is read-only.
//
// Exit codes:
//
//	0   success (installed / uninstalled / listed / shown), or --dry-run
//	1   expected failure: not found, not a preset, bad args, unconfirmed install
//	2   unexpected failure: storage or wiring error

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/plugin/fetch"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// errCodeValidation is the MCP error code emitted for bad-input failures in
// preset and install commands, matching mcp.CodeValidation = "E_VALIDATION".
const errCodeValidation = "E_VALIDATION"

// newPresetCmd returns the "maz preset" parent command.
func newPresetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Manage agent-steering presets (schema pack + instructions)",
		Long: "Manage presets: a schema pack bundled with an instructions.md body that\n" +
			"steers agents via the getContext \"Operating instructions\" section.\n\n" +
			"Installing a preset seeds its entity kinds and predicates and activates its\n" +
			"instructions; uninstalling deactivates the instructions but leaves the seeded\n" +
			"kinds and predicates in place. See the subcommands below.",
	}
	cmd.AddCommand(newPresetInstallCmd(), newPresetUninstallCmd(), newPresetListCmd(), newPresetShowCmd(), newPresetPackageCmd())
	return cmd
}

// presetInstallFlags holds parsed flags for "maz preset install".
type presetInstallFlags struct {
	dryRun   bool
	force    bool
	yes      bool
	checksum string
}

func newPresetInstallCmd() *cobra.Command {
	flags := &presetInstallFlags{}
	cmd := &cobra.Command{
		Use:   "install <preset>",
		Short: "Install a preset: seed its ontology and activate its instructions",
		Long: `Install a preset, seeding its entity kinds and predicates AND activating
its instructions.md body so it steers agents via getContext.

<preset> can be:
  - the name of a builtin preset (shipped in the binary),
  - a path to a local directory containing manifest.toml and instructions.md, or
  - a remote git source: github.com/user/repo[@ref][#subdir] or a full git URL
    (https://, ssh://, git@host:owner/repo). The clone is pinned to the resolved
    commit SHA, which is recorded alongside the source and bundle checksum as
    provenance (see 'maz preset show').

The seeding events and the presetInstalled event are appended in one atomic
batch and materialised immediately — no 'maz rebuild' needed. If a daemon serves
this data dir it holds the store lock, so install refuses; stop it and retry.

A NON-builtin (local OR remote) preset's instructions body flows into the
highest-priority agent-control surface, so it is surfaced for review and must be
confirmed before install: answer the interactive prompt, or pass --yes. A
non-interactive install of a non-builtin preset without --yes is refused.

Remote sources require git on PATH and reuse your existing git credentials.
Pass --checksum sha256:... to refuse a fetched bundle whose canonical contents
do not match (verified before the body is ever parsed or surfaced); compute the
expected value with 'maz preset package <dir>'.

Exit codes:
  0   installed (events appended), --dry-run, or interactively declined
  1   expected failure: not found, not a preset, no argument, a checksum
      mismatch, or a non-interactive refusal (non-builtin body, no --yes, no
      TTY to confirm)
  2   unexpected failure: storage or wiring error

Examples:
  maz preset install crm
  maz preset install ./my-preset/ --yes
  maz preset install github.com/acme/crm-preset --yes
  maz preset install github.com/acme/presets@v1.0.0#crm --yes --checksum sha256:abc123...
  maz preset install crm --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				//nolint:errcheck // best-effort stderr
				fmt.Fprintf(cmd.ErrOrStderr(), "preset install: <preset> argument required\n")
				return silentError{code: 1, err: errors.New("preset install: <preset> argument required")}
			}
			if len(args) > 1 {
				return fmt.Errorf("preset install: accepts 1 argument, got %d", len(args))
			}
			return runPresetInstall(cmd, args[0], flags)
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "show what would be seeded/activated without appending events")
	cmd.Flags().BoolVar(&flags.force, "force", false, "overwrite user-authored predicates/kinds on name collision (data loss)")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "skip the non-builtin instructions-review confirmation prompt")
	cmd.Flags().StringVar(&flags.checksum, "checksum", "", "require a fetched remote bundle to match this sha256:... checksum (tamper gate)")
	return cmd
}

// presetInstallResult is the JSON/CLI output shape for "maz preset install".
type presetInstallResult struct {
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	Description          string   `json:"description"`
	KindsRegistered      []string `json:"kindsRegistered"`
	PredicatesRegistered []string `json:"predicatesRegistered"`
	KindsSkipped         []string `json:"kindsSkipped,omitempty"`
	PredicatesSkipped    []string `json:"predicatesSkipped,omitempty"`
	// GrantedScopes lists the scopes the manifest declared and the operator
	// granted at install (confirmed via the install gate or --yes). Empty/omitted
	// when the manifest declares no scopes.
	GrantedScopes []string   `json:"grantedScopes,omitempty"`
	DryRun        bool       `json:"dryRun"`
	InstalledAt   *time.Time `json:"installedAt,omitempty"`
}

// gatePresetManifestCompat runs the shared apiVersion + scope-token gate (see
// validateManifestCompat) for `maz preset install`, emitting any failure with
// the preset E_VALIDATION envelope. Both manifest fields are optional, so a
// preset declaring neither gates to a no-op and installs exactly as before.
func gatePresetManifestCompat(cmd *cobra.Command, m *schemapack.PackManifest) error {
	if err := validateManifestCompat(m); err != nil {
		return emitPresetInstallError(cmd, err, errCodeValidation)
	}
	return nil
}

func runPresetInstall(cmd *cobra.Command, arg string, flags *presetInstallFlags) error { //nolint:gocognit,gocyclo // install orchestration; linear steps that cannot be split without losing context
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir := presetDataDir(cmd)

	r, err := resolvePackWithSource(ctx, arg, dataDir, flags.checksum)
	if err != nil {
		// --checksum on a local or builtin source is flag misuse (E_VALIDATION);
		// every other resolution failure is a not-found (E_NOT_FOUND).
		code := "E_NOT_FOUND"
		if flags.checksum != "" && !fetch.IsRemoteSpec(arg) {
			code = errCodeValidation
		}
		return emitPresetInstallError(cmd, err, code)
	}
	pack, source := r.pack, r.source
	if !pack.IsPreset() {
		err := fmt.Errorf("pack %q has no instructions.md, so it is not a preset; install it with `maz install`", pack.Manifest.Name)
		return emitPresetInstallError(cmd, err, "E_NOT_FOUND")
	}

	// Install-time manifest gate: refuse an incompatible apiVersion or an invalid
	// scope token before any state is touched (and before dry-run). No-op when the
	// manifest declares neither field.
	if gerr := gatePresetManifestCompat(cmd, &pack.Manifest); gerr != nil {
		return gerr
	}

	if flags.dryRun {
		kinds, predicates := packRegisteredNames(pack)
		return renderPresetInstallResult(cmd, &presetInstallResult{
			Name:                 pack.Manifest.Name,
			Version:              pack.Manifest.Version,
			Description:          pack.Manifest.Description,
			KindsRegistered:      kinds,
			PredicatesRegistered: predicates,
			GrantedScopes:        pack.Manifest.Scopes,
			DryRun:               true,
		})
	}

	proceed, gerr := confirmNonBuiltinInstall(cmd, pack, source, flags)
	if gerr != nil {
		return gerr
	}
	if !proceed {
		//nolint:errcheck // best-effort stdout
		fmt.Fprintln(cmd.OutOrStdout(), "preset install cancelled")
		return nil // exit 0: user declined
	}

	now := time.Now().UTC()

	// When a daemon is live, we cannot open the store in-process. Instead build
	// the batch without appending it, then route through the admin endpoint.
	// Collision detection (filterPackAgainstDB) is skipped: the dispatcher's
	// UPSERT is idempotent — re-registering an existing kind or predicate is a
	// no-op (A.6 documented behavior change). The idempotency guard becomes a
	// Precheck so a duplicate install exits 0 without appending.
	if preflightDaemon("", dataDir) != nil {
		batch, ir, berr := buildPresetBatch(cmd, pack, pack, r.prov, now)
		if berr != nil {
			return berr
		}

		// Idempotency: if the preset row already exists and --force is absent,
		// the Precheck will cause the admin endpoint to return alreadyPresent and
		// we skip rendering. This mirrors the in-process guard below.
		var precheck []matCheck
		if !flags.force {
			precheck = []matCheck{{
				query:   "SELECT COUNT(1) FROM presets WHERE name = ?",
				args:    []any{pack.Manifest.Name},
				label:   "preset:" + pack.Manifest.Name,
				confirm: "absent", // abort when already present
			}}
		}
		checks := buildPresetMatChecks(ir, pack.Manifest.Name)
		if aerr := appendBatchViaDaemonOrInProcess(ctx, cmd, dataDir, logger, "preset install", batch, precheck, checks); aerr != nil {
			return aerr
		}
		// Check if we got an alreadyPresent response: appendBatchViaDaemonOrInProcess
		// returns nil on alreadyPresent. Render the "already installed" message.
		// We detect the no-op by checking if the precheck was set and the batch
		// size is the same (batch was not modified). Since appendBatchViaDaemonOrInProcess
		// returns nil for both "appended" and "alreadyPresent", we must check
		// post-fact. The simplest approach: always render the full install result
		// (skippedKinds/Preds are empty under daemon since we skipped filterPackAgainstDB).
		kinds, _ := packRegisteredNames(pack)
		preds := make([]string, 0, len(pack.Predicates))
		for i := range pack.Predicates {
			preds = append(preds, pack.Predicates[i].Predicate)
		}
		return renderPresetInstallResult(cmd, &presetInstallResult{
			Name:                 ir.PackName,
			Version:              pack.Manifest.Version,
			Description:          pack.Manifest.Description,
			KindsRegistered:      ir.KindsRegistered,
			PredicatesRegistered: ir.PredicatesRegistered,
			KindsSkipped:         kinds[:0], // empty: collision check deferred to UPSERT
			PredicatesSkipped:    preds[:0],
			GrantedScopes:        pack.Manifest.Scopes,
			InstalledAt:          &now,
		})
	}

	// No daemon: open wiring in-process for collision detection + install.
	wired, closeFn, err := openMaterializingLog(ctx, cmd, dataDir, logger, "preset install")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("preset install: close wiring", zap.Error(cerr))
		}
	}()

	db, qerr := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if qerr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("open sqlite for collision check: %w", qerr))
	}
	filteredPack, skippedKinds, skippedPreds, cerr := filterPackAgainstDB(ctx, cmd, db, pack, flags.force)
	if cerr != nil {
		return wrapPresetFatal(cmd, cerr)
	}

	// Idempotency guard: if this preset is already active and --force is absent,
	// return a clear "already installed" message and exit 0. Without this gate,
	// re-running preset install appends duplicate entityKindRegistered /
	// predicateRegistered / presetInstalled events on every invocation, causing
	// unbounded log growth (read-time dedup hides it but the log still grows).
	// With --force, we fall through and re-append — useful for upgrading the
	// instructions body without uninstalling first.
	if !flags.force {
		var n int
		if serr := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM presets WHERE name = ?", pack.Manifest.Name).Scan(&n); serr != nil {
			return wrapPresetFatal(cmd, fmt.Errorf("check existing preset: %w", serr))
		}
		if n > 0 {
			msg := fmt.Sprintf("preset %q is already installed (use --force to re-append its events)", pack.Manifest.Name)
			if isPresetJSONMode(cmd) {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				//nolint:errcheck // best-effort JSON
				_ = enc.Encode(map[string]string{"status": "already_installed", "name": pack.Manifest.Name})
			} else {
				//nolint:errcheck // best-effort stdout
				fmt.Fprintf(cmd.OutOrStdout(), "preset install: %s\n", msg)
			}
			return nil // exit 0: no-op is success
		}
	}

	ir, serr := seedAndActivatePreset(ctx, cmd, wired, pack, filteredPack, r.prov, now)
	if serr != nil {
		return serr
	}

	return renderPresetInstallResult(cmd, &presetInstallResult{
		Name:                 ir.PackName,
		Version:              pack.Manifest.Version,
		Description:          pack.Manifest.Description,
		KindsRegistered:      ir.KindsRegistered,
		PredicatesRegistered: ir.PredicatesRegistered,
		KindsSkipped:         skippedKinds,
		PredicatesSkipped:    skippedPreds,
		GrantedScopes:        pack.Manifest.Scopes,
		InstalledAt:          &now,
	})
}

// seedAndActivatePreset builds the seeding events (entityKindRegistered /
// predicateRegistered) and the presetInstalled marker, appends them in ONE
// atomic batch — the preset event LAST so a brief reader that observes the
// active row is guaranteed the vocabulary is already applied — then confirms
// every seeded kind/predicate plus the presets row materialised.
//
// This is the in-process path (no daemon running). The daemon path uses
// buildPresetBatch + appendBatchViaDaemonOrInProcess directly.
func seedAndActivatePreset(
	ctx context.Context,
	cmd *cobra.Command,
	wired *daemon.Wired,
	pack, filteredPack *schemapack.Pack,
	prov *fetch.Provenance,
	now time.Time,
) (*schemapack.InstallResult, error) {
	batch, ir, berr := buildPresetBatch(cmd, pack, filteredPack, prov, now)
	if berr != nil {
		return nil, berr
	}
	if _, aerr := wired.Orchestrator.Log.Append(ctx, batch); aerr != nil {
		return nil, wrapPresetFatal(cmd, fmt.Errorf("append preset events: %w", aerr))
	}
	checks := buildPresetMatChecks(ir, pack.Manifest.Name)
	if mcerr := confirmMaterialised(ctx, cmd, wired, "preset install", checks...); mcerr != nil {
		return nil, mcerr
	}
	return ir, nil
}

// buildPresetBatch builds the event batch for a preset install without
// appending it. The batch is: ontology events (kinds + predicates from
// filteredPack) + seed events + presetInstalled marker. This is the
// batch-only form used by the daemon-routing path in runPresetInstall.
func buildPresetBatch(
	cmd *cobra.Command,
	pack, filteredPack *schemapack.Pack,
	prov *fetch.Provenance,
	now time.Time,
) ([]kernel.Event, *schemapack.InstallResult, error) {
	installer := &schemapack.Installer{} // BuildEvents does not use Log
	ontologyEvents, ir, berr := installer.BuildEvents(filteredPack, now)
	if berr != nil {
		return nil, nil, wrapPresetFatal(cmd, fmt.Errorf("build seeding events: %w", berr))
	}
	presetEvent := &ontology.PresetInstalledEvent{
		Name:         pack.Manifest.Name,
		Version:      pack.Manifest.Version,
		Description:  pack.Manifest.Description,
		Instructions: pack.Instructions,
		AddedAt:      now,
	}
	if prov != nil {
		presetEvent.SourceURL = prov.SourceURL
		presetEvent.PinnedRef = prov.PinnedRef
		presetEvent.Checksum = prov.Checksum
	}
	presetPayload, merr := json.Marshal(presetEvent)
	if merr != nil {
		return nil, nil, wrapPresetFatal(cmd, fmt.Errorf("marshal presetInstalled: %w", merr))
	}
	seedEvents := pack.SeedEvents
	batch := make([]kernel.Event, 0, len(ontologyEvents)+len(seedEvents)+1)
	batch = append(batch, ontologyEvents...)
	batch = append(batch, seedEvents...)
	batch = append(batch, kernel.Event{
		Kind:       kernel.EventKind(ontology.KindPresetInstalled),
		Source:     "preset/install",
		Scope:      kernel.ScopePersonal,
		Confidence: 1.0,
		Payload:    presetPayload,
	})
	return batch, ir, nil
}

// buildPresetMatChecks builds the post-append materialisation probes for a
// preset install: one COUNT query per registered kind, predicate, and the
// presets row itself.
func buildPresetMatChecks(ir *schemapack.InstallResult, presetName string) []matCheck {
	checks := make([]matCheck, 0, len(ir.KindsRegistered)+len(ir.PredicatesRegistered)+1)
	for _, k := range ir.KindsRegistered {
		checks = append(checks, matCheck{
			query:   "SELECT COUNT(1) FROM entity_kinds WHERE name = ?",
			args:    []any{k},
			label:   k,
			confirm: "present",
		})
	}
	for _, p := range ir.PredicatesRegistered {
		checks = append(checks, matCheck{
			query:   "SELECT COUNT(1) FROM predicates WHERE predicate = ?",
			args:    []any{p},
			label:   p,
			confirm: "present",
		})
	}
	checks = append(checks, matCheck{
		query:   "SELECT COUNT(1) FROM presets WHERE name = ?",
		args:    []any{presetName},
		label:   "preset:" + presetName,
		confirm: "present",
	})
	return checks
}

// installBuiltinPreset seeds and activates a builtin preset by name against
// dataDir, reusing the same materialising-append path as `maz preset install`.
// It powers the `maz init` agent-steering on-ramp. Builtins are trusted (shipped
// in the binary), so there is no instructions-review gate. Quiet on success —
// the caller prints any user-facing confirmation. It opens its own wiring and so
// requires the store lock: run it before a daemon is started for dataDir.
func installBuiltinPreset(ctx context.Context, cmd *cobra.Command, dataDir, name string) (*schemapack.InstallResult, error) {
	logger := logging.FromContext(ctx)
	pack, err := schemapack.LoadBuiltin(name)
	if err != nil {
		return nil, fmt.Errorf("load builtin preset %q: %w", name, err)
	}
	if !pack.IsPreset() {
		return nil, fmt.Errorf("builtin %q is not a preset", name)
	}
	wired, closeFn, err := openMaterializingLog(ctx, cmd, dataDir, logger, "init preset")
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("init preset: close wiring", zap.Error(cerr))
		}
	}()
	db, qerr := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if qerr != nil {
		return nil, fmt.Errorf("open sqlite for preset seeding: %w", qerr)
	}
	filteredPack, _, _, ferr := filterPackAgainstDB(ctx, cmd, db, pack, false)
	if ferr != nil {
		return nil, ferr
	}
	now := time.Now().UTC()
	// Builtins carry no remote provenance.
	ir, serr := seedAndActivatePreset(ctx, cmd, wired, pack, filteredPack, nil, now)
	if serr != nil {
		return nil, serr
	}
	return ir, nil
}

// confirmNonBuiltinInstall runs the human-review gate before a preset's
// untrusted instructions body can steer an agent. It covers BOTH local
// directories and remotely-fetched bundles (a third party's body is no more
// trusted than a local one); only builtins — shipped and reviewed in the binary
// — skip it. Using `!= sourceBuiltin` is fail-closed, so an unknown/error source
// is gated too. It returns proceed=false (nil err) when the user declines on a
// TTY, and an error when a non-interactive install lacks --yes.
func confirmNonBuiltinInstall(cmd *cobra.Command, pack *schemapack.Pack, source packSource, flags *presetInstallFlags) (bool, error) {
	if source == sourceBuiltin {
		return true, nil
	}
	// A remote install with --yes and no --checksum skips BOTH the body review
	// and the integrity pin, so it trusts whatever the remote serves at this
	// moment. Surface that on stderr (the install still proceeds).
	if source == sourceRemote && flags.yes && flags.checksum == "" {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(),
			"preset install: warning: installing a remote preset with --yes and no --checksum; the remote tip is trusted as-is")
	}
	return surfaceAndConfirmPreset(cmd, pack, flags.yes)
}

// surfaceAndConfirmPreset obtains consent to install a non-builtin preset whose
// instructions body is untrusted. With --yes the operator has pre-consented, so
// it returns true WITHOUT echoing the body (otherwise a scripted `--yes`
// install would dump the untrusted body to a captured stderr — the very leak
// this feature denies to agent surfaces). In a non-interactive context (JSON
// mode or non-TTY) without --yes it REFUSES (exit 1): an unreviewed body must
// never install non-interactively. Only in an interactive TTY does it print the
// manifest, the vocabulary, and the FULL verbatim body for human review, then
// prompt [y/N] on stderr (so --json stdout stays clean).
func surfaceAndConfirmPreset(cmd *cobra.Command, pack *schemapack.Pack, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	errOut := cmd.ErrOrStderr()
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(errOut, "preset install: a non-builtin preset's instructions must be reviewed; re-run with --yes to confirm")
		return false, silentError{code: 1, err: errors.New("preset install: unconfirmed non-builtin instructions body")}
	}
	//nolint:errcheck // best-effort stderr review block (interactive only)
	fmt.Fprintf(errOut, "\nPreset %q (v%s) — %s\n", pack.Manifest.Name, pack.Manifest.Version, pack.Manifest.Description)
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(errOut, "Will seed %d entity kind(s) and %d predicate(s).\n", len(pack.Kinds), len(pack.Predicates))
	if len(pack.Manifest.Scopes) > 0 {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(errOut, "Requests read access to scopes: %v\n", pack.Manifest.Scopes)
	}
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(errOut, "\n─── instructions.md (will steer your agent) ───\n%s\n──────────────────────────────────────────────\n", pack.Instructions)
	return uiform.PromptYN(errOut, "Install this preset and let its instructions steer your agent?", false)
}

// presetUninstallFlags holds parsed flags for "maz preset uninstall".
type presetUninstallFlags struct {
	yes bool
}

func newPresetUninstallCmd() *cobra.Command {
	flags := &presetUninstallFlags{}
	cmd := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Deactivate a preset's instructions (its kinds/predicates remain)",
		Long: `Deactivate a preset: drop its active row and instructions body so it no
longer steers agents. This is a CLEAN CUT — the entity kinds and predicates the
preset seeded REMAIN as plain extension ontology and stay resolvable. Review or
remove them with 'maz schema kinds list' / 'maz schema predicates'.

Exit codes:
  0   uninstalled, or interactively declined
  1   expected failure: preset not active, no argument, or a
      non-interactive refusal (no --yes, no TTY to confirm)
  2   unexpected failure: storage or wiring error

Examples:
  maz preset uninstall crm
  maz preset uninstall crm --yes`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("preset uninstall: accepts 1 argument, got %d", len(args))
			}
			return runPresetUninstall(cmd, args[0], flags)
		},
	}
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// confirmPresetUninstall gates the deactivation. Returns (true, nil) to
// proceed, (false, nil) when the user declines interactively (a "cancelled"
// note is printed), and (false, err) when a scripted/non-interactive
// invocation is missing --yes. Mirrors confirmRebuild / confirmSignEvents.
func confirmPresetUninstall(cmd *cobra.Command, name string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintln(cmd.ErrOrStderr(), "preset uninstall: pass --yes to confirm in non-interactive mode")
		return false, silentError{code: 1, err: errors.New("preset uninstall: unconfirmed")}
	}
	proceed, err := uiform.PromptYN(cmd.ErrOrStderr(),
		fmt.Sprintf("Deactivate preset %q? Its instructions stop steering the agent; its kinds/predicates remain.", name), false)
	if err != nil {
		return false, err
	}
	if !proceed {
		//nolint:errcheck // best-effort stdout
		fmt.Fprintln(cmd.OutOrStdout(), "preset uninstall cancelled")
		return false, nil
	}
	return true, nil
}

// presetUninstallResult is the JSON output shape for "maz preset uninstall".
// It mirrors presetInstallResult in spirit: a stable, documented JSON envelope
// so piped invocations and agent tooling can parse the result without scraping
// plain text. The "status" field always carries "uninstalled" on success.
type presetUninstallResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Note repeats the human-facing reminder (text mode prints it too) that
	// deactivating a preset leaves its entity kinds and predicates installed.
	Note string `json:"note"`
}

func runPresetUninstall(cmd *cobra.Command, name string, flags *presetUninstallFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	dataDir := presetDataDir(cmd)
	jsonMode := isPresetJSONMode(cmd)

	// "is it installed?" check: under a live daemon we cannot open the store
	// in-process. The daemon-routed path folds the not-installed check into a
	// Precheck (confirm:"present"). Under no daemon, read directly via in-process
	// wiring (unchanged behaviour).
	if perr := preflightDaemon("", dataDir); perr != nil {
		return uninstallPresetViaDaemon(ctx, cmd, dataDir, logger, name, jsonMode, flags.yes)
	}

	// No daemon: in-process path.
	wired, closeFn, err := openMaterializingLog(ctx, cmd, dataDir, logger, "preset uninstall")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("preset uninstall: close wiring", zap.Error(cerr))
		}
	}()

	db, qerr := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if qerr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("open sqlite: %w", qerr))
	}
	var n int
	if serr := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM presets WHERE name = ?", name).Scan(&n); serr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("check preset: %w", serr))
	}
	if n == 0 {
		return renderPresetNotInstalled(cmd, name, jsonMode)
	}

	if proceed, cerr := confirmPresetUninstall(cmd, name, flags.yes); cerr != nil || !proceed {
		return cerr
	}

	payload, merr := json.Marshal(&ontology.PresetDeactivatedEvent{Name: name, RemovedAt: time.Now().UTC()})
	if merr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("marshal presetDeactivated: %w", merr))
	}
	if _, aerr := wired.Orchestrator.Log.Append(ctx, []kernel.Event{{
		Kind:   kernel.EventKind(ontology.KindPresetDeactivated),
		Source: "preset:" + name,
		// Stamp the personal scope explicitly to mirror the presetInstalled event
		// (seedAndActivatePreset). The log would otherwise backfill DefaultScope,
		// but asserting it at mint keeps the install/uninstall pair symmetric so
		// the tombstone is bucketed into the same personal sync chunk as its
		// install — a remote rebuild then replays both, never the install alone.
		Scope:      kernel.ScopePersonal,
		Confidence: 1.0,
		Payload:    payload,
	}}); aerr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("append presetDeactivated: %w", aerr))
	}

	if caerr := confirmAbsent(ctx, cmd, wired, "preset uninstall",
		matCheck{
			query:   "SELECT COUNT(1) FROM presets WHERE name = ?",
			args:    []any{name},
			label:   "preset:" + name,
			confirm: "absent",
		}); caerr != nil {
		return caerr
	}

	return renderPresetUninstallResult(cmd, name, jsonMode)
}

// renderPresetNotInstalled emits the "not installed" result shared by the
// daemon-routed and in-process uninstall paths: a JSON E_NOT_FOUND envelope in
// JSON mode, a stderr line otherwise, and exit 1. Keeping this identical across
// both paths is why uninstall behaves the same whether or not a daemon runs.
func renderPresetNotInstalled(cmd *cobra.Command, name string, jsonMode bool) error {
	notInstalled := fmt.Errorf("preset %q is not installed", name)
	if jsonMode {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		//nolint:errcheck // best-effort JSON: the caller reads the exit code, not this write error
		_ = enc.Encode(errorEnvelope{
			Error: notInstalled.Error(),
			Code:  "E_NOT_FOUND",
		})
	}
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(cmd.ErrOrStderr(), "preset uninstall: %v\n", notInstalled)
	return silentError{code: 1, err: notInstalled}
}

// uninstallPresetViaDaemon routes a preset deactivation through the live
// daemon's admin endpoint (the daemon holds the exclusive store lock, so an
// in-process write would fail). It first checks the preset exists via the
// lock-tolerant read-only pool (openSchemaReadDB) — returning the SAME exit-1
// "not installed" result as the in-process path — then confirms and routes the
// tombstone. A bare confirm:"present" Precheck would instead map a missing
// preset to alreadyPresent → exit 0 + "ok deactivated", a divergence from the
// in-process E_NOT_FOUND. Source is "preset:"+name to stay byte-identical to
// the in-process tombstone, so the two paths produce the same event.
func uninstallPresetViaDaemon(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger, name string, jsonMode, yes bool) error {
	db, closeDB, derr := openSchemaReadDB(ctx, cmd, dataDir, logger)
	if derr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("open schema read db: %w", derr))
	}
	var n int
	qerr := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM presets WHERE name = ?", name).Scan(&n)
	closeDB()
	if qerr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("check preset: %w", qerr))
	}
	if n == 0 {
		return renderPresetNotInstalled(cmd, name, jsonMode)
	}

	if proceed, cerr := confirmPresetUninstall(cmd, name, yes); cerr != nil || !proceed {
		return cerr
	}
	payload, merr := json.Marshal(&ontology.PresetDeactivatedEvent{Name: name, RemovedAt: time.Now().UTC()})
	if merr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("marshal presetDeactivated: %w", merr))
	}
	batch := []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindPresetDeactivated),
		Source:     "preset:" + name,
		Scope:      kernel.ScopePersonal,
		Confidence: 1.0,
		Payload:    payload,
	}}
	// Post-append: verify the row is gone. No precheck — the explicit existence
	// check above already returns the correct exit code for a missing preset,
	// and the dispatcher tombstone is idempotent if a concurrent uninstall raced.
	checks := []matCheck{{
		query:   "SELECT COUNT(1) FROM presets WHERE name = ?",
		args:    []any{name},
		label:   "preset:" + name,
		confirm: "absent",
	}}
	if aerr := appendBatchViaDaemonOrInProcess(ctx, cmd, dataDir, logger, "preset uninstall", batch, nil, checks); aerr != nil {
		return aerr
	}
	return renderPresetUninstallResult(cmd, name, jsonMode)
}

// renderPresetUninstallResult writes the success output for preset uninstall.
// Extracted to share between the daemon-routing and in-process paths.
func renderPresetUninstallResult(cmd *cobra.Command, name string, jsonMode bool) error {
	if jsonMode {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(presetUninstallResult{
			Name:   name,
			Status: "uninstalled",
			Note:   "Entity kinds and predicates remain installed; review them with `maz schema kinds list` / `maz schema predicates`.",
		})
	}
	//nolint:errcheck // best-effort stdout
	fmt.Fprintf(cmd.OutOrStdout(), "Deactivated preset %q.\nIts entity kinds and predicates remain installed; review them with `maz schema kinds list` / `maz schema predicates`.\n", name)
	return nil
}

// isPresetJSONMode reports whether the active output writer is JSON or NDJSON.
// Extracted here because preset commands don't use output.From but still need
// to branch on the format flag to honour the two-mode contract (B6.0).
func isPresetJSONMode(cmd *cobra.Command) bool {
	wr := output.From(cmd)
	return wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
}

func newPresetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active presets",
		Long: `List the presets currently active. Does not print instructions bodies
(use 'maz preset show <name>' for that).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("preset list: accepts no arguments, got %d", len(args))
			}
			return runPresetList(cmd)
		},
	}
	return cmd
}

func runPresetList(cmd *cobra.Command) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	db, closeFn, err := openPresetReadDB(ctx, cmd, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	rows, qerr := db.QueryContext(ctx, "SELECT name, version, description, added_at FROM presets ORDER BY name")
	if qerr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("query presets: %w", qerr))
	}
	defer func() { _ = rows.Close() }()

	type presetListItem struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		InstalledAt string `json:"installedAt"`
	}
	// Non-nil slice so an empty result marshals to `[]`, not `null` — a JSON
	// consumer of a list endpoint expects an array.
	items := []presetListItem{}
	for rows.Next() {
		var it presetListItem
		if serr := rows.Scan(&it.Name, &it.Version, &it.Description, &it.InstalledAt); serr != nil {
			return wrapPresetFatal(cmd, fmt.Errorf("scan preset: %w", serr))
		}
		items = append(items, it)
	}
	if rerr := rows.Err(); rerr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("iterate presets: %w", rerr))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatNDJSON {
		// NDJSON: one compact JSON object per line, not a pretty array.
		for i := range items {
			if err := wr.Emit(items[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if wr.Format() == output.FormatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}
	if len(items) == 0 {
		_, werr := fmt.Fprintln(w, "No presets installed. Install one with `maz preset install <name>`.")
		return werr
	}
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "name"},
		{Title: "version"},
		{Title: "installed at"},
		{Title: "description"},
	})
	for _, it := range items {
		tbl.Row(it.Name, it.Version, it.InstalledAt, it.Description)
	}
	return tbl.Render()
}

func newPresetShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one active preset, including its instructions body",
		Long: `Show an active preset's metadata AND its full instructions body. The body
surfaces only here, to your local terminal — it is never written to a sync
surface.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("preset show: accepts 1 argument, got %d", len(args))
			}
			return runPresetShow(cmd, args[0])
		},
	}
	return cmd
}

func runPresetShow(cmd *cobra.Command, name string) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	db, closeFn, err := openPresetReadDB(ctx, cmd, logger)
	if err != nil {
		return err
	}
	defer closeFn()

	type presetShowResult struct {
		Name         string `json:"name"`
		Version      string `json:"version"`
		Description  string `json:"description"`
		Instructions string `json:"instructions"`
		InstalledAt  string `json:"installedAt"`
		// Provenance for remotely-installed presets (#224); empty for
		// local-directory and builtin installs.
		SourceURL string `json:"sourceUrl,omitempty"`
		PinnedRef string `json:"pinnedRef,omitempty"`
		Checksum  string `json:"checksum,omitempty"`
	}
	var res presetShowResult
	serr := db.QueryRowContext(ctx,
		"SELECT name, version, description, instructions, added_at, source_url, pinned_ref, checksum FROM presets WHERE name = ?", name).
		Scan(&res.Name, &res.Version, &res.Description, &res.Instructions, &res.InstalledAt, &res.SourceURL, &res.PinnedRef, &res.Checksum)
	if errors.Is(serr, sql.ErrNoRows) {
		notInstalled := fmt.Errorf("preset %q is not installed", name)
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "preset show: %v\n", notInstalled)
		wr := output.From(cmd)
		if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
			//nolint:errcheck // best-effort JSON error envelope
			_ = wr.Emit(errorEnvelope{
				Error: notInstalled.Error(),
				Code:  "E_NOT_FOUND",
			})
		}
		return silentError{code: 1, err: notInstalled}
	}
	if serr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("query preset: %w", serr))
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatNDJSON {
		// NDJSON: one compact JSON object per line (no indentation).
		return wr.Emit(res)
	}
	if wr.Format() == output.FormatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	//nolint:errcheck // best-effort tabwriter; flush error is returned below
	fmt.Fprintf(tw, "Name\t%s\n", res.Name)
	//nolint:errcheck // best-effort tabwriter
	fmt.Fprintf(tw, "Version\t%s\n", res.Version)
	//nolint:errcheck // best-effort tabwriter
	fmt.Fprintf(tw, "Description\t%s\n", res.Description)
	//nolint:errcheck // best-effort tabwriter
	fmt.Fprintf(tw, "Installed at\t%s\n", res.InstalledAt)
	// Provenance lines surface only for remotely-installed presets, so
	// local/builtin `preset show` output is unchanged.
	if res.SourceURL != "" {
		//nolint:errcheck // best-effort tabwriter
		fmt.Fprintf(tw, "Source\t%s\n", res.SourceURL)
	}
	if res.PinnedRef != "" {
		//nolint:errcheck // best-effort tabwriter
		fmt.Fprintf(tw, "Pinned ref\t%s\n", res.PinnedRef)
	}
	if res.Checksum != "" {
		//nolint:errcheck // best-effort tabwriter
		fmt.Fprintf(tw, "Checksum\t%s\n", res.Checksum)
	}
	if ferr := tw.Flush(); ferr != nil {
		return ferr
	}
	_, werr := fmt.Fprintf(w, "\nInstructions:\n%s\n", res.Instructions)
	return werr
}

// openPresetReadDB opens an in-process read connection to the presets projection,
// matching the read pathway of `maz schema kinds list`. It refuses if a daemon
// holds the store lock (stop the daemon and retry).
func openPresetReadDB(ctx context.Context, cmd *cobra.Command, logger *zap.Logger) (*sql.DB, func(), error) {
	dataDir := presetDataDir(cmd)
	wired, err := daemon.OpenWiring(ctx, daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "preset: open wiring: %v\n", err)
		return nil, nil, silentError{code: 2, err: err}
	}
	db, derr := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if derr != nil {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("preset: close wiring after open-sqlite failure", zap.Error(cerr))
		}
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "preset: open sqlite: %v\n", derr)
		return nil, nil, silentError{code: 2, err: derr}
	}
	return db, func() {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("preset: close wiring", zap.Error(cerr))
		}
	}, nil
}

// presetDataDir resolves the data-dir flag, defaulting to the configured path.
func presetDataDir(cmd *cobra.Command) string {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	return dataDir
}

// emitPresetInstallError reports an expected preset-install failure: it always
// prints to stderr and, in JSON/NDJSON mode, emits a {error,code} envelope to
// stdout so a piped consumer can parse the failure without scraping stderr.
// Returns silentError{code:1} so the caller can return it directly.
func emitPresetInstallError(cmd *cobra.Command, err error, code string) error {
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(cmd.ErrOrStderr(), "preset install: %v\n", err)
	if wr := output.From(cmd); wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		//nolint:errcheck // best-effort JSON error envelope
		_ = wr.Emit(errorEnvelope{Error: err.Error(), Code: code})
	}
	return silentError{code: 1, err: err}
}

// wrapPresetFatal prints the error to stderr and returns silentError(2).
func wrapPresetFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "preset", err)
}

// renderPresetInstallResult renders the install result as JSON or CLI text.
func renderPresetInstallResult(cmd *cobra.Command, res *presetInstallResult) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	return renderPresetInstallCLI(w, res)
}

func renderPresetInstallCLI(w io.Writer, res *presetInstallResult) error {
	prefix := "Installed"
	if res.DryRun {
		prefix = "Would install"
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "Preset\t%s@%s\n", res.Name, res.Version); err != nil {
		return err
	}
	if res.DryRun {
		if _, err := fmt.Fprintf(tw, "Mode\tdry-run (no events appended)\n"); err != nil {
			return err
		}
	} else if res.InstalledAt != nil {
		if _, err := fmt.Fprintf(tw, "Installed at\t%s\n", res.InstalledAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// writeBulletList no-ops on an empty slice, so optional sections need no guard.
	scopeHeader := "\nGranted scopes:\n"
	if res.DryRun {
		scopeHeader = "\nWould grant scopes:\n"
	}
	for _, sec := range []bulletSection{
		{fmt.Sprintf("\n%s %d entity kind(s):\n", prefix, len(res.KindsRegistered)), res.KindsRegistered},
		{fmt.Sprintf("\n%s %d predicate(s):\n", prefix, len(res.PredicatesRegistered)), res.PredicatesRegistered},
		{fmt.Sprintf("\nSkipped %d entity kind(s) (user-owned; use --force):\n", len(res.KindsSkipped)), res.KindsSkipped},
		{fmt.Sprintf("\nSkipped %d predicate(s) (user-owned; use --force):\n", len(res.PredicatesSkipped)), res.PredicatesSkipped},
		{scopeHeader, res.GrantedScopes},
	} {
		if err := writeBulletList(w, sec.header, sec.items); err != nil {
			return err
		}
	}
	if !res.DryRun {
		if _, err := fmt.Fprintf(w, "\nThe preset is now steering agents via getContext.\n"); err != nil {
			return err
		}
	}
	return nil
}
