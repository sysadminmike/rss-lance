package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rss-lance/server/debug"
)

// OfflineConfig holds offline-mode settings read from the settings table.
type OfflineConfig struct {
	SnapshotIntervalMins int
	ArticleDays          int
	CachePath            string
}

// DefaultOfflineConfig returns the compiled defaults for offline mode.
func DefaultOfflineConfig() OfflineConfig {
	return OfflineConfig{
		SnapshotIntervalMins: 10,
		ArticleDays:          7,
		CachePath:            "./data/offline_cache.db",
	}
}

// loadOfflineConfig reads offline settings from the settings map,
// falling back to compiled defaults.
func loadOfflineConfig(settings map[string]string) OfflineConfig {
	d := DefaultOfflineConfig()
	d.SnapshotIntervalMins = settingInt(settings, "offline_snapshot_interval_mins", d.SnapshotIntervalMins)
	d.ArticleDays = settingInt(settings, "offline_article_days", d.ArticleDays)
	if v, ok := settings["offline_cache_path"]; ok {
		v = strings.Trim(v, `"`)
		if v != "" {
			d.CachePath = v
		}
	}
	return d
}

// offlineCache manages the local DuckDB cache file for offline mode.
// It holds cached copies of articles, feeds, categories, and settings,
// plus a pending_changes table for writes made while offline.
type offlineCache struct {
	mu   sync.Mutex
	conn *sql.DB
	cfg  OfflineConfig

	isOffline    atomic.Bool
	lastSnapshot time.Time

	// In-memory settings cache for fast reads while offline
	settingsMu    sync.RWMutex
	settingsCache map[string]string

	logFn func(entry LogEntry)
}

// newOfflineCache opens (or creates) the DuckDB cache file and
// ensures all required tables exist.
func newOfflineCache(cfg OfflineConfig) (*offlineCache, error) {
	// Ensure the parent directory exists so DuckDB can create the file.
	if dir := filepath.Dir(cfg.CachePath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create offline cache dir %s: %w", dir, err)
		}
	}

	conn, err := openOfflineDuckDB(cfg.CachePath)
	if err != nil {
		return nil, fmt.Errorf("open offline cache %s: %w", cfg.CachePath, err)
	}

	oc := &offlineCache{
		conn:          conn,
		cfg:           cfg,
		settingsCache: make(map[string]string),
	}

	if err := oc.ensureTables(); err != nil {
		conn.Close()
		return nil, err
	}

	log.Printf("Offline cache opened: %s", cfg.CachePath)
	return oc, nil
}

// ensureTables creates all required tables if they don't already exist.
func (oc *offlineCache) ensureTables() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS cached_articles (
			article_id VARCHAR PRIMARY KEY,
			feed_id VARCHAR,
			title VARCHAR,
			url VARCHAR,
			author VARCHAR,
			content VARCHAR,
			summary VARCHAR,
			published_at TIMESTAMP,
			fetched_at TIMESTAMP,
			is_read BOOLEAN,
			is_starred BOOLEAN,
			guid VARCHAR,
			created_at TIMESTAMP,
			updated_at TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS cached_feeds (
			feed_id VARCHAR PRIMARY KEY,
			title VARCHAR,
			url VARCHAR,
			site_url VARCHAR,
			icon_url VARCHAR,
			category_id VARCHAR,
			subcategory_id VARCHAR,
			last_fetched TIMESTAMP,
			last_article_date TIMESTAMP,
			fetch_interval_mins INTEGER,
			fetch_tier VARCHAR,
			error_count INTEGER,
			last_error VARCHAR,
			unread_count INTEGER,
			created_at TIMESTAMP,
			updated_at TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS cached_categories (
			category_id VARCHAR PRIMARY KEY,
			name VARCHAR,
			parent_id VARCHAR,
			sort_order INTEGER,
			created_at TIMESTAMP,
			updated_at TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS cached_settings (
			key VARCHAR PRIMARY KEY,
			value VARCHAR
		)`,
		`CREATE TABLE IF NOT EXISTS pending_changes (
			id INTEGER PRIMARY KEY,
			article_id VARCHAR,
			action VARCHAR,
			value VARCHAR,
			timestamp VARCHAR
		)`,
		// Auto-increment sequence for pending_changes
		`CREATE SEQUENCE IF NOT EXISTS pending_changes_seq START 1`,
		// Log fallback table - stores log entries when Lance is unreachable
		`CREATE TABLE IF NOT EXISTS cached_logs (
			log_id VARCHAR PRIMARY KEY,
			timestamp TIMESTAMP,
			level VARCHAR,
			category VARCHAR,
			message VARCHAR,
			details VARCHAR,
			created_at TIMESTAMP
		)`,
	}

	for _, stmt := range stmts {
		if _, err := oc.conn.Exec(stmt); err != nil {
			return fmt.Errorf("offline cache schema: %w (stmt: %s)", err, stmt[:min(80, len(stmt))])
		}
	}
	return nil
}

// close shuts down the DuckDB connection.
func (oc *offlineCache) close() error {
	if oc.conn != nil {
		return oc.conn.Close()
	}
	return nil
}

// ── Cached logs (DuckDB fallback for log writes) ─────────────────────────────

// insertLogs batch-inserts log entries into the cached_logs table.
func (oc *offlineCache) insertLogs(entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	oc.mu.Lock()
	defer oc.mu.Unlock()

	tx, err := oc.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO cached_logs
		(log_id, timestamp, level, category, message, details, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		_, err := stmt.Exec(e.LogID, e.Timestamp, e.Level, e.Category,
			e.Message, e.Details, e.CreatedAt)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// drainLogs reads the oldest N log entries from cached_logs for replay to Lance.
func (oc *offlineCache) drainLogs(batchSize int) ([]LogEntry, error) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	rows, err := oc.conn.Query(fmt.Sprintf(
		`SELECT log_id, timestamp, level, category, message, details, created_at
		 FROM cached_logs ORDER BY created_at ASC LIMIT %d`, batchSize))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.LogID, &e.Timestamp, &e.Level, &e.Category,
			&e.Message, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// deleteLogs removes specific log entries from cached_logs by log_id.
func (oc *offlineCache) deleteLogs(logIDs []string) error {
	if len(logIDs) == 0 {
		return nil
	}

	oc.mu.Lock()
	defer oc.mu.Unlock()

	quoted := make([]string, len(logIDs))
	for i, id := range logIDs {
		quoted[i] = "'" + escapeSQ(id) + "'"
	}
	_, err := oc.conn.Exec(fmt.Sprintf(
		"DELETE FROM cached_logs WHERE log_id IN (%s)",
		strings.Join(quoted, ", ")))
	return err
}

// pendingLogCount returns the number of log entries waiting in cached_logs.
func (oc *offlineCache) pendingLogCount() int {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	var count int
	oc.conn.QueryRow("SELECT COUNT(*) FROM cached_logs").Scan(&count)
	return count
}

// queryCachedLogs returns cached log entries matching the given filters.
// It returns entries with Service="api" (cached logs are always API-origin)
// sorted by timestamp DESC. This does NOT delete the entries.
func (oc *offlineCache) queryCachedLogs(level, category string) ([]LogEntry, error) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	var conditions []string
	if level != "" {
		conditions = append(conditions, fmt.Sprintf("level = '%s'", escapeSQ(level)))
	}
	if category != "" {
		conditions = append(conditions, fmt.Sprintf("category = '%s'", escapeSQ(category)))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	q := fmt.Sprintf(
		`SELECT log_id, timestamp, level, category, message, details, created_at
		 FROM cached_logs%s ORDER BY timestamp DESC`, where)

	rows, err := oc.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.LogID, &e.Timestamp, &e.Level, &e.Category,
			&e.Message, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Service = "api"
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ── Snapshot ──────────────────────────────────────────────────────────────────

// snapshotArticles upserts recent articles into the cache.
func (oc *offlineCache) snapshotArticles(articles []Article) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "snapshot articles: %d to cache", len(articles))

	if len(articles) == 0 {
		return nil
	}

	tx, err := oc.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO cached_articles
		(article_id, feed_id, title, url, author, content, summary,
		 published_at, fetched_at, is_read, is_starred, guid, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, a := range articles {
		_, err := stmt.Exec(
			a.ArticleID, a.FeedID, a.Title, a.URL, a.Author,
			a.Content, a.Summary, a.PublishedAt, a.FetchedAt,
			a.IsRead, a.IsStarred, "", a.CreatedAt, a.UpdatedAt,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// snapshotFeeds upserts all feeds into the cache.
func (oc *offlineCache) snapshotFeeds(feeds []Feed) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "snapshot feeds: %d to cache", len(feeds))

	if len(feeds) == 0 {
		return nil
	}

	tx, err := oc.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Clear and replace (feeds are a small table)
	if _, err := tx.Exec("DELETE FROM cached_feeds"); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO cached_feeds
		(feed_id, title, url, site_url, icon_url, category_id, subcategory_id,
		 last_fetched, last_article_date, fetch_interval_mins, fetch_tier,
		 error_count, last_error, unread_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, f := range feeds {
		_, err := stmt.Exec(
			f.FeedID, f.Title, f.URL, f.SiteURL, f.IconURL,
			f.CategoryID, f.SubcategoryID,
			f.LastFetched, f.LastArticleDate,
			f.FetchIntervalMins, f.FetchTier,
			f.ErrorCount, f.LastError, f.UnreadCount,
			f.CreatedAt, f.UpdatedAt,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// snapshotCategories upserts all categories into the cache.
func (oc *offlineCache) snapshotCategories(cats []Category) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "snapshot categories: %d to cache", len(cats))

	tx, err := oc.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM cached_categories"); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO cached_categories
		(category_id, name, parent_id, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range cats {
		_, err := stmt.Exec(c.CategoryID, c.Name, c.ParentID, c.SortOrder, c.CreatedAt, c.UpdatedAt)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// snapshotSettings upserts all settings into the cache and in-memory map.
func (oc *offlineCache) snapshotSettings(settings map[string]string) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "snapshot settings: %d to cache", len(settings))

	tx, err := oc.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM cached_settings"); err != nil {
		return err
	}

	stmt, err := tx.Prepare("INSERT OR IGNORE INTO cached_settings (key, value) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for k, v := range settings {
		if _, err := stmt.Exec(k, v); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Update in-memory cache
	oc.settingsMu.Lock()
	oc.settingsCache = make(map[string]string, len(settings))
	for k, v := range settings {
		oc.settingsCache[k] = v
	}
	oc.settingsMu.Unlock()

	return nil
}

// updateLastSnapshot records the time of the last successful snapshot.
func (oc *offlineCache) updateLastSnapshot() {
	oc.lastSnapshot = time.Now().UTC()
}

// ── Offline reads ─────────────────────────────────────────────────────────────

// getFeeds returns cached feeds for offline use.
func (oc *offlineCache) getFeeds() ([]Feed, error) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	rows, err := oc.conn.Query(`
		SELECT feed_id, title, url, site_url, icon_url,
		       category_id, subcategory_id,
		       last_fetched, last_article_date,
		       fetch_interval_mins, fetch_tier,
		       error_count, last_error, unread_count,
		       created_at, updated_at
		FROM cached_feeds
		ORDER BY LOWER(title)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var feeds []Feed
	for rows.Next() {
		var f Feed
		if err := rows.Scan(
			&f.FeedID, &f.Title, &f.URL, &f.SiteURL, &f.IconURL,
			&f.CategoryID, &f.SubcategoryID,
			&f.LastFetched, &f.LastArticleDate,
			&f.FetchIntervalMins, &f.FetchTier,
			&f.ErrorCount, &f.LastError, &f.UnreadCount,
			&f.CreatedAt, &f.UpdatedAt,
		); err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// getArticles returns cached articles with filtering and pagination.
func (oc *offlineCache) getArticles(feedID string, limit, offset int, unreadOnly bool, sortAsc bool) ([]Article, error) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	where := "WHERE 1=1"
	if feedID != "" {
		where += fmt.Sprintf(" AND feed_id = '%s'", escapeSQ(feedID))
	}
	if unreadOnly {
		where += " AND is_read = false"
	}

	sortDir := "DESC"
	if sortAsc {
		sortDir = "ASC"
	}

	q := fmt.Sprintf(`SELECT article_id, feed_id, title, url, author,
		summary, published_at, fetched_at, is_read, is_starred,
		created_at, updated_at
		FROM cached_articles %s
		ORDER BY published_at %s
		LIMIT %d OFFSET %d`, where, sortDir, limit, offset)

	rows, err := oc.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var arts []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(
			&a.ArticleID, &a.FeedID, &a.Title, &a.URL, &a.Author,
			&a.Summary, &a.PublishedAt, &a.FetchedAt,
			&a.IsRead, &a.IsStarred,
			&a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		arts = append(arts, a)
	}
	return arts, rows.Err()
}

// getArticle returns a single cached article with content.
func (oc *offlineCache) getArticle(articleID string) (*Article, error) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	row := oc.conn.QueryRow(`SELECT article_id, feed_id, title, url, author,
		content, summary, published_at, fetched_at, is_read, is_starred,
		created_at, updated_at
		FROM cached_articles WHERE article_id = ?`, articleID)

	var a Article
	err := row.Scan(
		&a.ArticleID, &a.FeedID, &a.Title, &a.URL, &a.Author,
		&a.Content, &a.Summary, &a.PublishedAt, &a.FetchedAt,
		&a.IsRead, &a.IsStarred,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

// getArticleBatch returns multiple cached articles with content.
func (oc *offlineCache) getArticleBatch(articleIDs []string) ([]Article, error) {
	if len(articleIDs) == 0 {
		return nil, nil
	}

	oc.mu.Lock()
	defer oc.mu.Unlock()

	quoted := make([]string, len(articleIDs))
	for i, id := range articleIDs {
		quoted[i] = "'" + escapeSQ(id) + "'"
	}
	inClause := strings.Join(quoted, ", ")

	q := fmt.Sprintf(`SELECT article_id, feed_id, title, url, author,
		content, summary, published_at, fetched_at, is_read, is_starred,
		created_at, updated_at
		FROM cached_articles WHERE article_id IN (%s)`, inClause)

	rows, err := oc.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var arts []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(
			&a.ArticleID, &a.FeedID, &a.Title, &a.URL, &a.Author,
			&a.Content, &a.Summary, &a.PublishedAt, &a.FetchedAt,
			&a.IsRead, &a.IsStarred,
			&a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		arts = append(arts, a)
	}
	return arts, rows.Err()
}

// getCategories returns cached categories.
func (oc *offlineCache) getCategories() ([]Category, error) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	rows, err := oc.conn.Query(`SELECT category_id, name, parent_id, sort_order,
		created_at, updated_at
		FROM cached_categories ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cats []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.CategoryID, &c.Name, &c.ParentID, &c.SortOrder, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	return cats, rows.Err()
}

// getSettings returns cached settings from in-memory copy (fast).
func (oc *offlineCache) getSettings() map[string]string {
	oc.settingsMu.RLock()
	defer oc.settingsMu.RUnlock()
	result := make(map[string]string, len(oc.settingsCache))
	for k, v := range oc.settingsCache {
		result[k] = v
	}
	return result
}

// getSetting returns a single cached setting from in-memory copy.
func (oc *offlineCache) getSetting(key string) (string, bool) {
	oc.settingsMu.RLock()
	defer oc.settingsMu.RUnlock()
	v, ok := oc.settingsCache[key]
	return v, ok
}

// ── Offline writes (pending changes) ──────────────────────────────────────────

// addPendingChange records a user action in the pending_changes table.
func (oc *offlineCache) addPendingChange(articleID, action, value string) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "offline pending change: %s %s (value=%q)", action, articleID, value)

	ts := time.Now().UTC().Format(time.RFC3339Nano)

	_, err := oc.conn.Exec(`INSERT INTO pending_changes (id, article_id, action, value, timestamp)
		VALUES (nextval('pending_changes_seq'), ?, ?, ?, ?)`,
		articleID, action, value, ts)
	return err
}

// addPendingSettingChange records a setting change with UPSERT semantics.
// Only the final value for each setting key is kept.
func (oc *offlineCache) addPendingSettingChange(key, value string) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "offline pending setting: %s = %q", key, value)

	ts := time.Now().UTC().Format(time.RFC3339Nano)

	// Delete existing pending setting change for this key, then insert new one
	_, err := oc.conn.Exec(
		`DELETE FROM pending_changes WHERE action = 'setting' AND article_id = ?`, key)
	if err != nil {
		return err
	}
	_, err = oc.conn.Exec(`INSERT INTO pending_changes (id, article_id, action, value, timestamp)
		VALUES (nextval('pending_changes_seq'), ?, 'setting', ?, ?)`,
		key, value, ts)
	return err
}

// updateCachedArticle updates a cached article's read/star state locally.
func (oc *offlineCache) updateCachedArticle(articleID string, isRead *bool, isStarred *bool) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "offline cache update article %s (read=%v, starred=%v)", articleID, isRead, isStarred)

	var sets []string
	if isRead != nil {
		sets = append(sets, fmt.Sprintf("is_read = %t", *isRead))
	}
	if isStarred != nil {
		sets = append(sets, fmt.Sprintf("is_starred = %t", *isStarred))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, fmt.Sprintf("updated_at = '%s'", time.Now().UTC().Format("2006-01-02 15:04:05.000000")))

	q := fmt.Sprintf("UPDATE cached_articles SET %s WHERE article_id = '%s'",
		strings.Join(sets, ", "), escapeSQ(articleID))
	_, err := oc.conn.Exec(q)
	return err
}

// markAllReadCached marks all articles for a feed as read in the cache.
func (oc *offlineCache) markAllReadCached(feedID string) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "offline cache mark-all-read feed %s", feedID)

	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000000")
	_, err := oc.conn.Exec(
		fmt.Sprintf("UPDATE cached_articles SET is_read = true, updated_at = '%s' WHERE feed_id = '%s'",
			ts, escapeSQ(feedID)))
	return err
}

// updateCachedSetting updates a setting in both the DuckDB cache and in-memory map.
func (oc *offlineCache) updateCachedSetting(key, value string) error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	debug.Log(debug.Lance, "offline cache update setting %s = %q", key, value)

	_, err := oc.conn.Exec(`INSERT OR REPLACE INTO cached_settings (key, value) VALUES (?, ?)`, key, value)
	if err != nil {
		return err
	}

	oc.settingsMu.Lock()
	oc.settingsCache[key] = value
	oc.settingsMu.Unlock()

	return nil
}

// ── Replay ────────────────────────────────────────────────────────────────────

// PendingChange represents a single change recorded while offline.
type PendingChange struct {
	ID        int
	ArticleID string
	Action    string
	Value     string
	Timestamp string
}

// getPendingChanges returns all pending changes in order.
func (oc *offlineCache) getPendingChanges() ([]PendingChange, error) {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	rows, err := oc.conn.Query("SELECT id, article_id, action, value, timestamp FROM pending_changes ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var changes []PendingChange
	for rows.Next() {
		var c PendingChange
		if err := rows.Scan(&c.ID, &c.ArticleID, &c.Action, &c.Value, &c.Timestamp); err != nil {
			return nil, err
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// pendingCount returns the number of pending changes.
func (oc *offlineCache) pendingCount() int {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	var count int
	oc.conn.QueryRow("SELECT COUNT(*) FROM pending_changes").Scan(&count)
	return count
}

// cachedArticleCount returns the number of cached articles.
func (oc *offlineCache) cachedArticleCount() int {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	var count int
	oc.conn.QueryRow("SELECT COUNT(*) FROM cached_articles").Scan(&count)
	return count
}

// clearPendingChanges removes all pending changes after successful replay.
func (oc *offlineCache) clearPendingChanges() error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	_, err := oc.conn.Exec("DELETE FROM pending_changes")
	return err
}

// evictReadArticles removes read articles from the cache after replay.
func (oc *offlineCache) evictReadArticles() error {
	oc.mu.Lock()
	defer oc.mu.Unlock()

	_, err := oc.conn.Exec("DELETE FROM cached_articles WHERE is_read = true")
	return err
}

// ── Status ────────────────────────────────────────────────────────────────────

// OfflineStatus holds the state returned by the offline status API.
type OfflineStatus struct {
	Offline        bool   `json:"offline"`
	PendingChanges int    `json:"pending_changes"`
	PendingLogs    int    `json:"pending_logs"`
	LastSnapshot   string `json:"last_snapshot"`
	CacheArticles  int    `json:"cache_articles"`
}

// status returns the current offline cache status.
func (oc *offlineCache) status() OfflineStatus {
	lastSnap := ""
	if !oc.lastSnapshot.IsZero() {
		lastSnap = oc.lastSnapshot.Format(time.RFC3339)
	}
	return OfflineStatus{
		Offline:        oc.isOffline.Load(),
		PendingChanges: oc.pendingCount(),
		PendingLogs:    oc.pendingLogCount(),
		LastSnapshot:   lastSnap,
		CacheArticles:  oc.cachedArticleCount(),
	}
}

// ── Snapshot orchestrator ─────────────────────────────────────────────────────

// Snapshot runs a full snapshot cycle: query Lance via the store, write to cache.
// The store parameter is used to read live data from Lance.
// Returns the number of articles, feeds, categories, and settings cached.
func (oc *offlineCache) Snapshot(store Store) error {
	debug.Log(debug.Lance, "offline snapshot starting...")

	// Read articles updated in the last N days
	cutoff := time.Now().UTC().AddDate(0, 0, -oc.cfg.ArticleDays)
	cutoffStr := cutoff.Format("2006-01-02 15:04:05")
	_ = cutoffStr // Used below via store queries

	// Get all articles within the time window
	// We fetch in pages to avoid massive single queries
	const pageSize = 5000
	var allArticles []Article
	offset := 0
	for {
		arts, err := store.GetArticles("", pageSize, offset, false, false)
		if err != nil {
			return fmt.Errorf("snapshot articles page %d: %w", offset/pageSize, err)
		}
		// Filter by updated_at within the window
		for _, a := range arts {
			if a.UpdatedAt != nil && a.UpdatedAt.After(cutoff) {
				allArticles = append(allArticles, a)
			} else if a.UpdatedAt == nil && a.PublishedAt != nil && a.PublishedAt.After(cutoff) {
				// Fallback for articles without updated_at
				allArticles = append(allArticles, a)
			}
		}
		if len(arts) < pageSize {
			break
		}
		offset += pageSize
	}

	// For articles that were fetched without content (list view), fetch content
	// in batches so the cache has full content for offline reading
	var needContent []string
	for _, a := range allArticles {
		if a.Content == "" {
			needContent = append(needContent, a.ArticleID)
		}
	}
	if len(needContent) > 0 {
		// Batch fetch content in chunks of 100
		contentMap := make(map[string]string)
		for i := 0; i < len(needContent); i += 100 {
			end := i + 100
			if end > len(needContent) {
				end = len(needContent)
			}
			batch, err := store.GetArticleBatch(needContent[i:end])
			if err != nil {
				debug.Log(debug.Lance, "snapshot content batch error: %v", err)
				continue
			}
			for _, a := range batch {
				contentMap[a.ArticleID] = a.Content
			}
		}
		// Merge content back
		for i := range allArticles {
			if c, ok := contentMap[allArticles[i].ArticleID]; ok && c != "" {
				allArticles[i].Content = c
			}
		}
	}

	if err := oc.snapshotArticles(allArticles); err != nil {
		return fmt.Errorf("snapshot articles: %w", err)
	}

	// Snapshot feeds
	feeds, err := store.GetFeeds()
	if err != nil {
		return fmt.Errorf("snapshot feeds: %w", err)
	}
	if err := oc.snapshotFeeds(feeds); err != nil {
		return fmt.Errorf("snapshot feeds write: %w", err)
	}

	// Snapshot categories
	cats, err := store.GetCategories()
	if err != nil {
		return fmt.Errorf("snapshot categories: %w", err)
	}
	if err := oc.snapshotCategories(cats); err != nil {
		return fmt.Errorf("snapshot categories write: %w", err)
	}

	// Snapshot settings
	settings, err := store.GetSettings()
	if err != nil {
		return fmt.Errorf("snapshot settings: %w", err)
	}
	if err := oc.snapshotSettings(settings); err != nil {
		return fmt.Errorf("snapshot settings write: %w", err)
	}

	oc.updateLastSnapshot()

	debug.Log(debug.Lance, "offline snapshot complete: %d articles, %d feeds, %d categories, %d settings",
		len(allArticles), len(feeds), len(cats), len(settings))

	oc.emitLog("info", fmt.Sprintf("Snapshot completed: %d articles, %d feeds, %d categories",
		len(allArticles), len(feeds), len(cats)))

	return nil
}

// ── Replay orchestrator ───────────────────────────────────────────────────────

// replayState holds collapsed article state during replay.
type replayState struct {
	IsRead    *bool
	IsStarred *bool
}

// Replay reads pending changes and applies them to Lance via the writer.
// Returns the number of changes replayed.
func (oc *offlineCache) Replay(writer *lanceWriter) (int, error) {
	changes, err := oc.getPendingChanges()
	if err != nil {
		return 0, fmt.Errorf("read pending changes: %w", err)
	}
	if len(changes) == 0 {
		return 0, nil
	}

	debug.Log(debug.Lance, "replaying %d pending changes...", len(changes))

	// Collapse article changes per article_id to final state
	articleStates := make(map[string]*replayState)
	var markAllReadFeeds []string
	var settingChanges []PendingChange

	for _, c := range changes {
		switch c.Action {
		case "read":
			s := getOrCreateReplayState(articleStates, c.ArticleID)
			v := true
			s.IsRead = &v
		case "unread":
			s := getOrCreateReplayState(articleStates, c.ArticleID)
			v := false
			s.IsRead = &v
		case "star":
			s := getOrCreateReplayState(articleStates, c.ArticleID)
			v := true
			s.IsStarred = &v
		case "unstar":
			s := getOrCreateReplayState(articleStates, c.ArticleID)
			v := false
			s.IsStarred = &v
		case "mark_all_read":
			markAllReadFeeds = append(markAllReadFeeds, c.ArticleID)
		case "setting":
			settingChanges = append(settingChanges, c)
		}
	}

	// Replay mark-all-read first
	for _, feedID := range markAllReadFeeds {
		if err := writer.MarkAllRead(feedID); err != nil {
			return 0, fmt.Errorf("replay mark_all_read %s: %w", feedID, err)
		}
	}

	// Replay article state changes via FlushOverrides
	if len(articleStates) > 0 {
		overrides := make(map[string]*articleOverride, len(articleStates))
		for id, s := range articleStates {
			overrides[id] = &articleOverride{
				IsRead:    s.IsRead,
				IsStarred: s.IsStarred,
			}
		}
		if err := writer.FlushOverrides(overrides); err != nil {
			return 0, fmt.Errorf("replay article overrides: %w", err)
		}
	}

	// Replay setting changes
	if len(settingChanges) > 0 {
		settingsMap := make(map[string]string, len(settingChanges))
		for _, c := range settingChanges {
			settingsMap[c.ArticleID] = c.Value // ArticleID holds the setting key
		}
		if err := writer.PutSettingsBatch(settingsMap); err != nil {
			return 0, fmt.Errorf("replay settings: %w", err)
		}
	}

	// Clear pending changes
	if err := oc.clearPendingChanges(); err != nil {
		return 0, fmt.Errorf("clear pending changes: %w", err)
	}

	// Evict read articles
	if err := oc.evictReadArticles(); err != nil {
		debug.Log(debug.Lance, "evict read articles error: %v", err)
	}

	replayed := len(changes)
	debug.Log(debug.Lance, "replay complete: %d changes applied", replayed)
	oc.emitLog("info", fmt.Sprintf("Reconnected: %d changes replayed", replayed))

	return replayed, nil
}

func getOrCreateReplayState(m map[string]*replayState, id string) *replayState {
	if s, ok := m[id]; ok {
		return s
	}
	s := &replayState{}
	m[id] = s
	return s
}

// emitLog sends a structured log entry if logFn is set.
func (oc *offlineCache) emitLog(level, message string) {
	if oc.logFn == nil {
		return
	}
	now := time.Now().UTC()
	oc.logFn(LogEntry{
		Timestamp: &now,
		Level:     level,
		Category:  "offline",
		Message:   message,
	})
}
