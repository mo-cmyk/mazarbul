package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// fakeSecretStore is an in-memory secretStore for tests. It mimics the
// env-then-keychain chain semantics enough for the CLI subcommand tests:
// Get/Set/Delete operate on the same map, ErrNotFound is returned for
// missing keys, Set is always allowed.
type fakeSecretStore struct {
	data map[string]string
	err  error // injected error returned by every method
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{data: map[string]string{}}
}

func (f *fakeSecretStore) Get(_ context.Context, name string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.data[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}

func (f *fakeSecretStore) Set(_ context.Context, name string, value string) error {
	if f.err != nil {
		return f.err
	}
	f.data[name] = value
	return nil
}

func (f *fakeSecretStore) Delete(_ context.Context, name string) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.data[name]; !ok {
		return secret.ErrNotFound
	}
	delete(f.data, name)
	return nil
}

// installFakeSecretStore swaps newSecretChain for the test duration and
// returns the fake so the test can poke at the underlying map.
func installFakeSecretStore(t *testing.T) *fakeSecretStore {
	t.Helper()
	fake := newFakeSecretStore()
	prev := newSecretChain
	t.Cleanup(func() { newSecretChain = prev })
	newSecretChain = func() secretStore { return fake }
	return fake
}

// newSecretTestRoot builds a root cobra command that has the persistent
// flags needed for output.From(cmd) resolution (--output, --json).
func newSecretTestRoot(secret *cobra.Command, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.AddCommand(secret)
	if outBuf != nil {
		root.SetOut(outBuf)
		secret.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		secret.SetErr(errBuf)
	}
	return root
}

func runSecretCmdWithOutput(t *testing.T, outputFlag string, args ...string) (string, string, error) {
	t.Helper()
	secretCmd := newSecretCmd()
	var outBuf, errBuf bytes.Buffer
	root := newSecretTestRoot(secretCmd, &outBuf, &errBuf)
	if outputFlag != "" {
		allArgs := append([]string{"secret", "--output", outputFlag}, args...)
		root.SetArgs(allArgs)
	} else {
		root.SetArgs(append([]string{"secret"}, args...))
	}
	err := root.ExecuteContext(t.Context())
	return outBuf.String(), errBuf.String(), err
}

// runSecretSubCmd invokes a subcommand via a minimal root that registers
// --output as "text" so output.From(cmd) does not fall back to JSON on the
// non-TTY bytes.Buffer stdout.
func runSecretSubCmd(t *testing.T, subArgs ...string) (string, string, error) {
	t.Helper()
	secretCmd := newSecretCmd()
	var outBuf, errBuf bytes.Buffer
	// Wrap in a root with --output so the subcommand inherits the flag.
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("output", "text", "output format")
	root.AddCommand(secretCmd)
	root.SetOut(&outBuf)
	secretCmd.SetOut(&outBuf)
	root.SetErr(&errBuf)
	secretCmd.SetErr(&errBuf)
	root.SetArgs(append([]string{"secret"}, subArgs...))
	err := root.ExecuteContext(t.Context())
	return outBuf.String(), errBuf.String(), err
}

// ---- mask ------------------------------------------------------------------

func TestMaskSecret(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"sk-ant-1234567890abcdef", "sk-a...***...cdef"},
		{"shorter", "***"},
		{"", "***"},
		{"12345678901", "***"}, // 11 chars: below the 12-char threshold collapses to ***
		{"123456789012", "1234...***...9012"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			if got := maskSecret(c.in); got != c.want {
				t.Errorf("maskSecret(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ---- set -------------------------------------------------------------------

func TestSecretSet_StoresValue(t *testing.T) {
	fake := installFakeSecretStore(t)

	_, _, err := runSecretSubCmd(t, "set", "anthropic.api_key", "--value", "sk-ant-secret-1")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := fake.data["anthropic.api_key"]; got != "sk-ant-secret-1" {
		t.Errorf("stored = %q, want %q", got, "sk-ant-secret-1")
	}
}

func TestSecretSet_RejectsEmptyValue(t *testing.T) {
	installFakeSecretStore(t)

	_, stderr, err := runSecretSubCmd(t, "set", "anthropic.api_key", "--value", "")
	if err == nil {
		t.Fatal("set with empty --value: expected error, got nil")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("expected silentError code=1, got %T %v", err, err)
	}
	if !strings.Contains(stderr, "empty value rejected") {
		t.Errorf("stderr missing rejection message: %q", stderr)
	}
}

// ---- get -------------------------------------------------------------------

func TestSecretGet_DefaultIsMasked(t *testing.T) {
	fake := installFakeSecretStore(t)
	fake.data[secret.NameAnthropicAPIKey] = "sk-ant-1234567890abcdef"

	stdout, _, err := runSecretSubCmd(t, "get", secret.NameAnthropicAPIKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := strings.TrimSpace(stdout)
	if got == "sk-ant-1234567890abcdef" {
		t.Error("get without --reveal leaked the raw secret to stdout")
	}
	if !strings.Contains(got, "...***...") {
		t.Errorf("masked output = %q, expected the *** sentinel", got)
	}
}

func TestSecretGet_RevealOnNonTTYPrintsRaw(t *testing.T) {
	fake := installFakeSecretStore(t)
	fake.data[secret.NameAnthropicAPIKey] = "sk-ant-1234567890abcdef"

	// runSecretSubCmd routes stdout through bytes.Buffer (not a TTY), so
	// --reveal must succeed and print the raw value.
	stdout, _, err := runSecretSubCmd(t, "get", secret.NameAnthropicAPIKey, "--reveal")
	if err != nil {
		t.Fatalf("get --reveal: %v", err)
	}
	if strings.TrimSpace(stdout) != "sk-ant-1234567890abcdef" {
		t.Errorf("--reveal stdout = %q, want raw secret", strings.TrimSpace(stdout))
	}
}

func TestSecretGet_NotFoundIsExit1(t *testing.T) {
	installFakeSecretStore(t)

	_, stderr, err := runSecretSubCmd(t, "get", "anthropic.api_key")
	if err == nil {
		t.Fatal("get on missing secret: expected error, got nil")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("expected silentError code=1, got %T %v", err, err)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr missing not-found message: %q", stderr)
	}
}

// ---- list ------------------------------------------------------------------

func TestSecretList_TextOutput(t *testing.T) {
	fake := installFakeSecretStore(t)
	fake.data[secret.NameAnthropicAPIKey] = "sk-ant-x"

	// Explicitly request text output so the non-TTY stdout doesn't
	// trigger automatic JSON mode.
	stdout, _, err := runSecretCmdWithOutput(t, "text", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "anthropic.api_key") || !strings.Contains(stdout, "present") {
		t.Errorf("expected anthropic.api_key=present in output: %q", stdout)
	}
	if !strings.Contains(stdout, "openai.api_key") || !strings.Contains(stdout, "missing") {
		t.Errorf("expected openai.api_key=missing in output: %q", stdout)
	}
}

func TestSecretList_JSONOutput(t *testing.T) {
	fake := installFakeSecretStore(t)
	fake.data[secret.NameAnthropicAPIKey] = "sk-ant-x"

	// Explicitly request JSON output via --output flag.
	stdout, _, err := runSecretCmdWithOutput(t, "json", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []secretListEntry
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode json: %v (stdout=%q)", err, stdout)
	}
	if len(got) != len(knownSecretNames) {
		t.Fatalf("got %d entries, want %d", len(got), len(knownSecretNames))
	}
	var anthropicEntry *secretListEntry
	for i, e := range got {
		if e.Name == secret.NameAnthropicAPIKey {
			anthropicEntry = &got[i]
			break
		}
	}
	if anthropicEntry == nil {
		t.Fatal("anthropic.api_key entry missing from JSON list")
	}
	if !anthropicEntry.Present {
		t.Error("anthropic.api_key.present = false, want true")
	}
}

// ---- delete ----------------------------------------------------------------

func TestSecretDelete_RemovesValue(t *testing.T) {
	fake := installFakeSecretStore(t)
	fake.data[secret.NameAnthropicAPIKey] = "sk-ant-x"

	_, _, err := runSecretSubCmd(t, "delete", secret.NameAnthropicAPIKey)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := fake.data[secret.NameAnthropicAPIKey]; ok {
		t.Error("entry survived delete")
	}
}

func TestSecretDelete_MissingIsExit1(t *testing.T) {
	installFakeSecretStore(t)

	_, stderr, err := runSecretSubCmd(t, "delete", secret.NameAnthropicAPIKey)
	if err == nil {
		t.Fatal("delete missing: expected error, got nil")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("expected silentError code=1, got %T %v", err, err)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr missing not-found message: %q", stderr)
	}
}
