// Package model holds the types exchanged over the API. It has no
// dependencies so the CLI (built for macOS too) can import it without pulling
// in server code.
package model

import "time"

const (
	VMRunning = "running"
	VMPaused  = "paused"
	VMStopped = "stopped"

	SnapFull = "full"
	SnapDiff = "diff"
)

type Image struct {
	Name      string    `json:"name"`
	Kernel    string    `json:"kernel"`
	Rootfs    string    `json:"rootfs"`
	CreatedAt time.Time `json:"created_at"`
}

type VM struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	State     string    `json:"state"`
	VCPUs     int       `json:"vcpus"`
	MemMiB    int       `json:"mem_mib"`
	NetSlot   int       `json:"net_slot"`
	PID       int       `json:"pid,omitempty"`
	Origin    string    `json:"origin,omitempty"` // snapshot this VM was forked from
	Head      string    `json:"head,omitempty"`   // latest snapshot of this VM; next diff builds on it
	CreatedAt time.Time `json:"created_at"`
}

type Snapshot struct {
	ID        string    `json:"id"`
	VMID      string    `json:"vm_id"`
	Parent    string    `json:"parent,omitempty"` // delta base; empty for full snapshots
	Kind      string    `json:"kind"`
	MemMiB    int       `json:"mem_mib"`
	VCPUs     int       `json:"vcpus"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
}

type Usage struct {
	ImageAllocated int64 `json:"image_allocated"` // bytes the sparse image really uses on the host fs
	FSUsed         int64 `json:"fs_used"`
	FSTotal        int64 `json:"fs_total"`
	Mounted        bool  `json:"mounted"`
}

type ImportImageRequest struct {
	Name   string `json:"name"`
	Kernel string `json:"kernel"` // path on the daemon's host
	Rootfs string `json:"rootfs"`
}

type CreateVMRequest struct {
	Image  string `json:"image"`
	VCPUs  int    `json:"vcpus"`
	MemMiB int    `json:"mem_mib"`
}

type PinRequest struct {
	Kind string `json:"kind,omitempty"` // "", "full" or "diff"
}

type ForkRequest struct {
	Count int `json:"count"`
}

type ForkResponse struct {
	VMs   []VM   `json:"vms"`
	Error string `json:"error,omitempty"` // set if only some forks started
}

// ExecRequest runs a command in the guest via the agent.
type ExecRequest struct {
	Argv       []string `json:"argv"`
	Env        []string `json:"env,omitempty"`   // KEY=VALUE, added to the agent's defaults
	Dir        string   `json:"dir,omitempty"`   // default /root
	Stdin      []byte   `json:"stdin,omitempty"` // base64 in JSON
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

type ExecResult struct {
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	// Error is set when the command could not run at all (not found, timed
	// out); ExitCode is then -1.
	Error string `json:"error,omitempty"`
}

// TerminalControl is a JSON text message on /v1/vms/{id}/terminal. Clients
// send {"type":"resize","cols":C,"rows":R}; the daemon sends
// {"type":"exit","code":N} when the shell ends, then closes. Binary messages
// carry terminal bytes both ways.
type TerminalControl struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Code int    `json:"code"`
}

type Error struct {
	Error string `json:"error"`
}
