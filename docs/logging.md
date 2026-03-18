# Structured Logging

RSS-Lance has a structured logging system with separate log tables per service, a unified schema for combined queries, and per-category toggles configurable from the web UI.

---

## Architecture

```
                         ┌─────────────────────────┐
                         │   log_fetcher.lance/     │  Written by Python fetcher
                         └────────────┬────────────┘
                                      │
┌──────────────┐      DuckDB UNION ALL│
│  GET /api/logs│◄─────────────────────┤
│  (Go server)  │                      │
└──────────────┘      DuckDB UNION ALL│
                                      │
                         ┌────────────┴────────────┐
                         │   log_api.lance/         │  Written by Go server
                         └─────────────────────────┘
```

Each service writes to its own Lance table. The `/api/logs` endpoint queries both tables via DuckDB `UNION ALL` to present a unified, time-ordered view.

---

## 3-Tier Log Write Path (Go Server)

The Go server uses a resilient 3-tier write path so log entries are never lost during storage outages:

```
  Log Entry
     │
     ▼
┌──────────┐   success   ┌───────────────┐
│  Lance   │◄────────────│  logBuffer    │  Tier 1: Primary (Lance)
│  (log_api│             │  (in-memory)  │
│  .lance) │             └───────┬───────┘
└──────────┘                     │ failure
                                 ▼
                          ┌───────────────┐
                          │  DuckDB       │  Tier 2: Fallback (cached_logs)
                          │  cached_logs  │
                          │  (offline_    │
                          │  cache.db)    │
                          └───────┬───────┘
                                  │ failure
                                  ▼
                          ┌───────────────┐
                          │  Memory       │  Tier 3: Last resort (logBuffer)
                          │  (prepend to  │  Subject to memory cap
                          │   logBuffer)  │
                          └───────────────┘
```

### How It Works

1. **Normal operation**: Log entries buffer in memory (`logBuffer`, batch of 20 or 30s timer), then flush to Lance via the lancedb-go native SDK.

2. **Lance unavailable**: If the Lance write fails, entries fall back to the `cached_logs` table in `offline_cache.db` (DuckDB). An infrastructure event is recorded.

3. **DuckDB also unavailable**: If both Lance and DuckDB fail, entries stay in the in-memory `logBuffer` and will retry on the next flush cycle. A memory cap (default: 100,000 entries, configurable via `log.memory_cap`) prevents unbounded growth -- oldest entries are dropped if the cap is exceeded.

4. **Drain-back**: A background goroutine (`runLogDrain`) watches for Lance recovery. When Lance becomes writable again, it drains `cached_logs` in batches of 500, writing them back to Lance and deleting from DuckDB. The `handleReconnect` path also triggers a drain.

5. **Read path**: `GET /api/logs` queries both Lance tables AND `cached_logs`, merging results by timestamp so undrained entries are visible immediately.

### Infrastructure Events

Storage problems are captured in a fixed-size ring buffer (`infraRing`, 64 entries) with category `storage_events`. These events are drained into Lance alongside normal logs when Lance recovers, providing a record of what went wrong.

### Configuration

| Setting          | Default  | Description                              |
|------------------|----------|------------------------------------------|
| `log.memory_cap` | `100000` | Max entries held in memory before oldest are dropped |

### Monitoring

The `GET /api/server-status` response includes a `log_buffer` section:

```json
{
  "log_buffer": {
    "memory_entries": 0,
    "duckdb_entries": 5,
    "infra_events": 2
  }
}
```

The `GET /api/offline-status` response includes `pending_logs` showing cached_logs count.

---

## Log Table Schema

Both `log_api` and `log_fetcher` tables share an identical schema:

| Column     | Type              | Description                              |
|------------|-------------------|------------------------------------------|
| log_id     | string (UUID)     | Unique identifier for the entry          |
| timestamp  | timestamp         | When the event occurred (UTC)            |
| level      | string            | `debug`, `info`, `warn`, or `error`      |
| category   | string            | Grouped category name (see below)        |
| message    | string            | Human-readable event description         |
| details    | string            | Optional JSON blob with structured data  |
| created_at | timestamp         | When the row was written                 |

---

## Log Categories

### Fetcher Categories

Written to `log_fetcher` by the Python fetcher daemon.

| Category            | Setting Key                       | Default | Description                          |
|---------------------|-----------------------------------|---------|--------------------------------------|
| fetch_cycle         | log.fetcher.fetch_cycle           | on      | Fetch cycle start/end summaries      |
| feed_fetch          | log.fetcher.feed_fetch            | on      | Each feed fetched + article count    |
| article_processing  | log.fetcher.article_processing    | off     | Debug: each individual article       |
| compaction          | log.fetcher.compaction            | on      | Table compaction events              |
| tier_changes        | log.fetcher.tier_changes          | on      | Adaptive frequency tier changes      |
| sanitization        | log.fetcher.sanitization          | off     | Debug: what the sanitizer stripped    |
| errors              | log.fetcher.errors                | on      | Fetch errors and failures            |

### API Server Categories

Written to `log_api` by the Go HTTP server.

| Category           | Setting Key                        | Default | Description                          |
|--------------------|------------------------------------|---------|--------------------------------------|
| lifecycle          | log.api.lifecycle                  | on      | Server start/stop events             |
| requests           | log.api.requests                   | off     | All API requests (very verbose)      |
| settings_changes   | log.api.settings_changes           | on      | When settings are modified           |
| feed_actions       | log.api.feed_actions               | on      | Add feed, mark-all-read, etc.        |
| article_actions    | log.api.article_actions            | off     | Read/star individual articles        |
| errors             | log.api.errors                     | on      | Error responses                      |
| storage_events     | (always on)                        | on      | Log storage failover/recovery events |

### Master Toggles

Each service has a master enable/disable:
- `log.fetcher.enabled` - turns off all fetcher logging when false
- `log.api.enabled` - turns off all API server logging when false

---

## Settings

All log settings are stored in the `settings` Lance table as key-value pairs. The Python fetcher seeds default values on first run. Settings can be changed via:

- **Web UI:** Settings page has grouped toggle switches per service
- **API:** `PUT /api/settings` with `{"key": "log.api.lifecycle", "value": true}`

### Log Retention

Each service trims its own log table — the Python fetcher trims `log_fetcher` after each fetch cycle, and the Go server trims `log_api` every 5 minutes via a background goroutine.

Two retention modes are available, controlled by `log.retention_mode`:

- **`count`** (default): `log.max_entries` (default 10000) sets the maximum number of entries per table. Set to `0` to retain all logs.
- **`age`**: `log.max_age_days` (default 30) deletes entries older than N days.

Switch between modes on the Settings page under **Log Retention**.

| Setting              | Values            | Default   | Description                                    |
|----------------------|-------------------|-----------|------------------------------------------------|
| `log.retention_mode` | `count` or `age`  | `count`   | Which trimming strategy to use                 |
| `log.max_entries`    | 0–100000          | `10000`   | Max entries per table (count mode). 0 = no limit |
| `log.max_age_days`   | 1–3650            | `30`      | Max age in days (age mode)                     |

---

## API Endpoint

### GET /api/logs

Returns combined log entries from all services, ordered by timestamp descending (newest first).

**Query Parameters:**

| Parameter | Values                           | Default |
|-----------|----------------------------------|---------|
| service   | `api`, `fetcher`, or empty (all) | all     |
| level     | `debug`, `info`, `warn`, `error` | all     |
| category  | any category name                | all     |
| limit     | integer                          | 100     |
| offset    | integer                          | 0       |

**Response:**

```json
{
  "entries": [
    {
      "log_id": "a1b2c3d4-...",
      "timestamp": "2026-03-17T10:30:00",
      "level": "info",
      "category": "feed_fetch",
      "service": "fetcher",
      "message": "Fetched Ars Technica: 5 new articles",
      "details": "{\"feed_id\": \"abc123\", \"new\": 5}",
      "created_at": "2026-03-17T10:30:00"
    }
  ],
  "total": 42,
  "limit": 100,
  "offset": 0
}
```

**Error responses:**
- `400` for invalid `service` or `level` values
- `500` for internal errors

---

## Writing Log Entries

### In Python (fetcher)

```python
import json

# Simple log
db.log_event("info", "feed_fetch", f"Fetched {title}: {count} new articles",
             json.dumps({"feed_id": fid, "new": count}))

# Debug-level (only written if article_processing category is enabled)
db.log_event("debug", "article_processing", f"Processing: {article_title}",
             json.dumps({"article_id": aid, "guid": guid}))

# Error
db.log_event("error", "errors", f"Failed to fetch {url}: {str(e)}",
             json.dumps({"url": url, "status_code": resp.status}))
```

Settings are cached at startup via `_load_log_settings()`. Call `db._load_log_settings()` to refresh after changing settings.

### In Go (API server)

```go
// Simple log
logger.Log("info", "lifecycle", "Server started on "+addr, "")

// Log with structured details
logger.LogJSON("info", "feed_actions", "Feed queued: "+url,
    map[string]any{"url": url, "category_id": catID})
```

The `ServerLogger` (in `api/logs.go`) checks settings before writing and writes asynchronously via goroutines.

---

## Adding a New Log Category

1. Add a default setting in `fetcher/db.py` `DEFAULT_SETTINGS`:
   ```python
   "log.fetcher.my_category": True,
   ```
2. Add the toggle to `frontend/js/settings-page.js` (in the `logGroups` array)
3. Use `db.log_event(level, "my_category", ...)` or `logger.Log(level, "my_category", ...)`
4. **Add log verification to `e2e_test.py`** - check the new log entries appear via `/api/logs`
5. Update the tables in `AGENT.md` and this document

---

## Table Creation

- **log_fetcher:** Created by the Python fetcher at startup (`db.py` `__init__`)
- **log_api:** Also created by the Python fetcher at startup (ensures the table exists for the Go server)
- **Graceful fallback:** If the Go server starts before the fetcher has run (log_api table doesn't exist yet), `WriteLog()` silently returns nil instead of failing

---

## Web UI

### Settings Page

The Settings page (Other > Settings) has a "Logging" section with:
- Master enable/disable toggle per service
- Individual toggle for each category
- Log retention mode selector (Count / Age) with matching input field
- "Save Log Settings" button (batch PUT to settings API)

### Logs Viewer

The System Logs page (Other > System Logs) shows:
- Combined logs from all services in a table
- Filters: service dropdown, level dropdown, entries-per-page
- Click a row to expand and view the details JSON
- Pagination controls (Previous / Next)
- Auto-refresh toggle (30-second interval)
- Level-colored badges (green/blue/yellow/red) and service badges

---

## E2E Testing

The `e2e_test.py` integration test verifies the full logging pipeline:

1. Enables all log categories (including debug-level) via `db.put_settings()`
2. Writes 6 test log entries via `db.log_event()` covering all fetcher categories
3. Verifies entries exist in `log_fetcher` via DuckDB (count, levels, categories, details JSON)
4. Verifies `log_api` table was created (empty, ready for server)
5. After server actions (queue feed, mark-all-read), queries `/api/logs` to verify:
   - Lifecycle logs (server started)
   - Feed action logs (queue, mark-all-read)
   - Service filter (`?service=fetcher`, `?service=api`)
   - Level filter (`?level=error`)
   - Category filter (`?category=feed_fetch`)
   - Pagination (limit/offset, no overlap)
   - Invalid filter rejection (400 status)
   - Timestamp ordering (descending)
   - Log entry structure (log_id, timestamp, level, service, details JSON)
