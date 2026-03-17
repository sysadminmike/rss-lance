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
```

```bash
# Linux / macOS
./test.sh python|go|frontend|backend|database|api
```

## Test Suites

| Suite | Framework | Tests | What it covers |
|---|---|---|---|
| **Python fetcher** | pytest | 127 | Feed parsing, config loading, adaptive tier logic, content cleaning, LanceDB integration |
| **Migration** | pytest | 7 | OPML export → import round-trip (feeds, categories, hierarchy, idempotency) |
| **Go API** | go test | 39 | All REST endpoints (feeds, articles, categories, mark read/star, pagination, status, server status) |
| **Go DB** | go test | 6 | SQL escaping, JSON field validation |
| **Frontend** | Jest + jsdom | 86 | HTML sanitization, relative time, feed activity, DOM structure, API patterns |

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

The migration test (`migrate/test_opml_roundtrip.py`) verifies that OPML export → import preserves feeds, categories, hierarchy, and feed-category assignments. It also tests empty DB export and idempotent double-import.

```powershell
# Windows
python -m pytest migrate/test_opml_roundtrip.py -v

# Linux / macOS
python -m pytest migrate/test_opml_roundtrip.py -v
```

## End-to-End Integration Test

`e2e_test.py` is a standalone script that exercises the full stack:

- Serves static RSS feeds via a local HTTP server
- Populates LanceDB using the Python fetcher
- Starts the real Go server against a temp database
- Hits every API endpoint (list, view, read, star, filter, paginate, sort)
- Verifies DB state via DuckDB CLI
- **229 checks total**

```powershell
# Prerequisites: build the server and download DuckDB
.\build.ps1 server
.\build.ps1 duckdb

# Run E2E test
python e2e_test.py
python e2e_test.py --verbose    # show HTTP details
python e2e_test.py --keep       # keep temp dir for debugging
```

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
