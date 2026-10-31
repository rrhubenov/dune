// Package image prepares guest root filesystems for dune.
//
// The guest's address and default route come from the kernel's "ip=" boot
// argument (see netns.KernelIPArg), which works for any image whose kernel has
// IP autoconfiguration. DNS has no kernel equivalent, so it is written into
// the rootfs at import.
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

// Prepare loop-mounts an ext4 rootfs at a temporary dir under tmpBase and
// writes /etc/resolv.conf. It replaces a symlink (e.g. to systemd-resolved's
// stub) with a plain file.
func Prepare(rootfs, tmpBase string, nameservers []string) (err error) {
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
	return os.WriteFile(resolv, []byte(b.String()), 0o644)
}
