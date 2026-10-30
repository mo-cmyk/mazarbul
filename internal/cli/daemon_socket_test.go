package cli

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// TestDaemonSocketPath_AgreesWithDoctorReader is the regression gate for
// the doctor "mcp.unix socket not present" false negative: the daemon
// supervisor was binding its Unix socket at $TMPDIR/mazarbul.sock (the
// old daemon.DefaultSocketPath path), while doctor stat'd
// <dataDir>/mazarbul.sock (mcp.DefaultUnixSocket). A healthy daemon
// looked broken to the doctor probe because writer and reader did not
// agree on the canonical path.
//
// The canonical path is mcp.DefaultUnixSocket(dataDir): doctor and the
// `maz mcp serve` client both read from there, and the dataDir tree is
// where Mazarbul's other state lives (event log, sqlite, ladybug). The
// daemon supervisor's UnixSocket must agree.
func TestDaemonSocketPath_AgreesWithDoctorReader(t *testing.T) {
	t.Parallel()
	for _, dataDir := range []string{
		"/tmp/maz-test-1",
		"/Users/x/Library/Application Support/Mazarbul",
		"/var/lib/mazarbul",
	} {
		t.Run(dataDir, func(t *testing.T) {
			t.Parallel()
			if got, want := daemonSocketPath(dataDir), mcp.DefaultUnixSocket(dataDir); got != want {
				t.Errorf("daemonSocketPath(%q) = %q, want %q\n"+
					"writer and doctor reader must agree on the canonical Unix-socket path",
					dataDir, got, want)
			}
		})
	}
}
