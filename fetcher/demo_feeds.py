"""
Insert demo RSS feeds into LanceDB for testing.

Usage:
    python fetcher/demo_feeds.py [--data ./data]

This creates the feeds/articles/categories tables if they don't exist,
and adds a handful of known-good RSS feeds for immediate testing.
"""

from __future__ import annotations

import argparse
import sys
import os

# Ensure fetcher/ is importable
sys.path.insert(0, os.path.dirname(__file__))

from config import Config
from db import DB

DEMO_FEEDS = [
    {
        "url": "https://www.theguardian.com/science/rss",
        "title": "The Guardian - Science",
        "site_url": "https://www.theguardian.com/science",
        "category": "Science & Tech",
    },
    {
        "url": "https://blog.lancedb.com/rss/",
        "title": "LanceDB Blog",
        "site_url": "https://blog.lancedb.com",
        "category": "Tech",
    },
    {
        "url": "https://hnrss.org/frontpage",
        "title": "Hacker News - Front Page",
        "site_url": "https://news.ycombinator.com",
        "category": "Tech",
    },
    {
        "url": "https://www.nasa.gov/rss/dyn/breaking_news.rss",
        "title": "NASA Breaking News",
        "site_url": "https://www.nasa.gov",
        "category": "Science & Tech",
    },
    {
        "url": "https://feeds.arstechnica.com/arstechnica/index",
        "title": "Ars Technica",
        "site_url": "https://arstechnica.com",
        "category": "Tech",
    },
]


def insert_demo_feeds(data_path: str | None = None) -> None:
    """Insert demo feeds into the LanceDB tables."""
    config = Config()
    if data_path:
        config.storage_path = data_path

    print(f"Using data path: {config.storage_path}")
    db = DB(config)

    existing_urls = {f["url"] for f in db.get_all_feeds()}
    added = 0

    for demo in DEMO_FEEDS:
        if demo["url"] in existing_urls:
            print(f"  [skip] Already exists: {demo['title']}")
            continue

        feed_id = db.add_feed(
            url=demo["url"],
            title=demo["title"],
            site_url=demo.get("site_url", ""),
        )
        print(f"  [added] {demo['title']}  (feed_id={feed_id})")
        added += 1

    print(f"\nDone. {added} new feeds added ({len(existing_urls)} already existed).")
    print("\nNext step: run the fetcher to populate articles:")
    print("  .\\run.ps1 fetch-once        (Windows)")
    print("  ./run.sh fetch-once          (Linux/macOS)")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Insert demo RSS feeds")
    parser.add_argument("--data", default=None,
                        help="Path to data directory (default: from config.toml)")
    args = parser.parse_args()
    insert_demo_feeds(args.data)
