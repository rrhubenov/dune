// Package client is a Go client for duned's v1 API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/rrhubenov/dune/internal/model"
)

type Client struct {
	base string // http://host:port, or http://duned for a unix socket
	http *http.Client
}

// New takes "unix:///run/dune/duned.sock" or "http://host:port".
func New(host string) (*Client, error) {
	if path, ok := strings.CutPrefix(host, "unix://"); ok {
		return &Client{base: "http://duned", http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		}}}, nil
	}
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("bad host %q: want unix:///path or http://host:port", host)
	}
	return &Client{base: strings.TrimSuffix(host, "/"), http: http.DefaultClient}, nil
}

// do sends in as JSON (if non-nil) and decodes the response into out (if non-nil).
func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.raw(method, path, "application/json", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// raw returns the response for 2xx statuses; other statuses become errors
// carrying the API's error message.
func (c *Client) raw(method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Dune-Request", "cli") // see api.CSRFHeader
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var e model.Error
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return nil, &Error{Status: resp.StatusCode, Msg: e.Error, Body: b}
	}
	return nil, &Error{Status: resp.StatusCode, Msg: strings.TrimSpace(string(b)), Body: b}
}

type Error struct {
	Status int
	Msg    string
	Body   []byte
}

func (e *Error) Error() string { return e.Msg }

func (c *Client) Storage() (model.Usage, error) {
	var u model.Usage
	return u, c.do("GET", "/v1/storage", nil, &u)
}

func (c *Client) Host() (model.HostStats, error) {
	var h model.HostStats
	return h, c.do("GET", "/v1/host", nil, &h)
}

func (c *Client) Trim() (model.Usage, error) {
	var u model.Usage
	return u, c.do("POST", "/v1/storage/trim", nil, &u)
}

func (c *Client) Images() ([]model.Image, error) {
	var ims []model.Image
	return ims, c.do("GET", "/v1/images", nil, &ims)
}

func (c *Client) ImportImage(req model.ImportImageRequest) (model.Image, error) {
	var im model.Image
	return im, c.do("POST", "/v1/images", req, &im)
}

func (c *Client) VMs() ([]model.VM, error) {
	var vms []model.VM
	return vms, c.do("GET", "/v1/vms", nil, &vms)
}

func (c *Client) VM(id string) (model.VM, error) {
	var vm model.VM
	return vm, c.do("GET", "/v1/vms/"+url.PathEscape(id), nil, &vm)
}

func (c *Client) CreateVM(req model.CreateVMRequest) (model.VM, error) {
	var vm model.VM
	return vm, c.do("POST", "/v1/vms", req, &vm)
}

func (c *Client) DeleteVM(id string) error {
	return c.do("DELETE", "/v1/vms/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Pause(id string) (model.VM, error) {
	var vm model.VM
	return vm, c.do("POST", "/v1/vms/"+url.PathEscape(id)+"/pause", nil, &vm)
}

func (c *Client) Resume(id string) (model.VM, error) {
	var vm model.VM
	return vm, c.do("POST", "/v1/vms/"+url.PathEscape(id)+"/resume", nil, &vm)
}

func (c *Client) Pin(id, kind string) (model.Snapshot, error) {
	var sn model.Snapshot
	return sn, c.do("POST", "/v1/vms/"+url.PathEscape(id)+"/snapshots", model.PinRequest{Kind: kind}, &sn)
}

func (c *Client) Snapshots() ([]model.Snapshot, error) {
	var sns []model.Snapshot
	return sns, c.do("GET", "/v1/snapshots", nil, &sns)
}

func (c *Client) DeleteSnapshot(id string) error {
	return c.do("DELETE", "/v1/snapshots/"+url.PathEscape(id), nil, nil)
}

// Fork returns the VMs started. If only some started, it returns them along
// with the error.
func (c *Client) Fork(snapID string, count int) ([]model.VM, error) {
	var resp model.ForkResponse
	err := c.do("POST", "/v1/snapshots/"+url.PathEscape(snapID)+"/fork", model.ForkRequest{Count: count}, &resp)
	var e *Error
	if errors.As(err, &e) && json.Unmarshal(e.Body, &resp) == nil && len(resp.VMs) > 0 {
		return resp.VMs, err
	}
	return resp.VMs, err
}

// Ports lists ports listening in VMs, of one VM if id is set.
func (c *Client) Ports(id string) ([]model.Port, error) {
	path := "/v1/ports"
	if id != "" {
		path = "/v1/vms/" + url.PathEscape(id) + "/ports"
	}
	var ps []model.Port
	return ps, c.do("GET", path, nil, &ps)
}

// Exec runs a command in the VM through its guest agent.
func (c *Client) Exec(id string, req model.ExecRequest) (model.ExecResult, error) {
	var r model.ExecResult
	return r, c.do("POST", "/v1/vms/"+url.PathEscape(id)+"/exec", req, &r)
}

// Terminal opens a login shell in the VM. See model.TerminalControl for the
// message protocol.
func (c *Client) Terminal(ctx context.Context, id string, cols, rows int) (*websocket.Conn, error) {
	u := fmt.Sprintf("%s/v1/vms/%s/terminal?cols=%d&rows=%d", c.base, url.PathEscape(id), cols, rows)
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		if resp != nil && resp.Body != nil {
			var e model.Error
			if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
				return nil, errors.New(e.Error)
			}
		}
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	return conn, nil
}

// Console returns the last tail bytes of the VM's serial console log.
func (c *Client) Console(id string, tail int) ([]byte, error) {
	resp, err := c.raw("GET", "/v1/vms/"+url.PathEscape(id)+"/console?tail="+strconv.Itoa(tail), "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// SendConsole types data on the VM's serial console.
func (c *Client) SendConsole(id string, data []byte) error {
	resp, err := c.raw("POST", "/v1/vms/"+url.PathEscape(id)+"/console", "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
