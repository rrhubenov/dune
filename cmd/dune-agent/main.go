//go:build linux

// dune-agent runs inside dune guests. It serves the agent protocol (see
// internal/agent) over vsock: run commands, and fix up a guest restored from
// a snapshot (clock, entropy, hostname).
//
// Its listening socket is part of the guest's memory, so it survives
// snapshot/restore: every fork has a working agent the moment it resumes.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/rrhubenov/dune/internal/agent"
)

const (
	version        = "1"
	maxOutput      = 16 << 20
	maxRequest     = 32 << 20
	defaultTimeout = 60 * time.Second
	maxTimeout     = time.Hour
)

func main() {
	log.SetFlags(0)
	ln, err := listenVsock(agent.Port)
	if err != nil {
		log.Fatalf("dune-agent: %v", err)
	}
	log.Printf("dune-agent %s listening on vsock port %d", version, agent.Port)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, agent.Health{Version: version})
	})
	mux.HandleFunc("POST /exec", handleExec)
	mux.HandleFunc("POST /fixup", handleFixup)
	mux.HandleFunc("GET /pty", handlePTY)
	log.Fatal((&http.Server{Handler: mux}).Serve(ln))
}

func handleExec(w http.ResponseWriter, r *http.Request) {
	var req agent.ExecRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequest)).Decode(&req); err != nil || len(req.Argv) == 0 {
		http.Error(w, "bad request: need argv", http.StatusBadRequest)
		return
	}
	timeout := defaultTimeout
	if req.TimeoutSec > 0 {
		timeout = min(time.Duration(req.TimeoutSec)*time.Second, maxTimeout)
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Env = append([]string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=dumb",
		"LANG=C.UTF-8",
	}, req.Env...)
	cmd.Dir = req.Dir
	if cmd.Dir == "" {
		cmd.Dir = "/root"
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	stdout, stderr := &capped{max: maxOutput}, &capped{max: maxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Own process group, so a timeout kills the whole tree.
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGKILL) }

	err := cmd.Run()
	res := agent.ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: cmd.ProcessState.ExitCode()}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.ExitCode, res.Error = -1, fmt.Sprintf("timed out after %s", timeout)
	case err != nil && !errors.As(err, &exitErr):
		res.ExitCode, res.Error = -1, err.Error()
	}
	writeJSON(w, res)
}

func handleFixup(w http.ResponseWriter, r *http.Request) {
	var req agent.FixupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var errs []error
	if req.UnixNano > 0 {
		ts := unix.NsecToTimespec(req.UnixNano)
		if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
			errs = append(errs, fmt.Errorf("set clock: %w", err))
		}
	}
	if len(req.Entropy) > 0 {
		if err := addEntropy(req.Entropy); err != nil {
			errs = append(errs, fmt.Errorf("entropy: %w", err))
		}
	}
	if req.Hostname != "" {
		if err := unix.Sethostname([]byte(req.Hostname)); err != nil {
			errs = append(errs, fmt.Errorf("hostname: %w", err))
		}
		os.WriteFile("/etc/hostname", []byte(req.Hostname+"\n"), 0o644)
	}
	if err := errors.Join(errs...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// addEntropy credits data to the kernel RNG (RNDADDENTROPY) and forces the
// CRNG to reseed (RNDRESEEDCRNG), so forks of one snapshot stop producing
// the same random numbers.
func addEntropy(data []byte) error {
	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	buf := make([]byte, 8+len(data))
	binary.NativeEndian.PutUint32(buf[0:], uint32(len(data)*8))
	binary.NativeEndian.PutUint32(buf[4:], uint32(len(data)))
	copy(buf[8:], data)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return e
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDRESEEDCRNG, 0); e != 0 {
		return e
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// capped keeps the first max bytes written and discards the rest.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// vsock listener. Go's net package doesn't know AF_VSOCK, so accept with raw
// syscalls and wrap each connection's fd in an *os.File, which the runtime
// poller handles like any socket.

type vsockListener struct{ fd int }

func listenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	return &vsockListener{fd: fd}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		nfd, sa, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		if err == unix.EINTR || err == unix.ECONNABORTED {
			continue
		}
		if err != nil {
			return nil, err
		}
		remote := vsockAddr{}
		if vm, ok := sa.(*unix.SockaddrVM); ok {
			remote = vsockAddr{cid: vm.CID, port: vm.Port}
		}
		return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock"), remote: remote}, nil
	}
}

func (l *vsockListener) Close() error   { return unix.Close(l.fd) }
func (l *vsockListener) Addr() net.Addr { return vsockAddr{cid: unix.VMADDR_CID_ANY, port: agent.Port} }

type vsockConn struct {
	*os.File
	remote vsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr {
	return vsockAddr{cid: unix.VMADDR_CID_ANY, port: agent.Port}
}
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

type vsockAddr struct{ cid, port uint32 }

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string {
	return strconv.FormatUint(uint64(a.cid), 10) + ":" + strconv.FormatUint(uint64(a.port), 10)
}
