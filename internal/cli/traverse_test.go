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

// makeTraverseRoot wires a minimal cobra root for traverse tests. It includes
// entity, relate, and traverse subcommands so end-to-end seeding works.
func makeTraverseRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newEntityCmd())
	root.AddCommand(newRelateCmd())
	root.AddCommand(newTraverseCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// runTraverseCmd calls `maz traverse <seed>` with optional extra args.
func runTraverseCmd(t *testing.T, dataDir, seed string, extraArgs ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := makeTraverseRoot(&outBuf, &errBuf)
	args := append([]string{"--data-dir", dataDir, "--output", "text", "traverse", seed}, extraArgs...)
	root.SetArgs(args)
	root.SetContext(context.Background())
	execErr := root.Execute()
	return outBuf.String(), errBuf.String(), execErr
}

// TestTraverseReadsCanonicalGraphFile verifies that `maz traverse` resolves an
// entity by display name (using the nav-tool name-resolution path) and returns
// the connected neighbor. It replaces the old test that seeded the graph file
// directly with bare uids that bypassed the SQLite entities projection; the
// new version seeds real entities + a relation so the name-resolving traverse
// can find them.
//
// The documented intent (traverse returns the neighbor end-to-end under the
// new nav-tool path) is preserved; the path-mapping invariant is now enforced
// inside the navigator layer rather than in the test.
func TestTraverseReadsCanonicalGraphFile(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	aliceName := runEntityCreateCmd(t, dir, "person", "Merry Hofmann")
	zurichName := runEntityCreateCmd(t, dir, "place", "Zurich")
	aliceUID := strings.TrimPrefix(aliceName, "entities/")
	zurichUID := strings.TrimPrefix(zurichName, "entities/")

	_ = runRelateCmd(t, dir, aliceUID, "works_at", zurichUID)

	// Traverse by display name — exercises the new name-resolution path.
	out, errOut, execErr := runTraverseCmd(t, dir, "Merry Hofmann")
	if execErr != nil {
		t.Fatalf("traverse Merry Hofmann: %v (stderr=%q)", execErr, errOut)
	}
	if !strings.Contains(out, "Zurich") {
		t.Errorf("traverse did not return neighbor 'Zurich'; stdout=%q stderr=%q", out, errOut)
	}
}

// TestTraverseBogusNameExitsNonZero verifies that traversing an unknown entity
// name exits non-zero with an actionable message.
func TestTraverseBogusNameExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	_, errOut, execErr := runTraverseCmd(t, dir, "DefinitelyNoSuchEntity99xZZ")
	if execErr == nil {
		t.Fatal("traverse with unknown entity expected non-zero exit, got 0")
	}
	if !strings.Contains(errOut, "DefinitelyNoSuchEntity99xZZ") &&
		!strings.Contains(errOut, "not found") &&
		!strings.Contains(errOut, "E_NOT_FOUND") {
		t.Errorf("expected actionable error message for unknown seed; stderr=%q", errOut)
	}
}

// TestTraverseHelp verifies that --help is non-empty and includes examples.
func TestTraverseHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeTraverseRoot(&buf, nil)
	root.SetArgs([]string{"traverse", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("traverse --help produced empty output")
	}
	for _, want := range []string{"Examples:", "maz traverse", "--depth"} {
		if !strings.Contains(out, want) {
			t.Errorf("traverse help missing %q\n%s", want, out)
		}
	}
}

// TestTraverse_NotFoundExitsOne pins B4: when the neighbors tool returns an
// E_NOT_FOUND error (unknown entity seed), traverse must exit with code 1
// (expected failure), not code 2 (unexpected/bug).
func TestTraverse_NotFoundExitsOne(t *testing.T) {
	tool := fakeTeamToolFunc{
		name: "neighbors",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return nil, mcp.NewNotFoundError("entity not found")
		},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeTraverseRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"traverse", "ghost-entity",
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error for not-found entity")
	}
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1 for E_NOT_FOUND", se.code)
	}
}

// TestTraverse_ValidationErrorExitsOne pins B4: an E_VALIDATION response from
// the neighbors tool must exit with code 1, not code 2.
func TestTraverse_ValidationErrorExitsOne(t *testing.T) {
	tool := fakeTeamToolFunc{
		name: "neighbors",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return nil, mcp.NewValidationError("ambiguous name", nil)
		},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeTraverseRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"traverse", "ambiguous-name",
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error for validation failure")
	}
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1 for E_VALIDATION", se.code)
	}
}

// TestTraverse_RepeatedPredicateUnions pins B5: passing --predicate multiple
// times must send all values to the tool (union), not only the last one.
func TestTraverse_RepeatedPredicateUnions(t *testing.T) {
	var capturedReq json.RawMessage
	tool := fakeTeamToolFunc{
		name: "neighbors",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			capturedReq = req
			return json.RawMessage(`{"neighbors":[]}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf bytes.Buffer
	root := makeTraverseRoot(&outBuf, nil)
	root.SetArgs([]string{
		"--output", "text",
		"--data-dir", t.TempDir(),
		"traverse", "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"--predicate", "works_at",
		"--predicate", "reports_to",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("traverse with two predicates: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(capturedReq, &req); err != nil {
		t.Fatalf("captured request not valid JSON: %v (%s)", err, capturedReq)
	}
	preds, ok := req["predicates"]
	if !ok {
		t.Fatal("tool request missing 'predicates' field; both --predicate values must be forwarded")
	}
	predsSlice, ok := preds.([]any)
	if !ok {
		t.Fatalf("predicates field type = %T, want []any", preds)
	}
	if len(predsSlice) != 2 {
		t.Errorf("predicates len = %d, want 2; got %v", len(predsSlice), predsSlice)
	}
	want := map[string]bool{"works_at": true, "reports_to": true}
	for _, p := range predsSlice {
		s, _ := p.(string)
		if !want[s] {
			t.Errorf("unexpected predicate value %q", s)
		}
	}
}
