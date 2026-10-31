package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rrhubenov/dune/internal/agent"
	"github.com/rrhubenov/dune/internal/backend"
	"github.com/rrhubenov/dune/internal/backend/firecracker"
	"github.com/rrhubenov/dune/internal/netns"
	"github.com/rrhubenov/dune/internal/storage"
	"github.com/rrhubenov/dune/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestLifecycle drives the manager end to end: import, boot, pin full + diff,
// fork, pin a fork (a chain spanning two VMs), fork the fork, delete.
// Needs root and Firecracker. Run with DUNE_TEST_FC=1.
func TestLifecycle(t *testing.T) {
	if os.Getenv("DUNE_TEST_FC") == "" {
		t.Skip("set DUNE_TEST_FC=1 to run (needs root, Firecracker, reflink fs)")
	}
	rel := env("DUNE_TEST_FC_RELEASE", "/srv/vms/fc/release-v1.17.0-x86_64")
	work := env("DUNE_TEST_VM_DIR", "/srv/vms/dunetest-vm")
	os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(work, "dune.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) // registered first, so it runs after the VM cleanup below
	db.MinSlot = 15000
	st := &storage.Store{Mount: work} // reuse the spike store; no image of our own
	fc := firecracker.New(firecracker.Config{
		Firecracker:    filepath.Join(rel, "firecracker-v1.17.0-x86_64"),
		Jailer:         filepath.Join(rel, "jailer-v1.17.0-x86_64"),
		SnapshotEditor: filepath.Join(rel, "snapshot-editor-v1.17.0-x86_64"),
		ChrootBase:     st.Path("jail"),
	})
	agentBin, err := filepath.Abs(env("DUNE_TEST_AGENT", "bin/dune-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agentBin); err != nil {
		t.Fatalf("guest agent (make build): %v", err)
	}
	// Slots start at 1; keep test VM uids away from real ones.
	m := New(Config{Storage: st, DB: db, Backend: fc, UIDBase: 200000, AgentBinary: agentBin})
	ctx := context.Background()
	if err := netns.HostSetup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		vms, err := m.List()
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}
		for _, vm := range vms {
			if err := m.Delete(ctx, vm.ID); err != nil {
				t.Error(err)
			}
		}
	})

	send := func(id, line string) {
		t.Helper()
		if _, err := fmt.Fprintf(fc.ConsoleInput(id), "%s\n", line); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(id, want string, poke func()) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		next := time.Now()
		for time.Now().Before(deadline) {
			if poke != nil && time.Now().After(next) {
				poke()
				next = time.Now().Add(time.Second)
			}
			if out, _ := os.ReadFile(fc.ConsolePath(id)); strings.Contains(string(out), want) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		out, _ := os.ReadFile(fc.ConsolePath(id))
		t.Fatalf("vm %s: %q not seen; console:\n%s", id, want, out)
	}
	// run types cmd and waits for a marker printed after it.
	marker := 0
	run := func(id, cmd string) {
		t.Helper()
		marker++
		send(id, fmt.Sprintf(`%s; echo DO""NE%d`, cmd, marker))
		waitFor(id, fmt.Sprintf("DONE%d", marker), nil)
	}
	timed := func(what string, f func()) {
		start := time.Now()
		f()
		t.Logf("%s: %s", what, time.Since(start).Round(time.Millisecond))
	}

	timed("import image", func() {
		_, err = m.ImportImage("ubuntu", env("DUNE_TEST_KERNEL", "/srv/vms/fc/vmlinux-6.1.188"),
			env("DUNE_TEST_ROOTFS", "/srv/vms/fc/ubuntu.ext4"))
	})
	if err != nil {
		t.Fatal(err)
	}

	var parent store.VM
	timed("create", func() { parent, err = m.Create(ctx, CreateOpts{Image: "ubuntu", MemMiB: 512}) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(parent.ID, "READY", func() { send(parent.ID, `echo REA""DY`) })
	run(parent.ID, "echo base > /root/stage")

	// exec runs argv via the guest agent and returns trimmed stdout.
	exec := func(id string, stdin string, argv ...string) (string, int) {
		t.Helper()
		res, err := m.Exec(ctx, id, agent.ExecRequest{Argv: argv, Stdin: []byte(stdin)})
		if err != nil {
			t.Fatalf("exec %s %v: %v", id, argv, err)
		}
		if res.Error != "" {
			t.Fatalf("exec %s %v: %s", id, argv, res.Error)
		}
		return strings.TrimSpace(string(res.Stdout)), res.ExitCode
	}
	if h, _ := exec(parent.ID, "", "hostname"); h != "dune-"+parent.ID {
		t.Errorf("parent hostname = %q", h)
	}
	if _, code := exec(parent.ID, "", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if out, _ := exec(parent.ID, "piped", "cat"); out != "piped" {
		t.Errorf("stdin passthrough = %q", out)
	}

	// Interactive terminal: run a command, resize, exit with a code.
	pty, err := m.OpenPTY(ctx, parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	var screen strings.Builder
	expect := func(want string) int {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(screen.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("pty: %q not seen; output:\n%s", want, screen.String())
			}
			data, exited, code, err := pty.Next()
			if err != nil {
				t.Fatalf("pty: %v; output:\n%s", err, screen.String())
			}
			if exited {
				return code
			}
			screen.Write(data)
		}
		return 0
	}
	pty.Write([]byte("echo PTY$((40+2))\n"))
	expect("PTY42")
	pty.Resize(100, 30)
	pty.Write([]byte("stty size\n"))
	expect("30 100")
	pty.Write([]byte("exit 3\n"))
	if code := expect("\x00never"); code != 3 {
		t.Errorf("pty exit code = %d, want 3", code)
	}
	pty.Close()

	var base, d1 store.Snapshot
	timed("pin (auto: full)", func() { base, err = m.Pin(ctx, parent.ID, "") })
	if err != nil || base.Kind != store.SnapFull {
		t.Fatalf("pin = %+v, %v", base, err)
	}
	run(parent.ID, "echo d1 > /root/stage; sync")
	timed("pin (auto: diff)", func() { d1, err = m.Pin(ctx, parent.ID, "") })
	if err != nil || d1.Kind != store.SnapDiff || d1.Parent != base.ID {
		t.Fatalf("pin = %+v, %v", d1, err)
	}

	var forks []store.VM
	timed("fork 3", func() { forks, err = m.Fork(ctx, d1.ID, 3) })
	if err != nil || len(forks) != 3 {
		t.Fatalf("fork = %d, %v", len(forks), err)
	}
	uuids := map[string]bool{}
	for _, f := range forks {
		if f.Head != d1.ID || f.Origin != d1.ID {
			t.Errorf("fork %s head/origin = %s/%s, want %s", f.ID, f.Head, f.Origin, d1.ID)
		}
		run(f.ID, `grep -q d1 /root/stage && echo STA""GE-OK`)
		waitFor(f.ID, "STAGE-OK", nil)
		// Post-restore fixup: own hostname, reseeded RNG, current clock.
		if h, _ := exec(f.ID, "", "hostname"); h != "dune-"+f.ID {
			t.Errorf("fork %s hostname = %q", f.ID, h)
		}
		u, _ := exec(f.ID, "", "cat", "/proc/sys/kernel/random/uuid")
		if uuids[u] {
			t.Errorf("fork %s: uuid %s repeats across forks (RNG not reseeded)", f.ID, u)
		}
		uuids[u] = true
		ts, _ := exec(f.ID, "", "date", "+%s")
		if sec, _ := strconv.ParseInt(ts, 10, 64); time.Since(time.Unix(sec, 0)).Abs() > 5*time.Second {
			t.Errorf("fork %s clock %s, host %d", f.ID, ts, time.Now().Unix())
		}
	}

	// Pause/resume a fork.
	f := forks[0]
	if vm, err := m.Pause(ctx, f.ID); err != nil || vm.State != store.VMPaused {
		t.Fatalf("pause = %+v, %v", vm, err)
	}
	if _, err := m.Pause(ctx, f.ID); err != nil {
		t.Fatalf("pause again: %v", err)
	}
	if vm, err := m.Resume(ctx, f.ID); err != nil || vm.State != store.VMRunning {
		t.Fatalf("resume = %+v, %v", vm, err)
	}

	// Diff of a fork builds on the snapshot it was forked from.
	run(f.ID, "echo f1 > /root/stage; sync")
	var f1 store.Snapshot
	timed("pin fork (diff)", func() { f1, err = m.Pin(ctx, f.ID, "") })
	if err != nil || f1.Kind != store.SnapDiff || f1.Parent != d1.ID {
		t.Fatalf("pin fork = %+v, %v", f1, err)
	}
	var grand []store.VM
	timed("fork the fork", func() { grand, err = m.Fork(ctx, f1.ID, 1) })
	if err != nil {
		t.Fatal(err)
	}
	run(grand[0].ID, `grep -q f1 /root/stage && echo GRA""ND-OK`)
	waitFor(grand[0].ID, "GRAND-OK", nil)
	// Route comes from the kernel ip= arg, DNS from image import: no setup.
	run(grand[0].ID, `ping -c1 -W3 one.one.one.one >/dev/null && echo GNET""-OK`)
	waitFor(grand[0].ID, "GNET-OK", nil)

	// d1 has children (f1) and heads (other forks): refuse.
	if err := m.DeleteSnapshot(d1.ID); !errors.Is(err, store.ErrHasChildren) {
		t.Fatalf("delete d1: %v", err)
	}
	// Explicit full pin on a VM that has a head.
	sn, err := m.Pin(ctx, forks[1].ID, backend.Full)
	if err != nil || sn.Kind != store.SnapFull || sn.Parent != "" {
		t.Fatalf("full pin = %+v, %v", sn, err)
	}

	vms, _ := m.List()
	if len(vms) != 5 {
		t.Fatalf("list = %d VMs, want 5", len(vms))
	}
}
