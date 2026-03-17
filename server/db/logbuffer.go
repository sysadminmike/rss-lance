// Package db - buffered log writer for Lance log tables.
//
// Instead of writing every log entry immediately (one INSERT per call),
// the logBuffer accumulates entries in memory and flushes them as a
// single batched INSERT when either:
//   - the buffer reaches FlushThreshold entries, OR
//   - FlushIntervalSecs has elapsed since the first buffered entry.
//
// This pattern mirrors writeCache (cache.go) and is reusable for future
// metrics collection which will need the same buffered-INSERT approach.
package db

import (
	"log"
	"sync"
	"time"

	"rss-lance/server/debug"
)

// LogBufferConfig controls log buffer flush behaviour.
type LogBufferConfig struct {
	FlushThreshold    int // flush after N buffered entries (default 20)
	FlushIntervalSecs int // flush after N seconds even if threshold not reached (default 30)
}

// DefaultLogBufferConfig returns sensible defaults.
func DefaultLogBufferConfig() LogBufferConfig {
	return LogBufferConfig{
		FlushThreshold:    20,
		FlushIntervalSecs: 30,
	}
}

// logBuffer accumulates LogEntry records and flushes them in batches.
type logBuffer struct {
	mu      sync.Mutex
	entries []LogEntry
	timer   *time.Timer
	flushFn func(entries []LogEntry) error
	cfg     LogBufferConfig
	stopped bool
}

func newLogBuffer(cfg LogBufferConfig, flushFn func([]LogEntry) error) *logBuffer {
	if cfg.FlushThreshold <= 0 {
		cfg.FlushThreshold = 20
	}
	if cfg.FlushIntervalSecs <= 0 {
		cfg.FlushIntervalSecs = 30
	}
	return &logBuffer{
		entries: make([]LogEntry, 0, cfg.FlushThreshold),
		flushFn: flushFn,
		cfg:     cfg,
	}
}

// add buffers a log entry. If the threshold is reached, flush immediately.
func (lb *logBuffer) add(entry LogEntry) {
	lb.mu.Lock()

	if lb.stopped {
		lb.mu.Unlock()
		return
	}

	lb.entries = append(lb.entries, entry)
	shouldFlush := len(lb.entries) >= lb.cfg.FlushThreshold
	lb.startTimerLocked()
	lb.mu.Unlock()

	if shouldFlush {
		lb.flush()
	}
}

// startTimerLocked starts the flush timer on the first buffered entry.
// Caller must hold lb.mu.
func (lb *logBuffer) startTimerLocked() {
	if len(lb.entries) == 1 && lb.timer == nil && lb.cfg.FlushIntervalSecs > 0 {
		lb.timer = time.AfterFunc(
			time.Duration(lb.cfg.FlushIntervalSecs)*time.Second,
			func() { lb.flush() },
		)
	}
}

// flush writes all buffered entries via the flush function.
func (lb *logBuffer) flush() error {
	lb.mu.Lock()

	if lb.timer != nil {
		lb.timer.Stop()
		lb.timer = nil
	}

	if len(lb.entries) == 0 {
		lb.mu.Unlock()
		return nil
	}

	// Snapshot and clear under lock
	batch := lb.entries
	lb.entries = make([]LogEntry, 0, lb.cfg.FlushThreshold)
	count := len(batch)
	lb.mu.Unlock()

	debug.Log(debug.Batch, "flushing %d buffered log entries...", count)

	err := lb.flushFn(batch)
	if err != nil {
		log.Printf("log buffer flush (%d entries) error: %v", count, err)
		debug.Log(debug.Batch, "log flush FAILED: %v", err)

		// Put failed entries back (prepend so ordering is preserved)
		lb.mu.Lock()
		lb.entries = append(batch, lb.entries...)
		lb.mu.Unlock()
	} else {
		debug.Log(debug.Batch, "log flush OK: %d entries", count)
	}
	return err
}

// close flushes any remaining entries and stops the buffer.
func (lb *logBuffer) close() error {
	lb.mu.Lock()
	if lb.stopped {
		lb.mu.Unlock()
		return nil
	}
	lb.stopped = true
	lb.mu.Unlock()

	return lb.flush()
}

// pending returns the number of buffered entries not yet flushed.
func (lb *logBuffer) pending() int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return len(lb.entries)
}
