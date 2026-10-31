package netns

import (
	"os"
	"strings"
	"testing"

	"github.com/rrhubenov/dune/internal/run"
)

func TestSlotAddrs(t *testing.T) {
	cases := []struct {
		slot     int
		host, ns string
	}{
		{1, "10.200.0.5/30", "10.200.0.6/30"},
		{63, "10.200.0.253/30", "10.200.0.254/30"},
		{64, "10.200.1.1/30", "10.200.1.2/30"},
		{16383, "10.200.255.253/30", "10.200.255.254/30"},
	}
	for _, c := range cases {
		h, n, err := slotAddrs(c.slot)
		if err != nil || h.String() != c.host || n.String() != c.ns {
			t.Errorf("slot %d = %s %s %v, want %s %s", c.slot, h, n, err, c.host, c.ns)
		}
	}
	for _, bad := range []int{0, 16384} {
		if _, _, err := slotAddrs(bad); err == nil {
			t.Errorf("slot %d: want error", bad)
		}
	}
}

// TestCreate needs root and touches host networking. Run with DUNE_TEST_ROOT=1.
func TestCreate(t *testing.T) {
	if os.Getenv("DUNE_TEST_ROOT") == "" {
		t.Skip("set DUNE_TEST_ROOT=1 to run (needs root)")
	}
	const id, slot = "nstest", 16000
	if err := HostSetup(); err != nil {
		t.Fatal(err)
	}
	Delete(id)
	if err := Create(id, slot, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := Delete(id); err != nil {
			t.Error(err)
		}
		if run.Quiet("ip", "link", "show", HostIf(id)) {
			t.Error("host veth still exists after Delete")
		}
	}()

	out, err := run.Output("ip", "-n", Name(id), "link", "show", TapName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, TapMAC) {
		t.Errorf("tap0 MAC not %s:\n%s", TapMAC, out)
	}
	// The namespace reaches the internet through host NAT.
	if err := run.Cmd("ip", "netns", "exec", Name(id), "ping", "-c", "1", "-W", "3", "1.1.1.1"); err != nil {
		t.Errorf("ping from netns: %v", err)
	}
}
