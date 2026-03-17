// Package db - write cache for Lance table state changes.
//
// Instead of writing every SetArticleRead / SetArticleStarred to Lance
// immediately, the cache accumulates changes in a Go map.  Read queries
// incorporate pending changes via a generated CTE + LEFT JOIN, so the
// API always returns up-to-date state even before the periodic MERGE
// into Lance fires.
//
// The MERGE flush runs when either:
//   - the cache reaches FlushThreshold entries, OR
//   - FlushIntervalSecs has elapsed since the first pending write.
//
// MarkAllRead bypasses the cache (it's already a bulk operation that
// goes straight to Lance via MERGE INTO).
//
// All persistent state lives in Lance tables.  The cache is purely
// in-memory and rebuilds (empty) on server restart.
package db

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"rss-lance/server/debug"
)

// CacheConfig controls write-cache flush behaviour.
type CacheConfig struct {
	FlushThreshold   int // flush after N pending writes (default 20)
	FlushIntervalSecs int // flush after N seconds even if threshold not reached (default 120)
}

// DefaultCacheConfig returns sensible defaults.
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		FlushThreshold:    20,
		FlushIntervalSecs: 120,
	}
}

// articleOverride holds pending state changes for a single article.
// nil fields mean "no override" - keep the Lance value.
type articleOverride struct {
	IsRead    *bool
	IsStarred *bool
}

// writeCache holds pending article state changes in memory.
// Reads incorporate these via a CTE LEFT JOIN so the API always returns
// up-to-date values even before the periodic MERGE into Lance.
type writeCache struct {
	mu      sync.RWMutex
	pending map[string]*articleOverride // article_id → overrides
	timer   *time.Timer
	mergeFn func(overrides map[string]*articleOverride) error
	logFn   func(entry LogEntry) // optional: structured log callback (for frontend logs)
	cfg     CacheConfig
	stopped bool
}

func newWriteCache(cfg CacheConfig, mergeFn func(map[string]*articleOverride) error) *writeCache {
	if cfg.FlushThreshold <= 0 {
		cfg.FlushThreshold = 20
	}
	if cfg.FlushIntervalSecs <= 0 {
		cfg.FlushIntervalSecs = 120
	}
	return &writeCache{
		pending: make(map[string]*articleOverride),
		mergeFn: mergeFn,
		cfg:     cfg,
	}
}

// setRead records a pending is_read change.
func (c *writeCache) setRead(articleID string, isRead bool) {
	c.mu.Lock()

	debug.Log(debug.Batch, "cache setRead %s=%v (pending=%d)", articleID, isRead, len(c.pending))

	ov, ok := c.pending[articleID]
	if !ok {
		ov = &articleOverride{}
		c.pending[articleID] = ov
	}
	ov.IsRead = &isRead

	shouldFlush := len(c.pending) >= c.cfg.FlushThreshold
	c.startTimerLocked()
	c.mu.Unlock()

	if shouldFlush {
		c.flush()
	}
}

// setStarred records a pending is_starred change.
func (c *writeCache) setStarred(articleID string, isStarred bool) {
	c.mu.Lock()

	debug.Log(debug.Batch, "cache setStarred %s=%v (pending=%d)", articleID, isStarred, len(c.pending))

	ov, ok := c.pending[articleID]
	if !ok {
		ov = &articleOverride{}
		c.pending[articleID] = ov
	}
	ov.IsStarred = &isStarred

	shouldFlush := len(c.pending) >= c.cfg.FlushThreshold
	c.startTimerLocked()
	c.mu.Unlock()

	if shouldFlush {
		c.flush()
	}
}

// startTimerLocked starts the flush timer on the first pending entry.
// Caller must hold c.mu.
func (c *writeCache) startTimerLocked() {
	if len(c.pending) == 1 && c.timer == nil && c.cfg.FlushIntervalSecs > 0 {
		c.timer = time.AfterFunc(
			time.Duration(c.cfg.FlushIntervalSecs)*time.Second,
			func() { c.flush() },
		)
	}
}

// clearFeed removes all pending overrides for articles in the given feed.
// Called after MarkAllRead to avoid stale cache entries overriding the bulk write.
func (c *writeCache) clearFeed(articleIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range articleIDs {
		delete(c.pending, id)
	}
}

// ── Read helpers ──────────────────────────────────────────────────────────────

// pendingCTE returns a SQL CTE clause for the pending overrides, plus
// a boolean indicating whether the CTE was generated.
// When true, callers should LEFT JOIN _cache and use COALESCE.
// When false, no pending changes exist - use the simpler query form.
func (c *writeCache) pendingCTE() (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.pending) == 0 {
		return "", false
	}

	var values []string
	for id, ov := range c.pending {
		isRead := "CAST(NULL AS BOOLEAN)"
		if ov.IsRead != nil {
			isRead = fmt.Sprintf("%t", *ov.IsRead)
		}
		isStarred := "CAST(NULL AS BOOLEAN)"
		if ov.IsStarred != nil {
			isStarred = fmt.Sprintf("%t", *ov.IsStarred)
		}
		values = append(values, fmt.Sprintf("('%s', %s, %s)", escapeSQ(id), isRead, isStarred))
	}

	cte := fmt.Sprintf(
		"WITH _cache AS (SELECT * FROM (VALUES %s) AS t(article_id, is_read, is_starred)) ",
		strings.Join(values, ", "),
	)
	return cte, true
}

// ── Flush ─────────────────────────────────────────────────────────────────────

// flush merges all pending overrides into Lance and clears the cache.
func (c *writeCache) flush() error {
	c.mu.Lock()

	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}

	if len(c.pending) == 0 {
		c.mu.Unlock()
		return nil
	}

	// Snapshot and clear under lock
	overrides := c.pending
	c.pending = make(map[string]*articleOverride)
	count := len(overrides)
	c.mu.Unlock()

	debug.Log(debug.Batch, "flushing %d cached overrides to Lance...", count)

	err := c.mergeFn(overrides)
	if err != nil {
		log.Printf("cache flush (%d entries) error: %v", count, err)
		debug.Log(debug.Batch, "flush FAILED: %v", err)
		c.emitLog("error", fmt.Sprintf("Cache flush failed (%d entries): %v", count, err))

		// Put failed entries back (don't overwrite newer entries)
		c.mu.Lock()
		for id, ov := range overrides {
			if _, exists := c.pending[id]; !exists {
				c.pending[id] = ov
			}
		}
		c.mu.Unlock()
	} else {
		log.Printf("cache flush: %d writes committed to Lance", count)
		debug.Log(debug.Batch, "flush OK: %d writes", count)
		c.emitLog("info", fmt.Sprintf("Cache flush: %d writes committed to Lance", count))
	}
	return err
}

// close flushes any remaining writes and stops the cache.
func (c *writeCache) close() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()

	return c.flush()
}

// emitLog sends a structured log entry if a logFn callback is set.
func (c *writeCache) emitLog(level, message string) {
	if c.logFn == nil {
		return
	}
	now := time.Now().UTC()
	c.logFn(LogEntry{
		Timestamp: &now,
		Level:     level,
		Category:  "lifecycle",
		Message:   message,
	})
}

// Stats returns the number of pending read and star overrides in the cache.
func (c *writeCache) Stats() (pendingReads, pendingStars int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ov := range c.pending {
		if ov.IsRead != nil {
			pendingReads++
		}
		if ov.IsStarred != nil {
			pendingStars++
		}
	}
	return
}
