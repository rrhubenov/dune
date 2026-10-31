// Package hoststats reads host memory and disk usage, and per-VM memory from
// each VM's cgroup.
package hoststats

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/rrhubenov/dune/internal/model"
)

const cgroupRoot = "/sys/fs/cgroup"

// Collect gathers stats. dataDir picks the filesystem reported as disk.
// vmCgroups maps VM id to its cgroup path relative to the cgroup root (e.g.
// "dune.slice/dune-vm-<id>.scope").
//
// MemVMs sums the live VMs rather than reading their parent slice: when a
// VM's scope is removed, its leftover page cache charges move up to the
// slice, which would count deleted VMs.
func Collect(dataDir string, vmCgroups map[string]string) (model.HostStats, error) {
	var s model.HostStats
	mem, err := meminfo()
	if err != nil {
		return s, err
	}
	s.MemTotal, s.MemAvailable = mem["MemTotal"], mem["MemAvailable"]
	s.VMMem = map[string]int64{}
	for id, cg := range vmCgroups {
		if n := cgroupMemory(cg); n > 0 {
			s.VMMem[id] = n
			s.MemVMs += n
		}
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dataDir, &fs); err != nil {
		return s, err
	}
	s.DiskPath = dataDir
	s.DiskTotal = int64(fs.Blocks) * fs.Bsize
	s.DiskFree = int64(fs.Bavail) * fs.Bsize
	return s, nil
}

// meminfo returns /proc/meminfo values in bytes.
func meminfo() (map[string]int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// "MemTotal:       32790604 kB"
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			n *= 1024
		}
		out[k] = n
	}
	return out, sc.Err()
}

// cgroupMemory returns a cgroup's memory use the way `docker stats` counts
// it: memory.current minus inactive_file. memory.current includes page cache
// the cgroup caused, e.g. a 1 GiB snapshot file it just wrote, which is
// reclaimable and would make a VM look bigger than its guest RAM. Returns 0
// if unavailable.
func cgroupMemory(cg string) int64 {
	if cg == "" {
		return 0
	}
	dir := filepath.Join(cgroupRoot, cg)
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if stat, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		for _, line := range strings.Split(string(stat), "\n") {
			if v, ok := strings.CutPrefix(line, "inactive_file "); ok {
				inactive, _ := strconv.ParseInt(v, 10, 64)
				n = max(n-inactive, 0)
				break
			}
		}
	}
	return n
}
