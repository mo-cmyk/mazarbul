package ingest

import (
	"context"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// stubSecrets is a fake SecretReader keyed by name.
type stubSecrets struct {
	vals map[string]string
	err  error
}

func (s stubSecrets) Get(_ context.Context, name string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if v, ok := s.vals[name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func TestNewSource_UnknownConnectorErrors(t *testing.T) {
	t.Parallel()
	_, err := NewSource(context.Background(), "slack", Config{RepoPath: "/tmp"})
	if err == nil {
		t.Fatal("NewSource(slack): want error, got nil")
	}
}

func TestNewSource_LocalGitRequiresRepoPath(t *testing.T) {
	t.Parallel()
	_, err := NewSource(context.Background(), connectorLocalGit, Config{})
	if err == nil {
		t.Fatal("NewSource(local-git) with empty repo path: want error, got nil")
	}
}

func TestNewSource_LocalGitConstructsWithPRStream(t *testing.T) {
	t.Parallel()
	src, err := NewSource(context.Background(), connectorLocalGit, Config{RepoPath: "/tmp/repo"})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if src.Name() != connectorLocalGit {
		t.Errorf("Name = %q, want %q", src.Name(), connectorLocalGit)
	}
	// The GitHub client is always wired, so the PR stream is offered.
	streams := src.Streams()
	if len(streams) != 2 {
		t.Fatalf("streams = %v, want commits + pull-requests", streams)
	}
}

func TestConnectors_ListsRegistered(t *testing.T) {
	t.Parallel()
	got := Connectors()
	want := map[string]bool{connectorLocalGit: false, connectorClaudeChats: false, connectorGranola: false}
	for _, name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("Connectors() has unexpected %q", name)
			continue
		}
		want[name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("Connectors() missing %q (got %v)", name, got)
		}
	}
}

func TestLookupGitHubToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		secrets   SecretReader
		wantToken string
		wantErr   bool
	}{
		{"nil provider yields empty token", nil, "", false},
		{
			"present token returned",
			stubSecrets{vals: map[string]string{githubTokenSecretKey: "ghp_x"}},
			"ghp_x", false,
		},
		{"not found yields empty token", stubSecrets{vals: map[string]string{}}, "", false},
		{"real provider error propagates", stubSecrets{err: errors.New("keychain locked")}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := lookupGitHubToken(context.Background(), tc.secrets)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.wantToken {
				t.Errorf("token = %q, want %q", got, tc.wantToken)
			}
		})
	}
}
