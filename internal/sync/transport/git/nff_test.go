package git

import (
	"errors"
	"testing"
)

// TestIsNonFastForward distinguishes a genuine non-fast-forward race (retryable)
// from a permanent server-side rejection (must fail fast). A permanent refusal —
// pre-receive hook, protected branch, permission denial — also contains the word
// "rejected", so matching that bare substring would send it into the retry loop,
// exhaust the attempts, reset the mirror, and mislabel it as push contention.
func TestIsNonFastForward(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{
			"non-fast-forward race",
			errors.New("! [rejected]        main -> main (non-fast-forward)\nerror: failed to push some refs"),
			true,
		},
		{
			"fetch first race",
			errors.New("! [rejected]        main -> main (fetch first)\nhint: Updates were rejected because the remote contains work"),
			true,
		},
		{
			// Concurrent pushes to a shared (file://) remote lose the atomic
			// ref update — a race a rebase-and-retry resolves, phrased as a
			// remote rejection rather than "(non-fast-forward)".
			"cannot lock ref race",
			errors.New("remote: error: cannot lock ref 'refs/heads/main': is at aaa but expected bbb\n! [remote rejected] HEAD -> main (failed to update ref)"),
			true,
		},
		{
			"pre-receive hook declined",
			errors.New("! [remote rejected] main -> main (pre-receive hook declined)\nerror: failed to push some refs"),
			false,
		},
		{
			"protected branch hook declined",
			errors.New("! [remote rejected] main -> main (protected branch hook declined)"),
			false,
		},
		{
			"permission denied",
			errors.New("! [remote rejected] main -> main (permission denied)"),
			false,
		},
		{
			"generic network failure",
			errors.New("fatal: unable to access remote: Connection timed out"),
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isNonFastForward(tc.err); got != tc.want {
				t.Errorf("isNonFastForward(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
