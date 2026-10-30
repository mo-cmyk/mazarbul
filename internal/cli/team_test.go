package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// fakeTeamTool is a minimal mcp.Tool used to drive the team commands
// hermetically: it returns a canned result (or error) for a single tool
// name without any kernel/daemon machinery.
type fakeTeamTool struct {
	name   string
	result json.RawMessage
	err    error
}

func (f fakeTeamTool) Name() string                  { return f.name }
func (f fakeTeamTool) Description() string           { return "fake " + f.name }
func (f fakeTeamTool) InputSchema() json.RawMessage  { return json.RawMessage(`{}`) }
func (f fakeTeamTool) OutputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f fakeTeamTool) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// stubTeamWiring routes every team command through the in-process path
// with the supplied fake tools, restoring the seams on cleanup. Passing
// no tools simulates an unprovisioned identity layer (tool-not-found).
func stubTeamWiring(t *testing.T, tools ...mcp.Tool) {
	t.Helper()
	prevPre := preflightDaemon
	preflightDaemon = func(string, string) error { return nil } // no daemon → in-process
	t.Cleanup(func() { preflightDaemon = prevPre })

	prevOpen := openWiringFn
	openWiringFn = func(context.Context, daemon.WiringConfig) (*daemon.Wired, error) {
		return &daemon.Wired{Tools: tools}, nil
	}
	t.Cleanup(func() { openWiringFn = prevOpen })
}

// runTeam drives `maz team <args...>` against a stubbed wiring and returns
// stdout, stderr, and the execute error.
func runTeam(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", t.TempDir(), "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newTeamCmd())
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	root.SetContext(context.Background())
	err := root.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestTeamHelpHasExamples(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"create", "grant", "revoke", "members"} {
		var buf bytes.Buffer
		root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
		root.PersistentFlags().String("data-dir", "", "data directory")
		root.PersistentFlags().Bool("json", false, "JSON output")
		root.PersistentFlags().String("output", "", "output format")
		root.AddCommand(newTeamCmd())
		root.SetOut(&buf)
		root.SetArgs([]string{"team", sub, "--help"})
		if err := root.Execute(); err != nil {
			t.Fatalf("team %s --help: %v", sub, err)
		}
		out := buf.String()
		if strings.TrimSpace(out) == "" {
			t.Fatalf("team %s --help produced empty output", sub)
		}
		if !strings.Contains(out, "Examples:") {
			t.Errorf("team %s --help missing Examples:\n%s", sub, out)
		}
		if !strings.Contains(out, "maz team "+sub) {
			t.Errorf("team %s --help missing example invocation\n%s", sub, out)
		}
	}
}

// TestTeamGrantRequiresExactlyOneKeySource: no t.Parallel — stubs package-level seams.
func TestTeamGrantRequiresExactlyOneKeySource(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "neither key flag is rejected",
			args: []string{"team", "grant"},
			want: "exactly one of",
		},
		{
			name: "both key flags are rejected",
			args: []string{
				"team", "grant",
				"--join-key", "BLOB", "--from-file", "/tmp/x.joinkey",
			},
			want: "mutually exclusive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A tool is wired so the failure is the flag check, not a missing tool.
			stubTeamWiring(t, fakeTeamTool{name: "team_grant", result: json.RawMessage(`{}`)})
			_, errOut, err := runTeam(t, tc.args...)
			if err == nil {
				t.Fatalf("expected error for %q", tc.name)
			}
			var se silentError
			if !errors.As(err, &se) || se.code != 1 {
				t.Errorf("want silentError code 1, got %v", err)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr %q missing %q", errOut, tc.want)
			}
		})
	}
}

// TestTeamGrantFromFileTrimsKey: no t.Parallel — stubs package-level seams.
func TestTeamGrantFromFileTrimsKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "alice.joinkey")
	if err := os.WriteFile(keyPath, []byte("  SOMEJOINKEYBLOB==\n\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	var gotKey string
	tool := fakeTeamToolFunc{
		name: "team_grant",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			var in struct {
				JoinKey string `json:"joinKey"`
			}
			_ = json.Unmarshal(req, &in)
			gotKey = in.JoinKey
			return json.RawMessage(`{"granteePrincipalUid":"P1","dekEpoch":3,"status":"granted"}`), nil
		},
	}
	stubTeamWiring(t, tool)
	out, _, err := runTeam(t, "--output", "text",
		"team", "grant", "--from-file", keyPath)
	if err != nil {
		t.Fatalf("team grant: %v", err)
	}
	if gotKey != "SOMEJOINKEYBLOB==" {
		t.Errorf("join key = %q, want trimmed SOMEJOINKEYBLOB==", gotKey)
	}
	if !strings.Contains(out, "epoch 3") {
		t.Errorf("human output missing epoch: %q", out)
	}
}

// TestTeamMembersJSONIsValidJSON: no t.Parallel — stubs package-level seams.
func TestTeamMembersJSONIsValidJSON(t *testing.T) {
	raw := `{"members":[{"principalUid":"P1","dekEpoch":2,"wrapPublicKey":"AAAABBBBCCCCDDDD","isGenesis":true}]}`
	stubTeamWiring(t, fakeTeamTool{name: "team_members", result: json.RawMessage(raw)})
	out, _, err := runTeam(t, "--output", "json", "team", "members")
	if err != nil {
		t.Fatalf("team members: %v", err)
	}
	var parsed map[string]any
	if uerr := json.Unmarshal([]byte(out), &parsed); uerr != nil {
		t.Fatalf("team members --output json did not emit valid JSON: %v\n%s", uerr, out)
	}
	if _, ok := parsed["members"]; !ok {
		t.Errorf("json missing members key: %s", out)
	}
}

// TestTeamKeyTextPrintsJoinRequest: no t.Parallel — stubs package-level seams.
func TestTeamKeyTextPrintsJoinRequest(t *testing.T) {
	raw := `{"principalUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","wrapPublicKey":"cHViS2V5QmFzZTY0","joinKey":"ENCODEDJOINKEYBLOB"}`
	stubTeamWiring(t, fakeTeamTool{name: "team_key", result: json.RawMessage(raw)})
	out, _, err := runTeam(t, "--output", "text", "team", "key")
	if err != nil {
		t.Fatalf("team key: %v", err)
	}
	if !strings.Contains(out, "01ARZ3NDEKTSV4RRFFQ69G5FAV") || !strings.Contains(out, "cHViS2V5QmFzZTY0") {
		t.Errorf("team key text missing principal/key: %q", out)
	}
	// The blob and the actionable grant hint are the join handoff UX.
	if !strings.Contains(out, "ENCODEDJOINKEYBLOB") {
		t.Errorf("team key text missing the join-key blob: %q", out)
	}
	if !strings.Contains(out, "maz team grant --join-key") {
		t.Errorf("team key text missing grant hint: %q", out)
	}
}

// TestTeamKeyJSONIsRawToolResult: no t.Parallel — stubs package-level seams.
func TestTeamKeyJSONIsRawToolResult(t *testing.T) {
	raw := `{"principalUid":"P1","wrapPublicKey":"K1"}`
	stubTeamWiring(t, fakeTeamTool{name: "team_key", result: json.RawMessage(raw)})
	out, _, err := runTeam(t, "--output", "json", "team", "key")
	if err != nil {
		t.Fatalf("team key: %v", err)
	}
	var got struct {
		PrincipalUID  string `json:"principalUid"`
		WrapPublicKey string `json:"wrapPublicKey"`
	}
	if uerr := json.Unmarshal([]byte(out), &got); uerr != nil {
		t.Fatalf("team key --output json invalid: %v\n%s", uerr, out)
	}
	if got.PrincipalUID != "P1" || got.WrapPublicKey != "K1" {
		t.Errorf("got %+v", got)
	}
}

// TestTeamMembersTableTruncatesWrapKey: no t.Parallel — stubs package-level seams.
func TestTeamMembersTableTruncatesWrapKey(t *testing.T) {
	raw := `{"members":[{"principalUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","dekEpoch":2,` +
		`"wrapPublicKey":"AAAABBBBCCCCDDDDEEEEFFFF","isGenesis":true}]}`
	stubTeamWiring(t, fakeTeamTool{name: "team_members", result: json.RawMessage(raw)})
	out, _, err := runTeam(t, "--output", "text", "team", "members")
	if err != nil {
		t.Fatalf("team members: %v", err)
	}
	if !strings.Contains(out, "PRINCIPAL") || !strings.Contains(out, "WRAP_KEY") {
		t.Errorf("table header missing: %q", out)
	}
	if !strings.Contains(out, "AAAABBBBCCCC…") {
		t.Errorf("wrap key not truncated with ellipsis: %q", out)
	}
	if strings.Contains(out, "EEEEFFFF") {
		t.Errorf("full wrap key leaked into table: %q", out)
	}
	if !strings.Contains(out, "yes") {
		t.Errorf("genesis flag missing: %q", out)
	}
}

// TestTeamMembersNDJSONStreamsRows: no t.Parallel — stubs package-level seams.
func TestTeamMembersNDJSONStreamsRows(t *testing.T) {
	raw := `{"members":[` +
		`{"principalUid":"P1","dekEpoch":1,"wrapPublicKey":"K1","isGenesis":true},` +
		`{"principalUid":"P2","dekEpoch":2,"wrapPublicKey":"K2","isGenesis":false}]}`
	stubTeamWiring(t, fakeTeamTool{name: "team_members", result: json.RawMessage(raw)})
	out, _, err := runTeam(t, "--output", "ndjson", "team", "members")
	if err != nil {
		t.Fatalf("team members: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("ndjson want 2 lines, got %d: %q", len(lines), out)
	}
	for _, ln := range lines {
		var row teamMember
		if uerr := json.Unmarshal([]byte(ln), &row); uerr != nil {
			t.Errorf("ndjson line not valid JSON: %v (%q)", uerr, ln)
		}
	}
}

// TestTeamToolErrorExitsNonZero: no t.Parallel — stubs package-level seams.
func TestTeamToolErrorExitsNonZero(t *testing.T) {
	stubTeamWiring(t, fakeTeamTool{name: "team_grant", err: errors.New("E_PERMISSION: caller is not an admitted member")})
	_, errOut, err := runTeam(t,
		"team", "grant", "--join-key", "SOMEJOINKEYBLOB")
	if err == nil {
		t.Fatal("expected non-zero exit on a tool error")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("want silentError code 1, got %v", err)
	}
	if !strings.Contains(errOut, "E_PERMISSION") {
		t.Errorf("stderr should surface the tool error verbatim: %q", errOut)
	}
}

// TestTeamToolNotAvailableExitsOne: no t.Parallel — stubs package-level seams.
func TestTeamToolNotAvailableExitsOne(t *testing.T) {
	// No tools wired → the dispatch must report the actionable hint.
	stubTeamWiring(t)
	_, errOut, err := runTeam(t, "team", "members")
	if err == nil {
		t.Fatal("expected an error when team tools are absent")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("want silentError code 1, got %v", err)
	}
	if !strings.Contains(errOut, "team tools not available") {
		t.Errorf("stderr missing actionable hint: %q", errOut)
	}
}

// TestTeamCreateHumanOutput: no t.Parallel — stubs package-level seams.
func TestTeamCreateHumanOutput(t *testing.T) {
	raw := `{"genesisPrincipalUid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","dekEpoch":1,"status":"created"}`
	stubTeamWiring(t, fakeTeamTool{name: "team_create", result: json.RawMessage(raw)})
	out, _, err := runTeam(t, "--output", "text", "team", "create")
	if err != nil {
		t.Fatalf("team create: %v", err)
	}
	for _, want := range []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "envelope mode", "maz team grant"} {
		if !strings.Contains(out, want) {
			t.Errorf("create output missing %q:\n%s", want, out)
		}
	}
}

// TestTeamJoinHumanOutput: no t.Parallel — stubs package-level seams.
func TestTeamJoinHumanOutput(t *testing.T) {
	raw := `{"remoteUrl":"git@github.com:acme/brain.git","nextSteps":["run team key","sync pull"],"status":"joined"}`
	stubTeamWiring(t, fakeTeamTool{name: "team_join", result: json.RawMessage(raw)})
	out, _, err := runTeam(t, "--output", "text", "team", "join", "--remote", "git@github.com:acme/brain.git")
	if err != nil {
		t.Fatalf("team join: %v", err)
	}
	for _, want := range []string{"git@github.com:acme/brain.git", "Next steps", "run team key", "sync pull"} {
		if !strings.Contains(out, want) {
			t.Errorf("join output missing %q:\n%s", want, out)
		}
	}
}

// TestTeamJoinRequiresRemote: no t.Parallel — stubs package-level seams.
func TestTeamJoinRequiresRemote(t *testing.T) {
	stubTeamWiring(t, fakeTeamTool{name: "team_join", result: json.RawMessage(`{}`)})
	_, _, err := runTeam(t, "team", "join")
	if err == nil {
		t.Fatal("team join without --remote should error (required flag)")
	}
}

// TestTeamRevokePrintsChecklistVerbatim: no t.Parallel — stubs package-level seams.
func TestTeamRevokePrintsChecklistVerbatim(t *testing.T) {
	raw := `{"revokedPrincipalUid":"P1","newDekEpoch":4,"rewrappedTo":["P2","P3"],` +
		`"checklist":["forward re-key complete","remove their git-host access yourself",` +
		`"chunks they already pulled stay readable"]}`
	// team_members must be stubbed so the pre-revoke principal validation (B20.0
	// fix) can confirm that P1 is an admitted member before dispatching the revoke.
	membersRaw := `{"members":[{"principalUid":"P1","dekEpoch":1,"wrapPublicKey":"K1","isGenesis":true}]}`
	stubTeamWiring(t,
		fakeTeamTool{name: "team_members", result: json.RawMessage(membersRaw)},
		fakeTeamTool{name: "team_revoke", result: json.RawMessage(raw)},
	)
	out, _, err := runTeam(t, "--output", "text", "team", "revoke", "--principal", "P1")
	if err != nil {
		t.Fatalf("team revoke: %v", err)
	}
	for _, want := range []string{
		"P1", "4", "P2, P3",
		"remove their git-host access yourself",
		"chunks they already pulled stay readable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("revoke output missing %q:\n%s", want, out)
		}
	}
}

// fakeTeamToolFunc is a fake tool whose Invoke is a closure, used when a
// test needs to inspect the request body the command marshalled.
type fakeTeamToolFunc struct {
	name   string
	invoke func(req json.RawMessage) (json.RawMessage, error)
}

func (f fakeTeamToolFunc) Name() string                  { return f.name }
func (f fakeTeamToolFunc) Description() string           { return "fake " + f.name }
func (f fakeTeamToolFunc) InputSchema() json.RawMessage  { return json.RawMessage(`{}`) }
func (f fakeTeamToolFunc) OutputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f fakeTeamToolFunc) Invoke(_ context.Context, req json.RawMessage) (json.RawMessage, error) {
	return f.invoke(req)
}

// TestTeamCreate_BeforeInitExitsOneWithHint verifies that running
// "maz team create" when the team tool is absent (identity not provisioned /
// maz init not yet run) exits with code 1 and a hint to run maz init.
// B20.1: the current code must NOT exit 2 (E_INTERNAL).
func TestTeamCreate_BeforeInitExitsOneWithHint(t *testing.T) {
	// No tools wired simulates an unprovisioned identity layer
	// (the same signal invokeTeamTool produces for a missing tool).
	stubTeamWiring(t)
	_, errOut, err := runTeam(t, "team", "create")
	if err == nil {
		t.Fatal("expected error when team tools are absent")
	}
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("expected exit code 1 (expected failure), got %d\nstderr: %s", se.code, errOut)
	}
	if !strings.Contains(errOut, "init") {
		t.Errorf("stderr should hint at maz init: %q", errOut)
	}
}

// TestTeamRevoke_UnknownPrincipalExitsOne verifies that "maz team revoke
// --principal <unknown>" returns exit 1 with a validation error rather than
// silently rotating the DEK.
// B20.0: the CLI must validate the principal against team recipients first.
func TestTeamRevoke_UnknownPrincipalExitsOne(t *testing.T) {
	// Wire team_members to return two known members; team_revoke is a
	// func-tool so we can assert it is NEVER called.
	revokeCalled := false
	membersRaw := json.RawMessage(`{"members":[` +
		`{"principalUid":"KNOWN01","dekEpoch":1,"wrapPublicKey":"K1","isGenesis":true},` +
		`{"principalUid":"KNOWN02","dekEpoch":1,"wrapPublicKey":"K2","isGenesis":false}]}`)
	stubTeamWiring(t,
		fakeTeamTool{name: "team_members", result: membersRaw},
		fakeTeamToolFunc{
			name: "team_revoke",
			invoke: func(_ json.RawMessage) (json.RawMessage, error) {
				revokeCalled = true
				return json.RawMessage(`{"revokedPrincipalUid":"UNKNOWN","newDekEpoch":2,"checklist":[]}`), nil
			},
		},
	)
	_, errOut, err := runTeam(t, "team", "revoke", "--principal", "UNKNOWN_PRINCIPAL")
	if err == nil {
		t.Fatal("expected error for unknown principal, got nil")
	}
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("want exit code 1 (validation), got %d\nstderr: %s", se.code, errOut)
	}
	if revokeCalled {
		t.Error("team_revoke tool was called despite an unknown principal; DEK was rotated silently")
	}
	if !strings.Contains(errOut, "UNKNOWN_PRINCIPAL") {
		t.Errorf("stderr should name the unknown principal: %q", errOut)
	}
}

// TestTeamRevoke_KnownPrincipalProceeds verifies that a valid principal
// (present in team members) is NOT blocked by the validation.
// B20.0 complementary: the happy path must still work after the fix.
func TestTeamRevoke_KnownPrincipalProceeds(t *testing.T) {
	membersRaw := json.RawMessage(`{"members":[` +
		`{"principalUid":"KNOWN01","dekEpoch":1,"wrapPublicKey":"K1","isGenesis":true},` +
		`{"principalUid":"KNOWN02","dekEpoch":1,"wrapPublicKey":"K2","isGenesis":false}]}`)
	revokeRaw := json.RawMessage(`{"revokedPrincipalUid":"KNOWN02","newDekEpoch":2,` +
		`"rewrappedTo":["KNOWN01"],"checklist":["forward re-key complete"]}`)
	stubTeamWiring(t,
		fakeTeamTool{name: "team_members", result: membersRaw},
		fakeTeamTool{name: "team_revoke", result: revokeRaw},
	)
	out, _, err := runTeam(t, "team", "revoke", "--principal", "KNOWN02")
	if err != nil {
		t.Fatalf("team revoke known principal: %v", err)
	}
	if !strings.Contains(out, "KNOWN02") {
		t.Errorf("output should name the revoked principal: %q", out)
	}
}

func TestDescribeGrantedEpochs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		epochs  []int
		current int
		want    string
	}{
		{"full history multi epoch", []int{0, 1, 2}, 2, "the full team history (epochs 0–2)"},
		{"full history single epoch", []int{0}, 0, "the full team history (epoch 0)"},
		{"gap lists actual epochs", []int{1, 2}, 2, "team data for epochs 1, 2 (a prior epoch's key was unavailable on the granting device)"},
		{"empty grants nothing", nil, 1, "no team history (no DEK epoch was wrapped)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := describeGrantedEpochs(tc.epochs, tc.current); got != tc.want {
				t.Errorf("describeGrantedEpochs(%v, %d) = %q, want %q", tc.epochs, tc.current, got, tc.want)
			}
		})
	}
}
