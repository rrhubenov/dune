// Package firecracker runs VMs as jailed Firecracker processes.
//
// Every VM gets a jail (chroot) at <ChrootBase>/<firecracker binary name>/<id>/root:
//
//	/rootfs.ext4         private disk, reflinked from the image or a snapshot
//	/vmlinux             kernel, hard link (boot only)
//	/vmstate, /mem       snapshot files, hard links, read-only (restore only)
//	/run/api.sock        Firecracker API socket
//
// Snapshots record the disk path as Firecracker saw it ("/rootfs.ext4"), so
// every fork restores with the same path but, thanks to its own chroot, a
// different file. ChrootBase must be on the reflink-capable store, next to the
// images and snapshots, so all of these copies are reflinks or hard links.
//
// Next to root/ (outside the chroot) the backend keeps console.log (guest
// serial output) and firecracker.log (Firecracker's own stderr).
package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rrhubenov/dune/internal/backend"
	"github.com/rrhubenov/dune/internal/netns"
	"github.com/rrhubenov/dune/internal/run"
	"github.com/rrhubenov/dune/internal/storage"
)

const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"

type Config struct {
	Firecracker    string // absolute path; its name must contain "firecracker" (jailer rule)
	Jailer         string
	SnapshotEditor string
	ChrootBase     string // on the store, e.g. /srv/dune/store/jail
	BootArgs       string // DefaultBootArgs if empty
}

// Paths inside the chroot.
const (
	jailDisk    = "/rootfs.ext4"
	jailKernel  = "/vmlinux"
	jailVMState = "/vmstate"
	jailMem     = "/mem"
	jailSock    = "/run/api.sock"
	snapVMState = "/snap.vmstate" // where Firecracker writes a new snapshot
	snapMem     = "/snap.mem"
)

// Files in a snapshot dir.
const (
	VMStateFile = "vmstate"
	MemFile     = "mem"      // full memory, or only dirty pages for a diff
	MergedFile  = "mem.full" // diff applied onto its parent's full memory (cached)
	DiskFile    = "disk.ext4"
)

type Backend struct {
	cfg Config

	mu    sync.Mutex
	procs map[string]*proc
}

// proc is a VM process started by this daemon instance.
type proc struct {
	pid     int
	done    chan struct{} // closed once the process has exited and been reaped
	console io.WriteCloser
}

var _ backend.Backend = (*Backend)(nil)

func New(cfg Config) *Backend {
	if cfg.BootArgs == "" {
		cfg.BootArgs = DefaultBootArgs
	}
	return &Backend{cfg: cfg, procs: map[string]*proc{}}
}

// JailDir is the per-VM dir holding root/ and the logs.
func (b *Backend) JailDir(id string) string {
	return filepath.Join(b.cfg.ChrootBase, filepath.Base(b.cfg.Firecracker), id)
}

func (b *Backend) root(id string) string { return filepath.Join(b.JailDir(id), "root") }

// hostPath maps a path inside the VM's chroot to the host.
func (b *Backend) hostPath(id, p string) string { return filepath.Join(b.root(id), p) }

func (b *Backend) client(id string) *client { return newClient(b.hostPath(id, jailSock)) }

// ConsolePath is the file the guest's serial console is written to.
func (b *Backend) ConsolePath(id string) string { return filepath.Join(b.JailDir(id), "console.log") }

// ConsoleInput returns a writer to the guest's serial console (ttyS0), or nil
// if the VM was not started by this daemon instance.
// BETA: replaced by a vsock guest agent later; lost on daemon restart.
func (b *Backend) ConsoleInput(id string) io.Writer {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.procs[id]; p != nil {
		return p.console
	}
	return nil
}

func (b *Backend) Boot(ctx context.Context, m backend.Machine) (pid int, err error) {
	if err := b.prepareJail(m); err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			b.Stop(context.Background(), m.ID, 0)
		}
	}()
	if err := b.privateDisk(m, m.Rootfs); err != nil {
		return 0, err
	}
	if err := os.Link(m.Kernel, b.hostPath(m.ID, jailKernel)); err != nil {
		return 0, err
	}
	pid, err = b.start(ctx, m)
	if err != nil {
		return 0, err
	}
	c := b.client(m.ID)
	steps := []struct {
		path string
		body any
	}{
		{"/machine-config", machineConfig{VCPUCount: m.VCPUs, MemSizeMiB: m.MemMiB, TrackDirtyPages: true}},
		{"/boot-source", bootSource{KernelImagePath: jailKernel, BootArgs: b.cfg.BootArgs}},
		{"/drives/rootfs", drive{DriveID: "rootfs", PathOnHost: jailDisk, IsRootDevice: true}},
		{"/network-interfaces/eth0", networkInterface{IfaceID: "eth0", GuestMAC: netns.GuestMAC, HostDevName: netns.TapName}},
		{"/actions", action{ActionType: "InstanceStart"}},
	}
	for _, s := range steps {
		if err := c.put(ctx, s.path, s.body); err != nil {
			return 0, err
		}
	}
	return pid, nil
}

func (b *Backend) Restore(ctx context.Context, m backend.Machine, chain []string) (pid int, err error) {
	if len(chain) == 0 {
		return 0, errors.New("empty snapshot chain")
	}
	mem, err := b.fullMemory(chain)
	if err != nil {
		return 0, err
	}
	last := chain[len(chain)-1]
	if err := b.prepareJail(m); err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			b.Stop(context.Background(), m.ID, 0)
		}
	}()
	if err := b.privateDisk(m, filepath.Join(last, DiskFile)); err != nil {
		return 0, err
	}
	// Shared, read-only: Firecracker maps memory privately (copy-on-write),
	// so all forks of a snapshot share one file in the host page cache.
	if err := os.Link(filepath.Join(last, VMStateFile), b.hostPath(m.ID, jailVMState)); err != nil {
		return 0, err
	}
	if err := os.Link(mem, b.hostPath(m.ID, jailMem)); err != nil {
		return 0, err
	}
	pid, err = b.start(ctx, m)
	if err != nil {
		return 0, err
	}
	err = b.client(m.ID).put(ctx, "/snapshot/load", snapshotLoad{
		SnapshotPath:    jailVMState,
		MemBackend:      memBackend{BackendType: "File", BackendPath: jailMem},
		TrackDirtyPages: true,
		ResumeVM:        true,
		ClockRealtime:   true,
	})
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// fullMemory returns a full memory file for the last snapshot in chain,
// merging diffs onto reflinked copies of their parent's full memory and
// caching the result in each diff's dir.
func (b *Backend) fullMemory(chain []string) (string, error) {
	if len(chain) == 1 {
		return filepath.Join(chain[0], MemFile), nil
	}
	dir := chain[len(chain)-1]
	merged := filepath.Join(dir, MergedFile)
	if _, err := os.Stat(merged); err == nil {
		return merged, nil
	}
	parent, err := b.fullMemory(chain[:len(chain)-1])
	if err != nil {
		return "", err
	}
	tmp := merged + ".tmp." + strconv.Itoa(os.Getpid()) + "." + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := storage.Reflink(parent, tmp); err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if err := run.Cmd(b.cfg.SnapshotEditor, "edit-memory", "rebase",
		"--memory-path", tmp, "--diff-path", filepath.Join(dir, MemFile)); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return "", err
	}
	return merged, os.Rename(tmp, merged)
}

func (b *Backend) Pause(ctx context.Context, id string) error {
	return b.client(id).patch(ctx, "/vm", vmState{State: "Paused"})
}

func (b *Backend) Resume(ctx context.Context, id string) error {
	return b.client(id).patch(ctx, "/vm", vmState{State: "Resumed"})
}

// Snapshot pauses the VM, writes vmstate + memory, reflinks the disk and,
// if resume, resumes it (also when the snapshot fails).
func (b *Backend) Snapshot(ctx context.Context, id, dir string, kind backend.SnapshotKind, resume bool) (err error) {
	typ := "Full"
	if kind == backend.Diff {
		typ = "Diff"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	c := b.client(id)
	if err := b.Pause(ctx, id); err != nil {
		return err
	}
	if resume {
		defer func() {
			if rerr := b.Resume(context.Background(), id); err == nil {
				err = rerr
			}
		}()
	}
	if err := c.put(ctx, "/snapshot/create", snapshotCreate{
		SnapshotType: typ, SnapshotPath: snapVMState, MemFilePath: snapMem,
	}); err != nil {
		return err
	}
	// The VM is paused, so its disk is quiescent: no new writes until resume.
	if err := storage.Reflink(b.hostPath(id, jailDisk), filepath.Join(dir, DiskFile)); err != nil {
		return err
	}
	moves := []struct{ from, to string }{
		{b.hostPath(id, snapVMState), filepath.Join(dir, VMStateFile)},
		{b.hostPath(id, snapMem), filepath.Join(dir, MemFile)},
	}
	for _, mv := range moves {
		if err := os.Rename(mv.from, mv.to); err != nil {
			return err
		}
	}
	// Snapshot files get hard-linked into other VMs' jails. Root-owned and
	// read-only, so no VM (even one later given this VM's uid) can modify them.
	for _, f := range []string{VMStateFile, MemFile, DiskFile} {
		p := filepath.Join(dir, f)
		if err := os.Chown(p, 0, 0); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o444); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Stop(ctx context.Context, id string, pid int) error {
	b.mu.Lock()
	p := b.procs[id]
	delete(b.procs, id)
	b.mu.Unlock()
	if p != nil {
		syscall.Kill(p.pid, syscall.SIGKILL)
		p.console.Close()
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return fmt.Errorf("vm %s: pid %d did not exit after SIGKILL", id, p.pid)
		}
	} else if b.Alive(id, pid) {
		// Not our child (daemon restarted); init reaps it, we poll.
		syscall.Kill(pid, syscall.SIGKILL)
		deadline := time.Now().Add(10 * time.Second)
		for b.Alive(id, pid) {
			if time.Now().After(deadline) {
				return fmt.Errorf("vm %s: pid %d did not exit after SIGKILL", id, pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return os.RemoveAll(b.JailDir(id))
}

// Alive checks /proc/<pid>/cmdline for the jailer's "--id <id>" arguments,
// which Firecracker keeps after the jailer execs it.
func (b *Backend) Alive(id string, pid int) bool {
	if pid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	return bytes.Contains(cmdline, []byte("\x00--id\x00"+id+"\x00"))
}

// privateDisk reflinks src into the VM's jail as its writable disk. The
// source may be a read-only snapshot file, so ownership and mode are set
// explicitly rather than inherited.
func (b *Backend) privateDisk(m backend.Machine, src string) error {
	dst := b.hostPath(m.ID, jailDisk)
	if err := storage.Reflink(src, dst); err != nil {
		return err
	}
	if err := os.Chown(dst, m.UID, m.GID); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}

// prepareJail creates the VM's chroot, owned by the VM's user so Firecracker
// can create its socket and snapshot files there.
func (b *Backend) prepareJail(m backend.Machine) error {
	if _, err := os.Stat(b.JailDir(m.ID)); err == nil {
		return fmt.Errorf("vm %s: jail %s already exists", m.ID, b.JailDir(m.ID))
	}
	runDir := b.hostPath(m.ID, "/run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	for _, d := range []string{b.root(m.ID), runDir} {
		if err := os.Chown(d, m.UID, m.GID); err != nil {
			return err
		}
	}
	return nil
}

// start launches the jailer, which sets up /dev nodes in the chroot, joins the
// netns, drops to m.UID/m.GID and execs Firecracker, then waits for the API
// socket. The process gets its own session so signals to the daemon (e.g.
// Ctrl-C in a terminal) don't reach VMs.
func (b *Backend) start(ctx context.Context, m backend.Machine) (int, error) {
	jail := b.JailDir(m.ID)
	console, err := os.OpenFile(filepath.Join(jail, "console.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return 0, err
	}
	defer console.Close()
	fclog, err := os.OpenFile(filepath.Join(jail, "firecracker.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return 0, err
	}
	defer fclog.Close()

	cmd := exec.Command(b.cfg.Jailer,
		"--id", m.ID,
		"--exec-file", b.cfg.Firecracker,
		"--uid", strconv.Itoa(m.UID),
		"--gid", strconv.Itoa(m.GID),
		"--chroot-base-dir", b.cfg.ChrootBase,
		"--netns", m.NetNS,
		"--",
		"--api-sock", jailSock,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stdout = console
	cmd.Stderr = fclog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return 0, err
	}
	p := &proc{pid: cmd.Process.Pid, done: make(chan struct{}), console: stdin}
	go func() {
		cmd.Wait()
		close(p.done)
	}()
	b.mu.Lock()
	b.procs[m.ID] = p
	b.mu.Unlock()

	sock := b.hostPath(m.ID, jailSock)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return p.pid, nil
		}
		select {
		case <-p.done:
			return 0, fmt.Errorf("vm %s: jailer exited: %s", m.ID, tail(filepath.Join(jail, "firecracker.log")))
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("vm %s: API socket did not appear: %s", m.ID, tail(filepath.Join(jail, "firecracker.log")))
		}
	}
}

// tail returns the last ~1 KiB of a log file, for error messages.
func tail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	if len(b) > 1024 {
		b = b[len(b)-1024:]
	}
	return string(b)
}
