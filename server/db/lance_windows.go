//go:build windows

// Windows implementation of Store using an external duckdb.exe CLI process.
// No CGo or GCC required - queries are sent via exec.Command and results
// parsed from DuckDB's JSON output mode.
//
// IMPORTANT: DuckDB is used ONLY as a query/write engine against Lance files.
// A local server.duckdb file caches extension installs; it can be safely
// deleted and will be recreated on next startup.  ALL persistent state lives
// in the Lance tables so the user can connect from any machine.
package db

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rss-lance/server/debug"
)

// cliStore shells out to duckdb.exe for every query (reads only).
// A mutex serialises access to avoid concurrent CLI conflicts.
// A local server.duckdb file persists extension installs; the data
// directory is ATTACHed as a Lance namespace each session.
//
// All CUD (Create/Update/Delete) operations go through the embedded
// lanceWriter which uses the lancedb-go native SDK.
type cliStore struct {
	mu        sync.Mutex
	dataPath  string
	dbPath    string // local DuckDB database file (cache; safe to delete)
	duckdbBin string
	cache     *writeCache
	writer    *lanceWriter // native lancedb-go for CUD ops
	logBuf    *logBuffer   // buffered log writes via native SDK
}

// findDuckDB locates the duckdb.exe binary.
// Search order: next to server binary → tools/ subdir → PATH.
func findDuckDB() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for _, rel := range []string{"duckdb.exe", "tools\\duckdb.exe"} {
			candidate := filepath.Join(dir, rel)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	// Also check relative to CWD
	for _, rel := range []string{"tools\\duckdb.exe", "duckdb.exe"} {
		if _, err := os.Stat(rel); err == nil {
			abs, _ := filepath.Abs(rel)
			return abs, nil
		}
	}
	if p, err := exec.LookPath("duckdb"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf(
		"duckdb.exe not found - place it next to the server binary, in tools/, or add to PATH.\n" +
			"Download from https://github.com/duckdb/duckdb/releases")
}

// Open creates a cliStore backed by an external duckdb.exe process.
// A local server.duckdb file is created in the data directory if it
// doesn't exist; delete it to force a clean rebuild on next startup.
func Open(dataPath string) (Store, error) {
	// For cloud URIs, DuckDB's cache file goes next to the executable;
	// for local paths, ensure the data directory exists.
	var dbPath string
	if isCloudURI(dataPath) {
		exeDir, _ := os.Executable()
		dbPath = filepath.Join(filepath.Dir(exeDir), "server.duckdb")
	} else {
		if err := os.MkdirAll(dataPath, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
		dbPath = filepath.Join(dataPath, "server.duckdb")
	}

	duckdbBin, err := findDuckDB()
	if err != nil {
		return nil, err
	}
	log.Printf("Using DuckDB CLI: %s", duckdbBin)

	log.Printf("DuckDB database: %s", dbPath)

	s := &cliStore{
		dataPath:  dataPath,
		dbPath:    dbPath,
		duckdbBin: duckdbBin,
	}

	if err := s.bootstrap(); err != nil {
		return nil, err
	}

	// Open native lancedb-go writer for all CUD operations
	w, err := newLanceWriter(dataPath)
	if err != nil {
		return nil, fmt.Errorf("lance writer: %w", err)
	}
	s.writer = w

	// Read cache/buffer settings from the settings table (falls back to
	// compiled defaults when the table doesn't exist yet — first boot).
	settings := make(map[string]string)
	if s.logTableExists("settings") {
		if all, err := s.GetSettings(); err == nil {
			settings = all
		}
	}

	// Set up write cache - reads JOIN pending overrides via CTE;
	// periodic flush writes through the native lancedb-go SDK.
	s.cache = newWriteCache(loadCacheConfig(settings), func(overrides map[string]*articleOverride) error {
		return s.writer.FlushOverrides(overrides)
	})
	s.cache.logFn = func(entry LogEntry) { s.WriteLog(entry) }

	// Set up buffered log writer - batches log entries and flushes
	// via the native lancedb-go SDK (no DuckDB for log inserts).
	s.logBuf = newLogBuffer(loadLogBufferConfig(settings), func(entries []LogEntry) error {
		return s.writer.InsertLogs(entries)
	})

	return s, nil
}

func (s *cliStore) bootstrap() error {
	// Install and load the Lance extension, then ATTACH data dir as Lance namespace.
	// For S3 paths, also install httpfs so DuckDB can read from S3.
	var extra string
	if isCloudURI(s.dataPath) {
		extra = "INSTALL httpfs;\nLOAD httpfs;\nSET s3_url_style = 'path';\n"
	}
	stmts := fmt.Sprintf("INSTALL lance FROM community;\nLOAD lance;\n%sATTACH IF NOT EXISTS '%s' AS _lance (TYPE LANCE);",
		extra, filepath.ToSlash(s.dataPath))
	if err := s.execSQL(stmts); err != nil {
		log.Printf("bootstrap warning: %v", err)
	}
	return nil
}

func (s *cliStore) Close() error {
	if s.logBuf != nil {
		s.logBuf.close()
	}
	if s.cache != nil {
		s.cache.close()
	}
	if s.writer != nil {
		return s.writer.close()
	}
	return nil
}

func (s *cliStore) CacheStats() (pendingReads, pendingStars int) {
	if s.cache != nil {
		return s.cache.Stats()
	}
	return 0, 0
}

// ── low-level CLI helpers ─────────────────────────────────────────────────────

// execSQL runs SQL that does not return rows.
// Uses the local server.duckdb database file.
func (s *cliStore) execSQL(sql string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	debug.Log(debug.DuckDB, "EXEC >>>\n%s", sql)

	cmd := exec.Command(s.duckdbBin, s.dbPath, "-c", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		debug.Log(debug.DuckDB, "EXEC ERR: %v | output: %s", err, truncate(string(out), 500))
		return fmt.Errorf("duckdb exec: %w\nOutput: %s", err, truncate(string(out), 500))
	}

	debug.Log(debug.DuckDB, "EXEC OK")
	return nil
}

// queryJSON runs a SELECT and returns the parsed JSON rows.
func (s *cliStore) queryJSON(sql string) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	debug.Log(debug.DuckDB, "QUERY >>>\n%s", sql)

	cmd := exec.Command(s.duckdbBin, s.dbPath, "-json", "-c", sql)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		debug.Log(debug.DuckDB, "QUERY ERR: %v | stderr: %s", err, truncate(stderr, 500))
		return nil, fmt.Errorf("duckdb query: %w\nStderr: %s", err, truncate(stderr, 500))
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		debug.Log(debug.DuckDB, "QUERY OK: 0 rows")
		return nil, nil
	}

	// DuckDB's Lance extension sometimes emits malformed JSON like "[{]"
	// for queries that return zero rows.  Treat as empty result.
	if trimmed == "[{]" {
		debug.Log(debug.DuckDB, "QUERY OK: 0 rows (lance empty-result quirk)")
		return nil, nil
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		debug.Log(debug.DuckDB, "QUERY JSON PARSE ERR: %v", err)
		return nil, fmt.Errorf("parse duckdb json: %w\nOutput: %s", err, truncate(trimmed, 500))
	}

	debug.Log(debug.DuckDB, "QUERY OK: %d rows", len(rows))
	return rows, nil
}

// lancePreamble returns SQL that loads the Lance extension and attaches
// the data directory as a Lance namespace.  Each CLI invocation is a fresh
// session, so this must prefix every lance query/exec.
func (s *cliStore) lancePreamble() string {
	return fmt.Sprintf("LOAD lance;\nATTACH IF NOT EXISTS '%s' AS _lance (TYPE LANCE);\n",
		filepath.ToSlash(s.dataPath))
}

// lanceQuery runs a SELECT that queries lance tables.
func (s *cliStore) lanceQuery(sql string) ([]map[string]any, error) {
	return s.queryJSON(s.lancePreamble() + sql)
}

// lanceExec runs an exec statement that writes to lance tables.
func (s *cliStore) lanceExec(sql string) error {
	return s.execSQL(s.lancePreamble() + sql)
}

// lanceTable returns the DuckDB namespace reference for a lance table,
// e.g. "_lance.main.articles".
func (s *cliStore) lanceTable(table string) string {
	return "_lance.main." + table
}

// ── JSON row helpers ──────────────────────────────────────────────────────────

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999-07",
	"2006-01-02 15:04:05.999999+00",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05+00",
	"2006-01-02 15:04:05-07",
	"2006-01-02 15:04:05",
}

func rowStr(row map[string]any, key string) string {
	v := row[key]
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func rowInt(row map[string]any, key string) int {
	v := row[key]
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int(val)
	case json.Number:
		n, _ := val.Int64()
		return int(n)
	case string:
		// DuckDB JSON mode sometimes returns integers as strings
		// (e.g. when SUM result involves a cast expression).
		var n int
		if _, err := fmt.Sscanf(val, "%d", &n); err == nil {
			return n
		}
		return 0
	default:
		return 0
	}
}

func rowBool(row map[string]any, key string) bool {
	v := row[key]
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	// DuckDB Lance extension returns booleans as strings in JSON mode
	if s, ok := v.(string); ok {
		return s == "true"
	}
	return false
}

func rowTime(row map[string]any, key string) *time.Time {
	v := row[key]
	if v == nil {
		return nil
	}
	s := fmt.Sprint(v)
	if s == "" || s == "<nil>" {
		return nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── Feed queries ──────────────────────────────────────────────────────────────

func (s *cliStore) GetFeeds() ([]Feed, error) {
	feedTbl := s.lanceTable("feeds")
	artTbl := s.lanceTable("articles")

	cte, hasCTE := s.cache.pendingCTE()
	unreadExpr := "a.is_read"
	cacheJoin := ""
	if hasCTE {
		cacheJoin = "LEFT JOIN _cache c ON a.article_id = c.article_id"
		unreadExpr = "COALESCE(c.is_read, a.is_read)"
	}

	q := fmt.Sprintf(`%s
		SELECT
			f.feed_id, f.title, f.url, f.site_url, f.icon_url,
			f.category_id, f.subcategory_id,
			f.last_fetched, f.last_article_date,
			f.fetch_interval_mins, f.fetch_tier,
			f.error_count, f.last_error,
			COUNT(CASE WHEN %s = FALSE THEN 1 END) AS unread_count
		FROM %s f
		LEFT JOIN %s a ON a.feed_id = f.feed_id
		%s
		GROUP BY
			f.feed_id, f.title, f.url, f.site_url, f.icon_url,
			f.category_id, f.subcategory_id,
			f.last_fetched, f.last_article_date,
			f.fetch_interval_mins, f.fetch_tier,
			f.error_count, f.last_error
		ORDER BY LOWER(f.title)
	`, cte, unreadExpr, feedTbl, artTbl, cacheJoin)

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, err
	}
	feeds := make([]Feed, 0, len(rows))
	for _, r := range rows {
		feeds = append(feeds, rowToFeed(r))
	}
	return feeds, nil
}

func (s *cliStore) GetFeed(feedID string) (*Feed, error) {
	tbl := s.lanceTable("feeds")
	q := fmt.Sprintf(`
		SELECT feed_id, title, url, site_url, icon_url,
		       category_id, subcategory_id,
		       last_fetched, last_article_date,
		       fetch_interval_mins, fetch_tier,
		       error_count, last_error
		FROM %s
		WHERE feed_id = '%s'
		LIMIT 1
	`, tbl, escapeSQ(feedID))

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	f := rowToFeed(rows[0])
	return &f, nil
}

func (s *cliStore) QueueFeed(url, categoryID string) error {
	return s.writer.InsertPendingFeed(url, categoryID)
}

func (s *cliStore) GetPendingFeeds() ([]string, error) {
	tbl := s.lanceTable("pending_feeds")
	rows, err := s.lanceQuery(fmt.Sprintf(
		`SELECT url FROM %s ORDER BY requested_at`, tbl))
	if err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(rows))
	for _, r := range rows {
		urls = append(urls, rowStr(r, "url"))
	}
	return urls, nil
}

// ── Article queries ───────────────────────────────────────────────────────────

func (s *cliStore) GetArticles(feedID string, limit, offset int, unreadOnly bool, sortAsc bool) ([]Article, error) {
	artTbl := s.lanceTable("articles")

	cte, hasCTE := s.cache.pendingCTE()

	// Column expressions: overlay cache when pending changes exist
	isReadExpr := "a.is_read"
	isStarredExpr := "a.is_starred"
	cacheJoin := ""
	if hasCTE {
		isReadExpr = "COALESCE(c.is_read, a.is_read)"
		isStarredExpr = "COALESCE(c.is_starred, a.is_starred)"
		cacheJoin = "LEFT JOIN _cache c ON a.article_id = c.article_id"
	}

	feedFilter := ""
	if feedID != "" {
		feedFilter = fmt.Sprintf("AND a.feed_id = '%s'", escapeSQ(feedID))
	}
	unreadFilter := ""
	if unreadOnly {
		unreadFilter = fmt.Sprintf("AND %s = FALSE", isReadExpr)
	}

	sortDir := "DESC"
	if sortAsc {
		sortDir = "ASC"
	}

	q := fmt.Sprintf(`%s
		SELECT
			a.article_id, a.feed_id, a.title, a.url, a.author,
			a.summary, a.published_at, a.fetched_at,
			%s AS is_read, %s AS is_starred
		FROM %s a
		%s
		WHERE 1=1 %s %s
		ORDER BY a.published_at %s
		LIMIT %d OFFSET %d
	`, cte, isReadExpr, isStarredExpr, artTbl, cacheJoin,
		feedFilter, unreadFilter, sortDir, limit, offset)

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, err
	}
	arts := make([]Article, 0, len(rows))
	for _, r := range rows {
		arts = append(arts, rowToArticle(r))
	}
	return arts, nil
}

func (s *cliStore) GetArticle(articleID string) (*Article, error) {
	artTbl := s.lanceTable("articles")

	cte, hasCTE := s.cache.pendingCTE()
	isReadExpr := "a.is_read"
	isStarredExpr := "a.is_starred"
	cacheJoin := ""
	if hasCTE {
		isReadExpr = "COALESCE(c.is_read, a.is_read)"
		isStarredExpr = "COALESCE(c.is_starred, a.is_starred)"
		cacheJoin = "LEFT JOIN _cache c ON a.article_id = c.article_id"
	}

	q := fmt.Sprintf(`%s
		SELECT
			a.article_id, a.feed_id, a.title, a.url, a.author,
			a.content, a.summary, a.published_at, a.fetched_at,
			%s AS is_read, %s AS is_starred
		FROM %s a
		%s
		WHERE a.article_id = '%s'
		LIMIT 1
	`, cte, isReadExpr, isStarredExpr, artTbl, cacheJoin, escapeSQ(articleID))

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	a := rowToArticleWithContent(rows[0])
	return &a, nil
}

func (s *cliStore) GetArticleBatch(articleIDs []string) ([]Article, error) {
	if len(articleIDs) == 0 {
		return nil, nil
	}
	artTbl := s.lanceTable("articles")

	cte, hasCTE := s.cache.pendingCTE()
	isReadExpr := "a.is_read"
	isStarredExpr := "a.is_starred"
	cacheJoin := ""
	if hasCTE {
		isReadExpr = "COALESCE(c.is_read, a.is_read)"
		isStarredExpr = "COALESCE(c.is_starred, a.is_starred)"
		cacheJoin = "LEFT JOIN _cache c ON a.article_id = c.article_id"
	}

	// Build IN clause
	quoted := make([]string, len(articleIDs))
	for i, id := range articleIDs {
		quoted[i] = "'" + escapeSQ(id) + "'"
	}
	inClause := strings.Join(quoted, ", ")

	q := fmt.Sprintf(`%s
		SELECT
			a.article_id, a.feed_id, a.title, a.url, a.author,
			a.content, a.summary, a.published_at, a.fetched_at,
			%s AS is_read, %s AS is_starred
		FROM %s a
		%s
		WHERE a.article_id IN (%s)
	`, cte, isReadExpr, isStarredExpr, artTbl, cacheJoin, inClause)

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, err
	}
	arts := make([]Article, 0, len(rows))
	for _, r := range rows {
		arts = append(arts, rowToArticleWithContent(r))
	}
	return arts, nil
}

func (s *cliStore) SetArticleRead(articleID string, isRead bool) error {
	// Record in cache - reads will overlay this via CTE JOIN.
	// Periodic flush will MERGE into Lance.
	s.cache.setRead(articleID, isRead)
	return nil
}

func (s *cliStore) SetArticleStarred(articleID string, isStarred bool) error {
	s.cache.setStarred(articleID, isStarred)
	return nil
}

func (s *cliStore) MarkAllRead(feedID string) error {
	// Flush any pending cached writes first so the native UPDATE sees clean state
	if err := s.cache.flush(); err != nil {
		return err
	}
	return s.writer.MarkAllRead(feedID)
}

// ── Category queries ──────────────────────────────────────────────────────────

func (s *cliStore) GetCategories() ([]Category, error) {
	tbl := s.lanceTable("categories")
	q := fmt.Sprintf(`
		SELECT category_id, name, parent_id, sort_order, created_at, updated_at
		FROM %s
		ORDER BY sort_order, name
	`, tbl)

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, err
	}
	cats := make([]Category, 0, len(rows))
	for _, r := range rows {
		cats = append(cats, Category{
			CategoryID: rowStr(r, "category_id"),
			Name:       rowStr(r, "name"),
			ParentID:   rowStr(r, "parent_id"),
			SortOrder:  rowInt(r, "sort_order"),
			CreatedAt:  rowTime(r, "created_at"),
			UpdatedAt:  rowTime(r, "updated_at"),
		})
	}
	return cats, nil
}

// ── row → struct converters ───────────────────────────────────────────────────

func rowToFeed(r map[string]any) Feed {
	return Feed{
		FeedID:            rowStr(r, "feed_id"),
		Title:             rowStr(r, "title"),
		URL:               rowStr(r, "url"),
		SiteURL:           rowStr(r, "site_url"),
		IconURL:           rowStr(r, "icon_url"),
		CategoryID:        rowStr(r, "category_id"),
		SubcategoryID:     rowStr(r, "subcategory_id"),
		LastFetched:       rowTime(r, "last_fetched"),
		LastArticleDate:   rowTime(r, "last_article_date"),
		FetchIntervalMins: rowInt(r, "fetch_interval_mins"),
		FetchTier:         rowStr(r, "fetch_tier"),
		ErrorCount:        rowInt(r, "error_count"),
		LastError:         rowStr(r, "last_error"),
		UnreadCount:       rowInt(r, "unread_count"),
		CreatedAt:         rowTime(r, "created_at"),
		UpdatedAt:         rowTime(r, "updated_at"),
	}
}

func rowToArticle(r map[string]any) Article {
	return Article{
		ArticleID:   rowStr(r, "article_id"),
		FeedID:      rowStr(r, "feed_id"),
		Title:       rowStr(r, "title"),
		URL:         rowStr(r, "url"),
		Author:      rowStr(r, "author"),
		Summary:     rowStr(r, "summary"),
		PublishedAt: rowTime(r, "published_at"),
		FetchedAt:   rowTime(r, "fetched_at"),
		IsRead:      rowBool(r, "is_read"),
		IsStarred:   rowBool(r, "is_starred"),
		CreatedAt:   rowTime(r, "created_at"),
		UpdatedAt:   rowTime(r, "updated_at"),
	}
}

func rowToArticleWithContent(r map[string]any) Article {
	a := rowToArticle(r)
	a.Content = rowStr(r, "content")
	return a
}

// ── Settings queries ──────────────────────────────────────────────────────────

func (s *cliStore) GetSettings() (map[string]string, error) {
	tbl := s.lanceTable("settings")
	q := fmt.Sprintf(`SELECT key, value FROM %s ORDER BY key`, tbl)

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, err
	}
	settings := make(map[string]string, len(rows))
	for _, r := range rows {
		settings[rowStr(r, "key")] = rowStr(r, "value")
	}
	return settings, nil
}

func (s *cliStore) GetSetting(key string) (string, bool, error) {
	tbl := s.lanceTable("settings")
	q := fmt.Sprintf(`SELECT value FROM %s WHERE key = '%s' LIMIT 1`, tbl, escapeSQ(key))

	rows, err := s.lanceQuery(q)
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rowStr(rows[0], "value"), true, nil
}

func (s *cliStore) PutSetting(key, value string) error {
	// Check if key exists; if not, INSERT instead of UPDATE
	_, found, err := s.GetSetting(key)
	if err != nil {
		return err
	}
	if found {
		return s.writer.PutSetting(key, value)
	}
	return s.writer.InsertSetting(key, value)
}

func (s *cliStore) PutSettings(settings map[string]string) error {
	// Get all existing keys in one query
	existing, err := s.GetSettings()
	if err != nil {
		return err
	}

	// Split into updates (existing keys) and inserts (new keys)
	updates := make(map[string]string)
	inserts := make(map[string]string)
	for k, v := range settings {
		if _, found := existing[k]; found {
			updates[k] = v
		} else {
			inserts[k] = v
		}
	}

	// Batch update existing keys via lance writer (grouped by value)
	if len(updates) > 0 {
		if err := s.writer.PutSettingsBatch(updates); err != nil {
			return err
		}
	}

	// Insert new keys via native SDK
	if len(inserts) > 0 {
		if err := s.writer.InsertSettings(inserts); err != nil {
			return err
		}
	}
	return nil
}

// ── Status / diagnostics ──────────────────────────────────────────────────────

func (s *cliStore) GetDBStatus() (*DBStatus, error) {
	status := &DBStatus{DataPath: s.dataPath}

	// Table names and their lance directories
	tables := []string{"articles", "feeds", "categories", "pending_feeds", "settings"}
	for _, lt := range []string{"log_api", "log_fetcher"} {
		if s.logTableExists(lt) {
			tables = append(tables, lt)
		}
	}

	for _, name := range tables {
		ts := TableStats{Name: name}

		// Row count
		q := fmt.Sprintf(`SELECT COUNT(*) AS cnt FROM %s`, s.lanceTable(name))
		rows, err := s.lanceQuery(q)
		if err == nil && len(rows) > 0 {
			ts.RowCount = rowInt(rows[0], "cnt")
		}

		// Directory size on disk
		dir := filepath.Join(s.dataPath, name+".lance")
		ts.SizeBytes = dirSize(dir)

		// Lance-native metadata
		ts.Version, ts.Columns, ts.Indexes = s.writer.getTableMeta(name)
		ts.NumColumns = len(ts.Columns)

		status.Tables = append(status.Tables, ts)
	}

	// Article aggregate stats (overlay write-cache like other queries)
	artTbl := s.lanceTable("articles")
	cte, hasCTE := s.cache.pendingCTE()
	isReadExpr := "a.is_read"
	isStarredExpr := "a.is_starred"
	cacheJoin := ""
	if hasCTE {
		cacheJoin = "LEFT JOIN _cache c ON a.article_id = c.article_id"
		isReadExpr = "COALESCE(c.is_read, a.is_read)"
		isStarredExpr = "COALESCE(c.is_starred, a.is_starred)"
	}
	q := fmt.Sprintf(`%s SELECT
		COUNT(*) AS total,
		SUM(CASE WHEN %s = false THEN 1 ELSE 0 END) AS unread,
		SUM(CASE WHEN %s = true THEN 1 ELSE 0 END) AS starred,
		MIN(a.published_at) AS oldest,
		MAX(a.published_at) AS newest
	FROM %s a
	%s`, cte, isReadExpr, isStarredExpr, artTbl, cacheJoin)
	rows, err := s.lanceQuery(q)
	if err == nil && len(rows) > 0 {
		status.Articles.Total = rowInt(rows[0], "total")
		status.Articles.Unread = rowInt(rows[0], "unread")
		status.Articles.Starred = rowInt(rows[0], "starred")
		status.Articles.Oldest = rowStr(rows[0], "oldest")
		status.Articles.Newest = rowStr(rows[0], "newest")
	}

	return status, nil
}

// ── Raw table viewer ──────────────────────────────────────────────────────────

func (s *cliStore) QueryTable(table string, limit, offset int) (*TableQueryResult, error) {
	if !allowedTables[table] {
		return nil, fmt.Errorf("unknown table: %s", table)
	}
	tbl := s.lanceTable(table)

	// Total row count
	cntRows, err := s.lanceQuery(fmt.Sprintf("SELECT COUNT(*) AS cnt FROM %s", tbl))
	if err != nil {
		return nil, err
	}
	total := 0
	if len(cntRows) > 0 {
		total = rowInt(cntRows[0], "cnt")
	}

	// Fetch page
	rows, err := s.lanceQuery(fmt.Sprintf("SELECT * FROM %s LIMIT %d OFFSET %d", tbl, limit, offset))
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []map[string]any{}
	}

	// Extract column names from first row or table metadata
	var cols []string
	if len(rows) > 0 {
		for k := range rows[0] {
			cols = append(cols, k)
		}
	} else {
		_, colInfos, _ := s.writer.getTableMeta(table)
		for _, c := range colInfos {
			cols = append(cols, c.Name)
		}
	}

	return &TableQueryResult{
		Table:   table,
		Columns: cols,
		Rows:    rows,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
	}, nil
}

// ── Logging queries ───────────────────────────────────────────────────────────

// logTableExists checks if a log lance table directory exists on disk.
func (s *cliStore) logTableExists(name string) bool {
	dir := filepath.Join(s.dataPath, name+".lance")
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func (s *cliStore) WriteLog(entry LogEntry) error {
	s.logBuf.add(entry)
	return nil
}

func (s *cliStore) GetLogs(opts LogQuery) ([]LogEntry, int, error) {
	// Flush buffered entries so they are visible to the read query
	if s.logBuf != nil {
		s.logBuf.flush()
	}

	if opts.Limit <= 0 {
		opts.Limit = 100
	}

	// Build UNION ALL across available log tables
	var sources []string

	if opts.Service == "" || opts.Service == "api" {
		if s.logTableExists("log_api") {
			sources = append(sources, fmt.Sprintf(
				`SELECT log_id, timestamp, level, category, 'api' AS service, message, details, created_at FROM %s`,
				s.lanceTable("log_api")))
		}
	}
	if opts.Service == "" || opts.Service == "fetcher" {
		if s.logTableExists("log_fetcher") {
			sources = append(sources, fmt.Sprintf(
				`SELECT log_id, timestamp, level, category, 'fetcher' AS service, message, details, created_at FROM %s`,
				s.lanceTable("log_fetcher")))
		}
	}

	if len(sources) == 0 {
		return nil, 0, nil
	}

	unionSQL := strings.Join(sources, " UNION ALL ")

	// Build WHERE clause
	var conditions []string
	if opts.Level != "" {
		conditions = append(conditions, fmt.Sprintf("level = '%s'", escapeSQ(opts.Level)))
	}
	if opts.Category != "" {
		conditions = append(conditions, fmt.Sprintf("category = '%s'", escapeSQ(opts.Category)))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// Get total count
	countQ := fmt.Sprintf(`SELECT COUNT(*) AS cnt FROM (%s) AS logs%s`, unionSQL, where)
	countRows, err := s.lanceQuery(countQ)
	total := 0
	if err == nil && len(countRows) > 0 {
		total = rowInt(countRows[0], "cnt")
	}

	// Get paginated results
	q := fmt.Sprintf(`SELECT * FROM (%s) AS logs%s ORDER BY timestamp DESC LIMIT %d OFFSET %d`,
		unionSQL, where, opts.Limit, opts.Offset)

	rows, err := s.lanceQuery(q)
	if err != nil {
		return nil, 0, err
	}

	entries := make([]LogEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, LogEntry{
			LogID:     rowStr(r, "log_id"),
			Timestamp: rowTime(r, "timestamp"),
			Level:     rowStr(r, "level"),
			Category:  rowStr(r, "category"),
			Service:   rowStr(r, "service"),
			Message:   rowStr(r, "message"),
			Details:   rowStr(r, "details"),
			CreatedAt: rowTime(r, "created_at"),
		})
	}
	return entries, total, nil
}

func (s *cliStore) TrimLogs(maxEntries int) error {
	if maxEntries <= 0 {
		return nil // 0 means retain all logs
	}
	if !s.logTableExists("log_api") {
		return nil
	}
	tbl := s.lanceTable("log_api")

	// Count rows
	q := fmt.Sprintf(`SELECT COUNT(*) AS cnt FROM %s`, tbl)
	rows, err := s.lanceQuery(q)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	count := rowInt(rows[0], "cnt")
	if count <= maxEntries {
		return nil
	}

	// Find cutoff timestamp
	cutoffQ := fmt.Sprintf(
		`SELECT timestamp FROM %s ORDER BY timestamp DESC LIMIT 1 OFFSET %d`,
		tbl, maxEntries-1)
	cutoffRows, err := s.lanceQuery(cutoffQ)
	if err != nil {
		return err
	}
	if len(cutoffRows) == 0 {
		return nil
	}
	cutoff := rowStr(cutoffRows[0], "timestamp")
	filter := fmt.Sprintf("timestamp < '%s'", escapeSQ(cutoff))
	return s.writer.DeleteOldLogs(filter)
}

func (s *cliStore) TrimLogsByAge(maxAgeDays int) error {
	if maxAgeDays <= 0 {
		return nil
	}
	if !s.logTableExists("log_api") {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -maxAgeDays).Format("2006-01-02T15:04:05.000000")
	filter := fmt.Sprintf("timestamp < '%s'", escapeSQ(cutoff))
	return s.writer.DeleteOldLogs(filter)
}
