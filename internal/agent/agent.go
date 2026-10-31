// Package agent defines the guest agent protocol and the host-side client.
//
// The agent (cmd/dune-agent) listens on vsock port Port inside the guest and
// serves HTTP/1.1. The host reaches it through the backend's vsock transport
// (for Firecracker: a unix socket plus a "CONNECT <port>" handshake), one
// connection per request.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/rrhubenov/dune/internal/model"
)

const Port = 1024

type (
	ExecRequest = model.ExecRequest
	ExecResult  = model.ExecResult
)

// FixupRequest makes a restored guest distinct from its siblings and current.
type FixupRequest struct {
	UnixNano int64  `json:"unix_nano"` // host wall clock
	Entropy  []byte `json:"entropy"`   // mixed into the guest RNG, which is then reseeded
	Hostname string `json:"hostname,omitempty"`
}

type Health struct {
	Version string `json:"version"`
}

type Client struct {
	http *http.Client
	dial func(ctx context.Context) (net.Conn, error)
}

// NewClient returns a client that opens a fresh connection per request with dial.
func NewClient(dial func(ctx context.Context) (net.Conn, error)) *Client {
	return &Client{dial: dial, http: &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: true,
	}}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("agent %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	return h, c.do(ctx, "GET", "/health", nil, &h)
}

func (c *Client) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var r ExecResult
	return r, c.do(ctx, "POST", "/exec", req, &r)
}

func (c *Client) Fixup(ctx context.Context, req FixupRequest) error {
	return c.do(ctx, "POST", "/fixup", req, nil)
}
