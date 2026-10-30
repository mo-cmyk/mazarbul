package mcp

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// Security response headers applied to every response on both the HTTP
// and Unix transports. The MCP surface serves JSON, not HTML, so these
// have no effect on current clients; they are a fail-safe foundation for
// any future HTML/browser surface (a viewer, an OAuth callback) so that
// such a surface cannot be framed, sniffed, or allowed to load
// sub-resources by default. Tightening here later is a no-op for the
// JSON API.
const (
	cspValue            = "default-src 'none'; frame-ancestors 'none'; sandbox"
	contentTypeNosniff  = "nosniff"
	referrerPolicyValue = "no-referrer"
)

// securityHeaders wraps next so every response carries a strict
// Content-Security-Policy and related hardening headers. Applied to both
// transports.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", cspValue)
		h.Set("X-Content-Type-Options", contentTypeNosniff)
		h.Set("Referrer-Policy", referrerPolicyValue)
		next.ServeHTTP(w, r)
	})
}

// hardenTCP wraps next with the TCP-transport security gate. The TCP
// listener is reachable by any local process and, via DNS rebinding,
// by a malicious web page pointed at 127.0.0.1, so it requires:
//
//  1. A loopback Host allow-list. A DNS-rebinding attack reaches the
//     port with the attacker's hostname in the Host header (e.g.
//     "attacker.example" resolved to 127.0.0.1); rejecting any
//     non-loopback Host defeats it.
//  2. A loopback Origin allow-list. Browsers attach Origin on
//     cross-origin requests; a non-loopback Origin is a browser-driven
//     cross-site attempt and is rejected. A missing Origin (ordinary
//     non-browser clients) is permitted.
//  3. A constant-time bearer-token check. A browser/rebinding attacker
//     cannot read the 0600 token file or the process table, so the token
//     is the credential that distinguishes a legitimate local client
//     from a hostile web page that merely shares the loopback address.
//
// The health endpoint is exempt from the token — it carries no sensitive
// data and must stay trivially probeable — but is still subject to the
// Host/Origin allow-list. Token must be non-empty; Options.validate
// guarantees that whenever the TCP transport is enabled. The Unix socket
// is not wrapped with this gate: its 0600 permissions are the credential.
func hardenTCP(next http.Handler, token string, log *zap.Logger) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHostport(r.Host) {
			log.Warn("mcp http: rejected request with non-loopback Host", zap.String("host", r.Host))
			http.Error(w, "forbidden: non-loopback Host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
			log.Warn("mcp http: rejected request with disallowed Origin", zap.String("origin", origin))
			http.Error(w, "forbidden: disallowed Origin", http.StatusForbidden)
			return
		}
		if r.URL.Path != HealthPath && !bearerTokenValid(r.Header.Get("Authorization"), want) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerTokenValid reports whether authHeader is a "Bearer <token>"
// credential equal to want. The scheme is matched case-insensitively per
// RFC 7235; the token bytes are compared in constant time so a mismatch
// reveals nothing about how many leading bytes matched.
func bearerTokenValid(authHeader string, want []byte) bool {
	// Fail closed on an empty expected token: ConstantTimeCompare("", "")
	// returns 1, so an empty want would accept "Authorization: Bearer "
	// (empty credential) and bypass the gate. Options.validate already
	// forbids an empty Token whenever the TCP transport is enabled; this
	// guard keeps bearerTokenValid correct on its own, independent of the
	// call site, should a future caller skip that check.
	if len(want) == 0 {
		return false
	}
	scheme, tok, ok := strings.Cut(authHeader, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), want) == 1
}

// isLoopbackOrigin reports whether origin (an "Origin" header value, e.g.
// "http://localhost:7842") has a loopback host. A malformed or hostless
// origin is rejected.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHostport(u.Host)
}

// isLoopbackHostport reports whether hostport (a Host header or URL host,
// with or without a :port and with or without IPv6 brackets) names the
// loopback interface: the literal "localhost", any 127.0.0.0/8 address,
// or ::1.
func isLoopbackHostport(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
