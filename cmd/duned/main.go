// duned is dune's daemon. It runs as root on the VM host, owns the storage
// path, state DB, VM processes and their networking, and serves the API on a
// root-only unix socket and on the host's tailnet address.
package main

import (
	"context"
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
	"github.com/rrhubenov/dune/internal/netns"
	"github.com/rrhubenov/dune/internal/run"
	"github.com/rrhubenov/dune/internal/storage"
	"github.com/rrhubenov/dune/internal/store"
	"github.com/rrhubenov/dune/internal/vm"
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
	mgr := vm.New(vm.Config{Storage: st, DB: db, Backend: fc, UIDBase: cfg.UIDBase, AgentBinary: agentBin})
	if err := mgr.Recover(); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	handler := (&api.Server{VMs: mgr, Storage: st, Console: fc}).Handler()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
		auth := &api.WhoisAuth{Allow: cfg.AllowUsers}
		remote := &http.Server{Handler: auth.Wrap(handler)}
		servers = append(servers, remote)
		go func() {
			addr, err := tailnetAddr(ctx, cfg.Listen)
			if err != nil {
				errc <- err
				return
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				errc <- err
				return
			}
			slog.Info("listening", "tailnet", addr, "allow", cfg.AllowUsers)
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
