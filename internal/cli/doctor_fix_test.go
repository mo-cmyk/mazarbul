package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	clistatus "github.com/mo-cmyk/mazarbul/internal/cli/status"
	"github.com/mo-cmyk/mazarbul/internal/doctor"
)

// failResult builds a StatusFail Result at index for a failing check.
func failResult(index int, name, detail string) doctor.Result {
	return doctor.Result{Index: index, Name: name, Subsystem: "storage", Status: doctor.StatusFail, Detail: detail}
}

// TestApplyDoctorFixes_YesAppliesSafe: --yes applies a safe remediation without
// prompting, and verifyFix re-runs the check (now passing) to confirm.
func TestApplyDoctorFixes_YesAppliesSafe(t *testing.T) {
	t.Parallel()
	var applied int
	fixed := false
	checks := []doctor.Check{
		doctor.CheckFunc{
			N: "identity.self_principal_link", Sub: "identity",
			Fn: func(context.Context) (doctor.Status, string, error) {
				if fixed {
					return doctor.StatusOK, "linked", nil
				}
				return doctor.StatusFail, "missing", nil
			},
			Rem: &doctor.Remediation{
				Description: "link self to principal",
				Safe:        true,
				Apply:       func(context.Context) error { applied++; fixed = true; return nil },
			},
		},
	}
	failing := []doctor.Result{failResult(0, "identity.self_principal_link", "missing")}

	var buf bytes.Buffer
	tally := applyDoctorFixes(context.Background(), &buf, clistatus.New(&buf, false), checks, failing, true)

	if applied != 1 {
		t.Fatalf("Apply called %d times, want 1", applied)
	}
	if tally.applied != 1 || tally.unresolved() != 0 {
		t.Fatalf("tally = %+v, want applied=1 unresolved=0", tally)
	}
	if !strings.Contains(buf.String(), "fixed") {
		t.Errorf("output should confirm the fix: %q", buf.String())
	}
}

// TestApplyDoctorFixes_YesFlagsUnsafe: --yes NEVER auto-applies an unsafe
// remediation (the migration/audit/permission safety rule). It is flagged.
func TestApplyDoctorFixes_YesFlagsUnsafe(t *testing.T) {
	t.Parallel()
	var applied int
	checks := []doctor.Check{
		doctor.CheckFunc{
			N: "storage.migration", Sub: "storage",
			Fn: func(context.Context) (doctor.Status, string, error) {
				return doctor.StatusFail, "migration 0006 not applied", nil
			},
			Rem: &doctor.Remediation{
				Description: "rebuild from the event log",
				Safe:        false, // migration repairs are never auto-fixed
				Apply:       func(context.Context) error { applied++; return nil },
			},
		},
	}
	failing := []doctor.Result{failResult(0, "storage.migration", "migration 0006 not applied")}

	var buf bytes.Buffer
	tally := applyDoctorFixes(context.Background(), &buf, clistatus.New(&buf, false), checks, failing, true)

	if applied != 0 {
		t.Fatalf("unsafe Apply called %d times under --yes, want 0 (never auto-fixed)", applied)
	}
	if tally.flagged != 1 || tally.applied != 0 {
		t.Fatalf("tally = %+v, want flagged=1 applied=0", tally)
	}
	if !strings.Contains(buf.String(), "flagged") {
		t.Errorf("output should flag the unsafe remediation: %q", buf.String())
	}
}

// TestApplyDoctorFixes_NonRemediableGuidance: a failing check with no
// remediation prints manual guidance and is never auto-fixed.
func TestApplyDoctorFixes_NonRemediableGuidance(t *testing.T) {
	t.Parallel()
	checks := []doctor.Check{
		doctor.CheckFunc{
			N: "datadir.writable", Sub: "storage",
			Fn: func(context.Context) (doctor.Status, string, error) {
				return doctor.StatusFail, "permission denied", nil
			},
			// no Rem -> non-remediable
		},
	}
	failing := []doctor.Result{failResult(0, "datadir.writable", "permission denied")}

	var buf bytes.Buffer
	tally := applyDoctorFixes(context.Background(), &buf, clistatus.New(&buf, false), checks, failing, true)

	if tally.guidance != 1 {
		t.Fatalf("tally = %+v, want guidance=1", tally)
	}
	if !strings.Contains(buf.String(), "no automatic fix") {
		t.Errorf("output should print manual guidance: %q", buf.String())
	}
}

// TestApplyDoctorFixes_NoYesSkipsNonInteractive: without --yes and with no TTY
// (go test), the confirm short-circuits to "no" and the safe fix is skipped,
// never auto-applied. This is the confirm-before-log-write guarantee.
func TestApplyDoctorFixes_NoYesSkipsNonInteractive(t *testing.T) {
	t.Parallel()
	var applied int
	checks := []doctor.Check{
		doctor.CheckFunc{
			N: "identity.self_principal_link", Sub: "identity",
			Fn: func(context.Context) (doctor.Status, string, error) {
				return doctor.StatusFail, "missing", nil
			},
			Rem: &doctor.Remediation{
				Description: "link self to principal",
				Safe:        true,
				Apply:       func(context.Context) error { applied++; return nil },
			},
		},
	}
	failing := []doctor.Result{failResult(0, "identity.self_principal_link", "missing")}

	var buf bytes.Buffer
	tally := applyDoctorFixes(context.Background(), &buf, clistatus.New(&buf, false), checks, failing, false)

	if applied != 0 {
		t.Fatalf("Apply called %d times without --yes in non-interactive mode, want 0", applied)
	}
	if tally.skipped != 1 {
		t.Fatalf("tally = %+v, want skipped=1", tally)
	}
}

// TestApplyDoctorFixes_AppliedButStillFailing: a remediation whose Apply
// returns nil but does not actually clear the failure is counted as failed,
// because verifyFix re-runs the single check and still sees StatusFail. This
// keeps the exit code honest when a "fix" silently fails to take.
func TestApplyDoctorFixes_AppliedButStillFailing(t *testing.T) {
	t.Parallel()
	var applied int
	checks := []doctor.Check{
		doctor.CheckFunc{
			N: "stubborn.check", Sub: "storage",
			Fn: func(context.Context) (doctor.Status, string, error) {
				return doctor.StatusFail, "still broken", nil // never recovers
			},
			Rem: &doctor.Remediation{
				Description: "attempt repair",
				Safe:        true,
				Apply:       func(context.Context) error { applied++; return nil },
			},
		},
	}
	failing := []doctor.Result{failResult(0, "stubborn.check", "still broken")}

	var buf bytes.Buffer
	tally := applyDoctorFixes(context.Background(), &buf, clistatus.New(&buf, false), checks, failing, true)

	if applied != 1 {
		t.Fatalf("Apply called %d times, want 1", applied)
	}
	if tally.failed != 1 || tally.unresolved() != 1 {
		t.Fatalf("tally = %+v, want failed=1 unresolved=1", tally)
	}
	if !strings.Contains(buf.String(), "still failing") {
		t.Errorf("output should report the still-failing check: %q", buf.String())
	}
}

// TestFailingResults filters to StatusFail and preserves registration order.
func TestFailingResults(t *testing.T) {
	t.Parallel()
	in := []doctor.Result{
		{Index: 0, Status: doctor.StatusOK},
		{Index: 1, Status: doctor.StatusFail, Name: "a"},
		{Index: 2, Status: doctor.StatusWarn},
		{Index: 3, Status: doctor.StatusFail, Name: "b"},
	}
	got := failingResults(in)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("failingResults = %+v, want [a b]", got)
	}
}
