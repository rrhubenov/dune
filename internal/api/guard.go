package api

import (
	"net"
	"net/http"
	"slices"
	"strings"
)

// CSRFHeader must be present on state-changing requests over the tailnet.
// Browsers only send custom headers on cross-origin requests after a CORS
// preflight, which duned never approves, so other sites open in the user's
// browser can't trigger pins, forks or deletes.
const CSRFHeader = "X-Dune-Request"

// BrowserGuard protects the tailnet listener from attacks through the user's
// own browser (which passes the Tailscale identity check):
//   - Host must be one of Hosts, defeating DNS rebinding;
//   - non-GET requests must carry CSRFHeader.
//
// WebSocket upgrades are GETs; their Origin is checked at accept time.
func BrowserGuard(hosts []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		if !slices.Contains(hosts, host) {
			writeJSON(w, http.StatusMisdirectedRequest, Error{Error: "unexpected Host " + r.Host})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(CSRFHeader) == "" {
			writeJSON(w, http.StatusForbidden, Error{Error: "missing " + CSRFHeader + " header"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
