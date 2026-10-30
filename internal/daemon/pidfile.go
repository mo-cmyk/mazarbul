package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Mazarbul daemon ownership proof. Greptile P1 #107 (security, rounds
// 1–5): we need to decide whether a PID in the PID file is the daemon
// the supervisor spawned, vs. An unrelated live process that recycled
// a stale PID. Three rounds of strengthening:
//
//  1. Substring match on "internal-daemon-serve" — defeated by any
//     process whose argv mentioned the token (grep, vim, log paths).
//  2. argv[0]/argv[1] shape from strings.Fields(`ps -o args=`) —
//     defeated by executable paths containing whitespace, which the
//     ps display string fragments.
//  3. Adjacent-pair walk — defeated by an unrelated `maz` process
//     whose later argv happened to contain "<path>/maz
//     internal-daemon-serve" anywhere (e.g. a long-lived `maz daemon
//     logs --follow /tmp/maz internal-daemon-serve`).
//
// Round 4 (this implementation) does the daemon-owned handshake
// Greptile recommended: every supervisor launch generates a 128-bit
// random token, writes it into the PID file alongside the PID, and
// passes it as `--token <hex>` on the child's argv. `daemon stop`
// reads the token from the PID file and requires the live process's
// `ps -o args=` output to END with " --token <hex>" — an unforgeable
// per-instance secret. Combined with comm-basename == "maz", the
// only process that can satisfy the check is one that was spawned by
// our supervisor with this exact token. A user who can read the PID
// file (0600 in $TMPDIR / $XDG_RUNTIME_DIR) already has full
// privilege to kill the daemon by other means; the threat model is
// PID reuse and confused-deputy, not a hostile peer with the same
// uid.
const (
	daemonExecutableName = "maz"
	daemonSubcommand     = "internal-daemon-serve"
	daemonTokenFlag      = "--token"
)

// isMazDaemonPID is a package-level seam so tests can stub the OS-level
// identity check. Production callers go through IsMazDaemon.
//
//nolint:gochecknoglobals // test seam for the OS-level identity probe; swap via SetIsMazDaemonForTest
var isMazDaemonPID = checkMazDaemonPIDViaPS

// IsMazDaemon reports whether pid corresponds to a live Mazarbul
// daemon launched with token. Returns false for dead PIDs, unrelated
// live PIDs (post-reuse), processes whose argv does not end in
// "--token <token>", and ALL token-less calls. The empty-token case
// is treated as unowned/stale rather than falling back to a weaker
// adjacency check (Greptile P1 #107 round 6 security): an unrelated
// `maz` process can satisfy the adjacency proof, so a tokenless PID
// file is unsafe to act on. Safe to call on PIDs we do not own.
func IsMazDaemon(pid int, token string) bool {
	if !processAlive(pid) {
		return false
	}
	return isMazDaemonPID(pid, token)
}

// checkMazDaemonPIDViaPS proves daemon ownership via two `ps`
// projections. When a non-empty token is supplied it is the
// authoritative signal; otherwise we fall back to a weaker
// comm+adjacency check for backward compatibility with token-less PID
// files.
//
// Layer 1 — executable identity. `ps -o comm=` prints the executable
// path verbatim (not an argv-style display, so a path containing
// whitespace survives intact). Filepath.Base() collapses any path
// prefix and we compare against daemonExecutableName. This single
// layer rejects grep, vim, mazd, and every non-maz process whose argv
// merely mentions the daemon subcommand or a token-looking string.
//
// Layer 2 — token suffix (when token != ""). `ps -o args=` is the
// argv joined by single spaces. We require the trimmed output to end
// with " --token <TOKEN>". Because the supervisor passes the token as
// the LAST argv element and the token is 128 bits of crypto/rand
// entropy minted per supervisor launch, no unrelated process — even
// another `maz` invocation — can have this exact trailing suffix
// without having been spawned by us with this exact token. The token
// is unforgeable in the threat model (PID reuse, confused-deputy)
// because an unrelated process cannot know it.
//
// Layer 2 fallback (when token == ""). The adjacent-pair walk from
// the round-3 check stays in place for PID files written before
// tokenisation. Acceptable migration risk: a redeployed token-bearing
// daemon overwrites the legacy file on first start, restoring the
// strong path.
func checkMazDaemonPIDViaPS(pid int, token string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	commOut, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output() // #nosec G204 -- pid is an int formatted via strconv.Itoa, not user-supplied text
	if err != nil {
		return false
	}
	if filepath.Base(strings.TrimSpace(string(commOut))) != daemonExecutableName {
		return false
	}
	argsOut, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output() // #nosec G204 -- pid is an int formatted via strconv.Itoa, not user-supplied text
	if err != nil {
		return false
	}
	return matchesDaemonArgv(string(argsOut), token)
}

// matchesDaemonArgv tests whether the `ps -o args=` output names a
// Mazarbul daemon launched with the given token. The token must appear
// as a delimited "--token <token>" flag pair: either the final argument,
// or immediately followed by a further flag. The daemon child appends
// "--http-addr <addr>" after "--token" (see buildDaemonChildArgs), so a
// strict trailing-suffix check would wrongly reject every daemon started
// with an HTTP listener — making `daemon status` report "stale" and
// `daemon stop` orphan a healthy process.
// Exported via MatchesDaemonArgvForTest for table-tests.
//
// An empty token never matches: Greptile P1 #107 round 6 (security)
// — the previous adjacent-pair fallback was satisfiable by an
// unrelated `maz` process whose argv happened to contain a
// `<...>/maz internal-daemon-serve` pair (e.g. `maz daemon logs
// --follow /tmp/maz internal-daemon-serve`). Requiring the per-launch
// 128-bit random token as a delimited pair keeps that property: an argv
// lacking the secret token never matches, regardless of trailing args.
// Treating tokenless PID files as unowned forces `daemon stop` to clean
// them up rather than signal, which is the safe failure mode. Token-
// bearing daemons always overwrite a legacy file on their next start, so
// the "tokenless live daemon" case cannot persist.
func matchesDaemonArgv(raw, token string) bool {
	if token == "" {
		return false
	}
	trimmed := strings.TrimRight(raw, " \t\r\n")
	pair := " " + daemonTokenFlag + " " + token
	// Final argument, or followed by a space + further flags (e.g.
	// "--http-addr <addr>"). The trailing-space form prevents a token
	// that is merely a prefix of a longer argv token from matching.
	return strings.HasSuffix(trimmed, pair) || strings.Contains(trimmed, pair+" ")
}

// SetIsMazDaemonForTest swaps the identity probe and returns the
// previous function so tests can defer restoration. Test-only.
func SetIsMazDaemonForTest(fn func(int, string) bool) func(int, string) bool {
	prev := isMazDaemonPID
	isMazDaemonPID = fn
	return prev
}

// MatchesDaemonArgvForTest exposes the argv shape parser to the
// external _test package so the strict-shape behaviour can be
// table-tested without spawning processes. Test-only.
func MatchesDaemonArgvForTest(raw, token string) bool {
	return matchesDaemonArgv(raw, token)
}

// ErrAlreadyRunning is returned by WritePID when the PID file exists and
// the recorded PID still belongs to a live process per kill -0 semantics.
var ErrAlreadyRunning = errors.New("daemon: already running")

// PIDInfo is the result of reading a PID file. Running reflects a
// kill -0 probe on the recorded PID; the field is honest about
// staleness so the supervisor can decide what to do. Token is the
// per-launch ownership secret the supervisor wrote alongside the PID;
// empty when the file is in the legacy single-line format. DataDir is
// the kernel store the daemon serves (line 3); empty for legacy files
// written before the data dir was recorded, which lets a CLI command
// decide whether the running daemon actually owns the --data-dir it
// was asked to operate on.
type PIDInfo struct {
	PID     int
	Running bool
	Token   string
	DataDir string
}

// DefaultPath returns the canonical PID-file path for this platform:
//  1. $MAZ_PIDFILE                   (explicit override; matches the MAZ_* convention in internal/config)
//  2. $XDG_RUNTIME_DIR/mazarbul.pid  (Linux desktops with systemd)
//  3. $TMPDIR/mazarbul.pid           (macOS sets TMPDIR per session)
//  4. /tmp/mazarbul.pid              (final fallback)
func DefaultPath() string {
	if v := os.Getenv("MAZ_PIDFILE"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "mazarbul.pid")
	}
	if v := os.Getenv("TMPDIR"); v != "" {
		return filepath.Join(v, "mazarbul.pid")
	}
	return "/tmp/mazarbul.pid"
}

// DataDirPIDPath returns the PID file path scoped to a specific data directory.
// Using a per-data-dir path prevents isolated daemon instances from colliding
// on the same global PID file, which causes mis-detection when running multiple
// daemon instances simultaneously (e.g. an eval harness and the production
// daemon). When MAZ_PIDFILE is set, that explicit override still wins so tests
// can drive the path precisely.
//
// The PID file is written inside the data directory at "mazarbul.pid" so it is
// co-located with the kernel store it serves. The data directory must exist
// before the daemon writes its PID file (WritePID is called only after the
// kernel store opens successfully).
func DataDirPIDPath(dataDir string) string {
	if v := os.Getenv("MAZ_PIDFILE"); v != "" {
		return v
	}
	return filepath.Join(dataDir, "mazarbul.pid")
}

// WatchFSPIDToken is the ownership token written into the filesystem
// watcher's PID file. It distinguishes a watch-fs PID file from the
// daemon's (which carries a per-launch token) for any caller inspecting
// the file's identity.
const WatchFSPIDToken = "watch-fs"

// WatchFSPIDPath returns the PID file path for the maz watch-fs process,
// scoped to a data directory so it never collides with the daemon's
// mazarbul.pid in the same store. MAZ_PIDFILE does NOT override this:
// that variable targets the daemon, and the watcher keeps a distinct
// path so the two liveness probes stay independent.
func WatchFSPIDPath(dataDir string) string {
	return filepath.Join(dataDir, "watch-fs.pid")
}

// WritePID writes the current process PID, the per-launch ownership
// token, and the served data dir to path with mode 0600. The file
// format is "<PID>\n<TOKEN>\n<DATADIR>\n". The token line is always
// emitted when dataDir is non-empty so the data dir is unambiguously
// line 3 even for an empty token; dataDir may be empty, in which case
// the third line is omitted (and ReadPID returns DataDir=="").
//
// It returns ErrAlreadyRunning when path exists, the recorded PID is
// live, AND the PID's process passes the daemon-ownership probe for
// the file's token. A stale file (PID gone, contents unreadable,
// contents malformed, recycled PID owned by an unrelated process, or
// anything other than a regular file such as a symlink) is unlinked
// and replaced with a fresh 0600 regular file.
//
// Security: the previous implementation used os.WriteFile, which (a)
// only applies the requested mode when creating a new inode (so an
// existing 0644 file would stay world-readable after a stale overwrite)
// and (b) follows symlinks (so a pre-planted symlink at the predictable
// /tmp/mazarbul.pid path could redirect the truncate to an unrelated
// file). Unlinking first and creating with O_EXCL avoids both. The
// unlink targets the symlink itself, not its target.
func WritePID(path, token, dataDir string) error {
	// Require liveness AND token-bearing maz-daemon identity to
	// declare the existing file's owner alive. Bare liveness would
	// refuse to claim a PID file whose stale PID has been recycled by
	// an unrelated user process (editor, shell), permanently blocking
	// daemon start until manual cleanup. IsMazDaemon confirms the
	// live PID is actually OUR daemon for this token before honoring
	// the claim.
	if info, err := ReadPID(path); err == nil && info.Running && IsMazDaemon(info.PID, info.Token) {
		return fmt.Errorf("%w: pid %d holds %s", ErrAlreadyRunning, info.PID, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: clear stale PID file %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- caller-supplied PID file path is the package's input; O_EXCL rejects races
	if err != nil {
		return fmt.Errorf("daemon: create PID file %s: %w", path, err)
	}
	body := strconv.Itoa(os.Getpid()) + "\n"
	// Emit the token line whenever a data dir follows, so the data dir
	// is unambiguously line 3 even when token is empty.
	if token != "" || dataDir != "" {
		body += token + "\n"
	}
	if dataDir != "" {
		body += dataDir + "\n"
	}
	if _, werr := f.WriteString(body); werr != nil {
		cerr := f.Close()
		rerr := os.Remove(path)
		return fmt.Errorf("daemon: write PID file %s: %w", path, errors.Join(werr, cerr, rerr))
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("daemon: close PID file %s: %w", path, err)
	}
	return nil
}

// ReadPID reads path and returns the parsed PID, the recorded
// ownership token (empty for legacy single-line files), and a
// liveness flag. The file format is "<PID>\n<TOKEN>\n"; bodies with
// only the first line are accepted for backward compatibility with
// pre-token daemons. A missing file, a non-regular file (symlink,
// fifo, device), an unreadable file, or malformed contents return an
// error; callers must not assume the file is well-formed. Refusing
// non-regular files prevents a planted symlink at the predictable
// PID-file path from causing the caller to inadvertently read
// another file's contents.
func ReadPID(path string) (PIDInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return PIDInfo{}, fmt.Errorf("daemon: stat PID file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return PIDInfo{}, fmt.Errorf("daemon: PID file %s is not a regular file (mode %s); refusing", path, fi.Mode())
	}
	data, err := os.ReadFile(path) // #nosec G304 -- caller-supplied path; symlinks and other non-regular entries rejected above
	if err != nil {
		return PIDInfo{}, fmt.Errorf("daemon: read PID file %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return PIDInfo{}, fmt.Errorf("daemon: malformed PID file %s: %w", path, err)
	}
	if pid <= 0 {
		return PIDInfo{}, fmt.Errorf("daemon: malformed PID file %s: non-positive pid %d", path, pid)
	}
	token := ""
	if len(lines) >= 2 {
		token = strings.TrimSpace(lines[1])
	}
	dataDir := ""
	if len(lines) >= 3 {
		// A data dir is a filesystem path: leading/trailing spaces are valid
		// path characters, so trim only a Windows CR line artifact rather than
		// TrimSpace, which would silently rewrite the path and let
		// daemonServesDataDir mismatch a daemon-held store.
		dataDir = strings.TrimSuffix(lines[2], "\r")
	}
	return PIDInfo{PID: pid, Running: processAlive(pid), Token: token, DataDir: dataDir}, nil
}

// DeletePID removes path. A missing file is not an error.
func DeletePID(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("daemon: delete PID file %s: %w", path, err)
}

// processAlive reports whether pid corresponds to a process the caller
// can address with a signal (kill -0 semantics on Unix). EPERM means the
// process exists but is owned by a different uid, which we still treat
// as alive; ESRCH and anything else means dead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true
	case errors.Is(err, syscall.EPERM):
		return true
	default:
		return false
	}
}
