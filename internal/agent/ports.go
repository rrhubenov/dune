package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// TCP tunnels: the host sends "GET /tcp?port=N" with "Upgrade: dune-tcp"; the
// agent connects to port N on the guest's loopback and, after "101 Switching
// Protocols", relays raw bytes both ways until either side closes.
const TCPUpgrade = "dune-tcp"

// ListenPort is a TCP socket listening in the guest (GET /ports).
type ListenPort struct {
	Port    int      `json:"port"`
	Addr    string   `json:"addr"` // bound address: 0.0.0.0, ::, 127.0.0.1, ...
	PID     int      `json:"pid,omitempty"`
	Process string   `json:"process,omitempty"` // /proc/<pid>/comm
	Args    []string `json:"args,omitempty"`    // /proc/<pid>/cmdline, capped
	Cwd     string   `json:"cwd,omitempty"`
}

// Ports lists the guest's listening TCP sockets.
func (c *Client) Ports(ctx context.Context) ([]ListenPort, error) {
	var ps []ListenPort
	return ps, c.do(ctx, "GET", "/ports", nil, &ps)
}

// DialTCP opens a stream to a TCP port on the guest's loopback.
func (c *Client) DialTCP(ctx context.Context, port int) (net.Conn, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("bad port %d", port)
	}
	return c.upgrade(ctx, fmt.Sprintf("/tcp?port=%d", port), TCPUpgrade)
}

// upgrade sends an HTTP upgrade request on a fresh connection and returns the
// connection once the agent switched protocols.
func (c *Client) upgrade(ctx context.Context, path, proto string) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(10 * time.Second)
	}
	conn.SetDeadline(dl)
	req := "GET " + path + " HTTP/1.1\r\nHost: agent\r\nConnection: Upgrade\r\nUpgrade: " + proto + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, statusErr("GET", path, resp, msg)
	}
	conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

// bufConn reads through the bufio.Reader that parsed the upgrade response, so
// bytes the agent sent right after it are not lost.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
