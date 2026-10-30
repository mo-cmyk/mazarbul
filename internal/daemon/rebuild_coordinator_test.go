package daemon

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// TestRebuildCoordinatorOneAtATime pins the single-rebuild invariant: while a
// rebuild is running, start() refuses (started=false) so the admin endpoint can
// map that to 409.
func TestRebuildCoordinatorOneAtATime(t *testing.T) {
	t.Parallel()
	c := newRebuildCoordinator(context.Background(), zap.NewNop())

	// Simulate an in-flight rebuild without spawning the job goroutine.
	c.running = true

	jobUID, started := c.start(nil, t.TempDir(), rebuildKindRebuild, false)
	if started {
		t.Fatalf("start returned started=true while a rebuild is running; want 409")
	}
	if jobUID != "" {
		t.Fatalf("start returned jobUID %q while busy; want empty", jobUID)
	}
}

// TestRebuildCoordinatorStatusTransitions verifies the lifecycle setters only
// mutate the status that matches the active jobUID, so a stale setter from a
// previous job cannot clobber the current one's status.
func TestRebuildCoordinatorStatusTransitions(t *testing.T) {
	t.Parallel()
	c := newRebuildCoordinator(context.Background(), zap.NewNop())
	c.status = testStatus("job1", rebuildKindReindex)

	t.Run("matching jobUID advances state", func(t *testing.T) {
		c.setState("job1", rebuildStateSwapping, "swapping")
		if got := c.statusView().State; got != string(rebuildStateSwapping) {
			t.Fatalf("state = %q; want swapping", got)
		}
	})

	t.Run("stale jobUID is ignored", func(t *testing.T) {
		c.setSucceeded("ghost")
		if got := c.statusView().State; got != string(rebuildStateSwapping) {
			t.Fatalf("stale setSucceeded mutated state to %q; want swapping unchanged", got)
		}
	})

	t.Run("matching jobUID succeeds", func(t *testing.T) {
		c.setSucceeded("job1")
		st := c.statusView()
		if st.State != string(rebuildStateSucceeded) {
			t.Fatalf("state = %q; want succeeded", st.State)
		}
		if st.Progress != 1 {
			t.Fatalf("progress = %v; want 1", st.Progress)
		}
	})

	t.Run("failed records error", func(t *testing.T) {
		c.status = testStatus("job2", rebuildKindRebuild)
		c.setFailed("job2", errors.New("boom"))
		st := c.statusView()
		if st.State != string(rebuildStateFailed) {
			t.Fatalf("state = %q; want failed", st.State)
		}
		if st.Error != "boom" {
			t.Fatalf("error = %q; want boom", st.Error)
		}
	})
}

// TestRebuildCoordinatorProgressCapped verifies the replay JobReporter caps
// fractional progress below 1.0 so the terminal 1.0 is reserved for a committed
// swap — a fast replay never reads as "done" before the swap runs.
func TestRebuildCoordinatorProgressCapped(t *testing.T) {
	t.Parallel()
	c := newRebuildCoordinator(context.Background(), zap.NewNop())
	c.status = testStatus("job1", rebuildKindRebuild)

	rep, _ := c.Begin(context.Background(), "rebuild", "Rebuild")
	rep.Update(1.0, "replaying")
	if got := c.statusView().Progress; got > 0.95 {
		t.Fatalf("progress = %v; want capped <= 0.95", got)
	}

	// Done/Fail on the replay reporter are inert: the coordinator owns terminal
	// state (set only after the swap).
	rep.Done()
	if got := c.statusView().State; got == string(rebuildStateSucceeded) {
		t.Fatalf("reporter.Done marked the coordinator succeeded; want unchanged")
	}
}

// testStatus builds a building-state status snapshot for the given job, the
// starting point the lifecycle setters mutate from.
func testStatus(jobUID string, kind rebuildKind) rebuildStatusView {
	return rebuildStatusView{JobUID: jobUID, Kind: string(kind), State: string(rebuildStateBuilding)}
}
