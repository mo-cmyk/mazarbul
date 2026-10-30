package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
)

// friendlyTypeName maps a reflect.Kind to a user-facing type description that
// avoids Go-internal names (float32, int64, etc.). Used to build the
// validation error message when config set receives a value of the wrong type.
func friendlyTypeName(k reflect.Kind) string {
	switch k {
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Bool:
		return "boolean"
	default:
		return "string"
	}
}

// providerSentinelNone is the user-facing sentinel for "no provider
// configured" in defaults.chat / defaults.embed. Embed=none disables
// recall; chat=none disables `maz ask`.
const providerSentinelNone = "none"

// configSrcProject labels a config value whose effective source is the
// per-project .mazarbul.toml overlay.
const configSrcProject = "project"

// boolStringTrue and boolStringFalse are the canonical lower-case string
// representations of boolean values parsed by inferType.
const (
	boolStringTrue  = "true"
	boolStringFalse = "false"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and modify Mazarbul configuration",
		Long: `Commands for reading and writing Mazarbul configuration.

Configuration is layered (highest priority first):
  1. CLI flags
  2. MAZ_* environment variables
  3. ./.mazarbul.toml (per-project, opt-in, read-only)
  4. ${DATA_DIR}/config.toml
  5. ${DATA_DIR}/state.toml
  6. ~/.config/mazarbul/config.yaml
  7. Compiled defaults

Recall tuning knobs (mmr_lambda, recency_boost, graph_weight, …) live in the
[recall] section; see "maz help-topics recall" for the full reference.

Examples:
  # Show the effective configuration:
  maz config list

  # Get a single key:
  maz config get defaults.chat

  # Set a key in the book-level config:
  maz config set recall.mmr_lambda 0.7

  # Edit the config file in $EDITOR:
  maz config edit

  # Print the config file path:
  maz config path`,
	}
	cmd.AddCommand(newConfigGetCmd())
	cmd.AddCommand(newConfigPathCmd())
	cmd.AddCommand(newConfigSetCmd())
	cmd.AddCommand(newConfigEditCmd())
	cmd.AddCommand(newConfigListCmd())
	return cmd
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get [key]",
		Short: "Print the resolved configuration (optionally one key)",
		Long: `Print the effective user configuration. When a dotted key is given,
print only that field's value (e.g. defaults.chat, providers.ollama.chat_model).
With no arguments, print the full resolved configuration.

Exit codes:
  0   success
  1   key not found
  2   unexpected failure

Examples:
  maz config get
  maz config get defaults.chat
  maz config get recall.mmr_lambda
  maz config get --output json | jq .`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runConfigGetKey(cmd, args[0])
			}
			return runConfigGet(cmd)
		},
	}
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Long: `Print the absolute path of the active config file (${DATA_DIR}/config.toml).

Examples:
  maz config path
  cat $(maz config path)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir := configDataDir(cmd)
			_, err := fmt.Fprintln(cmd.OutOrStdout(), config.UserConfigPath(dataDir))
			return err
		},
	}
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Long: `Write a single configuration key-value pair to ${DATA_DIR}/config.toml.
The key uses dotted TOML-path notation matching the config file structure.
The write is atomic (temp-file + rename). The config is validated after write.

Supported top-level sections: providers, defaults, recall, composer,
ingestion, ontology, log, sync, vault, watch_fs, output, saved_recalls.

Exit codes:
  0   success
  1   validation error (bad key or value)
  2   unexpected failure (I/O or parse error)

Examples:
  maz config set defaults.chat ollama
  maz config set recall.mmr_lambda 0.7
  maz config set providers.ollama.chat_model qwen2.5:7b
  maz config set sync.auto_push true`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigSet(cmd, args[0], args[1])
		},
	}
}

func newConfigEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open the config file in $EDITOR",
		Long: `Open ${DATA_DIR}/config.toml in the editor specified by the $EDITOR
environment variable (falls back to vi). After saving, the file is
re-validated; if validation fails, the error is printed to stderr.

Exit codes:
  0   success (file saved and validated)
  1   validation error after edit
  2   unexpected failure ($EDITOR not found, I/O error)

Examples:
  maz config edit
  EDITOR=nano maz config edit`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigEdit(cmd)
		},
	}
}

func newConfigListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List effective configuration with source annotations",
		Long: `List the fully-resolved configuration together with the source file
for each key. Sources are shown as:
  default   compiled-in default
  config    ${DATA_DIR}/config.toml
  state     ${DATA_DIR}/state.toml
  project   ./.mazarbul.toml

In JSON mode the output is a bare array [{"key","value","source"}].

Exit codes:
  0   success
  2   unexpected failure

Examples:
  maz config list
  maz config list --sources
  maz config list --output json | jq '.[] | select(.source=="state")'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigList(cmd)
		},
	}
	cmd.Flags().Bool("sources", false, "annotate each key with its source file")
	return cmd
}

// configDataDir resolves the --data-dir flag or falls back to the default.
func configDataDir(cmd *cobra.Command) string {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	return dataDir
}

// runConfigGet prints the full resolved config.
func runConfigGet(cmd *cobra.Command) error {
	dataDir := configDataDir(cmd)
	wd, _ := os.Getwd() //nolint:errcheck // wd="" falls through to no per-project overlay; LoadEffectiveConfig handles it
	cfg, err := config.LoadEffectiveConfig(dataDir, wd)
	if err != nil {
		// silentError suppresses cobra's default error print, so we
		// surface the message ourselves; otherwise the operator sees
		// only exit code 8 with nothing to act on for the same broken
		// file the daemon refuses at boot.
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: %v\n", err)
		return silentError{code: 8, err: err}
	}
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(cfg)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintln(w, string(raw))
	return werr
}

// runConfigGetKey prints the value of a single dotted key.
func runConfigGetKey(cmd *cobra.Command, key string) error {
	dataDir := configDataDir(cmd)
	wd, _ := os.Getwd() //nolint:errcheck // wd="" falls through to no per-project overlay; LoadEffectiveConfig handles it
	cfg, err := config.LoadEffectiveConfig(dataDir, wd)
	if err != nil {
		// Same rationale as runConfigGet: silentError eats cobra's
		// default print, so the parse / schema error has to land on
		// stderr here or the operator sees only exit code 8.
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: %v\n", err)
		return silentError{code: 8, err: err}
	}
	// Render to a snake_case-keyed generic map (TOML namespace) so a dotted key
	// the operator typed (defaults.chat) walks the same key names the config
	// file uses — not the PascalCase Go fields a JSON marshal would expose.
	m, err := configToGenericMap(cfg)
	if err != nil {
		return silentError{code: 2, err: err}
	}

	val, found := walkDotted(m, key)
	if !found {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: key %q not found\n", key)
		return silentError{code: 1, err: fmt.Errorf("key not found: %s", key)}
	}
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{"key": key, "value": val})
	}
	switch v := val.(type) {
	case string:
		_, err = fmt.Fprintln(w, v)
	default:
		var b []byte
		b, err = json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
	}
	return err
}

// walkDotted traverses m by dotted key path. Returns the value and true
// when found.
func walkDotted(m map[string]any, key string) (any, bool) {
	parts := strings.SplitN(key, ".", 2)
	val, ok := m[parts[0]]
	if !ok || len(parts) == 1 {
		return val, ok
	}
	sub, ok := val.(map[string]any)
	if !ok {
		return nil, false
	}
	return walkDotted(sub, parts[1])
}

// configSetItem is one key-value pair for JSON list output.
type configSetItem struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// runConfigList prints the effective config with source annotations.
func runConfigList(cmd *cobra.Command) error { //nolint:gocognit,gocyclo // format-routing function; branches are independent output formats
	dataDir := configDataDir(cmd)

	// Collect sources in precedence order (lowest first).
	defaults := config.Defaults()
	_ = defaults // used for annotating compiled defaults

	cfg, cfgErr := config.LoadUserConfig(dataDir)
	if cfgErr != nil && !errors.Is(cfgErr, config.ErrUserConfigNotFound) {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: %v\n", cfgErr)
		return silentError{code: 2, err: cfgErr}
	}
	state, stateErr := config.LoadUserState(dataDir)
	if stateErr != nil && !errors.Is(stateErr, config.ErrUserStateNotFound) {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: %v\n", stateErr)
		return silentError{code: 2, err: stateErr}
	}
	wd, _ := os.Getwd() //nolint:errcheck // cwd fallback; empty string means no project config
	projectCfg, projectErr := config.LoadProjectConfig(wd)
	projectPresent := projectErr == nil

	// Start from defaults so values that the user never explicitly
	// configured still appear (with source="default") instead of as
	// empty strings. Then overlay config.toml, state.toml, project.
	// The per-layer reads above already surface parse/schema errors,
	// so LoadEffectiveConfig should not error here in practice; on the
	// off chance it does (e.g. a write-after-read between the two
	// calls) we fail the command rather than print stale data.
	resolved, err := config.LoadEffectiveConfig(dataDir, wd)
	if err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: %v\n", err)
		return silentError{code: 8, err: err}
	}
	// Build the layer-attributed view from the per-layer values we
	// already have so the "source" label is accurate.
	_ = cfg
	_ = state
	_ = projectCfg

	// Build flat key list with sources by marshaling each layer and diffing.
	items := buildConfigItems(cfg, state, projectCfg, resolved, cfgErr, stateErr, projectPresent)

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	switch wr.Format() {
	case output.FormatNDJSON:
		// NDJSON: one compact JSON object per line.
		for i := range items {
			if err := wr.EmitStream(items[i]); err != nil {
				return silentError{code: 2, err: err}
			}
		}
		return wr.Flush()
	case output.FormatJSON:
		// Bare array, uniform with `maz list entities/relations/episodes`. A
		// non-nil slice keeps an empty result `[]`, not `null`.
		if items == nil {
			items = []configSetItem{}
		}
		if err := wr.Emit(items); err != nil {
			return silentError{code: 2, err: err}
		}
		return wr.Flush()
	case output.FormatCSV, output.FormatKV:
		// Structured tabular: emit each item as a record.
		// CSV columns are key,value,source (sorted alphabetically by flattenRecord).
		// KV produces one key:value block per item separated by blank lines.
		for i := range items {
			if err := wr.Emit(items[i]); err != nil {
				return silentError{code: 2, err: err}
			}
		}
		return wr.Flush()
	default:
		// FormatText: fall through to the human-readable table below.
	}

	showSources, _ := cmd.Flags().GetBool("sources")
	for _, item := range items {
		if showSources {
			//nolint:errcheck // best-effort stdout write
			fmt.Fprintf(w, "%-40s = %-20v  [%s]\n", item.Key, item.Value, item.Source)
		} else {
			//nolint:errcheck // best-effort stdout write
			fmt.Fprintf(w, "%-40s = %v\n", item.Key, item.Value)
		}
	}
	return nil
}

// buildConfigItems produces the flat annotated key list. We marshal the
// resolved config, then walk it to produce items with source labels.
//
//nolint:gocritic // hugeParam: UserConfig structs are value types shared across callers; pointer would widen the API surface
func buildConfigItems(cfg config.UserConfig, state config.UserState, projectCfg config.UserConfig,
	resolved config.UserConfig, cfgErr, stateErr error, projectPresent bool,
) []configSetItem {
	// Marshal resolved to generic map.
	raw, _ := json.Marshal(resolved) //nolint:errcheck // in-memory marshal of known-good type
	var m map[string]any
	_ = json.Unmarshal(raw, &m) //nolint:errcheck // raw is our own marshal output

	var items []configSetItem
	flattenMap("", m, func(key string, val any) {
		src := "default"
		if cfgErr == nil {
			if valueExistsInConfig(cfg, key, val) {
				src = "config"
			}
		}
		if stateErr == nil {
			if valueExistsInState(state, key, val) {
				src = "state"
			}
		}
		if projectPresent {
			if valueExistsInProjectConfig(projectCfg, key, val) {
				src = configSrcProject
			}
		}
		items = append(items, configSetItem{Key: key, Value: val, Source: src})
	})
	return items
}

// flattenMap recursively walks a generic map and calls fn for each leaf.
func flattenMap(prefix string, m map[string]any, fn func(key string, val any)) {
	for k, v := range m {
		fullKey := k
		if prefix != "" {
			fullKey = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			flattenMap(fullKey, sub, fn)
		} else {
			fn(fullKey, v)
		}
	}
}

// valueExistsInConfig checks if a given key's value comes from the config file.
//
//nolint:gocritic // hugeParam: UserConfig is a value type; pointer adds allocation with no semantic benefit here
func valueExistsInConfig(cfg config.UserConfig, key string, val any) bool {
	raw, _ := json.Marshal(cfg) //nolint:errcheck // in-memory marshal of known-good type
	var m map[string]any
	_ = json.Unmarshal(raw, &m) //nolint:errcheck // raw is our own marshal output
	found, ok := walkDotted(m, key)
	if !ok {
		return false
	}
	return jsonEqual(found, val)
}

// valueExistsInState checks if a value comes from state.toml.
//
//nolint:gocritic // hugeParam: UserState is a value type; pointer adds allocation with no semantic benefit here
func valueExistsInState(state config.UserState, key string, val any) bool {
	raw, _ := json.Marshal(state) //nolint:errcheck // in-memory marshal of known-good type
	var m map[string]any
	_ = json.Unmarshal(raw, &m) //nolint:errcheck // raw is our own marshal output
	found, ok := walkDotted(m, key)
	if !ok {
		return false
	}
	return jsonEqual(found, val)
}

// valueExistsInProjectConfig checks if a value comes from .mazarbul.toml.
//
//nolint:gocritic // hugeParam: UserConfig is a value type; pointer adds allocation with no semantic benefit here
func valueExistsInProjectConfig(cfg config.UserConfig, key string, val any) bool {
	raw, _ := json.Marshal(cfg) //nolint:errcheck // in-memory marshal of known-good type
	var m map[string]any
	_ = json.Unmarshal(raw, &m) //nolint:errcheck // raw is our own marshal output
	found, ok := walkDotted(m, key)
	if !ok {
		return false
	}
	return jsonEqual(found, val)
}

// jsonEqual reports equality via JSON encoding (handles interface{} numeric
// equality which json.Marshal normalizes).
func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a) //nolint:errcheck // in-memory marshal of known-good type
	bb, _ := json.Marshal(b) //nolint:errcheck // in-memory marshal of known-good type
	return bytes.Equal(ab, bb)
}

// runConfigSet writes a single key to config.toml atomically.
func runConfigSet(cmd *cobra.Command, key, value string) error {
	dataDir := configDataDir(cmd)
	cfgPath := config.UserConfigPath(dataDir)

	// Reject managed/reserved keys before any other validation. These keys
	// are written by maz internals (migrations, daemon) and must not be set
	// directly. Writing schema_version, for example, would brick the config:
	// every subsequent command would fail "schema_version=2, supports up to 1".
	if isManagedConfigKey(key) {
		err := fmt.Errorf("config: %s is managed by `maz migrate` and cannot be set directly", key)
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error()) //nolint:errcheck // best-effort stderr write
		return silentError{code: 1, err: err}
	}

	// Reject unknown dotted keys upfront. Without this, `maz config
	// set defaults.chatt ollama` would walk the JSON map, write
	// `defaults.chatt` into the generic map, then silently drop it on
	// the UserConfig round-trip (Go's json unmarshal ignores unknown
	// fields). The command would print "set …" and exit 0 despite
	// not having changed anything the daemon honours.
	if !isKnownConfigKey(key) {
		err := fmt.Errorf("config: unknown key %q (run `maz config list` to see current keys)", key)
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error()) //nolint:errcheck // best-effort stderr write
		return silentError{code: 1, err: err}
	}

	// Load existing config (or start fresh).
	cfg, err := config.LoadUserConfig(dataDir)
	if err != nil && !errors.Is(err, config.ErrUserConfigNotFound) {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: load: %v\n", err)
		return silentError{code: 2, err: err}
	}

	// Render to a snake_case-keyed generic map (TOML namespace), apply the
	// dotted key, and round-trip back into the typed config. TOML — not JSON —
	// so the dotted key the operator typed (defaults.chat) matches the map keys.
	m, err := configToGenericMap(cfg)
	if err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: %v\n", err)
		return silentError{code: 2, err: err}
	}

	// Resolve the value to the destination field's Go type. A wrong-typed value
	// is rejected here (exit 1, naming the dotted key) rather than leaking the
	// TOML decoder's internals on exit 2.
	leaf, verr := resolveConfigLeaf(key, value)
	if verr != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintln(cmd.ErrOrStderr(), verr.Error())
		return silentError{code: 1, err: verr}
	}
	if err := setDotted(m, key, leaf); err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config set: %v\n", err)
		return silentError{code: 1, err: err}
	}

	updated, err := genericMapToConfig(m)
	if err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: re-parse: %v\n", err)
		return silentError{code: 2, err: err}
	}
	if updated.SchemaVersion == 0 {
		updated.SchemaVersion = config.CurrentUserConfigSchemaVersion
	}

	// Enum validation for keys whose legal values are a closed set.
	// Done after unmarshal so the string is normalised and the type
	// check above has already fired for obviously-wrong types.
	if err := validateConfigSetEnum(key, value); err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentError{code: 1, err: err}
	}

	if err := config.WriteTOMLFileAtomic(cfgPath, updated); err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: write: %v\n", err)
		return silentError{code: 2, err: err}
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{"ok": true, "key": key, "value": value})
	}
	//nolint:errcheck // best-effort stdout write
	fmt.Fprintf(w, "set %s = %s\n", key, value)
	return nil
}

// validateUserConfigSemantics performs the semantic checks the TOML
// parser doesn't: defaults.chat / defaults.embed must reference a
// supported provider, every referenced provider must have a
// matching [providers.<name>] block, and the schema version must be
// the one this build understands. Mirrors what the daemon's boot
// path checks so a `maz config edit` save is rejected before the
// next daemon start runs into the same mismatch.
//
//nolint:gocritic // hugeParam: UserConfig copies are cheap; pointer adds nil-check ceremony for no benefit
func validateUserConfigSemantics(cfg config.UserConfig) error {
	if cfg.SchemaVersion != 0 && cfg.SchemaVersion > config.CurrentUserConfigSchemaVersion {
		return fmt.Errorf("schema_version=%d unsupported (this build understands up to %d)",
			cfg.SchemaVersion, config.CurrentUserConfigSchemaVersion)
	}
	known := map[string]bool{"anthropic": true, "ollama": true, providerSentinelNone: true}
	if cfg.Defaults.Chat != "" && !known[cfg.Defaults.Chat] {
		return fmt.Errorf("defaults.chat=%q is not a supported provider", cfg.Defaults.Chat)
	}
	if cfg.Defaults.Embed != "" && !known[cfg.Defaults.Embed] {
		return fmt.Errorf("defaults.embed=%q is not a supported provider", cfg.Defaults.Embed)
	}
	for slot, name := range map[string]string{"chat": cfg.Defaults.Chat, "embed": cfg.Defaults.Embed} {
		if name == "" || name == providerSentinelNone {
			continue
		}
		if _, ok := cfg.Providers[name]; !ok {
			return fmt.Errorf("defaults.%s=%q but [providers.%s] is missing", slot, name, name)
		}
	}
	return nil
}

// isManagedConfigKey reports whether key is reserved for internal use by maz
// (migrations, daemon boot) and must not be written directly by users via
// `maz config set`. Managed keys are still valid schema keys; they are simply
// controlled by maz tooling rather than the operator.
func isManagedConfigKey(key string) bool {
	switch key {
	case "schema_version":
		return true
	default:
		return false
	}
}

// isKnownConfigKey reports whether key is a dotted TOML-path — the snake_case
// form `maz config set --help` advertises and config.toml uses — that names a
// real UserConfig field. It walks the struct by `toml` tag, so the accepted key
// universe is exactly the config file's, not the PascalCase Go-field names a
// JSON round-trip exposes (the mismatch that used to reject every documented
// key, e.g. defaults.chat / providers.ollama.chat_model). A map-valued field
// (providers.<name>, sync.remotes.<scope>) accepts a free operator-named child
// key and resolves any remaining path against the map's value struct, so a typo
// PAST the map level (providers.ollama.bogus) is still caught.
func isKnownConfigKey(key string) bool {
	if key == "" {
		return false
	}
	t := reflect.TypeOf(config.UserConfig{})
	for _, part := range strings.Split(key, ".") {
		t = derefConfigType(t)
		switch t.Kind() {
		case reflect.Struct:
			ft, ok := configFieldByTOMLTag(t, part)
			if !ok {
				return false
			}
			t = ft
		case reflect.Map:
			// A free, operator-named map key (provider/scope name); the rest of
			// the path resolves against the map's value type.
			t = t.Elem()
		default:
			// A scalar/slice leaf has no child keys, so a deeper path is a typo.
			return false
		}
	}
	return true
}

// derefConfigType unwraps pointer types so a *float64 recall knob (e.g.
// recall.graph_weight) resolves to its underlying kind during the key walk.
func derefConfigType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// configFieldByTOMLTag returns the type of t's field whose `toml` tag (sans
// options like ",omitempty") equals tag. A field without a toml tag falls back
// to its Go name so the walk never silently skips one.
func configFieldByTOMLTag(t reflect.Type, tag string) (reflect.Type, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if name == "" {
			name = f.Name
		}
		if name == tag {
			return f.Type, true
		}
	}
	return nil, false
}

// configToGenericMap renders cfg as a snake_case-keyed generic map via TOML, so
// dotted-key reads/writes operate on the same key namespace config.toml and
// `--help` use. JSON marshaling would key the map by PascalCase Go field names —
// the mismatch that made documented keys unsettable.
//
//nolint:gocritic // hugeParam: UserConfig copies are cheap here; pointer adds nil-check ceremony for no benefit
func configToGenericMap(cfg config.UserConfig) (map[string]any, error) {
	raw, err := toml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode config to toml: %w", err)
	}
	var m map[string]any
	if err := toml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode config toml to map: %w", err)
	}
	return m, nil
}

// genericMapToConfig is the inverse of configToGenericMap: a snake_case-keyed
// generic map back into a typed UserConfig via TOML.
func genericMapToConfig(m map[string]any) (config.UserConfig, error) {
	raw, err := toml.Marshal(m)
	if err != nil {
		return config.UserConfig{}, fmt.Errorf("encode config map to toml: %w", err)
	}
	var cfg config.UserConfig
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return config.UserConfig{}, fmt.Errorf("decode config toml: %w", err)
	}
	return cfg, nil
}

// validateConfigSetEnum checks keys whose legal values form a closed set and
// returns a user-actionable error when value is not in that set. Called after
// the type pre-check normalises obvious mismatches; only string-typed enum
// fields need coverage here.
func validateConfigSetEnum(key, value string) error {
	if key == "recall.fusion_mode" {
		if value != "rrf" && value != "weighted" {
			return fmt.Errorf("config: invalid value %q for recall.fusion_mode; expected one of: rrf, weighted", value)
		}
	}
	return nil
}

// configLeafKind walks the UserConfig struct by toml tag (mirroring
// isKnownConfigKey) and returns the deref'd reflect.Kind of the leaf field the
// dotted key names, so config set can reject a wrong-typed value before the
// TOML round-trip. Ok is false when key does not resolve to a leaf.
func configLeafKind(key string) (reflect.Kind, bool) {
	if key == "" {
		return reflect.Invalid, false
	}
	t := reflect.TypeOf(config.UserConfig{})
	for _, part := range strings.Split(key, ".") {
		t = derefConfigType(t)
		switch t.Kind() {
		case reflect.Struct:
			ft, ok := configFieldByTOMLTag(t, part)
			if !ok {
				return reflect.Invalid, false
			}
			t = ft
		case reflect.Map:
			// A free, operator-named map key; resolve the rest against the value type.
			t = t.Elem()
		default:
			return reflect.Invalid, false
		}
	}
	return derefConfigType(t).Kind(), true
}

// valueMatchesKind reports whether the operator-supplied string can populate a
// field of the given kind. Numeric and boolean leaves must parse; every other
// kind (string, slice, map, struct) accepts the raw value and defers to TOML.
func valueMatchesKind(value string, kind reflect.Kind) bool {
	switch kind {
	case reflect.Bool:
		_, err := strconv.ParseBool(strings.ToLower(value))
		return err == nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, err := strconv.ParseInt(value, 10, 64)
		return err == nil
	case reflect.Float32, reflect.Float64:
		// Reject NaN/±Inf: strconv.ParseFloat accepts them, but they are not
		// meaningful config values and would otherwise reach the TOML round-trip.
		f, err := strconv.ParseFloat(value, 64)
		return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	default:
		return true
	}
}

// resolveConfigLeaf types the operator-supplied string for the field key names.
// When the key resolves to a known leaf it validates the value against that
// field's kind (returning an actionable error on mismatch) and coerces it to
// the matching Go type; for an unknown leaf it falls back to inferType's guess.
func resolveConfigLeaf(key, value string) (any, error) {
	kind, ok := configLeafKind(key)
	if !ok {
		return inferType(value), nil
	}
	if !valueMatchesKind(value, kind) {
		return nil, fmt.Errorf("config: %s expects a %s, got %q", key, friendlyTypeName(kind), value)
	}
	return coerceConfigValue(value, kind), nil
}

// coerceConfigValue converts the operator-supplied string to the Go type the
// destination field's kind expects, so the strictly-typed TOML round-trip
// accepts it. ValueMatchesKind has already vetted parseability for numeric and
// boolean kinds; any other kind (and any unexpected parse miss) falls back to
// inferType's best guess.
func coerceConfigValue(value string, kind reflect.Kind) any {
	switch kind {
	case reflect.Bool:
		if b, err := strconv.ParseBool(strings.ToLower(value)); err == nil {
			return b
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if i, err := strconv.ParseInt(value, 10, 64); err == nil {
			return i
		}
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	case reflect.String:
		// String leaves are stored verbatim. Routing them through inferType
		// would silently coerce values like "1e5" or "true" into a number or
		// bool before the TOML round-trip; a string field must keep its string.
		return value
	default:
		// Slice / map / struct leaves keep inferType's guess below.
	}
	return inferType(value)
}

// setDotted sets key (dotted path) to a pre-resolved leaf value in a generic
// map. The caller is responsible for typing leaf (see coerceConfigValue /
// inferType). Intermediate maps are created as needed.
func setDotted(m map[string]any, key string, leaf any) error {
	parts := strings.SplitN(key, ".", 2)
	if len(parts) == 1 {
		m[key] = leaf
		return nil
	}
	sub, ok := m[parts[0]]
	if !ok || sub == nil {
		sub = map[string]any{}
	}
	subMap, ok := sub.(map[string]any)
	if !ok {
		return fmt.Errorf("key %q is not a map", parts[0])
	}
	if err := setDotted(subMap, parts[1], leaf); err != nil {
		return err
	}
	m[parts[0]] = subMap
	return nil
}

// inferType converts a string flag value to the most appropriate Go type.
// Booleans are recognised first, then numeric values via strconv.ParseFloat
// (which handles trailing zeros like "0.0", "1.0", "0.50" correctly).
// Anything else is returned as a plain string.
func inferType(s string) any {
	switch strings.ToLower(s) {
	case boolStringTrue:
		return true
	case boolStringFalse:
		return false
	}
	// strconv.ParseFloat correctly parses trailing-zero forms ("0.0", "1.0",
	// "0.50") that the previous fmt.Sscanf + round-trip check rejected.
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// runConfigEdit opens the config file in $EDITOR (falls back to vi).
func runConfigEdit(cmd *cobra.Command) error {
	dataDir := configDataDir(cmd)
	cfgPath := config.UserConfigPath(dataDir)

	// Ensure the file exists (create minimal one if absent).
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		blank := config.UserConfig{SchemaVersion: config.CurrentUserConfigSchemaVersion}
		if werr := config.WriteTOMLFileAtomic(cfgPath, blank); werr != nil {
			//nolint:errcheck // best-effort stderr write on error path
			fmt.Fprintf(cmd.ErrOrStderr(), "config: create: %v\n", werr)
			return silentError{code: 2, err: werr}
		}
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	editorCmd := exec.CommandContext(cmd.Context(), editor, cfgPath)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr

	if err := editorCmd.Run(); err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: editor: %v\n", err)
		return silentError{code: 2, err: err}
	}

	// Re-validate after save. We previously only checked that the
	// file parsed as TOML, which let semantically invalid configs
	// (unknown provider, bogus default values, schema-version
	// mismatch) through. Reuse the canonical loader so a save is
	// rejected with the same error the daemon would see at boot.
	if _, err := config.LoadUserConfig(dataDir); err != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: validation failed: %v\n", err)
		return silentError{code: 8, err: err}
	}
	cfg, err := config.LoadUserConfig(dataDir)
	if err != nil {
		return silentError{code: 8, err: err}
	}
	if verr := validateUserConfigSemantics(cfg); verr != nil {
		//nolint:errcheck // best-effort stderr write on error path
		fmt.Fprintf(cmd.ErrOrStderr(), "config: validation failed: %v\n", verr)
		return silentError{code: 8, err: verr}
	}
	//nolint:errcheck // best-effort stdout write
	fmt.Fprintln(cmd.OutOrStdout(), "config saved and validated")
	return nil
}
