# Offline Mode / Buffered Writes

The Go server uses a local DuckDB file (`offline_cache.db`) as a write buffer and offline cache. This is **always active** -- there is no toggle. All user writes (mark read, star, settings changes) are buffered through a `pending_changes` table in DuckDB first, then flushed to Lance every 30 seconds by a background goroutine.

When the Lance data source (NFS share, S3 bucket, remote mount) becomes unreachable, the server continues working: reads fall back to cached data and writes accumulate in the buffer until the connection returns.

Useful for:

- Laptops that disconnect from Wi-Fi or a VPN
- NFS/Samba shares that go down for maintenance
- S3 outages or credential expiry
- Storage server reboots
- Normal operation (write buffering reduces Lance write amplification)

---

## How It Works

### Normal Mode (online)

```
Browser --> Go server --> in-memory write cache (CTE overlay)
                              |
                              +--> DuckDB pending_changes (buffered writes)
                              |        |
                              |        +--> flush to Lance every 30s
                              |
                              +--> DuckDB reads --> Lance tables
```

All writes go through two layers:
1. **In-memory write cache** (`cache.go`) -- overlays read/star changes onto DuckDB query results via SQL CTEs so changes are visible immediately
2. **DuckDB pending_changes** (`offline_cache.go`) -- persists all changes to disk so they survive server restarts; flushed to Lance every 30 seconds

### Offline Mode (Lance unreachable)

```
Browser --> Go server --> local DuckDB cache (offline_cache.db)
                              |
                              +-- pending_changes table (replayed on reconnect)
```

When Lance becomes unreachable, reads fall back to cached snapshots and writes continue accumulating in `pending_changes`. When the connection returns, queued changes are replayed.

---

## Configuration

The offline/buffering system starts automatically. Snapshot and cache settings are stored in the `settings` Lance table (editable via **Other -> Settings** in the UI):

| Setting | Default | Description |
|---|---|---|
| Snapshot interval (minutes) | 10 | How often to refresh the local cache while online |
| Article days | 7 | How many days of articles to cache (by `updated_at`) |

The cache file path defaults to `./data/offline_cache.db` and can be changed via the `offline_cache_path` setting key if needed.

---

## Snapshot (Online -> Cache)

While online, a background goroutine runs on the configured interval and copies:

| Data | Scope |
|---|---|
| Articles | Where `updated_at` is within the configured article-days window. Full content included so articles are readable offline, not just titles. |
| Feeds | All rows (small table) |
| Categories | All rows (small table) |
| Settings | All rows (needed for server config while offline) |

Snapshots use UPSERT semantics -- only new or changed rows are written. The snapshot timestamp is tracked and reported via the API.

`updated_at` (the server's own timestamp, refreshed on every read/star/modification) is used rather than `published_at` (the feed author's timestamp). This ensures that newly imported feeds with old publication dates are still captured in the cache.

---

## Offline Detection

Detection is both **proactive** and **reactive**:

- **Health probe**: A background goroutine runs a lightweight Lance query (`SELECT 1 FROM _lance.main.feeds LIMIT 1`) every 30 seconds while online, every 5 seconds while offline.
- **Reactive fallback**: If any normal read query fails with a Lance error, the server immediately switches to offline mode and retries from the cache.

When the health probe fails, `isOffline` is set to true and all subsequent reads are served from the local cache.

---

## Reading (Online + Offline)

All Store read methods use a two-layer overlay: in-memory write cache (CTE) on top of DuckDB/Lance queries. When offline, reads transparently fall back to the local DuckDB cache:

| Endpoint | Reads from cache |
|---|---|
| `GET /api/feeds` | `cached_feeds` |
| `GET /api/feeds/:id` | `cached_feeds` |
| `GET /api/articles` | `cached_articles` (with sort, filter, pagination) |
| `GET /api/articles/:id` | `cached_articles` |
| `GET /api/articles/batch` | `cached_articles` |
| `GET /api/categories` | `cached_categories` |
| `GET /api/settings` | In-memory settings copy → `cached_settings` → defaults |

API handlers don't know whether they're serving live or cached data. The fallback is handled inside the Store implementation.

---

## Writing (Buffered Path)

All user write actions are buffered through two layers:

1. **In-memory write cache** (`cache.go`) -- the `cached_articles` or `cached_settings` row is updated immediately so subsequent reads reflect the change via CTE overlay.
2. **DuckDB pending changes** (`offline_cache.go`) -- the action is appended to the `pending_changes` DuckDB table. A background goroutine flushes pending changes to Lance every 30 seconds while online. While offline, changes accumulate until reconnection.

Supported offline write actions:

| Action | What's recorded |
|---|---|
| Mark article read/unread | `action=read` or `action=unread`, `article_id`, `value=true/false` |
| Star/unstar article | `action=star` or `action=unstar`, `article_id`, `value=true/false` |
| Mark all read (feed) | `action=mark_all_read`, `article_id=<feed_id>` |
| Change a setting | `action=setting`, `article_id=<key>`, `value=<json>` |

**Deduplication rules:**

- **Settings**: UPSERT keyed on `(action='setting', article_id=key)`. Changing the same setting 5 times while offline keeps only the final value.
- **Articles**: All rows kept; collapsed to final state per article at replay time.

---

## Flush & Replay

While online, a background goroutine (`runPendingFlush`) runs every 30 seconds and flushes pending changes to Lance. When coming back from offline, the same process replays accumulated changes:

1. Read all rows from `pending_changes` ordered by ID
2. Collapse article changes per `article_id` to a final state (read and star are independent)
3. Replay `mark_all_read` entries via `lanceWriter.MarkAllRead(feedID)`
4. Replay article overrides in batch via `lanceWriter.FlushOverrides()`
5. Replay settings changes via `lanceWriter.PutSettingsBatch()`
6. On success: clear `pending_changes`
7. Evict read articles from the cache (`DELETE FROM cached_articles WHERE is_read = true`)
8. Set `isOffline = false`

Conflict resolution is last-write-wins (single-user system -- no merge logic needed).

---

## Cache Schema

The local DuckDB file (`offline_cache.db`) contains 6 tables:

| Table | Purpose |
|---|---|
| `cached_articles` | Mirrors `articles.lance` -- full article content for offline reading |
| `cached_feeds` | Mirrors `feeds.lance` -- feed metadata for the sidebar |
| `cached_categories` | Mirrors `categories.lance` -- category tree |
| `cached_settings` | Mirrors `settings.lance` -- all settings (key/value) |
| `pending_changes` | Change log -- `id`, `article_id`, `action`, `value`, `timestamp` |
| `cached_logs` | Log fallback buffer -- entries that failed to write to Lance (see [logging.md](logging.md#3-tier-log-write-path-go-server)) |

---

## API Endpoint

### `GET /api/offline-status`

Returns the current offline state. Polled by the frontend every 30 seconds.

```json
{
  "offline": false,
  "pending_changes": 0,
  "pending_logs": 0,
  "last_snapshot": "2026-03-18T10:30:00Z",
  "cache_articles": 342
}
```

| Field | Type | Description |
|---|---|---|
| `offline` | bool | Whether the server is currently in offline mode |
| `pending_changes` | int | Number of queued writes waiting to flush/replay |
| `pending_logs` | int | Number of log entries in `cached_logs` waiting to drain back to Lance |
| `last_snapshot` | string | ISO 8601 timestamp of the last successful snapshot |
| `cache_articles` | int | Number of articles in the local cache |

---

## Frontend

The browser UI shows a banner at the top of the page:

- **Offline**: Amber banner -- "Working offline -- N changes pending"
- **Back online**: Green banner -- "Back online -- changes synced" (auto-hides after 5 seconds)

The banner appears automatically based on the `/api/offline-status` poll. No user action is needed.

---

## Settings Reference

All offline settings are stored in the `settings` Lance table (same as all other runtime settings). They can be read and written via `GET/PUT /api/settings`.

| Key | Default | Description |
|---|---|---|
| `offline_snapshot_interval_mins` | `10` | Snapshot interval in minutes |
| `offline_article_days` | `7` | Days of articles to cache (by `updated_at`) |
| `offline_cache_path` | `./data/offline_cache.db` | Path to the local DuckDB cache file |

---

## Files

| File | Role |
|---|---|
| `server/db/offline_cache.go` | Cache manager -- DuckDB file ops, snapshot, pending changes, flush/replay, eviction |
| `server/db/cache.go` | In-memory write cache -- CTE overlay for immediate read visibility of pending changes |
| `server/db/lance_windows.go` | Windows Store -- buffered write path + offline fallback on all read/write methods |
| `server/db/lance_cgo.go` | Linux/FreeBSD Store -- same buffered write path + offline fallback |
| `server/db/fscheck_windows.go` | Windows local-FS detection (GetDriveTypeW) |
| `server/db/fscheck_other.go` | Non-Windows local-FS detection (statfs) |
| `server/db/fscheck_linux.go` | Linux statfs magic-number check |
| `server/db/fscheck_bsd.go` | FreeBSD/macOS Fstypename check |
| `server/api/offline_status.go` | `GET /api/offline-status` endpoint |
| `frontend/js/app.js` | Offline status polling and banner display |
| `frontend/js/settings-page.js` | Offline cache settings UI section |
| `frontend/css/style.css` | Offline banner styling |

---

## Limitations

- **Cache age**: Articles older than the configured window are not available offline. Starred articles are not exempt -- they follow the same time-window rules.
- **Single user**: Conflict resolution is last-write-wins. There is no merge logic for concurrent writers.
- **No fetcher integration**: The Python fetcher continues writing to Lance normally. It is unaware of offline mode. New articles fetched while the server is offline won't appear until the next snapshot after reconnection.
- **Full content required**: The cache stores full article HTML. If article content is large, the cache file size grows accordingly.

---

## DuckDB Path Separation

When Lance data lives on a network share (NFS, SMB) or S3, the DuckDB database file (`server.duckdb`) should be on local storage. DuckDB requires a local filesystem for reliable file locking -- network filesystems may silently corrupt the file.

Set `duckdb_path` in `config.toml` under `[storage]`:

```toml
[storage]
type = "local"
path = "/mnt/nas/rss-lance"        # Lance data on NFS
duckdb_path = "/var/lib/rss-lance"  # DuckDB on local SSD
```

The server warns at startup if the DuckDB path is detected as non-local (NFS, SMB, CIFS, removable drive). Detection uses:

- **Windows**: `GetDriveTypeW` API (detects network shares and removable drives)
- **Linux**: `statfs` magic numbers (NFS, CIFS, FUSE, Ceph, Lustre, etc.)
- **FreeBSD/macOS**: `statfs` Fstypename string (nfs, smbfs, cifs, fuse, etc.)
