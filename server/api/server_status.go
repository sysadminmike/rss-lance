package api

import (
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// ServerStatusHandler handles GET /api/server-status
type ServerStatusHandler struct {
	startTime time.Time
	cacheStats func() CacheStatsInfo
}

// CacheStatsInfo holds write cache statistics exposed to the status endpoint.
type CacheStatsInfo struct {
	PendingReads int    `json:"pending_reads"`
	PendingStars int    `json:"pending_stars"`
	LastFlushTime string `json:"last_flush_time,omitempty"`
	LastFlushDurationMs int64 `json:"last_flush_duration_ms"`
}

func NewServerStatusHandler(startTime time.Time, cacheStats func() CacheStatsInfo) *ServerStatusHandler {
	return &ServerStatusHandler{startTime: startTime, cacheStats: cacheStats}
}

func (h *ServerStatusHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	now := time.Now()
	uptimeSecs := int64(now.Sub(h.startTime).Seconds())

	// Build info
	goVersion := runtime.Version()
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	numCPU := runtime.NumCPU()
	goroutines := runtime.NumGoroutine()
	pid := os.Getpid()

	hostname, _ := os.Hostname()

	vcsRevision := ""
	vcsTime := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				vcsRevision = s.Value
			case "vcs.time":
				vcsTime = s.Value
			}
		}
	}

	// GC recent pauses (from ring buffer, last 256 pauses)
	numRecent := int(ms.NumGC)
	if numRecent > 20 {
		numRecent = 20
	}
	recentPauses := make([]uint64, 0, numRecent)
	for i := 0; i < numRecent; i++ {
		idx := (int(ms.NumGC) - numRecent + i) % 256
		recentPauses = append(recentPauses, ms.PauseNs[idx])
	}

	var lastGCTime string
	if ms.LastGC > 0 {
		lastGCTime = time.Unix(0, int64(ms.LastGC)).UTC().Format(time.RFC3339)
	}

	hostUptimeSecs := getHostUptime()

	// Write cache stats
	var cache CacheStatsInfo
	if h.cacheStats != nil {
		cache = h.cacheStats()
	}

	resp := map[string]any{
		"server": map[string]any{
			"uptime_seconds":     uptimeSecs,
			"start_time":         h.startTime.UTC().Format(time.RFC3339),
			"pid":                pid,
			"go_version":         goVersion,
			"os":                 goos,
			"arch":               goarch,
			"num_cpu":            numCPU,
			"goroutines":         goroutines,
			"build_vcs_revision": vcsRevision,
			"build_vcs_time":     vcsTime,
		},
		"host": map[string]any{
			"uptime_seconds": hostUptimeSecs,
			"hostname":       hostname,
		},
		"memory": map[string]any{
			"heap_alloc_bytes":   ms.HeapAlloc,
			"heap_sys_bytes":     ms.HeapSys,
			"heap_idle_bytes":    ms.HeapIdle,
			"heap_in_use_bytes":  ms.HeapInuse,
			"heap_objects":       ms.HeapObjects,
			"stack_in_use_bytes": ms.StackInuse,
			"sys_bytes":          ms.Sys,
			"total_alloc_bytes":  ms.TotalAlloc,
			"mallocs":            ms.Mallocs,
			"frees":              ms.Frees,
		},
		"gc": map[string]any{
			"num_gc":               ms.NumGC,
			"last_gc_time":         lastGCTime,
			"last_pause_ns":        ms.PauseNs[(ms.NumGC+255)%256],
			"total_pause_ns":       ms.PauseTotalNs,
			"gc_cpu_fraction":      ms.GCCPUFraction,
			"next_gc_target_bytes": ms.NextGC,
			"recent_pauses_ns":     recentPauses,
		},
		"write_cache": cache,
	}

	jsonOK(w, resp)
}
