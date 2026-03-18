//go:build !windows

package db

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// Known network filesystem magic numbers (from statfs(2) / Linux kernel headers).
var networkFSTypes = map[int64]string{
	0x6969:     "NFS",
	0xFF534D42: "CIFS/SMB",
	0x65735546: "FUSE (sshfs/rclone)",
	0x7461636f: "OCFS2",
	0x00C36400: "CEPH",
	0x5346414F: "AFS",
	0x47504653: "GPFS",
	0x0BD00BD0: "Lustre",
}

// CheckLocalFS checks whether the given path is on a local filesystem.
// Returns (isLocal, fsDescription, error).
// On Linux this uses statfs(2) to check the filesystem type magic number.
// On FreeBSD/macOS this checks the filesystem type name string.
func CheckLocalFS(path string) (bool, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, "", fmt.Errorf("resolve path: %w", err)
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(absPath, &stat); err != nil {
		return false, "", fmt.Errorf("statfs: %w", err)
	}

	return checkStatfs(&stat)
}
