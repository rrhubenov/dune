// Package vm orchestrates VMs: it ties the state DB, the storage path,
// per-VM networking and the hypervisor backend into the operations the API
// exposes (create, pause/resume, pin, fork, delete).
//
// Layout on the store:
//
//	images/<name>/vmlinux, rootfs.ext4
//	snapshots/<id>/vmstate, mem, disk.ext4 (+ mem.full cache for diffs)
//	jail/...                                (owned by the backend)
package vm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rrhubenov/dune/internal/agent"
	"github.com/rrhubenov/dune/internal/backend"
	"github.com/rrhubenov/dune/internal/events"
	"github.com/rrhubenov/dune/internal/image"
	"github.com/rrhubenov/dune/internal/model"
	"github.com/rrhubenov/dune/internal/netns"
	"github.com/rrhubenov/dune/internal/run"
	"github.com/rrhubenov/dune/internal/storage"
	"github.com/rrhubenov/dune/internal/store"
)

var ErrState = errors.New("invalid state")

// trimDelay batches fstrim after deletes.
const trimDelay = 30 * time.Second

type Config struct {
	Storage *storage.Store
	DB      *store.Store
	Backend backend.Backend
	// VM processes run as UIDBase+slot (same for gid), so no two live VMs
	// share a uid.
	UIDBase int
	// AgentBinary is the guest agent installed into imported images
	// (static linux/amd64). Empty: images get no agent.
	AgentBinary string
	// Events, if set, is told about every change.
	Events *events.Hub
}

type Manager struct {
	cfg Config

	mu        sync.Mutex
	locks     map[string]*sync.Mutex // per VM / snapshot id
	trimTimer *time.Timer
}

type CreateOpts = model.CreateVMRequest

func New(cfg Config) *Manager {
	return &Manager{cfg: cfg, locks: map[string]*sync.Mutex{}}
}

// lock serializes operations on one VM or snapshot.
func (m *Manager) lock(id string) func() {
	m.mu.Lock()
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Manager) imageDir(name string) string { return m.cfg.Storage.Path("images", name) }
func (m *Manager) snapDir(id string) string    { return m.cfg.Storage.Path("snapshots", id) }

func (m *Manager) machine(vm store.VM) backend.Machine {
	id := m.cfg.UIDBase + vm.NetSlot
	return backend.Machine{
		ID: vm.ID, VCPUs: vm.VCPUs, MemMiB: vm.MemMiB,
		NetNS: netns.Path(vm.ID), UID: id, GID: id, Hostname: hostname(vm.ID),
	}
}

func (m *Manager) notify(topics ...string) { m.cfg.Events.Publish(topics...) }

func hostname(id string) string { return "dune-" + id }

func (m *Manager) agent(id string) *agent.Client {
	return agent.NewClient(func(ctx context.Context) (net.Conn, error) {
		return m.cfg.Backend.DialAgent(ctx, id, agent.Port)
	})
}

// Recover reconciles the DB with reality after a daemon (re)start: VMs whose
// process is gone are marked stopped. Their files stay until deleted.
func (m *Manager) Recover() error {
	vms, err := m.cfg.DB.ListVMs()
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if vm.State == store.VMStopped || m.cfg.Backend.Alive(vm.ID, vm.PID) {
			continue
		}
		slog.Warn("vm process gone; marking stopped", "vm", vm.ID, "pid", vm.PID)
		if err := m.cfg.DB.SetVMState(vm.ID, store.VMStopped, 0); err != nil {
			return err
		}
		m.notify(events.VMs)
	}
	return nil
}

func (m *Manager) Images() ([]store.Image, error) { return m.cfg.DB.ListImages() }

// ImportImage copies a kernel and rootfs into the store under name and
// prepares the rootfs for dune's guest network (see package image).
func (m *Manager) ImportImage(name, kernel, rootfs string) (store.Image, error) {
	defer m.lock("image:" + name)()
	dir := m.imageDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Image{}, err
	}
	im := store.Image{
		Name:      name,
		Kernel:    m.cfg.Storage.Path("images", name, "vmlinux"),
		Rootfs:    m.cfg.Storage.Path("images", name, "rootfs.ext4"),
		CreatedAt: time.Now(),
	}
	for _, c := range []struct{ src, dst string }{{kernel, im.Kernel}, {rootfs, im.Rootfs}} {
		// Reflinks when src is on the store, sparse copy otherwise.
		if err := run.Cmd("cp", "--reflink=auto", "--sparse=always", c.src, c.dst+".tmp"); err != nil {
			return im, err
		}
		if c.dst == im.Rootfs {
			if err := image.Prepare(c.dst+".tmp", dir, image.DefaultNameservers, m.cfg.AgentBinary); err != nil {
				os.Remove(c.dst + ".tmp")
				return im, fmt.Errorf("prepare rootfs: %w", err)
			}
		}
		if err := os.Chmod(c.dst+".tmp", 0o444); err != nil {
			return im, err
		}
		if err := os.Rename(c.dst+".tmp", c.dst); err != nil {
			return im, err
		}
	}
	defer m.notify(events.Images, events.Storage)
	return im, m.cfg.DB.PutImage(im)
}

func (m *Manager) Create(ctx context.Context, opts CreateOpts) (store.VM, error) {
	if opts.VCPUs <= 0 {
		opts.VCPUs = 1
	}
	if opts.MemMiB <= 0 {
		opts.MemMiB = 1024
	}
	im, err := m.cfg.DB.GetImage(opts.Image)
	if err != nil {
		return store.VM{}, err
	}
	vm, err := m.cfg.DB.CreateVM(store.VM{Image: im.Name, State: store.VMStopped, VCPUs: opts.VCPUs, MemMiB: opts.MemMiB})
	if err != nil {
		return vm, err
	}
	defer m.notify(events.VMs)
	defer m.lock(vm.ID)()
	mach := m.machine(vm)
	mach.Kernel, mach.Rootfs = im.Kernel, im.Rootfs
	pid, err := m.start(ctx, vm, func() (int, error) { return m.cfg.Backend.Boot(ctx, mach) })
	if err != nil {
		return vm, err
	}
	vm.State, vm.PID = store.VMRunning, pid
	return vm, nil
}

// start sets up networking and runs launch; on failure it removes everything,
// including the VM's DB row.
func (m *Manager) start(ctx context.Context, vm store.VM, launch func() (int, error)) (int, error) {
	mach := m.machine(vm)
	err := netns.Create(vm.ID, vm.NetSlot, mach.UID, mach.GID)
	var pid int
	if err == nil {
		pid, err = launch()
	}
	if err == nil {
		err = m.cfg.DB.SetVMState(vm.ID, store.VMRunning, pid)
	}
	if err != nil {
		m.cfg.Backend.Stop(context.Background(), vm.ID, pid)
		netns.Delete(vm.ID)
		m.cfg.DB.DeleteVM(vm.ID)
		return 0, fmt.Errorf("start vm %s: %w", vm.ID, err)
	}
	return pid, nil
}

func (m *Manager) Get(id string) (store.VM, error) { return m.cfg.DB.GetVM(id) }

func (m *Manager) List() ([]store.VM, error) { return m.cfg.DB.ListVMs() }

func (m *Manager) Pause(ctx context.Context, id string) (store.VM, error) {
	return m.setRunState(ctx, id, store.VMRunning, store.VMPaused, m.cfg.Backend.Pause)
}

func (m *Manager) Resume(ctx context.Context, id string) (store.VM, error) {
	return m.setRunState(ctx, id, store.VMPaused, store.VMRunning, m.cfg.Backend.Resume)
}

func (m *Manager) setRunState(ctx context.Context, id, from, to string, op func(context.Context, string) error) (store.VM, error) {
	defer m.lock(id)()
	vm, err := m.cfg.DB.GetVM(id)
	if err != nil {
		return vm, err
	}
	if vm.State == to {
		return vm, nil
	}
	if vm.State != from {
		return vm, fmt.Errorf("vm %s is %s: %w", id, vm.State, ErrState)
	}
	if err := op(ctx, id); err != nil {
		return vm, err
	}
	vm.State = to
	defer m.notify(events.VMs)
	return vm, m.cfg.DB.SetVMState(id, to, vm.PID)
}

// Pin snapshots a VM. kind "" means diff if the VM has a previous snapshot
// (its head), else full. A diff with no head is upgraded to full.
func (m *Manager) Pin(ctx context.Context, id string, kind backend.SnapshotKind) (store.Snapshot, error) {
	defer m.lock(id)()
	vm, err := m.cfg.DB.GetVM(id)
	if err != nil {
		return store.Snapshot{}, err
	}
	if vm.State != store.VMRunning && vm.State != store.VMPaused {
		return store.Snapshot{}, fmt.Errorf("vm %s is %s: %w", id, vm.State, ErrState)
	}
	if kind == "" {
		kind = backend.Diff
	}
	if vm.Head == "" {
		kind = backend.Full
	}
	sn := store.Snapshot{VMID: vm.ID, Kind: string(kind), MemMiB: vm.MemMiB, VCPUs: vm.VCPUs, Image: vm.Image}
	if kind == backend.Diff {
		sn.Parent = vm.Head
	}
	sn, err = m.cfg.DB.CreateSnapshot(sn)
	if err != nil {
		return sn, err
	}
	defer m.notify(events.Snapshots, events.VMs, events.Storage)
	err = m.cfg.Backend.Snapshot(ctx, vm.ID, m.snapDir(sn.ID), kind, vm.State == store.VMRunning)
	if err != nil {
		os.RemoveAll(m.snapDir(sn.ID))
		m.cfg.DB.DeleteSnapshot(sn.ID)
		// A failed snapshot may already have reset Firecracker's dirty-page
		// bitmap, so a diff on the old head could miss pages. Force full.
		m.cfg.DB.SetVMHead(vm.ID, "")
		return sn, fmt.Errorf("pin vm %s: %w", id, err)
	}
	return sn, m.cfg.DB.SetVMHead(vm.ID, sn.ID)
}

// Fork starts count new VMs from a snapshot. On error it returns the VMs
// started so far alongside the error.
func (m *Manager) Fork(ctx context.Context, snapID string, count int) ([]store.VM, error) {
	if count <= 0 {
		count = 1
	}
	defer m.lock(snapID)()
	chain, err := m.cfg.DB.Chain(snapID)
	if err != nil {
		return nil, err
	}
	dirs := make([]string, len(chain))
	for i, sn := range chain {
		dirs[i] = m.snapDir(sn.ID)
	}
	sn := chain[len(chain)-1]
	defer m.notify(events.VMs, events.Snapshots)
	var out []store.VM
	for range count {
		vm, err := m.cfg.DB.CreateVM(store.VM{
			Image: sn.Image, State: store.VMStopped, VCPUs: sn.VCPUs, MemMiB: sn.MemMiB,
			Origin: sn.ID, Head: sn.ID, // dirty tracking restarts at restore, so diffs build on sn
		})
		if err != nil {
			return out, err
		}
		unlock := m.lock(vm.ID)
		pid, err := m.start(ctx, vm, func() (int, error) { return m.cfg.Backend.Restore(ctx, m.machine(vm), dirs) })
		unlock()
		if err != nil {
			return out, err
		}
		vm.State, vm.PID = store.VMRunning, pid
		// Non-fatal: the fork runs either way, e.g. from images without an
		// agent. But without fresh entropy it shares RNG state with siblings.
		if err := m.fixup(ctx, vm.ID); err != nil {
			slog.Warn("post-restore fixup failed", "vm", vm.ID, "err", err)
		}
		out = append(out, vm)
	}
	return out, nil
}

// fixup gives a restored guest the current time, fresh entropy and its own
// hostname. The agent's listener lives in the restored memory, so it answers
// right after resume; the retry covers vsock transport reset after restore.
func (m *Manager) fixup(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	seed := make([]byte, 64)
	rand.Read(seed)
	c := m.agent(id)
	for {
		err := c.Fixup(ctx, agent.FixupRequest{UnixNano: time.Now().UnixNano(), Entropy: seed, Hostname: hostname(id)})
		if err == nil || ctx.Err() != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// OpenPTY starts a login shell on a terminal in a running VM.
func (m *Manager) OpenPTY(ctx context.Context, id string, cols, rows uint16) (*agent.PTY, error) {
	vm, err := m.cfg.DB.GetVM(id)
	if err != nil {
		return nil, err
	}
	if vm.State != store.VMRunning {
		return nil, fmt.Errorf("vm %s is %s: %w", id, vm.State, ErrState)
	}
	p, err := m.agent(id).OpenPTY(ctx, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("guest agent: %w", err)
	}
	return p, nil
}

// Exec runs a command in a running VM through its guest agent.
func (m *Manager) Exec(ctx context.Context, id string, req agent.ExecRequest) (agent.ExecResult, error) {
	vm, err := m.cfg.DB.GetVM(id)
	if err != nil {
		return agent.ExecResult{}, err
	}
	if vm.State != store.VMRunning {
		return agent.ExecResult{}, fmt.Errorf("vm %s is %s: %w", id, vm.State, ErrState)
	}
	if len(req.Argv) == 0 {
		return agent.ExecResult{}, errors.New("empty command")
	}
	res, err := m.agent(id).Exec(ctx, req)
	if err != nil {
		return res, fmt.Errorf("guest agent: %w", err)
	}
	return res, nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	defer m.lock(id)()
	vm, err := m.cfg.DB.GetVM(id)
	if err != nil {
		return err
	}
	if err := m.cfg.Backend.Stop(ctx, vm.ID, vm.PID); err != nil {
		return err
	}
	if err := netns.Delete(vm.ID); err != nil {
		return err
	}
	if err := m.cfg.DB.DeleteVM(vm.ID); err != nil {
		return err
	}
	m.notify(events.VMs, events.Snapshots)
	m.trimSoon()
	return nil
}

func (m *Manager) Snapshots() ([]store.Snapshot, error) { return m.cfg.DB.ListSnapshots() }

// DeleteSnapshot refuses while diffs build on it or a VM's next diff would.
// Running forks are unaffected: their jails hold hard links / reflinks.
func (m *Manager) DeleteSnapshot(id string) error {
	defer m.lock(id)()
	if err := m.cfg.DB.DeleteSnapshot(id); err != nil {
		return err
	}
	if err := os.RemoveAll(m.snapDir(id)); err != nil {
		return err
	}
	m.notify(events.Snapshots)
	m.trimSoon()
	return nil
}

// trimSoon schedules an fstrim, coalescing bursts of deletes into one.
func (m *Manager) trimSoon() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trimTimer != nil {
		m.trimTimer.Reset(trimDelay)
		return
	}
	m.trimTimer = time.AfterFunc(trimDelay, func() {
		m.mu.Lock()
		m.trimTimer = nil
		m.mu.Unlock()
		if err := m.cfg.Storage.Trim(); err != nil {
			slog.Error("fstrim", "err", err)
		}
		m.notify(events.Storage)
	})
}
