// Package backend defines what a hypervisor backend must do. The daemon owns
// IDs, storage layout, networking and the snapshot tree; a backend only runs
// machines and reads/writes snapshot files in directories it is handed.
package backend

import (
	"context"
	"net"
)

type SnapshotKind string

const (
	Full SnapshotKind = "full"
	Diff SnapshotKind = "diff" // only memory pages dirtied since this machine's previous snapshot (or restore)
)

// Machine describes one VM process.
type Machine struct {
	ID     string
	VCPUs  int
	MemMiB int
	Kernel string // host path; used by Boot only
	Rootfs string // host path to the disk image to clone; used by Boot only
	NetNS  string // netns file the VM joins, e.g. /run/netns/dune-<id>
	// Hostname is set at boot. Restored machines keep their snapshot's
	// hostname until the guest agent changes it.
	Hostname string
	UID      int // unprivileged user the VM process runs as
	GID      int
}

type Backend interface {
	// Boot clones m.Rootfs into a private disk and cold-boots m.Kernel.
	Boot(ctx context.Context, m Machine) (pid int, err error)
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	// Snapshot pauses the machine and writes its state, memory and disk into
	// dir, then resumes it if resume is set.
	Snapshot(ctx context.Context, id, dir string, kind SnapshotKind, resume bool) error
	// Restore starts m from a snapshot chain: chain[0] is a full snapshot's
	// dir, each later dir a diff on top of the previous one. The disk comes
	// from the last dir. m.Kernel and m.Rootfs are ignored.
	Restore(ctx context.Context, m Machine, chain []string) (pid int, err error)
	// Stop kills the machine and removes its private files. Idempotent. pid
	// is the last known process id (0 if none); it is only killed if it is
	// verifiably this machine, since it may come from before a daemon restart.
	Stop(ctx context.Context, id string, pid int) error
	// Alive reports whether pid is still this machine's process.
	Alive(id string, pid int) bool
	// DialAgent opens a stream to the guest agent's vsock port.
	DialAgent(ctx context.Context, id string, port uint32) (net.Conn, error)
}
