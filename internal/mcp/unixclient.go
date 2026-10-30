package mcp

import (
	"context"
	"net"
	"net/http"
	"time"
)

// unixDialTimeout bounds the connect phase of a request to the daemon socket.
// A healthy local Unix socket connects in well under a millisecond, so this
// never bites a live daemon; it only fast-fails a socket whose listener is
// wedged and not accepting (a missing socket already fails instantly with
// ENOENT). It bounds only the dial, never the request/response, so it cannot
// truncate a legitimately long-lived or streaming call — those stay bounded by
// the caller's own request context.
const unixDialTimeout = 3 * time.Second

// UnixHTTPClient returns an *http.Client whose transport dials the Unix
// socket at socketPath for every request, ignoring the request URL host.
// Local clients use it to reach the daemon over the trusted 0600 socket —
// the default transport now that the TCP listener is off by default — so
// they need no bearer token (the socket's filesystem permissions are the
// credential). The request URL is a placeholder such as "http://unix/mcp";
// only its path is meaningful. A zero timeout leaves the client without a
// deadline, which callers want for long-lived / streaming requests.
func UnixHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: unixDialTimeout}
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: timeout,
	}
}
