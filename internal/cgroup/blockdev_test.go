//go:build unix

package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeSys builds a sysfs tree like the real one, where /sys/dev/block/MAJ:MIN
// is a symlink into /sys/devices. It returns the root and the MAJ:MIN of the
// device holding dir, which is the entry WholeDiskDevice will look up.
func fakeSys(t *testing.T, dir string) (root, majMin string) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	majMin = fmt.Sprintf("%d:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)))
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev", "block"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, majMin
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWholeDiskDeviceFromPartition(t *testing.T) {
	dir := t.TempDir()
	root, majMin := fakeSys(t, dir)
	mustWrite(t, filepath.Join(root, "devices", "nvme0n1", "dev"), "259:0\n")
	mustWrite(t, filepath.Join(root, "devices", "nvme0n1", "nvme0n1p1", "dev"), majMin+"\n")
	mustWrite(t, filepath.Join(root, "devices", "nvme0n1", "nvme0n1p1", "partition"), "1\n")
	// A decoy that only a lexically cleaned "<entry>/../dev" would read.
	mustWrite(t, filepath.Join(root, "dev", "block", "dev"), "9:9\n")
	if err := os.Symlink(filepath.Join("..", "..", "devices", "nvme0n1", "nvme0n1p1"), filepath.Join(root, "dev", "block", majMin)); err != nil {
		t.Fatal(err)
	}

	got, err := WholeDiskDevice(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "259:0" {
		t.Fatalf("got %q, want the parent disk 259:0", got)
	}
}

func TestWholeDiskDeviceAlreadyWholeDisk(t *testing.T) {
	dir := t.TempDir()
	root, majMin := fakeSys(t, dir)
	mustWrite(t, filepath.Join(root, "devices", "sdd", "dev"), majMin+"\n")
	if err := os.Symlink(filepath.Join("..", "..", "devices", "sdd"), filepath.Join(root, "dev", "block", majMin)); err != nil {
		t.Fatal(err)
	}

	got, err := WholeDiskDevice(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != majMin {
		t.Fatalf("got %q, want %q", got, majMin)
	}
}

// tmpfs and overlay have no entry under /sys/dev/block. The plan requires data
// on a real disk, because io.max would have nothing to throttle otherwise.
func TestWholeDiskDeviceNotABlockDevice(t *testing.T) {
	dir := t.TempDir()
	root, _ := fakeSys(t, dir)
	if _, err := WholeDiskDevice(root, dir); err == nil {
		t.Fatal("want error")
	}
}

func TestWholeDiskDeviceMissingPath(t *testing.T) {
	if _, err := WholeDiskDevice(t.TempDir(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("want error")
	}
}
