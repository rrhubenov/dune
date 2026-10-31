// Package storage manages dune's single storage path: a sparse XFS image with
// reflink enabled, loop-mounted by the daemon. Every VM disk and snapshot file
// lives inside it so copies can be reflinks (copy-on-write, near-instant).
package storage

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/rrhubenov/dune/internal/run"
)

type Store struct {
	Image string // sparse image file, e.g. /srv/dune/store.img
	Mount string // where it is mounted, e.g. /srv/dune/store
	Size  string // size passed to truncate when creating, e.g. "500G"
}

type Usage struct {
	ImageAllocated int64 `json:"image_allocated"` // bytes the sparse image really uses on the host fs
	FSUsed         int64 `json:"fs_used"`
	FSTotal        int64 `json:"fs_total"`
	Mounted        bool  `json:"mounted"`
}

// Setup creates the image if missing and mounts it if not mounted. Idempotent.
func (s *Store) Setup() error {
	if _, err := os.Stat(s.Image); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(s.Image), 0o755); err != nil {
			return err
		}
		if err := run.Cmd("truncate", "-s", s.Size, s.Image); err != nil {
			return err
		}
		if err := run.Cmd("mkfs.xfs", "-q", "-m", "reflink=1", s.Image); err != nil {
			os.Remove(s.Image)
			return err
		}
	} else if err != nil {
		return err
	}
	mounted, err := s.Mounted()
	if err != nil || mounted {
		return err
	}
	if err := os.MkdirAll(s.Mount, 0o755); err != nil {
		return err
	}
	return run.Cmd("mount", "-o", "loop", s.Image, s.Mount)
}

// Mounted reports whether something is mounted at s.Mount.
func (s *Store) Mounted() (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	want := filepath.Clean(s.Mount)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Field 5 is the mount point (octal-escaped; our paths have no spaces).
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && fields[4] == want {
			return true, nil
		}
	}
	return false, sc.Err()
}

// Trim releases freed XFS blocks back to the host by punching holes in the image.
func (s *Store) Trim() error {
	return run.Cmd("fstrim", s.Mount)
}

func (s *Store) Usage() (Usage, error) {
	var u Usage
	var st unix.Stat_t
	if err := unix.Stat(s.Image, &st); err != nil {
		return u, err
	}
	u.ImageAllocated = st.Blocks * 512
	mounted, err := s.Mounted()
	if err != nil {
		return u, err
	}
	u.Mounted = mounted
	if mounted {
		var fs unix.Statfs_t
		if err := unix.Statfs(s.Mount, &fs); err != nil {
			return u, err
		}
		u.FSTotal = int64(fs.Blocks) * fs.Bsize
		u.FSUsed = int64(fs.Blocks-fs.Bfree) * fs.Bsize
	}
	return u, nil
}

// Path returns an absolute path inside the store.
func (s *Store) Path(elem ...string) string {
	return filepath.Join(append([]string{s.Mount}, elem...)...)
}

// Reflink makes dst a copy-on-write clone of src. Both must be on the store.
// It fails rather than falling back to a byte copy: a silent full copy of a
// multi-GB disk would hide a misconfiguration.
func Reflink(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("reflink %s -> %s: %w", src, dst, err)
	}
	return out.Close()
}
