package hoststats

import (
	"os"
	"testing"
)

func TestCollect(t *testing.T) {
	s, err := Collect("/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemTotal <= 0 || s.MemAvailable <= 0 || s.MemAvailable > s.MemTotal {
		t.Errorf("memory: total %d available %d", s.MemTotal, s.MemAvailable)
	}
	if s.DiskTotal <= 0 || s.DiskFree > s.DiskTotal {
		t.Errorf("disk: total %d free %d", s.DiskTotal, s.DiskFree)
	}
	if s.MemVMs != 0 || len(s.VMMem) != 0 {
		t.Errorf("no cgroups given, got %d / %v", s.MemVMs, s.VMMem)
	}
}

// With dune VMs running under systemd, their slice reports usage.
func TestSliceMemory(t *testing.T) {
	if _, err := os.Stat(cgroupRoot + "/dune.slice"); err != nil {
		t.Skip("no dune.slice on this host")
	}
	if n := cgroupMemory("dune.slice"); n <= 0 {
		t.Errorf("dune.slice memory.current = %d", n)
	}
}
