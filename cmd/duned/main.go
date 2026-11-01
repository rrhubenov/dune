// duned is dune's daemon. It runs as root on the VM host, owns the storage
// path, state DB, VM processes and their networking, and serves the API on a
// root-only unix socket and on the host's tailnet address.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rrhubenov/dune/internal/api"
	"github.com/rrhubenov/dune/internal/backend/firecracker"
	"github.com/rrhubenov/dune/internal/events"
	"github.com/rrhubenov/dune/internal/hoststats"
	"github.com/rrhubenov/dune/internal/model"
	"github.com/rrhubenov/dune/internal/netns"
	"github.com/rrhubenov/dune/internal/ports"
	"github.com/rrhubenov/dune/internal/run"
	"github.com/rrhubenov/dune/internal/storage"
	"github.com/rrhubenov/dune/internal/store"
	"github.com/rrhubenov/dune/internal/vm"
	"github.com/rrhubenov/dune/internal/web"
)

func main() {
	configPath := flag.String("config", defaultConfigPath, "config file (JSON)")
	flag.Parse()
	if err := serve(*configPath); err != nil {
		slog.Error("duned", "err", err)
		os.Exit(1)
	}
}

func serve(configPath string) error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	for _, bin := range []string{cfg.Firecracker, cfg.Jailer, cfg.SnapshotEditor} {
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("firecracker release binary: %w", err)
		}
	}

	st := &storage.Store{Image: cfg.StoreImage, Mount: cfg.StoreMount, Size: cfg.StoreSize}
	if err := st.Setup(); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	db, err := store.Open(cfg.DB)
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	defer db.Close()
	if err := netns.HostSetup(); err != nil {
		return fmt.Errorf("host network: %w", err)
	}
	fc := firecracker.New(firecracker.Config{
		Firecracker:    cfg.Firecracker,
		Jailer:         cfg.Jailer,
		SnapshotEditor: cfg.SnapshotEditor,
		ChrootBase:     st.Path("jail"),
		Scopes:         firecracker.SystemdAvailable(),
	})
	agentBin := cfg.Agent
	if _, err := os.Stat(agentBin); err != nil {
		slog.Warn("no guest agent binary; imported images get no agent", "err", err)
		agentBin = ""
	}
	hub := events.NewHub()
	mgr := vm.New(vm.Config{Storage: st, DB: db, Backend: fc, UIDBase: cfg.UIDBase, AgentBinary: agentBin, Events: hub})
	if err := mgr.Recover(); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	host := func(ids []string) (model.HostStats, error) {
		cgs := map[string]string{}
		for _, id := range ids {
			cgs[id] = fc.Cgroup(id)
		}
		return hoststats.Collect(cfg.DataDir, cgs)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Ports noticed in VMs; published once the tailnet address is known.
	auth := &api.WhoisAuth{Allow: cfg.AllowUsers}
	pub := ports.New(ports.Config{
		Source: mgr, Alloc: db, Events: hub, Lo: cfg.portLo, Hi: cfg.portHi,
		Allow: func(remote net.Addr) bool {
			_, err := auth.Check(remote.String())
			return err == nil
		},
	})
	go pub.Run(ctx)

	handler := (&api.Server{VMs: mgr, Storage: st, Console: fc, Events: hub, UI: web.Handler(), Host: host, Ports: pub.List}).Handler()

	local, err := listenUnix(cfg.Socket)
	if err != nil {
		return err
	}
	servers := []*http.Server{{Handler: handler}}
	errc := make(chan error, 2)
	go func() { errc <- servers[0].Serve(local) }()
	slog.Info("listening", "socket", cfg.Socket)

	if cfg.Listen != "" {
		if len(cfg.AllowUsers) == 0 {
			slog.Warn("allow_users is empty: every tailnet request will be rejected")
		}
		remote := &http.Server{}
		servers = append(servers, remote)
		go func() {
			addr, err := tailnetAddr(ctx, cfg.Listen)
			if err != nil {
				errc <- err
				return
			}
			hosts := allowedHosts(addr)
			remote.Handler = auth.Wrap(api.BrowserGuard(hosts, handler))
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				errc <- err
				return
			}
			slog.Info("listening", "tailnet", addr, "allow", cfg.AllowUsers, "hosts", hosts)
			ip, _, _ := net.SplitHostPort(addr)
			pub.SetIP(ip)
			slog.Info("publishing VM ports", "ip", ip, "range", cfg.PortRange)
			errc <- remote.Serve(ln)
		}()
	}

	select {
	case <-ctx.Done():
		slog.Info("shutting down; VMs keep running")
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdown)
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// listenUnix creates the root-only API socket, replacing a stale one.
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// allowedHosts lists the Host header values the tailnet listener accepts:
// its address plus this machine's Tailscale names (short and MagicDNS).
func allowedHosts(addr string) []string {
	host, _, _ := net.SplitHostPort(addr)
	hosts := []string{host}
	out, err := run.Output("tailscale", "status", "--json", "--self", "--peers=false")
	if err != nil {
		slog.Warn("tailscale status; only the IP is an allowed Host", "err", err)
		return hosts
	}
	var st struct {
		Self struct {
			DNSName  string
			HostName string
		}
	}
	if json.Unmarshal([]byte(out), &st) == nil {
		for _, h := range []string{st.Self.DNSName, st.Self.HostName} {
			if h = strings.ToLower(strings.TrimSuffix(h, ".")); h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

// tailnetAddr resolves "tailscale" to <tailscale IPv4>:7070, waiting for
// tailscaled to come up (duned may start first at boot).
func tailnetAddr(ctx context.Context, listen string) (string, error) {
	if listen != "tailscale" {
		return listen, nil
	}
	for {
		out, err := run.Output("tailscale", "ip", "-4")
		if ip := strings.TrimSpace(out); err == nil && ip != "" {
			return net.JoinHostPort(ip, "7070"), nil
		}
		slog.Warn("waiting for tailscale IP", "err", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
