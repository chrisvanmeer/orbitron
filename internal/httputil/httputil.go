// Package httputil holds request helpers shared between the server (API) and
// the web dashboard. Both packages cannot import each other, so small
// common helpers live here.
package httputil

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the best-effort client IP for a request. When Orbitron runs
// behind a reverse proxy (e.g. nginx) the proxy's address is the only thing in
// r.RemoteAddr, so the proxy headers are honored: X-Real-IP first (nginx sets
// it to the actual client), then the first (leftmost, outermost) entry of the
// X-Forwarded-For chain, falling back to r.RemoteAddr with its port stripped
// so the logged value is a plain address in all cases.
func ClientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}

	if ff := r.Header.Get("X-Forwarded-For"); ff != "" {
		if i := strings.Index(ff, ","); i >= 0 {
			return strings.TrimSpace(ff[:i])
		}
		return strings.TrimSpace(ff)
	}

	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
