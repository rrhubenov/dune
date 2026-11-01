package ports

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rrhubenov/dune/internal/agent"
	"github.com/rrhubenov/dune/internal/model"
)

// fakeVMs stands in for the VM manager: guest ports map to local servers.
type fakeVMs struct {
	mu      sync.Mutex
	vms     []model.VM
	ports   map[string][]agent.ListenPort
	targets map[int]string // guest port -> local address
	err     error
}

func (f *fakeVMs) List() ([]model.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.VM(nil), f.vms...), nil
}

func (f *fakeVMs) ListenPorts(ctx context.Context, id string) ([]agent.ListenPort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ports[id], f.err
}

func (f *fakeVMs) DialPort(ctx context.Context, id string, port int) (net.Conn, error) {
	f.mu.Lock()
	addr := f.targets[port]
	f.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (f *fakeVMs) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// fakeAlloc hands out free ephemeral ports, stable per VM and guest port.
type fakeAlloc struct {
	mu sync.Mutex
	m  map[string]int
}

func (a *fakeAlloc) HostPort(vmID string, guest, lo, hi int, avoid func(int) bool) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := fmt.Sprint(vmID, guest)
	if p, ok := a.m[k]; ok {
		return p, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	a.m[k] = p
	return p, nil
}

func (a *fakeAlloc) DropHostPort(vmID string, guest int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.m, fmt.Sprint(vmID, guest))
	return nil
}

func TestPublish(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html><head><title>\n  Shop &amp; Co </title></head><body>hello from the guest</body></html>")
	}))
	defer web.Close()
	raw, _ := net.Listen("tcp", "127.0.0.1:0") // speaks something else
	defer raw.Close()
	go func() {
		for {
			c, err := raw.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "-ERR unknown command\r\n")
			c.Close()
		}
	}()

	vm := model.VM{ID: "aaaa", State: model.VMRunning}
	src := &fakeVMs{
		vms: []model.VM{vm},
		ports: map[string][]agent.ListenPort{vm.ID: {
			{Port: 5173, Addr: "127.0.0.1", PID: 10, Process: "node", Args: []string{"node", "/root/shop/node_modules/vite/bin/vite.js"}, Cwd: "/root/shop"},
			{Port: 5173, Addr: "::1", PID: 10, Process: "node"},
			{Port: 6379, Addr: "0.0.0.0", PID: 11, Process: "redis-server", Args: []string{"redis-server *:6379"}},
			{Port: 22, Addr: "0.0.0.0", PID: 1, Process: "systemd", Args: []string{"/sbin/init"}},
			{Port: 2222, Addr: "0.0.0.0", PID: 30, Process: "sshd"},
		}},
		targets: map[int]string{5173: web.Listener.Addr().String(), 6379: raw.Addr().String()},
	}
	var mu sync.Mutex
	allow := true
	p := New(Config{Source: src, Alloc: &fakeAlloc{m: map[string]int{}}, Lo: 1, Hi: 65535,
		Allow: func(net.Addr) bool { mu.Lock(); defer mu.Unlock(); return allow }})
	p.SetIP("127.0.0.1")
	ctx := t.Context()

	p.poll(ctx)
	if got := p.List(""); len(got) != 0 {
		t.Fatalf("shown after one poll: %+v", got)
	}
	p.poll(ctx)
	var ps []model.Port
	waitFor(t, func() bool {
		ps = p.List("")
		return len(ps) == 2 && ps[0].Probed && ps[1].Probed
	})
	vite, redis := ps[0], ps[1]
	if vite.Port != 5173 || vite.Label != "vite" || !vite.HTTP || vite.Title != "Shop & Co" || vite.Cwd != "/root/shop" || vite.URL == "" {
		t.Fatalf("vite = %+v", vite)
	}
	if redis.Port != 6379 || redis.Label != "redis-server" || redis.HTTP || redis.URL != "" || redis.Addr == "" {
		t.Fatalf("redis = %+v", redis)
	}

	body := get(t, vite.URL)
	if !strings.Contains(body, "hello from the guest") {
		t.Fatalf("through published port: %q", body)
	}

	mu.Lock()
	allow = false
	mu.Unlock()
	// Checked per connection, so use a new one.
	fresh := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if _, err := fresh.Get(vite.URL); err == nil {
		t.Fatal("denied peer got a response")
	}

	// The server stops: unpublished.
	src.set(func() { src.ports[vm.ID] = src.ports[vm.ID][2:] })
	p.poll(ctx)
	if got := p.List(""); len(got) != 1 || got[0].Port != 6379 {
		t.Fatalf("after stop: %+v", got)
	}
	if c, err := net.Dial("tcp", vite.Addr); err == nil {
		c.Close()
		t.Fatal("listener still open after the port closed")
	}

	// The VM goes away: everything goes.
	src.set(func() { src.vms = nil })
	p.poll(ctx)
	if got := p.List(""); len(got) != 0 {
		t.Fatalf("after vm stop: %+v", got)
	}
}

// TestBackoff: a VM whose agent can't answer is polled less often and keeps
// what it showed before.
func TestBackoff(t *testing.T) {
	vm := model.VM{ID: "bbbb", State: model.VMRunning}
	src := &fakeVMs{vms: []model.VM{vm}, ports: map[string][]agent.ListenPort{
		vm.ID: {{Port: 8000, Addr: "0.0.0.0", PID: 5, Process: "python3", Args: []string{"python3", "-m", "http.server"}}},
	}}
	p := New(Config{Source: src, Alloc: &fakeAlloc{m: map[string]int{}}})
	ctx := t.Context()
	p.poll(ctx)
	p.poll(ctx)
	if got := p.List(vm.ID); len(got) != 1 || got[0].Label != "http.server" || got[0].Addr != "" {
		t.Fatalf("detect-only = %+v", got)
	}
	src.set(func() { src.err = fmt.Errorf("%w: old", agent.ErrUnsupported) })
	p.poll(ctx)
	if p.vms[vm.ID].next.Before(time.Now().Add(time.Minute)) {
		t.Fatal("no backoff for an old agent")
	}
	if got := p.List(vm.ID); len(got) != 1 {
		t.Fatalf("lost ports on error: %+v", got)
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		comm string
		args []string
		want string
	}{
		{"node", []string{"node", "/root/app/node_modules/.bin/vite"}, "vite"},
		{"node", []string{"node", "--inspect", "server.mjs"}, "server"},
		{"next-server (v1", []string{"next-server (v15.1.0)"}, "next-server"},
		{"python3", []string{"/usr/bin/python3", "-m", "http.server", "8000"}, "http.server"},
		{"python3.12", []string{"python3.12", "manage.py", "runserver"}, "manage.py"},
		{"uvicorn", []string{"/root/.venv/bin/python", "/root/.venv/bin/uvicorn", "app:app"}, "uvicorn"},
		{"postgres", []string{"/usr/lib/postgresql/16/bin/postgres", "-D", "/var/lib/postgresql"}, "postgres"},
		{"x", nil, "x"},
	}
	for _, c := range cases {
		if got := label(c.comm, c.args); got != c.want {
			t.Errorf("label(%q, %q) = %q, want %q", c.comm, c.args, got, c.want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
