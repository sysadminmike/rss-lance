package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// StatsSnapshot holds one point-in-time sample of server metrics.
type StatsSnapshot struct {
	Timestamp    string `json:"timestamp"`
	HeapAlloc    uint64 `json:"heap_alloc"`
	HeapSys      uint64 `json:"heap_sys"`
	StackInUse   uint64 `json:"stack_in_use"`
	Goroutines   int    `json:"goroutines"`
	GCPauseNs    uint64 `json:"gc_pause_ns"`
	NumGC        uint32 `json:"num_gc"`
}

// StatsCollector samples runtime metrics on a fixed interval and retains
// them in an in-memory ring for a configurable retention window.
// All data is lost on server restart — by design.
type StatsCollector struct {
	mu        sync.RWMutex
	history   []StatsSnapshot
	retention time.Duration // how long to keep samples
	interval  time.Duration
	stop      chan struct{}
	getSetting func(key string) (string, bool, error)
	lastNumGC uint32
}

const (
	DefaultStatsRetentionMinutes = 60
	DefaultStatsIntervalSecs     = 5
)

// NewStatsCollector creates a collector that samples every `interval` and
// retains data for `retention`. Call Start() to begin sampling.
func NewStatsCollector(getSetting func(key string) (string, bool, error)) *StatsCollector {
	return &StatsCollector{
		retention:  time.Duration(DefaultStatsRetentionMinutes) * time.Minute,
		interval:   time.Duration(DefaultStatsIntervalSecs) * time.Second,
		stop:       make(chan struct{}),
		getSetting: getSetting,
	}
}

// Start begins the background sampling goroutine. Call once at startup.
func (sc *StatsCollector) Start() {
	// Take an initial sample immediately
	sc.sample()
	go sc.loop()
}

// Stop halts the background sampler.
func (sc *StatsCollector) Stop() {
	close(sc.stop)
}

func (sc *StatsCollector) loop() {
	ticker := time.NewTicker(sc.interval)
	defer ticker.Stop()
	for {
		select {
		case <-sc.stop:
			return
		case <-ticker.C:
			sc.refreshRetention()
			sc.sample()
		}
	}
}

// refreshRetention checks the settings for an updated retention value.
func (sc *StatsCollector) refreshRetention() {
	if sc.getSetting == nil {
		return
	}
	val, found, err := sc.getSetting("stats.retention_minutes")
	if err != nil || !found {
		return
	}
	var minutes float64
	if json.Unmarshal([]byte(val), &minutes) == nil && minutes >= 1 {
		sc.mu.Lock()
		sc.retention = time.Duration(minutes) * time.Minute
		sc.mu.Unlock()
	}
}

func (sc *StatsCollector) sample() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// Get the latest GC pause
	var pauseNs uint64
	if ms.NumGC > 0 {
		pauseNs = ms.PauseNs[(ms.NumGC+255)%256]
	}

	snap := StatsSnapshot{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		HeapAlloc:  ms.HeapAlloc,
		HeapSys:    ms.HeapSys,
		StackInUse: ms.StackInuse,
		Goroutines: runtime.NumGoroutine(),
		GCPauseNs:  pauseNs,
		NumGC:      ms.NumGC,
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.history = append(sc.history, snap)
	sc.lastNumGC = ms.NumGC

	// Prune entries older than retention window
	cutoff := time.Now().Add(-sc.retention)
	trimIdx := 0
	for trimIdx < len(sc.history) {
		t, err := time.Parse(time.RFC3339, sc.history[trimIdx].Timestamp)
		if err != nil || t.After(cutoff) {
			break
		}
		trimIdx++
	}
	if trimIdx > 0 {
		sc.history = sc.history[trimIdx:]
	}
}

// History returns a copy of the collected snapshots.
func (sc *StatsCollector) History() []StatsSnapshot {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	out := make([]StatsSnapshot, len(sc.history))
	copy(out, sc.history)
	return out
}

// Handle serves GET /api/server-status/history
func (sc *StatsCollector) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sc.mu.RLock()
	retention := sc.retention
	sc.mu.RUnlock()

	jsonOK(w, map[string]any{
		"retention_minutes": int(retention.Minutes()),
		"interval_seconds":  int(sc.interval.Seconds()),
		"count":             len(sc.History()),
		"history":           sc.History(),
	})
}
