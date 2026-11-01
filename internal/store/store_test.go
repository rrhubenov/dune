package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "dune.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.PutImage(Image{Name: "ubuntu", Kernel: "k", Rootfs: "r", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNetSlotReuse(t *testing.T) {
	s := open(t)
	var ids []string
	for i := 1; i <= 3; i++ {
		vm, err := s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
		if err != nil {
			t.Fatal(err)
		}
		if vm.NetSlot != i {
			t.Fatalf("slot = %d, want %d", vm.NetSlot, i)
		}
		ids = append(ids, vm.ID)
	}
	if err := s.DeleteVM(ids[1]); err != nil {
		t.Fatal(err)
	}
	vm, err := s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if vm.NetSlot != 2 {
		t.Fatalf("reused slot = %d, want 2", vm.NetSlot)
	}
	if err := s.DeleteVM(ids[0]); err != nil {
		t.Fatal(err)
	}
	vm, _ = s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
	if vm.NetSlot != 1 {
		t.Fatalf("reused slot = %d, want 1", vm.NetSlot)
	}
}

func TestChainAndDelete(t *testing.T) {
	s := open(t)
	vm, _ := s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
	base, _ := s.CreateSnapshot(Snapshot{VMID: vm.ID, Kind: SnapFull, Image: "ubuntu"})
	d1, _ := s.CreateSnapshot(Snapshot{VMID: vm.ID, Kind: SnapDiff, Parent: base.ID, Image: "ubuntu"})
	d2, _ := s.CreateSnapshot(Snapshot{VMID: vm.ID, Kind: SnapDiff, Parent: d1.ID, Image: "ubuntu"})
	s.SetVMHead(vm.ID, d2.ID)

	chain, err := s.Chain(d2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 3 || chain[0].ID != base.ID || chain[2].ID != d2.ID {
		t.Fatalf("chain = %+v", chain)
	}
	if err := s.DeleteSnapshot(d1.ID); !errors.Is(err, ErrHasChildren) {
		t.Fatalf("delete parent: %v", err)
	}
	if err := s.DeleteSnapshot(d2.ID); !errors.Is(err, ErrHasChildren) {
		t.Fatalf("delete head: %v", err)
	}
	s.SetVMHead(vm.ID, "")
	if err := s.DeleteSnapshot(d2.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMinSlot(t *testing.T) {
	s := open(t)
	s.MinSlot = 100
	for want := 100; want <= 101; want++ {
		vm, err := s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
		if err != nil || vm.NetSlot != want {
			t.Fatalf("slot = %d, %v; want %d", vm.NetSlot, err, want)
		}
	}
}

func TestHostPorts(t *testing.T) {
	s := open(t)
	a, _ := s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
	b, _ := s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
	hp := func(vm string, guest int, avoid func(int) bool) int {
		t.Helper()
		p, err := s.HostPort(vm, guest, 20000, 20002, avoid)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := hp(a.ID, 5173, nil); p != 20000 {
		t.Fatalf("first = %d", p)
	}
	if p := hp(a.ID, 5173, nil); p != 20000 {
		t.Fatalf("not stable: %d", p)
	}
	// Same guest port in another VM (a fork) gets its own host port.
	if p := hp(b.ID, 5173, func(p int) bool { return p == 20001 }); p != 20002 {
		t.Fatalf("avoid ignored: %d", p)
	}
	if p := hp(a.ID, 3000, nil); p != 20001 {
		t.Fatalf("third = %d", p)
	}
	if _, err := s.HostPort(b.ID, 8080, 20000, 20002, nil); !errors.Is(err, ErrNoHostPorts) {
		t.Fatalf("full range: %v", err)
	}
	if err := s.DeleteVM(a.ID); err != nil {
		t.Fatal(err)
	}
	if p := hp(b.ID, 8080, nil); p != 20000 {
		t.Fatalf("not released with its VM: %d", p)
	}
	if err := s.DropHostPort(b.ID, 8080); err != nil {
		t.Fatal(err)
	}
	if p := hp(b.ID, 9000, nil); p != 20000 {
		t.Fatalf("not dropped: %d", p)
	}
}

// TestMigrateV1 opens a database created before the ports table existed.
func TestMigrateV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dune.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1 + "PRAGMA user_version = 1;"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.PutImage(Image{Name: "ubuntu", CreatedAt: time.Now()})
	vm, _ := s.CreateVM(VM{Image: "ubuntu", State: VMRunning, VCPUs: 1, MemMiB: 512})
	if _, err := s.HostPort(vm.ID, 80, 1, 2, nil); err != nil {
		t.Fatal(err)
	}
	var v int
	s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != len(migrations) {
		t.Fatalf("user_version = %d", v)
	}
}
