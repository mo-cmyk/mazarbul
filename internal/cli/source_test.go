package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBuildIngestRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		connector string
		flags     sourceIngestFlags
		wantKeys  map[string]any
		absent    []string
	}{
		{
			name:      "minimal includes connector and repo only",
			connector: "local-git",
			flags:     sourceIngestFlags{repo: "/r"},
			wantKeys:  map[string]any{"connector": "local-git", "repoPath": "/r"},
			absent:    []string{"scope", "dryRun", "maxItems", "maxExtractionCalls"},
		},
		{
			name:      "all options set",
			connector: "local-git",
			flags:     sourceIngestFlags{repo: "/r", scope: "team", dryRun: true, maxItems: 10, maxExtractionCalls: 5},
			wantKeys: map[string]any{
				"connector": "local-git", "repoPath": "/r", "scope": "team",
				"dryRun": true, "maxItems": float64(10), "maxExtractionCalls": float64(5),
			},
		},
		{
			name:      "zero bounds are omitted",
			connector: "local-git",
			flags:     sourceIngestFlags{repo: "/r", maxItems: 0},
			absent:    []string{"maxItems", "maxExtractionCalls", "dryRun"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := buildIngestRequest(tc.connector, &tc.flags)
			if err != nil {
				t.Fatalf("buildIngestRequest: %v", err)
			}
			var got map[string]any
			if uerr := json.Unmarshal(raw, &got); uerr != nil {
				t.Fatalf("unmarshal: %v", uerr)
			}
			for k, want := range tc.wantKeys {
				if got[k] != want {
					t.Errorf("key %q = %v (%T), want %v (%T)", k, got[k], got[k], want, want)
				}
			}
			for _, k := range tc.absent {
				if _, ok := got[k]; ok {
					t.Errorf("key %q should be absent, got %v", k, got[k])
				}
			}
		})
	}
}

func TestRenderIngestReportCLI_Text(t *testing.T) {
	t.Parallel()
	report := `{"connector":"local-git","jobUid":"job-1","dryRun":true,"itemsApplied":3,"itemsSkipped":1,"episodesAppended":3,"streams":[{"stream":"commits","sequence":3,"applied":3,"skipped":1},{"stream":"pull-requests","applied":0,"skipped":0,"err":"network down"}]}`
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderIngestReportCLI(cmd, "source connect", json.RawMessage(report)); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"DRY RUN", "local-git", "applied", "commits", "network down", "job-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderSourceListCLI_Text(t *testing.T) {
	t.Parallel()
	resp := `{"connectors":["local-git"],"watermarks":[{"connector":"local-git","stream":"commits","sequence":7,"advanceTime":"2026-06-17T12:00:00Z"}]}`
	cmd := &cobra.Command{}
	cmd.Flags().String("output", "text", "output format")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderSourceListCLI(cmd, json.RawMessage(resp)); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"connectors: local-git", "commits", "7"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderSourceListCLI_EmptyText(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderSourceListCLI(cmd, json.RawMessage(`{"connectors":["local-git"],"watermarks":[]}`)); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "no sources connected yet") {
		t.Errorf("expected empty hint, got:\n%s", buf.String())
	}
}

func TestSourceSubcommands_Registered(t *testing.T) {
	t.Parallel()
	parent := newSourceCmd()
	want := map[string]bool{"connect": false, "ingest": false, "list": false, "ref": false, "record": false}
	for _, c := range parent.Commands() {
		name := strings.Fields(c.Use)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("maz source missing subcommand %q", name)
		}
	}
}

func TestSourceConnectCmd_RequiresRepoFlag(t *testing.T) {
	t.Parallel()
	cmd := newSourceConnectCmd()
	flag := cmd.Flags().Lookup("repo")
	if flag == nil {
		t.Fatal("connect command missing --repo flag")
	}
	for _, name := range []string{"scope", "dry-run", "max-items", "max-extraction-calls"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("connect command missing --%s flag", name)
		}
	}
}
