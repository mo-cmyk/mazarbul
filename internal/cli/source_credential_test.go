package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// runSourceCmd executes `maz source <args...>` against an isolated root and
// returns stdout, stderr, and the command error. Tests that touch the secret
// store set MAZ_SECRETS_DIR so PersistentChain/DefaultChain resolve to a
// directory FileProvider rather than the host keychain.
func runSourceCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	sourceCmd := newSourceCmd()
	var outBuf, errBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("output", "text", "output format")
	root.AddCommand(sourceCmd)
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"source"}, args...))
	err := root.ExecuteContext(t.Context())
	return outBuf.String(), errBuf.String(), err
}

func TestSourceAdd_StoresKeyInPersistentStore(t *testing.T) {
	t.Setenv(secret.EnvSecretsDir, t.TempDir())
	const key = "grn_testkey1234567890abcdef"
	out, _, err := runSourceCmd(t, "add", "granola", "--no-verify", "--api-key", key)
	if err != nil {
		t.Fatalf("add granola: %v", err)
	}
	if !strings.Contains(out, secret.NameGranolaAPIKey) {
		t.Errorf("stdout = %q, want a confirmation naming %s", out, secret.NameGranolaAPIKey)
	}
	got, gerr := secret.PersistentChain().Get(t.Context(), secret.NameGranolaAPIKey)
	if gerr != nil || got != key {
		t.Fatalf("persisted key = %q, %v; want %q", got, gerr, key)
	}
}

func TestSourceAdd_RejectsUnkeyedConnector(t *testing.T) {
	t.Setenv(secret.EnvSecretsDir, t.TempDir())
	_, _, err := runSourceCmd(t, "add", "local-git", "--no-verify", "--api-key", "x")
	if err == nil || !strings.Contains(err.Error(), "no API key to store") {
		t.Fatalf("want unkeyed-connector refusal, got %v", err)
	}
}

func TestSourceRevoke_DeletesAndVerifiesAbsent(t *testing.T) {
	t.Setenv(secret.EnvSecretsDir, t.TempDir())
	if err := secret.PersistentChain().Set(t.Context(), secret.NameGranolaAPIKey, "grn_tostore"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	out, _, err := runSourceCmd(t, "revoke", "granola")
	if err != nil {
		t.Fatalf("revoke granola: %v", err)
	}
	if !strings.Contains(out, "verified absent") {
		t.Errorf("stdout = %q, want verified-absent confirmation", out)
	}
	if _, gerr := secret.PersistentChain().Get(t.Context(), secret.NameGranolaAPIKey); gerr != secret.ErrNotFound { //nolint:errorlint // sentinel compared directly; chain returns the canonical ErrNotFound
		t.Fatalf("key still present after revoke: %v", gerr)
	}
}

func TestSourceRevoke_FailsLoudOnEnvShadow(t *testing.T) {
	t.Setenv(secret.EnvSecretsDir, t.TempDir())
	// A persisted entry AND a shadowing env override. Deleting the keychain entry
	// must not be reported as success while the env layer keeps the key live.
	if err := secret.PersistentChain().Set(t.Context(), secret.NameGranolaAPIKey, "grn_persisted"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	t.Setenv(secret.CanonicalEnvName(secret.NameGranolaAPIKey), "grn_shadow")

	_, _, err := runSourceCmd(t, "revoke", "granola")
	if err == nil {
		t.Fatal("revoke succeeded despite a shadowing env override; want a loud failure")
	}
	envVar := secret.CanonicalEnvName(secret.NameGranolaAPIKey)
	if !strings.Contains(err.Error(), envVar) || !strings.Contains(err.Error(), "environment") {
		t.Errorf("error = %v, want it to name %s and the environment", err, envVar)
	}
}

func TestSourceRevoke_NothingStored(t *testing.T) {
	t.Setenv(secret.EnvSecretsDir, t.TempDir())
	out, _, err := runSourceCmd(t, "revoke", "granola")
	if err != nil {
		t.Fatalf("revoke with nothing stored should be a clean no-op, got %v", err)
	}
	if !strings.Contains(out, "nothing to revoke") {
		t.Errorf("stdout = %q, want a nothing-to-revoke message", out)
	}
}

func TestSourceRevoke_RejectsUnkeyedConnector(t *testing.T) {
	_, _, err := runSourceCmd(t, "revoke", "claude-chats")
	if err == nil || !strings.Contains(err.Error(), "no stored API key") {
		t.Fatalf("want unkeyed-connector refusal, got %v", err)
	}
}
