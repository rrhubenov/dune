//go:build linux

package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"testing"
)

func TestParseProcNet(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1111 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2222 1 0000000000000000 100 0 0 10 0
   2: 0200A8C0:0016 0100A8C0:C350 01 00000000:00000000 02:000A7F3B 00000000     0        0 3333 4 0000000000000000 20 4 30 10 -1
`
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 4444 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5555 1 0000000000000000 100 0 0 10 0
`
	got4, err := parseProcNet(tcp)
	if err != nil {
		t.Fatal(err)
	}
	got6, err := parseProcNet(tcp6)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range append(got4, got6...) {
		got = append(got, net.JoinHostPort(s.addr.String(), strconv.Itoa(s.port))+"#"+strconv.FormatUint(s.inode, 10))
	}
	want := []string{"127.0.0.1:5173#1111", "0.0.0.0:22#2222", "[::1]:8080#4444", "[::]:80#5555"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v (established socket must be skipped)", got, want)
	}
}

// TestListenPortsLive finds this test's own listener in the real /proc.
func TestListenPortsLive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ps, err := listenPorts("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Port != port {
			continue
		}
		if p.Addr != "127.0.0.1" || p.PID != os.Getpid() || p.Process == "" || len(p.Args) == 0 || p.Cwd == "" {
			t.Fatalf("incomplete entry: %+v", p)
		}
		return
	}
	t.Fatalf("port %d not found in %+v", port, ps)
}

func TestRelay(t *testing.T) {
	// echo server standing in for the guest service
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		c, err := srv.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()
	target, err := dialLoopback(srv.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	host, peer := net.Pipe()
	go relay(target, host, bufio.NewReader(host))
	if _, err := peer.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(peer, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	peer.Close()
}
