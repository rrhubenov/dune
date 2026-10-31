package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountedRoot(t *testing.T) {
	ok, err := (&Store{Mount: "/"}).Mounted()
	if err != nil || !ok {
		t.Fatalf("Mounted(/) = %v, %v", ok, err)
	}
}

// TestReflink needs a reflink-capable fs; set DUNE_TEST_REFLINK_DIR to a dir on
// the store (e.g. /srv/dune/store/tmp) to run it.
func TestReflink(t *testing.T) {
	dir := os.Getenv("DUNE_TEST_REFLINK_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	defer os.Remove(src)
	defer os.Remove(dst)
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Reflink(src, dst)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
		t.Skipf("no reflink support in %s: %v", dir, err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello" {
		t.Fatalf("dst = %q", b)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	}
}
