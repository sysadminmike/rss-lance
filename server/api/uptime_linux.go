//go:build linux

package api

import (
	"os"
	"strconv"
	"strings"
)

// getHostUptime returns the host machine uptime in seconds on Linux.
func getHostUptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return -1
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1
	}
	return int64(secs)
}
