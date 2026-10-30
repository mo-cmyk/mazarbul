package ingest

import (
	"context"
	"fmt"
	"testing"
)

// TestLocalGitSource_PullCommitsBoundedSlices pins the memory-bound fix: a single
// pull returns at most `limit` commits oldest-first and signals more-remain via
// Done=false, so a large backfill is paged limit-at-a-time instead of buffering
// the whole history into memory. The two-phase rev-list/log path must preserve
// chronological order ACROSS slice boundaries and lose no commits.
func TestLocalGitSource_PullCommitsBoundedSlices(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	want := make([]string, 0, 5)
	for i := range 5 {
		want = append(want, commit(t, dir, fmt.Sprintf("commit %d", i)))
	}
	src := NewLocalGitSource(dir, nil, nil)

	const limit = 2
	got := make([]string, 0, len(want))
	wm := Watermark{}
	sawPartialThenMore := false
	for pulls := range 10 {
		batch, err := src.Pull(context.Background(), streamCommits, wm, limit)
		if err != nil {
			t.Fatalf("pull %d: %v", pulls, err)
		}
		if len(batch.Items) > limit {
			t.Fatalf("pull %d returned %d items, want <= %d (slice not bounded)", pulls, len(batch.Items), limit)
		}
		if len(batch.Items) == limit && !batch.Done {
			sawPartialThenMore = true
		}
		for _, it := range batch.Items {
			got = append(got, it.ExternalID)
		}
		if batch.Done {
			break
		}
		wm = Watermark{Cursor: batch.NextCursor, Sequence: batch.NextSequence}
	}

	if !sawPartialThenMore {
		t.Error("never saw a full, not-Done slice — bounding/Done signalling is wrong")
	}
	if len(got) != len(want) {
		t.Fatalf("collected %d commits across slices, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("commit %d = %s, want %s (chronological order broken across slices)", i, got[i], want[i])
		}
	}
}
