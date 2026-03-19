# AGENT.md - Hints for AI Agents & Contributors

## Project Overview

RSS-Lance is a self-hosted single-user RSS reader using LanceDB for storage.  

---

## Environment Setup

### Coding Hints
- **NEVER use non-ASCII characters in code, shell scripts** This includes em dashes, curly quotes, ellipsis characters, multiplication signs, arrows, checkmarks, etc. They cause encoding problems in PowerShell scripts, shell scripts, TOML files, and Go source. Use only plain ASCII equivalents: `-` or `--` instead of em/en dashes, `*` or `x` instead of multiplication signs, `->` instead of arrows, `...` (three dots) instead of ellipsis. The ONLY exception is echo/print/Write-Host output strings where UTF-8 characters are acceptable for display purposes (e.g. progress indicators, emoji in status messages). If you see non-ASCII characters when editing files, replace them with ASCII equivalents immediately.
- **NEVER use em dashes in documentation**.
- When you update code, also update IMPLEMENTATION_PLAN.md, README.md, and AGENT.md and documentation in docs/ to keep documentation in sync, also update tests to keep them in sync.
- Check windows machine as it could have windows system for linux so bash and other gnu tools may be avaliable
- **For installing software on Windows, ask the user directly.** Provide the download link, explain what the tool is for in one sentence, and let the user install it. This is faster than trying multiple automated approaches. For example: "Please install MSYS2 from https://www.msys2.org/ -- it provides GCC needed to compile the Go server via CGo."
- **All new features MUST include structured logging.** Every user-facing action (API endpoint, feed operation, settings change) should emit a log entry via the logging system. Use the appropriate logger: `db.log_event()` in Python, `logger.Log()`/`logger.LogJSON()` in Go. See [docs/logging.md](docs/logging.md) and the [Structured Logging System](#structured-logging-system) section below.
- **All new features MUST update `e2e_test.py`.** When adding a new feature, add E2E test checks that verify both the feature itself AND that the expected log entries were generated. Query `/api/logs` with appropriate filters to confirm log entries exist after the action.

### IMPORTANT: Do NOT run `go test` directly

The Go server requires CGo with specific linker flags (`liblancedb_go.a`, `-lws2_32`, etc.)
and MSYS2 GCC. Running `go test ./...` or `cd server; go test ./api` **will fail** with
CGo linker errors (undefined references to lancedb symbols, missing `-lws2_32`, etc.).

**Similarly, do NOT run `go build` directly.** Always use `build.ps1 server` or `build.sh server`
which set up the correct CGo environment automatically.

**Always use the build/test scripts instead:**
```powershell
.\build.ps1 server   # Windows - build with correct CGo flags
.\test.ps1 go        # Windows - runs all Go tests with correct CGo flags
.\test.ps1 api       # Windows - runs only Go API tests
./test.sh go         # Linux/macOS
```

The build and test scripts (`build.ps1` / `test.ps1` / `test.sh`) automatically:
1. Locate MSYS2 GCC and add it to PATH
2. Set `CGO_ENABLED=1`, `CGO_CFLAGS`, and `CGO_LDFLAGS` with the correct library paths
3. Run `go build` or `go test` with the proper environment

This is a **hard requirement** - there is no workaround short of extracting the `Store` interface into a CGo-free package (planned but not yet done).

Do NOT delete `server/lib/windows_amd64/liblancedb_go.a` -- rebuilding it from Rust source takes ~20 minutes. Avoid `clean` operations or file deletions that would remove this pre-built static library. If the file already exists, preserve it.

### IMPORTANT: Python virtual environment is in .venv

The Python virtual environment lives at `.venv/` in the project root. **You must activate it
or use its Python binary directly** before running any Python commands. Running `python` or
`pip` without activation will use the system Python, which won't have the required packages.

**Quick activation:**
```powershell
# Windows PowerShell
.\. .venv\Scripts\Activate.ps1

# Linux/macOS/FreeBSD
source .venv/bin/activate
```

**Or use the binary directly (no activation needed):**
```powershell
# Windows
.\.venv\Scripts\python.exe tests/e2e_test.py
.\.venv\Scripts\pip.exe install -r fetcher/requirements.txt

# Linux
.venv/bin/python tests/e2e_test.py
```

The build scripts (`build.ps1 setup` / `build.sh setup`) create the venv and install
dependencies. If `.venv/` doesn't exist, run `build.ps1 setup` first.

### Python Virtual Environment

- **Location:** `.venv/` in project root
- **Python version:** 3.10+ (Dockerfile uses 3.12)
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

### Testing a Branch with Git Worktrees (Windows)

Use git worktrees to build and test a branch without switching away from main.
This keeps your main working tree clean and avoids recompiling when switching back.

**1. Create the worktree:**
```powershell
git worktree add ..\rss-lance-wip wip-changes   # adjust branch name as needed
cd ..\rss-lance-wip
```

**2. Create junctions for large non-tracked assets:**

These files are not in git and are too large to duplicate. Windows junctions
make them appear in the worktree without copying.

```powershell
# From the worktree root:
cmd /c mklink /J server\lib  ..\rss-lance\server\lib     # ~390 MB .a file
cmd /c mklink /J tools       ..\rss-lance\tools           # duckdb.exe
cmd /c mklink /J .venv       ..\rss-lance\.venv           # Python venv
```

If the branch needs its own Python deps (different requirements.txt), skip the
`.venv` junction and run `build.ps1 setup` to create a fresh venv in the worktree.

The `server/_lancedb-go` directory is gitignored but referenced via `go.mod replace`.
If the worktree's go.mod points to a local path, junction it too:
```powershell
cmd /c mklink /J server\_lancedb-go ..\rss-lance\server\_lancedb-go
```

**3. Build and test:**
```powershell
.\build.ps1 server        # builds using junctioned lib/
.\test.ps1 go             # Go tests (53 pass, 10 skip typical)
.venv\Scripts\python.exe -m pytest tests\python\ -v --tb=short   # Python tests
```

The `data/` directory is created automatically by every build command.
If no Lance tables exist (fresh worktree), `test_duckdb_persistent.py` skips
gracefully. To get data, either run the server + fetcher once, or junction data/:
```powershell
cmd /c mklink /J data ..\rss-lance\data   # share main's data (read-only testing)
```

**4. E2E tests:**
```powershell
.venv\Scripts\python.exe tests\e2e_test.py   # runs against build\rss-lance-server.exe
```

E2E creates its own temp directory for data, configs, and server logs. The server
binary is always `build\rss-lance-server.exe` relative to the worktree root.
The E2E test uses `ROOT = Path(__file__).resolve().parent.parent` to find everything,
so it works correctly from a worktree without additional configuration.

Save output for comparison:
```powershell
.venv\Scripts\python.exe tests\e2e_test.py 2>&1 > build\e2e_output.txt
Get-Content build\e2e_output.txt | Select-Object -Last 40   # check summary
```

**Expected E2E results: ~329 passed / ~7 expected failures out of ~336 total.**

The 7 expected failures are:

- **Settings DB Verification (4 failures):** The E2E test queries Lance tables directly via
  DuckDB CLI to verify settings values. Because all writes are now buffered through
  `pending_changes` and flushed every 30s, the direct Lance query may not see the latest
  values yet. The API returns correct values (all API-based settings checks pass). These
  failures are expected and harmless -- they test the flush timing, not correctness.

- **Offline Mode (3 failures):** The E2E test checks for an `"enabled"` field in the
  `/api/offline-status` response that no longer exists (offline mode is always active, the
  toggle was removed). The test also checks snapshot article counts using `updated_at`
  filtering that may see 0 cached articles depending on timing. These tests need updating
  to match the new always-active offline architecture.

**5. Cleanup when done:**
```powershell
cd ..\rss-lance
git worktree remove ..\rss-lance-wip
```

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
|   |   |-- cache.go        # In-memory write cache + CTE overlay for immediate read visibility
|   |   |-- offline_cache.go # DuckDB pending_changes buffer + offline snapshot cache
|   |   |-- logbuffer.go    # Buffered log writer (batch flush)
|   |   |-- lance_writer.go # Shared CUD via lancedb-go native SDK (flush target)
|   |   |-- duckdb_process.go # Persistent duckdb.exe subprocess (Windows only)
|   |   |-- lance_windows.go # Windows: DuckDB CLI reads + buffered write path
|   |   +-- lance_cgo.go    # Non-Windows (Linux/FreeBSD/macOS): embedded DuckDB reads + buffered write path
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
|-- tests/              # All tests consolidated here
|   |-- python/         # Python unit + integration tests (pytest)
|   |   |-- test_config.py, test_content_cleaner.py, test_db.py
|   |   |-- test_duckdb_persistent.py, test_feed_parser.py
|   |   |-- test_opml_roundtrip.py, test_tiers.py
|   |-- frontend/       # Frontend Jest tests
|   |   +-- api.test.js, dom.test.js, feeds.test.js, relativeTime.test.js, sanitise.test.js
|   |-- e2e_test.py     # End-to-end integration test
|   |-- stress_test.py  # Stress & security test suite
|   +-- benchmark.py    # Performance benchmarks (insert, sanitize, read)
|-- tools/              # DuckDB CLI binary (downloaded at build time)
|-- build.ps1           # Windows build script
|-- build.sh            # Linux/FreeBSD build script
|-- run.ps1             # Windows runtime commands (daily use)
|-- run.sh              # Linux/FreeBSD runtime commands (daily use)
|-- test.ps1            # Windows test runner
|-- test.sh             # Linux/FreeBSD test runner
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
See [docs/testing.md](docs/testing.md) for running instructions and suite details.

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

`tests/e2e_test.py` is a standalone script (separate from the unit test suite) that exercises the full stack:

1. Starts a local HTTP server serving static RSS XML (3 feeds: Alpha=3, Bravo=5, Sanitize=6 = 14 articles)
2. Populates LanceDB using the Python fetcher's DB module
3. Verifies sanitization pipeline (tracking pixels, social links, tracking params, scripts, site chrome)
4. Verifies fetcher log writes via DuckDB
5. Verifies initial data via DuckDB
6. Starts the real `rss-lance-server.exe` with a temp config
7. **Verifies build version** -- if `--build-version` was given, checks `/api/server-status` `build_version` matches before running any API tests (catches stale binaries, concurrent builds)
8. Hits every API endpoint like the frontend would
9. Verifies read/star state changes, pagination, sorting, filtering
10. Tests log settings, trimming (count + age modes), retention
11. Tests custom CSS settings (set, update, clear, batch)
12. Tests config endpoint and shutdown API (restart with show_shutdown=true)
13. Checks final DB state via DuckDB
14. **Post-failure checks** -- if tests fail and `--build-version` was given, re-checks server health to detect crashes or binary replacement

**~290 checks** across 39 test sections covering: prerequisites, setup, local RSS server, populate data, sanitization (chrome/tracking/scripts), fetcher log writes, DuckDB verification, server startup, **build version verification**, feed listing, single feed, article listing, articles by feed, view article, batch fetch, mark read/unread, unread filter, star/unstar, mark-all-read, multiple state changes (cache), DB status, server runtime status, final global state, categories, sorting, pagination, log settings, log trimming (count mode), log trimming (age mode), settings DB verification, custom CSS, error handling, final DuckDB verification, queue feed, logs API endpoint, config (show_shutdown), shutdown API, and **offline mode** (data disappear/recovery, skipped on Windows).

### Test File Locations

| Suite | Location | Framework | What it tests |
|---|---|---|---|
| Python fetcher | `tests/python/test_*.py` | pytest | Feed parsing, config, tiers, content cleaner, DB integration (real LanceDB in temp dirs) |
| Go API | `server/api/api_test.go` | go test | All REST endpoints via mock Store (no CGo needed in test logic, but CGo required to compile because of transitive `db` import) |
| Go DB | `server/db/store_test.go` | go test | SQL escaping (8 cases), Feed/Article struct JSON field validation (6 tests) |
| Frontend | `tests/frontend/*.test.js` | Jest + jsdom | Sanitization, time formatting, feed activity, DOM structure, API patterns |
| OPML roundtrip | `tests/python/test_opml_roundtrip.py` | pytest | Export -> import -> verify round-trip |
| DuckDB persistent | `tests/python/test_duckdb_persistent.py` | pytest | Persistent DuckDB process read performance vs CLI |
| E2E integration | `tests/e2e_test.py` | standalone | Full-stack: ~290 checks across all services |
| Stress test | `tests/stress_test.py` | standalone | Concurrency, rate limiting, security, chaos testing |
| Benchmark | `tests/benchmark.py` | standalone | Insert, sanitize, pipeline, and read performance |

### CGo Dependency for Go Tests

See **"Do NOT run `go test` directly"** at the top of this file. The Go API tests transitively import `lancedb-go` via `lance_writer.go`, so compiling them requires CGo (GCC + `liblancedb_go.a`), even though the tests use an in-memory mock. Always use the test scripts.

> **Future improvement:** Split the `Store` interface and types into a separate package (e.g. `db/types`) with no CGo dependency, so API tests can compile without GCC. See IMPLEMENTATION_PLAN.md for the planned approach.

### Frontend Tests (Node.js required)

Frontend tests use Jest with jsdom. Node.js is **not** required to run the app -- only for running frontend tests. If Node.js/npm is not found, the test runner skips the frontend suite. See [docs/testing.md](docs/testing.md) for running instructions.

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
- **Python 3.10+** -- needed to run the fetcher (Dockerfile uses 3.12)
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
- **Note:** `psycopg2-binary` is only in `migrate/requirements.txt` -- it is only needed for the TT-RSS PostgreSQL import (`import_ttrss.py`). It is NOT in `fetcher/requirements.txt`.

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
- **DuckDB database file** (`server.duckdb`) lives in the data path by default. When data is on NFS/S3, set `duckdb_path` in `config.toml` to a local directory -- DuckDB requires local storage for reliable file locking. The server warns at startup if the DuckDB path is on a non-local filesystem.
- **DuckDB version** 1.5.0 (downloaded by `build.ps1 duckdb` into `tools/`).
- **DuckDB Lance extension** cannot handle `UPDATE ... WHERE id IN (...)` (fails with "Lance UPDATE does not support UPDATE with joins or FROM") - this is why the Go server uses lancedb-go for writes.
- **DuckDB file locking on Windows:** DuckDB uses exclusive file locks on Windows, so only **one** DuckDB process can open the Lance data directory at a time. The persistent DuckDB process (see below) uses a **single** long-lived `duckdb.exe :memory: -json` process with queries serialized through it via stdin/stdout. Two-phase startup: (1) one-shot INSTALL to cache the extension, (2) persistent process with LOAD lance + ATTACH + verification query. The process auto-restarts if it dies (crash, OOM, user kill) -- detects broken pipe on write or EOF on read, kills old process, re-runs Phase 2, retries the failed query once. Go unit tests cover kill-and-restart scenarios. All process death/restart events are logged at ERROR level via `log.Printf("ERROR: ...")`.

### WARNING: DuckDB Persistent Process Gotchas (Windows)

The persistent `duckdb.exe` process (`server/db/duckdb_process.go`) communicates via stdin/stdout using a sentinel-based protocol. There are subtle bugs that can cause 30-second timeouts or protocol desync. **Read all of these before modifying the DuckDB process code:**

1. **Semicolons are REQUIRED.** DuckDB interactive mode will NOT execute a statement until it sees a terminating `;`. The `sendAndRead()` method auto-appends one if missing, but any code that constructs SQL fed to this process must be aware. Without the semicolon, DuckDB silently waits for more input, the reader goroutine blocks, and the query times out after 30s.

2. **Errored queries produce NO stdout output.** When a SQL query fails (e.g. table not found), DuckDB sends the error message to **stderr only** -- nothing goes to stdout. The sentinel `SELECT '__SENTINEL__' AS _s;` still succeeds and appears on stdout. This means `sendAndRead()` reads the sentinel as the "real result" and then waits forever for a second sentinel that never arrives. The fix: after reading the first result, check if it IS the sentinel (`isSentinelResult()`). If so, the real query errored -- return empty rows and skip the second read.

3. **Multi-statement SQL (two semicolons) produces TWO result sets.** If someone sends `CREATE TABLE ...; SELECT ...;` as a single string, DuckDB outputs two JSON arrays. The reader would see the first as the real result and the second as the sentinel, then the actual sentinel has nothing to consume it -- desyncing all subsequent queries. **Always send exactly one statement per `query()` or `execStmt()` call.** CTEs (`WITH ... SELECT ...`) are fine because they are a single statement.

4. **DuckDB `-json` mode outputs `[{]` for some Lance queries with zero rows.** This is not valid JSON. The `parseJSONRows()` function handles this quirk explicitly. If you see JSON parse errors on empty result sets, check for this pattern.

5. **Process death is transparent to the API layer.** When `duckdb.exe` dies (kill, crash, OOM), the next `query()` or `execStmt()` call detects it (broken pipe on stdin write, or EOF on stdout read), auto-restarts the process, and retries the query once. The API handler never sees an error. All death/restart events log at ERROR level. If the restart itself fails, the error propagates up to the caller.

6. **The reader goroutine tracks JSON bracket depth** including string-literal awareness (to avoid counting `[` inside strings). If you modify `readLoop()`, be careful with the `inString`/`escape` tracking -- getting it wrong causes the reader to split one JSON array into two or merge two arrays into one, desyncing the protocol.
- **Single-user only** - no auth layer; each user runs their own instance.
- **Cross-platform targets:** Windows amd64, Linux amd64/arm64, macOS amd64/arm64, FreeBSD amd64.
- The Go server serves static files from `frontend/` and exposes a REST API under `/api/`.
- When adding new Python dependencies, update `fetcher/requirements.txt` or `migrate/requirements.txt` accordingly.

---

## Docker

See [docs/docker.md](docs/docker.md) for user-facing deployment details.

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

`tests/benchmark.py` provides 4 modes:
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
| sanitization        | log.fetcher.sanitization           | Debug: what the sanitizer stripped     |
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
| storage_events     | (always on)                         | Log storage failover/recovery events  |

### 3-Tier Log Write Path

The Go server buffers log entries in memory and flushes them through a 3-tier path:
1. **Lance** (primary) -- `log_api.lance` via lancedb-go SDK
2. **DuckDB** (fallback) -- `cached_logs` table in `offline_cache.db`
3. **Memory** (last resort) -- entries stay in `logBuffer`, subject to `log.memory_cap` (default 100,000)

A background drain goroutine moves `cached_logs` entries back to Lance when it recovers.
Storage infrastructure events are captured in a `storage_events` category via an in-memory ring buffer.
See [docs/logging.md](docs/logging.md#3-tier-log-write-path-go-server) for full details.

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
- [ ] **Settings in database** - new feature settings live in `DEFAULT_SETTINGS` (`fetcher/db.py`) + Settings UI (`settings-page.js`), NOT `config.toml` (unless needed for bootstrap -- see below)

### Settings Placement: config.toml vs Settings Table

See [docs/configuration.md](docs/configuration.md) for the user-facing config.toml reference.

New feature settings **do NOT go in `config.toml`**. They belong in the Lance **settings table** (managed via the Settings page in the UI). `config.toml` is reserved exclusively for bootstrap settings -- things the application needs before it can open the Lance database.

**config.toml (bootstrap only):**

| Section | Keys | Why it must be in config.toml |
|---|---|---|
| `[storage]` | `type`, `path`, `duckdb_path`, `s3_region`, `s3_endpoint` | Needed to locate and open the Lance files; `duckdb_path` separates DuckDB from data path for NFS/S3 setups |
| `[server]` | `host`, `port`, `frontend_dir` | Needed to bind the HTTP server and find static files |
| `[server]` | `show_shutdown` | Admin/safety control for the shutdown route |
| `[migration.*]` | connection strings, credentials | One-time import tools, not runtime features |

**Settings table (everything else):**

All runtime-configurable behavior lives in the settings table (`data/settings.lance/`), exposed via `GET/PUT /api/settings` and the Settings page. This includes: UI preferences, logging toggles, compaction thresholds, cache tuning, fetcher intervals, tier thresholds, custom CSS, and any future feature settings.

**When adding a new feature setting:**

1. Add the default value to `DEFAULT_SETTINGS` in `fetcher/db.py`
2. Add UI controls to the appropriate section of `frontend/js/settings-page.js`
3. Read the setting via the settings API or settings cache at runtime (Go: `store.GetSetting(key)` / `store.GetSettings()`; Python: settings dict from `_load_settings()`)
4. Do **NOT** add it to `config.toml` unless the app cannot start without it

**Rule of thumb:** If the setting cannot take effect until after the database is open, it belongs in the database.

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
