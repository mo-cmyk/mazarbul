package doctor

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

func TestWatchFSCheck(t *testing.T) {
	t.Run("not running is healthy", func(t *testing.T) {
		dir := t.TempDir()
		st, detail, err := newWatchFSCheck(dir).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if st != StatusOK {
			t.Errorf("status = %q, want OK (optional feature off); detail=%q", st, detail)
		}
	})

	t.Run("live pidfile reports running", func(t *testing.T) {
		dir := t.TempDir()
		if err := daemon.WritePID(daemon.WatchFSPIDPath(dir), daemon.WatchFSPIDToken, dir); err != nil {
			t.Fatalf("WritePID: %v", err)
		}
		st, detail, err := newWatchFSCheck(dir).Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if st != StatusOK {
			t.Errorf("status = %q, want OK; detail=%q", st, detail)
		}
	})
}
