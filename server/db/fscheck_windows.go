//go:build windows

package db

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

// Drive type constants from Win32 GetDriveTypeW.
const (
	driveUnknown   = 0
	driveNoRootDir = 1
	driveRemovable = 2
	driveFixed     = 3
	driveRemote    = 4
	driveCDROM     = 5
	driveRAMDisk   = 6
)

var (
	kernel32    = syscall.NewLazyDLL("kernel32.dll")
	getDriveType = kernel32.NewProc("GetDriveTypeW")
)

// driveTypeString returns a human-readable name for the drive type.
func driveTypeString(dt uintptr) string {
	switch dt {
	case driveRemovable:
		return "removable (USB/SD card)"
	case driveFixed:
		return "local disk"
	case driveRemote:
		return "network share (NFS/SMB)"
	case driveCDROM:
		return "CD-ROM"
	case driveRAMDisk:
		return "RAM disk"
	default:
		return "unknown"
	}
}

// CheckLocalFS checks whether the given path is on a local fixed disk.
// Returns (isLocal, driveDescription, error).
// On Windows this uses the Win32 GetDriveTypeW API.
func CheckLocalFS(path string) (bool, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, "", fmt.Errorf("resolve path: %w", err)
	}

	// Extract the volume root (e.g. "C:\")
	vol := filepath.VolumeName(absPath)
	if vol == "" {
		return false, "", fmt.Errorf("no volume name for path: %s", absPath)
	}

	// UNC paths (\\server\share) are always remote
	if len(vol) >= 2 && vol[0] == '\\' && vol[1] == '\\' {
		return false, "network share (UNC path)", nil
	}

	rootPath := vol + `\`
	rootPtr, err := syscall.UTF16PtrFromString(rootPath)
	if err != nil {
		return false, "", fmt.Errorf("utf16 encode: %w", err)
	}

	ret, _, _ := getDriveType.Call(uintptr(unsafe.Pointer(rootPtr)))
	desc := driveTypeString(ret)

	switch ret {
	case driveFixed, driveRAMDisk:
		return true, desc, nil
	case driveRemovable:
		return false, desc, nil
	case driveRemote:
		return false, desc, nil
	case driveCDROM:
		return false, desc, nil
	default:
		// Unknown -- assume local to avoid false warnings
		return true, desc, nil
	}
}
