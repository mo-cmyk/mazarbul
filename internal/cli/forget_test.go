package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// makeForgetRoot wires a minimal cobra root carrying the forget command
// plus the persistent flags it reads via cmd.Root().PersistentFlags().
func makeForgetRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newForgetCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// TestForget_NormalizeUID unit-tests the normalizeForgetUID helper in
// isolation — no daemon, no kernel. This is the primary regression target for
// B9.0: "episodes/<ULID>" must resolve to the bare ULID so the MCP tool does
// not double-prefix it.
func TestForget_NormalizeUID(t *testing.T) {
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	cases := []struct {
		name     string
		input    string
		wantUID  string
		wantColl forgetCollection
		wantErr  bool
	}{
		{
			name:     "bare ULID defaults to episode",
			input:    bareUID,
			wantUID:  bareUID,
			wantColl: forgetEpisode,
		},
		{
			name:     "episodes resource name strips prefix",
			input:    "episodes/" + bareUID,
			wantUID:  bareUID,
			wantColl: forgetEpisode,
		},
		{
			name:     "relations resource name strips prefix",
			input:    "relations/" + bareUID,
			wantUID:  bareUID,
			wantColl: forgetRelation,
		},
		{
			name:    "double prefix is rejected",
			input:   "episodes/episodes/" + bareUID,
			wantErr: true,
		},
		{
			name:    "lowercase ULID is rejected",
			input:   "01arz3ndektsv4rrffq69g5fav",
			wantErr: true,
		},
		{
			name:    "empty string is rejected",
			input:   "",
			wantErr: true,
		},
		{
			name:    "random text is rejected",
			input:   "not-a-ulid",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uid, coll, err := normalizeForgetUID(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeForgetUID(%q) = %q, nil; want error", tc.input, uid)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeForgetUID(%q) unexpected error: %v", tc.input, err)
			}
			if uid != tc.wantUID {
				t.Errorf("uid = %q, want %q", uid, tc.wantUID)
			}
			if coll != tc.wantColl {
				t.Errorf("collection = %v, want %v", coll, tc.wantColl)
			}
		})
	}
}

// TestForget_EpisodeResourceNameRoutedAsEpisodeUID pins that passing
// "episodes/<ULID>" to `maz forget` sends {"episodeUid": "<ULID>"} to the
// tool — not {"episodeUid": "episodes/<ULID>"} which would produce the
// double-prefix "not found" error (B9.0).
func TestForget_EpisodeResourceNameRoutedAsEpisodeUID(t *testing.T) {
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	var capturedReq json.RawMessage
	tool := fakeTeamToolFunc{
		name: "forget",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			capturedReq = req
			return json.RawMessage(`{"name":"episodes/` + bareUID + `","status":"forgotten"}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeForgetRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"forget", "episodes/" + bareUID,
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("forget episodes/<uid>: %v (stderr=%q)", err, errBuf.String())
	}

	var req map[string]any
	if err := json.Unmarshal(capturedReq, &req); err != nil {
		t.Fatalf("captured request not valid JSON: %v (%s)", err, capturedReq)
	}
	if got, ok := req["episodeUid"]; !ok || got != bareUID {
		t.Errorf("tool received episodeUid = %v; want bare %q", got, bareUID)
	}
	if _, bad := req["relationUid"]; bad {
		t.Errorf("tool received unexpected relationUid field")
	}
}

// TestForget_RelationResourceNameRoutedAsRelationUID pins that passing
// "relations/<ULID>" dispatches {"relationUid": "<ULID>"} — not episodeUid.
func TestForget_RelationResourceNameRoutedAsRelationUID(t *testing.T) {
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	var capturedReq json.RawMessage
	tool := fakeTeamToolFunc{
		name: "forget",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			capturedReq = req
			return json.RawMessage(`{"name":"relations/` + bareUID + `","status":"forgotten"}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeForgetRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"forget", "relations/" + bareUID,
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("forget relations/<uid>: %v (stderr=%q)", err, errBuf.String())
	}

	var req map[string]any
	if err := json.Unmarshal(capturedReq, &req); err != nil {
		t.Fatalf("captured request not valid JSON: %v (%s)", err, capturedReq)
	}
	if got, ok := req["relationUid"]; !ok || got != bareUID {
		t.Errorf("tool received relationUid = %v; want bare %q", got, bareUID)
	}
	if _, bad := req["episodeUid"]; bad {
		t.Errorf("tool received unexpected episodeUid field")
	}
}

// TestForget_MalformedUIDExitsOne pins that a clearly invalid argument
// exits with code 1 (expected failure), not code 2 (unexpected).
func TestForget_MalformedUIDExitsOne(t *testing.T) {
	stubTeamWiring(t) // no tools needed; normalizeForgetUID fires first

	var errBuf bytes.Buffer
	root := makeForgetRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--data-dir", t.TempDir(),
		"forget", "episodes/episodes/notaulid",
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error for malformed uid")
	}
	var se silentError
	if !isSilentError(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
}

// isSilentError unwraps err into *se if it is a silentError.
func isSilentError(err error, se *silentError) bool {
	var s silentError
	if errors.As(err, &s) {
		*se = s
		return true
	}
	return false
}

// TestForget_HelpNonEmpty pins that --help is non-empty and documents both
// the resource-name and bare-ULID forms.
func TestForget_HelpNonEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeForgetRoot(&buf, nil)
	root.SetArgs([]string{"forget", "--help"})
	root.SetContext(context.Background())
	_ = root.Execute()
	help := buf.String()
	if len(help) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"episodes/", "relations/", "maz forget"} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not contain %q", want)
		}
	}
}

// TestForget_HelpUsesLowercaseEventKinds pins B13: the help text must reference
// camelCase event kinds (episodeForgotten, relationForgotten) not PascalCase.
func TestForget_HelpUsesLowercaseEventKinds(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeForgetRoot(&buf, nil)
	root.SetArgs([]string{"forget", "--help"})
	root.SetContext(context.Background())
	_ = root.Execute()
	help := buf.String()
	if strings.Contains(help, "EpisodeForgotten") {
		t.Errorf("help contains PascalCase 'EpisodeForgotten'; must use camelCase 'episodeForgotten'")
	}
	if strings.Contains(help, "RelationForgotten") {
		t.Errorf("help contains PascalCase 'RelationForgotten'; must use camelCase 'relationForgotten'")
	}
}

// TestForget_NotFoundExitsOne pins B3: a tool E_NOT_FOUND response must
// exit with code 1 (expected failure), not code 2 (unexpected/bug).
func TestForget_NotFoundExitsOne(t *testing.T) {
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	tool := fakeTeamToolFunc{
		name: "forget",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return nil, mcp.NewNotFoundError("episode not found")
		},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeForgetRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"forget", bareUID,
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error for not-found episode")
	}
	var se silentError
	if !isSilentError(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1 for E_NOT_FOUND", se.code)
	}
}

// TestForget_ValidationErrorExitsOne pins B3: an E_VALIDATION tool response
// must exit with code 1, not code 2.
func TestForget_ValidationErrorExitsOne(t *testing.T) {
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	tool := fakeTeamToolFunc{
		name: "forget",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return nil, mcp.NewValidationError("bad input", nil)
		},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeForgetRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"forget", bareUID,
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error for validation failure")
	}
	var se silentError
	if !isSilentError(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1 for E_VALIDATION", se.code)
	}
}

// TestForget_MalformedUIDJSONEmitsValidationEnvelope pins BUG-B (cluster
// forget-malformed-envelope): in JSON mode a malformed uid (too short,
// lowercase, excluded Crockford chars, empty, or a resource-name with a
// garbage uid) must emit {"error":...,"code":"E_VALIDATION"} to stdout and
// exit 1. Before the fix, stdout was empty and only stderr had text.
func TestForget_MalformedUIDJSONEmitsValidationEnvelope(t *testing.T) {
	// No t.Parallel on the parent OR the subtests below: each subtest's
	// stubTeamWiring mutates the package-level preflightDaemon / openWiringFn
	// seam. Parallel subtests would race each other on it under -race (and a
	// parallel parent would race other top-level tests), so this stays serial.
	badUIDs := []string{
		"xyz",
		"",
		"01arz3ndektsv4rrffq69g5fav", // lowercase ULID
		"01ARZ3NDEKTSV4RRFFQ69G5FAI", // I is excluded from Crockford Base32
		"01ARZ3NDEKTSV4RRFFQ69G5FAL", // L is excluded
		"01ARZ3NDEKTSV4RRFFQ69G5FAO", // O is excluded
		"01ARZ3NDEKTSV4RRFFQ69G5FAU", // U is excluded
		"episodes/notaulid",
	}
	for _, badUID := range badUIDs {
		t.Run(badUID, func(t *testing.T) {
			// No t.Parallel: stubTeamWiring writes the shared seam (see parent).
			stubTeamWiring(t) // no tools needed; normalizeForgetUID fires first

			var outBuf, errBuf bytes.Buffer
			root := makeForgetRoot(&outBuf, &errBuf)
			args := []string{
				"--output", "json",
				"--data-dir", t.TempDir(),
				"forget",
			}
			if badUID != "" {
				args = append(args, badUID)
			} else {
				// empty string: pass as explicit arg so ExactArgs(1) is satisfied
				args = append(args, "")
			}
			root.SetArgs(args)
			root.SetContext(context.Background())

			err := root.Execute()
			if err == nil {
				t.Fatalf("uid=%q: expected error, got nil", badUID)
			}
			var se silentError
			if !isSilentError(err, &se) {
				t.Fatalf("uid=%q: expected silentError, got %T: %v", badUID, err, err)
			}
			if se.code != 1 {
				t.Errorf("uid=%q: exit code = %d, want 1", badUID, se.code)
			}

			raw := outBuf.String()
			if raw == "" {
				t.Fatalf("uid=%q: JSON mode: stdout empty; expected {error,code} envelope", badUID)
			}
			var got map[string]any
			if jerr := json.Unmarshal([]byte(strings.TrimSpace(raw)), &got); jerr != nil {
				t.Fatalf("uid=%q: JSON mode: stdout not valid JSON: %v\nraw: %q", badUID, jerr, raw)
			}
			if _, ok := got["error"]; !ok {
				t.Errorf("uid=%q: JSON envelope missing 'error' key: %s", badUID, raw)
			}
			code, ok := got["code"]
			if !ok {
				t.Errorf("uid=%q: JSON envelope missing 'code' key: %s", badUID, raw)
			} else if code != string(mcp.CodeValidation) {
				t.Errorf("uid=%q: code = %q, want %q", badUID, code, mcp.CodeValidation)
			}
		})
	}
}

// TestForget_EntityCollectionClearsMessage pins E2: passing entities/<ulid>
// must produce an error that names the unsupported "entities" collection and
// does NOT say "not a valid ULID or resource name". Exit code must be 1.
func TestForget_EntityCollectionClearsMessage(t *testing.T) {
	// No t.Parallel: stubTeamWiring mutates the package-level preflightDaemon /
	// openWiringFn seam, which races -race against parallel readers. Serial tests
	// run before parallels resume.
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	stubTeamWiring(t) // normalizeForgetUID fires before any tool call

	var errBuf bytes.Buffer
	root := makeForgetRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"forget", "entities/" + bareUID,
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for entities/<uid>")
	}
	var se silentError
	if !isSilentError(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
	stderr := errBuf.String()
	if strings.Contains(stderr, "not a valid ULID") {
		t.Errorf("error should not say %q; got: %s", "not a valid ULID", stderr)
	}
	if !strings.Contains(stderr, "cannot be forgotten") {
		t.Errorf("error should say %q; got: %s", "cannot be forgotten", stderr)
	}
}

// TestForget_BareEntityUIDHintsEntity pins E1: when a bare ULID is passed and
// the forget tool returns E_NOT_FOUND (because it looked it up as an episode)
// but a "get" probe confirms it is an entity, the error message must contain
// "entit" and exit code must be 1.
func TestForget_BareEntityUIDHintsEntity(t *testing.T) {
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	forgetTool := fakeTeamToolFunc{
		name: "forget",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return nil, &mcp.ToolError{Code: mcp.CodeNotFound, Message: "episodes/" + bareUID + " not found; nothing to forget"}
		},
	}
	// The "get" tool returns success, simulating an existing entity.
	getTool := fakeTeamToolFunc{
		name: "get",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"type":"entity","entity":{"uid":"` + bareUID + `"}}`), nil
		},
	}
	stubTeamWiring(t, forgetTool, getTool)

	var errBuf bytes.Buffer
	root := makeForgetRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"forget", bareUID,
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when forgetting an entity uid")
	}
	var se silentError
	if !isSilentError(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
	stderr := errBuf.String()
	if !strings.Contains(strings.ToLower(stderr), "entit") {
		t.Errorf("error should mention entity; got: %s", stderr)
	}
}

// TestForget_NotFoundJSONEmitsErrorEnvelope pins B3: in JSON mode a not-found
// response must emit a {"error":...,"code":...} JSON object to stdout (not
// just write to stderr) so a piped consumer can parse it.
func TestForget_NotFoundJSONEmitsErrorEnvelope(t *testing.T) {
	const bareUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	tool := fakeTeamToolFunc{
		name: "forget",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return nil, mcp.NewNotFoundError("episode not found")
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeForgetRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "json",
		"--data-dir", t.TempDir(),
		"forget", bareUID,
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	var se silentError
	if !isSilentError(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
	raw := outBuf.String()
	if raw == "" {
		t.Fatal("JSON mode: stdout empty; expected a JSON error envelope")
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(raw)), &got); jerr != nil {
		t.Fatalf("JSON mode: stdout not valid JSON: %v\nraw: %q", jerr, raw)
	}
	if _, ok := got["error"]; !ok {
		t.Errorf("JSON error envelope missing 'error' key: %s", raw)
	}
	if _, ok := got["code"]; !ok {
		t.Errorf("JSON error envelope missing 'code' key: %s", raw)
	}
}
