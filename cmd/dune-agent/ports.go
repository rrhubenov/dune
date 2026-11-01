//go:build linux

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rrhubenov/dune/internal/agent"
)

// handlePorts lists the guest's listening TCP sockets with the process that
// owns each, so the host can notice dev servers without the guest's help.
func handlePorts(w http.ResponseWriter, r *http.Request) {
	ps, err := listenPorts("/proc")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ps)
}

// handleTCP upgrades the connection and relays it to a port on the guest's
// loopback. Loopback reaches servers bound to localhost as well as wildcard
// ones, which is the point: dev servers often listen on localhost only.
func handleTCP(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), agent.TCPUpgrade) {
		http.Error(w, "expected Upgrade: "+agent.TCPUpgrade, http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "bad port", http.StatusBadRequest)
		return
	}
	target, err := dialLoopback(port)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer target.Close()
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + agent.TCPUpgrade + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		return
	}
	relay(target, conn, brw.Reader)
}

// dialLoopback tries IPv4 then IPv6 loopback ("localhost" may resolve to
// either, so servers bind one or the other).
func dialLoopback(port int) (net.Conn, error) {
	var errs []error
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("port %d: %w", port, errors.Join(errs...))
}

// relay copies both ways until either direction ends, then closes both: the
// vsock side can't reliably half-close, so neither side does.
func relay(target net.Conn, host net.Conn, hostR *bufio.Reader) {
	var once sync.Once
	done := make(chan struct{})
	stop := func() {
		once.Do(func() {
			target.Close()
			host.Close()
			close(done)
		})
	}
	go func() { io.Copy(target, hostR); stop() }()
	go func() { io.Copy(host, target); stop() }()
	<-done
}

// listenPorts reads <proc>/net/tcp{,6} for listening sockets and maps each to
// its owning process through <proc>/<pid>/fd.
func listenPorts(proc string) ([]agent.ListenPort, error) {
	var socks []procSock
	for _, f := range []string{"net/tcp", "net/tcp6"} {
		b, err := os.ReadFile(filepath.Join(proc, f))
		if errors.Is(err, os.ErrNotExist) {
			continue // no IPv6
		}
		if err != nil {
			return nil, err
		}
		s, err := parseProcNet(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		socks = append(socks, s...)
	}
	owners := socketOwners(proc, socks)
	out := make([]agent.ListenPort, 0, len(socks))
	for _, s := range socks {
		p := agent.ListenPort{Port: s.port, Addr: s.addr.String()}
		if pid, ok := owners[s.inode]; ok {
			p.PID = pid
			p.Process, p.Args, p.Cwd = procInfo(proc, pid)
		}
		out = append(out, p)
	}
	return out, nil
}

type procSock struct {
	addr  netip.Addr
	port  int
	inode uint64
}

const tcpListen = "0A"

// parseProcNet parses /proc/net/tcp or tcp6 and returns listening sockets.
// Lines look like
//
//	0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 12345 ...
//
// where addresses are 32-bit words in host byte order, printed as hex.
func parseProcNet(s string) ([]procSock, error) {
	var out []procSock
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 10 || f[3] != tcpListen {
			continue
		}
		hexAddr, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			return nil, fmt.Errorf("bad local address %q", f[1])
		}
		raw, err := hex.DecodeString(hexAddr)
		if err != nil || (len(raw) != 4 && len(raw) != 16) {
			return nil, fmt.Errorf("bad local address %q", f[1])
		}
		for w := 0; w < len(raw); w += 4 { // each word: host order -> bytes
			binary.NativeEndian.PutUint32(raw[w:], binary.BigEndian.Uint32(raw[w:]))
		}
		addr, _ := netip.AddrFromSlice(raw)
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil {
			return nil, fmt.Errorf("bad port %q", hexPort)
		}
		inode, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad inode %q", f[9])
		}
		out = append(out, procSock{addr: addr.Unmap(), port: int(port), inode: inode})
	}
	return out, nil
}

// socketOwners maps socket inodes to a pid holding them open.
func socketOwners(proc string, socks []procSock) map[uint64]int {
	want := map[uint64]bool{}
	for _, s := range socks {
		want[s.inode] = true
	}
	owners := map[uint64]int{}
	ents, _ := os.ReadDir(proc)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(proc, e.Name(), "fd")
		fds, _ := os.ReadDir(fdDir)
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 64)
			if err != nil || !want[inode] {
				continue
			}
			if _, seen := owners[inode]; !seen {
				owners[inode] = pid
			}
		}
	}
	return owners
}

const (
	maxArgs   = 32
	maxArgLen = 256
)

func procInfo(proc string, pid int) (comm string, args []string, cwd string) {
	dir := filepath.Join(proc, strconv.Itoa(pid))
	if b, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
		comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		for a := range strings.SplitSeq(strings.TrimRight(string(b), "\x00"), "\x00") {
			if len(args) == maxArgs {
				break
			}
			if len(a) > maxArgLen {
				a = a[:maxArgLen]
			}
			args = append(args, a)
		}
	}
	cwd, _ = os.Readlink(filepath.Join(dir, "cwd"))
	return comm, args, cwd
}
