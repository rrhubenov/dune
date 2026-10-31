package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	defer db.Close()
	st := &storage.Store{Mount: work} // reuse the spike store; no image of our own
	fc := firecracker.New(firecracker.Config{
		Firecracker:    filepath.Join(rel, "firecracker-v1.17.0-x86_64"),
		Jailer:         filepath.Join(rel, "jailer-v1.17.0-x86_64"),
		SnapshotEditor: filepath.Join(rel, "snapshot-editor-v1.17.0-x86_64"),
		ChrootBase:     st.Path("jail"),
	})
	// Slots start at 1; keep test VM uids away from real ones.
	m := New(Config{Storage: st, DB: db, Backend: fc, UIDBase: 200000})
	ctx := context.Background()
	if err := netns.HostSetup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		vms, _ := m.List()
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
	for _, f := range forks {
		if f.Head != d1.ID || f.Origin != d1.ID {
			t.Errorf("fork %s head/origin = %s/%s, want %s", f.ID, f.Head, f.Origin, d1.ID)
		}
		run(f.ID, `grep -q d1 /root/stage && echo STA""GE-OK`)
		waitFor(f.ID, "STAGE-OK", nil)
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
	run(grand[0].ID, `ip route replace default via 172.16.0.1; echo nameserver 1.1.1.1 > /etc/resolv.conf; ping -c1 -W3 1.1.1.1 >/dev/null && echo GNET""-OK`)
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
