package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rrhubenov/dune/internal/events"
)

// TestWebSocketOrigin: pages served from published VM ports share the
// daemon's host (other port), so they are same-site. WebSocket upgrades must
// still reject them, or a page from a VM could open terminals in other VMs.
func TestWebSocketOrigin(t *testing.T) {
	h := (&Server{Events: events.NewHub()}).Handler()
	for _, c := range []struct {
		origin  string
		blocked bool
	}{
		{"http://100.97.247.80:20017", true},
		{"http://evil.example", true},
		{"http://100.97.247.80:7070", false},
	} {
		r := httptest.NewRequest("GET", "/v1/events", nil)
		r.Host = "100.97.247.80:7070"
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		r.Header.Set("Origin", c.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if blocked := w.Code == http.StatusForbidden; blocked != c.blocked {
			t.Errorf("origin %s: status %d, want blocked=%v", c.origin, w.Code, c.blocked)
		}
	}
}
