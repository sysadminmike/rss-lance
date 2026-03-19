// Package db - write cache for Lance table state changes.
//
// Read queries incorporate pending changes via a generated CTE + LEFT JOIN,
// so the API always returns up-to-date state. Writes go to DuckDB
// pending_changes; this cache is a read-only in-memory overlay.
//
// Background flush goroutine (runPendingFlush) periodically replays
// pending_changes from DuckDB into Lance. After a successful flush,
// the in-memory overlay is cleared.
package db

import (
	"fmt"
	"strings"
	"sync"

	"rss-lance/server/debug"
)

// articleOverride holds pending state changes for a single article.
// nil fields mean "no override" - keep the Lance value.
type articleOverride struct {
	IsRead    *bool
	IsStarred *bool
}

// writeCache holds pending article state changes in memory.
// Reads incorporate these via a CTE LEFT JOIN so the API always returns
// up-to-date values even before the background flush to Lance.
// Writes go to DuckDB pending_changes; this cache is a read-only overlay.
type writeCache struct {
	mu      sync.RWMutex
	pending map[string]*articleOverride // article_id → overrides
	stopped bool
}

func newWriteCache() *writeCache {
	return &writeCache{
		pending: make(map[string]*articleOverride),
	}
}

// setRead records a pending is_read change in the in-memory overlay.
func (c *writeCache) setRead(articleID string, isRead bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	debug.Log(debug.Batch, "cache setRead %s=%v (pending=%d)", articleID, isRead, len(c.pending))

	ov, ok := c.pending[articleID]
	if !ok {
		ov = &articleOverride{}
		c.pending[articleID] = ov
	}
	ov.IsRead = &isRead
}

// setStarred records a pending is_starred change in the in-memory overlay.
func (c *writeCache) setStarred(articleID string, isStarred bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	debug.Log(debug.Batch, "cache setStarred %s=%v (pending=%d)", articleID, isStarred, len(c.pending))

	ov, ok := c.pending[articleID]
	if !ok {
		ov = &articleOverride{}
		c.pending[articleID] = ov
	}
	ov.IsStarred = &isStarred
}

// clearFeed removes all pending overrides for articles in the given feed.
func (c *writeCache) clearFeed(articleIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range articleIDs {
		delete(c.pending, id)
	}
}

// clear removes all pending overrides from the in-memory cache.
// Called after a successful background flush to Lance.
func (c *writeCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = make(map[string]*articleOverride)
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

// close is a no-op; pending changes are persisted in DuckDB.
func (c *writeCache) close() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()
	return nil
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
