//go:build windows

package api

import (
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	getTickCount64   = kernel32.NewProc("GetTickCount64")
)

// getHostUptime returns the host machine uptime in seconds on Windows.
func getHostUptime() int64 {
	ret, _, _ := getTickCount64.Call()
	ms := *(*uint64)(unsafe.Pointer(&ret))
	return int64(ms / 1000)
}
