// Package ports notices TCP ports listening in VMs and publishes them on the
// tailnet, without the guest's cooperation: duned polls each running VM's
// agent for its listening sockets, and every port that stays up gets a host
// port on the tailnet address. Connections to it are checked against the
// tailnet peer (like the API) and tunneled over vsock to the guest's loopback,
// so dev servers bound to localhost work too.
package ports

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rrhubenov/dune/internal/agent"
	"github.com/rrhubenov/dune/internal/events"
	"github.com/rrhubenov/dune/internal/model"
)

// Source is the VM manager, as far as ports are concerned.
type Source interface {
	List() ([]model.VM, error)
	ListenPorts(ctx context.Context, id string) ([]agent.ListenPort, error)
	DialPort(ctx context.Context, id string, port int) (net.Conn, error)
}

// Allocator hands out host ports (see store.HostPort).
type Allocator interface {
	HostPort(vmID string, guest, lo, hi int, avoid func(int) bool) (int, error)
	DropHostPort(vmID string, guest int) error
}

type Config struct {
	Source Source
	Alloc  Allocator
	Events *events.Hub
	// Lo and Hi bound the host ports published ports get.
	Lo, Hi int
	// Allow decides, per connection, whether a peer may use a published
	// port. Nil admits no one.
	Allow func(remote net.Addr) bool
	// Interval between polls of each VM (default 1s).
	Interval time.Duration
}

const (
	// A port is shown once it was seen in this many consecutive polls, so
	// short-lived listeners (test servers) don't flicker through the UI.
	minSeen   = 2
	maxProbes = 3
	// Concurrent agent requests per poll.
	parallel = 8
)

type Publisher struct {
	cfg Config

	mu  sync.Mutex
	ip  string // tailnet address to publish on; "" = detect only
	vms map[string]*vmState
}

type vmState struct {
	ports map[int]*entry
	next  time.Time // next poll; backoff after errors
	fails int
}

type entry struct {
	model.Port
	pid     int
	seen    int
	ln      net.Listener
	retry   time.Time // next listen attempt after failures
	probes  int
	probing bool
}

func (e *entry) visible() bool { return e.seen >= minSeen }

func (e *entry) close() {
	if e.ln != nil {
		e.ln.Close()
		e.ln = nil
		e.Addr = ""
	}
}

func New(cfg Config) *Publisher {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	return &Publisher{cfg: cfg, vms: map[string]*vmState{}}
}

// SetIP starts publishing on ip (the tailnet address, which may only be known
// some time after startup). Listeners open on the next poll.
func (p *Publisher) SetIP(ip string) {
	p.mu.Lock()
	p.ip = ip
	p.mu.Unlock()
}

// List returns the visible ports, of one VM if vmID is set, ordered by VM
// and port.
func (p *Publisher) List(vmID string) []model.Port {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []model.Port{}
	for id, st := range p.vms {
		if vmID != "" && id != vmID {
			continue
		}
		for _, e := range st.ports {
			if !e.visible() {
				continue
			}
			pt := e.Port
			if pt.HTTP && pt.Addr != "" {
				pt.URL = "http://" + pt.Addr + "/"
			}
			out = append(out, pt)
		}
	}
	slices.SortFunc(out, func(a, b model.Port) int {
		if c := strings.Compare(a.VMID, b.VMID); c != 0 {
			return c
		}
		return a.Port - b.Port
	})
	return out
}

// Run polls until ctx ends, then closes all published listeners.
func (p *Publisher) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		p.poll(ctx)
		select {
		case <-ctx.Done():
			p.mu.Lock()
			for _, st := range p.vms {
				for _, e := range st.ports {
					e.close()
				}
			}
			p.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

type pollResult struct {
	id  string
	ps  []agent.ListenPort
	err error
}

func (p *Publisher) poll(ctx context.Context) {
	vms, err := p.cfg.Source.List()
	if err != nil {
		slog.Warn("ports: list vms", "err", err)
		return
	}
	now := time.Now()
	running, keep := map[string]bool{}, map[string]bool{}
	for _, vm := range vms {
		switch vm.State {
		case model.VMRunning:
			running[vm.ID], keep[vm.ID] = true, true
		case model.VMPaused: // keep what it had; connections wait for resume
			keep[vm.ID] = true
		}
	}

	p.mu.Lock()
	changed := false
	for id, st := range p.vms {
		if keep[id] {
			continue
		}
		for _, e := range st.ports {
			changed = changed || e.visible()
			e.close()
		}
		delete(p.vms, id)
	}
	var due []string
	for id := range running {
		st := p.vms[id]
		if st == nil {
			st = &vmState{ports: map[int]*entry{}}
			p.vms[id] = st
		}
		if !now.Before(st.next) {
			due = append(due, id)
		}
	}
	p.mu.Unlock()

	results := make([]pollResult, len(due))
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for i, id := range due {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			ps, err := p.cfg.Source.ListenPorts(c, id)
			results[i] = pollResult{id, ps, err}
		})
	}
	wg.Wait()

	p.mu.Lock()
	for _, r := range results {
		st := p.vms[r.id]
		if st == nil {
			continue
		}
		if r.err != nil {
			st.fails++
			if errors.Is(r.err, agent.ErrUnsupported) {
				if st.fails == 1 {
					slog.Info("ports: guest agent too old for port detection", "vm", r.id)
				}
				st.next = now.Add(5 * time.Minute)
			} else {
				// Forks answer once their vsock transport is back; a VM
				// without an agent never does.
				st.next = now.Add(min(p.cfg.Interval<<min(st.fails, 6), 30*time.Second))
			}
			continue
		}
		st.fails, st.next = 0, time.Time{}
		changed = p.update(r.id, st, r.ps, now) || changed
	}
	var probes []*entry
	for _, st := range p.vms {
		for _, e := range st.ports {
			if !e.visible() {
				continue
			}
			if e.ln == nil && p.ip != "" && !now.Before(e.retry) {
				changed = p.listen(e, now) || changed
			}
			if !e.Probed && !e.probing {
				e.probing = true
				probes = append(probes, e)
			}
		}
	}
	type probeJob struct {
		vm        string
		port, pid int
	}
	jobs := make([]probeJob, len(probes))
	for i, e := range probes {
		jobs[i] = probeJob{e.VMID, e.Port.Port, e.pid}
	}
	p.mu.Unlock()

	for _, j := range jobs {
		go p.probe(ctx, j.vm, j.port, j.pid)
	}
	if changed {
		p.cfg.Events.Publish(events.Ports)
	}
}

// update applies one poll's listening sockets to a VM's state and reports
// whether the visible set changed.
func (p *Publisher) update(id string, st *vmState, ps []agent.ListenPort, now time.Time) bool {
	cur := map[int]agent.ListenPort{}
	for _, lp := range ps {
		if ignored(lp) {
			continue
		}
		// The same port on IPv4 and IPv6 is one service.
		if old, ok := cur[lp.Port]; ok && old.PID != 0 {
			continue
		}
		cur[lp.Port] = lp
	}
	changed := false
	for port, e := range st.ports {
		if _, ok := cur[port]; !ok {
			changed = changed || e.visible()
			e.close()
			delete(st.ports, port)
		}
	}
	for port, lp := range cur {
		e := st.ports[port]
		if e == nil {
			e = &entry{Port: model.Port{VMID: id, Port: port, Since: now}}
			st.ports[port] = e
		}
		if e.pid != lp.PID { // a new server on the same port: probe again
			// A probe still running for the old one is discarded (pid check).
			e.pid, e.Probed, e.HTTP, e.Title, e.probes, e.probing = lp.PID, false, false, "", 0, false
			changed = changed || e.visible()
		}
		e.Bind = lp.Addr
		e.Process = lp.Process
		e.Label = label(lp.Process, lp.Args)
		e.Cmdline = strings.Join(lp.Args, " ")
		e.Cwd = lp.Cwd
		e.seen++
		changed = changed || e.seen == minSeen
	}
	return changed
}

// ignored filters system services that every guest runs. Sockets held by
// PID 1 are systemd socket activation (e.g. ssh.socket on port 22).
func ignored(lp agent.ListenPort) bool {
	if lp.PID == 1 {
		return true
	}
	switch lp.Process {
	case "sshd", "systemd-resolve", "systemd-resolved":
		return true
	}
	return lp.Port == 53
}

// listen publishes e on a host port, retrying with another port if the
// allocated one is taken by something else on the host.
func (p *Publisher) listen(e *entry, now time.Time) bool {
	avoid := map[int]bool{}
	for range 5 {
		hp, err := p.cfg.Alloc.HostPort(e.VMID, e.Port.Port, p.cfg.Lo, p.cfg.Hi, func(n int) bool { return avoid[n] })
		if err != nil {
			slog.Warn("ports: allocate host port", "vm", e.VMID, "port", e.Port.Port, "err", err)
			break
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(p.ip, strconv.Itoa(hp)))
		if err != nil {
			avoid[hp] = true
			p.cfg.Alloc.DropHostPort(e.VMID, e.Port.Port)
			continue
		}
		e.ln, e.Addr = ln, ln.Addr().String()
		go p.accept(ln, e.VMID, e.Port.Port)
		return true
	}
	e.retry = now.Add(30 * time.Second)
	return false
}

func (p *Publisher) accept(ln net.Listener, vmID string, port int) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go p.serve(c, vmID, port)
	}
}

func (p *Publisher) serve(c net.Conn, vmID string, port int) {
	defer c.Close()
	if p.cfg.Allow == nil || !p.cfg.Allow(c.RemoteAddr()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	g, err := p.cfg.Source.DialPort(ctx, vmID, port)
	cancel()
	if err != nil {
		slog.Info("ports: connect to guest", "vm", vmID, "port", port, "err", err)
		return
	}
	relay(c, g)
}

// relay copies both ways until either direction ends, then closes both (the
// vsock leg can't half-close reliably).
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

func (p *Publisher) probe(ctx context.Context, vmID string, port, pid int) {
	res, conclusive := probeHTTP(ctx, func(ctx context.Context) (net.Conn, error) {
		return p.cfg.Source.DialPort(ctx, vmID, port)
	}, port)
	p.mu.Lock()
	changed := false
	if st := p.vms[vmID]; st != nil {
		if e := st.ports[port]; e != nil && e.pid == pid {
			e.probing = false
			e.probes++
			if conclusive || e.probes >= maxProbes {
				e.Probed, e.HTTP, e.Title = true, res.http, res.title
				changed = true
			}
		}
	}
	p.mu.Unlock()
	if changed {
		p.cfg.Events.Publish(events.Ports)
	}
}
