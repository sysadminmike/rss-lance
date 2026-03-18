//go:build freebsd || darwin

package db

import (
	"fmt"
	"strings"
	"syscall"
)

// Known network filesystem type names on FreeBSD and macOS.
var networkFSNames = map[string]bool{
	"nfs":    true,
	"nfs4":   true,
	"smbfs":  true,
	"cifs":   true,
	"fuse":   true,
	"fusefs": true,
	"sshfs":  true,
	"afs":    true,
}

// checkStatfs inspects the FreeBSD/macOS statfs result to determine if
// the filesystem is local or network-mounted.
func checkStatfs(stat *syscall.Statfs_t) (bool, string, error) {
	// Fstypename is a fixed-size byte array; convert to string
	var fsName string
	for i, b := range stat.Fstypename {
		if b == 0 {
			fsName = string(stat.Fstypename[:i])
			break
		}
	}
	if fsName == "" {
		fsName = string(stat.Fstypename[:])
	}
	fsName = strings.TrimRight(fsName, "\x00")
	lower := strings.ToLower(fsName)

	if networkFSNames[lower] {
		return false, fmt.Sprintf("network filesystem (%s)", fsName), nil
	}
	return true, fmt.Sprintf("local filesystem (%s)", fsName), nil
}
