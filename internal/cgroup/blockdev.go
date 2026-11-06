//go:build unix

package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// WholeDiskDevice returns "MAJ:MIN" of the whole disk holding path, because
// io.max rejects partitions.
func WholeDiskDevice(sysRoot, path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	dev := uint64(st.Dev)
	majMin := fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))

	entry := filepath.Join(sysRoot, "dev", "block", majMin)
	if _, err := os.Stat(entry); err != nil {
		return "", fmt.Errorf("%s (device %s) is not on a block device: %w", path, majMin, err)
	}
	if _, err := os.Stat(entry + "/partition"); errors.Is(err, fs.ErrNotExist) {
		return majMin, nil
	} else if err != nil {
		return "", err
	}

	// Not filepath.Join: it would remove the ".." lexically, but entry is a
	// symlink, and ".." has to be resolved from where the symlink points.
	parent, err := os.ReadFile(entry + "/../dev")
	if err != nil {
		return "", fmt.Errorf("parent disk of %s: %w", majMin, err)
	}
	return strings.TrimSpace(string(parent)), nil
}
