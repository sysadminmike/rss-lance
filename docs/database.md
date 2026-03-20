# Database Schema & Concurrency

For an overview of how LanceDB and DuckDB work together (with ASCII diagrams
of the read/write data flow, offline mode, cache layers, and the lance writer),
see [architecture.md](architecture.md).

## Timestamp Convention

All tables include `created_at` and `updated_at` columns:

- **`created_at`**: Set to current UTC time on insert. Never updated.
- **`updated_at`**: Set to current UTC time on insert and refreshed to UTC now on every update.
- **Type**: `timestamp[us]` (microsecond precision, no timezone annotation). Values are always UTC.
- **Management**: Application-managed - the Python fetcher and Go server both set these explicitly.

## Tables

### feeds.lance

| Column | Type | Description |
|---|---|---|
| `feed_id` | string | UUID primary key |
| `title` | string | Feed title |
| `url` | string | RSS/Atom feed URL |
| `site_url` | string | Website URL |
| `icon_url` | string | Favicon URL |
| `category_id` | string | FK → categories |
| `subcategory_id` | string | Sub-category reference |
| `last_fetched` | timestamp | Last fetch attempt |
| `last_article_date` | timestamp | Newest article date |
| `fetch_interval_mins` | int32 | Minutes between fetches |
| `fetch_tier` | string | `active` / `slowing` / `quiet` / `dormant` / `dead` |
| `tier_changed_at` | timestamp | When tier last changed |
| `last_successful_fetch` | timestamp | Last successful fetch |
| `error_count` | int32 | Consecutive failures |
| `last_error` | string | Most recent error |
| `created_at` | timestamp | When added |
| `updated_at` | timestamp | Last modification |

### articles.lance

| Column | Type | Description |
|---|---|---|
| `article_id` | string | UUID primary key |
| `feed_id` | string | FK → feeds |
| `title` | string | Headline |
| `url` | string | Permalink |
| `author` | string | Author name |
| `content` | string | Full HTML content |
| `summary` | string | Short description |
| `published_at` | timestamp | Publication date |
| `fetched_at` | timestamp | When fetcher downloaded this |
| `is_read` | bool | Read status |
| `is_starred` | bool | Starred status |
| `guid` | string | RSS guid / Atom id (dedup key) |
| `schema_version` | int32 | Schema version at write time |
| `created_at` | timestamp | When record was created |
| `updated_at` | timestamp | Last modification |

### categories.lance

| Column | Type | Description |
|---|---|---|
| `category_id` | string | UUID primary key |
| `name` | string | Display name |
| `parent_id` | string | FK → self for nesting |
| `sort_order` | int32 | UI ordering hint |
| `created_at` | timestamp | When added |
| `updated_at` | timestamp | Last modification |

### pending_feeds.lance

| Column | Type | Description |
|---|---|---|
| `url` | string | RSS/Atom URL to subscribe to |
| `category_id` | string | Optional category |
| `requested_at` | timestamp | When the user clicked "Add Feed" |
| `created_at` | timestamp | When record was created |
| `updated_at` | timestamp | Last modification |

### settings.lance

| Column | Type | Description |
|---|---|---|
| `key` | string | Setting key (primary key) |
| `value` | string | JSON-encoded value |
| `created_at` | timestamp | When setting was first created |
| `updated_at` | timestamp | Last modification |

## Concurrency & Multi-Process Access

### Why Not SQLite?

SQLite is often the go-to for "file-based database", but it has fundamental
limitations that make it unsuitable for this architecture:

- **Single writer** - SQLite uses file-level locking. Only one process can write
  at a time; concurrent writers block or fail with `SQLITE_BUSY`.
- **No network storage** - SQLite requires a local POSIX filesystem. You cannot
  safely open a SQLite database over NFS, Samba, or S3. The FAQ explicitly
  warns against this.
- **Monolithic file** - the entire database is one `.db` file. Copying it while
  a writer is active can produce a corrupt backup. Tools like `.backup` or WAL
  checkpointing exist, but add complexity.
- **No concurrent cross-process reads during writes** - readers can be blocked
  by writers depending on journal mode.

Lance solves all of these. Multiple independent processes - even on different
machines - can read and write the same tables concurrently because Lance uses
**MVCC (Multi-Version Concurrency Control)** with optimistic concurrency and
automatic conflict resolution.

### How it works

The fetcher (Python) and server (Go) run as **separate processes** sharing the
same Lance tables on disk (or over NFS / S3).

Each write creates a new immutable table version. On commit, Lance atomically
writes a manifest file using `put-if-not-exists` (or `rename-if-not-exists`).
If two writers race, only one succeeds; the other detects the conflict and
either rebases or retries automatically depending on the operation types.

### Why our access pattern is safe

| Process | Table | Operation | Lance txn type |
|---------|-------|-----------|----------------|
| Fetcher | articles | Adds new rows | **Append** |
| Fetcher | feeds | Updates metadata (last_fetched, error_count, …) | **Update** |
| Server | articles | Updates `is_read` / `is_starred` | **Update** |
| Server | feeds | Reads only | **Read** |

Key compatibility rules from the
[Lance Transaction Specification](https://lance.org/format/table/transaction/):

- **Append + Append** - always compatible, no conflict.
- **Append + Update** - compatible (new fragments vs. existing fragments).
- **Update + Update on different rows** - automatically rebaseable (deletion
  masks are merged).
- **Reads** - always safe; MVCC gives each reader a consistent snapshot.

Because the fetcher only *appends* articles and the server only *updates*
`is_read`/`is_starred` on existing articles, the two processes never touch
overlapping rows in the same table and will never conflict.

### Do I need DynamoDB or an external lock manager?

**No.** An external manifest store (e.g. DynamoDB) is only needed when the
backing object store lacks atomic write primitives.

**Why DynamoDB?** Lance commits work by atomically writing a new manifest file
(e.g. `42.manifest`). On a local filesystem this uses `rename-if-not-exists`;
on S3 it uses `PUT-IF-NONE-MATCH` (conditional PUT). If two writers race, the
atomic operation guarantees exactly one wins. But some object stores (older S3,
R2, B2) don't support conditional writes at all - there's no way to say "only
write this if it doesn't already exist". Without that primitive, two writers
could both believe they successfully committed version 42, corrupting the table.
DynamoDB (or any key-value store with `put-if-not-exists`) fills this gap: Lance
writes the manifest path to DynamoDB first using a conditional put, and only the
winner proceeds. It's purely a commit-coordination mechanism - DynamoDB stores
only a single pointer per table version, not any actual data.

| Storage | Atomic ops? | External store needed? | Notes |
|---------|-------------|------------------------|-------|
| Local filesystem | Yes (rename) | No | |
| NFS | Yes | No | Supports atomic rename across clients |
| Samba (SMB/CIFS) | Yes | No | Supports atomic rename; works for LAN setups |
| SSHFS (FUSE) | **Maybe** | No | See caveat below |
| AWS S3 | Yes (`PUT-IF-NONE-MATCH`, added 2024) | No | Requires a recent SDK that sends the conditional header |
| S3 (old SDK / pre-2024) | No | Yes - use DynamoDB | Legacy path; upgrade SDK if possible |
| MinIO | Yes | No | Supports `PUT-IF-NONE-MATCH` (conditional writes) since RELEASE.2023-09-07 |
| Cloudflare R2 | **No** | Yes - use DynamoDB | R2 does not support conditional `PUT`; needs an external manifest store |
| Backblaze B2 | **No** | Yes - use DynamoDB | No conditional write support |
| Google GCS | Yes (`If-None-Match`) | No | Natively supports conditional object writes |
| Azure Blob | Yes (conditional headers) | No | Natively supports conditional object writes |

#### SSHFS / FUSE filesystems

SSHFS mounts a remote directory over SSH using FUSE. **It will generally work**
for a single-writer scenario, but behaviour under concurrent writers from
multiple machines depends on:

- **`rename` atomicity** - SSHFS translates `rename()` into an SFTP rename,
  which is atomic on the remote filesystem (e.g. ext4, XFS). So Lance's
  `rename-if-not-exists` commit primitive should succeed.
- **Metadata caching** - SSHFS aggressively caches `stat`/`readdir` results by
  default. A second process may not immediately see a new manifest file. Mount
  with `-o cache=no` or `-o cache_timeout=0` to disable this.
- **No server-side locking** - Unlike NFS, SSHFS has no lock manager. This is
  fine because Lance uses atomic file operations rather than advisory locks.


For the default local-disk, NFS, or Samba deployment, no additional
infrastructure is required.

### Cloud Storage = Cloud Security

When Lance tables live on S3 or R2, there is no application-level authentication
to worry about. Access control is handled entirely by the cloud provider:

- **AWS S3** - IAM roles/policies control who can read/write the bucket. The
  fetcher and server just need valid AWS credentials (environment variables,
  instance profiles, or IRSA). No passwords, no tokens, no API keys in
  `config.toml`.
- **Cloudflare R2** - uses S3-compatible API tokens and bucket-level
  permissions.
- **GCS / Azure** - their native IAM and conditional-write support work
  directly with Lance.

This means the security perimeter is your cloud IAM policy, not your
application code. You don't need firewalls, VPNs, or reverse proxies to protect
your data - the bucket policy is the firewall.

### Backup with rsync / Syncthing

Because the data is just files, you can use standard file-sync tools to keep copies across machines:

```bash
# rsync to a backup server
rsync -av --delete ./data/ user@backup-server:/backups/rss-lance/

# Or use Syncthing to keep two machines in sync automatically
# Just point Syncthing at the data/ folder on both machines
#
# WARNING: Syncthing is file-sync, NOT a shared filesystem. Only one
# rss-lance instance (fetcher + server pair) should write to a given
# dataset at a time. The backup copy is for disaster recovery, not
# for running a second reader/writer. Lance's MVCC concurrency model
# relies on atomic file operations on a shared filesystem (NFS, Samba,
# S3) - Syncthing's eventual-consistency replication cannot provide
# this. If two instances write independently and Syncthing merges the
# files, the manifest history will diverge and the table may corrupt.
```

Lance files are immutable once written - a new version never modifies existing
files, so `rsync` / Syncthing always copies consistent data. Compare this with
SQLite, where copying the `.db` file mid-transaction can produce a corrupt
backup.


## Server Database Architecture (Go Server)

The Go server uses a layered read/write architecture where **DuckDB serves as the
SQL read engine and crash-safe write buffer**, while **Lance is the durable
persistence layer that all writes ultimately land in**.

```
User action (mark read, star, settings)
        │
        ▼
  ┌─────────────────────────────────────────────────────┐
  │  In-Memory Write Cache (writeCache)                 │
  │  map[article_id] → {is_read, is_starred}            │
  │  Immediate visibility for the current request       │
  └────────────────────────┬────────────────────────────┘
                           │ also written synchronously
                           ▼
  ┌─────────────────────────────────────────────────────┐
  │  DuckDB offline_cache.db  (pending_changes table)   │
  │  Persistent write buffer — survives server restart  │
  │  Queued here until Lance is reachable               │
  └────────────────────────┬────────────────────────────┘
                           │ flushed every 30 s (background goroutine)
                           ▼
  ┌─────────────────────────────────────────────────────┐
  │  Lance tables (lancedb-go native SDK)               │
  │  articles.lance  feeds.lance  settings.lance …      │
  │  Source of truth — durable, MVCC-versioned files    │
  └─────────────────────────────────────────────────────┘
```

### DuckDB as SQL Read Engine

All **reads** go through DuckDB using the Lance extension, which lets DuckDB query
`.lance` format files directly. This gives the server full SQL — JOINs, CTEs,
aggregations, `LIMIT`/`OFFSET` pagination — over Lance tables without loading them
into memory.

On startup, DuckDB runs:

```sql
LOAD lance;
ATTACH IF NOT EXISTS '/path/to/data' AS _lance (TYPE LANCE);
```

Queries then reference tables as `_lance.main.articles`, `_lance.main.feeds`, etc.

#### DuckDB Process Mode

On Windows (and Linux/macOS when compiled with `-tags duckdb_cli`), DuckDB runs as
a **persistent subprocess** (`duckdb.exe :memory: -json`). A background goroutine
pipes SQL in over stdin and reads JSON results from stdout. This amortises the
~600 ms process-startup cost across all queries; one DuckDB process handles all
reads for the lifetime of the server.

On Linux/macOS (default), DuckDB is embedded directly via **CGo** (`go-duckdb`)
and called in-process.

Both modes expose the same `Store` interface; the rest of the server has no
visibility into which is running.

### In-Memory Write Cache (Immediate Read-Your-Writes)

When a user marks an article read or stars it, the state change is written to
Lance asynchronously (every 30 seconds). To prevent the UI from flickering back
to the old value while the flush is pending, an in-memory overlay is applied to
every read query.

`writeCache` holds a map of pending overrides:

```go
type writeCache struct {
    pending map[string]*articleOverride // article_id → {is_read, is_starred}
}
```

`pendingCTE()` serialises the map into a SQL CTE that DuckDB inlines at query time:

```sql
WITH _cache AS (
    SELECT * FROM (VALUES
        ('article-123', true,  NULL),   -- pending is_read = true
        ('article-456', NULL,  true)    -- pending is_starred = true
    ) AS t(article_id, is_read, is_starred)
)
SELECT
    a.article_id, a.title,
    COALESCE(c.is_read,    a.is_read)    AS is_read,
    COALESCE(c.is_starred, a.is_starred) AS is_starred
FROM _lance.main.articles a
LEFT JOIN _cache c ON a.article_id = c.article_id
WHERE ...
```

`COALESCE` prefers the in-memory cache value; if no override exists for a row the
Lance value is used unchanged. The overlay is cleared after the next successful
flush to Lance.

### DuckDB as Crash-Safe Write Buffer

The in-memory cache is not enough on its own — if the server restarts, pending
changes would be lost. Every write is therefore also buffered into a **local DuckDB
file** (`offline_cache.db`, configurable via `duckdb_path`) in a
`pending_changes` table:

```sql
CREATE TABLE pending_changes (
    id         INTEGER PRIMARY KEY,
    article_id VARCHAR,
    action     VARCHAR,  -- 'read' | 'unread' | 'star' | 'unstar' |
                         -- 'mark_all_read' | 'setting'
    value      VARCHAR,
    timestamp  VARCHAR
);
```

This file lives on a **local filesystem** even when Lance tables are on NFS or S3.
DuckDB needs exclusive file locks and is unreliable on network storage; keeping
`offline_cache.db` local avoids that constraint entirely.

On restart, the server replays `pending_changes` back to Lance before accepting new
requests, so no writes are ever silently lost.

### Lance as the Persistence Layer (Writes via lancedb-go)

Once the 30-second flush timer fires, `offline_cache.Replay()` collapses all
pending changes to their final state (multiple mark-read/unread events for the same
article reduce to one) and calls `lanceWriter`, which uses the **lancedb-go native
SDK** to write directly to Lance tables.

DuckDB's Lance extension is **read-only** from the server's perspective — it cannot
do UPDATE with joins or subqueries. All mutations go through `lanceWriter`:

| Method | Operation | Lance primitive |
|--------|-----------|-----------------|
| `SetArticleRead` / `SetArticleStarred` | Mark individual articles | `table.Update(filter, values)` |
| `MarkAllRead(feedID)` | Mark all articles in a feed | `table.Update(filter, values)` |
| `FlushOverrides(overrides)` | Batch article state flush | Grouped `table.Update` per unique payload |
| `PutSetting(key, val)` | Write a settings row | `table.Update` |
| `InsertLogs(entries)` | Append log rows | `table.Add(batch)` |

`FlushOverrides` groups articles by identical `{is_read, is_starred}` payload to
minimise the number of Lance write operations (and S3 PUT costs if using cloud
storage).

### Offline / Disconnected Mode

If Lance becomes unreachable (NFS goes offline, S3 credentials expire, filesystem
full), the server transitions to **offline mode** automatically and continues
serving reads from a cached snapshot.

#### Offline Detection

A health probe runs every 30 seconds (5 seconds while already offline):

```
probeLance() → SELECT 1 FROM feeds LIMIT 1
  success → if was offline: handleReconnect()
  failure → goOffline() → mark isOffline = true
```

#### Reads While Offline

When `isOffline` is true, `GetArticles`, `GetFeeds`, etc. switch to
`offlineCache.getArticles(…)` which queries `cached_articles`, `cached_feeds`, and
`cached_categories` tables inside `offline_cache.db`. These are populated from a
periodic **snapshot** taken while Lance is reachable (configurable interval).
Writes still queue into `pending_changes` as normal.

#### 3-Tier Log Write Fallback

Log writes use a cascading fallback chain:

| Tier | Storage | When used |
|------|---------|-----------|
| 1 | `log_api.lance` via lancedb-go | Normal (Lance reachable) |
| 2 | `cached_logs` table in `offline_cache.db` | Lance write fails |
| 3 | In-process `logBuffer` ring (capped) | DuckDB also unavailable |

When Lance comes back online, the server drains Tier 2 (`cached_logs`) into Lance
automatically.

#### Reconnect & Replay

When the health probe detects Lance is reachable again:

1. `Replay()` reads all rows from `pending_changes` in `offline_cache.db`.
2. Collapses multi-event sequences per article/setting to final state.
3. Flushes to Lance via `lanceWriter`.
4. Clears `pending_changes` and the in-memory write cache.
5. Takes a fresh snapshot of Lance data into `offline_cache.db`.
6. Drains any logs queued in `cached_logs` to `log_api.lance`.

No user interaction is required; the transition back online is fully automatic.

---

## How the Fetcher Writes to DB Tables

The fetcher runs as a Python process (daemon or one-shot via `--once`). Each
fetch cycle follows this sequence:

### 1. Determine which feeds are due

`get_feeds_due()` reads **feeds.lance** and compares each feed's
`last_fetched` + `fetch_interval_mins` against the current time. Feeds with
tier `dead` are skipped. Settings like tier thresholds and intervals come from
**settings.lance**.

### 2. Fetch feeds concurrently

Up to `max_concurrent` feeds are fetched in parallel via a thread pool. For
each feed, `fetch_one()`:

1. **Downloads** the RSS/Atom XML using `requests` (via `feed_parser.py`).
2. **Parses** entries with `feedparser`, sanitises HTML (strips tracking
   pixels, dangerous tags, social share links, tracking URL params).
3. **Deduplicates** - reads existing `guid` values from **articles.lance** for
   that feed and drops any articles already stored.
4. **Strips site chrome** - compares the HTML of multiple articles from the
   same feed and removes repeated boilerplate (nav bars, related posts, etc.).

### 3. Batched writes

To minimise Lance version churn (and S3 PUT costs), the fetcher batches all
writes for the entire cycle:

- `begin_batch()` - enters batching mode.
- `add_articles()` - buffers new article rows in memory.
- `update_feed_after_fetch()` - queues per-feed metadata updates
  (`last_fetched`, `error_count`, tier, etc.).
- `flush_batch()` - at the end of the cycle, does **one** `table.add()` for
  all articles (a single Lance append) and then applies each queued feed
  update.

### 4. Post-cycle maintenance

- **Compaction** - `compact_if_needed()` checks each table's fragment count
  against thresholds from **settings.lance** and runs `compact_files()` +
  `cleanup_old_versions()` when exceeded.
- **Log trimming** - `trim_logs()` caps **log_fetcher** at `log.max_entries`.
- **Tier changes** - if a feed hasn't had new articles for long enough, its
  `fetch_tier` is downgraded (active → slowing → quiet → dormant → dead),
  increasing the `fetch_interval_mins` each time. A feed that receives new
  articles is immediately promoted back to `active`.

### 5. Logging

Throughout the cycle, `log_event()` buffers log entries and
`flush_log_batch()` appends them to **log_fetcher.lance** in a single write.

## The pending_feeds Queue

`pending_feeds` is a **cross-process message queue** that decouples the UI
from the fetcher:

| Step | Who | What |
|------|-----|------|
| 1 | User | Clicks "Add Feed" in the frontend |
| 2 | Go server | `POST /api/feeds` → `QueueFeed()` → inserts a row into **pending_feeds.lance** with the URL and timestamp |
| 3 | Python fetcher | On its next `run_once()` cycle, reads **pending_feeds.lance**, creates a proper **feeds.lance** row for each URL, does the initial fetch, and deletes the pending row |

This design exists because the Go API server is a **read-heavy** process - it
serves the UI and shouldn't be doing slow network fetches. The Python fetcher
is the only process that creates feeds and writes articles, keeping the write
path simple and avoiding Lance write conflicts between processes.

The flow is:

```
Browser → POST /api/feeds → Go server inserts into pending_feeds
                                          ↓
                              Python fetcher picks up on next cycle
                                          ↓
                              Fetches RSS, creates feed row, stores articles
                                          ↓
                              Deletes the pending_feeds row
```

## Query Escaping

The Python fetcher builds LanceDB filter expressions dynamically (e.g. `feed_id = '<value>'`).
To prevent filter injection, all values passed into these expressions are validated
or escaped by `_escape_filter_value()` in `fetcher/db.py`:

- Values matching the expected UUID/hex pattern (`^[a-fA-F0-9-]+$`) are passed through directly
- All other values have single quotes escaped (`'` -> `''`) to prevent breaking out of string literals

This applies to all `table.update()` and `table.delete()` filter strings in `db.py`, `main.py`,
and `datafix.py`.

The Go server uses parameterised DuckDB queries where possible and `escapeSQLString()` in
`server/db/store.go` for SQL string literals.

## GUID Collision Prevention

Each article has a `guid` field used for deduplication. The fetcher derives it in
`fetcher/feed_parser.py` using this priority:

1. RSS `<guid>` or Atom `<id>` element (globally unique by spec)
2. Article `<link>` URL (usually unique per feed)
3. **Fallback hash:** `sha1(feed_id + title + published)` -- the `feed_id` is included as a
   salt so articles from different feeds with the same title and date do not collide

---

## Table Field Reference

### feeds.lance

| Column | Type | Description |
|---|---|---|
| `feed_id` | string | UUID primary key |
| `title` | string | Feed title |
| `url` | string | RSS/Atom feed URL |
| `site_url` | string | Website URL |
| `icon_url` | string | Favicon URL |
| `category_id` | string | FK → categories |
| `subcategory_id` | string | Sub-category reference |
| `last_fetched` | timestamp | Last fetch attempt |
| `last_article_date` | timestamp | Newest article date |
| `fetch_interval_mins` | int32 | Minutes between fetches |
| `fetch_tier` | string | `active` / `slowing` / `quiet` / `dormant` / `dead` |
| `tier_changed_at` | timestamp | When tier last changed |
| `last_successful_fetch` | timestamp | Last successful fetch |
| `error_count` | int32 | Consecutive failures |
| `last_error` | string | Most recent error |
| `created_at` | timestamp | When added |
| `updated_at` | timestamp | Last modification |

### articles.lance

| Column | Type | Description |
|---|---|---|
| `article_id` | string | UUID primary key |
| `feed_id` | string | FK → feeds |
| `title` | string | Headline |
| `url` | string | Permalink |
| `author` | string | Author name |
| `content` | string | Full HTML content |
| `summary` | string | Short description |
| `published_at` | timestamp | Publication date |
| `fetched_at` | timestamp | When fetcher downloaded this |
| `is_read` | bool | Read status |
| `is_starred` | bool | Starred status |
| `guid` | string | RSS guid / Atom id (dedup key) |
| `schema_version` | int32 | Schema version at write time |
| `created_at` | timestamp | When record was created |
| `updated_at` | timestamp | Last modification |

### categories.lance

| Column | Type | Description |
|---|---|---|
| `category_id` | string | UUID primary key |
| `name` | string | Display name |
| `parent_id` | string | FK → self for nesting |
| `sort_order` | int32 | UI ordering hint |
| `created_at` | timestamp | When added |
| `updated_at` | timestamp | Last modification |

### pending_feeds.lance

| Column | Type | Description |
|---|---|---|
| `url` | string | RSS/Atom URL to subscribe to |
| `category_id` | string | Optional category |
| `requested_at` | timestamp | When the user clicked "Add Feed" |
| `created_at` | timestamp | When record was created |
| `updated_at` | timestamp | Last modification |

### settings.lance

| Column | Type | Description |
|---|---|---|
| `key` | string | Setting key (primary key) |
| `value` | string | JSON-encoded value |
| `created_at` | timestamp | When setting was first created |
| `updated_at` | timestamp | Last modification |

### log_api.lance / log_fetcher.lance

| Column | Type | Description |
|---|---|---|
| `log_id` | string | UUID primary key |
| `timestamp` | timestamp | When the event occurred |
| `level` | string | `error` / `warn` / `info` / `debug` |
| `category` | string | Event category (see logs documentation) |
| `message` | string | Human-readable log message |
| `details` | string | JSON-encoded extra context |
| `created_at` | timestamp | When record was written |