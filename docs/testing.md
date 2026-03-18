# Testing RSS-Lance

## Quick Start

```powershell
# Windows - run all tests
.\test.ps1

# Linux / macOS - run all tests
./test.sh
```

## Running Specific Suites

```powershell
# Windows
.\test.ps1 python      # Python fetcher tests
.\test.ps1 go          # Go API + DB tests
.\test.ps1 frontend    # Frontend Jest tests (requires Node.js)
.\test.ps1 backend     # Python + Go (no frontend)
.\test.ps1 database    # Python DB integration tests only
.\test.ps1 api         # Go API handler tests only
.\test.ps1 e2e         # E2E integration (builds server with test version + runs)
```

```bash
# Linux / macOS
./test.sh python|go|frontend|backend|database|api|e2e
```

## Test Suites

| Suite | Framework | Tests | What it covers |
|---|---|---|---|
| **Python fetcher** | pytest | 127 | Feed parsing, config loading, adaptive tier logic, content cleaning, LanceDB integration |
| **Migration** | pytest | 7 | OPML export -> import round-trip (feeds, categories, hierarchy, idempotency) |
| **DuckDB persistent** | standalone | 7 | Persistent DuckDB process pool - validates the :memory: process approach for Windows reads (install, load, attach, rapid queries, performance comparison, pool round-robin, concurrent pool) |
| **Go API** | go test | 39 | All REST endpoints (feeds, articles, categories, mark read/star, pagination, status, server status) |
| **Go DB** | go test | 6 | SQL escaping, JSON field validation |
| **Frontend** | Jest + jsdom | 86 | HTML sanitization, relative time, feed activity, DOM structure, API patterns |
| **E2E integration** | standalone | ~290 checks | Full-stack: fetcher, server, every API endpoint, sanitization, logging, offline mode |
| **Stress test** | standalone | varies | Concurrency, rate limiting, security, chaos testing |
| **Benchmark** | standalone | - | Insert, sanitize, pipeline, and read performance |

## Tests in the Build

The `all` build command runs tests after building:

```powershell
# Full build with tests (default)
.\build.ps1 all

# Full build WITHOUT tests
.\build.ps1 -NoTests all
```

```bash
./build.sh all              # with tests
./build.sh --no-tests all   # without tests
```

## Requirements per Suite

| Suite | Requires |
|---|---|
| Python | Python 3.10+ with `.venv` set up (`build.ps1 setup`) |
| Migration | Python 3.10+ with `.venv` set up (same as Python) |
| Go | Go 1.21+, GCC/MinGW (Windows only, for CGo compilation) |
| Frontend | Node.js + npm (test-only dependency, not needed to run the app) |

If a required tool is missing, the test runner skips that suite with a `[SKIP]` message and continues.

## OPML Round-Trip Test

The migration test (`tests/python/test_opml_roundtrip.py`) verifies that OPML export -> import preserves feeds, categories, hierarchy, and feed-category assignments. It also tests empty DB export and idempotent double-import.

```powershell
# Windows
python -m pytest tests/python/test_opml_roundtrip.py -v

# Linux / macOS
python -m pytest tests/python/test_opml_roundtrip.py -v
```

## DuckDB Persistent Process Test

`tests/python/test_duckdb_persistent.py` validates the persistent DuckDB process approach planned for replacing the spawn-per-query pattern in `lance_windows.go`. It tests:

1. One-shot `INSTALL lance` (ensures extension is on disk)
2. Persistent `:memory:` process with `LOAD lance` + extension verification
3. `ATTACH` data directory and run real queries
4. Rapid-fire queries on a single process (20 queries)
5. Performance comparison vs spawn-per-query (typically 15-23x speedup)
6. Pool of 3 persistent processes with round-robin query distribution
7. Concurrent queries via thread pool + DuckDB process pool (4 processes, 20 queries, 0 errors)

```powershell
python tests/python/test_duckdb_persistent.py
```

## End-to-End Integration Test

`tests/e2e_test.py` is a standalone script that exercises the full stack:

- Serves static RSS feeds via a local HTTP server
- Populates LanceDB using the Python fetcher
- Starts the real Go server against a temp database
- Hits every API endpoint (list, view, read, star, filter, paginate, sort)
- Verifies DB state via DuckDB CLI
- **~290 checks total**

```powershell
# Prerequisites: build the server and download DuckDB
.\build.ps1 server
.\build.ps1 duckdb

# Run E2E test
python tests/e2e_test.py
python tests/e2e_test.py --verbose    # show HTTP details
python tests/e2e_test.py --keep       # keep temp dir for debugging
```

### Build Version Verification

The e2e test supports `--build-version <id>` to verify the running server binary
matches the one that was just built. This catches stale binaries, accidental
overwrites from concurrent builds, and server crashes mid-test.

**Automated (recommended):**

```powershell
# Windows -- generates a random test version, builds server, runs e2e
.\test.ps1 e2e

# Linux
./test.sh e2e
```

**Manual:**

```powershell
# Windows
$env:BUILD_VERSION = "test-myid123"; .\build.ps1 server
python tests/e2e_test.py --build-version test-myid123

# Linux
BUILD_VERSION=test-myid123 ./build.sh server
python tests/e2e_test.py --build-version test-myid123
```

The verification works as follows:

1. Before any API tests run, queries `/api/server-status` and checks `server.build_version`
2. If the version does not match, aborts with a message to rebuild
3. After any test failures, re-checks the server version to detect:
   - **Server crash** -- server unreachable, suggests rerunning the test
   - **Binary replaced** -- version changed mid-test, warns about concurrent builds

## Example Output

```
======================================================================
  Python Fetcher Tests
======================================================================

  [PASS] test_config > TestConfigLoad::test_defaults
  [PASS] test_config > TestConfigLoad::test_custom_values
  [PASS] test_db > TestDBFeeds::test_add_feed
  ...

======================================================================
  Go Server Tests
======================================================================

  [PASS] api/TestListFeeds
  [PASS] api/TestGetFeed
  ...

======================================================================
  TEST SUMMARY
======================================================================

  Suites:  Python Fetcher Tests, Go Server Tests, Frontend Tests
  Total:   265 tests
  Passed:  265

  ALL TESTS PASSED
```
