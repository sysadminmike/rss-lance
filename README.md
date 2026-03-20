# RSS-Lance

A self-hosted, single-user RSS reader built on **LanceDB** - an open columnar data format stored as plain files. No database server. No web server. No framework. Just files.

## What Makes This Different

### No Infrastructure

Most RSS readers need a stack: a web server (nginx/Apache), an application server (Rails, Django, Node), and a database (Postgres, MySQL, SQLite). RSS-Lance needs **none of that**.

| Traditional RSS reader | RSS-Lance |
|---|---|
| PostgreSQL / MySQL / SQLite | **None** - data is Lance files on disk or S3 |
| nginx / Apache / Caddy | **None** - Go binary serves HTTP directly |
| Application framework | **None** - standalone Go binary + Python script |
| Database backups & dumps | **None** - just copy the files |
| Connection strings & credentials | **None** - processes open files directly |

The entire data layer is just files:

```
data/
  feeds.lance/          ← feed subscriptions
  articles.lance/       ← all articles
  categories.lance/     ← folder organisation
  pending_feeds.lance/  ← queue for new feeds
```

### Not Like SQLite

People see "file-based database" and think SQLite. **This is fundamentally different.**

SQLite is a single-writer, single-file database. Only one process can write at a time, and the database is a single `.db` file that you can't read while it's being written. If two programs try to write simultaneously, one blocks or fails. You can't put a SQLite file on S3 and have two programs operate on it.

LanceDB uses **MVCC (multi-version concurrency control)** across multiple files. The Python fetcher and Go server run as independent processes writing to the same Lance tables concurrently - no coordination, no locking. Each write creates a new immutable version, and readers always see a consistent snapshot. This works on local disk, over NFS, and on S3 - the same guarantees apply everywhere.

### Two Independent Programs, One Data Directory

The **Python fetcher** (writer) and the **Go server** (reader/writer) are completely independent programs that operate on the same Lance files. They don't talk to each other - they don't even need to run on the same machine. There is no process sitting between your programs and your data. The files *are* the database.

```
  +--------------+                                     +--------------+
  | Feed Fetcher |--writes-->  data/*.lance  <--reads--| Go Server    |
  |   (Python)   |            (just files)             | (serves UI)  |
  +--------------+                                     +--------------+
        |                                               |
        |              independent processes            |
        |              no shared runtime                |
        |              no database server               |
        +------------ can run on different machines ----+
```

### Cloud Storage = Cloud Security

When your data lives on S3 or Cloudflare R2, you inherit the cloud provider's authentication and security model. There's no application-level auth to configure, no passwords to manage, no ports to firewall. Access control is handled entirely by IAM policies, bucket permissions, and pre-signed URLs - the same battle-tested infrastructure that secures everything else on AWS/Cloudflare.

This means you can:

- **Run everything on one machine** - fetcher and server side by side, Lance files in `./data`
- **Split across machines** - fetcher on a Linux server, server on your Windows laptop, Lance files on a network share (Samba/NFS)
- **Put data on S3/R2** - fetcher as an AWS Lambda, server on a Raspberry Pi, Lance files on S3 - secured by IAM, not by your app
- **Back up by copying files** - `rsync`, Syncthing, or just `cp -r data/ /backup/` - no database dumps, no export tools

### Backup with rsync / Syncthing / cp

Because the data is just files, you can back up with `rsync`, Syncthing, or plain `cp -r` - no database dumps needed. Lance files are immutable once written, so copies are always consistent. See [docs/database.md](docs/database.md#backup-with-rsync--syncthing) for details and caveats.

---

## Quick Start

### Simple (build in place)

```powershell
# Windows
git clone https://github.com/sysadminmike/rss-lance rss-lance; cd rss-lance
.\build.ps1 all
.\run.ps1 fetch-once
.\run.ps1 server
```

```bash
# Linux / macOS
git clone https://github.com/sysadminmike/rss-lance rss-lance && cd rss-lance
chmod +x build.sh
./build.sh all
./run.sh fetch-once
./run.sh server
```

### Install to a directory of your choice

Use `-Dir` (Windows) or `-d` (Linux/macOS) to build into any directory. The build script copies everything the app needs - server binary, fetcher scripts, frontend, config, and run scripts - into that directory so it is fully self-contained.

```powershell
# Windows - install to C:\Apps\rss-lance
git clone https://github.com/sysadminmike/rss-lance rss-lance; cd rss-lance
.\build.ps1 -Dir C:\Apps\rss-lance all

# Now run from the install directory
cd C:\Apps\rss-lance
.\run.ps1 fetch-once
.\run.ps1 server
```

```bash
# Linux / macOS - install to /opt/rss-lance
git clone https://github.com/sysadminmike/rss-lance rss-lance && cd rss-lance
chmod +x build.sh
./build.sh -d /opt/rss-lance all

# Now run from the install directory
cd /opt/rss-lance
./run.sh fetch-once
./run.sh server
```

Once installed, the cloned git repo is no longer needed - you can delete it to free up disk space:

```powershell
# Windows
Remove-Item -Recurse -Force C:\path\to\rss-lance   # the cloned repo, NOT the install dir
```

```bash
# Linux / macOS
rm -rf /path/to/rss-lance   # the cloned repo, NOT the install dir
```

Open **http://127.0.0.1:8080**.

> **Windows note:** If PowerShell blocks `.ps1` scripts, prefix with `powershell -ExecutionPolicy Bypass -File`, e.g. `powershell -ExecutionPolicy Bypass -File .\run.ps1 fetch-once`

The `all` command sets up the Python venv, downloads DuckDB, builds the Go server, and inserts demo feeds. After `fetch-once` populates articles, the server is ready to use.

> Both DuckDB and LanceDB can be compiled into the server binary or run as separate processes -- see [docs/building.md](docs/building.md#duckdb-and-lance-build-modes).

> **Minimal build:** If you just want the bare minimum to run the app (no tests, no demo data, no Node.js), use `minimum` instead of `all`:
> ```powershell
> .\build.ps1 minimum   # Windows
> ./build.sh minimum    # Linux / macOS
> ```
> This runs only setup → duckdb → server. You can add feeds later from the UI or command line.

---

## How It Works

### Two Independent Programs

**Feed Fetcher** (Python) - Periodically fetches RSS/Atom feeds, parses articles, writes to Lance tables. Creates all tables and manages schemas. Handles compaction (merging small data fragments). Supports daemon mode and one-shot mode (for cron).

**HTTP Server** (Go) - A single static binary that serves the browser UI and REST API directly - no nginx, Apache, or reverse proxy needed. Reads via DuckDB (an embedded SQL engine, not a separate server) with the Lance extension, writes via the lancedb-go native SDK. Never creates tables - only reads and updates existing data.

### Adaptive Fetch Frequency

Feeds are automatically fetched less often if they go quiet:

| Activity | Interval |
|---|---|
| Active (recent articles) | Every 30 min |
| Slowing (3+ days quiet) | Daily |
| Quiet (2+ weeks quiet) | Weekly |
| Dormant (2+ months quiet) | Monthly |
| Dead (6+ months quiet) | Stopped |

Feeds are promoted back to Active immediately when they publish a new article.

### Content Sanitization

Article content is cleaned at two levels to protect against XSS, tracking, and clutter:

1. **At fetch time** (Python fetcher) - dangerous HTML (scripts, iframes, event handlers, `javascript:` URIs), social sharing links, and tracking pixels are all stripped **before** content is stored in the database
2. **At display time** (JS frontend) - a second pass removes any remaining scripts, styles, event handlers, social containers, and site-navigation chrome

This defence-in-depth approach means that even if one layer is bypassed, the other still protects the user. See [docs/sanitization.md](docs/sanitization.md) for full details on what is stripped, how detection works, and what content is preserved.

### Offline Mode

If the Lance data source becomes unreachable (NFS share unmounted, S3 outage, network drop), the server automatically falls back to a local DuckDB cache. All writes are buffered through DuckDB first and flushed to Lance in the background, so reads always reflect the latest state. When the connection returns, any remaining queued changes are replayed. The local cache is always active -- no manual toggle needed. See [docs/offline.md](docs/offline.md) for details.

### DB Table Viewer

Browse raw database tables via **Other → DB Tables** in the sidebar. Select any table to see its data with Prev/Next pagination.

### Custom CSS

Override any UI styling via the built-in CSS editor. Open **Other →  Settings** in the sidebar, enter your CSS rules, and click Save - changes are applied instantly without reloading. Your custom CSS is stored in the database, so it's included in backups and follows your data if you move it to another machine. See [docs/configuration.md](docs/configuration.md#custom-css) for details.

---

## Adding Feeds

From the browser UI, click **+ Add Feed** and enter an RSS/Atom URL.

Or from the command line:

```shell
# Windows
.\.venv\Scripts\python fetcher\main.py --add "https://example.com/feed.xml"

# Linux / macOS
.venv/bin/python fetcher/main.py --add "https://example.com/feed.xml"
```

---

## Project Layout

```
rss-lance/
├── server/          Go HTTP server (API + static file serving)
├── fetcher/         Python feed fetcher daemon
├── frontend/        Browser UI (vanilla HTML/CSS/JS)
├── migrate/         Import/export scripts (OPML, TT-RSS, FreshRSS, Miniflux)
├── docs/            Build, test, and configuration docs
├── data/            Lance tables (created at runtime)
├── config.toml      Runtime configuration
├── build.ps1        Windows build script
├── build.sh         Linux/macOS build script
├── run.ps1          Windows run commands
└── run.sh           Linux/macOS run commands
```

---

## Documentation

| Topic | Link |
|---|---|
| Building from source | [docs/building.md](docs/building.md) |
| Running tests | [docs/testing.md](docs/testing.md) |
| Configuration & debug logging | [docs/configuration.md](docs/configuration.md) |
| Content sanitization | [docs/sanitization.md](docs/sanitization.md) |
| Importing & exporting | [docs/importing.md](docs/importing.md) |
| Upgrading & maintenance | [docs/upgrading.md](docs/upgrading.md) |
| Docker deployment | [docs/docker.md](docs/docker.md) |
| S3 / cloud storage | [docs/s3.md](docs/s3.md) |
| Database & concurrency | [docs/database.md](docs/database.md) |
| Logging | [docs/logging.md](docs/logging.md) |
| Offline mode | [docs/offline.md](docs/offline.md) |
| REST API & schema reference | [docs/api.md](docs/api.md) |

---

## License

[MIT](LICENSE)
