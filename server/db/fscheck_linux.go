//go:build linux

package db

import (
	"fmt"
	"syscall"
)

// checkStatfs inspects the Linux statfs result to determine if the
// filesystem is local or network-mounted.
func checkStatfs(stat *syscall.Statfs_t) (bool, string, error) {
	fsType := int64(stat.Type)
	if name, ok := networkFSTypes[fsType]; ok {
		return false, fmt.Sprintf("network filesystem (%s)", name), nil
	}
	return true, fmt.Sprintf("local filesystem (type 0x%X)", fsType), nil
}
