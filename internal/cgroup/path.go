package cgroup

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// DirForPID returns the cgroup v2 directory of a process, read from
// <procRoot>/<pid>/cgroup.
func DirForPID(procRoot string, pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			return path.Join("/sys/fs/cgroup", rel), nil
		}
	}
	return "", errors.New("cgroup v1 not supported")
}
