# Architecture -- How LanceDB and DuckDB Work Together

This document explains the data architecture of RSS-Lance: how LanceDB stores
all persistent data, how DuckDB acts as the query engine and cache layer, and
how the pieces fit together in online, offline, and degraded modes.

See also: [database.md](database.md) for table schemas, [offline.md](offline.md)
for offline-mode configuration and settings.

---

## Overview

RSS-Lance uses **two database engines** for different jobs:

| Engine | Role | Files | Safe to delete? |
|--------|------|-------|-----------------|
| **LanceDB** | All persistent storage (feeds, articles, categories, settings, logs) | `data/*.lance/` directories | No -- this is the source of truth |
| **DuckDB** (`server.duckdb`) | Lance extension cache + query engine state (see note below) | `data/server.duckdb` | **Yes** -- recreated on startup |
| **DuckDB** (`offline_cache.db`) | Write buffer, offline snapshots, log fallback | `data/offline_cache.db` | No -- holds queued writes and cached data |

**Note on `server.duckdb` by platform:**

- **Windows / CLI build**: the persistent query subprocess runs with `:memory:`.
  `server.duckdb` is only opened once at startup by a one-shot process that runs
  `INSTALL lance FROM community` to cache the extension binary on disk. The main
  query process then `LOAD`s the cached extension but runs entirely in memory.
- **Linux / CGo build**: `server.duckdb` is opened as a file-backed DuckDB
  database that stays open for all queries. It still holds no user data -- just
  DuckDB internal state (extension cache, statistics).
- **Both**: `server.duckdb` contains zero user data. All persistent state lives
  in the `.lance/` directories. Deleting it costs a few seconds on next startup
  (re-caches the Lance extension).

**Why two engines?**

- LanceDB is a columnar format optimized for versioned append/update on local
  disk, NFS, or S3. It does not have a built-in SQL engine.
- DuckDB provides full SQL (JOINs, CTEs, aggregations, window functions) and
  can query Lance files directly via `ATTACH ... (TYPE LANCE)`.
- Writes go through the LanceDB native SDK (or Python sidecar) because DuckDB's
  Lance extension is read-only for mutations like UPDATE and DELETE.

### What lives where

```
data/
  feeds.lance/            <-- LanceDB: all feed metadata (source of truth)
  articles.lance/         <-- LanceDB: all articles + content
  categories.lance/       <-- LanceDB: category hierarchy
  pending_feeds.lance/    <-- LanceDB: queued feed subscriptions
  settings.lance/         <-- LanceDB: key-value settings store
  log_fetcher.lance/      <-- LanceDB: fetcher structured logs
  log_api.lance/          <-- LanceDB: API server structured logs
  server.duckdb           <-- DuckDB: Lance extension cache (ephemeral)
  offline_cache.db        <-- DuckDB: snapshots + pending writes + log fallback
```

---

## DuckDB Build Variants

The Go server has two build modes for DuckDB access. Both implement the same
`Store` interface and behave identically from the API's perspective:

| Build | When | DuckDB access | File |
|-------|------|---------------|------|
| Windows / `duckdb_cli` tag | Default on Windows | External `duckdb.exe` persistent subprocess | `lance_windows.go` + `duckdb_process.go` |
| Linux/FreeBSD (CGo) | Default on Linux | Embedded go-duckdb via CGo (in-process) | `lance_cgo.go` |

The subprocess variant uses a **sentinel protocol** -- every query is followed
by `SELECT '__SENTINEL__' AS _s` so the reader can stay synchronized even when
queries error (DuckDB sends nothing to stdout on error). See `duckdb_process.go`
for details.

---

## Diagram 1: Online Mode (Normal Operation)

When Lance storage is reachable, reads go through DuckDB with an in-memory
write cache overlay, and writes are buffered then flushed to Lance:

```
  Browser (frontend)
      |
      v
  Go API Server
      |
      v
  writeCache (cache.go)
  Generates CTE with pending read/star overrides
      |
      v
  DuckDB query engine
  SELECT ... FROM _lance.main.articles
  LEFT JOIN pending_cte ...
  COALESCE(override, lance_value)
      |
      v
  Lance tables (*.lance/)
  Source of truth for all data


  WRITE path (mark read, star, settings, etc.)
  =============================================

  Browser
      |
      v
  Go API Server
      |
      v
  writeCache (in-memory)          Immediate: changes visible
  map[articleID]*articleOverride   in next read via CTE overlay
      |
      v
  DuckDB offline_cache.db         Durable: survives restart
  pending_changes table            (action, article_id, value, timestamp)
      |
      |  every 30s (runPendingFlush goroutine)
      v
  Lance Writer                    Flush: collapsed changes written
  (native SDK or Python sidecar)  to Lance tables permanently
      |
      v
  Lance tables (*.lance/)


  SNAPSHOT path (background, configurable interval)
  =================================================

  Lance tables (*.lance/)
      |
      |  every N minutes (runSnapshotLoop goroutine)
      v
  DuckDB offline_cache.db
  cached_articles    (recent articles by updated_at window)
  cached_feeds       (all feeds)
  cached_categories  (all categories)
  cached_settings    (all settings)
```

### Read flow step by step

1. API handler calls `store.GetArticles(...)` (or similar)
2. `writeCache.pendingCTE()` checks for in-memory overrides
3. If overrides exist, generates a SQL CTE: `WITH pending AS (VALUES ...)`
4. DuckDB runs the query: `SELECT ... FROM _lance.main.articles LEFT JOIN pending ...`
5. `COALESCE` blends overrides with Lance values -- pending changes win
6. Results returned to browser as JSON

### Write flow step by step

1. API handler calls `store.SetArticleRead(id, true)`
2. `writeCache` stores `{IsRead: true}` for that article ID (immediate)
3. `offlineCache.RecordChange("read", articleID)` writes to DuckDB `pending_changes`
4. Every 30 seconds, `runPendingFlush` reads all `pending_changes` rows
5. Collapses duplicates (last write wins per article)
6. Calls `writer.FlushOverrides(overrides)` to write to Lance
7. On success, clears `pending_changes` and resets in-memory cache

---

## Diagram 2: Offline Mode (Lance Unreachable)

When the Lance data source (NFS share, S3 bucket, remote mount) becomes
unreachable, the server switches to cached reads and queued writes:

```
  ONLINE                               OFFLINE
  ======                               =======

  Lance reachable                      Lance unreachable
       |                                    |
       v                                    v
  Health probe: SELECT 1               Health probe: FAIL
  FROM _lance.main.feeds LIMIT 1       isOffline = true
  (every 30s online)                   (retries every 5s)
       |                                    |
       v                                    v
  Normal DuckDB queries             +-> DuckDB offline_cache.db
  against Lance tables              |   cached_articles
                                    |   cached_feeds
                                    |   cached_categories
                                    |   cached_settings
                                    |
  Writes flush to Lance             |   Writes queue in DuckDB
  every 30s                         |   pending_changes table
                                    |   (replayed on reconnect)
                                    |
                                    |   Frontend shows amber banner
                                    |   "Storage offline - using cache"


  RECONNECTION
  ============

  Health probe succeeds
       |
       v
  isOffline = false
       |
       +---> Replay pending_changes --> Lance Writer --> Lance tables
       |
       +---> Resume normal DuckDB-to-Lance reads
       |
       +---> Resume snapshot loop
       |
       +---> Frontend shows green "Back online"
```

### Offline detection

Detection is both **proactive** and **reactive**:

- **Health probe**: background goroutine runs `SELECT 1 FROM _lance.main.feeds LIMIT 1`
  every 30 seconds while online, every 5 seconds while offline.
- **Reactive fallback**: if any normal read query fails with a Lance error, the
  server immediately sets `isOffline = true` and retries from the DuckDB cache.

### What is cached for offline reads

| DuckDB table | Source | Scope |
|---|---|---|
| `cached_articles` | `articles.lance` | Recent articles within configured day window (default 7 days by `updated_at`) |
| `cached_feeds` | `feeds.lance` | All rows |
| `cached_categories` | `categories.lance` | All rows |
| `cached_settings` | `settings.lance` | All rows |

Snapshots use UPSERT semantics -- only new or changed rows are written.

### What is queued for offline writes

| Action type | Stored in `pending_changes` |
|---|---|
| `read` / `unread` | article_id + is_read value |
| `star` / `unstar` | article_id + is_starred value |
| `mark_all_read` | feed_id |
| `setting` | key + value |

On reconnect, changes are collapsed (last write wins) and replayed via the
Lance writer. The frontend polls `GET /api/offline-status` every 30 seconds
to show connection state.

---

## Diagram 3: DuckDB Unavailable / Degraded Modes

DuckDB is the **only query engine** in this architecture. Without it, neither
Lance tables nor the offline cache can be read -- both require DuckDB SQL to
query. There are two failure modes:

### 3a. DuckDB binary missing or fails to start

If `duckdb.exe` is not found (Windows/CLI) or the CGo DuckDB connection fails
to open (Linux), the server **cannot start**. No reads, no writes, no API:

```
  DuckDB MISSING                       DuckDB PRESENT
  ==============                       ===============

  Server startup                       Server startup
       |                                    |
       v                                    v
  Find/open DuckDB                     Find/open DuckDB
       |                                    |
       v                                    v
  FAIL: binary not found              SUCCESS
  or connection error                       |
       |                                    v
       v                               LOAD lance extension
  SERVER CANNOT START                  ATTACH Lance tables
  No query engine = no reads                |
  No reads = no API                         v
  No API = nothing works               Normal operation

  Lance files?  ON DISK BUT UNREADABLE
  (no SQL engine to query them)

  offline_cache.db?  ON DISK BUT UNREADABLE
  (also a DuckDB database -- needs DuckDB to open)

  Writes?  IMPOSSIBLE
  (writer still works but nobody can trigger it)

  Fix: install DuckDB, then restart the server.
  No data is lost -- Lance tables are untouched.
```

### 3b. offline_cache.db fails to open (DuckDB present, cache file broken)

DuckDB itself works fine, but `offline_cache.db` cannot be opened (permissions,
corrupt file, disk full). The server runs with reduced capabilities:

```
  FULL MODE (offline_cache.db OK)        DEGRADED MODE (offline_cache.db FAILED)
  ===================================    ========================================

  Reads:                                 Reads:
    DuckDB -> Lance (online)               DuckDB -> Lance (online)
    DuckDB -> cached_* (offline)           NO OFFLINE READS (Lance must be up)

  Writes:                                Writes:
    writeCache (memory)                    writeCache (memory)
    + pending_changes (DuckDB)             NO persistent buffer
    + flush to Lance                       + flush to Lance (direct)
    Survives restart: YES                  Survives restart: NO

  Snapshots:                             Snapshots:
    Periodic Lance -> DuckDB               NONE (no cache to write to)

  Logs:                                  Logs:
    Tier 1: Lance log_api                  Tier 1: Lance log_api
    Tier 2: DuckDB cached_logs             TIER 2 UNAVAILABLE
    Tier 3: Memory buffer (100K cap)       Tier 3: Memory buffer (100K cap)
                                           Lost on restart if Lance is down

  Health probe:                          Health probe:
    YES (triggers offline switch)          Still runs, but offline switch
                                           has no cached data to serve


  Error on startup:
  =================

  Open() -> initLogFallback() -> open offline_cache.db
      |
      +-> SUCCESS: full offline support
      |
      +-> FAILURE: log warning, continue without offline cache
                   offCache = nil
                   All operations that check offCache skip gracefully
```

### Why DuckDB is required for everything

```
                    +------------------+
                    |                  |
                    |     DuckDB       |
                    |   Query Engine   |
                    |                  |
                    +---+---------+----+
                        |         |
              ATTACH    |         |    Open
              (Lance    |         |    (native
              extension)|         |    DuckDB)
                        v         v
                  +---------+ +-----------+
                  | *.lance | | offline   |
                  | tables  | | _cache.db |
                  +---------+ +-----------+

  Without DuckDB, BOTH storage layers are inaccessible:
  - Lance tables have no SQL engine to query them
  - offline_cache.db is a DuckDB file that needs DuckDB to read
  - The Lance writer can still write, but no reads = no useful API
```

### Impact summary

| Capability | Full mode | No offline_cache.db | No DuckDB at all |
|---|---|---|---|
| Server starts | Yes | Yes | **No** |
| Online reads (Lance) | Normal | Normal | **Impossible** |
| Online writes | Buffered + durable | In-memory only | **Impossible** |
| Offline reads | From DuckDB cache | **Unavailable** | **Impossible** |
| Offline writes | Queued in DuckDB | **Lost on crash** | **Impossible** |
| Log persistence | 3-tier | 2-tier (Lance + memory) | **None** |
| Snapshot loop | Active | Disabled | **N/A** |
| Data loss risk | None | Pending writes on crash | None (server won't start) |

---

## Diagram 4: DuckDB Upgrade Flow

DuckDB can be upgraded without restarting the Go server. The server provides
API endpoints to safely stop and restart the DuckDB process:

```
  Normal operation
       |
       v
  POST /api/duckdb/restart  (or stopForUpgrade internal call)
       |
       v
  1. Flush write cache
     writeCache -> pending_changes -> Lance Writer -> Lance tables
     (ensures no pending data is lost)
       |
       v
  2. Acquire query mutex
     (waits for any in-flight query to complete)
       |
       v
  3. Kill DuckDB process
     (subprocess killed, or CGo connection closed)
       |
       v
  4. stoppedForUpgrade = true
     All incoming queries return:
     "DuckDB is stopped for upgrade -- replace the binary and click Start"
       |
       v
  [User replaces duckdb.exe with new version]
       |
       v
  5. startAfterUpgrade (or POST /api/duckdb/restart again)
       |
       v
  6. Spawn new DuckDB process
     - INSTALL lance (if needed)
     - LOAD lance
     - ATTACH data directory
       |
       v
  7. Capture new version info
     - DuckDB version: SELECT version()
     - Lance extension version: from duckdb_extensions()
       |
       v
  8. stoppedForUpgrade = false
     Normal query processing resumes
       |
       v
  Back to normal operation


  server.duckdb file
  ==================

  This file only caches the Lance extension binary. It can be
  safely deleted before or after an upgrade -- it will be recreated
  on the next DuckDB startup via INSTALL lance.

  If the new DuckDB version is incompatible with the cached extension,
  delete server.duckdb and let it reinstall:

    1. Stop server (or stop DuckDB via API)
    2. Delete data/server.duckdb
    3. Replace duckdb.exe
    4. Restart server (or start DuckDB via API)
    5. server.duckdb is recreated with the compatible extension
```

### Important notes

- **Lance tables are unaffected** by DuckDB upgrades. DuckDB is just a query
  engine -- all persistent data lives in `.lance/` directories.
- **offline_cache.db** may need to be deleted if the new DuckDB version changes
  its on-disk format. The server recreates it on startup (cached data will be
  re-snapshotted from Lance; pending changes are flushed before the upgrade).
- The `server.duckdb` file is purely an extension cache. Deleting it costs a
  few seconds on next startup (re-downloads the Lance extension).

---

## Diagram 5: Lance External Writer

All writes to Lance tables go through a **writer** abstraction. There are two
implementations selected at build time:

```
  Option A: External writer (build tag: lance_external)
  =====================================================

  Go server
      |
      v
  lance_process.go
  Spawns Python subprocess
      |
      v
  tools/lance_writer.py
  Long-lived sidecar process
  JSON-line protocol on stdin/stdout
      |
      v
  LanceDB Python SDK
      |
      v
  *.lance/ files


  Option B: Native writer (default build)
  ========================================

  Go server
      |
      v
  lance_writer.go
  Uses lancedb-go SDK directly
  (in-process, no subprocess)
      |
      v
  LanceDB Rust core (via CGo)
  (compiled into server binary as liblancedb_go.a)
      |
      v
  *.lance/ files


  Only ONE of A or B is compiled in. They never run together.
  Both expose the same lanceWriter interface:

  FlushOverrides(overrides)    -- batch apply read/star changes
  SetArticleRead(id, bool)     -- single article read toggle
  SetArticleStarred(id, bool)  -- single article star toggle
  MarkAllRead(feedID)          -- bulk mark feed as read
  InsertLogs(entries)          -- batch insert log entries
  PutSetting(key, value)       -- update setting
  PutSettingsBatch(map)        -- batch update settings by value
  InsertPendingFeed(url, cat)  -- queue feed subscription
  DeletePendingFeed(url)       -- remove pending feed
  DeleteOldLogs(maxAge)        -- trim old log entries
  TableExists(name)            -- check if .lance dir exists
  TableMeta(name)              -- get schema, version, indexes
```

### External writer protocol (lance_writer.py)

The Python sidecar communicates via **JSON lines** on stdin/stdout:

```
  Go server                          lance_writer.py
      |                                   |
      |  {"cmd":"flush_overrides",        |
      |   "overrides":{                   |
      |     "id1":{"is_read":true},       |
      |     "id2":{"is_starred":false}}}  |
      |---------------------------------->|
      |                                   |  Groups by value,
      |                                   |  calls lancedb update()
      |                                   |  for each group
      |  {"ok":true}                      |
      |<----------------------------------|
      |                                   |
      |  {"cmd":"insert_logs",            |
      |   "entries":[{...},{...}]}        |
      |---------------------------------->|
      |                                   |  Calls table.add()
      |  {"ok":true}                      |
      |<----------------------------------|
```

- One command per line, one response per line
- Subprocess started once at server startup, reused for all writes
- Auto-restarts on broken pipe with retry of the failed command
- Used on Windows (where CGo linking is complex) and optionally on Linux

### Native writer (lance_writer.go)

The native writer links directly against `liblancedb_go.a` (Rust static
library compiled from a fork of lancedb-go). No subprocess, no JSON
serialization -- writes go through in-process function calls. This is the
default on Linux/FreeBSD.

---

## Diagram 6: 3-Tier Log Buffering

Log writes use a resilient 3-tier architecture that ensures logs survive
Lance outages and (partially) server restarts:

```
  log entry
      |
      v
  logBuffer (logbuffer.go)
  In-memory accumulator
  Flushes when: 20 entries OR 30 seconds elapsed
      |
      v
  flushLogs3Tier()
      |
      +---> Tier 1: Lance log_api table (via writer.InsertLogs)
      |         |
      |         +-> SUCCESS: signal runLogDrain to drain DuckDB backlog
      |         |
      |         +-> FAIL: fall through to Tier 2
      |
      +---> Tier 2: DuckDB offline_cache.db cached_logs table
      |         |
      |         +-> SUCCESS: entries safe on disk, drained later
      |         |
      |         +-> FAIL: fall through to Tier 3
      |
      +---> Tier 3: Memory buffer (prepend back to logBuffer)
                |
                +-> Capped at 100,000 entries (oldest dropped)
                +-> Lost on server restart


  Log drain goroutine (runLogDrain)
  =================================

  Runs when:
    - Tier 1 write succeeds (immediate signal)
    - Every 30 seconds (periodic check)

  DuckDB cached_logs (Tier 2 backlog)
      |
      |  SELECT * FROM cached_logs ORDER BY timestamp LIMIT 500
      v
  writer.InsertLogs(batch)  -->  Lance log_api (Tier 1)
      |
      +-> SUCCESS: DELETE batch from cached_logs, repeat
      +-> FAIL: stop, retry next cycle


  Log reads (GET /api/logs)
  =========================

  Online:  SELECT ... FROM _lance.main.log_api
           UNION ALL
           SELECT ... FROM cached_logs   (DuckDB backlog not yet drained)

  Offline: SELECT ... FROM cached_logs   (DuckDB only)
```

### Tier comparison

| Tier | Storage | Survives restart | Capacity | When used |
|------|---------|-----------------|----------|-----------|
| **1** | Lance `log_api` table | Yes | Unlimited (trimmed by age/count) | Normal operation |
| **2** | DuckDB `cached_logs` | Yes | Unlimited (drained to Tier 1) | Lance unreachable |
| **3** | Memory buffer | **No** | 100,000 entries max | Both Lance and DuckDB unreachable |

### Log query merging

When querying logs, the server UNIONs Lance and DuckDB results so that
recently-written logs (not yet drained from Tier 2) still appear in query
results. This ensures no gap in log visibility during Lance outages.

---

## Summary: Data Flow by Component

```
  +------------------+     +------------------+     +------------------+
  |                  |     |                  |     |                  |
  |   Browser /      |     |   Go API         |     |   Python         |
  |   Frontend       |<--->|   Server         |     |   Fetcher        |
  |                  |     |                  |     |                  |
  +------------------+     +--------+---------+     +--------+---------+
                                    |                        |
                 +------------------+------------------+     |
                 |                  |                  |     |
                 v                  v                  v     v
          +-----------+     +--------------+     +-----------+
          |           |     |              |     |           |
          | DuckDB    |     | DuckDB       |     | LanceDB   |
          | server    |     | offline      |     | Python    |
          | .duckdb   |     | _cache.db    |     | SDK       |
          |           |     |              |     |           |
          | Extension |     | cached_*     |     | Writes    |
          | cache     |     | pending_chg  |     | directly  |
          | (query    |     | cached_logs  |     | to .lance |
          |  engine)  |     | (buffer +    |     | tables    |
          |           |     |  offline)    |     |           |
          +-----+-----+     +------+-------+     +-----+-----+
                |                   |                   |
                |   ATTACH (read)   |   flush/snapshot  |
                +----->---+---------+-------<-----------+
                          |
                          v
                   +-------------+
                   |             |
                   | *.lance/    |
                   | directories |
                   |             |
                   | feeds       |
                   | articles    |
                   | categories  |
                   | settings    |
                   | log_api     |
                   | log_fetcher |
                   |             |
                   +-------------+
                   Source of Truth
```

### Process responsibilities

| Process | Reads from | Writes to |
|---------|-----------|-----------|
| **Go API server** | DuckDB (via Lance ATTACH) | Lance (via writer), DuckDB (cache + buffer) |
| **Python fetcher** | Lance (via Python SDK) | Lance (via Python SDK) |
| **lance_writer.py** (if external) | -- | Lance (via Python SDK, on behalf of Go server) |
| **DuckDB** (query engine) | Lance files (read-only ATTACH) | `server.duckdb` (extension cache), `offline_cache.db` (cache tables) |
