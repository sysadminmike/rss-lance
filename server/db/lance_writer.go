// lance_writer.go - CUD (Create/Update/Delete) operations via lancedb-go native SDK.
//
// DuckDB remains the query engine for all SELECT operations (it handles JOINs,
// CTEs, aggregations, etc. against Lance-format files perfectly).
//
// However, DuckDB's Lance extension cannot do UPDATE with joins/subqueries:
//
//	"Not implemented Error: Lance UPDATE does not support UPDATE with joins or FROM"
//
// The lancedb-go SDK talks to Lance tables natively and fully supports
// Insert, Update, and Delete.  This file provides a lanceWriter that both
// lance_windows.go (CLI) and lance_cgo.go (embedded) embed for their
// write operations, keeping DuckDB exclusively for reads.
package db

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"rss-lance/server/debug"

	"github.com/apache/arrow/go/v17/arrow"
	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	"github.com/lancedb/lancedb-go/pkg/contracts"
	"github.com/lancedb/lancedb-go/pkg/lancedb"
)

// isCloudURI returns true for cloud storage URIs (s3://, gs://, az://).
func isCloudURI(path string) bool {
	return len(path) > 5 &&
		(path[:5] == "s3://" || path[:5] == "gs://" || path[:5] == "az://")
}

// lanceWriter wraps a native lancedb-go connection for CUD operations.
// Table handles are opened lazily and cached for reuse.
type lanceWriter struct {
	conn     contracts.IConnection
	dataPath string

	// Cached table handles (opened lazily)
	articles     contracts.ITable
	feeds        contracts.ITable
	categories   contracts.ITable
	pendingFeeds contracts.ITable
	settings     contracts.ITable
	logAPI       contracts.ITable
}

// newLanceWriter opens a native lancedb-go connection to the data directory.
func newLanceWriter(dataPath string) (*lanceWriter, error) {
	ctx := context.Background()
	conn, err := lancedb.Connect(ctx, dataPath, nil)
	if err != nil {
		return nil, fmt.Errorf("lancedb connect: %w", err)
	}

	log.Printf("LanceDB native writer connected: %s", dataPath)
	debug.Log(debug.Lance, "lancedb-go writer connected to %s", dataPath)

	return &lanceWriter{
		conn:     conn,
		dataPath: dataPath,
	}, nil
}

// close releases the native connection and any open table handles.
func (w *lanceWriter) close() error {
	tables := []contracts.ITable{w.articles, w.feeds, w.categories, w.pendingFeeds, w.settings, w.logAPI}
	for _, t := range tables {
		if t != nil {
			t.Close()
		}
	}
	return w.conn.Close()
}

// getTableMeta returns lance-native metadata (version, columns, indexes) for a table.
func (w *lanceWriter) getTableMeta(name string) (version int, columns []ColumnInfo, indexes []LanceIndexInfo) {
	ctx := context.Background()
	t, err := w.conn.OpenTable(ctx, name)
	if err != nil {
		return 0, nil, nil
	}
	defer t.Close()

	if v, err := t.Version(ctx); err == nil {
		version = v
	}

	if schema, err := t.Schema(ctx); err == nil {
		for _, f := range schema.Fields() {
			columns = append(columns, ColumnInfo{Name: f.Name, Type: f.Type.String()})
		}
	}

	if idxList, err := t.GetAllIndexes(ctx); err == nil {
		for _, idx := range idxList {
			indexes = append(indexes, LanceIndexInfo{
				Name:      idx.Name,
				Columns:   idx.Columns,
				IndexType: idx.IndexType,
			})
		}
	}

	return
}

// ── table accessors (lazy open) ──────────────────────────────────────────────

func (w *lanceWriter) openTable(name string) (contracts.ITable, error) {
	t, err := w.conn.OpenTable(context.Background(), name)
	if err != nil {
		return nil, fmt.Errorf("open lance table %q: %w", name, err)
	}
	debug.Log(debug.Lance, "opened lance table %q for writes", name)
	return t, nil
}

func (w *lanceWriter) getArticles() (contracts.ITable, error) {
	if w.articles != nil {
		return w.articles, nil
	}
	t, err := w.openTable("articles")
	if err != nil {
		return nil, err
	}
	w.articles = t
	return t, nil
}

func (w *lanceWriter) getFeeds() (contracts.ITable, error) {
	if w.feeds != nil {
		return w.feeds, nil
	}
	t, err := w.openTable("feeds")
	if err != nil {
		return nil, err
	}
	w.feeds = t
	return t, nil
}

func (w *lanceWriter) getCategories() (contracts.ITable, error) {
	if w.categories != nil {
		return w.categories, nil
	}
	t, err := w.openTable("categories")
	if err != nil {
		return nil, err
	}
	w.categories = t
	return t, nil
}

func (w *lanceWriter) getPendingFeeds() (contracts.ITable, error) {
	if w.pendingFeeds != nil {
		return w.pendingFeeds, nil
	}
	t, err := w.openTable("pending_feeds")
	if err != nil {
		return nil, err
	}
	w.pendingFeeds = t
	return t, nil
}

func (w *lanceWriter) getSettings() (contracts.ITable, error) {
	if w.settings != nil {
		return w.settings, nil
	}
	t, err := w.openTable("settings")
	if err != nil {
		return nil, err
	}
	w.settings = t
	return t, nil
}

// PutSetting updates the value column for an existing settings key.
func (w *lanceWriter) PutSetting(key, value string) error {
	t, err := w.getSettings()
	if err != nil {
		return err
	}
	filter := fmt.Sprintf("key = '%s'", escapeSQ(key))
	debug.Log(debug.Lance, "UPDATE settings WHERE %s SET value=%q", filter, value)
	return t.Update(context.Background(), filter, map[string]interface{}{
		"value":      value,
		"updated_at": time.Now().UTC(),
	})
}

// PutSettingsBatch groups settings by value and does one UPDATE per unique
// value using a key IN (...) filter. Much faster than updating one-by-one.
func (w *lanceWriter) PutSettingsBatch(settings map[string]string) error {
	t, err := w.getSettings()
	if err != nil {
		return err
	}
	ctx := context.Background()
	now := time.Now().UTC()

	// Group keys by value so we do one UPDATE per unique value.
	groups := make(map[string][]string) // value → list of keys
	for k, v := range settings {
		groups[v] = append(groups[v], k)
	}

	for val, keys := range groups {
		var filter string
		if len(keys) == 1 {
			filter = fmt.Sprintf("key = '%s'", escapeSQ(keys[0]))
		} else {
			quoted := make([]string, len(keys))
			for i, k := range keys {
				quoted[i] = "'" + escapeSQ(k) + "'"
			}
			filter = fmt.Sprintf("key IN (%s)", joinStrings(quoted, ", "))
		}
		debug.Log(debug.Lance, "batch UPDATE settings (%d keys) SET value=%q", len(keys), val)
		if err := t.Update(ctx, filter, map[string]interface{}{
			"value":      val,
			"updated_at": now,
		}); err != nil {
			return fmt.Errorf("batch update settings (%d keys): %w", len(keys), err)
		}
	}
	return nil
}

// ── Article CUD ──────────────────────────────────────────────────────────────

// UpdateArticle sets arbitrary columns on a single article.
func (w *lanceWriter) UpdateArticle(articleID string, updates map[string]interface{}) error {
	t, err := w.getArticles()
	if err != nil {
		return err
	}
	updates["updated_at"] = time.Now().UTC()
	filter := fmt.Sprintf("article_id = '%s'", escapeSQ(articleID))
	debug.Log(debug.Lance, "UPDATE articles WHERE %s SET %v", filter, updates)
	return t.Update(context.Background(), filter, updates)
}

// SetArticleRead sets is_read on one article.
func (w *lanceWriter) SetArticleRead(articleID string, isRead bool) error {
	return w.UpdateArticle(articleID, map[string]interface{}{
		"is_read": isRead,
	})
}

// SetArticleStarred sets is_starred on one article.
func (w *lanceWriter) SetArticleStarred(articleID string, isStarred bool) error {
	return w.UpdateArticle(articleID, map[string]interface{}{
		"is_starred": isStarred,
	})
}

// MarkAllRead sets is_read=true for all unread articles in a feed.
func (w *lanceWriter) MarkAllRead(feedID string) error {
	t, err := w.getArticles()
	if err != nil {
		return err
	}
	filter := fmt.Sprintf("feed_id = '%s' AND is_read = false", escapeSQ(feedID))
	debug.Log(debug.Lance, "UPDATE articles SET is_read=true WHERE %s", filter)
	return t.Update(context.Background(), filter, map[string]interface{}{
		"is_read":    true,
		"updated_at": time.Now().UTC(),
	})
}

// FlushOverrides merges a batch of pending article overrides into Lance.
// This replaces the old DuckDB-based UPDATE ... WHERE article_id IN (...).
func (w *lanceWriter) FlushOverrides(overrides map[string]*articleOverride) error {
	t, err := w.getArticles()
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Group by identical update payload so we can batch.
	type updateKey struct {
		hasRead    bool
		readVal    bool
		hasStar    bool
		starredVal bool
	}
	groups := make(map[updateKey][]string) // key → article IDs
	for id, ov := range overrides {
		k := updateKey{}
		if ov.IsRead != nil {
			k.hasRead = true
			k.readVal = *ov.IsRead
		}
		if ov.IsStarred != nil {
			k.hasStar = true
			k.starredVal = *ov.IsStarred
		}
		groups[k] = append(groups[k], id)
	}

	for k, ids := range groups {
		updates := make(map[string]interface{})
		if k.hasRead {
			updates["is_read"] = k.readVal
		}
		if k.hasStar {
			updates["is_starred"] = k.starredVal
		}
		if len(updates) == 0 {
			continue
		}
		updates["updated_at"] = time.Now().UTC()

		// Build an OR chain of article_id = '...' for this group.
		// LanceDB filter expressions use SQL-like syntax.
		filter := buildIDFilter(ids)
		debug.Log(debug.Lance, "batch UPDATE articles (%d ids) SET %v", len(ids), updates)
		if err := t.Update(ctx, filter, updates); err != nil {
			return fmt.Errorf("batch update (%d articles): %w", len(ids), err)
		}
	}
	return nil
}

// buildIDFilter builds "article_id IN ('a','b','c')" for a slice of IDs.
func buildIDFilter(ids []string) string {
	if len(ids) == 1 {
		return fmt.Sprintf("article_id = '%s'", escapeSQ(ids[0]))
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "'" + escapeSQ(id) + "'"
	}
	return fmt.Sprintf("article_id IN (%s)", joinStrings(quoted, ", "))
}

// joinStrings joins without importing strings (already imported elsewhere in package).
func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	n := len(sep) * (len(ss) - 1)
	for _, s := range ss {
		n += len(s)
	}
	buf := make([]byte, 0, n)
	for i, s := range ss {
		if i > 0 {
			buf = append(buf, sep...)
		}
		buf = append(buf, s...)
	}
	return string(buf)
}

// ── Feed CUD ─────────────────────────────────────────────────────────────────

// DeletePendingFeed removes a URL from the pending_feeds table.
func (w *lanceWriter) DeletePendingFeed(url string) error {
	t, err := w.getPendingFeeds()
	if err != nil {
		return err
	}
	filter := fmt.Sprintf("url = '%s'", escapeSQ(url))
	debug.Log(debug.Lance, "DELETE pending_feeds WHERE %s", filter)
	return t.Delete(context.Background(), filter)
}

// Arrow schema for the pending_feeds table.
var pendingFeedsSchema = arrow.NewSchema([]arrow.Field{
	{Name: "url", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "category_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "requested_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: false},
	{Name: "created_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: false},
	{Name: "updated_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: false},
}, nil)

// InsertPendingFeed adds a URL to the pending_feeds table via native SDK.
func (w *lanceWriter) InsertPendingFeed(url, categoryID string) error {
	t, err := w.getPendingFeeds()
	if err != nil {
		return err
	}

	pool := memory.NewGoAllocator()
	now := time.Now().UTC()
	nowMicro := arrow.Timestamp(now.UnixMicro())

	urlB := array.NewStringBuilder(pool)
	defer urlB.Release()
	catB := array.NewStringBuilder(pool)
	defer catB.Release()
	reqB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer reqB.Release()
	creB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer creB.Release()
	updB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer updB.Release()

	urlB.Append(url)
	catB.Append(categoryID)
	reqB.Append(nowMicro)
	creB.Append(nowMicro)
	updB.Append(nowMicro)

	urlArr := urlB.NewArray()
	defer urlArr.Release()
	catArr := catB.NewArray()
	defer catArr.Release()
	reqArr := reqB.NewArray()
	defer reqArr.Release()
	creArr := creB.NewArray()
	defer creArr.Release()
	updArr := updB.NewArray()
	defer updArr.Release()

	cols := []arrow.Array{urlArr, catArr, reqArr, creArr, updArr}
	record := array.NewRecord(pendingFeedsSchema, cols, 1)
	defer record.Release()

	debug.Log(debug.Lance, "INSERT pending_feed url=%s via native SDK", url)
	return t.Add(context.Background(), record, nil)
}

// Arrow schema for the settings table.
var settingsSchema = arrow.NewSchema([]arrow.Field{
	{Name: "key", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "value", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "created_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: false},
	{Name: "updated_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: false},
}, nil)

// InsertSetting adds a new setting row via the native SDK.
func (w *lanceWriter) InsertSetting(key, value string) error {
	t, err := w.getSettings()
	if err != nil {
		return err
	}

	pool := memory.NewGoAllocator()
	now := time.Now().UTC()
	nowMicro := arrow.Timestamp(now.UnixMicro())

	keyB := array.NewStringBuilder(pool)
	defer keyB.Release()
	valB := array.NewStringBuilder(pool)
	defer valB.Release()
	creB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer creB.Release()
	updB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer updB.Release()

	keyB.Append(key)
	valB.Append(value)
	creB.Append(nowMicro)
	updB.Append(nowMicro)

	keyArr := keyB.NewArray()
	defer keyArr.Release()
	valArr := valB.NewArray()
	defer valArr.Release()
	creArr := creB.NewArray()
	defer creArr.Release()
	updArr := updB.NewArray()
	defer updArr.Release()

	cols := []arrow.Array{keyArr, valArr, creArr, updArr}
	record := array.NewRecord(settingsSchema, cols, 1)
	defer record.Release()

	debug.Log(debug.Lance, "INSERT setting key=%s via native SDK", key)
	return t.Add(context.Background(), record, nil)
}

// InsertSettings adds multiple new setting rows in one batch via native SDK.
func (w *lanceWriter) InsertSettings(settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	t, err := w.getSettings()
	if err != nil {
		return err
	}

	n := len(settings)
	pool := memory.NewGoAllocator()
	now := time.Now().UTC()
	nowMicro := arrow.Timestamp(now.UnixMicro())

	keyB := array.NewStringBuilder(pool)
	defer keyB.Release()
	valB := array.NewStringBuilder(pool)
	defer valB.Release()
	creB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer creB.Release()
	updB := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer updB.Release()

	for k, v := range settings {
		keyB.Append(k)
		valB.Append(v)
		creB.Append(nowMicro)
		updB.Append(nowMicro)
	}

	keyArr := keyB.NewArray()
	defer keyArr.Release()
	valArr := valB.NewArray()
	defer valArr.Release()
	creArr := creB.NewArray()
	defer creArr.Release()
	updArr := updB.NewArray()
	defer updArr.Release()

	cols := []arrow.Array{keyArr, valArr, creArr, updArr}
	record := array.NewRecord(settingsSchema, cols, int64(n))
	defer record.Release()

	debug.Log(debug.Lance, "INSERT %d settings via native SDK", n)
	return t.Add(context.Background(), record, nil)
}

// ── Log CUD ──────────────────────────────────────────────────────────────────

func (w *lanceWriter) getLogAPI() (contracts.ITable, error) {
	if w.logAPI != nil {
		return w.logAPI, nil
	}
	t, err := w.openTable("log_api")
	if err != nil {
		return nil, err
	}
	w.logAPI = t
	return t, nil
}

// DeleteOldLogs removes log entries matching the given filter expression.
func (w *lanceWriter) DeleteOldLogs(filter string) error {
	t, err := w.getLogAPI()
	if err != nil {
		return err
	}
	debug.Log(debug.Lance, "DELETE log_api WHERE %s", filter)
	return t.Delete(context.Background(), filter)
}

// logAPITableExists checks if the log_api Lance table directory exists on disk.
func (w *lanceWriter) logAPITableExists() bool {
	dir := filepath.Join(w.dataPath, "log_api.lance")
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// Arrow schema for the log_api table (matches fetcher's LOG_SCHEMA).
var logAPISchema = arrow.NewSchema([]arrow.Field{
	{Name: "log_id", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "timestamp", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: true},
	{Name: "level", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "category", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "message", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "details", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "created_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: false},
}, nil)

// InsertLogs writes a batch of log entries to the log_api table using the
// native lancedb-go SDK. This replaces the old DuckDB INSERT path.
func (w *lanceWriter) InsertLogs(entries []LogEntry) error {
	if !w.logAPITableExists() {
		return nil // table not created yet (fetcher creates it on first run)
	}
	if len(entries) == 0 {
		return nil
	}

	t, err := w.getLogAPI()
	if err != nil {
		return err
	}

	n := len(entries)
	pool := memory.NewGoAllocator()

	logIDBuilder := array.NewStringBuilder(pool)
	defer logIDBuilder.Release()
	tsBuilder := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer tsBuilder.Release()
	levelBuilder := array.NewStringBuilder(pool)
	defer levelBuilder.Release()
	categoryBuilder := array.NewStringBuilder(pool)
	defer categoryBuilder.Release()
	messageBuilder := array.NewStringBuilder(pool)
	defer messageBuilder.Release()
	detailsBuilder := array.NewStringBuilder(pool)
	defer detailsBuilder.Release()
	createdAtBuilder := array.NewTimestampBuilder(pool, &arrow.TimestampType{Unit: arrow.Microsecond})
	defer createdAtBuilder.Release()

	now := time.Now().UTC()
	nowMicro := arrow.Timestamp(now.UnixMicro())

	for _, e := range entries {
		logIDBuilder.Append(e.LogID)
		if e.Timestamp != nil {
			tsBuilder.Append(arrow.Timestamp(e.Timestamp.UnixMicro()))
		} else {
			tsBuilder.Append(nowMicro)
		}
		levelBuilder.Append(e.Level)
		categoryBuilder.Append(e.Category)
		messageBuilder.Append(e.Message)
		detailsBuilder.Append(e.Details)
		createdAtBuilder.Append(nowMicro)
	}

	logIDArr := logIDBuilder.NewArray()
	defer logIDArr.Release()
	tsArr := tsBuilder.NewArray()
	defer tsArr.Release()
	levelArr := levelBuilder.NewArray()
	defer levelArr.Release()
	categoryArr := categoryBuilder.NewArray()
	defer categoryArr.Release()
	messageArr := messageBuilder.NewArray()
	defer messageArr.Release()
	detailsArr := detailsBuilder.NewArray()
	defer detailsArr.Release()
	createdAtArr := createdAtBuilder.NewArray()
	defer createdAtArr.Release()

	columns := []arrow.Array{logIDArr, tsArr, levelArr, categoryArr, messageArr, detailsArr, createdAtArr}
	record := array.NewRecord(logAPISchema, columns, int64(n))
	defer record.Release()

	debug.Log(debug.Lance, "INSERT %d log entries into log_api via native SDK", n)
	return t.Add(context.Background(), record, nil)
}
