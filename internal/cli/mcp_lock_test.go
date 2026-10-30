package cli

import (
	"errors"
	"testing"
)

// TestIsDataDirLocked_DetectsLadybugStatusOne is the regression gate for
// Finding 5: `maz mcp serve` used to emit the raw "failed to open database
// with status 1" chain when the data dir was already locked. isDataDirLocked
// must recognise the lock-held case so callers can surface an actionable hint.
func TestIsDataDirLocked_DetectsLadybugStatusOne(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "status 1 in chain",
			err:  errors.New("daemon: open orchestrator: ladybug: failed to open database with status 1"),
			want: true,
		},
		{
			name: "generic lock message",
			err:  errors.New("failed to open database: lock held"),
			want: true,
		},
		{
			name: "unrelated error",
			err:  errors.New("permission denied"),
			want: false,
		},
		{
			name: "short status 1 variant",
			err:  errors.New("status 1"),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isDataDirLocked(tc.err)
			if got != tc.want {
				t.Errorf("isDataDirLocked(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
