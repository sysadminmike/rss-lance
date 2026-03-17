//go:build !windows && !linux

package api

// getHostUptime returns -1 (unavailable) on unsupported platforms.
func getHostUptime() int64 {
	return -1
}
