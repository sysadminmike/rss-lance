//go:build windows || duckdb_cli

// CLI implementation of Store using a persistent DuckDB CLI process.
// Used on Windows always, and on Linux/macOS/FreeBSD when built with
// -tags duckdb_cli (fallback when embedded go-duckdb CGo fails to compile).
// Queries are piped via stdin and JSON results read from stdout.
//
// IMPORTANT: DuckDB is used ONLY as a query/write engine against Lance files.
// A local server.duckdb file caches extension installs; it can be safely
// deleted and will be recreated on next startup.  ALL persistent state lives
// in the Lance tables so the user can connect from any machine.
//
// DuckDB uses exclusive file locks on Windows, so only ONE process can
// ATTACH the data directory.  The persistent process auto-restarts if it
// dies (crash, OOM, user kill via Task Manager, etc.).
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"rss-lance/server/debug"
)

// cliStore uses a persistent duckdb.exe process for reads and the
// lancedb-go native SDK for writes.  The persistent process is started
// once and reused for all queries; if it dies it auto-restarts.
//
// All CUD (Create/Update/Delete) operations go through the embedded
// lanceWriter which uses the lancedb-go native SDK.
type cliStore struct {
	dataPath  string
	dbPath    string // local DuckDB database file (cache; safe to delete)
	duckdbBin string
	duckProc  *duckDBProcess // persistent duckdb.exe for reads
	cache     *writeCache
	writer    *lanceWriter // native lancedb-go for CUD ops
	logBuf    *logBuffer   // buffered log writes via native SDK

	// Offline cache (nil when offline mode is disabled OR log fallback DuckDB failed)
	offCache    *offlineCache
	offCtx      context.Context
	offCancel   context.CancelFunc

	// Infrastructure event ring buffer (storage_events)
	infraRing  *infraRing
	logDrainCh chan struct{} // signals the drain goroutine when Lance flush succeeds

	probeWake chan struct{} // wakes runHealthProbe when goOffline fires

	logTableCache map[string]bool // cached positive os.Stat results for log tables
}

// findDuckDB locates the DuckDB CLI binary.
// Search order: next to server binary -> tools/ subdir -> PATH.
func findDuckDB() (string, error) {
	// Binary name and path separator differ by OS
	binName := "duckdb"
	toolsRel := "tools/duckdb"
	if runtime.GOOS == "windows" {
		binName = "duckdb.exe"
		toolsRel = "tools\\duckdb.exe"
	}

	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for _, rel := range []string{binName, toolsRel} {
			candidate := filepath.Join(dir, rel)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	// Also check relative to CWD
	for _, rel := range []string{toolsRel, binName} {
		if _, err := os.Stat(rel); err == nil {
			abs, _ := filepath.Abs(rel)
			return abs, nil
		}
	}
	if p, err := exec.LookPath("duckdb"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf(
		"%s not found - place it next to the server binary, in tools/, or add to PATH.\n"+
			"Download from https://github.com/duckdb/duckdb/releases", binName)
}

// Open creates a cliStore backed by an external duckdb.exe process.
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

	duckdbBin, err := findDuckDB()
	if err != nil {
		return nil, err
	}
	log.Printf("Using DuckDB CLI: %s", duckdbBin)

	log.Printf("DuckDB database: %s", dbPath)

	// Warn if DuckDB database is not on a local filesystem
	if isLocal, desc, err := CheckLocalFS(filepath.Dir(dbPath)); err == nil && !isLocal {
		log.Printf("WARNING: DuckDB database path %q is on %s.", dbPath, desc)
		log.Printf("WARNING: DuckDB requires a local filesystem for reliable file locking.")
		log.Printf("WARNING: Set [storage] duckdb_path in config.toml to a local directory.")
	}

	s := &cliStore{
		dataPath:  dataPath,
		dbPath:    dbPath,
		duckdbBin: duckdbBin,
		probeWake: make(chan struct{}, 1),
	}

	if err := s.bootstrap(); err != nil {
		return nil, err
	}

	// Phase 2: Start persistent duckdb.exe process (LOAD lance + ATTACH)
	proc, err := newDuckDBProcess(duckdbBin, dbPath, dataPath)
	if err != nil {
		return nil, fmt.Errorf("persistent duckdb: %w", err)
	}
	s.duckProc = proc
	s.duckProc.logFn = func(entry LogEntry) { s.WriteLog(entry) }

	// Open native lancedb-go writer for all CUD operations
	w, err := newLanceWriter(dataPath)
	if err != nil {
		return nil, fmt.Errorf("lance writer: %w", err)
	}
	s.writer = w
	s.writer.logFn = func(entry LogEntry) { s.WriteLog(entry) }
	s.writer.emitLog("info", fmt.Sprintf("Lance Python writer started (pid %d), lancedb %s, pyarrow %s",
		s.writer.pid(), s.writer.lancedbVersion, s.writer.pyarrowVersion))

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

// bootstrap runs Phase 1: a quick one-shot process to INSTALL the lance
// extension (cached in server.duckdb).  This is idempotent and fast if
// already installed.
func (s *cliStore) bootstrap() error {
	var extra string
	if isCloudURI(s.dataPath) {
		extra = "INSTALL httpfs;\n"
	}
	stmts := fmt.Sprintf("INSTALL lance FROM community;\n%s", extra)

	cmd := exec.Command(s.duckdbBin, s.dbPath, "-c", stmts)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("bootstrap INSTALL warning: %v | output: %s", err, truncate(string(out), 500))
	}
	return nil
}

func (s *cliStore) Close() error {
	if s.offCancel != nil {
		s.offCancel()
	}
	if s.logBuf != nil {
		s.logBuf.close()
	}
	if s.cache != nil {
		s.cache.close()
	}
	if s.duckProc != nil {
		s.duckProc.close()
	}
	if s.offCache != nil {
		s.offCache.close()
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

func (s *cliStore) DuckDBProcessInfo() *DuckDBProcessInfo {
	if s.duckProc == nil {
		return nil
	}
	// When stopped for upgrade, return info with Stopped=true and last-known versions
	if s.duckProc.stoppedForUpgrade {
		return &DuckDBProcessInfo{
			PID:           0,
			UptimeSeconds: 0,
			DuckDBVersion: s.duckProc.duckdbVersion,
			LanceVersion:  s.duckProc.lanceVersion,
			Stopped:       true,
		}
	}
	pid := s.duckProc.pid()
	if pid == 0 {
		return nil
	}
	return &DuckDBProcessInfo{
		PID:           pid,
		UptimeSeconds: int64(s.duckProc.uptime().Seconds()),
		DuckDBVersion: s.duckProc.duckdbVersion,
		LanceVersion:  s.duckProc.lanceVersion,
	}
}

func (s *cliStore) RestartDuckDB() error {
	if s.duckProc == nil {
		return fmt.Errorf("no DuckDB process to restart")
	}
	return s.duckProc.gracefulRestart()
}

func (s *cliStore) StopDuckDB() error {
	if s.duckProc == nil {
		return fmt.Errorf("no DuckDB process to stop")
	}
	if s.isOffline() {
		return fmt.Errorf("cannot stop DuckDB for upgrade while server is offline (write cache cannot be flushed to Lance)")
	}
	// Flush write cache before stopping
	s.FlushPendingChanges()
	return s.duckProc.stopForUpgrade()
}

func (s *cliStore) StartDuckDB() error {
	if s.duckProc == nil {
		return fmt.Errorf("no DuckDB process to start")
	}
	return s.duckProc.startAfterUpgrade()
}

func (s *cliStore) OfflineStatus() *OfflineStatus {
	if s.offCache == nil {
		return nil
	}
	st := s.offCache.status()
	return &st
}

// isOffline returns true when offline mode is enabled and Lance is unreachable.
func (s *cliStore) isOffline() bool {
	return s.offCache != nil && s.offCache.isOffline.Load()
}

// goOffline switches to offline mode if not already offline.
func (s *cliStore) goOffline(reason error) {
	if s.offCache == nil {
		return
	}
	if s.offCache.isOffline.CompareAndSwap(false, true) {
		log.Printf("Switched to offline mode: %v", reason)
		s.offCache.emitLog("warn", "Switched to offline mode: "+reason.Error())
		// Wake the health probe so it switches to fast polling immediately.
		select {
		case s.probeWake <- struct{}{}:
		default:
		}
	}
}

// initLogFallback opens the DuckDB offline_cache.db unconditionally for log
// fallback storage. The cached_logs table is created here. Full offline mode
// (snapshots, health probe, replay) is handled separately in initOffline.
func (s *cliStore) initLogFallback(settings map[string]string) error {
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
func (s *cliStore) initOffline(settings map[string]string) error {
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

// flushLogs3Tier implements the 3-tier log write path:
//   Tier 1: Lance (native SDK) -- primary storage
//   Tier 2: DuckDB cached_logs -- survives restart, drained when Lance returns
//   Tier 3: Memory (logBuffer prepend) -- last resort, capped
func (s *cliStore) flushLogs3Tier(entries []LogEntry) error {
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
					// Not critical -- they'll be regenerated
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
		// DuckDB also failed
		s.infraRing.add("error", fmt.Sprintf("DuckDB log write also failed: %v", err2))
		log.Printf("log flush to DuckDB also failed (%d entries): %v", len(entries), err2)
	}

	// Tier 3: Return error so logBuffer prepends entries back to memory
	return err
}

// runLogDrain periodically moves log entries from DuckDB cached_logs to Lance.
// Also triggered immediately when a Lance flush succeeds (via logDrainCh).
func (s *cliStore) runLogDrain() {
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
func (s *cliStore) drainCachedLogs(batchSize int) {
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

	// Successfully drained -- delete from DuckDB
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
func (s *cliStore) LogBufferStats() LogBufferStatus {
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

// runSnapshotLoop periodically snapshots Lance data into the offline cache.
func (s *cliStore) runSnapshotLoop(ctx context.Context, intervalMins int) {
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
// When offline, probes more frequently. On recovery, triggers replay.
func (s *cliStore) runHealthProbe(ctx context.Context) {
	for {
		interval := 30 * time.Second
		if s.isOffline() {
			interval = 5 * time.Second
		}

		select {
		case <-time.After(interval):
		case <-s.probeWake:
			// goOffline fired; give a brief pause then probe.
			time.Sleep(2 * time.Second)
		case <-ctx.Done():
			return
		}

		alive := s.probeLance()
		if alive && s.isOffline() {
			s.handleReconnect()
		}
	}
}

// probeLance runs a lightweight query to check if Lance is reachable.
func (s *cliStore) probeLance() bool {
	rows, err := s.lanceQuery("SELECT 1 FROM " + s.lanceTable("feeds") + " LIMIT 1")
	alive := err == nil && len(rows) > 0
	if !alive {
		log.Printf("probeLance: alive=%v err=%v rows=%d", alive, err, len(rows))
	}
	return alive
}

// runPendingFlush periodically flushes DuckDB pending_changes to Lance.
// Uses the same collapse logic as Replay: pending_changes → FlushOverrides /
// MarkAllRead / PutSettingsBatch, then clears. On failure entries stay in
// DuckDB and are retried next cycle.
func (s *cliStore) runPendingFlush(ctx context.Context) {
	const flushInterval = 30 * time.Second
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if s.isOffline() {
				continue // don't flush while Lance is unreachable
			}
			s.FlushPendingChanges()
		case <-ctx.Done():
			return
		}
	}
}

// FlushPendingChanges replays buffered writes from DuckDB into Lance.
// Clears the in-memory writeCache on success.
func (s *cliStore) FlushPendingChanges() {
	if s.offCache == nil {
		return
	}
	count, err := s.offCache.Replay(s.writer)
	if err != nil {
		debug.Log(debug.Lance, "pending flush error: %v", err)
		if s.writer != nil {
			s.writer.emitLog("error", fmt.Sprintf("Flush pending changes failed: %v", err))
		}
		return
	}
	if count > 0 {
		s.cache.clear()
		debug.Log(debug.Lance, "pending flush: %d changes committed to Lance", count)
		if s.writer != nil {
			s.writer.emitLog("info", fmt.Sprintf("Flushed %d pending changes to Lance", count))
		}
	}
}

// handleReconnect flushes pending changes and switches back to online mode.
func (s *cliStore) handleReconnect() {
	if s.offCache == nil {
		return
	}

	// Flush pending writes (same code path as periodic flush)
	s.FlushPendingChanges()

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

// ── low-level persistent process helpers ──────────────────────────────────────

// lanceQuery runs a SELECT against the persistent duckdb.exe process.
// No preamble needed -- LOAD + ATTACH done at process startup.
func (s *cliStore) lanceQuery(sql string) ([]map[string]any, error) {
	return s.duckProc.query(sql)
}

// lanceExec runs a write statement against the persistent process.
func (s *cliStore) lanceExec(sql string) error {
	return s.duckProc.execStmt(sql)
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

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getFeeds()
		}
		return nil, err
	}
	feeds := make([]Feed, 0, len(rows))
	for _, r := range rows {
		feeds = append(feeds, rowToFeed(r))
	}
	return feeds, nil
}

func (s *cliStore) GetFeed(feedID string) (*Feed, error) {
	if s.isOffline() {
		// GetFeed not cached individually; return nil (feed details not available offline)
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

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return nil, nil
		}
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
			%s AS is_read, %s AS is_starred,
			a.created_at, a.updated_at
		FROM %s a
		%s
		WHERE 1=1 %s %s
		ORDER BY a.published_at %s
		LIMIT %d OFFSET %d
	`, cte, isReadExpr, isStarredExpr, artTbl, cacheJoin,
		feedFilter, unreadFilter, sortDir, limit, offset)

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getArticles(feedID, limit, offset, unreadOnly, sortAsc)
		}
		return nil, err
	}
	arts := make([]Article, 0, len(rows))
	for _, r := range rows {
		arts = append(arts, rowToArticle(r))
	}
	return arts, nil
}

func (s *cliStore) GetArticle(articleID string) (*Article, error) {
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

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getArticle(articleID)
		}
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

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getArticleBatch(articleIDs)
		}
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

func (s *cliStore) SetArticleStarred(articleID string, isStarred bool) error {
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

func (s *cliStore) MarkAllRead(feedID string) error {
	// Update in-memory cache for all unread articles in this feed so reads
	// see the change immediately via the CTE overlay.
	artTbl := s.lanceTable("articles")
	q := fmt.Sprintf(`SELECT article_id FROM %s WHERE feed_id = '%s' AND is_read = false`,
		artTbl, escapeSQ(feedID))
	rows, err := s.lanceQuery(q)
	if err == nil {
		for _, r := range rows {
			s.cache.setRead(rowStr(r, "article_id"), true)
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

func (s *cliStore) GetCategories() ([]Category, error) {
	if s.isOffline() {
		return s.offCache.getCategories()
	}
	tbl := s.lanceTable("categories")
	q := fmt.Sprintf(`
		SELECT category_id, name, parent_id, sort_order, created_at, updated_at
		FROM %s
		ORDER BY sort_order, name
	`, tbl)

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getCategories()
		}
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
	if s.isOffline() {
		return s.offCache.getSettings(), nil
	}
	tbl := s.lanceTable("settings")
	q := fmt.Sprintf(`SELECT key, value FROM %s ORDER BY key`, tbl)

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			return s.offCache.getSettings(), nil
		}
		return nil, err
	}
	settings := make(map[string]string, len(rows))
	for _, r := range rows {
		settings[rowStr(r, "key")] = rowStr(r, "value")
	}
	// Overlay any pending settings changes so writes are visible immediately.
	if s.offCache != nil {
		for k, v := range s.offCache.getSettings() {
			settings[k] = v
		}
	}
	return settings, nil
}

func (s *cliStore) GetSetting(key string) (string, bool, error) {
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

	rows, err := s.lanceQuery(q)
	if err != nil {
		if s.offCache != nil {
			s.goOffline(err)
			v, ok := s.offCache.getSetting(key)
			return v, ok, nil
		}
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	return rowStr(rows[0], "value"), true, nil
}

func (s *cliStore) PutSetting(key, value string) error {
	if s.offCache == nil {
		return nil
	}
	debug.Log(debug.Lance, "buffered write: setting %s = %q", key, value)
	s.offCache.updateCachedSetting(key, value)
	s.offCache.addPendingSettingChange(key, value)
	return nil
}

func (s *cliStore) PutSettings(settings map[string]string) error {
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
	// Cache positive results -- once a log table exists it stays
	if s.logTableCache[name] {
		return true
	}
	dir := filepath.Join(s.dataPath, name+".lance")
	info, err := os.Stat(dir)
	exists := err == nil && info.IsDir()
	if exists {
		if s.logTableCache == nil {
			s.logTableCache = make(map[string]bool)
		}
		s.logTableCache[name] = true
	}
	return exists
}

func (s *cliStore) WriteLog(entry LogEntry) error {
	if s.logBuf == nil {
		return nil
	}
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

		// Build WHERE clause
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

		// Get total count from Lance
		countQ := fmt.Sprintf(`SELECT COUNT(*) AS cnt FROM (%s) AS logs%s`, unionSQL, where)
		countRows, err := s.lanceQuery(countQ)
		if err == nil && len(countRows) > 0 {
			lanceTotal = rowInt(countRows[0], "cnt")
		}

		if len(cachedEntries) == 0 {
			// Fast path: no cached entries, query Lance directly with pagination
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
			return entries, lanceTotal, nil
		}

		// Slow path: need to merge with cached entries
		// Fetch enough from Lance to cover the requested page after merging
		fetchLimit := opts.Offset + opts.Limit
		q := fmt.Sprintf(`SELECT * FROM (%s) AS logs%s ORDER BY timestamp DESC LIMIT %d`,
			unionSQL, where, fetchLimit)

		rows, err := s.lanceQuery(q)
		if err != nil {
			return nil, 0, err
		}

		for _, r := range rows {
			lanceEntries = append(lanceEntries, LogEntry{
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
