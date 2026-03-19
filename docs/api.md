# REST API

| Method | Endpoint | Description |
|---|---|---|
| GET | `/api/feeds` | List all feeds with unread counts |
| POST | `/api/feeds` | Queue a new feed `{"url": "..."}` |
| GET | `/api/feeds/:id` | Get a single feed |
| GET | `/api/feeds/:id/articles` | List articles for a feed |
| POST | `/api/feeds/:id/mark-all-read` | Mark all articles in feed as read |
| DELETE | `/api/feeds/:id` | Delete a feed (stub - returns 501) |
| GET | `/api/articles` | List all articles across feeds |
| GET | `/api/articles/:id` | Get full article content |
| POST | `/api/articles/batch` | Batch fetch articles `{"ids": [...]}` (max 100) |
| POST | `/api/articles/:id/read` | Mark article as read |
| POST | `/api/articles/:id/unread` | Mark article as unread |
| POST | `/api/articles/:id/star` | Star an article |
| POST | `/api/articles/:id/unstar` | Unstar an article |
| GET | `/api/categories` | List all categories |
| GET | `/api/settings` | Get all settings |
| GET | `/api/settings/:key` | Get a single setting |
| PUT | `/api/settings` | Batch update settings `{"key": value, ...}` |
| PUT | `/api/settings/:key` | Update a single setting `{"value": ...}` |
| GET | `/api/status` | DB status (table sizes, article counts) |
| GET | `/api/server-status` | Server runtime stats (memory, goroutines, GC, uptime, DuckDB version info) |
| GET | `/api/logs` | Query structured logs (filters: service, level, category, pagination) |
| GET | `/api/tables` | List available table names |
| GET | `/api/tables/{name}` | Raw table data with `?limit=200&offset=0` pagination |
| GET | `/api/config` | Server config exposed to frontend |
| POST | `/api/duckdb/restart` | Graceful DuckDB process restart (waits for running queries) |
| POST | `/api/duckdb/stop` | Flush cache and stop DuckDB for binary upgrade |
| POST | `/api/duckdb/start` | Start DuckDB after binary upgrade |
| POST | `/api/shutdown` | Graceful server shutdown (only when enabled) |

## Query Parameters

Article list endpoints accept: `?limit=50&offset=0&unread=true&sort=asc`

## Server CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-config` | `config.toml` | Path to config file |
| `-port` | (from config) | Override server port |
| `-debug` | (none) | Debug categories: `client,duckdb,batch,lance,all` |

## Config Endpoint

`GET /api/config` returns selected server configuration values to the frontend:

```json
{
  "show_shutdown": false
}
```

The `show_shutdown` field controls whether the frontend displays a "Stop Server" button
in the sidebar. Set `show_shutdown = true` in the `[server]` section of `config.toml`
to enable it.

## Shutdown Endpoint

`POST /api/shutdown` performs a graceful server shutdown. This endpoint is **only
registered** when `show_shutdown = true` in `config.toml`. When disabled (default),
requests to `/api/shutdown` return 404.

## Server Status Endpoint

`GET /api/server-status` returns live Go runtime stats including memory usage
(`runtime.MemStats`), goroutine count, GC pause history, server/host uptime,
build info (VCS revision, Go version), write cache stats (pending changes,
last flush time), and DuckDB external process info (Windows only -- includes
process PID, uptime, DuckDB version, and Lance extension version). Used by
the Server Status frontend page which auto-refreshes every 5 seconds.

## DuckDB Restart Endpoint

`POST /api/duckdb/restart` performs a graceful restart of the external DuckDB
process. The endpoint waits for any in-flight query to complete before stopping
the process and starting a fresh one. The DuckDB and Lance extension versions
are re-detected automatically after restart.

Returns `{"ok": true}` on success, or HTTP 500 with an error message on failure.
On platforms using embedded DuckDB (CGo mode on Linux), returns HTTP 500 with
`"DuckDB restart not supported on this platform"`.

## DuckDB Upgrade Flow (Stop/Start)

`POST /api/duckdb/stop` performs a safe DuckDB shutdown for binary upgrades:

1. Flushes the write cache (pending article read/star changes) to Lance
2. Waits for any in-flight query to complete
3. Kills the DuckDB process
4. Suppresses auto-restart so the binary can be safely replaced

Returns `{"ok": true, "message": "DuckDB stopped. Safe to replace binary."}` on success.
While stopped, the `duckdb_process` section in `/api/server-status` includes
`"stopped": true` and `"pid": 0`. API queries that need DuckDB will fall through
to the offline cache if available, or return HTTP 500 if no cached data exists.

If the server is currently offline (Lance data unreachable), the stop request is
rejected with HTTP 500 and the message `"cannot stop DuckDB for upgrade while
server is offline (write cache cannot be flushed to Lance)"`. Resolve the offline
condition first before upgrading.

`POST /api/duckdb/start` starts DuckDB after the binary has been replaced:

1. Clears the upgrade-stop flag
2. Starts a fresh DuckDB process
3. Re-detects the DuckDB and Lance extension versions

Returns `{"ok": true}` on success. Returns HTTP 500 if DuckDB is not currently
stopped for upgrade.

## Logs Endpoint

`GET /api/logs` queries structured log entries from `log_api` and `log_fetcher`
tables. Supports query parameters:

| Param | Values | Description |
|---|---|---|
| `service` | `api`, `fetcher`, or empty (both) | Filter by log source |
| `level` | `debug`, `info`, `warn`, `error` | Filter by severity |
| `category` | any category string | Filter by log category |
| `limit` | integer (default 100) | Page size |
| `offset` | integer (default 0) | Pagination offset |

## Table Viewer Endpoints

`GET /api/tables` returns the list of available table names as a JSON array:
`["articles", "feeds", "categories", "pending_feeds", "settings", "log_api", "log_fetcher"]`

`GET /api/tables/{name}` returns raw table data with pagination:

| Param | Default | Description |
|---|---|---|
| `limit` | 200 (configurable via `server.table_page_size` in Settings page) | Max rows per page (capped at 5000) |
| `offset` | 0 | Row offset for pagination |

## Schema

See [database.md](database.md) for table schemas and concurrency details.
