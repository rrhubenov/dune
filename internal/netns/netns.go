// Package netns gives each VM a private network namespace.
//
// Inside every namespace the layout is identical, so a forked VM's network
// config (baked into its memory snapshot) stays valid:
//
//	guest eth0 172.16.0.2 ── tap0 172.16.0.1/30 (fixed MAC) ── NAT ── veth0
//
// veth0 pairs with a host interface "duneN", where N is the VM's network slot
// and owns 10.200.0.0/16 offset N*4 (a /30): host side .1, namespace side .2.
// The host NATs 10.200.0.0/16 out of its uplink.
//
// The fixed tap0 MAC matters: a forked guest keeps its ARP cache, so the
// gateway must answer from the same MAC in every fork or the guest loses
// network until ARP re-resolves.
package netns

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"

	"github.com/rrhubenov/dune/internal/run"
)

const (
	TapName   = "tap0"
	TapAddr   = "172.16.0.1/30"
	TapMAC    = "06:00:ac:10:00:01"
	GuestIP   = "172.16.0.2"
	GuestMAC  = "06:00:ac:10:00:02" // guest derives 172.16.0.2 from this
	HostRange = "10.200.0.0/16"

	hostIfPrefix = "dune" // host veth names: dune1, dune2, ...
	nsVeth       = "veth0"
	fwdChain     = "DUNE-FORWARD"
	natChain     = "DUNE-POSTROUTING"
	maxSlot      = 16383
)

// Name returns the netns name for a VM. Its file is /run/netns/<name>.
func Name(vmID string) string { return "dune-" + vmID }

// Path returns the netns file path, as passed to the jailer's --netns.
func Path(vmID string) string { return "/run/netns/" + Name(vmID) }

// slotAddrs returns the host-side and namespace-side veth addresses of slot.
func slotAddrs(slot int) (host, ns netip.Prefix, err error) {
	if slot < 1 || slot > maxSlot {
		return host, ns, fmt.Errorf("network slot %d out of range 1..%d", slot, maxSlot)
	}
	off := slot * 4
	base := netip.AddrFrom4([4]byte{10, 200, byte(off >> 8), byte(off)})
	return netip.PrefixFrom(base.Next(), 30), netip.PrefixFrom(base.Next().Next(), 30), nil
}

// HostSetup enables forwarding and installs dune's host-wide iptables rules.
// Idempotent; call at daemon start.
//
// Docker sets the FORWARD policy to DROP, so dune jumps to its own chain from
// the top of FORWARD. VM-to-VM traffic across namespaces is dropped.
func HostSetup() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	ifs := hostIfPrefix + "+"
	steps := [][]string{
		{"-t", "filter", "-N", fwdChain},
		{"-t", "filter", "-F", fwdChain},
		{"-t", "filter", "-A", fwdChain, "-i", ifs, "-o", ifs, "-j", "DROP"},
		{"-t", "filter", "-A", fwdChain, "-i", ifs, "-j", "ACCEPT"},
		{"-t", "filter", "-A", fwdChain, "-o", ifs, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"-t", "nat", "-N", natChain},
		{"-t", "nat", "-F", natChain},
		{"-t", "nat", "-A", natChain, "-s", HostRange, "!", "-o", ifs, "-j", "MASQUERADE"},
	}
	for _, args := range steps {
		if args[2] == "-N" {
			// Creating an existing chain fails; that's fine.
			run.Quiet("iptables", args...)
			continue
		}
		if err := run.Cmd("iptables", args...); err != nil {
			return err
		}
	}
	jumps := [][]string{
		{"-t", "filter", "FORWARD", "-j", fwdChain},
		{"-t", "nat", "POSTROUTING", "-j", natChain},
	}
	for _, j := range jumps {
		check := append([]string{j[0], j[1], "-C"}, j[2:]...)
		if run.Quiet("iptables", check...) {
			continue
		}
		ins := append([]string{j[0], j[1], "-I", j[2], "1"}, j[3:]...)
		if err := run.Cmd("iptables", ins...); err != nil {
			return err
		}
	}
	return nil
}

// Create builds the namespace for a VM. uid/gid own tap0 so the jailed,
// unprivileged Firecracker process can attach to it.
func Create(vmID string, slot, uid, gid int) (err error) {
	hostAddr, nsAddr, err := slotAddrs(slot)
	if err != nil {
		return err
	}
	ns := Name(vmID)
	hostIf := hostIfPrefix + strconv.Itoa(slot)
	if err := run.Cmd("ip", "netns", "add", ns); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			Delete(vmID, slot)
		}
	}()
	// Run inside the namespace.
	in := func(name string, args ...string) error {
		return run.Cmd("ip", append([]string{"netns", "exec", ns, name}, args...)...)
	}
	steps := []func() error{
		func() error { return in("ip", "link", "set", "lo", "up") },
		func() error {
			return run.Cmd("ip", "link", "add", hostIf, "type", "veth", "peer", "name", nsVeth, "netns", ns)
		},
		func() error { return run.Cmd("ip", "addr", "add", hostAddr.String(), "dev", hostIf) },
		func() error { return run.Cmd("ip", "link", "set", hostIf, "up") },
		func() error { return in("ip", "addr", "add", nsAddr.String(), "dev", nsVeth) },
		func() error { return in("ip", "link", "set", nsVeth, "up") },
		func() error { return in("ip", "route", "add", "default", "via", hostAddr.Addr().String()) },
		func() error {
			return in("ip", "tuntap", "add", "dev", TapName, "mode", "tap",
				"user", strconv.Itoa(uid), "group", strconv.Itoa(gid))
		},
		func() error { return in("ip", "link", "set", TapName, "address", TapMAC) },
		func() error { return in("ip", "addr", "add", TapAddr, "dev", TapName) },
		func() error { return in("ip", "link", "set", TapName, "up") },
		func() error { return in("sysctl", "-q", "-w", "net.ipv4.ip_forward=1") },
		func() error {
			return in("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", nsVeth, "-j", "MASQUERADE")
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a VM's host veth and namespace. Idempotent.
//
// The host veth is deleted explicitly: "ip netns delete" only unlinks the
// name, and the kernel tears the namespace (and the veth inside it) down
// asynchronously, or never while a process still holds it. Deleting one end of
// a veth pair removes both, synchronously, so the slot is free on return.
func Delete(vmID string, slot int) error {
	hostIf := hostIfPrefix + strconv.Itoa(slot)
	if run.Quiet("ip", "link", "show", hostIf) {
		if err := run.Cmd("ip", "link", "delete", hostIf); err != nil {
			return err
		}
	}
	if _, err := os.Stat(Path(vmID)); os.IsNotExist(err) {
		return nil
	}
	return run.Cmd("ip", "netns", "delete", Name(vmID))
}
