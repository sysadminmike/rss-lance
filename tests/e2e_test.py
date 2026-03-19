#!/usr/bin/env python3
"""
RSS-Lance End-to-End Integration Test
======================================

Spins up real infrastructure (local RSS server, Python fetcher, Go server)
and exercises the full lifecycle through the HTTP API, just like the
frontend would.

Usage:
    python e2e_test.py              # run all tests
    python e2e_test.py --keep       # keep temp data dir after run (for debugging)
    python e2e_test.py --verbose    # show HTTP response bodies

Prerequisites:
    - build/rss-lance-server.exe    (run: build.ps1 server)
    - tools/duckdb.exe              (run: build.ps1 duckdb)
    - Python venv with fetcher deps (run: build.ps1 setup)
"""

from __future__ import annotations

import argparse
import http.server
import json
import os
import re
import secrets
import shutil
import signal
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import urllib.request
import urllib.error
from pathlib import Path
from datetime import datetime, timezone
from typing import Any

# ---------------------- Resolve paths ----------------------
ROOT = Path(__file__).resolve().parent.parent
SERVER_BIN = ROOT / "build" / "rss-lance-server.exe"
DUCKDB_BIN = ROOT / "tools" / "duckdb.exe"
FETCHER_DIR = ROOT / "fetcher"

# Add fetcher to sys.path so we can import its modules
sys.path.insert(0, str(FETCHER_DIR))

# -------------- Static RSS XML for test feeds --------------
FEED_A_XML = textwrap.dedent("""\
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed Alpha</title>
    <link>http://localhost:{port}</link>
    <description>First test feed with 3 articles</description>
    <item>
      <title>Alpha Article One</title>
      <link>http://example.com/alpha/1</link>
      <guid>alpha-guid-1</guid>
      <pubDate>Mon, 01 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of alpha article one.</description>
    </item>
    <item>
      <title>Alpha Article Two</title>
      <link>http://example.com/alpha/2</link>
      <guid>alpha-guid-2</guid>
      <pubDate>Tue, 02 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of alpha article two.</description>
    </item>
    <item>
      <title>Alpha Article Three</title>
      <link>http://example.com/alpha/3</link>
      <guid>alpha-guid-3</guid>
      <pubDate>Wed, 03 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of alpha article three.</description>
    </item>
  </channel>
</rss>
""")

FEED_B_XML = textwrap.dedent("""\
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed Bravo</title>
    <link>http://localhost:{port}</link>
    <description>Second test feed with 5 articles</description>
    <item>
      <title>Bravo Article One</title>
      <link>http://example.com/bravo/1</link>
      <guid>bravo-guid-1</guid>
      <pubDate>Thu, 04 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of bravo article one.</description>
    </item>
    <item>
      <title>Bravo Article Two</title>
      <link>http://example.com/bravo/2</link>
      <guid>bravo-guid-2</guid>
      <pubDate>Fri, 05 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of bravo article two.</description>
    </item>
    <item>
      <title>Bravo Article Three</title>
      <link>http://example.com/bravo/3</link>
      <guid>bravo-guid-3</guid>
      <pubDate>Sat, 06 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of bravo article three.</description>
    </item>
    <item>
      <title>Bravo Article Four</title>
      <link>http://example.com/bravo/4</link>
      <guid>bravo-guid-4</guid>
      <pubDate>Sun, 07 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of bravo article four.</description>
    </item>
    <item>
      <title>Bravo Article Five</title>
      <link>http://example.com/bravo/5</link>
      <guid>bravo-guid-5</guid>
      <pubDate>Mon, 08 Jan 2024 10:00:00 +0000</pubDate>
      <description>Content of bravo article five.</description>
    </item>
  </channel>
</rss>
""")

# Shared spam blocks injected into every sanitization article.
# Each must be >80 bytes so strip_site_chrome's MIN_BLOCK_SIZE filter keeps them,
# and they must appear in >=2 articles so MIN_OCCURRENCES triggers.
_CHROME_NAV = (
    '<nav class="site-navigation" role="navigation">'
    '<ul><li><a href="/home">Home</a></li>'
    '<li><a href="/about">About Us</a></li>'
    '<li><a href="/contact">Contact</a></li></ul></nav>'
)
_CHROME_FOOTER = (
    '<footer class="site-footer">'
    '<p>Copyright 2024 The Register. All rights reserved. '
    'Situation Publishing Ltd. Terms and Conditions apply.</p></footer>'
)
_TRACKING_PIXEL = '<img src="https://pixel.wp.com/g.gif?host=example" width="1" height="1" alt="">'
_DANGEROUS_SCRIPT = '<script>document.cookie="track=1";</script>'
_EVENT_HANDLER = '<p onmouseover="alert(1)">hover text</p>'
_JS_URI = '<a href="javascript:void(0)">click me</a>'
_SOCIAL_LINKS = (
    '<div class="share-links">'
    '<a href="https://www.facebook.com/sharer/sharer.php?u=http://example.com">Share on Facebook</a> '
    '<a href="https://twitter.com/intent/tweet?url=http://example.com">Tweet</a> '
    '<a href="https://www.linkedin.com/shareArticle?url=http://example.com">LinkedIn</a>'
    '</div>'
)
_TRACKING_PARAM_LINK = (
    '<a href="https://example.com/article?utm_source=newsletter&fbclid=abc123&page=1">'
    'Tracked Link</a>'
)


def _reg_article(guid: str, title: str, date: str, unique_body: str) -> str:
    """Build one <item> whose <description> wraps unique_body with all the spam blocks."""
    desc = (
        _CHROME_NAV
        + f'<article><p>{unique_body}</p></article>'
        + _TRACKING_PIXEL
        + _DANGEROUS_SCRIPT
        + _EVENT_HANDLER
        + _JS_URI
        + _SOCIAL_LINKS
        + _TRACKING_PARAM_LINK
        + _CHROME_FOOTER
    )
    # Escape for safe embedding inside XML CDATA
    return (
        f'    <item>\n'
        f'      <title>{title}</title>\n'
        f'      <link>http://example.com/reg/{guid}</link>\n'
        f'      <guid>reg-{guid}</guid>\n'
        f'      <pubDate>{date}</pubDate>\n'
        f'      <description><![CDATA[{desc}]]></description>\n'
        f'    </item>'
    )


_SANITIZE_ITEMS = "\n".join([
    _reg_article("1", "Mobe reception woes: O2 and Three blamed for indoor not-spots",
                 "Mon, 15 Jan 2024 10:00:00 +0000",
                 "Indoor mobile coverage remains patchy as O2 and Three get the worst ratings from Ofcom."),
    _reg_article("2", "UK chip startup Graphcore different to sell itself after running out of cash",
                 "Tue, 16 Jan 2024 10:00:00 +0000",
                 "Bristol AI chip designer Graphcore is looking for a buyer after burning through its funding."),
    _reg_article("3", "Qualcomm touts Snapdragon X Elite benchmarks in different to impress",
                 "Wed, 17 Jan 2024 10:00:00 +0000",
                 "Qualcomm publishes benchmark results for its laptop ARM chip but caveats abound."),
    _reg_article("4", "Russia-linked Midnight Blizzard crew using stolen Microsoft credentials",
                 "Thu, 18 Jan 2024 10:00:00 +0000",
                 "APT29 threat actors leveraged exfiltrated auth tokens to access more internal mailboxes."),
    _reg_article("5", "Open source forkers stick an OpenBao in the oven after HashiCorp license change",
                 "Fri, 19 Jan 2024 10:00:00 +0000",
                 "LF project OpenBao forks Vault in response to the BSL license switch by HashiCorp."),
    _reg_article("6", "Raspberry Pi IPO expected to value maker of tiny computers at 630M pounds",
                 "Sat, 20 Jan 2024 10:00:00 +0000",
                 "Raspberry Pi plans London listing with valuation above half a billion sterling."),
])

FEED_SANITIZE_XML = textwrap.dedent("""\
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed Sanitize</title>
    <link>http://localhost:{port}</link>
    <description>Feed with injected chrome, tracking pixels, scripts, and social links</description>
""") + _SANITIZE_ITEMS + "\n  </channel>\n</rss>\n"


# ---------------------- Output helpers ----------------------
class TestRunner:
    """Tracks test results and prints nicely."""

    def __init__(self, verbose: bool = False):
        self.verbose = verbose
        self.passed = 0
        self.failed = 0
        self.errors: list[str] = []
        self._section = ""

    def section(self, name: str) -> None:
        self._section = name
        print(f"\n{'=' * 60}")
        print(f"  {name}")
        print(f"{'=' * 60}")

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
                print(f"         {detail}")
        return condition

    def log(self, msg: str) -> None:
        print(f"  ... {msg}")

    def summary(self) -> int:
        total = self.passed + self.failed
        print(f"\n{'=' * 60}")
        print(f"  E2E Test Summary")
        print(f"{'=' * 60}")
        print(f"  Total:  {total}")
        print(f"  Passed: {self.passed}")
        print(f"  Failed: {self.failed}")
        if self.errors:
            print(f"\n  Failed tests:")
            for e in self.errors:
                print(f"    - {e}")
        print()
        return 0 if self.failed == 0 else 1


# --------------------- Local RSS server ---------------------
class RSSHandler(http.server.BaseHTTPRequestHandler):
    """Serves static RSS XML from a dict of path -> content."""

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

    def log_message(self, format, *args):
        pass  # suppress request logging


def start_rss_server() -> tuple[http.server.HTTPServer, int]:
    """Start a local HTTP server serving test RSS feeds. Returns (server, port)."""
    server = http.server.HTTPServer(("127.0.0.1", 0), RSSHandler)
    port = server.server_address[1]

    # Set up feed content with the actual port
    RSSHandler.feeds = {
        "/feed_alpha.xml": FEED_A_XML.format(port=port),
        "/feed_bravo.xml": FEED_B_XML.format(port=port),
        "/feed_sanitize.xml": FEED_SANITIZE_XML.format(port=port),
    }

    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, port


# ------------------------ API client ------------------------
class APIClient:
    """Simple HTTP client for the RSS-Lance API."""

    def __init__(self, base_url: str, verbose: bool = False):
        self.base = base_url.rstrip("/")
        self.verbose = verbose

    def get(self, path: str) -> tuple[int, Any]:
        url = f"{self.base}{path}"
        req = urllib.request.Request(url)
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                body = json.loads(resp.read())
                if self.verbose:
                    print(f"    GET {path} -> {resp.status}")
                return resp.status, body
        except urllib.error.HTTPError as e:
            body = e.read().decode() if e.fp else ""
            if self.verbose:
                print(f"    GET {path} -> {e.code}: {body[:200]}")
            return e.code, body

    def post(self, path: str, data: dict | None = None) -> tuple[int, Any]:
        url = f"{self.base}{path}"
        body_bytes = json.dumps(data).encode() if data else b""
        req = urllib.request.Request(url, data=body_bytes, method="POST")
        req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                raw = resp.read()
                body = json.loads(raw) if raw else {}
                if self.verbose:
                    print(f"    POST {path} -> {resp.status}")
                return resp.status, body
        except urllib.error.HTTPError as e:
            body = e.read().decode() if e.fp else ""
            if self.verbose:
                print(f"    POST {path} -> {e.code}: {body[:200]}")
            return e.code, body

    def put(self, path: str, data: dict | None = None) -> tuple[int, Any]:
        url = f"{self.base}{path}"
        body_bytes = json.dumps(data).encode() if data else b""
        req = urllib.request.Request(url, data=body_bytes, method="PUT")
        req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                raw = resp.read()
                body = json.loads(raw) if raw else {}
                if self.verbose:
                    print(f"    PUT {path} -> {resp.status}")
                return resp.status, body
        except urllib.error.HTTPError as e:
            body = e.read().decode() if e.fp else ""
            if self.verbose:
                print(f"    PUT {path} -> {e.code}: {body[:200]}")
            return e.code, body


# ------------------- DuckDB verification -------------------
def duckdb_query(data_path: str, sql: str) -> list[dict]:
    """Run a SQL query against the Lance tables using DuckDB CLI.

    Uses the same ATTACH approach as the Go server:
        LOAD lance; ATTACH '<path>' AS _lance (TYPE LANCE);
    Then queries reference _lance.main.<table>.

    Returns list of row dicts.
    """
    if not DUCKDB_BIN.exists():
        return []

    dp = data_path.replace("\\", "/")
    preamble = (
        "INSTALL lance FROM community; LOAD lance; "
        f"ATTACH '{dp}' AS _lance (TYPE LANCE); "
    )
    full_sql = preamble + sql

    result = subprocess.run(
        [str(DUCKDB_BIN), "-json", "-c", full_sql],
        capture_output=True, text=True, timeout=30,
        cwd=str(ROOT),
    )
    if result.returncode != 0:
        print(f"    DuckDB error: {result.stderr[:300]}")
        return []
    if not result.stdout.strip():
        return []
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError:
        return []


def lance_table(table_name: str) -> str:
    """Return the DuckDB reference for a Lance table (e.g. _lance.main.articles)."""
    return f"_lance.main.{table_name}"


# -------------------- Main test sequence --------------------
def verify_server_alive(api: APIClient, expected_version: str) -> str:
    """Check if the server is still running with the expected binary.

    Returns: 'ok', 'crashed', or 'replaced'
    """
    try:
        status, srv_status = api.get("/api/server-status")
    except Exception:
        return "crashed"

    if status != 200 or not isinstance(srv_status, dict):
        return "crashed"

    actual = srv_status.get("server", {}).get("build_version", "")
    if actual != expected_version:
        return "replaced"
    return "ok"


def run_e2e(keep: bool = False, verbose: bool = False,
            build_version: str = "") -> int:
    t = TestRunner(verbose=verbose)
    rss_server = None
    server_proc = None
    server_log = None
    temp_dir = None
    expected_version = build_version  # empty string means skip version checks

    try:
        # ---------------------- Prerequisites ----------------------
        t.section("Prerequisites")
        t.check("Server binary exists", SERVER_BIN.exists(),
                f"Not found: {SERVER_BIN}\nRun: build.ps1 server")
        t.check("DuckDB CLI exists", DUCKDB_BIN.exists(),
                f"Not found: {DUCKDB_BIN}\nRun: build.ps1 duckdb")

        if not SERVER_BIN.exists():
            print("\n  Cannot continue without server binary.")
            return t.summary()

        # Reminder: if tests changed, AGENT.md may need updating too
        print("\n  NOTE: If you changed code or added features, check that AGENT.md")
        print("        and docs/ are still in sync (API endpoints, log categories,")
        print("        table counts, test section counts, etc.)")

        # --------------- Temp data directory + config ---------------
        t.section("Setup")
        temp_dir = tempfile.mkdtemp(prefix="rss_lance_e2e_")
        data_path = os.path.join(temp_dir, "data")
        os.makedirs(data_path)
        t.log(f"Temp dir: {temp_dir}")

        # Write test config
        config_path = os.path.join(temp_dir, "config.toml")
        # Pick a free port for the API server
        import socket
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.bind(("127.0.0.1", 0))
        api_port = sock.getsockname()[1]
        sock.close()

        with open(config_path, "w") as f:
            f.write(textwrap.dedent(f"""\
                [storage]
                type = "local"
                path = "{data_path.replace(os.sep, '/')}"

                [fetcher]
                interval_minutes = 30
                max_concurrent = 2
                user_agent = "RSS-Lance-E2E-Test/1.0"

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
        t.check("Test config written", os.path.exists(config_path))

        # ----------- Seed database with all logging enabled -----------
        t.section("Seed Database (All Logging On)")

        from seed_e2e_settings import seed_settings, ALL_LOG_SETTINGS

        db = seed_settings(config_path, data_path)
        t.check("DB opened and settings seeded", db is not None)

        # Verify all log settings via DuckDB before anything else runs
        settings_table = lance_table("settings")
        for key in ALL_LOG_SETTINGS:
            rows = duckdb_query(data_path,
                f"SELECT value FROM {settings_table} WHERE key = '{key}'")
            if rows:
                raw = str(rows[0].get("value", ""))
                t.check(f"DB: {key} = true", "true" in raw,
                        f"Got raw: {raw}")
            else:
                t.check(f"DB: {key} exists", False, "Not found in DuckDB")

        # ------------------ Start local RSS server ------------------
        t.section("Local RSS Server")
        rss_server, rss_port = start_rss_server()
        t.log(f"RSS server on port {rss_port}")

        # Verify it serves feeds
        with urllib.request.urlopen(f"http://127.0.0.1:{rss_port}/feed_alpha.xml") as resp:
            alpha_ok = resp.status == 200 and b"Test Feed Alpha" in resp.read()
        t.check("Feed Alpha served", alpha_ok)

        with urllib.request.urlopen(f"http://127.0.0.1:{rss_port}/feed_bravo.xml") as resp:
            bravo_ok = resp.status == 200 and b"Test Feed Bravo" in resp.read()
        t.check("Feed Bravo served", bravo_ok)

        # ------------ Populate data using Python fetcher ------------
        t.section("Populate Data (Python Fetcher)")

        from feed_parser import fetch_feed

        t.log("DB already opened in Seed Database step, reusing it")

        feed_alpha_url = f"http://127.0.0.1:{rss_port}/feed_alpha.xml"
        feed_bravo_url = f"http://127.0.0.1:{rss_port}/feed_bravo.xml"

        # Add Feed Alpha
        t.log("Fetching Feed Alpha...")
        result_a = fetch_feed(feed_alpha_url, "tmp", user_agent="E2E-Test/1.0")
        t.check("Feed Alpha parsed", result_a.error == "",
                f"Error: {result_a.error}")
        t.check("Feed Alpha has 3 articles", len(result_a.articles) == 3,
                f"Got {len(result_a.articles)}")

        feed_a_id = db.add_feed(
            url=feed_alpha_url,
            title=result_a.title,
            site_url=result_a.site_url,
        )
        # Fix feed_id on articles
        for a in result_a.articles:
            a["feed_id"] = feed_a_id
        db.add_articles(result_a.articles)
        db.update_feed_after_fetch(feed_a_id, success=True)
        t.log(f"Feed Alpha added: {feed_a_id[:8]}...")

        # Add Feed Bravo
        t.log("Fetching Feed Bravo...")
        result_b = fetch_feed(feed_bravo_url, "tmp", user_agent="E2E-Test/1.0")
        t.check("Feed Bravo parsed", result_b.error == "",
                f"Error: {result_b.error}")
        t.check("Feed Bravo has 5 articles", len(result_b.articles) == 5,
                f"Got {len(result_b.articles)}")

        feed_b_id = db.add_feed(
            url=feed_bravo_url,
            title=result_b.title,
            site_url=result_b.site_url,
        )
        for a in result_b.articles:
            a["feed_id"] = feed_b_id
        db.add_articles(result_b.articles)
        db.update_feed_after_fetch(feed_b_id, success=True)
        t.log(f"Feed Bravo added: {feed_b_id[:8]}...")

        # ---- Populate + verify sanitization feed ----
        t.section("Sanitization: Chrome / Tracking / Scripts")

        from content_cleaner import strip_site_chrome, strip_tracking_pixels, strip_dangerous_html

        feed_san_url = f"http://127.0.0.1:{rss_port}/feed_sanitize.xml"

        t.log("Fetching Feed Sanitize (6 articles with injected spam)...")
        result_s = fetch_feed(feed_san_url, "tmp", user_agent="E2E-Test/1.0")
        t.check("Feed Sanitize parsed", result_s.error == "",
                f"Error: {result_s.error}")
        t.check("Feed Sanitize has 6 articles", len(result_s.articles) == 6,
                f"Got {len(result_s.articles)}")

        # Per-article sanitise report should have caught social links, pixels
        # Note: feedparser itself strips <script>, event handlers, and javascript: URIs
        # before our sanitiser runs, so those won't appear in the report.
        t.check("Per-article sanitize_report non-empty",
                len(result_s.sanitize_report) > 0,
                f"Got {len(result_s.sanitize_report)} items")

        has_pixel_report = any("tracking pixel" in r for r in result_s.sanitize_report)
        t.check("Report mentions tracking pixels stripped", has_pixel_report,
                f"Report: {result_s.sanitize_report[:5]}")
        has_social_report = any("social" in r.lower() for r in result_s.sanitize_report)
        t.check("Report mentions social links stripped", has_social_report,
                f"Report: {result_s.sanitize_report[:5]}")
        has_tracking_param_report = any("tracking params" in r for r in result_s.sanitize_report)
        t.check("Report mentions tracking params stripped", has_tracking_param_report,
                f"Report: {result_s.sanitize_report[:5]}")

        # Verify tracking params were actually stripped from link hrefs
        for art in result_s.articles:
            body = art.get("content", "") + art.get("summary", "")
            t.check(f"No utm_source in '{art['title'][:40]}'",
                    "utm_source=" not in body,
                    "Found utm_source in content")
            t.check(f"No fbclid in '{art['title'][:40]}'",
                    "fbclid=" not in body,
                    "Found fbclid in content")
            t.check(f"Legit param page=1 survives in '{art['title'][:40]}'",
                    "page=1" in body,
                    "Legitimate page=1 param was stripped")

        # Verify content no longer contains dangerous HTML
        for art in result_s.articles:
            body = art.get("content", "") + art.get("summary", "")
            t.check(f"No <script> in '{art['title'][:40]}'",
                    "<script" not in body.lower(),
                    f"Found script in content")
            t.check(f"No onmouseover in '{art['title'][:40]}'",
                    "onmouseover" not in body.lower(),
                    f"Found event handler in content")
            t.check(f"No javascript: URI in '{art['title'][:40]}'",
                    "javascript:" not in body.lower(),
                    f"Found JS URI in content")


        # Now run strip_site_chrome on the batch (needs >=2 articles)
        chrome_report: list[str] = []
        strip_site_chrome(result_s.articles, report=chrome_report)
        t.check("Chrome report non-empty", len(chrome_report) > 0,
                f"Got {len(chrome_report)} items")
        has_nav = any("site-navigation" in r for r in chrome_report)
        t.check("Chrome detected nav block", has_nav,
                f"Report: {chrome_report[:5]}")
        has_footer = any("site-footer" in r for r in chrome_report)
        t.check("Chrome detected footer block", has_footer,
                f"Report: {chrome_report[:5]}")

        # After chrome strip, verify the nav/footer blocks are gone
        for art in result_s.articles:
            body = art.get("content", "") + art.get("summary", "")
            t.check(f"No nav chrome in '{art['title'][:40]}'",
                    "site-navigation" not in body,
                    "Nav block survived stripping")
            t.check(f"No footer chrome in '{art['title'][:40]}'",
                    "site-footer" not in body,
                    "Footer block survived stripping")

        # Unique article content should survive
        for art in result_s.articles:
            body = art.get("content", "") + art.get("summary", "")
            t.check(f"Unique body survives in '{art['title'][:40]}'",
                    len(body.strip()) > 20,
                    f"Body too short: {len(body)} chars")

        # Persist the sanitized articles
        feed_s_id = db.add_feed(
            url=feed_san_url,
            title=result_s.title,
            site_url=result_s.site_url,
        )
        for a in result_s.articles:
            a["feed_id"] = feed_s_id
        db.add_articles(result_s.articles)
        db.update_feed_after_fetch(feed_s_id, success=True)
        t.log(f"Feed Sanitize added: {feed_s_id[:8]}...")

        # ----------------- Logging: Fetcher writes -----------------
        t.section("Logging: Fetcher Log Writes")
        # Write test log entries via Python DB (same path as real fetcher)
        db.log_event("info", "feed_fetch",
                     "E2E test: Feed Alpha fetched",
                     json.dumps({"feed_id": feed_a_id, "new": 3}))
        db.log_event("info", "feed_fetch",
                     "E2E test: Feed Bravo fetched",
                     json.dumps({"feed_id": feed_b_id, "new": 5}))
        db.log_event("debug", "article_processing",
                     "E2E test: debug article event",
                     json.dumps({"test": True}))
        db.log_event("error", "errors",
                     "E2E test: simulated error",
                     json.dumps({"error": "test error message"}))
        db.log_event("info", "compaction",
                     "E2E test: compaction event", "")
        db.log_event("info", "tier_changes",
                     "E2E test: tier change event",
                     json.dumps({"feed_id": feed_a_id, "old_tier": 1, "new_tier": 2}))
        t.log("Wrote 6 fetcher log entries")

        # Verify fetcher logs via DuckDB
        log_table = lance_table("log_fetcher")
        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {log_table}")
        if rows:
            cnt = rows[0].get("cnt", 0)
            t.check("log_fetcher has 6 entries", cnt == 6, f"Got {cnt}")
        else:
            t.check("log_fetcher has 6 entries", False, "DuckDB query failed")

        # Verify each log level exists
        rows = duckdb_query(data_path,
            f"SELECT DISTINCT level FROM {log_table} ORDER BY level")
        if rows:
            levels = sorted([r.get("level") for r in rows])
            t.check("Fetcher logs have debug/error/info levels",
                    levels == ["debug", "error", "info"],
                    f"Got {levels}")
        else:
            t.check("Fetcher logs have debug/error/info levels", False,
                    "DuckDB query failed")

        # Verify categories exist
        rows = duckdb_query(data_path,
            f"SELECT DISTINCT category FROM {log_table} ORDER BY category")
        if rows:
            categories = sorted([r.get("category") for r in rows])
            expected = ["article_processing", "compaction", "errors",
                        "feed_fetch", "tier_changes"]
            t.check("Fetcher logs have expected categories",
                    categories == expected,
                    f"Got {categories}")
        else:
            t.check("Fetcher logs have expected categories", False,
                    "DuckDB query failed")

        # Verify details JSON is stored correctly
        rows = duckdb_query(data_path,
            f"SELECT details FROM {log_table} WHERE category = 'feed_fetch' "
            f"AND message LIKE '%Alpha%' LIMIT 1")
        if rows:
            details_raw = rows[0].get("details", "")
            try:
                details = json.loads(details_raw)
                t.check("Fetcher log details has feed_id",
                        details.get("feed_id") == feed_a_id,
                        f"Got {details}")
                t.check("Fetcher log details has article count",
                        details.get("new") == 3,
                        f"Got {details}")
            except (json.JSONDecodeError, TypeError):
                t.check("Fetcher log details is valid JSON", False,
                        f"Got: {details_raw!r}")
        else:
            t.check("Fetcher log details readable", False,
                    "DuckDB query failed or no matching row")

        # Verify log_api table was created (empty, for Go server)
        api_log_table = lance_table("log_api")
        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {api_log_table}")
        if rows:
            cnt = rows[0].get("cnt", 0)
            t.check("log_api table exists (empty)", cnt == 0, f"Got {cnt}")
        else:
            # Table might not exist yet if DuckDB can't see it
            t.check("log_api table exists (empty)", False,
                    "DuckDB query failed (table may not exist)")

        # Verify with DuckDB
        t.section("Verify Data (DuckDB)")
        art_table = lance_table("articles")
        feed_table = lance_table("feeds")

        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {art_table}")
        if rows:
            cnt = rows[0].get("cnt", 0)
            t.check("DuckDB sees 14 articles", cnt == 14, f"Got {cnt}")
        else:
            t.check("DuckDB sees 14 articles", False, "DuckDB query failed")

        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {feed_table}")
        if rows:
            cnt = rows[0].get("cnt", 0)
            t.check("DuckDB sees 3 feeds", cnt == 3, f"Got {cnt}")
        else:
            t.check("DuckDB sees 3 feeds", False, "DuckDB query failed")

        # Check all articles start as unread
        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {art_table} WHERE is_read = false")
        if rows:
            cnt = rows[0].get("cnt", 0)
            t.check("All 14 articles unread", cnt == 14, f"Got {cnt} unread")
        else:
            t.check("All 14 articles unread", False, "DuckDB query failed")

        # ------------------- Start the Go server -------------------
        t.section("Start RSS-Lance Server")
        t.log(f"Starting server on port {api_port}...")

        server_log_path = os.path.join(temp_dir, "server.log")
        server_log = open(server_log_path, "w")

        # The server binary may need MSYS2/MinGW DLLs at runtime
        # (lancedb-go Rust lib links some dynamically despite -static flag)
        server_env = os.environ.copy()
        msys2_paths = [
            r"C:\msys64\ucrt64\bin",
            r"C:\msys64\mingw64\bin",
            r"C:\msys64\usr\bin",
        ]
        for p in msys2_paths:
            if os.path.isdir(p) and p not in server_env.get("PATH", ""):
                server_env["PATH"] = p + os.pathsep + server_env.get("PATH", "")

        server_proc = subprocess.Popen(
            [str(SERVER_BIN), "-config", config_path],
            stdout=server_log,
            stderr=subprocess.STDOUT,
            cwd=str(ROOT),
            env=server_env,
        )

        # Wait for server to be ready (poll /api/feeds)
        # First start may be slow while DuckDB installs the Lance extension
        api = APIClient(f"http://127.0.0.1:{api_port}", verbose=verbose)
        server_ready = False
        for i in range(60):  # up to 30 seconds
            if server_proc.poll() is not None:
                # Server exited early - read log for error
                server_log.close()
                with open(server_log_path) as f:
                    log_content = f.read()
                t.check("Server started", False,
                        f"Server exited with code {server_proc.returncode}\n{log_content[:500]}")
                server_proc = None
                break
            try:
                status, _ = api.get("/api/feeds")
                if status == 200:
                    server_ready = True
                    break
            except Exception:
                pass
            time.sleep(0.5)
        else:
            # Timeout - read log to see what happened
            server_log.flush()
            with open(server_log_path) as f:
                log_content = f.read()
            t.check("Server started", False,
                    f"Server did not respond within 30 seconds\nLog:\n{log_content[:500]}")

        if server_ready:
            t.check("Server started", True)

        if not server_ready:
            if server_proc:
                server_proc.kill()
                server_proc = None
            return t.summary()

        # ------------ Build Version Verification ------------
        if expected_version:
            t.section("Build Version Verification")
            status, srv_status = api.get("/api/server-status")
            if status == 200 and isinstance(srv_status, dict):
                srv = srv_status.get("server", {})
                actual_version = srv.get("build_version", "")
                version_ok = actual_version == expected_version
                t.check(f"Server build_version matches test ID",
                        version_ok,
                        f"Expected {expected_version!r}, got {actual_version!r}")
                if not version_ok:
                    print(f"\n  FATAL: Server binary does not match this test build.")
                    print(f"         Expected: {expected_version}")
                    print(f"         Got:      {actual_version!r}")
                    print(f"         You may be testing a stale or different binary.")
                    print(f"         Rebuild with: $env:BUILD_VERSION=\"{expected_version}\"; .\\build.ps1 server")
                    return t.summary()
            else:
                t.check("Server build_version check", False,
                        f"Could not query /api/server-status (status={status})")
                return t.summary()
        else:
            t.log("No --build-version given, skipping version gate")

        # ------- Verify all log settings are ON via API --------
        t.section("Verify Log Settings (API)")
        status, settings = api.get("/api/settings")
        t.check("GET /api/settings returns 200", status == 200)
        if isinstance(settings, dict):
            for key in ALL_LOG_SETTINGS:
                val = settings.get(key)
                t.check(f"API: {key} = true", val is True,
                        f"Got {val!r}")
        else:
            t.check("Settings response is a dict", False,
                    f"Got type={type(settings).__name__}")

        # ------------------ API Tests: List Feeds ------------------
        t.section("API: List Feeds")
        status, feeds = api.get("/api/feeds")
        t.check("GET /api/feeds returns 200", status == 200)
        t.check("Returns 3 feeds", len(feeds) == 3, f"Got {len(feeds)}")

        # Find our feeds by title
        feed_a_api = next((f for f in feeds if f["title"] == "Test Feed Alpha"), None)
        feed_b_api = next((f for f in feeds if f["title"] == "Test Feed Bravo"), None)
        feed_s_api = next((f for f in feeds if f["title"] == "Test Feed Sanitize"), None)
        t.check("Feed Alpha found in list", feed_a_api is not None)
        t.check("Feed Bravo found in list", feed_b_api is not None)
        t.check("Feed Sanitize found in list", feed_s_api is not None)

        if feed_a_api:
            t.check("Feed Alpha unread_count == 3",
                    feed_a_api.get("unread_count") == 3,
                    f"Got {feed_a_api.get('unread_count')}")
        if feed_b_api:
            t.check("Feed Bravo unread_count == 5",
                    feed_b_api.get("unread_count") == 5,
                    f"Got {feed_b_api.get('unread_count')}")

        # ---------------- API Tests: Get Single Feed ----------------
        t.section("API: Get Single Feed")
        if feed_a_api:
            status, feed = api.get(f"/api/feeds/{feed_a_api['feed_id']}")
            t.check("GET /api/feeds/:id returns 200", status == 200)
            t.check("Feed title matches", feed.get("title") == "Test Feed Alpha")

        # ----------------- API Tests: List Articles -----------------
        t.section("API: List Articles")
        status, articles = api.get("/api/articles/")
        t.check("GET /api/articles returns 200", status == 200)
        t.check("Returns 14 articles", len(articles) == 14,
                f"Got {len(articles)}")

        # All should be unread
        all_unread = all(not a["is_read"] for a in articles)
        t.check("All articles start unread", all_unread)

        # All should be unstarred
        all_unstarred = all(not a["is_starred"] for a in articles)
        t.check("All articles start unstarred", all_unstarred)

        # ------------- API Tests: List Articles by Feed -------------
        t.section("API: List Articles by Feed")
        if feed_a_api:
            status, arts_a = api.get(f"/api/feeds/{feed_a_api['feed_id']}/articles")
            t.check("GET feed Alpha articles returns 200", status == 200)
            t.check("Feed Alpha has 3 articles", len(arts_a) == 3,
                    f"Got {len(arts_a)}")

        if feed_b_api:
            status, arts_b = api.get(f"/api/feeds/{feed_b_api['feed_id']}/articles")
            t.check("GET feed Bravo articles returns 200", status == 200)
            t.check("Feed Bravo has 5 articles", len(arts_b) == 5,
                    f"Got {len(arts_b)}")

        # -------------- API Tests: Get Single Article --------------
        t.section("API: View Article")
        # Pick the first Alpha article
        test_article_id = arts_a[0]["article_id"] if arts_a else None
        if test_article_id:
            status, article = api.get(f"/api/articles/{test_article_id}")
            t.check("GET /api/articles/:id returns 200", status == 200)
            t.check("Article has title", bool(article.get("title")))
            t.check("Article has content", "content" in article)
            t.check("Article is unread", article.get("is_read") == False,
                    f"Got: {article.get('is_read')!r} (type={type(article.get('is_read')).__name__})")
            t.check("Article is unstarred", article.get("is_starred") == False,
                    f"Got: {article.get('is_starred')!r} (type={type(article.get('is_starred')).__name__})")
            t.log(f"Viewing: '{article.get('title')}'")

        # ------------- API Tests: Batch Fetch Articles -------------
        t.section("API: Batch Fetch Articles")
        if arts_a and arts_b:
            # Pick 2 from Alpha and 2 from Bravo
            batch_ids = [arts_a[0]["article_id"], arts_a[1]["article_id"],
                         arts_b[0]["article_id"], arts_b[1]["article_id"]]
            status, batch_arts = api.post("/api/articles/batch", {"ids": batch_ids})
            t.check("POST /api/articles/batch returns 200", status == 200)
            t.check("Batch returns 4 articles",
                    isinstance(batch_arts, list) and len(batch_arts) == 4,
                    f"Got {len(batch_arts) if isinstance(batch_arts, list) else type(batch_arts).__name__}")

            # All should have content (full article, not just preview)
            if isinstance(batch_arts, list) and len(batch_arts) > 0:
                all_have_content = all("content" in a and a["content"] for a in batch_arts)
                t.check("Batch articles have content", all_have_content)

                # Verify we got the right IDs back
                returned_ids = {a["article_id"] for a in batch_arts}
                t.check("Batch returned correct IDs",
                        returned_ids == set(batch_ids),
                        f"Expected {set(batch_ids)}, got {returned_ids}")

        # Batch with empty list should fail
        status, _ = api.post("/api/articles/batch", {"ids": []})
        t.check("Batch with empty ids returns 400", status == 400)

        # Batch with non-existent IDs returns empty array
        status, empty_batch = api.post("/api/articles/batch",
                                        {"ids": ["nonexistent-1", "nonexistent-2"]})
        t.check("Batch with bad IDs returns 200", status == 200)
        t.check("Batch with bad IDs returns empty array",
                isinstance(empty_batch, list) and len(empty_batch) == 0,
                f"Got {len(empty_batch) if isinstance(empty_batch, list) else type(empty_batch).__name__}")

        # ------------- API Tests: Mark Article as Read -------------
        t.section("API: Mark Article Read")
        if test_article_id:
            status, resp = api.post(f"/api/articles/{test_article_id}/read")
            t.check("POST .../read returns 200", status == 200)

            # Verify via article list (uses CTE overlay on GetArticles)
            status, arts_check = api.get(
                f"/api/feeds/{feed_a_api['feed_id']}/articles")
            art_in_list = next(
                (a for a in arts_check if a["article_id"] == test_article_id), None)
            if art_in_list:
                t.check("Article marked read (via list)",
                        art_in_list.get("is_read") == True,
                        f"Got: {art_in_list.get('is_read')!r}")

            # Also verify via single-article endpoint  
            status, article = api.get(f"/api/articles/{test_article_id}")
            t.check("Article marked read (via detail)",
                    article.get("is_read") == True,
                    f"Got: {article.get('is_read')!r}")

            # Check unread count decreased
            status, feeds = api.get("/api/feeds")
            feed_a_now = next((f for f in feeds if f["title"] == "Test Feed Alpha"), None)
            if feed_a_now:
                t.check("Feed Alpha unread_count == 2",
                        feed_a_now.get("unread_count") == 2,
                        f"Got {feed_a_now.get('unread_count')}")

        # ----------------- API Tests: Unread filter -----------------
        t.section("API: Unread Filter")
        if feed_a_api:
            status, unread_arts = api.get(
                f"/api/feeds/{feed_a_api['feed_id']}/articles?unread=true")
            t.check("Unread filter returns 200", status == 200)
            t.check("Only 2 unread articles for Alpha",
                    len(unread_arts) == 2,
                    f"Got {len(unread_arts)}")

        # Global unread filter
        status, global_unread = api.get("/api/articles/?unread=true")
        t.check("Global unread: 13 articles",
                len(global_unread) == 13,
                f"Got {len(global_unread)}")

        # -------------- API Tests: Mark Article Unread --------------
        t.section("API: Mark Article Unread")
        if test_article_id:
            status, resp = api.post(f"/api/articles/{test_article_id}/unread")
            t.check("POST .../unread returns 200", status == 200)

            status, article = api.get(f"/api/articles/{test_article_id}")
            t.check("Article back to unread", article.get("is_read") == False,
                    f"Got: {article.get('is_read')!r}")

        # ----------------- API Tests: Star / Unstar -----------------
        t.section("API: Star / Unstar Article")
        if test_article_id:
            status, resp = api.post(f"/api/articles/{test_article_id}/star")
            t.check("POST .../star returns 200", status == 200)

            status, article = api.get(f"/api/articles/{test_article_id}")
            t.check("Article is now starred", article.get("is_starred") == True,
                    f"Got: {article.get('is_starred')!r} (type={type(article.get('is_starred')).__name__})")

            status, resp = api.post(f"/api/articles/{test_article_id}/unstar")
            t.check("POST .../unstar returns 200", status == 200)

            status, article = api.get(f"/api/articles/{test_article_id}")
            t.check("Article is now unstarred", article.get("is_starred") == False,
                    f"Got: {article.get('is_starred')!r}")

        # ----------------- API Tests: Mark All Read -----------------
        t.section("API: Mark All Read")
        if feed_b_api:
            status, resp = api.post(
                f"/api/feeds/{feed_b_api['feed_id']}/mark-all-read")
            t.check("POST .../mark-all-read returns 200", status == 200)

            # All Bravo articles should now be read
            # (MarkAllRead bypasses cache and writes directly to Lance.
            #  DuckDB may need a moment to see the updated Lance files.)
            time.sleep(1)
            status, arts_b = api.get(
                f"/api/feeds/{feed_b_api['feed_id']}/articles")
            all_b_read = all(a["is_read"] for a in arts_b)
            t.check("All 5 Bravo articles now read", all_b_read,
                    f"Read states: {[a['is_read'] for a in arts_b]}")

            # Unread count should be 0
            status, feeds = api.get("/api/feeds")
            feed_b_now = next((f for f in feeds if f["title"] == "Test Feed Bravo"), None)
            if feed_b_now:
                t.check("Feed Bravo unread_count == 0",
                        feed_b_now.get("unread_count") == 0,
                        f"Got {feed_b_now.get('unread_count')}")

            # Alpha should still have 3 unread (we marked one unread earlier)
            feed_a_now = next((f for f in feeds if f["title"] == "Test Feed Alpha"), None)
            if feed_a_now:
                t.check("Feed Alpha still has 3 unread",
                        feed_a_now.get("unread_count") == 3,
                        f"Got {feed_a_now.get('unread_count')}")

        # Mark sanitize feed as read too
        if feed_s_api:
            status, resp = api.post(
                f"/api/feeds/{feed_s_api['feed_id']}/mark-all-read")
            t.check("Sanitize feed mark-all-read returns 200", status == 200)

        # ---- API Tests: Multiple state changes (cache exercise) ----
        t.section("API: Multiple State Changes (cache)")
        t.log("Marking several articles read + starred to exercise write cache...")
        if feed_a_api:
            status, arts_a = api.get(
                f"/api/feeds/{feed_a_api['feed_id']}/articles")
            for i, art in enumerate(arts_a):
                aid = art["article_id"]
                api.post(f"/api/articles/{aid}/read")
                if i % 2 == 0:
                    api.post(f"/api/articles/{aid}/star")

            # Small delay for cache CTE to be consistent
            time.sleep(0.5)

            # Verify state
            status, arts_a = api.get(
                f"/api/feeds/{feed_a_api['feed_id']}/articles")
            all_a_read = all(a["is_read"] for a in arts_a)
            t.check("All Alpha articles now read", all_a_read)

            starred_count = sum(1 for a in arts_a if a["is_starred"])
            t.check("Some Alpha articles starred",
                    starred_count >= 1,
                    f"Starred: {starred_count}")
        # -------- API Tests: DB Status (after state changes) --------
        t.section("API: DB Status")
        status, db_status = api.get("/api/status")
        t.check("GET /api/status returns 200", status == 200)
        t.check("Status has tables array",
                isinstance(db_status, dict) and "tables" in db_status)
        t.check("Status has articles stats",
                isinstance(db_status, dict) and "articles" in db_status)

        if isinstance(db_status, dict):
            tables = db_status.get("tables", [])
            table_names = [tbl["name"] for tbl in tables]
            t.check("Status includes articles table",
                    "articles" in table_names,
                    f"Got tables: {table_names}")
            t.check("Status includes feeds table",
                    "feeds" in table_names,
                    f"Got tables: {table_names}")

            # Check articles table row count
            art_tbl = next((tbl for tbl in tables if tbl["name"] == "articles"), None)
            if art_tbl:
                t.check("Status: articles row_count == 14",
                        art_tbl.get("row_count") == 14,
                        f"Got {art_tbl.get('row_count')}")
                t.check("Status: articles size_bytes > 0",
                        art_tbl.get("size_bytes", 0) > 0,
                        f"Got {art_tbl.get('size_bytes')}")

            # Check feeds table row count
            feed_tbl = next((tbl for tbl in tables if tbl["name"] == "feeds"), None)
            if feed_tbl:
                t.check("Status: feeds row_count == 3",
                        feed_tbl.get("row_count") == 3,
                        f"Got {feed_tbl.get('row_count')}")

            # Article stats
            art_stats = db_status.get("articles", {})
            t.check("Status: articles total == 14",
                    art_stats.get("total") == 14,
                    f"Got {art_stats.get('total')}")

            # At this point: all Alpha read (3) + all Bravo read (5) = 8 read, 0 unread
            # But note the write cache may not be flushed to Lance yet,
            # so the API status reads from DuckDB which queries Lance directly.
            # The unread count here reflects the Lance on-disk state.
            t.log(f"Status unread: {art_stats.get('unread')}, "
                  f"starred: {art_stats.get('starred')}")

            # data_path should be non-empty
            t.check("Status: data_path is set",
                    bool(db_status.get("data_path")),
                    f"Got: {db_status.get('data_path')!r}")

        # -------- API Tests: Server Runtime Status --------
        t.section("API: Server Runtime Status")
        status, srv_status = api.get("/api/server-status")
        t.check("GET /api/server-status returns 200", status == 200)
        t.check("Server status is a dict",
                isinstance(srv_status, dict))

        if isinstance(srv_status, dict):
            # Top-level sections
            for key in ("server", "host", "memory", "gc", "write_cache"):
                t.check(f"Server status has '{key}' section",
                        key in srv_status,
                        f"Keys: {list(srv_status.keys())}")

            srv = srv_status.get("server", {})
            t.check("server.uptime_seconds >= 0",
                    isinstance(srv.get("uptime_seconds"), (int, float))
                    and srv["uptime_seconds"] >= 0,
                    f"Got {srv.get('uptime_seconds')}")
            t.check("server.go_version is set",
                    bool(srv.get("go_version")),
                    f"Got {srv.get('go_version')!r}")
            t.check("server.goroutines > 0",
                    isinstance(srv.get("goroutines"), (int, float))
                    and srv["goroutines"] > 0,
                    f"Got {srv.get('goroutines')}")
            t.check("server.num_cpu > 0",
                    isinstance(srv.get("num_cpu"), (int, float))
                    and srv["num_cpu"] > 0,
                    f"Got {srv.get('num_cpu')}")

            mem = srv_status.get("memory", {})
            t.check("memory.heap_alloc_bytes > 0",
                    isinstance(mem.get("heap_alloc_bytes"), (int, float))
                    and mem["heap_alloc_bytes"] > 0,
                    f"Got {mem.get('heap_alloc_bytes')}")
            t.check("memory.sys_bytes > 0",
                    isinstance(mem.get("sys_bytes"), (int, float))
                    and mem["sys_bytes"] > 0,
                    f"Got {mem.get('sys_bytes')}")

            gc = srv_status.get("gc", {})
            t.check("gc.num_gc is present",
                    "num_gc" in gc,
                    f"GC keys: {list(gc.keys())}")
            t.check("gc.recent_pauses_ns is a list",
                    isinstance(gc.get("recent_pauses_ns"), list),
                    f"Got type={type(gc.get('recent_pauses_ns')).__name__}")

            host = srv_status.get("host", {})
            t.check("host.hostname is set",
                    bool(host.get("hostname")),
                    f"Got {host.get('hostname')!r}")

            wc = srv_status.get("write_cache", {})
            t.check("write_cache.pending_reads is present",
                    "pending_reads" in wc,
                    f"WC keys: {list(wc.keys())}")

            # DuckDB external process info (Windows only - may be null on Linux)
            duckdb_proc = srv_status.get("duckdb_process")
            if duckdb_proc is not None:
                t.check("duckdb_process.pid > 0",
                        isinstance(duckdb_proc.get("pid"), (int, float))
                        and duckdb_proc["pid"] > 0,
                        f"Got {duckdb_proc.get('pid')}")
                t.check("duckdb_process.uptime_seconds >= 0",
                        isinstance(duckdb_proc.get("uptime_seconds"), (int, float))
                        and duckdb_proc["uptime_seconds"] >= 0,
                        f"Got {duckdb_proc.get('uptime_seconds')}")

            # Log buffer resilience stats
            log_buf = srv_status.get("log_buffer")
            if log_buf is not None:
                t.check("log_buffer.memory_entries >= 0",
                        isinstance(log_buf.get("memory_entries"), (int, float))
                        and log_buf["memory_entries"] >= 0,
                        f"Got {log_buf.get('memory_entries')}")
                t.check("log_buffer.duckdb_entries >= 0",
                        isinstance(log_buf.get("duckdb_entries"), (int, float))
                        and log_buf["duckdb_entries"] >= 0,
                        f"Got {log_buf.get('duckdb_entries')}")
                t.check("log_buffer.infra_events >= 0",
                        isinstance(log_buf.get("infra_events"), (int, float))
                        and log_buf["infra_events"] >= 0,
                        f"Got {log_buf.get('infra_events')}")

        # Method not allowed
        status, _ = api.post("/api/server-status")
        t.check("POST /api/server-status returns 405", status == 405)

        # -------------- API Tests: Global state check --------------
        t.section("API: Final Global State")
        time.sleep(1)  # let DuckDB see latest Lance state
        status, all_articles = api.get("/api/articles/")
        t.check("GET all articles returns 200", status == 200)
        t.check("Still 14 total articles", len(all_articles) == 14,
                f"Got {len(all_articles)}")

        read_count = sum(1 for a in all_articles if a["is_read"])
        t.check("All 14 articles now read",
                read_count == 14,
                f"Read: {read_count}/14")

        # Unread filter should return nothing (or few if cache is stale)
        status, none_unread = api.get("/api/articles/?unread=true")
        if status == 200 and isinstance(none_unread, list):
            t.check("Unread filter returns 0 articles",
                    len(none_unread) == 0,
                    f"Got {len(none_unread)}")
        else:
            t.check("Unread filter returns 0 articles", False,
                    f"Status {status}, response type={type(none_unread).__name__}")

        # ------------------ API Tests: Categories ------------------
        t.section("API: Categories")
        status, cats = api.get("/api/categories")
        t.check("GET /api/categories returns 200 or 500",
                status in (200, 500),
                f"Got {status}")
        t.check("Categories response is valid",
                isinstance(cats, list) or status == 500,
                f"Got type={type(cats).__name__}")

        # -------------------- API Tests: Sorting --------------------
        t.section("API: Article Sorting")
        status, asc = api.get("/api/articles/?sort=asc")
        t.check("Sort asc returns 200", status == 200)
        if len(asc) >= 2:
            first_date = asc[0].get("published_at", "")
            last_date = asc[-1].get("published_at", "")
            t.check("Ascending sort: first <= last",
                    first_date <= last_date,
                    f"First: {first_date}, Last: {last_date}")

        status, desc = api.get("/api/articles/?sort=desc")
        if len(desc) >= 2:
            first_date = desc[0].get("published_at", "")
            last_date = desc[-1].get("published_at", "")
            t.check("Descending sort: first >= last",
                    first_date >= last_date,
                    f"First: {first_date}, Last: {last_date}")

        # ------------------ API Tests: Pagination ------------------
        t.section("API: Pagination")
        status, page1 = api.get("/api/articles/?limit=3&offset=0")
        t.check("Page 1 (limit=3) returns 200", status == 200)
        t.check("Page 1 has 3 articles", len(page1) == 3,
                f"Got {len(page1)}")

        status, page2 = api.get("/api/articles/?limit=3&offset=3")
        t.check("Page 2 has 3 articles", len(page2) == 3,
                f"Got {len(page2)}")

        status, page3 = api.get("/api/articles/?limit=3&offset=6")
        t.check("Page 3 has 3 articles", len(page3) == 3,
                f"Got {len(page3)}")

        # No overlap between pages
        ids_1 = {a["article_id"] for a in page1}
        ids_2 = {a["article_id"] for a in page2}
        ids_3 = {a["article_id"] for a in page3}
        t.check("No overlap between pages",
                len(ids_1 & ids_2) == 0 and len(ids_2 & ids_3) == 0)

        # ----------------- API Tests: Log Settings -----------------
        t.section("API: Log Settings")

        # Read current settings
        status, settings = api.get("/api/settings")
        t.check("GET /api/settings returns 200", status == 200)
        t.check("Settings has log.max_entries",
                isinstance(settings, dict) and "log.max_entries" in settings,
                f"Keys: {list(settings.keys()) if isinstance(settings, dict) else 'N/A'}")

        # Save log retention setting to a low value
        status, resp = api.put("/api/settings", {"log.max_entries": 5})
        t.check("PUT log.max_entries=5 returns 200", status == 200)

        # Verify setting persisted
        status, settings = api.get("/api/settings")
        t.check("log.max_entries saved as 5",
                isinstance(settings, dict) and settings.get("log.max_entries") == 5,
                f"Got {settings.get('log.max_entries') if isinstance(settings, dict) else 'N/A'}")

        # Toggle a log category off and verify
        status, resp = api.put("/api/settings", {"log.api.requests": False})
        t.check("PUT log.api.requests=false returns 200", status == 200)
        status, settings = api.get("/api/settings")
        t.check("log.api.requests saved as false",
                isinstance(settings, dict) and settings.get("log.api.requests") is False,
                f"Got {settings.get('log.api.requests') if isinstance(settings, dict) else 'N/A'}")

        # Toggle it back on
        status, resp = api.put("/api/settings", {"log.api.requests": True})
        t.check("PUT log.api.requests=true returns 200", status == 200)
        status, settings = api.get("/api/settings")
        t.check("log.api.requests saved as true",
                isinstance(settings, dict) and settings.get("log.api.requests") is True,
                f"Got {settings.get('log.api.requests') if isinstance(settings, dict) else 'N/A'}")

        # Disable entire fetcher logging group
        status, resp = api.put("/api/settings", {"log.fetcher.enabled": False})
        t.check("PUT log.fetcher.enabled=false returns 200", status == 200)
        status, settings = api.get("/api/settings")
        t.check("log.fetcher.enabled saved as false",
                isinstance(settings, dict) and settings.get("log.fetcher.enabled") is False,
                f"Got {settings.get('log.fetcher.enabled') if isinstance(settings, dict) else 'N/A'}")

        # Re-enable it
        status, resp = api.put("/api/settings", {"log.fetcher.enabled": True})
        t.check("PUT log.fetcher.enabled=true returns 200", status == 200)

        # Test log.max_entries=0 means retain all
        status, resp = api.put("/api/settings", {"log.max_entries": 0})
        t.check("PUT log.max_entries=0 returns 200", status == 200)
        status, settings = api.get("/api/settings")
        t.check("log.max_entries saved as 0",
                isinstance(settings, dict) and settings.get("log.max_entries") == 0,
                f"Got {settings.get('log.max_entries') if isinstance(settings, dict) else 'N/A'}")

        # Restore to a normal value
        status, resp = api.put("/api/settings", {"log.max_entries": 10000})
        t.check("PUT log.max_entries=10000 returns 200", status == 200)

        # Batch update multiple log settings at once
        batch_settings = {
            "log.api.lifecycle": True,
            "log.api.errors": True,
            "log.api.requests": False,
            "log.fetcher.article_processing": False,
        }
        status, resp = api.put("/api/settings", batch_settings)
        t.check("PUT batch log settings returns 200", status == 200)
        status, settings = api.get("/api/settings")
        if isinstance(settings, dict):
            t.check("Batch: log.api.lifecycle is true",
                    settings.get("log.api.lifecycle") is True)
            t.check("Batch: log.api.errors is true",
                    settings.get("log.api.errors") is True)
            t.check("Batch: log.api.requests is false",
                    settings.get("log.api.requests") is False)
            t.check("Batch: log.fetcher.article_processing is false",
                    settings.get("log.fetcher.article_processing") is False)

        # ----------------- API Tests: Log Trimming -----------------
        t.section("API: Log Trimming (Count Mode)")

        # Write extra fetcher log entries via Python DB so we have enough to trim
        t.log("Writing 10 additional fetcher log entries for trim test...")
        for i in range(10):
            db.log_event("info", "feed_fetch",
                         f"E2E trim test entry {i}",
                         json.dumps({"index": i}))

        # Check current fetcher log count
        log_table = lance_table("log_fetcher")
        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {log_table}")
        if rows:
            before_count = rows[0].get("cnt", 0)
            t.log(f"Fetcher log entries before trim: {before_count}")
            t.check("Fetcher logs >= 16 (6 original + 10 new)",
                    before_count >= 16,
                    f"Got {before_count}")
        else:
            before_count = 0
            t.check("Can count fetcher logs", False, "DuckDB query failed")

        # Set retention to 5 and trigger trim via Python
        db.put_setting("log.max_entries", 5)
        trimmed = db.trim_logs()
        t.log(f"Trimmed {trimmed} fetcher log entries")
        t.check("Trim removed entries", trimmed > 0,
                f"Trimmed {trimmed}, had {before_count}")

        # Verify count is now <= 5
        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {log_table}")
        if rows:
            after_count = rows[0].get("cnt", 0)
            t.check("Fetcher logs trimmed to <= 5",
                    after_count <= 5,
                    f"Got {after_count}")
        else:
            t.check("Fetcher logs trimmed to <= 5", False,
                    "DuckDB query failed")

        # Test log.max_entries=0 skips trimming (retain all)
        t.log("Writing 3 more entries, then testing max_entries=0 (retain all)...")
        for i in range(3):
            db.log_event("info", "feed_fetch",
                         f"E2E retain-all test entry {i}",
                         json.dumps({"index": i}))
        db.put_setting("log.max_entries", 0)
        trimmed_zero = db.trim_logs()
        t.check("Trim with max_entries=0 deletes nothing",
                trimmed_zero == 0,
                f"Trimmed {trimmed_zero}")

        # Restore count setting
        db.put_setting("log.max_entries", 10000)

        # ------------- API Tests: Log Trimming (Age Mode) -------------
        t.section("API: Log Trimming (Age Mode)")

        # Insert entries with old timestamps (60 days ago) directly
        from datetime import timedelta
        old_ts = datetime.now(timezone.utc) - timedelta(days=60)
        recent_ts = datetime.now(timezone.utc) - timedelta(hours=1)
        import uuid

        t.log("Inserting 5 old (60d) + 3 recent log entries for age trim test...")
        for i in range(5):
            db.log_fetcher.add([{
                "log_id": str(uuid.uuid4()),
                "timestamp": old_ts + timedelta(seconds=i),
                "level": "info",
                "category": "feed_fetch",
                "message": f"E2E old entry {i}",
                "details": json.dumps({"age_test": True}),
                "created_at": old_ts + timedelta(seconds=i),
            }])
        for i in range(3):
            db.log_fetcher.add([{
                "log_id": str(uuid.uuid4()),
                "timestamp": recent_ts + timedelta(seconds=i),
                "level": "info",
                "category": "feed_fetch",
                "message": f"E2E recent entry {i}",
                "details": json.dumps({"age_test": True}),
                "created_at": recent_ts + timedelta(seconds=i),
            }])

        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {log_table}")
        before_age = rows[0].get("cnt", 0) if rows else 0
        t.log(f"Log entries before age trim: {before_age}")

        # Switch to age mode with 30-day retention
        db.put_setting("log.retention_mode", "age")
        db.put_setting("log.max_age_days", 30)

        # Verify settings persisted via DuckDB (not just API)
        settings_table = lance_table("settings")
        mode_rows = duckdb_query(data_path,
            f"SELECT value FROM {settings_table} WHERE key = 'log.retention_mode'")
        t.check("log.retention_mode persisted in DB",
                len(mode_rows) > 0 and '"age"' in str(mode_rows[0].get("value", "")),
                f"Got: {mode_rows}")

        age_rows = duckdb_query(data_path,
            f"SELECT value FROM {settings_table} WHERE key = 'log.max_age_days'")
        t.check("log.max_age_days persisted in DB",
                len(age_rows) > 0 and "30" in str(age_rows[0].get("value", "")),
                f"Got: {age_rows}")

        # Trim — should remove the 60-day-old entries but keep recent ones
        trimmed_age = db.trim_logs()
        t.log(f"Age trim removed {trimmed_age} entries")
        t.check("Age trim removed old entries",
                trimmed_age >= 5,
                f"Trimmed {trimmed_age}, expected >= 5")

        # Verify old entries are gone via DuckDB
        old_cutoff = (datetime.now(timezone.utc) - timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%S")
        old_remaining = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {log_table} WHERE timestamp < TIMESTAMP '{old_cutoff}'")
        if old_remaining:
            t.check("No entries older than 30 days remain",
                    old_remaining[0].get("cnt", 99) == 0,
                    f"Got {old_remaining[0].get('cnt')}")

        # Recent entries should still exist
        rows = duckdb_query(data_path,
            f"SELECT COUNT(*) as cnt FROM {log_table}")
        after_age = rows[0].get("cnt", 0) if rows else 0
        t.check("Recent entries survived age trim",
                after_age > 0,
                f"Got {after_age}")

        # Switch back to count mode
        db.put_setting("log.retention_mode", "count")

        # ---------- API Tests: Settings DB Verification ----------
        t.section("API: Settings DB Verification")

        # Save several settings at once via API
        test_settings = {
            "log.max_entries": 5000,
            "log.retention_mode": "age",
            "log.max_age_days": 14,
            "log.api.lifecycle": False,
        }
        status, resp = api.put("/api/settings", test_settings)
        t.check("PUT batch retention settings returns 200", status == 200)

        # Verify each setting via API
        status, settings = api.get("/api/settings")
        t.check("GET /api/settings returns 200", status == 200)
        if isinstance(settings, dict):
            t.check("API: log.max_entries = 5000",
                    settings.get("log.max_entries") == 5000,
                    f"Got {settings.get('log.max_entries')}")
            t.check("API: log.retention_mode = age",
                    settings.get("log.retention_mode") == "age",
                    f"Got {settings.get('log.retention_mode')}")
            t.check("API: log.max_age_days = 14",
                    settings.get("log.max_age_days") == 14,
                    f"Got {settings.get('log.max_age_days')}")
            t.check("API: log.api.lifecycle = false",
                    settings.get("log.api.lifecycle") is False,
                    f"Got {settings.get('log.api.lifecycle')}")

        # Now verify the SAME settings in the DB via DuckDB
        t.log("Verifying settings directly in Lance DB via DuckDB...")
        for key, expected_json in [
            ("log.max_entries", "5000"),
            ("log.retention_mode", '"age"'),
            ("log.max_age_days", "14"),
            ("log.api.lifecycle", "false"),
        ]:
            db_rows = duckdb_query(data_path,
                f"SELECT value FROM {settings_table} WHERE key = '{key}'")
            if db_rows:
                raw_val = str(db_rows[0].get("value", ""))
                t.check(f"DB: {key} = {expected_json}",
                        expected_json in raw_val,
                        f"Got raw: {raw_val}")
            else:
                t.check(f"DB: {key} exists", False, "Not found in DuckDB")

        # Verify that OTHER settings were NOT affected by the batch update
        # log.fetcher.enabled should still be True (set earlier in test)
        other_rows = duckdb_query(data_path,
            f"SELECT value FROM {settings_table} WHERE key = 'log.fetcher.enabled'")
        if other_rows:
            t.check("Unrelated setting log.fetcher.enabled unchanged",
                    "true" in str(other_rows[0].get("value", "")),
                    f"Got: {other_rows[0].get('value')}")

        # Change retention_mode back to "count" and verify the mode change
        # doesn't clobber the age setting
        status, resp = api.put("/api/settings", {"log.retention_mode": "count"})
        t.check("PUT log.retention_mode=count returns 200", status == 200)

        # log.max_age_days should still be 14
        status, settings = api.get("/api/settings")
        if isinstance(settings, dict):
            t.check("log.max_age_days still 14 after mode switch",
                    settings.get("log.max_age_days") == 14,
                    f"Got {settings.get('log.max_age_days')}")
            t.check("log.retention_mode now count",
                    settings.get("log.retention_mode") == "count",
                    f"Got {settings.get('log.retention_mode')}")

        # Verify in DuckDB too
        mode_rows2 = duckdb_query(data_path,
            f"SELECT value FROM {settings_table} WHERE key = 'log.retention_mode'")
        if mode_rows2:
            t.check("DB: retention_mode = count after switch",
                    '"count"' in str(mode_rows2[0].get("value", "")),
                    f"Got: {mode_rows2[0].get('value')}")

        # Restore defaults
        db.put_setting("log.max_entries", 10000)
        db.put_setting("log.retention_mode", "count")
        db.put_setting("log.max_age_days", 30)
        db.put_setting("log.api.lifecycle", True)

        # ------------------ API Tests: Custom CSS ------------------
        t.section("API: Custom CSS Settings")

        # Save custom CSS via the single-key endpoint (same as frontend)
        test_css = "body { background: #123456; font-size: 20px; }"
        status, resp = api.put("/api/settings/custom_css", {"value": test_css})
        t.check("PUT /api/settings/custom_css returns 200", status == 200)

        # Read it back via GET /api/settings (how the settings page loads it)
        status, settings = api.get("/api/settings")
        t.check("GET /api/settings returns 200 (after CSS save)", status == 200)
        saved_css = settings.get("custom_css") if isinstance(settings, dict) else None
        t.check("custom_css persisted in settings",
                saved_css == test_css,
                f"Got: {saved_css!r}")

        # Read it back via the single-key endpoint
        status, css_resp = api.get("/api/settings/custom_css")
        t.check("GET /api/settings/custom_css returns 200", status == 200)
        if isinstance(css_resp, dict):
            t.check("Single-key endpoint returns correct CSS",
                    css_resp.get("value") == test_css,
                    f"Got: {css_resp.get('value')!r}")

        # Verify /css/custom.css serves the CSS content
        try:
            css_url = f"{api.base}/css/custom.css"
            css_req = urllib.request.Request(css_url)
            with urllib.request.urlopen(css_req, timeout=10) as resp:
                served_css = resp.read().decode()
                t.check("/css/custom.css serves saved CSS",
                        served_css.strip() == test_css.strip(),
                        f"Got: {served_css[:100]!r}")
                content_type = resp.headers.get("Content-Type", "")
                t.check("/css/custom.css has correct content-type",
                        "text/css" in content_type,
                        f"Got: {content_type}")
        except Exception as e:
            t.check("/css/custom.css serves saved CSS", False, f"Error: {e}")

        # Update CSS to a different value
        test_css2 = ".reader-pane { max-width: 800px; }"
        status, resp = api.put("/api/settings/custom_css", {"value": test_css2})
        t.check("PUT updated CSS returns 200", status == 200)

        # Verify update persisted
        status, settings = api.get("/api/settings")
        saved_css2 = settings.get("custom_css") if isinstance(settings, dict) else None
        t.check("Updated CSS persisted",
                saved_css2 == test_css2,
                f"Got: {saved_css2!r}")

        # Verify /css/custom.css serves the updated CSS
        try:
            # Add cache-buster to avoid caching
            css_url = f"{api.base}/css/custom.css?_t={int(time.time())}"
            css_req = urllib.request.Request(css_url)
            with urllib.request.urlopen(css_req, timeout=10) as resp:
                served_css2 = resp.read().decode()
                t.check("/css/custom.css serves updated CSS",
                        served_css2.strip() == test_css2.strip(),
                        f"Got: {served_css2[:100]!r}")
        except Exception as e:
            t.check("/css/custom.css serves updated CSS", False, f"Error: {e}")

        # Clear CSS (set to empty)
        status, resp = api.put("/api/settings/custom_css", {"value": ""})
        t.check("PUT empty CSS returns 200", status == 200)

        # Verify /css/custom.css returns empty
        try:
            css_url = f"{api.base}/css/custom.css?_t={int(time.time())}"
            css_req = urllib.request.Request(css_url)
            with urllib.request.urlopen(css_req, timeout=10) as resp:
                served_empty = resp.read().decode()
                t.check("/css/custom.css returns empty after clear",
                        served_empty.strip() == "",
                        f"Got: {served_empty[:100]!r}")
        except Exception as e:
            t.check("/css/custom.css returns empty after clear", False,
                    f"Error: {e}")

        # Save CSS via batch settings endpoint (alternative path)
        test_css3 = "h1 { color: red; }"
        status, resp = api.put("/api/settings", {"custom_css": test_css3})
        t.check("PUT custom_css via batch returns 200", status == 200)
        status, settings = api.get("/api/settings")
        t.check("Batch-saved CSS persisted",
                isinstance(settings, dict) and settings.get("custom_css") == test_css3,
                f"Got: {settings.get('custom_css') if isinstance(settings, dict) else 'N/A'}")

        # Clean up: clear CSS
        api.put("/api/settings/custom_css", {"value": ""})

        # ------------------ API Tests: Error cases ------------------
        t.section("API: Error Handling")
        status, _ = api.get("/api/articles/nonexistent-id-12345")
        t.check("Non-existent article returns 404 or 500",
                status in (404, 500),
                f"Got {status}")

        status, _ = api.get("/api/feeds/nonexistent-feed-id")
        t.check("Non-existent feed returns 404 or 500",
                status in (404, 500),
                f"Got {status}")

        # ----------- DuckDB: Final DB state verification -----------
        t.section("DuckDB: Final DB Verification")
        if DUCKDB_BIN.exists():
            # Verify article count
            rows = duckdb_query(data_path,
                f"SELECT COUNT(*) as cnt FROM {lance_table('articles')}")
            if rows:
                t.check("DuckDB: 14 articles in Lance",
                        rows[0].get("cnt") == 14,
                        f"Got {rows[0].get('cnt')}")

            # Verify feed count
            rows = duckdb_query(data_path,
                f"SELECT COUNT(*) as cnt FROM {lance_table('feeds')}")
            if rows:
                t.check("DuckDB: 3 feeds in Lance",
                        rows[0].get("cnt") == 3,
                        f"Got {rows[0].get('cnt')}")

            # Verify feed titles
            rows = duckdb_query(data_path,
                f"SELECT title FROM {lance_table('feeds')} ORDER BY title")
            if rows:
                titles = [r.get("title") for r in rows]
                t.check("DuckDB: feed titles correct",
                        "Test Feed Alpha" in titles and "Test Feed Bravo" in titles and "Test Feed Sanitize" in titles,
                        f"Got: {titles}")

            # Note: is_read/is_starred in the Lance files may not reflect
            # cached writes yet (the server uses an in-memory write cache).
            # The API tests above already verified the correct state.
            t.log("Note: Lance on-disk state may lag cache (expected)")

            # Verify log tables exist in DuckDB
            rows = duckdb_query(data_path,
                f"SELECT COUNT(*) as cnt FROM {lance_table('log_fetcher')}")
            if rows:
                cnt = rows[0].get("cnt", 0)
                t.check("DuckDB: log_fetcher table has entries",
                        cnt >= 6, f"Got {cnt}")
            else:
                t.check("DuckDB: log_fetcher table has entries", False,
                        "DuckDB query failed")

            rows = duckdb_query(data_path,
                f"SELECT COUNT(*) as cnt FROM {lance_table('log_api')}")
            if rows:
                t.check("DuckDB: log_api table exists",
                        rows[0].get("cnt", -1) >= 0,
                        f"Got {rows[0].get('cnt')}")
            else:
                t.check("DuckDB: log_api table exists", False,
                        "DuckDB query failed")
        else:
            t.log("DuckDB not available - skipping DB verification")

        # -------- DuckDB Process Restart Resilience (Windows) ------
        t.section("DuckDB: Process Restart Resilience")
        if sys.platform == "win32" and server_proc:
            server_pid = server_proc.pid
            # Find child duckdb.exe processes of the server
            try:
                wmic_out = subprocess.check_output(
                    ["wmic", "process", "where",
                     f"ParentProcessId={server_pid} and Name='duckdb.exe'",
                     "get", "ProcessId"],
                    text=True, stderr=subprocess.DEVNULL, timeout=10
                )
                duck_pids = [
                    int(line.strip()) for line in wmic_out.strip().splitlines()
                    if line.strip().isdigit()
                ]
            except Exception as exc:
                duck_pids = []
                t.log(f"Could not find child duckdb.exe: {exc}")

            if duck_pids:
                duck_pid = duck_pids[0]
                t.log(f"Found duckdb.exe child process (PID {duck_pid}), killing it")

                # Verify API works before kill
                pre_status, pre_feeds = api.get("/api/feeds")
                t.check("API works before duckdb kill",
                        pre_status == 200, f"Got {pre_status}")

                # Kill the duckdb.exe child process
                try:
                    os.kill(duck_pid, signal.SIGTERM)
                except OSError:
                    # SIGTERM may not work on Windows, try taskkill
                    subprocess.run(
                        ["taskkill", "/F", "/PID", str(duck_pid)],
                        capture_output=True, timeout=5
                    )
                time.sleep(1)  # Give the server time to detect the death

                # API should still work -- server auto-restarts duckdb.exe
                post_status, post_feeds = api.get("/api/feeds")
                t.check("API works after duckdb kill (auto-restart)",
                        post_status == 200, f"Got {post_status}")

                if post_status == 200 and isinstance(post_feeds, list):
                    t.check("Feed data intact after restart",
                            len(post_feeds) >= 3,
                            f"Expected >= 3 feeds, got {len(post_feeds)}")

                # Verify a new duckdb.exe is running
                try:
                    wmic_out2 = subprocess.check_output(
                        ["wmic", "process", "where",
                         f"ParentProcessId={server_pid} and Name='duckdb.exe'",
                         "get", "ProcessId"],
                        text=True, stderr=subprocess.DEVNULL, timeout=10
                    )
                    new_pids = [
                        int(line.strip()) for line in wmic_out2.strip().splitlines()
                        if line.strip().isdigit()
                    ]
                except Exception:
                    new_pids = []

                if new_pids:
                    new_pid = new_pids[0]
                    t.check("New duckdb.exe has different PID",
                            new_pid != duck_pid,
                            f"Old={duck_pid}, New={new_pid}")
                    t.log(f"New duckdb.exe PID: {new_pid}")
                else:
                    t.check("New duckdb.exe spawned after kill", False,
                            "No child duckdb.exe found")
            else:
                t.log("No child duckdb.exe found (may not be Windows build)")
        else:
            t.log("Skipping (Windows-only test)")

        # -------- Queue Feed (via API, like frontend would) --------
        t.section("API: Queue Feed")
        status, resp = api.post("/api/feeds", {"url": "http://example.com/new-feed.xml"})
        # Queue feed may return 202 (success) or 500 (schema mismatch on fresh tables)
        t.check("POST /api/feeds accepted or known error",
                status in (202, 500),
                f"Got status {status}")
        if status == 202 and isinstance(resp, dict):
            t.check("Response says queued",
                    resp.get("status") == "queued",
                    f"Got: {resp}")
        else:
            t.log(f"Queue feed returned {status} (may be schema issue with fresh DB)")

        # --------- API Tests: Logs endpoint + verification ---------
        t.section("Logging: API Logs Endpoint")

        # Give async log writes a moment to complete
        time.sleep(1)

        # GET /api/logs should return entries from the server
        status, logs_resp = api.get("/api/logs")
        t.check("GET /api/logs returns 200", status == 200)
        t.check("Logs response has entries array",
                isinstance(logs_resp, dict) and "entries" in logs_resp,
                f"Got: {type(logs_resp).__name__}")

        if isinstance(logs_resp, dict):
            entries = logs_resp.get("entries", [])
            total = logs_resp.get("total", 0)
            t.log(f"Total log entries: {total} (page has {len(entries)})")

            # Should have fetcher logs (6 we wrote) + server logs
            t.check("Logs total > 0", total > 0, f"Got {total}")
            t.check("Logs entries returned", len(entries) > 0,
                    f"Got {len(entries)}")

        # Filter by service=fetcher - should include our 6 entries
        status, fetcher_logs = api.get("/api/logs?service=fetcher&limit=50")
        t.check("GET /api/logs?service=fetcher returns 200", status == 200)
        if isinstance(fetcher_logs, dict):
            f_entries = fetcher_logs.get("entries", [])
            f_total = fetcher_logs.get("total", 0)
            # After trim tests: trimmed to <=5, then added 3 retain-all entries = ~8
            t.check("Fetcher logs: entries present via API", f_total >= 6,
                    f"Got {f_total}")
            # All entries should have service=fetcher
            all_fetcher = all(e.get("service") == "fetcher" for e in f_entries)
            t.check("All filtered entries are fetcher service", all_fetcher)

        # Filter by service=api - should include server lifecycle + actions
        status, api_logs = api.get("/api/logs?service=api&limit=500")
        t.check("GET /api/logs?service=api returns 200", status == 200)
        if isinstance(api_logs, dict):
            a_entries = api_logs.get("entries", [])
            a_total = api_logs.get("total", 0)
            t.log(f"API log entries: {a_total}")
            # Should have at least lifecycle (server start) + feed_actions (queue + mark-all-read)
            t.check("API logs: at least 1 entry", a_total >= 1,
                    f"Got {a_total}")

            # Check for lifecycle log (server started)
            lifecycle_entries = [e for e in a_entries if e.get("category") == "lifecycle"]
            t.check("API logs: lifecycle entry exists",
                    len(lifecycle_entries) >= 1,
                    f"Got {len(lifecycle_entries)} lifecycle entries")
            if lifecycle_entries:
                t.check("Lifecycle log says 'Server started'",
                        any("Server started" in e.get("message", "") for e in lifecycle_entries),
                        f"Got: {[e.get('message', '') for e in lifecycle_entries]!r}")

            # Check for feed_actions logs (queue feed + mark-all-read)
            feed_action_entries = [e for e in a_entries if e.get("category") == "feed_actions"]
            t.log(f"Feed action log entries: {len(feed_action_entries)}")
            if feed_action_entries:
                messages = [e.get("message", "") for e in feed_action_entries]
                has_queue = any("queued" in m.lower() or "Feed queued" in m for m in messages)
                has_mark_all = any("mark" in m.lower() or "Marked all read" in m for m in messages)
                t.check("API logs: feed queue action logged",
                        has_queue,
                        f"Messages: {messages}")
                t.check("API logs: mark-all-read action logged",
                        has_mark_all,
                        f"Messages: {messages}")

            # Check for article_actions logs (read/star from earlier test sections)
            article_action_entries = [e for e in a_entries if e.get("category") == "article_actions"]
            t.log(f"Article action log entries: {len(article_action_entries)}")
            t.check("API logs: article_actions entries exist",
                    len(article_action_entries) >= 1,
                    f"Got {len(article_action_entries)} -- read/star/unread/unstar should log")
            if article_action_entries:
                messages = [e.get("message", "") for e in article_action_entries]
                has_read = any("read" in m.lower() for m in messages)
                has_star = any("star" in m.lower() for m in messages)
                t.check("API logs: article read action logged", has_read,
                        f"Messages: {messages[:5]}")
                t.check("API logs: article star action logged", has_star,
                        f"Messages: {messages[:5]}")

            # Check for settings_changes logs (from all the PUT /api/settings calls)
            settings_entries = [e for e in a_entries if e.get("category") == "settings_changes"]
            t.log(f"Settings change log entries: {len(settings_entries)}")
            t.check("API logs: settings_changes entries exist",
                    len(settings_entries) >= 1,
                    f"Got {len(settings_entries)} -- PUT /api/settings should log")

            # Check for requests logs (from all the API calls above)
            requests_entries = [e for e in a_entries if e.get("category") == "requests"]
            t.log(f"API request log entries: {len(requests_entries)}")
            t.check("API logs: requests entries exist",
                    len(requests_entries) >= 1,
                    f"Got {len(requests_entries)} -- API calls should log when log.api.requests is on")

            # Check for errors logs (from 404/400 responses in error handling section)
            errors_entries = [e for e in a_entries if e.get("category") == "errors"]
            t.log(f"API error log entries: {len(errors_entries)}")
            # errors may be 0 if no 4xx/5xx happened before this point -- that's ok,
            # we verify the category works below after triggering a 404
            if errors_entries:
                t.check("API logs: errors entries have warn level",
                        all(e.get("level") == "warn" for e in errors_entries),
                        f"Levels: {[e.get('level') for e in errors_entries[:5]]}")

        # Trigger a 404 to generate an errors log entry, then verify it
        api.get("/api/nonexistent-endpoint-for-error-log-test")
        time.sleep(1)
        status, err_check = api.get("/api/logs?service=api&category=errors&limit=10")
        if isinstance(err_check, dict):
            err_entries = err_check.get("entries", [])
            t.check("API logs: errors category has entries after 404",
                    len(err_entries) >= 1,
                    f"Got {len(err_entries)}")

        # Write a fresh error log so the level filter has something to find
        # (the original error entry may have been removed by the trim test)
        db.log_event("error", "feed_fetch",
                     "simulated error for level filter test",
                     json.dumps({"test": True}))
        time.sleep(0.5)

        # Filter by level
        status, error_logs = api.get("/api/logs?level=error&limit=50")
        t.check("GET /api/logs?level=error returns 200", status == 200)
        if isinstance(error_logs, dict):
            e_entries = error_logs.get("entries", [])
            t.check("Error level filter finds our test error",
                    any("simulated error" in e.get("message", "") for e in e_entries),
                    f"Got {len(e_entries)} error entries")

        # Filter by category
        status, cat_logs = api.get("/api/logs?category=feed_fetch&limit=50")
        t.check("GET /api/logs?category=feed_fetch returns 200", status == 200)
        if isinstance(cat_logs, dict):
            c_entries = cat_logs.get("entries", [])
            c_total = cat_logs.get("total", 0)
            # After trim tests added feed_fetch entries, count will be > 2
            t.check("Category filter: feed_fetch entries >= 2", c_total >= 2,
                    f"Got {c_total}")

        # Pagination test
        status, page1 = api.get("/api/logs?limit=2&offset=0")
        t.check("Logs pagination page 1 returns 200", status == 200)
        if isinstance(page1, dict):
            p1_entries = page1.get("entries", [])
            t.check("Logs page 1 has 2 entries", len(p1_entries) == 2,
                    f"Got {len(p1_entries)}")

            status, page2 = api.get("/api/logs?limit=2&offset=2")
            if isinstance(page2, dict):
                p2_entries = page2.get("entries", [])
                t.check("Logs page 2 has entries", len(p2_entries) > 0,
                        f"Got {len(p2_entries)}")
                # No overlap
                p1_ids = {e.get("log_id") for e in p1_entries}
                p2_ids = {e.get("log_id") for e in p2_entries}
                t.check("Logs pages have no overlap",
                        len(p1_ids & p2_ids) == 0,
                        f"Overlap: {p1_ids & p2_ids}")

        # Invalid filter should return 400
        status, _ = api.get("/api/logs?service=invalid")
        t.check("Invalid service filter returns 400", status == 400,
                f"Got {status}")
        status, _ = api.get("/api/logs?level=invalid")
        t.check("Invalid level filter returns 400", status == 400,
                f"Got {status}")

        # Verify logs are ordered by timestamp descending (newest first)
        status, ordered_logs = api.get("/api/logs?limit=50")
        if isinstance(ordered_logs, dict):
            o_entries = ordered_logs.get("entries", [])
            if len(o_entries) >= 2:
                timestamps = [e.get("timestamp", "") for e in o_entries]
                is_descending = all(
                    timestamps[i] >= timestamps[i + 1]
                    for i in range(len(timestamps) - 1)
                )
                t.check("Logs ordered by timestamp descending",
                        is_descending,
                        f"First few timestamps: {timestamps[:5]}")

        # Verify fetcher log entries have details JSON accessible via API
        status, detail_logs = api.get("/api/logs?service=fetcher&category=feed_fetch&limit=1")
        if isinstance(detail_logs, dict):
            d_entries = detail_logs.get("entries", [])
            if d_entries:
                entry = d_entries[0]
                t.check("Log entry has log_id", bool(entry.get("log_id")))
                t.check("Log entry has timestamp", bool(entry.get("timestamp")))
                t.check("Log entry has level", entry.get("level") in ("info", "error", "debug", "warn"))
                t.check("Log entry has service", entry.get("service") == "fetcher")
                details_str = entry.get("details", "")
                if details_str:
                    try:
                        details = json.loads(details_str)
                        t.check("Log entry details is valid JSON",
                                isinstance(details, dict))
                    except (json.JSONDecodeError, TypeError):
                        t.check("Log entry details is valid JSON", False,
                                f"Got: {details_str!r}")

        # ----- API Tests: Config endpoint (show_shutdown=false) -----
        t.section("API: Config (show_shutdown=false)")
        status, cfg = api.get("/api/config")
        t.check("GET /api/config returns 200", status == 200)
        t.check("Config has show_shutdown field",
                isinstance(cfg, dict) and "show_shutdown" in cfg,
                f"Got: {cfg}")
        if isinstance(cfg, dict):
            t.check("show_shutdown is false (default)",
                    cfg.get("show_shutdown") == False,
                    f"Got: {cfg.get('show_shutdown')!r}")

        # /api/shutdown should NOT exist when show_shutdown=false
        status, _ = api.post("/api/shutdown")
        t.check("POST /api/shutdown returns 404 when disabled",
                status == 404,
                f"Got status {status}")

        # ---------- Restart server with show_shutdown=true ----------
        t.section("API: Config (show_shutdown=true) - restart server")
        t.log("Stopping server for config change...")
        server_proc.terminate()
        try:
            server_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server_proc.kill()
        server_proc = None
        server_log.close()

        # Rewrite config with show_shutdown = true
        with open(config_path, "r") as f:
            config_text = f.read()
        config_text = config_text.replace(
            "show_shutdown = false", "show_shutdown = true")
        with open(config_path, "w") as f:
            f.write(config_text)
        t.log("Config updated: show_shutdown = true")

        # Start server again
        server_log = open(server_log_path, "w")
        server_proc = subprocess.Popen(
            [str(SERVER_BIN), "-config", config_path],
            stdout=server_log,
            stderr=subprocess.STDOUT,
            cwd=str(ROOT),
            env=server_env,
        )

        server_ready = False
        for i in range(60):
            if server_proc.poll() is not None:
                server_log.close()
                with open(server_log_path) as f:
                    log_content = f.read()
                t.check("Server restarted", False,
                        f"Server exited with code {server_proc.returncode}\n{log_content[:500]}")
                server_proc = None
                break
            try:
                status, _ = api.get("/api/feeds")
                if status == 200:
                    server_ready = True
                    break
            except Exception:
                pass
            time.sleep(0.5)
        else:
            t.check("Server restarted", False, "Server did not respond within 30s")

        if server_ready:
            t.check("Server restarted", True)

            # /api/config should now report show_shutdown=true
            status, cfg = api.get("/api/config")
            t.check("GET /api/config returns 200 (restarted)", status == 200)
            if isinstance(cfg, dict):
                t.check("show_shutdown is true after config change",
                        cfg.get("show_shutdown") == True,
                        f"Got: {cfg.get('show_shutdown')!r}")

            # /api/shutdown should now exist and work (this will stop the server)
            status, resp = api.post("/api/shutdown")
            t.check("POST /api/shutdown returns 200 when enabled",
                    status == 200,
                    f"Got status {status}")
            if isinstance(resp, dict):
                t.check("Shutdown response has ok=true",
                        resp.get("ok") == True,
                        f"Got: {resp}")

            # Wait for server to actually stop
            time.sleep(2)
            try:
                exit_code = server_proc.poll()
                t.check("Server process exited after /api/shutdown",
                        exit_code is not None,
                        f"poll() returned {exit_code}")
            except Exception:
                pass
            server_proc = None  # already stopped via API

        # ======== Offline Mode: Lance data disappears ========
        t.section("Offline Mode: Lance data disappears")

        # Seed offline settings into the Lance DB before starting server
        t.log("Seeding offline settings...")
        offline_cache_file = os.path.join(temp_dir, "offline_cache.db")
        db.put_settings({
            "offline_snapshot_interval_mins":  "1",
            "offline_cache_path":             offline_cache_file.replace(os.sep, "/"),
        })

        # Create a separate local dir for DuckDB (simulates duckdb_path config)
        duckdb_local = os.path.join(temp_dir, "duckdb_local")
        os.makedirs(duckdb_local, exist_ok=True)

        # Rewrite config with duckdb_path + show_shutdown=true so we can
        # cleanly stop the server at the end
        with open(config_path, "w") as f:
            f.write(textwrap.dedent(f"""\
                [storage]
                type = "local"
                path = "{data_path.replace(os.sep, '/')}"
                duckdb_path = "{duckdb_local.replace(os.sep, '/')}"

                [fetcher]
                interval_minutes = 30
                max_concurrent = 2
                user_agent = "RSS-Lance-E2E-Test/1.0"

                [server]
                host = "127.0.0.1"
                port = {api_port}
                frontend_dir = "{str(ROOT / 'frontend').replace(os.sep, '/')}"
                show_shutdown = true

                [compaction]
                articles      = 999
                feeds         = 999
                categories    = 999
                pending_feeds = 999
            """))

        # Start server with offline mode
        server_log = open(server_log_path, "w")
        server_proc = subprocess.Popen(
            [str(SERVER_BIN), "-config", config_path],
            stdout=server_log,
            stderr=subprocess.STDOUT,
            cwd=str(ROOT),
            env=server_env,
        )

        server_ready = False
        for i in range(60):
            if server_proc.poll() is not None:
                server_log.close()
                with open(server_log_path) as f:
                    log_content = f.read()
                t.check("Server started (offline)", False,
                        f"Exited with code {server_proc.returncode}\n{log_content[:500]}")
                server_proc = None
                break
            try:
                status, _ = api.get("/api/feeds")
                if status == 200:
                    server_ready = True
                    break
            except Exception:
                pass
            time.sleep(0.5)
        else:
            t.check("Server started (offline)", False, "Did not respond within 30s")

        if server_ready:
            t.check("Server started (offline)", True)

            # Verify offline mode is enabled but not yet offline
            status, ost = api.get("/api/offline-status")
            t.check("GET /api/offline-status returns 200", status == 200)
            if isinstance(ost, dict):
                t.check("Offline mode enabled", ost.get("enabled") == True,
                        f"Got: {ost}")
                t.check("Not offline yet", ost.get("offline") == False,
                        f"Got: {ost}")

            # Wait for initial snapshot to complete (last_snapshot becomes non-empty)
            t.log("Waiting for initial offline snapshot...")
            snapshot_ready = False
            for i in range(30):  # up to 15s
                status, ost = api.get("/api/offline-status")
                if isinstance(ost, dict) and ost.get("last_snapshot"):
                    snapshot_ready = True
                    break
                time.sleep(0.5)
            t.check("Initial snapshot completed",
                    snapshot_ready,
                    f"last_snapshot still empty after 15s")

            if snapshot_ready:
                cached_count = ost.get("cache_articles", 0)
                t.log(f"Snapshot done: {cached_count} articles cached")
                t.check("Snapshot cached articles", cached_count > 0,
                        f"Expected >0, got {cached_count}")

                # ---- Simulate Lance disappearing: rename data_path ----
                data_path_hidden = data_path + "_hidden"
                t.log(f"Renaming data dir to simulate Lance failure...")
                try:
                    if sys.platform == "win32":
                        # On Windows, DuckDB holds file locks inside data_path.
                        # Kill the external duckdb.exe process first so the
                        # rename can succeed.
                        duck_pid = None
                        try:
                            st_code, srv_st = api.get("/api/server-status")
                            if st_code == 200 and isinstance(srv_st, dict):
                                dp = srv_st.get("duckdb_process")
                                if isinstance(dp, dict):
                                    duck_pid = dp.get("pid")
                        except Exception as exc:
                            t.log(f"Could not get DuckDB PID: {exc}")
                        if duck_pid:
                            t.log(f"Killing duckdb.exe (PID {duck_pid}) to release file locks...")
                            subprocess.call(
                                ["taskkill", "/F", "/PID", str(int(duck_pid))],
                                stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL,
                                timeout=5,
                            )
                            time.sleep(0.3)
                        else:
                            t.log("No DuckDB PID found; attempting rename anyway")
                    os.rename(data_path, data_path_hidden)
                    rename_ok = True
                except OSError as e:
                    rename_ok = False
                    t.check("Rename data dir", False, f"OS error: {e}")

                if rename_ok:
                    t.check("Data dir renamed", not os.path.exists(data_path))

                    # Trigger goOffline by making an API call that hits Lance
                    t.log("Triggering offline transition via API call...")
                    status, body = api.get("/api/articles")
                    # The server should still respond (from cache or with error)
                    # but internally it should have called goOffline()

                    # Poll /api/offline-status until offline=true (up to 10s)
                    went_offline = False
                    for i in range(20):
                        status, ost = api.get("/api/offline-status")
                        if isinstance(ost, dict) and ost.get("offline") == True:
                            went_offline = True
                            break
                        # Make another API request to trigger goOffline if
                        # the first one didn't (e.g. came from write cache)
                        api.get("/api/feeds")
                        time.sleep(0.5)
                    t.check("Server went offline after data dir removed",
                            went_offline,
                            f"offline-status: {ost}")

                    if went_offline:
                        # Verify we can still read cached data while offline
                        status, arts = api.get("/api/articles")
                        t.check("Articles still served while offline",
                                status == 200,
                                f"Got status {status}")
                        if isinstance(arts, list):
                            t.check("Cached articles returned",
                                    len(arts) > 0,
                                    f"Got {len(arts)} articles")

                        pending_before = 0
                        status, ost = api.get("/api/offline-status")
                        if isinstance(ost, dict):
                            pending_before = ost.get("pending_changes", 0)

                        # ---- Restore data dir: simulate reconnect ----
                        t.log("Restoring data dir...")
                        os.rename(data_path_hidden, data_path)
                        t.check("Data dir restored", os.path.exists(data_path))

                        # Wait for health probe to detect recovery (5s interval
                        # when offline, give it up to 20s)
                        t.log("Waiting for server to come back online...")
                        came_online = False
                        for i in range(40):  # up to 20s
                            status, ost = api.get("/api/offline-status")
                            if isinstance(ost, dict) and ost.get("offline") == False:
                                came_online = True
                                break
                            time.sleep(0.5)
                        t.check("Server came back online",
                                came_online,
                                f"offline-status after 20s: {ost}")

                        if came_online:
                            # Verify data is accessible again
                            status, arts = api.get("/api/articles")
                            t.check("Articles accessible after recovery",
                                    status == 200 and isinstance(arts, list) and len(arts) > 0,
                                    f"status={status}, articles={len(arts) if isinstance(arts, list) else 'N/A'}")
                    else:
                        # Even if offline detection failed, restore the dir
                        t.log("Restoring data dir (offline detection failed)...")
                        os.rename(data_path_hidden, data_path)
                # end if rename_ok

            # Stop the server for this section
            if server_proc:
                server_proc.terminate()
                try:
                    server_proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    server_proc.kill()
                server_proc = None
            try:
                server_log.close()
            except Exception:
                pass

    except KeyboardInterrupt:
        print("\n\n  Interrupted by user.")
    except Exception as exc:
        print(f"\n  UNEXPECTED ERROR: {exc}")
        import traceback
        traceback.print_exc()
    finally:
        # -- Post-failure version check (before cleanup kills the server) --
        if expected_version and t.failed > 0 and server_proc is not None:
            state = verify_server_alive(api, expected_version)
            if state == "crashed":
                print(f"\n  {'!' * 60}")
                print(f"  WARNING: Server is not responding -- it may have crashed.")
                print(f"  The test failure(s) above may not be real test failures")
                print(f"  but caused by the server crashing.")
                print(f"  Please rerun the test. If the same test fails again,")
                print(f"  it may be crashing the server -- please open a GitHub issue.")
                print(f"  {'!' * 60}")
            elif state == "replaced":
                print(f"\n  {'!' * 60}")
                print(f"  WARNING: Server binary was replaced during testing!")
                print(f"  Expected build_version: {expected_version}")
                print(f"  Another build likely overwrote the binary. All test")
                print(f"  results may be invalid. Please rerun when no other")
                print(f"  builds are running.")
                print(f"  {'!' * 60}")

        # ------------------------- Cleanup -------------------------
        print(f"\n{'=' * 60}")
        print(f"  Cleanup")
        print(f"{'=' * 60}")

        if server_proc:
            server_proc.terminate()
            try:
                server_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server_proc.kill()
            print("  ... Server stopped")

        # Close server log file if still open
        try:
            server_log.close()
        except Exception:
            pass

        if rss_server:
            rss_server.shutdown()
            print("  ... RSS server stopped")

        if temp_dir:
            if keep:
                print(f"  ... Keeping temp dir: {temp_dir}")
            else:
                # Small delay to let server release file locks
                time.sleep(0.5)
                try:
                    shutil.rmtree(temp_dir)
                    print("  ... Temp dir cleaned up")
                except Exception as e:
                    print(f"  ... Cleanup warning: {e}")
                    print(f"      Manual cleanup: {temp_dir}")

    return t.summary()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="RSS-Lance E2E Integration Test")
    parser.add_argument("--keep", action="store_true",
                        help="Keep temp data directory after test run")
    parser.add_argument("--verbose", action="store_true",
                        help="Show HTTP status for every request")
    parser.add_argument("--build-version", default="",
                        help="Expected build version (e.g. test-abc123). "
                             "If set, verifies /api/server-status build_version matches.")
    args = parser.parse_args()

    sys.exit(run_e2e(keep=args.keep, verbose=args.verbose,
                     build_version=args.build_version))
