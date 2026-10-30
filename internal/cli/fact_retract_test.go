package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runFactSubcmd drives a `maz fact <sub> ...` invocation against dataDir and
// returns trimmed stdout, failing on a non-zero exit.
func runFactSubcmd(t *testing.T, dataDir string, args ...string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newFactCmd())
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--output", "text", "--data-dir", dataDir, "fact"}, args...))
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("fact %v: %v (stderr=%q)", args, err, errBuf.String())
	}
	return strings.TrimSpace(outBuf.String())
}

// runForgetCmd drives the top-level `maz forget <arg>` against dataDir.
func runForgetCmd(t *testing.T, dataDir string, arg string) string {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newForgetCmd())
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--output", "text", "--data-dir", dataDir, "forget", arg})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("forget %q: %v (stderr=%q)", arg, err, errBuf.String())
	}
	return strings.TrimSpace(outBuf.String())
}

// TestFactRetractEmptiesSlot is the #2 end-to-end: after a slot changes value
// (pending → active), retracting the open value drops it from current reads and
// the slot reads as EMPTY — it does NOT fall back to the prior value. (The
// prior value's history is still walkable via get(facts/{uid}).)
func TestFactRetractEmptiesSlot(t *testing.T) {
	dir := t.TempDir()
	seedFactTestConfig(t, dir) // embed=none; skips under -short

	subjName := runEntityCreateCmd(t, dir, "person", "Retract Subject")
	uid := strings.TrimPrefix(subjName, "entities/")

	runFactSetCmd(t, dir, uid, "state.task.status", "pending")
	runFactSetCmd(t, dir, uid, "state.task.status", "active")

	// Sanity: the open value before retract is "active".
	if got := runFactSubcmd(t, dir, "get", uid); !strings.Contains(got, "active") {
		t.Fatalf("pre-retract slot should show active, got %q", got)
	}

	// Retract the open value.
	out := runFactSubcmd(t, dir, "retract", uid, "state.task.status")
	if !strings.Contains(out, "retracted") || !strings.Contains(out, "facts/") {
		t.Fatalf("retract output = %q, want 'retracted facts/{uid}'", out)
	}

	// The slot is now empty: neither the retracted value nor the prior one is current.
	got := runFactSubcmd(t, dir, "get", uid)
	if strings.Contains(got, "active") {
		t.Errorf("retracted value 'active' should not be a current fact; got %q", got)
	}
	if strings.Contains(got, "pending") {
		t.Errorf("slot should be empty after retract, not reverted to 'pending'; got %q", got)
	}
}

// TestFactRetractEmptySlotExitsOne retracting a slot with no open value is a
// clean bad-input exit (E_NOT_FOUND → exit 1), not a crash.
func TestFactRetractEmptySlotExitsOne(t *testing.T) {
	dir := t.TempDir()
	seedFactTestConfig(t, dir)

	subjName := runEntityCreateCmd(t, dir, "person", "Empty Slot Subject")
	uid := strings.TrimPrefix(subjName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newFactCmd())
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"--output", "text", "--data-dir", dir, "fact", "retract", uid, "state.never.set"})
	root.SetContext(context.Background())
	if err := root.Execute(); err == nil {
		t.Fatalf("retract of an empty slot expected to fail but exited 0 (stdout=%q)", outBuf.String())
	}
}

// TestForgetFactHardDeletes is the #2 hard-delete end-to-end: `maz forget
// facts/{uid}` removes the fact row; the slot is empty afterward.
func TestForgetFactHardDeletes(t *testing.T) {
	dir := t.TempDir()
	seedFactTestConfig(t, dir)

	subjName := runEntityCreateCmd(t, dir, "person", "Forget Subject")
	uid := strings.TrimPrefix(subjName, "entities/")

	factName := runFactSetCmd(t, dir, uid, "preference.coffee.milk", "oat")
	if !strings.HasPrefix(factName, "facts/") {
		t.Fatalf("fact set → want facts/{uid}, got %q", factName)
	}

	out := runForgetCmd(t, dir, factName)
	if !strings.Contains(out, "forgotten") || !strings.Contains(out, factName) {
		t.Fatalf("forget output = %q, want 'forgotten %s'", out, factName)
	}

	// The slot is empty after a hard delete.
	got := runFactSubcmd(t, dir, "get", uid)
	if strings.Contains(got, "oat") {
		t.Errorf("forgotten fact value 'oat' should be gone; got %q", got)
	}
}
