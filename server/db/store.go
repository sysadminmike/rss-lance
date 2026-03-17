// Package db defines the Store interface and shared types for RSS-Lance.
//
// ALL state lives in Lance tables - there is no local DuckDB database file.
// DuckDB is used only as a query/write engine against Lance format data.
// This means multiple machines can share the same Lance data (via local
// disk, NFS, Samba, or S3) and all see the same feeds, articles, and
// read/starred state.
//
// Two implementations exist, selected at compile time via build tags:
//   - lance_windows.go: shells out to duckdb.exe CLI (no CGo/GCC needed)
//   - lance_cgo.go:     uses embedded go-duckdb via CGo (Linux/FreeBSD)
package db

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store defines the database operations for the RSS-Lance server.
type Store interface {
	Close() error

	// Feeds
	GetFeeds() ([]Feed, error)
	GetFeed(feedID string) (*Feed, error)
	QueueFeed(url, categoryID string) error
	GetPendingFeeds() ([]string, error)

	// Articles
	GetArticles(feedID string, limit, offset int, unreadOnly bool, sortAsc bool) ([]Article, error)
	GetArticle(articleID string) (*Article, error)
	GetArticleBatch(articleIDs []string) ([]Article, error)
	SetArticleRead(articleID string, isRead bool) error
	SetArticleStarred(articleID string, isStarred bool) error
	MarkAllRead(feedID string) error

	// Categories
	GetCategories() ([]Category, error)

	// Settings (key/value store)
	GetSettings() (map[string]string, error)
	GetSetting(key string) (string, bool, error)
	PutSetting(key, value string) error
	PutSettings(settings map[string]string) error

	// Logging
	WriteLog(entry LogEntry) error
	GetLogs(opts LogQuery) ([]LogEntry, int, error)
	TrimLogs(maxEntries int) error
	TrimLogsByAge(maxAgeDays int) error

	// Status / diagnostics
	GetDBStatus() (*DBStatus, error)

	// Write cache stats
	CacheStats() (pendingReads, pendingStars int)

	// Raw table viewer
	QueryTable(table string, limit, offset int) (*TableQueryResult, error)
}

// ── Types ─────────────────────────────────────────────────────────────────────

type Feed struct {
	FeedID            string     `json:"feed_id"`
	Title             string     `json:"title"`
	URL               string     `json:"url"`
	SiteURL           string     `json:"site_url"`
	IconURL           string     `json:"icon_url"`
	CategoryID        string     `json:"category_id"`
	SubcategoryID     string     `json:"subcategory_id"`
	LastFetched       *time.Time `json:"last_fetched"`
	LastArticleDate   *time.Time `json:"last_article_date"`
	FetchIntervalMins int        `json:"fetch_interval_mins"`
	FetchTier         string     `json:"fetch_tier"`
	ErrorCount        int        `json:"error_count"`
	LastError         string     `json:"last_error"`
	UnreadCount       int        `json:"unread_count"`
	CreatedAt         *time.Time `json:"created_at"`
	UpdatedAt         *time.Time `json:"updated_at"`
}

type Article struct {
	ArticleID   string     `json:"article_id"`
	FeedID      string     `json:"feed_id"`
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	Author      string     `json:"author"`
	Content     string     `json:"content,omitempty"`
	Summary     string     `json:"summary"`
	PublishedAt *time.Time `json:"published_at"`
	FetchedAt   *time.Time `json:"fetched_at"`
	IsRead      bool       `json:"is_read"`
	IsStarred   bool       `json:"is_starred"`
	CreatedAt   *time.Time `json:"created_at"`
	UpdatedAt   *time.Time `json:"updated_at"`
}

type Category struct {
	CategoryID string     `json:"category_id"`
	Name       string     `json:"name"`
	ParentID   string     `json:"parent_id"`
	SortOrder  int        `json:"sort_order"`
	CreatedAt  *time.Time `json:"created_at"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// DBStatus holds diagnostic information about the database tables.
type DBStatus struct {
	Tables   []TableStats      `json:"tables"`
	Articles ArticleStats      `json:"articles"`
	DataPath string            `json:"data_path"`
}

// LogEntry represents a single log record. Schema is shared between
// log_api and log_fetcher tables so they can be UNIONed.
type LogEntry struct {
	LogID     string     `json:"log_id"`
	Timestamp *time.Time `json:"timestamp"`
	Level     string     `json:"level"`     // debug / info / warn / error
	Category  string     `json:"category"`  // grouped category name
	Service   string     `json:"service"`   // "api" or "fetcher" (populated at query time)
	Message   string     `json:"message"`
	Details   string     `json:"details"`   // optional JSON blob
	CreatedAt *time.Time `json:"created_at"`
}

// LogQuery defines filters for querying logs.
type LogQuery struct {
	Service  string // "api", "fetcher", or "" for all
	Level    string // "debug", "info", "warn", "error", or "" for all
	Category string // specific category or "" for all
	Limit    int
	Offset   int
}

// ColumnInfo describes a single column in a Lance table.
type ColumnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// LanceIndexInfo describes an index on a Lance table.
type LanceIndexInfo struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	IndexType string   `json:"index_type"`
}

// TableStats holds per-table statistics.
type TableStats struct {
	Name       string           `json:"name"`
	RowCount   int              `json:"row_count"`
	SizeBytes  int64            `json:"size_bytes"`
	Version    int              `json:"version"`
	NumColumns int              `json:"num_columns"`
	Columns    []ColumnInfo     `json:"columns"`
	Indexes    []LanceIndexInfo `json:"indexes"`
}

// ArticleStats holds aggregate article-level stats.
type ArticleStats struct {
	Total    int    `json:"total"`
	Unread   int    `json:"unread"`
	Starred  int    `json:"starred"`
	Oldest   string `json:"oldest"`
	Newest   string `json:"newest"`
}

// TableQueryResult holds paginated raw rows from a table.
type TableQueryResult struct {
	Table   string           `json:"table"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
}

// ── Shared helpers ────────────────────────────────────────────────────────────

// allowedTables is the set of Lance tables that can be queried via the raw
// table viewer.  Keeps the API surface explicit and prevents arbitrary SQL.
var allowedTables = map[string]bool{
	"articles":      true,
	"feeds":         true,
	"categories":    true,
	"pending_feeds": true,
	"settings":      true,
	"log_api":       true,
	"log_fetcher":   true,
}

// escapeSQ escapes single quotes for inline SQL string literals.
func escapeSQ(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'', '\'')
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

// dirSize recursively calculates the total size of a directory in bytes.
func dirSize(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size
}

// settingInt returns the integer value for a key in the settings map,
// falling back to def if not present or not parseable.
func settingInt(settings map[string]string, key string, def int) int {
	v, ok := settings[key]
	if !ok || v == "" {
		return def
	}
	// Settings may be stored as JSON-encoded strings (e.g. "20" or just 20)
	v = strings.Trim(v, "\"")
	n := 0
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// loadCacheConfig reads cache.flush_threshold and cache.flush_interval_secs
// from the settings table, falling back to compiled defaults.
func loadCacheConfig(settings map[string]string) CacheConfig {
	d := DefaultCacheConfig()
	return CacheConfig{
		FlushThreshold:    settingInt(settings, "cache.flush_threshold", d.FlushThreshold),
		FlushIntervalSecs: settingInt(settings, "cache.flush_interval_secs", d.FlushIntervalSecs),
	}
}

// loadLogBufferConfig reads log_buffer.flush_threshold and
// log_buffer.flush_interval_secs from the settings table.
func loadLogBufferConfig(settings map[string]string) LogBufferConfig {
	d := DefaultLogBufferConfig()
	return LogBufferConfig{
		FlushThreshold:    settingInt(settings, "log_buffer.flush_threshold", d.FlushThreshold),
		FlushIntervalSecs: settingInt(settings, "log_buffer.flush_interval_secs", d.FlushIntervalSecs),
	}
}
