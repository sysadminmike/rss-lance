#!/usr/bin/env python3
"""
Test: Persistent DuckDB process with Lance extension.

Validates that a long-running duckdb.exe process can be kept alive and reused
for multiple queries instead of spawning a new process per query. This is the
approach planned for replacing the current spawn-per-query pattern in
lance_windows.go (the Go server's Windows DuckDB read layer).

Strategy:
  1. One-shot process: INSTALL lance (ensures extension is on disk)
  2. Start persistent process using :memory: (no file lock contention)
  3. LOAD lance -> SELECT duckdb_extensions() to confirm it loaded
  4. ATTACH data dir -> run real queries
  5. Performance comparison vs spawn-per-query
  6. Pool of multiple :memory: processes (round-robin)
  7. Concurrent queries via thread pool + DuckDB process pool

Requires:
  - tools/duckdb.exe in the project root
  - data/ directory with Lance tables (feeds, articles)

Results (typical Windows NVMe):
  - Spawn-per-query: ~550-800ms per query
  - Persistent process: ~30-50ms per query (15-23x speedup)
  - Pool of 4 processes, 20 concurrent queries: ~0.03s total, 0 errors
"""

import concurrent.futures
import json
import queue
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent.parent
DUCKDB_BIN = ROOT / "tools" / "duckdb.exe"
DATA_PATH = ROOT / "data"
DB_PATH = str(DATA_PATH / "server.duckdb")

# Skip entire module if prerequisites are missing (e.g. git worktree with no data/)
_skip_reason = None
if not DUCKDB_BIN.exists():
    _skip_reason = f"duckdb.exe not found at {DUCKDB_BIN}"
elif not DATA_PATH.exists():
    _skip_reason = f"data/ directory not found at {DATA_PATH}"
elif not (DATA_PATH / "server.duckdb").exists():
    _skip_reason = f"server.duckdb not found at {DATA_PATH / 'server.duckdb'}"
elif not (DATA_PATH / "feeds.lance").exists() or not (DATA_PATH / "articles.lance").exists():
    _skip_reason = f"Lance tables not found in {DATA_PATH} (need feeds.lance + articles.lance)"

if _skip_reason:
    pytest.skip(_skip_reason, allow_module_level=True)


def section(name):
    print(f"\n{'='*60}")
    print(f"  {name}")
    print(f"{'='*60}")


SENTINEL_JSON = '"__SENTINEL__"'


def read_json_until_sentinel(proc):
    """Read JSON output lines until we see the sentinel result."""
    collected = []
    while True:
        line = proc.stdout.readline()
        if not line:
            break
        if SENTINEL_JSON in line:
            break
        collected.append(line)
    raw = "".join(collected).strip()
    if not raw:
        return []
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return raw


def send_query(proc, sql):
    """Send a query + sentinel, return parsed JSON result."""
    proc.stdin.write(f"{sql}\n")
    proc.stdin.write("SELECT '__SENTINEL__' AS s;\n")
    proc.stdin.flush()
    return read_json_until_sentinel(proc)


# ---------------------------------------------------------------
#  Step 1: One-shot process to INSTALL lance extension
# ---------------------------------------------------------------
section("Step 1: Install Lance extension (one-shot)")

result = subprocess.run(
    [str(DUCKDB_BIN), DB_PATH, "-c",
     "INSTALL lance;"],
    capture_output=True, text=True, timeout=30,
)
print(f"  exit code: {result.returncode}")
if result.stderr.strip():
    print(f"  stderr: {result.stderr.strip()[:300]}")
if result.returncode == 0:
    print(f"  [OK] Lance extension installed/confirmed")
else:
    print(f"  [WARN] Install returned {result.returncode} - may already be installed")


# ---------------------------------------------------------------
#  Step 2: Start persistent process with :memory:
# ---------------------------------------------------------------
section("Step 2: Start persistent :memory: process + LOAD lance")

proc = subprocess.Popen(
    [str(DUCKDB_BIN), ":memory:", "-json"],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    text=True,
    bufsize=0,
)

# LOAD lance and verify with extension check
proc.stdin.write("LOAD lance;\n")
proc.stdin.write("SELECT '__SENTINEL__' AS s;\n")
proc.stdin.flush()
load_result = read_json_until_sentinel(proc)
print(f"  LOAD lance output: {load_result}")

# Now query extensions to confirm lance is loaded
result = send_query(proc,
    "SELECT extension_name, loaded, install_path "
    "FROM duckdb_extensions() "
    "WHERE loaded = true AND extension_name = 'lance';")
print(f"  Extension check: {json.dumps(result, indent=2)}")

lance_loaded = (isinstance(result, list) and len(result) > 0
                and result[0].get("extension_name") == "lance")
print(f"  [{'OK' if lance_loaded else 'FAIL'}] Lance loaded = {lance_loaded}")


# ---------------------------------------------------------------
#  Step 3: ATTACH data dir and run real queries
# ---------------------------------------------------------------
section("Step 3: ATTACH Lance data + run queries")

lance_path = str(DATA_PATH).replace("\\", "/")
attach_result = send_query(proc,
    f"ATTACH IF NOT EXISTS '{lance_path}' AS _lance (TYPE LANCE);")
print(f"  ATTACH result: {attach_result}")

# Feeds count
r = send_query(proc, "SELECT COUNT(*) AS cnt FROM _lance.main.feeds;")
print(f"  Feed count: {r}")

# Articles count
r = send_query(proc, "SELECT COUNT(*) AS cnt FROM _lance.main.articles;")
print(f"  Article count: {r}")

# Sample feeds
r = send_query(proc, "SELECT feed_id, title FROM _lance.main.feeds LIMIT 3;")
print(f"  Sample feeds: {json.dumps(r, indent=2)[:500]}")

# Sample articles
r = send_query(proc,
    "SELECT article_id, title, is_read FROM _lance.main.articles LIMIT 3;")
print(f"  Sample articles: {json.dumps(r, indent=2)[:500]}")


# ---------------------------------------------------------------
#  Step 4: Multiple rapid queries (keep-alive works?)
# ---------------------------------------------------------------
section("Step 4: Rapid-fire queries on same process")

errors = 0
for i in range(20):
    r = send_query(proc,
        "SELECT COUNT(*) AS cnt FROM _lance.main.articles;")
    if not isinstance(r, list) or len(r) == 0:
        errors += 1
        print(f"  Query {i+1}: ERROR - got {r}")
print(f"  20 rapid queries: {20-errors}/20 succeeded, {errors} errors")
print(f"  [{'OK' if errors == 0 else 'FAIL'}]")

# Clean up persistent process
proc.stdin.close()
proc.wait(timeout=5)
print(f"  Process exited: code={proc.returncode}")


# ---------------------------------------------------------------
#  Step 5: Performance comparison
# ---------------------------------------------------------------
section("Step 5: Performance comparison (N=20)")

N = 20
lance_path = str(DATA_PATH).replace("\\", "/")

# Approach A: spawn new duckdb.exe per query (current approach)
t0 = time.perf_counter()
for i in range(N):
    full_sql = (f"LOAD lance;\n"
                f"ATTACH IF NOT EXISTS '{lance_path}' AS _lance (TYPE LANCE);\n"
                f"SELECT COUNT(*) AS cnt FROM _lance.main.articles;")
    subprocess.run(
        [str(DUCKDB_BIN), DB_PATH, "-json", "-c", full_sql],
        capture_output=True, text=True, timeout=30,
    )
t1 = time.perf_counter()
spawn_time = t1 - t0
print(f"  Spawn-per-query: {spawn_time:.3f}s  ({spawn_time/N*1000:.0f}ms/query)")

# Approach B: persistent process + sentinel (using :memory:)
t0 = time.perf_counter()
proc = subprocess.Popen(
    [str(DUCKDB_BIN), ":memory:", "-json"],
    stdin=subprocess.PIPE, stdout=subprocess.PIPE,
    stderr=subprocess.PIPE, text=True, bufsize=0,
)
# Bootstrap once
send_query(proc, "LOAD lance;")
send_query(proc,
    f"ATTACH IF NOT EXISTS '{lance_path}' AS _lance (TYPE LANCE);")

for i in range(N):
    send_query(proc,
        "SELECT COUNT(*) AS cnt FROM _lance.main.articles;")

proc.stdin.close()
proc.wait(timeout=5)
t1 = time.perf_counter()
persistent_time = t1 - t0
print(f"  Persistent:      {persistent_time:.3f}s  ({persistent_time/N*1000:.0f}ms/query)")
speedup = spawn_time / persistent_time if persistent_time > 0 else 0
print(f"  Speedup: {speedup:.1f}x")


# ---------------------------------------------------------------
#  Step 6: Can we open multiple :memory: processes? (pool test)
# ---------------------------------------------------------------
section("Step 6: Pool of 3 persistent processes")

lance_path = str(DATA_PATH).replace("\\", "/")
pool = []
for idx in range(3):
    p = subprocess.Popen(
        [str(DUCKDB_BIN), ":memory:", "-json"],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, text=True, bufsize=0,
    )
    send_query(p, "LOAD lance;")
    send_query(p,
        f"ATTACH IF NOT EXISTS '{lance_path}' AS _lance (TYPE LANCE);")
    # Verify
    r = send_query(p,
        "SELECT COUNT(*) AS cnt FROM _lance.main.articles;")
    ok = isinstance(r, list) and len(r) > 0
    print(f"  Process {idx+1}: {'OK' if ok else 'FAIL'} - {r}")
    pool.append(p)

# Use them round-robin
pool_errors = 0
for i in range(30):
    p = pool[i % 3]
    r = send_query(p,
        "SELECT COUNT(*) AS cnt FROM _lance.main.articles;")
    if not isinstance(r, list) or len(r) == 0:
        pool_errors += 1

print(f"  30 round-robin queries: {30-pool_errors}/30 ok")
print(f"  [{'OK' if pool_errors == 0 else 'FAIL'}]")

# Cleanup
for p in pool:
    p.stdin.close()
    p.wait(timeout=5)


# ---------------------------------------------------------------
#  Step 7: Pool of persistent processes (concurrent via threads)
# ---------------------------------------------------------------
section("Step 7: Pool of persistent DuckDB processes")

lance_path = str(DATA_PATH).replace("\\", "/")
db_path = DB_PATH

class DuckDBProcess:
    """A persistent DuckDB process that accepts queries via stdin.

    Uses a dedicated reader thread to avoid Windows pipe deadlocks.
    Queries are serialized per-process via a lock; results are passed
    back through a threading.Event + shared slot.
    """

    SENTINEL = '__SENTINEL__'

    def __init__(self, duckdb_bin, db_path, lance_path):
        self.proc = subprocess.Popen(
            [str(duckdb_bin), ":memory:", "-json"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,  # line-buffered
        )
        self.lock = threading.Lock()
        self._result_ready = threading.Event()
        self._result_data = None
        self._alive = True

        # Dedicated reader thread
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

        # Bootstrap: LOAD, ATTACH, then sentinel
        with self.lock:
            boot = (
                f"INSTALL lance;\n"
                f"LOAD lance;\n"
                f"ATTACH IF NOT EXISTS '{lance_path}' AS _lance (TYPE LANCE);\n"
            )
            self.proc.stdin.write(boot)
            self.proc.stdin.flush()
            time.sleep(0.3)
            self.proc.stdin.write("SELECT 'DONE' AS boot;\n")
            self.proc.stdin.flush()
            self._wait_for_result()  # consumes the [{"boot":"DONE"}]

    def _read_loop(self):
        """Background thread: reads JSON arrays from DuckDB stdout."""
        buf = []
        bracket_depth = 0
        while self._alive:
            try:
                line = self.proc.stdout.readline()
            except (ValueError, OSError):
                break
            if not line:
                break
            buf.append(line)
            for ch in line:
                if ch == '[':
                    bracket_depth += 1
                elif ch == ']':
                    bracket_depth -= 1

            if bracket_depth <= 0 and buf:
                raw = "".join(buf).strip()
                buf = []
                bracket_depth = 0
                if not raw:
                    continue
                try:
                    result = json.loads(raw)
                except json.JSONDecodeError:
                    result = raw
                self._result_data = result
                self._result_ready.set()

    def _wait_for_result(self, timeout=30):
        if not self._result_ready.wait(timeout=timeout):
            raise TimeoutError("DuckDB query timed out")
        self._result_ready.clear()
        return self._result_data

    def query(self, sql):
        with self.lock:
            self.proc.stdin.write(f"{sql}\n")
            self.proc.stdin.flush()
            return self._wait_for_result()

    def close(self):
        self._alive = False
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


class DuckDBPool:
    """Pool of persistent DuckDB processes for concurrent read access.

    Each process handles one query at a time (serialized by its own lock).
    The pool round-robins across processes so N processes can serve N
    concurrent queries without any process contention.
    """

    def __init__(self, size, duckdb_bin, db_path, lance_path):
        self.pool = queue.Queue()
        for _ in range(size):
            p = DuckDBProcess(duckdb_bin, db_path, lance_path)
            self.pool.put(p)

    def query(self, sql):
        proc = self.pool.get()
        try:
            return proc.query(sql)
        finally:
            self.pool.put(proc)

    def close(self):
        while not self.pool.empty():
            try:
                p = self.pool.get_nowait()
                p.close()
            except queue.Empty:
                break


POOL_SIZE = 4
pool = DuckDBPool(POOL_SIZE, str(DUCKDB_BIN), db_path, lance_path)

# Concurrent queries
errors = 0
results = []

def pool_query(i):
    return pool.query("SELECT COUNT(*) AS cnt FROM _lance.main.articles;")

t0 = time.perf_counter()
with concurrent.futures.ThreadPoolExecutor(max_workers=POOL_SIZE) as executor:
    futures = [executor.submit(pool_query, i) for i in range(20)]
    for f in concurrent.futures.as_completed(futures):
        try:
            r = f.result()
            results.append(r)
        except Exception as e:
            errors += 1
            print(f"  ERROR: {e}")

t1 = time.perf_counter()
pool.close()

print(f"  Pool of {POOL_SIZE} processes, 20 concurrent queries: {t1-t0:.3f}s")
print(f"  Errors: {errors}")
print(f"  Sample results: {results[:3]}")
print(f"  All results consistent: {len(set(str(r) for r in results)) == 1}")

print(f"\n{'='*60}")
print(f"  DONE")
print(f"{'='*60}")
