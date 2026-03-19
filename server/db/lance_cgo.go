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
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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

	// Offline cache (nil when offline mode is disabled OR log fallback DuckDB failed)
	offCache    *offlineCache
	offCtx      context.Context
	offCancel   context.CancelFunc

	// Infrastructure event ring buffer (storage_events)
	infraRing  *infraRing
	logDrainCh chan struct{} // signals the drain goroutine when Lance flush succeeds
}

// Open connects to DuckDB and loads the Lance extension.
// A local server.duckdb file is created in the data directory if it
// doesn't exist; delete it to force a clean rebuild on next startup.
// If duckdbPath is non-empty, the DuckDB database file is placed there
// instead of inside the data directory (useful when data is on NFS/S3
// but DuckDB needs a local filesystem for reliable file locking).
func Open(dataPath string, duckdbPath ...string) (Store, error) {
	// For cloud URIs, DuckDB's cache file goes next to the executable;
	// for local paths, ensure the data directory exists.
	var dbPath string
	if len(duckdbPath) > 0 && duckdbPath[0] != "" {
		// Explicit duckdb_path from config -- use it
		if err := os.MkdirAll(duckdbPath[0], 0o755); err != nil {
			return nil, fmt.Errorf("create duckdb dir: %w", err)
		}
		dbPath = filepath.Join(duckdbPath[0], "server.duckdb")
	} else if isCloudURI(dataPath) {
		exeDir, _ := os.Executable()
		dbPath = filepath.Join(filepath.Dir(exeDir), "server.duckdb")
	} else {
		dbPath = filepath.Join(dataPath, "server.duckdb")
	}

	// Ensure data directory exists for local paths
	if !isCloudURI(dataPath) {
		if err := os.MkdirAll(dataPath, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	log.Printf("DuckDB database: %s", dbPath)

	// Warn if DuckDB database is not on a local filesystem
	if isLocal, desc, err := CheckLocalFS(filepath.Dir(dbPath)); err == nil && !isLocal {
		log.Printf("WARNING: DuckDB database path %q is on %s.", dbPath, desc)
		log.Printf("WARNING: DuckDB requires a local filesystem for reliable file locking.")
		log.Printf("WARNING: Set [storage] duckdb_path in config.toml to a local directory.")
	}

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

	// Set up write cache - reads JOIN pending overrides via CTE.
	// Writes go to DuckDB pending_changes; background flush writes to Lance.
	s.cache = newWriteCache()

	// Set up buffered log writer with 3-tier fallback:
	//   Tier 1: Lance (via native lancedb-go SDK)
	//   Tier 2: DuckDB cached_logs (via offline_cache.db)
	//   Tier 3: Memory buffer (capped, entries prepended back by logBuffer)
	s.infraRing = newInfraRing(100)
	s.logDrainCh = make(chan struct{}, 1)

	s.logBuf = newLogBuffer(loadLogBufferConfig(settings), func(entries []LogEntry) error {
		return s.flushLogs3Tier(entries)
	})

	// Always open DuckDB offline_cache.db for log fallback, write buffering,
	// and offline snapshots/health probe.
	if err := s.initLogFallback(settings); err != nil {
		log.Printf("log fallback DuckDB init error (logs will only buffer in memory): %v", err)
	}

	// Start offline goroutines (snapshots, health probe)
	if err := s.initOffline(settings); err != nil {
		log.Printf("offline cache init error (continuing without offline): %v", err)
	}

	// Start log drain goroutine to move cached_logs -> Lance when possible
	go s.runLogDrain()

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
	if s.offCancel != nil {
		s.offCancel()
	}
	if s.logBuf != nil {
		s.logBuf.close()
	}
	if s.cache != nil {
		s.cache.close()
	}
	if s.offCache != nil {
		s.offCache.close()
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

func (s *cgoStore) DuckDBProcessInfo() *DuckDBProcessInfo {
	return nil
}

func (s *cgoStore) OfflineStatus() *OfflineStatus {
	if s.offCache == nil {
		return nil
	}
	st := s.offCache.status()
	return &st
}

// isOffline returns true when offline mode is enabled and Lance is unreachable.
func (s *cgoStore) isOffline() bool {
	return s.offCache != nil && s.offCache.isOffline.Load()
}

// goOffline switches to offline mode if not already offline.
func (s *cgoStore) goOffline(reason error) {
	if s.offCache == nil {
		return
	}
	if s.offCache.isOffline.CompareAndSwap(false, true) {
		log.Printf("Switched to offline mode: %v", reason)
		s.offCache.emitLog("warn", "Switched to offline mode: "+reason.Error())
	}
}

// initLogFallback opens the DuckDB offline_cache.db unconditionally for log
// fallback storage. The cached_logs table is created here. Full offline mode
// (snapshots, health probe, replay) is handled separately in initOffline.
func (s *cgoStore) initLogFallback(settings map[string]string) error {
	cfg := loadOfflineConfig(settings)

	oc, err := newOfflineCache(cfg)
	if err != nil {
		return err
	}
	oc.logFn = func(entry LogEntry) { s.WriteLog(entry) }
	s.offCache = oc
	log.Printf("Log fallback DuckDB opened: %s", cfg.CachePath)
	return nil
}

// initOffline starts the offline goroutines (snapshot loop, health probe).
// If offCache is already open (from initLogFallback), it reuses the connection.
func (s *cgoStore) initOffline(settings map[string]string) error {
	cfg := loadOfflineConfig(settings)

	// offCache was already opened by initLogFallback; just start goroutines.
	if s.offCache == nil {
		oc, err := newOfflineCache(cfg)
		if err != nil {
			return err
		}
		oc.logFn = func(entry LogEntry) { s.WriteLog(entry) }
		s.offCache = oc
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.offCtx = ctx
	s.offCancel = cancel

	// Run initial snapshot immediately
	go func() {
		if err := s.offCache.Snapshot(s); err != nil {
			log.Printf("initial offline snapshot error: %v", err)
		}
	}()

	go s.runSnapshotLoop(ctx, cfg.SnapshotIntervalMins)
	go s.runHealthProbe(ctx)
	go s.runPendingFlush(ctx)

	log.Printf("Offline cache active (snapshot every %d min, cache %d days of articles)",
		cfg.SnapshotIntervalMins, cfg.ArticleDays)
	return nil
}

// runSnapshotLoop periodically snapshots Lance data into the offline cache.
func (s *cgoStore) runSnapshotLoop(ctx context.Context, intervalMins int) {
	if intervalMins <= 0 {
		intervalMins = 10
	}
	ticker := time.NewTicker(time.Duration(intervalMins) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if s.isOffline() {
				continue // don't snapshot while offline
			}
			if err := s.offCache.Snapshot(s); err != nil {
				log.Printf("offline snapshot error: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// runHealthProbe periodically checks if Lance is reachable.
func (s *cgoStore) runHealthProbe(ctx context.Context) {
	for {
		interval := 30 * time.Second
		if s.isOffline() {
			interval = 5 * time.Second
		}

		select {
		case <-time.After(interval):
			alive := s.probeLance()
			if alive && s.isOffline() {
				s.handleReconnect()
			}
		case <-ctx.Done():
			return
		}
	}
}

// probeLance runs a lightweight query to check if Lance is reachable.
func (s *cgoStore) probeLance() bool {
	row := s.conn.QueryRow("SELECT 1 FROM " + s.lanceTable("feeds") + " LIMIT 1")
	var dummy int
	return row.Scan(&dummy) == nil
}

// runPendingFlush periodically flushes DuckDB pending_changes to Lance.
// Uses the same collapse logic as Replay: pending_changes → FlushOverrides /
// MarkAllRead / PutSettingsBatch, then clears. On failure entries stay in
// DuckDB and are retried next cycle.
func (s *cgoStore) runPendingFlush(ctx context.Context) {
	const flushInterval = 30 * time.Second
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if s.isOffline() {
				continue // don't flush while Lance is unreachable
			}
			s.flushPendingChanges()
		case <-ctx.Done():
			return
		}
	}
}

// flushPendingChanges replays buffered writes from DuckDB into Lance.
// Clears the in-memory writeCache on success.
func (s *cgoStore) flushPendingChanges() {
	if s.offCache == nil {
		return
	}
	count, err := s.offCache.Replay(s.writer)
	if err != nil {
		debug.Log(debug.Lance, "pending flush error: %v", err)
		return
	}
	if count > 0 {
		s.cache.clear()
		debug.Log(debug.Lance, "pending flush: %d changes committed to Lance", count)
	}
}

// handleReconnect flushes pending changes and switches back to online mode.
func (s *cgoStore) handleReconnect() {
	if s.offCache == nil {
		return
	}

	// Flush pending writes (same code path as periodic flush)
	s.flushPendingChanges()

	// Also drain any cached logs accumulated during the outage
	s.drainCachedLogs(500)

	s.offCache.isOffline.Store(false)
	log.Printf("Back online")
	s.offCache.emitLog("info", "Back online")

	// Trigger a fresh snapshot now that we're back
	go func() {
		if err := s.offCache.Snapshot(s); err != nil {
			log.Printf("post-reconnect snapshot error: %v", err)
		}
	}()
}

// flushLogs3Tier implements the 3-tier log write path:
//   Tier 1: Lance (native SDK) -- primary storage
//   Tier 2: DuckDB cached_logs -- survives restart, drained when Lance returns
//   Tier 3: Memory (logBuffer prepend) -- last resort, capped
func (s *cgoStore) flushLogs3Tier(entries []LogEntry) error {
	// Tier 1: Try Lance
	err := s.writer.InsertLogs(entries)
	if err == nil {
		// Lance succeeded -- signal drain goroutine to move any DuckDB backlog
		select {
		case s.logDrainCh <- struct{}{}:
		default:
		}

		// Drain infra events now that we can write
		if s.infraRing.pending() > 0 {
			infraEntries := s.infraRing.drain()
			if len(infraEntries) > 0 {
				if err2 := s.writer.InsertLogs(infraEntries); err2 != nil {
					debug.Log(debug.Batch, "infra event drain error: %v", err2)
				}
			}
		}
		return nil
	}

	// Lance failed -- record infrastructure event
	s.infraRing.add("error", fmt.Sprintf("Lance log write failed: %v", err))
	log.Printf("log flush to Lance failed (%d entries), trying DuckDB fallback: %v", len(entries), err)

	// Tier 2: Try DuckDB cached_logs
	if s.offCache != nil {
		err2 := s.offCache.insertLogs(entries)
		if err2 == nil {
			s.infraRing.add("warn", fmt.Sprintf("Logs diverted to DuckDB (%d entries)", len(entries)))
			debug.Log(debug.Batch, "log flush to DuckDB OK: %d entries", len(entries))
			return nil
		}
		s.infraRing.add("error", fmt.Sprintf("DuckDB log write also failed: %v", err2))
		log.Printf("log flush to DuckDB also failed (%d entries): %v", len(entries), err2)
	}

	// Tier 3: Return error so logBuffer prepends entries back to memory
	return err
}

// runLogDrain periodically moves log entries from DuckDB cached_logs to Lance.
func (s *cgoStore) runLogDrain() {
	const drainBatch = 500
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.drainCachedLogs(drainBatch)
		case <-s.logDrainCh:
			s.drainCachedLogs(drainBatch)
		}
	}
}

// drainCachedLogs reads a batch from DuckDB cached_logs and writes to Lance.
func (s *cgoStore) drainCachedLogs(batchSize int) {
	if s.offCache == nil {
		return
	}

	pending := s.offCache.pendingLogCount()
	if pending == 0 {
		return
	}

	entries, err := s.offCache.drainLogs(batchSize)
	if err != nil {
		debug.Log(debug.Batch, "log drain read error: %v", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	if err := s.writer.InsertLogs(entries); err != nil {
		debug.Log(debug.Batch, "log drain to Lance failed (%d entries): %v", len(entries), err)
		return
	}

	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.LogID
	}
	if err := s.offCache.deleteLogs(ids); err != nil {
		debug.Log(debug.Batch, "log drain delete error: %v", err)
		return
	}

	s.infraRing.add("info", fmt.Sprintf("Drained %d log entries from DuckDB to Lance", len(entries)))
	log.Printf("Log drain: moved %d entries from DuckDB to Lance (%d remaining)", len(entries), pending-len(entries))
}

// LogBufferStats returns the current state of the 3-tier log write path.
func (s *cgoStore) LogBufferStats() LogBufferStatus {
	st := LogBufferStatus{}
	if s.logBuf != nil {
		st.MemoryEntries = s.logBuf.pending()
	}
	if s.offCache != nil {
		st.DuckDBEntries = s.offCache.pendingLogCount()
	}
	if s.infraRing != nil {
		st.InfraEvents = s.infraRing.pending()
	}
	return st
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
	if s.isOffline() {
		return s.offCache.getFeeds()
	}
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
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getFeeds()
		}
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
	if s.isOffline() {
		return nil, nil
	}
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
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return nil, nil
		}
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
	if s.isOffline() {
		return s.offCache.getArticles(feedID, limit, offset, unreadOnly, sortAsc)
	}
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
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getArticles(feedID, limit, offset, unreadOnly, sortAsc)
		}
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
	if s.isOffline() {
		return s.offCache.getArticle(articleID)
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
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getArticle(articleID)
		}
	}
	return &a, err
}

func (s *cgoStore) GetArticleBatch(articleIDs []string) ([]Article, error) {
	if len(articleIDs) == 0 {
		return nil, nil
	}
	if s.isOffline() {
		return s.offCache.getArticleBatch(articleIDs)
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
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getArticleBatch(articleIDs)
		}
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
	s.cache.setRead(articleID, isRead)
	if s.offCache == nil {
		return nil
	}
	// Buffer in DuckDB pending_changes; background flush writes to Lance.
	action := "read"
	if !isRead {
		action = "unread"
	}
	debug.Log(debug.Lance, "buffered write: %s article %s", action, articleID)
	s.offCache.addPendingChange(articleID, action, "")
	v := isRead
	s.offCache.updateCachedArticle(articleID, &v, nil)
	return nil
}

func (s *cgoStore) SetArticleStarred(articleID string, isStarred bool) error {
	s.cache.setStarred(articleID, isStarred)
	if s.offCache == nil {
		return nil
	}
	// Buffer in DuckDB pending_changes; background flush writes to Lance.
	action := "star"
	if !isStarred {
		action = "unstar"
	}
	debug.Log(debug.Lance, "buffered write: %s article %s", action, articleID)
	s.offCache.addPendingChange(articleID, action, "")
	v := isStarred
	s.offCache.updateCachedArticle(articleID, nil, &v)
	return nil
}

func (s *cgoStore) MarkAllRead(feedID string) error {
	// Update in-memory cache for all unread articles in this feed so reads
	// see the change immediately via the CTE overlay.
	artTbl := s.lanceTable("articles")
	q := fmt.Sprintf(`SELECT article_id FROM %s WHERE feed_id = '%s' AND is_read = false`,
		artTbl, escapeSQ(feedID))
	rows, err := s.conn.Query(q)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				s.cache.setRead(id, true)
			}
		}
	}

	if s.offCache == nil {
		return nil
	}
	debug.Log(debug.Lance, "buffered write: mark_all_read feed %s", feedID)
	s.offCache.addPendingChange(feedID, "mark_all_read", "")
	s.offCache.markAllReadCached(feedID)
	return nil
}

// ── Category queries ──────────────────────────────────────────────────────────

func (s *cgoStore) GetCategories() ([]Category, error) {
	if s.isOffline() {
		return s.offCache.getCategories()
	}
	tbl := s.lanceTable("categories")
	q := fmt.Sprintf(`
		SELECT category_id, name, parent_id, sort_order, created_at, updated_at
		FROM %s
		ORDER BY sort_order, name
	`, tbl)

	rows, err := s.conn.Query(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getCategories()
		}
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
	if s.isOffline() {
		return s.offCache.getSettings(), nil
	}
	tbl := s.lanceTable("settings")
	q := fmt.Sprintf(`SELECT key, value FROM %s ORDER BY key`, tbl)

	rows, err := s.conn.Query(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getSettings(), nil
		}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Overlay any pending settings changes so writes are visible immediately.
	if s.offCache != nil {
		for k, v := range s.offCache.getSettings() {
			settings[k] = v
		}
	}
	return settings, nil
}

func (s *cgoStore) GetSetting(key string) (string, bool, error) {
	if s.isOffline() {
		v, ok := s.offCache.getSetting(key)
		return v, ok, nil
	}
	// Check for pending settings changes first (fast, in-memory).
	if s.offCache != nil {
		if v, ok := s.offCache.getSetting(key); ok {
			return v, true, nil
		}
	}
	tbl := s.lanceTable("settings")
	q := fmt.Sprintf(`SELECT value FROM %s WHERE key = '%s' LIMIT 1`, tbl, escapeSQ(key))

	row := s.conn.QueryRow(q)
	var v string
	err := row.Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			val, ok := s.offCache.getSetting(key)
			return val, ok, nil
		}
		return "", false, err
	}
	return v, true, nil
}

func (s *cgoStore) PutSetting(key, value string) error {
	if s.offCache == nil {
		return nil
	}
	debug.Log(debug.Lance, "buffered write: setting %s = %q", key, value)
	s.offCache.updateCachedSetting(key, value)
	s.offCache.addPendingSettingChange(key, value)
	return nil
}

func (s *cgoStore) PutSettings(settings map[string]string) error {
	if s.offCache == nil {
		return nil
	}
	debug.Log(debug.Lance, "buffered write: %d settings batch", len(settings))
	for k, v := range settings {
		s.offCache.updateCachedSetting(k, v)
		s.offCache.addPendingSettingChange(k, v)
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
		CAST(MIN(a.published_at) AS VARCHAR) AS oldest,
		CAST(MAX(a.published_at) AS VARCHAR) AS newest
	FROM %s a
	%s`, cte, isReadExpr, isStarredExpr, artTbl, cacheJoin)
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

	// Query cached_logs from DuckDB offline cache (fallback entries not yet drained)
	var cachedEntries []LogEntry
	if s.offCache != nil && (opts.Service == "" || opts.Service == "api") {
		if cl, err := s.offCache.queryCachedLogs(opts.Level, opts.Category); err == nil {
			cachedEntries = cl
		}
	}

	if len(sources) == 0 && len(cachedEntries) == 0 {
		return nil, 0, nil
	}

	// If we have both Lance sources and cached entries, query Lance then merge
	var lanceEntries []LogEntry
	lanceTotal := 0
	if len(sources) > 0 {
		unionSQL := strings.Join(sources, " UNION ALL ")

		var conditions []string
		if opts.Level != "" {
			conditions = append(conditions, fmt.Sprintf("level = '%s'", escapeSQ(opts.Level)))
		}
		if opts.Category != "" {
			conditions = append(conditions, fmt.Sprintf("category = '%s'", escapeSQ(opts.Category)))
		}
		if opts.StartTime != nil {
			conditions = append(conditions, fmt.Sprintf("timestamp >= '%s'", opts.StartTime.UTC().Format("2006-01-02T15:04:05Z")))
		}
		if opts.EndTime != nil {
			conditions = append(conditions, fmt.Sprintf("timestamp <= '%s'", opts.EndTime.UTC().Format("2006-01-02T15:04:05Z")))
		}
		where := ""
		if len(conditions) > 0 {
			where = " WHERE " + strings.Join(conditions, " AND ")
		}

		// Total count from Lance
		countQ := fmt.Sprintf(`SELECT COUNT(*) AS cnt FROM (%s) AS logs%s`, unionSQL, where)
		if err := s.conn.QueryRow(countQ).Scan(&lanceTotal); err != nil {
			lanceTotal = 0
		}

		if len(cachedEntries) == 0 {
			// Fast path: no cached entries, query Lance directly with pagination
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
			return entries, lanceTotal, rows.Err()
		}

		// Slow path: need to merge with cached entries
		fetchLimit := opts.Offset + opts.Limit
		q := fmt.Sprintf(`SELECT * FROM (%s) AS logs%s ORDER BY timestamp DESC LIMIT %d`,
			unionSQL, where, fetchLimit)

		rows, err := s.conn.Query(q)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()

		for rows.Next() {
			var e LogEntry
			if err := rows.Scan(&e.LogID, &e.Timestamp, &e.Level, &e.Category, &e.Service, &e.Message, &e.Details, &e.CreatedAt); err != nil {
				return nil, 0, err
			}
			lanceEntries = append(lanceEntries, e)
		}
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	}

	// Merge lance + cached entries, sort by timestamp DESC, apply pagination
	total := lanceTotal + len(cachedEntries)
	merged := make([]LogEntry, 0, len(lanceEntries)+len(cachedEntries))
	merged = append(merged, lanceEntries...)
	merged = append(merged, cachedEntries...)
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Timestamp == nil {
			return false
		}
		if merged[j].Timestamp == nil {
			return true
		}
		return merged[i].Timestamp.After(*merged[j].Timestamp)
	})

	// Apply offset and limit
	if opts.Offset >= len(merged) {
		return nil, total, nil
	}
	end := opts.Offset + opts.Limit
	if end > len(merged) {
		end = len(merged)
	}
	return merged[opts.Offset:end], total, nil
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
