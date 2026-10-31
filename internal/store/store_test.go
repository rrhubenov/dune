package store

import (
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
