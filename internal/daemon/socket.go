package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// DefaultHTTPListenAddr is the canonical loopback TCP address the MCP HTTP
// transport binds when a user opts into it. The transport is OFF by
// default (the Unix socket is the trusted local channel); this constant is
// the documented address to enable via `--http-addr` or MAZ_HTTP_ADDR and
// the one rendered in opt-in help text. Loopback-only; external interfaces
// are out of scope.
const DefaultHTTPListenAddr = "127.0.0.1:7842"

// ErrEphemeralHTTPAddr is returned by the daemon CLI when the resolved
// HTTP listen address requests a kernel-assigned port (":0"). The
// daemon needs a discoverable address so `maz daemon status` and
// downstream clients can connect; a kernel-assigned port is bound by
// the child but never plumbed back to the parent's CLI surface, so it
// is effectively invisible. Callers that need ephemeral binding (tests)
// reserve a real port via net.Listen + close before passing it in.
var ErrEphemeralHTTPAddr = errors.New("daemon: MAZ_HTTP_ADDR ephemeral port (:0) is not supported; pick a fixed port")

// ErrNonLoopbackHTTPAddr is returned when the resolved HTTP listen address
// binds a non-loopback interface (e.g. "0.0.0.0:7842" or a routable IP).
// The MCP surface exposes the user's entire memory; binding it anywhere but
// loopback makes the port network-reachable before any middleware runs, so
// the daemon refuses rather than silently widening its attack surface.
var ErrNonLoopbackHTTPAddr = errors.New("daemon: MAZ_HTTP_ADDR must bind a loopback address (127.0.0.0/8, ::1, or localhost); non-loopback binds are refused")

// ValidateHTTPAddr rejects address forms the daemon cannot safely expose.
// Empty is allowed (Unix-socket-only deployments); "host:0" is rejected
// because the bound port is undiscoverable; a non-loopback host is rejected
// because the opt-in TCP transport is loopback-only by design. Returns the
// normalised address or an error.
func ValidateHTTPAddr(addr string) (string, error) {
	if addr == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("daemon: parse MAZ_HTTP_ADDR %q: %w", addr, err)
	}
	if port == "0" {
		return "", fmt.Errorf("%w: got %q", ErrEphemeralHTTPAddr, addr)
	}
	if !isLoopbackBindHost(host) {
		return "", fmt.Errorf("%w: got %q", ErrNonLoopbackHTTPAddr, addr)
	}
	return addr, nil
}

// isLoopbackBindHost reports whether host (the host part of a listen address)
// names the loopback interface: the literal "localhost", or any IP in
// 127.0.0.0/8 or ::1. An empty host ("" from ":7842") binds all interfaces
// and is therefore NOT loopback.
func isLoopbackBindHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DefaultHTTPAddr returns the opt-in TCP listen address for the MCP HTTP
// transport: the value of MAZ_HTTP_ADDR, or "" when it is unset. The TCP
// transport is OFF by default — the Unix socket is the default local
// channel — so an empty result means "no TCP listener". Enabling TCP is an
// explicit act: set MAZ_HTTP_ADDR or pass `--http-addr` to `maz daemon
// start`. Tests use "127.0.0.1:0" for ephemeral, parallel-safe binding.
func DefaultHTTPAddr() string {
	return os.Getenv("MAZ_HTTP_ADDR")
}
