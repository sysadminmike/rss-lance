# AGENT.md - Hints for AI Agents & Contributors

## Project Overview

RSS-Lance is a self-hosted single-user RSS reader using LanceDB for storage.  

---

## Environment Setup

### Coding Hints
- NEVER use the em dash character. It causes problems in code/shell scripts. If you see it when working on files, change it to "-".  Also NEVER use it in md files or other documentation.
- Avoid using non standard ascii chars in code/shell scripts other than making echo/print/output nicer.
- When you update code, also update IMPLEMENTATION_PLAN.md, README.md, and AGENT.md and documentation in docs/ to keep documentation in sync, also update tests to keep them in sync.
- Check windows machine as it could have windows system for linux so bash and other gnu tools may be avaliable
- **All new features MUST include structured logging.** Every user-facing action (API endpoint, feed operation, settings change) should emit a log entry via the logging system. Use the appropriate logger: `db.log_event()` in Python, `logger.Log()`/`logger.LogJSON()` in Go. See [docs/logging.md](docs/logging.md) and the [Structured Logging System](#structured-logging-system) section below.
- **All new features MUST update `e2e_test.py`.** When adding a new feature, add E2E test checks that verify both the feature itself AND that the expected log entries were generated. Query `/api/logs` with appropriate filters to confirm log entries exist after the action.

### IMPORTANT: Do NOT run `go test` directly
The Go server requires CGo with specific linker flags (`liblancedb_go.a`, `-lws2_32`, etc.)
and MSYS2 GCC. Running `go test ./...` or `cd server; go test ./api` **will fail** with
CGo linker errors (undefined references to lancedb symbols, missing `-lws2_32`, etc.).

### IMPORTANT: There is a python virtual environment in .venv

**Always use the test scripts instead:**
```powershell
.\test.ps1 go        # Windows - runs all Go tests with correct CGo flags
.\test.ps1 api       # Windows - runs only Go API tests
./test.sh go         # Linux/macOS
```

The test scripts (`test.ps1` / `test.sh`) automatically:
1. Locate MSYS2 GCC and add it to PATH
2. Set `CGO_ENABLED=1`, `CGO_CFLAGS`, and `CGO_LDFLAGS` with the correct library paths
3. Run `go test` with the proper environment

If you need to verify Go code compiles, use `.\build.ps1 server` instead of `go build`.
This is a **hard requirement** - there is no workaround short of extracting the `Store`
interface into a CGo-free package (planned but not yet done).

### Python Virtual Environment

- **Location:** `.venv/` in project root
- **Python version:** 3.12+ (Dockerfile uses 3.12)
- **Activate (Windows PowerShell):**
  ```powershell
  Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
  .\.venv\Scripts\Activate.ps1
  ```
- **Activate (Linux/macOS/FreeBSD):**
  ```bash
  source .venv/bin/activate
  ```
- **Key packages:** `feedparser`, `lancedb`, `pandas`, `pyarrow`, `schedule`, `requests`, `tomli`
- **Install deps:** `pip install -r fetcher/requirements.txt`

### Go

- **Version:** 1.23 (per go.mod)
- **Installed via:** `winget install GoLang.Go`
- **If `go` not found after install**, refresh PATH:
  ```powershell
  $env:Path = [System.Environment]::GetEnvironmentVariable("Path","Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path","User")
  ```
- **Server module:** `server/` directory (Go module: `rss-lance/server`)
- **CGo required:** The server links against `liblancedb_go.a` (native Rust library) via CGo.
  On Windows this requires GCC from MSYS2 UCRT64 (`mingw-w64-ucrt-x86_64-gcc`).
  The `Ensure-Gcc` function in `build.ps1` auto-detects common MSYS2 paths.

### lancedb-go Native Library

The Go server uses `lancedb-go` for write operations (Update/Delete/Insert) to Lance tables.
This requires a pre-built native static library linked via CGo.

- **Pre-built library:** `server/lib/windows_amd64/liblancedb_go.a` (~350 MB, GNU archive format)
- **C header:** `server/include/lancedb.h`
- **Fork:** `server/_lancedb-go/` directory holds the forked lancedb-go source (https://github.com/sysadminmike/lancedb-go) - fork of lancedb-go v0.1.2 with:
  - `pkg/internal/table.go` line 187: `C.ulong(len(ipcBytes))` -> `C.size_t(len(ipcBytes))`
  - Fixes type mismatch: on Windows `unsigned long` is 32-bit but `size_t` is 64-bit
- **go.mod replace:** `replace github.com/lancedb/lancedb-go => github.com/sysadminmike/lancedb-go v0.0.0-20260317063623-767933bdbab9` (points to the GitHub fork, not a local path)
- **CGo flags (Windows):**
  ```
  CGO_ENABLED=1
  CGO_CFLAGS=-I<server>/include
  CGO_LDFLAGS=-static <server>/lib/windows_amd64/liblancedb_go.a -lws2_32 -luserenv -lntdll -lpthread
  ```
  The `-static` flag is **required** on Windows. Without it, the binary dynamically links
  MinGW runtime DLLs (`libwinpthread-1.dll`, `libgcc_s_seh-1.dll`) and will crash on any
  machine that doesn't have MSYS2 on PATH. With `-static`, the binary is fully self-contained.
- **CGo flags (Linux):**
  ```
  CGO_ENABLED=1
  CGO_CFLAGS=-I<server>/include
  CGO_LDFLAGS=<server>/lib/linux_amd64/liblancedb_go.a -lm -ldl -lpthread
  ```

### Rebuilding the Native Library from Rust Source

> **Rarely needed.** Only rebuild if modifying the Rust/C FFI layer or if the pre-built `.a` is missing.

The Rust source lives in `server/_lancedb-go/rust/` (Cargo.toml points to lancedb git tag v0.22.1).

**Windows prerequisites (MSYS2 UCRT64 terminal):**

These packages must be installed manually by the user in the **MSYS2 UCRT64** terminal
(not PowerShell, not regular MSYS2 MSYS). An AI agent cannot do this automatically because
MSYS2 runs in its own shell environment outside of VS Code terminals.

1. Install [MSYS2](https://www.msys2.org/) if not already installed (default path: `C:\msys64`)
2. Open **MSYS2 UCRT64** from the Start Menu (the icon has a yellow/orange stripe - not the
   purple MSYS or blue MINGW64 variants)
3. Run each line below in that terminal:
```bash
pacman -S mingw-w64-ucrt-x86_64-gcc mingw-w64-ucrt-x86_64-cmake
pacman -S mingw-w64-ucrt-x86_64-nasm mingw-w64-ucrt-x86_64-make
pacman -S mingw-w64-ucrt-x86_64-protobuf mingw-w64-ucrt-x86_64-rust
```
4. Confirm each prompt with `Y` when asked to proceed with installation
5. Close the MSYS2 terminal when done - the tools are now available at `C:\msys64\ucrt64\bin`
   and `build-native.ps1` will find them automatically

**Build:**
```powershell
cd server
.\build-native.ps1         # ~20 min first build, uses CARGO_TARGET_DIR=C:\ct
```

**Key details:**
- `CARGO_TARGET_DIR=C:\ct` is required on Windows - the `aws-lc-sys` crate creates deeply nested paths that exceed MAX_PATH (260 chars) when built inside the project tree.
- The Rust target is `x86_64-pc-windows-gnu` (not MSVC) - needed because Go's CGo uses the GNU toolchain.
- Build output (`C:\ct/`) can be kept between rebuilds for faster incremental compilation. Clean with `.\build-native.ps1 -Clean`.
- If git fetch fails during cargo build, add to `~/.cargo/config.toml`:
  ```toml
  [net]
  git-fetch-with-cli = true
  ```

---

## Build Scripts

| Platform | Script | Usage |
|---|---|---|
| Windows | `build.ps1` | `.\build.ps1 <command>` (build/setup only) |
| Windows | `run.ps1` | `.\run.ps1 <command>` (daily use) |
| Linux/FreeBSD | `build.sh` | `./build.sh <command>` (build/setup only) |
| Linux/FreeBSD | `run.sh` | `./run.sh <command>` (daily use) |

### Build Commands (build.ps1 / build.sh)

| Command | Description |
|---|---|
| `setup` | First-time setup: create venv, install deps, verify Go |
| `server` | Build Go HTTP server for current platform |
| `server-all` | Cross-compile server for Windows/Linux/macOS/FreeBSD (amd64; arm64 for Linux/macOS) |
| `fetcher` | Install Python fetcher dependencies |
| `run-fetcher` | Start the feed fetcher daemon |
| `fetch-once` | Fetch articles once and exit |
| `run-server` | Start the HTTP server |
| `demo-data` | Insert demo RSS feeds into LanceDB for testing |
| `duckdb` | Download DuckDB CLI into tools/ |
| `migrate` | One-off TT-RSS Postgres -> LanceDB import (installs psycopg2) |
| `migrate-cleanup` | Remove migrate scripts and Postgres deps after import |
| `test` | Run test suites (delegates to test.ps1/test.sh) |
| `clean` | Remove `build/` directory |
| `release` | Build server, package zip with exe + duckdb + frontend + fetcher + config + run scripts |
| `build-minimum` | Bare minimum to run the app (setup + duckdb + server). No tests, no demo data, no Node.js needed |
| `all` | Full build (setup + duckdb + server + demo-data) |
| `help` | Show available commands |

### Runtime Commands (run.ps1 / run.sh)

These are the scripts users interact with after building. Copied to `-Dir` target automatically.

| Command | Description |
|---|---|
| `fetch-once` | Fetch all due feeds once and exit |
| `fetch-daemon` | Run the fetcher continuously on a schedule |
| `server` | Start the HTTP server (http://127.0.0.1:8080) |
| `demo-data` | Insert demo RSS feeds for testing |
| `add-feed <url>` | Add a single RSS/Atom feed URL |
| `datafix` | Run retroactive article fixes (strip-chrome, strip-social) |
| `export-opml` | Export feeds to OPML 2.0 file |
| `benchmark` | Run performance benchmarks (insert, sanitize, pipeline, read) |
| `help` | Show available commands |

Options: `-DebugLog <categories>` (PS) / `--debug <categories>` (sh), `-Port <number>` / `--port <number>`

---

## Directory Layout

```
rss-lance/
|-- .venv/              # Python virtual environment (NOT in git)
|-- build/              # Compiled Go binaries (NOT in git)
|-- data/               # LanceDB tables at runtime (NOT in git)
|-- fetcher/            # Python feed fetcher daemon
|-- migrate/            # TT-RSS / FreshRSS / Miniflux / OPML migration scripts
|-- server/             # Go HTTP server
|   |-- api/            # REST API handlers
|   |-- db/             # Hybrid DuckDB (reads) + lancedb-go (writes)
|   |   |-- store.go        # Store interface + all struct types
|   |   |-- cache.go        # Write cache + CTE overlay
|   |   |-- logbuffer.go    # Buffered log writer (batch flush)
|   |   |-- lance_writer.go # Shared CUD via lancedb-go native SDK
|   |   |-- lance_windows.go # Windows: DuckDB CLI reads
|   |   +-- lance_cgo.go    # Non-Windows (Linux/FreeBSD/macOS): embedded DuckDB reads
|   |-- debug/          # Debug logging & HTTP middleware
|   |-- include/        # lancedb.h C header for CGo FFI
|   |-- lib/            # Pre-built native libraries (per-platform)
|   |   +-- windows_amd64/liblancedb_go.a
|   |-- _lancedb-go/    # Forked lancedb-go SDK source (C.ulong->C.size_t fix)
|   |   |-- pkg/        # Go bindings
|   |   +-- rust/       # Rust source (Cargo.toml -> lancedb v0.22.1)
|   |-- build-native.ps1 # Rebuild native lib from Rust (rarely needed)
|   +-- build-native.cmd # Same, for CMD
|-- frontend/           # Static HTML/CSS/JS frontend
|-- tools/              # DuckDB CLI binary (downloaded at build time)
|-- build.ps1           # Windows build script
|-- build.sh            # Linux/FreeBSD build script
|-- run.ps1             # Windows runtime commands (daily use)
|-- run.sh              # Linux/FreeBSD runtime commands (daily use)
|-- test.ps1            # Windows test runner
|-- test.sh             # Linux/FreeBSD test runner
|-- e2e_test.py         # End-to-end integration test (250 checks)
|-- benchmark.py        # Performance benchmarks (insert, sanitize, read)
|-- config.toml         # Runtime configuration (create from template)
|-- docker-compose.yml  # Docker compose (server + fetcher + tools)
|-- Dockerfile          # Multi-stage Docker build
|-- pyproject.toml      # pytest configuration
|-- .gitignore
|-- IMPLEMENTATION_PLAN.md
+-- AGENT.md            # This file
```

---

## Test Suite

The project has tests covering all three layers: Python fetcher, Go API/DB, and frontend JS.
There is also a standalone end-to-end integration test.

### Running Tests

| Script | Platform | Usage |
|---|---|---|
| `test.ps1` | Windows | `.\test.ps1 <suite>` |
| `test.sh` | Linux/macOS | `./test.sh <suite>` |

Suite options: `all`, `python`, `go`, `frontend`, `backend` (python+go), `database` (DB integration only), `api` (Go API only).

Tests also run automatically as part of `build.ps1 all` / `build.sh all`. To skip:
- Windows: `.\build.ps1 -NoTests all`
- Linux: `./build.sh --no-tests all`

### End-to-End Integration Test

`e2e_test.py` is a standalone script (separate from the unit test suite) that exercises the full stack:

1. Starts a local HTTP server serving static RSS XML (3 feeds: Alpha=3, Bravo=5, Sanitize=6 = 14 articles)
2. Populates LanceDB using the Python fetcher's DB module
3. Verifies sanitization pipeline (tracking pixels, social links, tracking params, scripts, site chrome)
4. Verifies fetcher log writes via DuckDB
5. Verifies initial data via DuckDB
6. Starts the real `rss-lance-server.exe` with a temp config
7. Hits every API endpoint like the frontend would
8. Verifies read/star state changes, pagination, sorting, filtering
9. Tests log settings, trimming (count + age modes), retention
10. Tests custom CSS settings (set, update, clear, batch)
11. Tests config endpoint and shutdown API (restart with show_shutdown=true)
12. Checks final DB state via DuckDB

**Prerequisites:** `build/rss-lance-server.exe` (run `build.ps1 server`) and `tools/duckdb.exe` (run `build.ps1 duckdb`).

**Run:**
```bash
python e2e_test.py              # normal run
python e2e_test.py --verbose    # show HTTP request/response details
python e2e_test.py --keep       # preserve temp dir for debugging
```

**250 checks** across 37 test sections covering: prerequisites, setup, local RSS server, populate data, sanitization (chrome/tracking/scripts), fetcher log writes, DuckDB verification, server startup, feed listing, single feed, article listing, articles by feed, view article, batch fetch, mark read/unread, unread filter, star/unstar, mark-all-read, multiple state changes (cache), DB status, server runtime status, final global state, categories, sorting, pagination, log settings, log trimming (count mode), log trimming (age mode), settings DB verification, custom CSS, error handling, final DuckDB verification, queue feed, logs API endpoint, config (show_shutdown), and shutdown API.

### Test File Locations

| Suite | Location | Framework | What it tests |
|---|---|---|---|
| Python fetcher | `fetcher/tests/test_*.py` | pytest | Feed parsing, config, tiers, content cleaner, DB integration (real LanceDB in temp dirs) |
| Go API | `server/api/api_test.go` | go test | All REST endpoints via mock Store (no CGo needed in test logic, but CGo required to compile because of transitive `db` import) |
| Go DB | `server/db/store_test.go` | go test | SQL escaping (8 cases), Feed/Article struct JSON field validation (6 tests) |
| Frontend | `frontend/tests/*.test.js` | Jest + jsdom | Sanitization, time formatting, feed activity, DOM structure, API patterns |
| OPML roundtrip | `migrate/test_opml_roundtrip.py` | pytest | Export -> import -> verify round-trip |
| E2E integration | `e2e_test.py` | standalone | Full-stack: 250 checks across all services |

### CGo Dependency for Go Tests

See **"Do NOT run `go test` directly"** at the top of this file. The Go API tests transitively import `lancedb-go` via `lance_writer.go`, so compiling them requires CGo (GCC + `liblancedb_go.a`), even though the tests use an in-memory mock. Always use the test scripts.

> **Future improvement:** Split the `Store` interface and types into a separate package (e.g. `db/types`) with no CGo dependency, so API tests can compile without GCC. See IMPLEMENTATION_PLAN.md for the planned approach.

### Frontend Tests (Node.js required)

Frontend tests use Jest with jsdom. Node.js is **not** required to run the app -- only for running frontend tests. If Node.js/npm is not found, the test runner skips the frontend suite.

To run frontend tests manually:
```bash
cd frontend
npm install    # one-time
npm test
```

### Test Output Format

The test runners parse output from pytest / go test / Jest and display unified `[PASS]` / `[FAIL]` / `[SKIP]` lines per test, with a summary at the end:
```
  [PASS] test_config > TestConfigLoad::test_defaults
  [PASS] api/TestListFeeds
  [FAIL] api/TestBrokenThing
         expected 200, got 500

  TEST SUMMARY
  Total:   258 tests
  Passed:  257
  Failed:  1
```

Failed tests show the error detail indented below the `[FAIL]` line so agents and humans can quickly identify what broke.

---

## Key Design Principles

### Self-contained single-directory deployment
The goal is to keep **everything in one directory**. After building, the user should have a single folder that contains the entire app: binary, frontend, config, data, and runtime scripts. No global installs beyond Go and Python (which the user provides).

**What lives in the project directory:**
- `.venv/` -- Python virtual environment with all fetcher dependencies
- `build/` -- compiled Go server binary
- `tools/` -- DuckDB CLI binary (downloaded at build time)
- `data/` -- LanceDB tables (runtime data)
- `frontend/` -- static HTML/CSS/JS (served by the Go binary)
- `fetcher/` -- Python scripts (run via `.venv/bin/python`)
- `config.toml` -- single config file
- `run.ps1` / `run.sh` -- daily-use commands

**What the user must install globally (cannot live in the directory):**
- **Python 3.12+** -- needed to run the fetcher
- **Go 1.23+** -- needed to compile the server (build time only, not runtime)
- **GCC / MSYS2** -- needed to compile the server due to CGo (build time only, not runtime)

**What is NOT checked into git (build artifacts):**
- `.venv/` -- created by `build.ps1 setup`
- `build/` -- created by `build.ps1 server`
- `tools/duckdb.exe` -- downloaded by `build.ps1 duckdb`
- `data/` -- created at runtime
- `frontend/node_modules/` -- only needed for frontend tests (Jest), not the app
- `*.exe` -- compiled binaries
- `*.log` -- test/runtime logs

**Syncthing for local backups:** For local installs, you can use [Syncthing](https://syncthing.net/) to replicate the `data/` directory between machines. Since all state is just files in `data/`, syncing that folder gives you a full backup. The Go server and Python fetcher are both read/write safe against the same Lance files.

### Self-contained after build
The app **must** be fully self-contained after `build.ps1 all` / `build.sh all`. The expected workflow is:

1. `git clone` the repo
2. Run the build script (`all` or step-by-step)
3. Optionally **delete the `.git` directory** (or the entire repo clone)
4. The app continues to work - all runtime files live in the project directory

This means **every runtime command** (`run-fetcher`, `fetch-once`, `run-server`, `demo-data`, etc.) must only reference files within the project directory itself (`fetcher/`, `frontend/`, `server/`, `config.toml`, etc.), **never** an external source checkout.

When using `-Dir` to build into a separate directory, the build script **copies** the required runtime files (`fetcher/`, `frontend/`, `config.toml`) into the target so it becomes self-contained too. The `migrate/` scripts are **not** copied during setup - they are only copied on-demand when the user runs the `migrate` command, and can be removed afterwards with `migrate-cleanup`.

### Migration tools (multi-format import/export)
The `migrate/` directory contains scripts for importing from various RSS readers and exporting:
- `import_ttrss.py` -- TT-RSS from Postgres (requires `psycopg2-binary`)
- `import_freshrss.py` -- FreshRSS via Google Reader API
- `import_miniflux.py` -- Miniflux via REST API
- `import_opml.py` -- OPML file import (feeds + folder hierarchy, no articles)
- `export_opml.py` -- Export feeds to OPML 2.0 file
- `common.py` -- Shared framework (dataclasses, write helpers, duplicate detection)
- `test_opml_roundtrip.py` -- pytest round-trip test

Migration is **not** part of normal operation:
- `migrate` command: copies scripts if needed, installs migration deps, runs the import
- `migrate-cleanup` command: deletes `migrate/` and uninstalls extra deps
- **Note:** `psycopg2-binary` is currently in `fetcher/requirements.txt` and gets installed during `setup`. Only `tqdm` is truly migrate-only.

### All state lives in LanceDB
All application state - feeds, articles, categories, read/starred status - is stored in **LanceDB tables** under the `data/` directory. There is no external database process; it's just files on disk (or S3). This means:

- **Backup** = copy the `data/` folder
- **Reset** = delete the `data/` folder
- **Migrate** = point at a new `data/` path in `config.toml`
- The Go server reads via **DuckDB + Lance extension** (SQL over Lance files)
- The Python fetcher writes via the **LanceDB Python library** (append/merge/delete)

---

## Important Notes

- **Execution policy (Windows):** PowerShell may block `.ps1` scripts. Run `Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass` first.
- **LanceDB tables** live in `data/` by default (configurable to S3 in `config.toml`). 7 tables: articles, feeds, categories, pending_feeds, settings, log_api, log_fetcher.
- **DuckDB version** 1.5.0 (downloaded by `build.ps1 duckdb` into `tools/`).
- **DuckDB Lance extension** cannot handle `UPDATE ... WHERE id IN (...)` (fails with "Lance UPDATE does not support UPDATE with joins or FROM") - this is why the Go server uses lancedb-go for writes.
- **Single-user only** - no auth layer; each user runs their own instance.
- **Cross-platform targets:** Windows amd64, Linux amd64/arm64, macOS amd64/arm64, FreeBSD amd64.
- The Go server serves static files from `frontend/` and exposes a REST API under `/api/`.
- When adding new Python dependencies, update `fetcher/requirements.txt` or `migrate/requirements.txt` accordingly.

---

## Docker

Multi-stage Dockerfile (Go 1.23 build -> Python 3.12 pip install -> Python 3.12-slim final ~150MB):
- Uses tini as init, runs as non-root `rss` user
- Volume at `/data`, exposes port 8080
- Patches config.toml for container environment (0.0.0.0, /data, /app/frontend)

`docker-compose.yml` defines 4 services:
- `server` -- Go HTTP server on port 8080
- `fetcher` -- Python fetcher daemon (continuous)
- `fetcher-once` -- One-shot fetch (tools profile)
- `demo-data` -- Insert demo feeds (tools profile)
- Shared volume `./data:/data`

---

## Benchmark

`benchmark.py` provides 4 modes:
- `insert` -- LanceDB write throughput (100-1000 articles x 10-250 feeds)
- `sanitize` -- content_cleaner pipeline timing on 1000 articles
- `pipeline` -- sanitize + insert end-to-end
- `read` -- Go server API latency (populates 1000 feeds, queries at exponential offsets + per-feed scroll)

Run via `run.ps1 benchmark <mode>` / `run.sh benchmark <mode>`.

---

## API Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/api/feeds` | List all feeds with unread counts |
| POST | `/api/feeds` | Queue a new feed URL (202 Accepted) |
| GET | `/api/feeds/:id` | Get single feed details |
| DELETE | `/api/feeds/:id` | Delete feed (stub, returns 501) |
| GET | `/api/feeds/:id/articles` | List articles for a feed |
| POST | `/api/feeds/:id/mark-all-read` | Mark all articles in feed as read |
| GET | `/api/articles/` | List all articles (supports ?unread=true, ?sort=asc/desc, ?limit, ?offset) |
| GET | `/api/articles/:id` | Get single article with content |
| POST | `/api/articles/batch` | Fetch multiple articles by ID |
| POST | `/api/articles/:id/read` | Mark article as read |
| POST | `/api/articles/:id/unread` | Mark article as unread |
| POST | `/api/articles/:id/star` | Star article |
| POST | `/api/articles/:id/unstar` | Unstar article |
| GET | `/api/categories` | List categories |
| GET | `/api/settings` | Get all settings |
| PUT | `/api/settings` | Batch update settings |
| GET | `/api/settings/:key` | Get single setting |
| PUT | `/api/settings/:key` | Set single setting |
| GET | `/api/status` | DB diagnostics (table sizes, row counts) |
| GET | `/api/server-status` | Go runtime stats (memory, GC, goroutines, uptime, write cache) |
| GET | `/api/server-status/history` | Time-series metrics (5s samples, 60min retention) |
| GET | `/api/logs` | Combined logs with filters (?service, ?level, ?category, ?limit, ?offset) |
| GET | `/api/tables/:name` | Raw table browser (articles, feeds, categories, pending_feeds, settings, log_api, log_fetcher) |
| GET | `/api/config` | Public runtime config (show_shutdown flag) |
| POST | `/api/shutdown` | Graceful shutdown (only when show_shutdown=true in config.toml) |
| GET | `/css/custom.css` | Serves custom CSS from settings |

---

## Structured Logging System

RSS-Lance has a structured logging system with separate log tables per service, a unified
schema so they can be combined via DuckDB `UNION ALL`, and per-category toggles in the UI.

### Log Table Schema

Both `log_api` and `log_fetcher` Lance tables share the same schema:

| Column     | Type             | Description                            |
|------------|------------------|----------------------------------------|
| log_id     | string (UUID)    | Unique identifier                      |
| timestamp  | timestamp (us)   | When the event occurred (UTC)          |
| level      | string           | `debug`, `info`, `warn`, or `error`    |
| category   | string           | Grouped category name (see below)      |
| message    | string           | Human-readable description             |
| details    | string           | Optional JSON blob with structured data|
| created_at | timestamp (us)   | When the row was written               |

### Log Categories

**Fetcher** (`log_fetcher` table, written by Python fetcher):

| Category            | Setting key                        | Description                           |
|---------------------|------------------------------------|---------------------------------------|
| fetch_cycle         | log.fetcher.fetch_cycle            | Fetch cycle summaries                 |
| feed_fetch          | log.fetcher.feed_fetch             | Each feed fetched + article count     |
| article_processing  | log.fetcher.article_processing     | Debug: each article processed         |
| compaction          | log.fetcher.compaction             | Table compaction events               |
| tier_changes        | log.fetcher.tier_changes           | Feed tier up/downgrades               |
| errors              | log.fetcher.errors                 | Fetch errors and failures             |

**API Server** (`log_api` table, written by Go server):

| Category           | Setting key                         | Description                           |
|--------------------|-------------------------------------|---------------------------------------|
| lifecycle          | log.api.lifecycle                   | Server start/stop events              |
| requests           | log.api.requests                    | All API requests (verbose)            |
| settings_changes   | log.api.settings_changes            | When settings are modified            |
| feed_actions       | log.api.feed_actions                | Add feed, mark-all-read, etc.         |
| article_actions    | log.api.article_actions             | Read/star individual articles         |
| errors             | log.api.errors                      | Error responses                       |

### How to Add Logging

**In Python (fetcher)**:
```python
# Use db.log_event(level, category, message, details_json)
db.log_event("info", "feed_fetch", f"Fetched {title}: {count} new articles",
             json.dumps({"feed_id": fid, "new": count}))

# Debug-level events are only written if the category is enabled
db.log_event("debug", "article_processing", f"Processing: {article_title}",
             json.dumps({"article_id": aid, "guid": guid}))
```
The fetcher checks `_should_log(category)` before writing. Settings are cached at startup.

**In Go (API server)**:
```go
// Use the ServerLogger from api/logs.go
logger.Log("info", "lifecycle", "Server started on "+addr, "")
logger.LogJSON("info", "feed_actions", "Feed queued: "+url,
    map[string]any{"url": url, "category_id": catID})
```
The server logger checks settings before writing. Writes are async (goroutine).

### Adding a New Log Category

1. Add a default setting in `fetcher/db.py` `DEFAULT_SETTINGS` (e.g. `"log.fetcher.my_category": True`)
2. Add the toggle to the settings page in `frontend/js/settings-page.js` (in the `logGroups` array)
3. Use `db.log_event(level, "my_category", ...)` in the fetcher or `logger.Log(level, "my_category", ...)` in the server
4. Update this table in AGENT.md and `docs/logging.md`
5. **Add log verification to `e2e_test.py`** - after the action that produces the log, query `/api/logs?category=my_category` and check the expected entry exists

### Checklist for New Features

Every new feature should include:

- [ ] **Logging calls** - emit appropriate log entries for the new actions
- [ ] **E2E test checks** - verify the feature works AND that log entries appear via `/api/logs`
- [ ] **Documentation** - update AGENT.md, IMPLEMENTATION_PLAN.md, and relevant docs/ files
- [ ] **Settings toggle** (if new category) - add default in `db.py` + toggle in `settings-page.js`

### Log Query API

`GET /api/logs` returns combined logs from all services via DuckDB `UNION ALL`.

| Query Param | Values                              | Default |
|-------------|-------------------------------------|---------|
| service     | `api`, `fetcher`, or empty (all)    | all     |
| level       | `debug`, `info`, `warn`, `error`    | all     |
| category    | any category name                   | all     |
| limit       | integer                             | 100     |
| offset      | integer                             | 0       |

Response: `{ "entries": [...], "total": N, "limit": N, "offset": N }`

### Log Retention

The setting `log.retention_mode` (default `"count"`) controls how logs are trimmed:
- **count mode**: `log.max_entries` (default 10000) caps entries per table. 0 = retain all.
- **age mode**: `log.max_age_days` (default 30) deletes entries older than N days.

Each service trims only its own table:
- The fetcher trims `log_fetcher` after each fetch cycle.
- The Go server trims `log_api` every 5 minutes via a background goroutine.

### UI Pages

- **Settings page** (Settings): has toggle switches for each log category, grouped by service, with a master enable/disable per service.
- **Logs page** (System Logs in sidebar): shows combined logs with filters for service, level, and limit. Click a row to expand its details JSON. Supports pagination and auto-refresh.
