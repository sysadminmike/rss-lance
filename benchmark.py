#!/usr/bin/env python3
"""
RSS-Lance Benchmark / Stress Test

Tests feeder insertion performance and API reader (scroll) performance.
Creates an isolated temporary database with a random port so it doesn't
interfere with your real data.

Usage:
    python benchmark.py                 # run all benchmarks
    python benchmark.py insert          # insertion benchmarks only
    python benchmark.py sanitize        # sanitization pipeline only
    python benchmark.py pipeline        # sanitize + insert (full pipeline)
    python benchmark.py read            # API read/scroll benchmarks only
    python benchmark.py --log info      # with logging (default: none)
"""

from __future__ import annotations

import argparse
import json
import os
import random
import shutil
import socket
import string
import subprocess
import sys
import tempfile
import time
import uuid
from contextlib import closing
from datetime import datetime, timezone, timedelta
from pathlib import Path
from typing import Any

# ---------------------------------------------------------------------------
# Ensure fetcher/ is importable
# ---------------------------------------------------------------------------
PROJECT_ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(PROJECT_ROOT / "fetcher"))

import lancedb
import pyarrow as pa

from db import (
    DB,
    FEEDS_SCHEMA,
    ARTICLES_SCHEMA,
    CATEGORIES_SCHEMA,
    SCHEMA_VERSION,
)
from config import Config
from content_cleaner import (
    strip_site_chrome,
    strip_tracking_pixels,
    strip_dangerous_html,
    strip_tracking_params,
)
from feed_parser import strip_social_links

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

_TS = pa.timestamp("us")

WORD_POOL = (
    "the quick brown fox jumps over lazy dog alpha bravo charlie delta echo "
    "foxtrot golf hotel india juliet kilo lima mike november oscar papa "
    "quantum rocket satellite typescript ubuntu vector webpack xray yaml zulu "
    "python golang javascript react angular vue database query index table "
    "benchmark performance latency throughput concurrent parallel async await "
    "network protocol buffer stream pipeline filter transform aggregate ".split()
)


def _rand_text(min_words: int = 5, max_words: int = 30) -> str:
    n = random.randint(min_words, max_words)
    return " ".join(random.choices(WORD_POOL, k=n))


def _rand_html(min_words: int = 50, max_words: int = 500) -> str:
    n = random.randint(min_words, max_words)
    words = random.choices(WORD_POOL, k=n)
    paragraphs = []
    i = 0
    while i < len(words):
        chunk = random.randint(10, 40)
        paragraphs.append("<p>" + " ".join(words[i : i + chunk]) + "</p>")
        i += chunk
    return "\n".join(paragraphs)


# Fragments injected into dirty HTML to exercise every sanitization step
_TRACKING_PIXELS = [
    '<img src="https://pixel.wp.com/g.gif?v=1&blog=123" width="1" height="1" />',
    '<img src="https://stats.wordpress.com/b.gif?rnd=12345" width="1" height="1" style="display:none" />',
    '<img src="https://example.com/track?uid=abc&e=open" width="1" height="1" />',
    '<img src="https://example.com/pixel.gif" width="0" height="0" />',
    '<img src="https://example.com/1x1.gif" />',
]

_DANGEROUS_HTML = [
    '<script>alert("xss")</script>',
    '<iframe src="https://evil.example.com"></iframe>',
    '<object data="https://evil.example.com/flash.swf"></object>',
    '<form action="https://phishing.example.com"><input type="submit" /></form>',
    '<style>body { display: none }</style>',
    '<a href="javascript:alert(1)">click me</a>',
]

_SOCIAL_LINKS = [
    '<a href="https://www.facebook.com/sharer/sharer.php?u=example">Share on Facebook</a>',
    '<a href="https://twitter.com/intent/tweet?text=hello">Tweet</a>',
    '<a href="https://www.linkedin.com/shareArticle?mini=true&url=example">LinkedIn</a>',
    '<a href="https://www.reddit.com/submit?url=example">Reddit</a>',
    '<a href="https://pinterest.com/pin/create?url=example">Pin it</a>',
]

_TRACKING_PARAM_LINKS = [
    '<a href="https://example.com/article?utm_source=rss&utm_medium=feed&utm_campaign=test">Read more</a>',
    '<a href="https://example.com/page?fbclid=abc123&gclid=xyz456">Link</a>',
    '<a href="https://example.com/?mc_eid=abc&mc_cid=def">Newsletter</a>',
]

# A shared "chrome" block that repeats across articles in a feed
_CHROME_BLOCK = (
    '<div class="site-footer"><nav><a href="/">Home</a> | '
    '<a href="/about">About</a> | <a href="/contact">Contact</a> | '
    '<a href="/privacy">Privacy Policy</a> | '
    '<a href="/terms">Terms of Service</a></nav>'
    '<p>Copyright 2025 Example Inc. All rights reserved. '
    'Use of this site constitutes acceptance of our terms.</p></div>'
)


def _rand_dirty_html(min_words: int = 50, max_words: int = 500,
                     inject_chrome: bool = True) -> str:
    """Generate realistic HTML with tracking pixels, dangerous tags,
    social links, and tracking params mixed in."""
    n = random.randint(min_words, max_words)
    words = random.choices(WORD_POOL, k=n)
    parts: list[str] = []
    i = 0
    while i < len(words):
        chunk = random.randint(10, 40)
        parts.append("<p>" + " ".join(words[i : i + chunk]) + "</p>")
        i += chunk

    # Sprinkle in dirty elements
    if random.random() < 0.7:
        parts.insert(random.randint(0, len(parts)),
                     random.choice(_TRACKING_PIXELS))
    if random.random() < 0.5:
        parts.insert(random.randint(0, len(parts)),
                     random.choice(_DANGEROUS_HTML))
    if random.random() < 0.6:
        parts.insert(random.randint(0, len(parts)),
                     random.choice(_SOCIAL_LINKS))
    if random.random() < 0.6:
        parts.insert(random.randint(0, len(parts)),
                     random.choice(_TRACKING_PARAM_LINKS))
    if inject_chrome:
        parts.append(_CHROME_BLOCK)

    return "\n".join(parts)


def _utcnow() -> datetime:
    return datetime.now(timezone.utc).replace(tzinfo=None)


def _rand_past(max_days: int = 365) -> datetime:
    delta = timedelta(
        days=random.randint(0, max_days),
        hours=random.randint(0, 23),
        minutes=random.randint(0, 59),
    )
    return _utcnow() - delta


def _free_port() -> int:
    with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _fmt_duration(seconds: float) -> str:
    if seconds < 1:
        return f"{seconds * 1000:.1f}ms"
    return f"{seconds:.2f}s"


def _fmt_rate(count: int, seconds: float) -> str:
    if seconds == 0:
        return "∞"
    return f"{count / seconds:,.0f}/s"


# ---------------------------------------------------------------------------
# Data generation
# ---------------------------------------------------------------------------

def generate_feeds(n: int) -> list[dict]:
    now = _utcnow()
    feeds = []
    for i in range(n):
        feeds.append({
            "feed_id": str(uuid.uuid4()),
            "title": f"Bench Feed {i:05d} - {_rand_text(2, 5)}",
            "url": f"https://bench-{i:05d}.example.com/feed.xml",
            "site_url": f"https://bench-{i:05d}.example.com",
            "icon_url": "",
            "category_id": "",
            "subcategory_id": "",
            "last_fetched": now,
            "last_article_date": now,
            "fetch_interval_mins": 30,
            "fetch_tier": "active",
            "tier_changed_at": now,
            "last_successful_fetch": now,
            "error_count": 0,
            "last_error": "",
            "created_at": now,
            "updated_at": now,
        })
    return feeds


def generate_articles(feeds: list[dict], articles_per_feed: int | None = None,
                      min_articles: int = 10, max_articles: int = 250) -> list[dict]:
    """Generate random articles for each feed.

    If articles_per_feed is set, each feed gets exactly that many.
    Otherwise each feed gets a random count in [min_articles, max_articles].
    """
    now = _utcnow()
    articles = []
    for feed in feeds:
        count = articles_per_feed if articles_per_feed is not None else random.randint(min_articles, max_articles)
        for j in range(count):
            pub = _rand_past(180)
            articles.append({
                "article_id": str(uuid.uuid4()),
                "feed_id": feed["feed_id"],
                "title": _rand_text(4, 12),
                "url": f"https://{feed['url'].split('//')[1].split('/')[0]}/article-{j}",
                "author": _rand_text(1, 3),
                "content": _rand_html(50, 500),
                "summary": _rand_text(10, 40),
                "published_at": pub,
                "fetched_at": now,
                "is_read": False,
                "is_starred": random.random() < 0.02,
                "guid": f"guid-{feed['feed_id'][:8]}-{j}",
                "schema_version": SCHEMA_VERSION,
                "created_at": now,
                "updated_at": now,
            })
    return articles


def generate_dirty_articles(feeds: list[dict], articles_per_feed: int | None = None,
                            min_articles: int = 10, max_articles: int = 250) -> list[dict]:
    """Like generate_articles but with realistic dirty HTML content."""
    now = _utcnow()
    articles = []
    for feed in feeds:
        count = articles_per_feed if articles_per_feed is not None else random.randint(min_articles, max_articles)
        for j in range(count):
            pub = _rand_past(180)
            articles.append({
                "article_id": str(uuid.uuid4()),
                "feed_id": feed["feed_id"],
                "title": _rand_text(4, 12),
                "url": f"https://{feed['url'].split('//')[1].split('/')[0]}/article-{j}",
                "author": _rand_text(1, 3),
                "content": _rand_dirty_html(50, 500, inject_chrome=True),
                "summary": _rand_text(10, 40),
                "published_at": pub,
                "fetched_at": now,
                "is_read": False,
                "is_starred": random.random() < 0.02,
                "guid": f"guid-{feed['feed_id'][:8]}-{j}",
                "schema_version": SCHEMA_VERSION,
                "created_at": now,
                "updated_at": now,
            })
    return articles


# ---------------------------------------------------------------------------
# Sanitization benchmark (pure Python, no DB)
# ---------------------------------------------------------------------------

def _bench_one_step(name: str, fn, articles: list[dict],
                    field: str = "content") -> dict:
    """Time a single sanitization function across all articles on a field."""
    t0 = time.perf_counter()
    for art in articles:
        if art.get(field):
            art[field] = fn(art[field])
    elapsed = time.perf_counter() - t0
    return {"step": name, "time": elapsed, "count": len(articles)}


def run_sanitize_benchmarks() -> list[dict]:
    """Benchmark each sanitization step independently + full pipeline."""
    import copy

    num_feeds = 50
    apf = 20
    total = num_feeds * apf

    print(f"\n{'=' * 70}")
    print(f"  SANITIZE: {total:,} articles ({num_feeds} feeds × {apf} articles)")
    print(f"{'=' * 70}")

    # Generate dirty data once
    t0 = time.perf_counter()
    feeds = generate_feeds(num_feeds)
    articles_orig = generate_dirty_articles(feeds, articles_per_feed=apf)
    gen_time = time.perf_counter() - t0
    print(f"  Data generation: {_fmt_duration(gen_time)}")

    results = []

    # --- Individual steps (each gets a fresh copy) ---
    steps = [
        ("strip_dangerous_html",  strip_dangerous_html),
        ("strip_social_links",    strip_social_links),
        ("strip_tracking_pixels", strip_tracking_pixels),
        ("strip_tracking_params", strip_tracking_params),
    ]

    for name, fn in steps:
        arts = copy.deepcopy(articles_orig)
        r = _bench_one_step(name, fn, arts)
        results.append(r)
        print(f"  {name:<30} {_fmt_duration(r['time']):>10} "
              f"({_fmt_rate(r['count'], r['time'])} articles/s)")

    # --- strip_site_chrome (operates on per-feed batches) ---
    arts = copy.deepcopy(articles_orig)
    by_feed: dict[str, list[dict]] = {}
    for a in arts:
        by_feed.setdefault(a["feed_id"], []).append(a)

    t0 = time.perf_counter()
    for feed_arts in by_feed.values():
        if len(feed_arts) >= 2:
            strip_site_chrome(feed_arts)
    chrome_time = time.perf_counter() - t0
    results.append({"step": "strip_site_chrome", "time": chrome_time, "count": len(arts)})
    print(f"  {'strip_site_chrome':<30} {_fmt_duration(chrome_time):>10} "
          f"({_fmt_rate(len(arts), chrome_time)} articles/s)")

    # --- Full pipeline (all steps in sequence) ---
    arts = copy.deepcopy(articles_orig)
    t0 = time.perf_counter()
    for art in arts:
        html = art.get("content", "")
        if html:
            html = strip_dangerous_html(html)
            html = strip_social_links(html)
            html = strip_tracking_pixels(html)
            html = strip_tracking_params(html)
            art["content"] = html
    # Then chrome
    by_feed2: dict[str, list[dict]] = {}
    for a in arts:
        by_feed2.setdefault(a["feed_id"], []).append(a)
    for feed_arts in by_feed2.values():
        if len(feed_arts) >= 2:
            strip_site_chrome(feed_arts)
    full_time = time.perf_counter() - t0
    results.append({"step": "FULL PIPELINE", "time": full_time, "count": len(arts)})
    print(f"  {'FULL PIPELINE':<30} {_fmt_duration(full_time):>10} "
          f"({_fmt_rate(len(arts), full_time)} articles/s)")

    return results


# ---------------------------------------------------------------------------
# Pipeline benchmark (sanitize + insert, like the real fetcher)
# ---------------------------------------------------------------------------

def run_pipeline_benchmarks(data_dir: str) -> list[dict]:
    """Benchmark the full pipeline: generate dirty data, sanitize, insert."""
    results = []
    scenarios = [
        # Keep pipeline scenarios smaller since sanitize is ~70 articles/s
        (100,  10,   "100 feeds × 10 articles"),
        (100,  100,  "100 feeds × 100 articles"),
    ]

    for num_feeds, apf, label in scenarios:
        # Clean slate
        for name in ("feeds.lance", "articles.lance", "categories.lance",
                     "pending_feeds.lance", "settings.lance",
                     "log_fetcher.lance", "log_api.lance", "server.duckdb"):
            p = os.path.join(data_dir, name)
            if os.path.isdir(p):
                shutil.rmtree(p)
            elif os.path.isfile(p):
                os.remove(p)

        cfg = Config()
        cfg.storage_path = data_dir
        db = DB(cfg)

        print(f"\n{'=' * 70}")
        print(f"  PIPELINE: {label}")
        print(f"{'=' * 70}")

        # Generate dirty data
        t0 = time.perf_counter()
        feeds = generate_feeds(num_feeds)
        articles = generate_dirty_articles(feeds, articles_per_feed=apf)
        gen_time = time.perf_counter() - t0
        total = len(articles)
        print(f"  Data generation ({total:,} articles): {_fmt_duration(gen_time)}")

        # Sanitize
        t1 = time.perf_counter()
        for art in articles:
            html = art.get("content", "")
            if html:
                html = strip_dangerous_html(html)
                html = strip_social_links(html)
                html = strip_tracking_pixels(html)
                html = strip_tracking_params(html)
                art["content"] = html

        by_feed: dict[str, list[dict]] = {}
        for a in articles:
            by_feed.setdefault(a["feed_id"], []).append(a)
        for feed_arts in by_feed.values():
            if len(feed_arts) >= 2:
                strip_site_chrome(feed_arts)
        sanitize_time = time.perf_counter() - t1
        print(f"  Sanitize:         {_fmt_duration(sanitize_time)} "
              f"({_fmt_rate(total, sanitize_time)} articles/s)")

        # Insert
        t2 = time.perf_counter()
        db.feeds.add(feeds)
        db.begin_batch()
        db.add_articles(articles)
        db.flush_batch()
        insert_time = time.perf_counter() - t2
        print(f"  Insert:           {_fmt_duration(insert_time)} "
              f"({_fmt_rate(total, insert_time)} articles/s)")

        pipeline_time = sanitize_time + insert_time
        print(f"  Total pipeline:   {_fmt_duration(pipeline_time)} "
              f"({_fmt_rate(total, pipeline_time)} articles/s)")

        results.append({
            "label": label,
            "num_articles": total,
            "sanitize_time": sanitize_time,
            "insert_time": insert_time,
            "pipeline_time": pipeline_time,
        })

    return results


# ---------------------------------------------------------------------------
# Insertion benchmark (pure Python / LanceDB)
# ---------------------------------------------------------------------------

def bench_insert(data_dir: str, num_feeds: int, articles_per_feed: int | None,
                 min_articles: int = 10, max_articles: int = 250,
                 label: str = "") -> dict:
    """Insert feeds + articles into a fresh LanceDB and measure timing."""

    # Clean slate for this run
    for name in ("feeds.lance", "articles.lance", "categories.lance",
                 "pending_feeds.lance", "settings.lance",
                 "log_fetcher.lance", "log_api.lance", "server.duckdb"):
        p = os.path.join(data_dir, name)
        if os.path.isdir(p):
            shutil.rmtree(p)
        elif os.path.isfile(p):
            os.remove(p)

    cfg = Config()
    cfg.storage_path = data_dir

    db = DB(cfg)

    # Generate data
    t0 = time.perf_counter()
    feeds = generate_feeds(num_feeds)
    articles = generate_articles(feeds, articles_per_feed, min_articles, max_articles)
    gen_time = time.perf_counter() - t0

    total_articles = len(articles)
    apf_label = str(articles_per_feed) if articles_per_feed else f"{min_articles}-{max_articles}"
    if not label:
        label = f"{num_feeds} feeds × {apf_label} articles/feed = {total_articles:,} articles"

    print(f"\n{'=' * 70}")
    print(f"  INSERT: {label}")
    print(f"{'=' * 70}")
    print(f"  Data generation: {_fmt_duration(gen_time)}")

    # Insert feeds - direct LanceDB add (bulk)
    t1 = time.perf_counter()
    db.feeds.add(feeds)
    feed_time = time.perf_counter() - t1
    print(f"  Feed insert ({num_feeds:,} feeds): {_fmt_duration(feed_time)} "
          f"({_fmt_rate(num_feeds, feed_time)})")

    # Insert articles using batch mode (as the real fetcher does)
    t2 = time.perf_counter()
    db.begin_batch()
    db.add_articles(articles)
    db.flush_batch()
    article_time = time.perf_counter() - t2
    print(f"  Article insert ({total_articles:,} articles): {_fmt_duration(article_time)} "
          f"({_fmt_rate(total_articles, article_time)})")

    total_time = feed_time + article_time
    print(f"  Total insert time: {_fmt_duration(total_time)} "
          f"({_fmt_rate(num_feeds + total_articles, total_time)} rows/s)")

    return {
        "label": label,
        "num_feeds": num_feeds,
        "num_articles": total_articles,
        "gen_time": gen_time,
        "feed_insert_time": feed_time,
        "article_insert_time": article_time,
        "total_time": total_time,
    }


def run_insert_benchmarks(data_dir: str) -> list[dict]:
    """Run the full set of insertion benchmarks."""
    results = []
    scenarios = [
        # (num_feeds, articles_per_feed, min, max, label)
        (100,  10,   None, None, "100 feeds × 10 articles"),
        (1000, 10,   None, None, "1000 feeds × 10 articles"),
        (100,  100,  None, None, "100 feeds × 100 articles"),
        (1000, 100,  None, None, "1000 feeds × 100 articles"),
        (1000, None, 10,   250,  "1000 feeds × 10-250 random articles"),
    ]

    for num_feeds, apf, min_a, max_a, label in scenarios:
        r = bench_insert(
            data_dir, num_feeds,
            articles_per_feed=apf,
            min_articles=min_a or 10,
            max_articles=max_a or 250,
            label=label,
        )
        results.append(r)

    return results


# ---------------------------------------------------------------------------
# API reader / scroll benchmark (Go server)
# ---------------------------------------------------------------------------

def _wait_for_server(port: int, timeout: float = 60.0) -> bool:
    """Poll until the server responds on /api/config (no DB hit)."""
    import urllib.request
    import urllib.error
    deadline = time.monotonic() + timeout
    url = f"http://127.0.0.1:{port}/api/config"

    # Phase 1: wait for TCP port to open
    while time.monotonic() < deadline:
        try:
            with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
                s.settimeout(2)
                s.connect(("127.0.0.1", port))
            break  # port is open
        except (ConnectionRefusedError, OSError, TimeoutError):
            time.sleep(0.5)
    else:
        return False

    # Phase 2: wait for HTTP to respond
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=5) as resp:
                if resp.status == 200:
                    return True
        except (urllib.error.URLError, urllib.error.HTTPError,
                ConnectionRefusedError, ConnectionResetError,
                OSError, TimeoutError):
            pass
        time.sleep(1)
    return False


def _api_get(port: int, path: str) -> tuple[int, Any]:
    """GET an API endpoint, return (status, parsed_json)."""
    import urllib.request
    import urllib.error
    url = f"http://127.0.0.1:{port}{path}"
    try:
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req, timeout=30) as resp:
            body = resp.read()
            return resp.status, json.loads(body) if body else None
    except urllib.error.HTTPError as e:
        body = e.read()
        return e.code, json.loads(body) if body else None


def bench_read_scroll(port: int, total_articles: int) -> dict:
    """Simulate scrolling through articles via the API, sampling pages at
    various offsets to show latency across the dataset without hammering
    the server with thousands of subprocess-backed queries.

    Samples: first 10 pages sequentially, then pages at exponentially
    growing offsets up to the end of the dataset.
    """
    page_size = 50
    total_pages = (total_articles + page_size - 1) // page_size

    # Build a list of page offsets to sample:
    # - pages 1-10 (sequential early pages)
    # - then exponentially spaced pages up to the last page
    sample_offsets: list[int] = []
    # First 10 pages
    for p in range(min(10, total_pages)):
        sample_offsets.append(p * page_size)
    # Exponentially spaced: 20, 40, 80, 160, 320, 640, 1280, 2560 ...
    page_num = 20
    while page_num < total_pages:
        sample_offsets.append(page_num * page_size)
        page_num = int(page_num * 2)
    # Always include the last page
    last_offset = (total_pages - 1) * page_size
    if last_offset not in sample_offsets:
        sample_offsets.append(last_offset)

    print(f"\n{'=' * 70}")
    print(f"  READ / SCROLL: sample {len(sample_offsets)} pages across "
          f"{total_articles:,} articles (limit={page_size})")
    print(f"{'=' * 70}")

    pages_fetched = 0
    articles_seen = 0
    timings: list[float] = []
    errors = 0

    t0 = time.perf_counter()
    for offset in sample_offsets:
        try:
            t_page = time.perf_counter()
            status, data = _api_get(port, f"/api/articles?limit={page_size}&offset={offset}&sort=desc")
            elapsed = time.perf_counter() - t_page
        except (ConnectionResetError, ConnectionRefusedError, OSError) as e:
            print(f"  ERROR at offset {offset}: {e}")
            errors += 1
            if errors >= 3:
                print("  Too many errors, stopping scroll benchmark.")
                break
            continue

        timings.append(elapsed)

        if status != 200 or not data:
            print(f"  ERROR at offset {offset}: HTTP {status}")
            errors += 1
            if errors >= 3:
                break
            continue

        items = data if isinstance(data, list) else data.get("articles", data.get("items", []))
        count = len(items)
        pages_fetched += 1
        articles_seen += count

        page_label = offset // page_size + 1
        print(f"  Page {page_label:5d} (offset={offset:6d}): "
              f"{count} articles in {_fmt_duration(elapsed)}")

    total_time = time.perf_counter() - t0
    avg_page = sum(timings) / len(timings) if timings else 0
    p50 = sorted(timings)[len(timings) // 2] if timings else 0
    p95 = sorted(timings)[int(len(timings) * 0.95)] if timings else 0
    p99 = sorted(timings)[int(len(timings) * 0.99)] if timings else 0

    print(f"\n  Pages fetched: {pages_fetched}")
    print(f"  Articles seen: {articles_seen:,}")
    print(f"  Total time:    {_fmt_duration(total_time)}")
    print(f"  Avg/page:      {_fmt_duration(avg_page)}")
    print(f"  p50:           {_fmt_duration(p50)}")
    print(f"  p95:           {_fmt_duration(p95)}")
    print(f"  p99:           {_fmt_duration(p99)}")

    return {
        "total_articles": articles_seen,
        "pages": pages_fetched,
        "total_time": total_time,
        "avg_page": avg_page,
        "p50": p50,
        "p95": p95,
        "p99": p99,
    }


def bench_read_by_feed(port: int) -> dict:
    """Fetch feeds list, then scroll articles for a sample of feeds."""
    print(f"\n{'=' * 70}")
    print(f"  READ / PER-FEED SCROLL")
    print(f"{'=' * 70}")

    status, feeds_data = _api_get(port, "/api/feeds")
    if status != 200:
        print(f"  ERROR fetching feeds: HTTP {status}")
        return {}

    feeds = feeds_data if isinstance(feeds_data, list) else feeds_data.get("feeds", [])

    # Sample up to 50 feeds evenly across the list to keep runtime reasonable
    max_sample = 50
    if len(feeds) > max_sample:
        step = len(feeds) // max_sample
        sampled_feeds = [feeds[i] for i in range(0, len(feeds), step)][:max_sample]
    else:
        sampled_feeds = feeds
    print(f"  Feeds: {len(feeds)} total, sampling {len(sampled_feeds)}")

    page_size = 50
    total_pages = 0
    total_articles = 0
    timings: list[float] = []
    errors = 0

    t0 = time.perf_counter()
    for i, feed in enumerate(sampled_feeds):
        feed_id = feed.get("feed_id", feed.get("id", ""))
        offset = 0
        while True:
            try:
                t_page = time.perf_counter()
                status, data = _api_get(
                    port,
                    f"/api/feeds/{feed_id}/articles?limit={page_size}&offset={offset}&sort=desc",
                )
                elapsed = time.perf_counter() - t_page
            except (ConnectionResetError, ConnectionRefusedError, OSError) as e:
                print(f"  ERROR feed {feed_id} offset {offset}: {e}")
                errors += 1
                break
            timings.append(elapsed)

            if status != 200 or not data:
                break
            items = data if isinstance(data, list) else data.get("articles", data.get("items", []))
            total_pages += 1
            total_articles += len(items)
            if len(items) < page_size:
                break
            offset += page_size

        if errors >= 3:
            print("  Too many errors, stopping per-feed benchmark.")
            break

        if (i + 1) % 10 == 0:
            print(f"  ... scrolled {i + 1}/{len(sampled_feeds)} feeds "
                  f"({total_articles:,} articles, {total_pages} pages)")

    total_time = time.perf_counter() - t0
    avg_page = sum(timings) / len(timings) if timings else 0
    p95 = sorted(timings)[int(len(timings) * 0.95)] if timings else 0

    print(f"\n  Feeds scrolled: {len(sampled_feeds)} (of {len(feeds)} total)")
    print(f"  Total pages:    {total_pages}")
    print(f"  Total articles: {total_articles:,}")
    print(f"  Total time:     {_fmt_duration(total_time)}")
    print(f"  Avg/page:       {_fmt_duration(avg_page)}")
    print(f"  p95/page:       {_fmt_duration(p95)}")

    return {
        "feeds": len(sampled_feeds),
        "total_pages": total_pages,
        "total_articles": total_articles,
        "total_time": total_time,
        "avg_page": avg_page,
        "p95": p95,
    }


def run_read_benchmarks(data_dir: str, port: int) -> list[dict]:
    """Start the Go server on a temp DB and benchmark reading."""

    # First populate a good-sized dataset: 1000 feeds × random 10-250 articles
    print("\n  Preparing read benchmark data (1000 feeds × 10-250 articles)...")
    bench_insert(data_dir, num_feeds=1000, articles_per_feed=None,
                 min_articles=10, max_articles=250,
                 label="(read bench data)")

    # Count total articles
    lance_db = lancedb.connect(data_dir)
    total_articles = lance_db.open_table("articles").to_pandas().shape[0]
    print(f"  Total articles in DB: {total_articles:,}")

    # Find server binary
    if sys.platform == "win32":
        exe = PROJECT_ROOT / "build" / "rss-lance-server.exe"
    else:
        exe = PROJECT_ROOT / "build" / "rss-lance-server"

    if not exe.exists():
        print(f"\n  !! Server binary not found at {exe}")
        print("  !! Build it first: build.ps1 server  (or build.sh server)")
        print("  !! Skipping read benchmarks.")
        return []

    # Write a minimal config for the benchmark server
    # path = "." works because config is inside data_dir and the server
    # resolves storage.path relative to the config file location.
    bench_config = os.path.join(data_dir, "bench_config.toml")
    frontend_path = str(PROJECT_ROOT / "frontend").replace("\\", "/")
    with open(bench_config, "w") as f:
        f.write('[storage]\ntype = "local"\npath = "."\n\n')
        f.write(f'[server]\nhost = "127.0.0.1"\nport = {port}\n')
        f.write(f'frontend_dir = "{frontend_path}"\n')
        f.write('show_shutdown = false\n')

    # Start the server — log stderr to file to avoid pipe buffer deadlock
    stderr_log = os.path.join(data_dir, "server_stderr.log")
    print(f"\n  Starting server on port {port} ...")
    stderr_fh = open(stderr_log, "w")
    server_proc = subprocess.Popen(
        [str(exe), "--config", bench_config, "--port", str(port)],
        stdout=subprocess.DEVNULL,
        stderr=stderr_fh,
    )

    results = []
    try:
        if not _wait_for_server(port):
            stderr_fh.flush()
            stderr_out = ""
            try:
                stderr_out = open(stderr_log).read()[:500]
            except Exception:
                pass
            poll = server_proc.poll()
            print(f"  !! Server failed to start within 30s (poll={poll}). Skipping read benchmarks.")
            if stderr_out:
                print(f"  !! stderr: {stderr_out}")
            return []
        print(f"  Server ready (pid {server_proc.pid})")

        # Benchmark 1: scroll all articles
        r1 = bench_read_scroll(port, total_articles)
        results.append({"type": "scroll_all", **r1})

        # Benchmark 2: scroll per-feed
        r2 = bench_read_by_feed(port)
        results.append({"type": "scroll_per_feed", **r2})

    finally:
        server_proc.terminate()
        try:
            server_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server_proc.kill()
        stderr_fh.close()
        print("  Server stopped.")

    return results


# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

def print_summary(insert_results: list[dict], read_results: list[dict],
                  sanitize_results: list[dict] | None = None,
                  pipeline_results: list[dict] | None = None):
    print(f"\n{'#' * 70}")
    print(f"  BENCHMARK SUMMARY")
    print(f"{'#' * 70}")

    if insert_results:
        print(f"\n  INSERT")
        print(f"  {'Label':<45} {'Articles':>10} {'Time':>10} {'Rate':>15}")
        print(f"  {'-' * 82}")
        for r in insert_results:
            rate = _fmt_rate(r["num_articles"], r["article_insert_time"])
            print(f"  {r['label']:<45} {r['num_articles']:>10,} "
                  f"{_fmt_duration(r['article_insert_time']):>10} {rate:>15}")

    if sanitize_results:
        print(f"\n  SANITIZE")
        print(f"  {'Step':<30} {'Articles':>10} {'Time':>10} {'Rate':>15}")
        print(f"  {'-' * 67}")
        for r in sanitize_results:
            rate = _fmt_rate(r["count"], r["time"])
            print(f"  {r['step']:<30} {r['count']:>10,} "
                  f"{_fmt_duration(r['time']):>10} {rate:>15}")

    if pipeline_results:
        print(f"\n  PIPELINE (sanitize + insert)")
        print(f"  {'Label':<30} {'Articles':>10} {'Sanitize':>10} "
              f"{'Insert':>10} {'Total':>10} {'Rate':>12}")
        print(f"  {'-' * 84}")
        for r in pipeline_results:
            rate = _fmt_rate(r["num_articles"], r["pipeline_time"])
            print(f"  {r['label']:<30} {r['num_articles']:>10,} "
                  f"{_fmt_duration(r['sanitize_time']):>10} "
                  f"{_fmt_duration(r['insert_time']):>10} "
                  f"{_fmt_duration(r['pipeline_time']):>10} {rate:>12}")

    if read_results:
        print(f"\n  READ")
        print(f"  {'Type':<25} {'Articles':>10} {'Pages':>8} {'Total':>10} "
              f"{'Avg/pg':>10} {'p95/pg':>10}")
        print(f"  {'-' * 75}")
        for r in read_results:
            print(f"  {r.get('type', '?'):<25} "
                  f"{r.get('total_articles', 0):>10,} "
                  f"{r.get('pages', r.get('total_pages', 0)):>8} "
                  f"{_fmt_duration(r.get('total_time', 0)):>10} "
                  f"{_fmt_duration(r.get('avg_page', 0)):>10} "
                  f"{_fmt_duration(r.get('p95', 0)):>10}")

    print()


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="RSS-Lance Benchmark")
    parser.add_argument("mode", nargs="?", default="all",
                        choices=["all", "insert", "sanitize", "pipeline", "read"],
                        help="Which benchmarks to run (default: all)")
    parser.add_argument("--log", default="none",
                        choices=["none", "info", "debug"],
                        help="Logging level (default: none)")
    parser.add_argument("--data-dir", default="",
                        help="Override temp data directory (for debugging)")
    args = parser.parse_args()

    # Configure logging
    import logging
    if args.log == "none":
        logging.disable(logging.CRITICAL)
    elif args.log == "info":
        logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
    elif args.log == "debug":
        logging.basicConfig(level=logging.DEBUG, format="%(levelname)s %(name)s: %(message)s")

    # Create isolated temp directory
    if args.data_dir:
        data_dir = str(Path(args.data_dir).resolve())
        os.makedirs(data_dir, exist_ok=True)
        cleanup = False
    else:
        data_dir = tempfile.mkdtemp(prefix="rss-lance-bench-")
        cleanup = True

    port = _free_port()

    print(f"\nRSS-Lance Benchmark")
    print(f"  Data dir: {data_dir}")
    print(f"  Port:     {port}")
    print(f"  Logging:  {args.log}")
    print(f"  Mode:     {args.mode}")

    insert_results: list[dict] = []
    read_results: list[dict] = []
    sanitize_results: list[dict] = []
    pipeline_results: list[dict] = []

    try:
        if args.mode in ("all", "insert"):
            insert_results = run_insert_benchmarks(data_dir)

        if args.mode in ("all", "sanitize"):
            sanitize_results = run_sanitize_benchmarks()

        if args.mode in ("all", "pipeline"):
            pipeline_results = run_pipeline_benchmarks(data_dir)

        if args.mode in ("all", "read"):
            read_results = run_read_benchmarks(data_dir, port)

        print_summary(insert_results, read_results, sanitize_results, pipeline_results)

    finally:
        if cleanup:
            shutil.rmtree(data_dir, ignore_errors=True)
            print(f"  Cleaned up: {data_dir}")
        else:
            print(f"  Data preserved at: {data_dir}")


if __name__ == "__main__":
    main()
