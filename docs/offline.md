# Offline Mode

Offline mode lets the Go server keep working when the Lance data source (NFS share, S3 bucket, remote mount) becomes temporarily unreachable. "Offline" means the server can't read or write Lance files -- the machine itself may still be on a network.

Useful for:

- Laptops that disconnect from Wi-Fi or a VPN
- NFS/Samba shares that go down for maintenance
- S3 outages or credential expiry
- Storage server reboots

---

## How It Works

### Normal Mode

```
Browser --> Go server --> DuckDB --> Lance tables (remote or local)
```

### Offline Mode

```
Browser --> Go server --> local DuckDB cache (offline_cache.db)
                              |
                              +-- pending_changes table (replayed on reconnect)
```

While online, a background goroutine periodically snapshots recent data from Lance into a local DuckDB file (`offline_cache.db`). When Lance becomes unreachable, all reads fall back to this cache and all writes are recorded in a `pending_changes` table. When the connection returns, queued changes are replayed back to Lance.

---

## Enabling Offline Mode

Open the browser UI, go to **Other → Settings**, and toggle **Offline Mode** on. The settings are:

| Setting | Default | Description |
|---|---|---|
| Enabled | Off | Master toggle -- nothing happens until this is on |
| Snapshot interval (minutes) | 10 | How often to refresh the local cache while online |
| Article days | 7 | How many days of articles to cache (by `updated_at`) |

These are stored in the `settings` Lance table and take effect immediately -- no server restart needed.

The cache file path defaults to `./data/offline_cache.db` and can be changed via the `offline_cache_path` setting key if needed.

---

## Snapshot (Online → Cache)

While online and enabled, a background goroutine runs on the configured interval and copies:

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

## Reading While Offline

All Store read methods transparently fall back to the DuckDB cache:

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

## Writing While Offline

User actions while offline are recorded in two places:

1. **Local cache update** -- the `cached_articles` or `cached_settings` row is updated immediately so subsequent reads reflect the change.
2. **Pending changes log** -- the action is appended to the `pending_changes` DuckDB table for later replay.

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

## Reconnection & Replay

When the health probe detects that Lance is reachable again:

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

**When offline mode is disabled:**

```json
{
  "enabled": false,
  "offline": false
}
```

**When enabled:**

```json
{
  "enabled": true,
  "offline": false,
  "pending_changes": 0,
  "pending_logs": 0,
  "last_snapshot": "2026-03-18T10:30:00Z",
  "cache_articles": 342
}
```

| Field | Type | Description |
|---|---|---|
| `enabled` | bool | Whether offline mode is turned on in settings |
| `offline` | bool | Whether the server is currently in offline mode |
| `pending_changes` | int | Number of queued writes waiting to replay |
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
| `offline_enabled` | `false` | Master toggle |
| `offline_snapshot_interval_mins` | `10` | Snapshot interval in minutes |
| `offline_article_days` | `7` | Days of articles to cache (by `updated_at`) |
| `offline_cache_path` | `./data/offline_cache.db` | Path to the local DuckDB cache file |

---

## Files

| File | Role |
|---|---|
| `server/db/offline_cache.go` | Cache manager -- DuckDB file ops, snapshot, pending changes, replay, eviction |
| `server/db/lance_windows.go` | Windows Store -- offline fallback wrappers on all read/write methods |
| `server/db/lance_cgo.go` | Linux/FreeBSD Store -- same fallback wrappers |
| `server/db/fscheck_windows.go` | Windows local-FS detection (GetDriveTypeW) |
| `server/db/fscheck_other.go` | Non-Windows local-FS detection (statfs) |
| `server/db/fscheck_linux.go` | Linux statfs magic-number check |
| `server/db/fscheck_bsd.go` | FreeBSD/macOS Fstypename check |
| `server/api/offline_status.go` | `GET /api/offline-status` endpoint |
| `frontend/js/app.js` | Offline status polling and banner display |
| `frontend/js/settings-page.js` | Offline Mode settings UI section |
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
