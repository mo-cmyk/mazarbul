package tools_test

// team_tools_test.go covers the thin MCP adapters over team.Service: the
// service-sentinel -> MCP error-code mapping and the success output shapes.
// The crypto lifecycle itself is proven in internal/team.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/team"
)

// fakeTeamOps is a programmable TeamOps double.
type fakeTeamOps struct {
	createRes  team.CreateResult
	joinRes    team.JoinResult
	grantRes   team.GrantResult
	revokeRes  team.RevokeResult
	membersRes []team.Member
	joinKeyRes team.JoinKey
	err        error
}

func (f fakeTeamOps) Create(context.Context, string) (team.CreateResult, error) {
	return f.createRes, f.err
}

func (f fakeTeamOps) Join(context.Context, string) (team.JoinResult, error) {
	return f.joinRes, f.err
}

func (f fakeTeamOps) Grant(_ context.Context, _ *team.JoinKey, _ string) (team.GrantResult, error) {
	return f.grantRes, f.err
}

func (f fakeTeamOps) RoleSet(_ context.Context, _ string, _ string) (team.RoleSetResult, error) {
	return team.RoleSetResult{}, f.err
}

func (f fakeTeamOps) Revoke(context.Context, string, string) (team.RevokeResult, error) {
	return f.revokeRes, f.err
}

func (f fakeTeamOps) Members(context.Context) ([]team.Member, error) {
	return f.membersRes, f.err
}

func (f fakeTeamOps) LocalJoinKey(context.Context) (team.JoinKey, error) {
	return f.joinKeyRes, f.err
}

func (f fakeTeamOps) RequestRegrant(_ context.Context, epochs []int) (team.RequestRegrantResult, error) {
	return team.RequestRegrantResult{MissingEpochs: epochs}, f.err
}

func TestTeamCreate_Success(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{createRes: team.CreateResult{GenesisPrincipalUID: "G", DEKEpoch: 0}}
	out, err := tools.NewTeamCreate(op).Invoke(context.Background(), json.RawMessage(`{"remote":"git@x:t.git"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		GenesisPrincipalUID string `json:"genesisPrincipalUid"`
		DEKEpoch            int    `json:"dekEpoch"`
		Status              string `json:"status"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GenesisPrincipalUID != "G" || got.Status != "created" {
		t.Errorf("output = %+v", got)
	}
}

func TestTeamCreate_AlreadyExists_Validation(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{err: team.ErrTeamExists}
	_, err := tools.NewTeamCreate(op).Invoke(context.Background(), json.RawMessage(`{}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

func TestTeamJoin_Success(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{joinRes: team.JoinResult{RemoteURL: "git@x:t.git"}}
	out, err := tools.NewTeamJoin(op).Invoke(context.Background(), json.RawMessage(`{"remote":"git@x:t.git"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		RemoteURL string   `json:"remoteUrl"`
		NextSteps []string `json:"nextSteps"`
		Status    string   `json:"status"`
	}
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if got.RemoteURL != "git@x:t.git" || got.Status != "joined" {
		t.Errorf("output = %+v", got)
	}
	if len(got.NextSteps) == 0 {
		t.Error("join should return actionable next steps")
	}
}

func TestTeamJoin_MissingRemote_Validation(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{}
	_, err := tools.NewTeamJoin(op).Invoke(context.Background(), json.RawMessage(`{}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

// grantReq builds a team_grant request body carrying an encoded join key.
func grantReq(t *testing.T, jk team.JoinKey) json.RawMessage {
	t.Helper()
	blob, err := jk.Encode()
	if err != nil {
		t.Fatalf("encode join key: %v", err)
	}
	return json.RawMessage(fmt.Sprintf(`{"joinKey":%q}`, blob))
}

func TestTeamGrant_NonMember_Permission(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{err: team.ErrNotMember}
	_, err := tools.NewTeamGrant(op).Invoke(context.Background(),
		grantReq(t, team.JoinKey{PrincipalUID: "B", WrapPublicKey: "AAAA"}))
	wantRelateCode(t, err, mcp.CodePermission)
}

func TestTeamGrant_MalformedJoinKey_Validation(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{grantRes: team.GrantResult{}}
	_, err := tools.NewTeamGrant(op).Invoke(context.Background(),
		json.RawMessage(`{"joinKey":"not-a-valid-blob!!"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

func TestTeamGrant_Success(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{grantRes: team.GrantResult{
		GranteePrincipalUID: "B", DEKEpoch: 2, GrantedEpochs: []int{0, 1, 2},
	}}
	out, err := tools.NewTeamGrant(op).Invoke(context.Background(),
		grantReq(t, team.JoinKey{PrincipalUID: "B", WrapPublicKey: "AAAA"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		DEKEpoch      int    `json:"dekEpoch"`
		GrantedEpochs []int  `json:"grantedEpochs"`
		Status        string `json:"status"`
	}
	_ = json.Unmarshal(out, &got)
	if got.DEKEpoch != 2 || got.Status != "granted" {
		t.Errorf("output = %+v", got)
	}
	// The full-history epoch set must round-trip so the CLI can report honest
	// coverage (a new member reads the entire shared brain, not just epoch 2+).
	if len(got.GrantedEpochs) != 3 || got.GrantedEpochs[0] != 0 || got.GrantedEpochs[2] != 2 {
		t.Errorf("grantedEpochs = %v, want [0 1 2]", got.GrantedEpochs)
	}
}

func TestTeamRevoke_NoTeam_NotFound(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{err: team.ErrNoTeam}
	_, err := tools.NewTeamRevoke(op).Invoke(context.Background(), json.RawMessage(`{"principalUid":"B"}`))
	wantRelateCode(t, err, mcp.CodeNotFound)
}

func TestTeamMembers_ListsACLWithoutPII(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{membersRes: []team.Member{
		{PrincipalUID: "G", WrapPublicKey: "pubG", DEKEpoch: 0, IsGenesis: true},
		{PrincipalUID: "A", WrapPublicKey: "pubA", DEKEpoch: 1},
	}}
	out, err := tools.NewTeamMembers(op).Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		Members []struct {
			PrincipalUID string `json:"principalUid"`
			DEKEpoch     int    `json:"dekEpoch"`
			IsGenesis    bool   `json:"isGenesis"`
		} `json:"members"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(got.Members))
	}
	if !got.Members[0].IsGenesis || got.Members[0].PrincipalUID != "G" {
		t.Errorf("first member = %+v, want genesis G", got.Members[0])
	}
	// No wrappedDek leaks into the members listing.
	if string(out) == "" || containsSubstr(string(out), "wrappedDek") {
		t.Errorf("members output must not expose wrappedDek: %s", out)
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestTeamCreate_ConfigAbsent_NotInternal asserts that when team.Create fails
// because the user config file is missing (i.e. maz init has not been run yet),
// mapTeamError maps the error to E_VALIDATION — NOT E_INTERNAL — and the error
// message hints at running maz init.
//
// B10: before the fix, config.ErrUserConfigNotFound fell through the default
// arm in mapTeamError and was returned as E_INTERNAL, causing agents to treat
// a stable precondition failure as a transient/retryable bug.
func TestTeamCreate_ConfigAbsent_NotInternal(t *testing.T) {
	t.Parallel()
	// Wrap as the real stack would: team.ConfigFileMutator wraps the sentinel
	// inside "team: enable envelope mode: team: load user config: <sentinel>".
	// We reproduce the wrapping here so the test is realistic without booting
	// real storage.
	wrappedErr := fmt.Errorf("team: enable envelope mode: team: load user config: %w", config.ErrUserConfigNotFound)
	op := fakeTeamOps{err: wrappedErr}
	_, err := tools.NewTeamCreate(op).Invoke(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var te *mcp.ToolError
	if !findToolError(err, &te) {
		t.Fatalf("expected *mcp.ToolError, got %T: %v", err, err)
	}
	if te.Code == mcp.CodeInternal {
		t.Errorf("config-absent error must not be E_INTERNAL (agents treat it as transient); got %s: %s", te.Code, te.Message)
	}
	if te.Code != mcp.CodeValidation {
		t.Errorf("expected E_VALIDATION for config-absent precondition, got %s", te.Code)
	}
	if !strings.Contains(te.Message, "init") {
		t.Errorf("error message should hint at maz init; got %q", te.Message)
	}
}

// findToolError unwraps err into a *mcp.ToolError via errors.As.
func findToolError(err error, out **mcp.ToolError) bool {
	var te *mcp.ToolError
	if errors.As(err, &te) {
		*out = te
		return true
	}
	return false
}

func TestTeamKey_Success(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{joinKeyRes: team.JoinKey{PrincipalUID: "P1", WrapPublicKey: "cHViS2V5"}}
	out, err := tools.NewTeamKey(op).Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		PrincipalUID  string `json:"principalUid"`
		WrapPublicKey string `json:"wrapPublicKey"`
		JoinKey       string `json:"joinKey"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PrincipalUID != "P1" || got.WrapPublicKey != "cHViS2V5" {
		t.Errorf("output = %+v, want {P1, cHViS2V5}", got)
	}
	// The emitted blob decodes back to the same join request a granter consumes.
	decoded, err := team.DecodeJoinKey(got.JoinKey)
	if err != nil {
		t.Fatalf("decode emitted join key: %v", err)
	}
	if decoded.PrincipalUID != "P1" || decoded.WrapPublicKey != "cHViS2V5" {
		t.Errorf("decoded join key = %+v, want {P1, cHViS2V5}", decoded)
	}
}

func TestTeamKey_SelfUnbound_Validation(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{err: team.ErrSelfUnbound}
	_, err := tools.NewTeamKey(op).Invoke(context.Background(), json.RawMessage(`{}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

func TestTeamRevoke_Success_IncludesChecklist(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{revokeRes: team.RevokeResult{RevokedPrincipalUID: "B", NewDEKEpoch: 1, RewrappedTo: []string{"G", "A"}}}
	out, err := tools.NewTeamRevoke(op).Invoke(context.Background(), json.RawMessage(`{"principalUid":"B"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		NewDEKEpoch int      `json:"newDekEpoch"`
		Checklist   []string `json:"checklist"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.NewDEKEpoch != 1 {
		t.Errorf("newDekEpoch = %d, want 1", got.NewDEKEpoch)
	}
	if len(got.Checklist) < 2 {
		t.Errorf("checklist too short: %v", got.Checklist)
	}
}

// ---------------------------------------------------------------------------
// Role / RBAC MCP tool tests
// ---------------------------------------------------------------------------

func TestMapTeamError_ErrInsufficientRole_IsPermission(t *testing.T) {
	t.Parallel()
	// ErrInsufficientRole must map to E_PERMISSION so agents understand they
	// need a higher-privileged principal, not a transient retry.
	op := fakeTeamOps{err: team.ErrInsufficientRole}
	_, err := tools.NewTeamGrant(op).Invoke(context.Background(),
		// Build a minimal valid join-key blob so the decode step succeeds.
		func() json.RawMessage {
			jk := team.JoinKey{PrincipalUID: "B", WrapPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
			blob, _ := jk.Encode()
			body, _ := json.Marshal(map[string]string{"joinKey": blob})
			return body
		}(),
	)
	wantRelateCode(t, err, mcp.CodePermission)
}

func TestTeamRoleSet_Success(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{}
	out, err := tools.NewTeamRoleSet(op).Invoke(context.Background(),
		json.RawMessage(`{"principalUid":"B","role":"admin"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		SubjectPrincipalUID string `json:"subjectPrincipalUid"`
		Role                string `json:"role"`
		Status              string `json:"status"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != "role_set" {
		t.Errorf("status = %q, want role_set", got.Status)
	}
}

func TestTeamRoleSet_MissingPrincipal_Validation(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{}
	_, err := tools.NewTeamRoleSet(op).Invoke(context.Background(),
		json.RawMessage(`{"principalUid":"","role":"member"}`))
	wantRelateCode(t, err, mcp.CodeValidation)
}

func TestTeamRoleSet_ErrInsufficientRole_Permission(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{err: team.ErrInsufficientRole}
	_, err := tools.NewTeamRoleSet(op).Invoke(context.Background(),
		json.RawMessage(`{"principalUid":"B","role":"owner"}`))
	wantRelateCode(t, err, mcp.CodePermission)
}

func TestTeamGrant_RolePassedThrough(t *testing.T) {
	t.Parallel()
	op := fakeTeamOps{grantRes: team.GrantResult{
		GranteePrincipalUID: "B",
		DEKEpoch:            0,
		GrantedEpochs:       []int{0},
	}}
	jk := team.JoinKey{PrincipalUID: "B", WrapPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
	blob, _ := jk.Encode()
	body, _ := json.Marshal(map[string]string{"joinKey": blob, "role": "reader"})
	out, err := tools.NewTeamGrant(op).Invoke(context.Background(), body)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Role != "reader" {
		t.Errorf("role = %q, want reader (echoed from input)", got.Role)
	}
}
