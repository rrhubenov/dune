package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// client talks to one Firecracker process's REST API over its unix socket.
type client struct {
	http *http.Client
}

func newClient(sock string) *client {
	return &client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}}
}

func (c *client) do(ctx context.Context, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fc"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		var fe struct {
			FaultMessage string `json:"fault_message"`
		}
		if json.Unmarshal(msg, &fe) == nil && fe.FaultMessage != "" {
			msg = []byte(fe.FaultMessage)
		}
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func (c *client) put(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPut, path, body)
}

func (c *client) patch(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPatch, path, body)
}

// Request bodies, per firecracker_spec-v1.17.0.yaml.

type machineConfig struct {
	VCPUCount       int  `json:"vcpu_count"`
	MemSizeMiB      int  `json:"mem_size_mib"`
	TrackDirtyPages bool `json:"track_dirty_pages"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type networkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

type action struct {
	ActionType string `json:"action_type"`
}

type vmState struct {
	State string `json:"state"` // "Paused" or "Resumed"
}

type snapshotCreate struct {
	SnapshotType string `json:"snapshot_type"` // "Full" or "Diff"
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type memBackend struct {
	BackendType string `json:"backend_type"` // "File"
	BackendPath string `json:"backend_path"`
}

type snapshotLoad struct {
	SnapshotPath    string     `json:"snapshot_path"`
	MemBackend      memBackend `json:"mem_backend"`
	TrackDirtyPages bool       `json:"track_dirty_pages"`
	ResumeVM        bool       `json:"resume_vm"`
	ClockRealtime   bool       `json:"clock_realtime"`
}
