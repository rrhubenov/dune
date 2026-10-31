// Package image prepares guest root filesystems for dune.
//
// The guest's address and default route come from the kernel's "ip=" boot
// argument (see netns.KernelIPArg), which works for any image whose kernel has
// IP autoconfiguration. DNS has no kernel equivalent, so it is written into
// the rootfs at import, along with the guest agent and its systemd unit.
package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rrhubenov/dune/internal/run"
)

// DefaultNameservers are written to the guest's /etc/resolv.conf.
var DefaultNameservers = []string{"1.1.1.1", "8.8.8.8"}

const agentUnit = `[Unit]
Description=dune guest agent
DefaultDependencies=no
After=local-fs.target

[Service]
ExecStart=/usr/local/bin/dune-agent
Restart=always
RestartSec=1

[Install]
WantedBy=sysinit.target
`

// Prepare loop-mounts an ext4 rootfs at a temporary dir under tmpBase,
// writes /etc/resolv.conf (replacing a symlink, e.g. to systemd-resolved's
// stub, with a plain file) and, if agentBin is set, installs the guest agent
// as an enabled systemd service.
func Prepare(rootfs, tmpBase string, nameservers []string, agentBin string) (err error) {
	mnt, err := os.MkdirTemp(tmpBase, "mnt-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)
	if err := run.Cmd("mount", "-o", "loop", rootfs, mnt); err != nil {
		return err
	}
	defer func() {
		if uerr := run.Cmd("umount", mnt); err == nil {
			err = uerr
		}
	}()
	etc := filepath.Join(mnt, "etc")
	if fi, err := os.Stat(etc); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s: no /etc; not a root filesystem?", rootfs)
	}
	resolv := filepath.Join(etc, "resolv.conf")
	if err := os.Remove(resolv); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var b strings.Builder
	b.WriteString("# written by dune at image import\n")
	for _, ns := range nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	if err := os.WriteFile(resolv, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if agentBin == "" {
		return nil
	}
	bin, err := os.ReadFile(agentBin)
	if err != nil {
		return fmt.Errorf("agent binary: %w", err)
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{"usr/local/bin/dune-agent", bin, 0o755},
		{"etc/systemd/system/dune-agent.service", []byte(agentUnit), 0o644},
	}
	for _, f := range files {
		p := filepath.Join(mnt, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.data, f.mode); err != nil {
			return err
		}
	}
	// Enable it: what "systemctl enable" would do, without running systemd.
	wants := filepath.Join(mnt, "etc/systemd/system/sysinit.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return err
	}
	link := filepath.Join(wants, "dune-agent.service")
	os.Remove(link)
	return os.Symlink("/etc/systemd/system/dune-agent.service", link)
}
