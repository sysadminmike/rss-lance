//go:build !windows

// Linux/FreeBSD implementation of Store using embedded go-duckdb via CGo.
// Requires GCC at build time. Faster than the CLI driver - no subprocess overhead.
//
// IMPORTANT: DuckDB is used ONLY as a query/write engine against Lance files.
// A local server.duckdb file caches extension installs; it can be safely
// deleted and will be recreated on next startup.  ALL persistent state lives
// in the Lance tables so the user can connect from any machine.
package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rss-lance/server/debug"

	_ "github.com/marcboeker/go-duckdb"
)

// cgoStore wraps a DuckDB connection via the go-duckdb CGo driver (reads only).
// A local server.duckdb file persists extension installs; the data
// directory is ATTACHed as a Lance namespace each session.
//
// All CUD (Create/Update/Delete) operations go through the embedded
// lanceWriter which uses the lancedb-go native SDK.
type cgoStore struct {
	conn     *sql.DB
	dataPath string
	cache    *writeCache
	writer   *lanceWriter // native lancedb-go for CUD ops
	logBuf   *logBuffer   // buffered log writes via native SDK
}

// Open connects to DuckDB and loads the Lance extension.
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
	log.Printf("DuckDB database: %s", dbPath)

	conn, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}

	s := &cgoStore{conn: conn, dataPath: dataPath}
	if err := s.bootstrap(); err != nil {
		conn.Close()
		return nil, err
	}

	// Open native lancedb-go writer for all CUD operations
	w, err := newLanceWriter(dataPath)
	if err != nil {
		conn.Close()
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

func (s *cgoStore) bootstrap() error {
	// Install/load Lance extension and ATTACH data dir as Lance namespace.
	// For S3 paths, also install httpfs so DuckDB can read from S3.
	stmts := []string{
		`INSTALL lance FROM community`,
		`LOAD lance`,
	}
	if isCloudURI(s.dataPath) {
		stmts = append(stmts,
			`INSTALL httpfs`,
			`LOAD httpfs`,
			`SET s3_url_style = 'path'`,
		)
	}
	stmts = append(stmts,
		fmt.Sprintf(`ATTACH IF NOT EXISTS '%s' AS _lance (TYPE LANCE)`, filepath.ToSlash(s.dataPath)),
	)
	for _, stmt := range stmts {
		debug.Log(debug.Lance, "bootstrap: %s", stmt)
		if _, err := s.conn.Exec(stmt); err != nil {
			log.Printf("bootstrap warning (%s...): %v", stmt[:min(40, len(stmt))], err)
		}
	}
	return nil
}

func (s *cgoStore) Close() error {
	if s.logBuf != nil {
		s.logBuf.close()
	}
	if s.cache != nil {
		s.cache.close()
	}
	if s.writer != nil {
		s.writer.close()
	}
	return s.conn.Close()
}

func (s *cgoStore) CacheStats() (pendingReads, pendingStars int) {
	if s.cache != nil {
		return s.cache.Stats()
	}
	return 0, 0
}

// lanceTable returns the DuckDB namespace reference for a lance table,
// e.g. "_lance.main.articles".
func (s *cgoStore) lanceTable(table string) string {
	return "_lance.main." + table
}

// lanceExec runs a write statement against Lance tables.
func (s *cgoStore) lanceExec(sqlStmt string) error {
	debug.Log(debug.DuckDB, "EXEC >>>\n%s", sqlStmt)
	_, err := s.conn.Exec(sqlStmt)
	if err != nil {
		debug.Log(debug.DuckDB, "EXEC ERR: %v", err)
	} else {
		debug.Log(debug.DuckDB, "EXEC OK")
	}
	return err
}

// ── Feed queries ──────────────────────────────────────────────────────────────

func (s *cgoStore) GetFeeds() ([]Feed, error) {
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

	rows, err := s.conn.Query(q)
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
			&f.ErrorCount, &f.LastError,
			&f.UnreadCount,
		); err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

func (s *cgoStore) GetFeed(feedID string) (*Feed, error) {
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

	row := s.conn.QueryRow(q)
	var f Feed
	err := row.Scan(
		&f.FeedID, &f.Title, &f.URL, &f.SiteURL, &f.IconURL,
		&f.CategoryID, &f.SubcategoryID,
		&f.LastFetched, &f.LastArticleDate,
		&f.FetchIntervalMins, &f.FetchTier,
		&f.ErrorCount, &f.LastError,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &f, err
}

func (s *cgoStore) QueueFeed(url, categoryID string) error {
	return s.writer.InsertPendingFeed(url, categoryID)
}

func (s *cgoStore) GetPendingFeeds() ([]string, error) {
	tbl := s.lanceTable("pending_feeds")
	q := fmt.Sprintf(`SELECT url FROM %s ORDER BY requested_at`, tbl)

	rows, err := s.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, rows.Err()
}

// ── Article queries ───────────────────────────────────────────────────────────

func (s *cgoStore) GetArticles(feedID string, limit, offset int, unreadOnly bool, sortAsc bool) ([]Article, error) {
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

	unreadFilter := ""
	if unreadOnly {
		unreadFilter = fmt.Sprintf("AND %s = FALSE", isReadExpr)
	}
	feedFilter := ""
	if feedID != "" {
		feedFilter = fmt.Sprintf("AND a.feed_id = '%s'", escapeSQ(feedID))
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

	rows, err := s.conn.Query(q)
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
		); err != nil {
			return nil, err
		}
		arts = append(arts, a)
	}
	return arts, rows.Err()
}

func (s *cgoStore) GetArticle(articleID string) (*Article, error) {
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

	row := s.conn.QueryRow(q)
	var a Article
	err := row.Scan(
		&a.ArticleID, &a.FeedID, &a.Title, &a.URL, &a.Author,
		&a.Content, &a.Summary, &a.PublishedAt, &a.FetchedAt,
		&a.IsRead, &a.IsStarred,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (s *cgoStore) GetArticleBatch(articleIDs []string) ([]Article, error) {
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

	rows, err := s.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	arts := make([]Article, 0, len(articleIDs))
	for rows.Next() {
		var a Article
		if err := rows.Scan(
			&a.ArticleID, &a.FeedID, &a.Title, &a.URL, &a.Author,
			&a.Content, &a.Summary, &a.PublishedAt, &a.FetchedAt,
			&a.IsRead, &a.IsStarred,
		); err != nil {
			return nil, err
		}
		arts = append(arts, a)
	}
	return arts, rows.Err()
}

func (s *cgoStore) SetArticleRead(articleID string, isRead bool) error {
	// Record in cache - reads will overlay this via CTE JOIN.
	// Periodic flush will UPDATE into Lance.
	s.cache.setRead(articleID, isRead)
	return nil
}

func (s *cgoStore) SetArticleStarred(articleID string, isStarred bool) error {
	s.cache.setStarred(articleID, isStarred)
	return nil
}

func (s *cgoStore) MarkAllRead(feedID string) error {
	// Flush any pending cached writes first so the native UPDATE sees clean state
	if err := s.cache.flush(); err != nil {
		return err
	}
	return s.writer.MarkAllRead(feedID)
}

// ── Category queries ──────────────────────────────────────────────────────────

func (s *cgoStore) GetCategories() ([]Category, error) {
	tbl := s.lanceTable("categories")
	q := fmt.Sprintf(`
		SELECT category_id, name, parent_id, sort_order, created_at, updated_at
		FROM %s
		ORDER BY sort_order, name
	`, tbl)

	rows, err := s.conn.Query(q)
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

// ── Settings queries ──────────────────────────────────────────────────────────

func (s *cgoStore) GetSettings() (map[string]string, error) {
	tbl := s.lanceTable("settings")
	q := fmt.Sprintf(`SELECT key, value FROM %s ORDER BY key`, tbl)

	rows, err := s.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		settings[k] = v
	}
	return settings, rows.Err()
}

func (s *cgoStore) GetSetting(key string) (string, bool, error) {
	tbl := s.lanceTable("settings")
	q := fmt.Sprintf(`SELECT value FROM %s WHERE key = '%s' LIMIT 1`, tbl, escapeSQ(key))

	row := s.conn.QueryRow(q)
	var v string
	err := row.Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *cgoStore) PutSetting(key, value string) error {
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

func (s *cgoStore) PutSettings(settings map[string]string) error {
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

func (s *cgoStore) GetDBStatus() (*DBStatus, error) {
	status := &DBStatus{DataPath: s.dataPath}

	tables := []string{"articles", "feeds", "categories", "pending_feeds", "settings"}
	for _, lt := range []string{"log_api", "log_fetcher"} {
		if s.logTableExists(lt) {
			tables = append(tables, lt)
		}
	}

	for _, name := range tables {
		ts := TableStats{Name: name}

		q := fmt.Sprintf(`SELECT COUNT(*) AS cnt FROM %s`, s.lanceTable(name))
		row := s.conn.QueryRow(q)
		var cnt int
		if err := row.Scan(&cnt); err == nil {
			ts.RowCount = cnt
		}

		dir := filepath.Join(s.dataPath, name+".lance")
		ts.SizeBytes = dirSize(dir)

		// Lance-native metadata
		ts.Version, ts.Columns, ts.Indexes = s.writer.getTableMeta(name)
		ts.NumColumns = len(ts.Columns)

		status.Tables = append(status.Tables, ts)
	}

	artTbl := s.lanceTable("articles")
	q := fmt.Sprintf(`SELECT
		COUNT(*) AS total,
		SUM(CASE WHEN is_read = false THEN 1 ELSE 0 END) AS unread,
		SUM(CASE WHEN is_starred = true THEN 1 ELSE 0 END) AS starred,
		CAST(MIN(published_at) AS VARCHAR) AS oldest,
		CAST(MAX(published_at) AS VARCHAR) AS newest
	FROM %s`, artTbl)
	row := s.conn.QueryRow(q)
	var total, unread, starred int
	var oldest, newest sql.NullString
	if err := row.Scan(&total, &unread, &starred, &oldest, &newest); err == nil {
		status.Articles.Total = total
		status.Articles.Unread = unread
		status.Articles.Starred = starred
		if oldest.Valid {
			status.Articles.Oldest = oldest.String
		}
		if newest.Valid {
			status.Articles.Newest = newest.String
		}
	}

	return status, nil
}

// ── Raw table viewer ──────────────────────────────────────────────────────────

func (s *cgoStore) QueryTable(table string, limit, offset int) (*TableQueryResult, error) {
	if !allowedTables[table] {
		return nil, fmt.Errorf("unknown table: %s", table)
	}
	tbl := s.lanceTable(table)

	// Total row count
	var total int
	row := s.conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", tbl))
	if err := row.Scan(&total); err != nil {
		return nil, err
	}

	// Fetch page
	sqlRows, err := s.conn.Query(fmt.Sprintf("SELECT * FROM %s LIMIT %d OFFSET %d", tbl, limit, offset))
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()

	colNames, err := sqlRows.Columns()
	if err != nil {
		return nil, err
	}

	var result []map[string]any
	for sqlRows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := sqlRows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(colNames))
		for i, col := range colNames {
			m[col] = vals[i]
		}
		result = append(result, m)
	}
	if result == nil {
		result = []map[string]any{}
	}

	return &TableQueryResult{
		Table:   table,
		Columns: colNames,
		Rows:    result,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
	}, nil
}

// ── Logging queries ───────────────────────────────────────────────────────────

func (s *cgoStore) logTableExists(name string) bool {
	dir := filepath.Join(s.dataPath, name+".lance")
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func (s *cgoStore) WriteLog(entry LogEntry) error {
	s.logBuf.add(entry)
	return nil
}

func (s *cgoStore) GetLogs(opts LogQuery) ([]LogEntry, int, error) {
	// Flush buffered entries so they are visible to the read query
	if s.logBuf != nil {
		s.logBuf.flush()
	}

	if opts.Limit <= 0 {
		opts.Limit = 100
	}

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

	// Total count
	countQ := fmt.Sprintf(`SELECT COUNT(*) AS cnt FROM (%s) AS logs%s`, unionSQL, where)
	var total int
	if err := s.conn.QueryRow(countQ).Scan(&total); err != nil {
		total = 0
	}

	// Paginated results
	q := fmt.Sprintf(`SELECT * FROM (%s) AS logs%s ORDER BY timestamp DESC LIMIT %d OFFSET %d`,
		unionSQL, where, opts.Limit, opts.Offset)

	rows, err := s.conn.Query(q)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.LogID, &e.Timestamp, &e.Level, &e.Category, &e.Service, &e.Message, &e.Details, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

func (s *cgoStore) TrimLogs(maxEntries int) error {
	if maxEntries <= 0 {
		return nil // 0 means retain all logs
	}
	if !s.logTableExists("log_api") {
		return nil
	}
	tbl := s.lanceTable("log_api")

	var count int
	if err := s.conn.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)).Scan(&count); err != nil {
		return err
	}
	if count <= maxEntries {
		return nil
	}

	var cutoff string
	err := s.conn.QueryRow(fmt.Sprintf(
		`SELECT CAST(timestamp AS VARCHAR) FROM %s ORDER BY timestamp DESC LIMIT 1 OFFSET %d`,
		tbl, maxEntries-1)).Scan(&cutoff)
	if err != nil {
		return err
	}
	filter := fmt.Sprintf("timestamp < '%s'", escapeSQ(cutoff))
	return s.writer.DeleteOldLogs(filter)
}

func (s *cgoStore) TrimLogsByAge(maxAgeDays int) error {
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
