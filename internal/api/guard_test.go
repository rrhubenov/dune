package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := BrowserGuard([]string{"100.97.247.80", "midas", "midas.tail981a68.ts.net"}, ok)
	cases := []struct {
		name, method, host string
		csrf               bool
		want               int
	}{
		{"get by ip", "GET", "100.97.247.80:7070", false, http.StatusTeapot},
		{"get by magicdns", "GET", "midas.tail981a68.ts.net:7070", false, http.StatusTeapot},
		{"get by short name, any case", "GET", "MIDAS:7070", false, http.StatusTeapot},
		{"dns rebinding host", "GET", "evil.example:7070", false, http.StatusMisdirectedRequest},
		{"post without header", "POST", "midas:7070", false, http.StatusForbidden},
		{"delete without header", "DELETE", "midas:7070", false, http.StatusForbidden},
		{"post with header", "POST", "midas:7070", true, http.StatusTeapot},
		{"post with header, bad host", "POST", "evil.example", true, http.StatusMisdirectedRequest},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/v1/vms", nil)
		r.Host = c.host
		if c.csrf {
			r.Header.Set(CSRFHeader, "x")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// The request logger must not hide http.Hijacker, or WebSocket upgrades fail.
func TestLoggerKeepsHijacker(t *testing.T) {
	var hijackable bool
	h := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hijackable = w.(http.Hijacker)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !hijackable {
		t.Fatal("logRequests' writer does not implement http.Hijacker")
	}
}
