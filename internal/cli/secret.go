package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// secretMarkerMissing is the presence label printed when a secret is absent.
const secretMarkerMissing = "missing"

// knownSecretNames is the catalog the `maz secret list` subcommand iterates
// to probe presence. New canonical secret names get added here and to the
// constants block in internal/secret/types.go in the same change so the CLI
// can list them without enumerating the OS keychain.
//
//nolint:gochecknoglobals // small static catalog, mirrors the secret.Name* constants
var knownSecretNames = []string{
	secret.NameAnthropicAPIKey,
	secret.NameOpenAIAPIKey,
}

// secretStore is the small subset of secret.Chain the CLI subcommands use.
// Defined as an interface so tests can inject an in-memory implementation
// instead of touching the OS keychain.
type secretStore interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name string, value string) error
	Delete(ctx context.Context, name string) error
}

// newSecretChain returns the canonical env -> keychain provider chain used
// by every secret subcommand. Defined as a package-level var so tests
// override it via a t.Cleanup-protected swap; production callers should
// never reach for it directly.
//
//nolint:gochecknoglobals // test-injection seam; pattern mirrors anthropicKeyAvailableFn in init.go
var newSecretChain = func() secretStore {
	return secret.DefaultChain()
}

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage stored API keys and other secrets",
		Long: `Set, retrieve, list, or delete secrets used by Mazarbul providers.

Secrets are stored in the OS keychain (macOS Keychain, Linux Secret Service,
Windows Credential Manager) by default. The environment-variable layer
(MAZ_SECRET_* or legacy ANTHROPIC_API_KEY / OPENAI_API_KEY) wins on read for
CI scenarios.

Canonical names are dotted lowercase, e.g. anthropic.api_key, openai.api_key.`,
	}

	cmd.AddCommand(newSecretSetCmd())
	cmd.AddCommand(newSecretGetCmd())
	cmd.AddCommand(newSecretListCmd())
	cmd.AddCommand(newSecretDeleteCmd())

	return cmd
}

// ---- set -------------------------------------------------------------------

type secretSetFlags struct {
	value string
	stdin bool
}

func newSecretSetCmd() *cobra.Command {
	flags := &secretSetFlags{}
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret",
		Long: `Store a secret value under the canonical name. The value is sourced
from --value, from stdin (when --stdin is set or stdin is piped), or from
an interactive password prompt (TTY without --value or --stdin). Never
echoes the value to the terminal.

Examples:
  # interactive prompt
  maz secret set anthropic.api_key
  # piped
  echo "sk-ant-..." | maz secret set anthropic.api_key --stdin
  # explicit value (avoid in scripts)
  maz secret set anthropic.api_key --value "sk-ant-..."`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretSet(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.value, "value", "", "secret value (avoid in scripts; prefer --stdin)")
	cmd.Flags().BoolVar(&flags.stdin, "stdin", false, "read value from stdin (trailing newline trimmed)")
	return cmd
}

func runSecretSet(cmd *cobra.Command, name string, flags *secretSetFlags) error {
	value, err := readSecretValue(cmd, flags)
	if err != nil {
		return wrapSecretFatal(cmd, err)
	}
	if value == "" {
		if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), "secret: empty value rejected"); werr != nil {
			return werr
		}
		return silentError{code: 1, err: errors.New("empty value")}
	}

	if err := newSecretChain().Set(cmd.Context(), name, value); err != nil {
		return wrapSecretFatal(cmd, fmt.Errorf("set %s: %w", name, err))
	}
	return emitSecretResult(cmd, "set", name, "")
}

// readSecretValue dispatches between --value, --stdin/piped, and an
// interactive TTY prompt that does not echo the secret.
func readSecretValue(cmd *cobra.Command, flags *secretSetFlags) (string, error) {
	if flags.value != "" {
		return flags.value, nil
	}
	stdin, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		stdin = os.Stdin
	}
	stdinIsTTY := term.IsTerminal(int(stdin.Fd()))
	if flags.stdin || !stdinIsTTY {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if _, werr := fmt.Fprint(cmd.ErrOrStderr(), "value: "); werr != nil {
		return "", werr
	}
	b, err := term.ReadPassword(int(stdin.Fd()))
	if _, werr := fmt.Fprintln(cmd.ErrOrStderr()); werr != nil {
		return "", werr
	}
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
}

// ---- get -------------------------------------------------------------------

type secretGetFlags struct {
	reveal bool
}

func newSecretGetCmd() *cobra.Command {
	flags := &secretGetFlags{}
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Retrieve a secret",
		Long: `Print the secret value for name. By default the value is masked
(first 4 + last 4 characters). Pass --reveal to print the raw value; the
command refuses --reveal when stdout is a TTY so the secret does not flash
on screen. Redirect or pipe stdout to use --reveal.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretGet(cmd, args[0], flags)
		},
	}
	cmd.Flags().BoolVar(&flags.reveal, "reveal", false, "print raw value (requires non-TTY stdout)")
	return cmd
}

func runSecretGet(cmd *cobra.Command, name string, flags *secretGetFlags) error {
	value, err := newSecretChain().Get(cmd.Context(), name)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "secret: %s not found\n", name); werr != nil {
				return werr
			}
			return silentError{code: 1, err: err}
		}
		return wrapSecretFatal(cmd, fmt.Errorf("get %s: %w", name, err))
	}

	out := maskSecret(value)
	if flags.reveal {
		if stdoutIsTTY(cmd) {
			if _, werr := fmt.Fprintln(cmd.ErrOrStderr(),
				"secret: --reveal refused on a TTY; redirect or pipe stdout"); werr != nil {
				return werr
			}
			return silentError{code: 1, err: errors.New("--reveal on tty")}
		}
		out = value
	}
	return emitSecretResult(cmd, "get", name, out)
}

// stdoutIsTTY reports whether cmd's stdout is connected to a terminal. Tests
// route output through a buffer; the type assertion fails and the function
// returns false so --reveal works inside `_test.go`.
func stdoutIsTTY(cmd *cobra.Command) bool {
	f, ok := cmd.OutOrStdout().(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// maskSecret renders value as the first 4 + an ellipsis + the last 4
// characters. Values shorter than 12 characters collapse to "***" so the
// mask never reveals more characters than it hides (`keep` chars on each
// end with at least `keep` chars between them). The output is safe to log.
func maskSecret(value string) string {
	const keep = 4
	if len(value) < keep*3 {
		return "***"
	}
	return value[:keep] + "...***..." + value[len(value)-keep:]
}

// ---- list ------------------------------------------------------------------

func newSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List canonical secret names and presence",
		Long: `Iterate the canonical secret names and report which ones have a stored
value. Never prints the values themselves; the list view is safe to copy
into bug reports.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretList(cmd)
		},
	}
}

type secretListEntry struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
}

func runSecretList(cmd *cobra.Command) error {
	chain := newSecretChain()
	entries := make([]secretListEntry, 0, len(knownSecretNames))
	for _, name := range knownSecretNames {
		_, err := chain.Get(cmd.Context(), name)
		present := err == nil
		if err != nil && !errors.Is(err, secret.ErrNotFound) {
			return wrapSecretFatal(cmd, fmt.Errorf("probe %s: %w", name, err))
		}
		entries = append(entries, secretListEntry{Name: name, Present: present})
	}

	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(entries)
	}
	for _, e := range entries {
		marker := secretMarkerMissing
		if e.Present {
			marker = "present"
		}
		if _, werr := fmt.Fprintf(w, "%-24s %s\n", e.Name, marker); werr != nil {
			return werr
		}
	}
	return nil
}

// ---- delete ----------------------------------------------------------------

func newSecretDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a stored secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretDelete(cmd, args[0])
		},
	}
}

func runSecretDelete(cmd *cobra.Command, name string) error {
	err := newSecretChain().Delete(cmd.Context(), name)
	switch {
	case errors.Is(err, secret.ErrNotFound):
		if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "secret: %s not found\n", name); werr != nil {
			return werr
		}
		return silentError{code: 1, err: err}
	case err != nil:
		return wrapSecretFatal(cmd, fmt.Errorf("delete %s: %w", name, err))
	}
	return emitSecretResult(cmd, "delete", name, "")
}

// ---- shared helpers --------------------------------------------------------

func wrapSecretFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "secret", err)
}

// emitSecretResult writes the confirmation line for set/get/delete. In JSON
// mode the output is an object with action, name, and (for get) value;
// otherwise either the masked/raw value (get) or a one-line confirmation.
func emitSecretResult(cmd *cobra.Command, action, name, value string) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		out := map[string]string{"action": action, "name": name}
		if value != "" {
			out["value"] = value
		}
		return json.NewEncoder(w).Encode(out)
	}
	if value != "" {
		if _, werr := fmt.Fprintln(w, value); werr != nil {
			return werr
		}
		return nil
	}
	if _, werr := fmt.Fprintf(w, "secret %s %s\n", action, name); werr != nil {
		return werr
	}
	return nil
}
