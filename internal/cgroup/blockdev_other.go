//go:build !unix

package cgroup

import "errors"

// WholeDiskDevice needs Unix device numbers. This stub lets the module build
// and test on any dev machine.
func WholeDiskDevice(sysRoot, path string) (string, error) {
	return "", errors.New("block device lookup is only supported on Unix")
}
