package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rrhubenov/dune/internal/backend"
	"github.com/rrhubenov/dune/internal/netns"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestBootSnapshotFork boots a VM, snapshots it (full, then diff) and forks
// two copies from the diff. Needs root, Firecracker release binaries, a kernel
// and rootfs, and a work dir on a reflink-capable fs. Run with DUNE_TEST_FC=1.
func TestBootSnapshotFork(t *testing.T) {
	if os.Getenv("DUNE_TEST_FC") == "" {
		t.Skip("set DUNE_TEST_FC=1 to run (needs root, Firecracker, reflink fs)")
	}
	rel := env("DUNE_TEST_FC_RELEASE", "/srv/vms/fc/release-v1.17.0-x86_64")
	kernel := env("DUNE_TEST_KERNEL", "/srv/vms/fc/vmlinux-6.1.188")
	rootfs := env("DUNE_TEST_ROOTFS", "/srv/vms/fc/ubuntu.ext4")
	work := env("DUNE_TEST_FC_DIR", "/srv/vms/dunetest")

	os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	b := New(Config{
		Firecracker:    filepath.Join(rel, "firecracker-v1.17.0-x86_64"),
		Jailer:         filepath.Join(rel, "jailer-v1.17.0-x86_64"),
		SnapshotEditor: filepath.Join(rel, "snapshot-editor-v1.17.0-x86_64"),
		ChrootBase:     filepath.Join(work, "jail"),
	})
	ctx := context.Background()
	if err := netns.HostSetup(); err != nil {
		t.Fatal(err)
	}

	machine := func(id string, slot int) backend.Machine {
		t.Helper()
		netns.Delete(id)
		if err := netns.Create(id, slot, 100000+slot, 100000+slot); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := b.Stop(ctx, id, 0); err != nil {
				t.Error(err)
			}
			if err := netns.Delete(id); err != nil {
				t.Error(err)
			}
		})
		return backend.Machine{
			ID: id, VCPUs: 1, MemMiB: 512, Kernel: kernel, Rootfs: rootfs,
			NetNS: netns.Path(id), UID: 100000 + slot, GID: 100000 + slot,
		}
	}

	// send types a line on the guest's serial console. Markers are split
	// with "" so the echoed command line itself never matches.
	send := func(id, line string) {
		t.Helper()
		if _, err := fmt.Fprintf(b.ConsoleInput(id), "%s\n", line); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(id, want string, timeout time.Duration, poke func()) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		next := time.Now()
		for time.Now().Before(deadline) {
			if poke != nil && time.Now().After(next) {
				poke()
				next = time.Now().Add(time.Second)
			}
			out, _ := os.ReadFile(b.ConsolePath(id))
			if strings.Contains(string(out), want) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		out, _ := os.ReadFile(b.ConsolePath(id))
		t.Fatalf("vm %s: %q not seen within %s; console tail:\n%s", id, want, timeout, lastLines(string(out), 30))
	}
	netCheck := func(id, marker string) {
		t.Helper()
		send(id, fmt.Sprintf("ping -c1 -W3 1.1.1.1 >/dev/null && echo %s\"\"OK || echo %s\"\"FAIL", marker, marker))
		waitFor(id, marker+"OK", 15*time.Second, nil)
	}

	// Boot the parent.
	start := time.Now()
	parent := machine("fct-p", 16001)
	if _, err := b.Boot(ctx, parent); err != nil {
		t.Fatal(err)
	}
	waitFor(parent.ID, "READY", 60*time.Second, func() { send(parent.ID, `echo REA""DY`) })
	t.Logf("boot to shell: %s", time.Since(start).Round(time.Millisecond))
	// BETA: route + DNS get baked into the base image later.
	send(parent.ID, "ip route replace default via 172.16.0.1; echo nameserver 1.1.1.1 > /etc/resolv.conf")
	netCheck(parent.ID, "PNET")

	snaps := filepath.Join(work, "snaps")
	start = time.Now()
	if err := b.Snapshot(ctx, parent.ID, filepath.Join(snaps, "base"), backend.Full, true); err != nil {
		t.Fatal(err)
	}
	t.Logf("full snapshot: %s", time.Since(start).Round(time.Millisecond))

	send(parent.ID, `echo forked-state > /root/x; sync; echo WRO""TE`)
	waitFor(parent.ID, "WROTE", 10*time.Second, nil)
	start = time.Now()
	if err := b.Snapshot(ctx, parent.ID, filepath.Join(snaps, "d1"), backend.Diff, false); err != nil {
		t.Fatal(err)
	}
	t.Logf("diff snapshot: %s", time.Since(start).Round(time.Millisecond))
	chain := []string{filepath.Join(snaps, "base"), filepath.Join(snaps, "d1")}

	// Fork two copies from the diff.
	forks := []backend.Machine{machine("fct-a", 16002), machine("fct-b", 16003)}
	for _, f := range forks {
		start = time.Now()
		if _, err := b.Restore(ctx, f, chain); err != nil {
			t.Fatal(err)
		}
		t.Logf("restore %s: %s", f.ID, time.Since(start).Round(time.Millisecond))
	}
	for _, f := range forks {
		send(f.ID, "cat /root/x")
		waitFor(f.ID, "forked-state", 10*time.Second, nil)
		netCheck(f.ID, "FNET")
	}

	// Disks are private: a file written in fork a is absent in fork b.
	send(forks[0].ID, `touch /root/only-a; sync; echo TOU""CHED`)
	waitFor(forks[0].ID, "TOUCHED", 10*time.Second, nil)
	send(forks[1].ID, `test -e /root/only-a && echo LEA""KED || echo ISOLA""TED`)
	waitFor(forks[1].ID, "ISOLATED", 10*time.Second, nil)
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
