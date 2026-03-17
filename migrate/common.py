"""
Shared helpers for all RSS-Lance migration / import scripts.

Each importer translates its source data into the three canonical row
types defined here (CategoryRow, FeedRow, ArticleRow), then hands them
to the write_* functions which handle:

  - duplicate detection  (skip categories / feeds / articles already in DB)
  - folder hierarchy     (parent_name resolved to parent_id automatically)
  - bulk writes to LanceDB
  - progress logging

Typical importer flow
─────────────────────
    from common import (
        CategoryRow, FeedRow, ArticleRow,
        write_categories, write_feeds, write_articles,
        load_feed_url_map,
    )

    cats  = [CategoryRow(name="Tech"), CategoryRow(name="AI", parent_name="Tech")]
    feeds = [FeedRow(url="https://…", title="…", category_name="Tech")]
    articles = (ArticleRow(feed_key="https://…", guid="…", title="…") for …)

    cat_map      = write_categories(cats,     db, dry_run)   # {name: lance_id}
    feed_url_map = write_feeds(feeds, db, cat_map, dry_run)  # {url: lance_id}
    write_articles(articles, db, feed_url_map, dry_run, total_hint=N)
"""

from __future__ import annotations

import logging
import sys
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterator

from tqdm import tqdm

# Allow importing fetcher modules when running from the project root or
# from the migrate/ directory.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "fetcher"))

import pyarrow as pa
from db import DB, FEEDS_SCHEMA, ARTICLES_SCHEMA, CATEGORIES_SCHEMA

log = logging.getLogger("migrate")


# ── canonical row types ───────────────────────────────────────────────────────

@dataclass
class CategoryRow:
    """A single folder / category to be written to LanceDB.

    Attributes
    ----------
    name        : display name shown in the UI
    parent_name : text name of the parent folder, or None for top-level
    sort_order  : ordering hint (0 = first)
    lance_id    : auto-generated UUID; override only if you need a stable ID
    """
    name:        str
    parent_name: str | None = None
    sort_order:  int = 0
    lance_id:    str = field(default_factory=lambda: str(uuid.uuid4()))


@dataclass
class FeedRow:
    """A single RSS/Atom feed subscription.

    Attributes
    ----------
    url               : the feed URL (xmlUrl); used as the unique key
    title             : display name
    site_url          : the home-page URL
    icon_url          : favicon / logo URL
    category_name     : display name of the folder this feed belongs to;
                        must match a CategoryRow.name that was already
                        written (or returned by write_categories)
    fetch_interval_mins: default polling interval
    """
    url:                 str
    title:               str = ""
    site_url:            str = ""
    icon_url:            str = ""
    category_name:       str | None = None
    fetch_interval_mins: int = 30


@dataclass
class ArticleRow:
    """A single article / entry.

    Attributes
    ----------
    feed_key   : key used to look up the LanceDB feed_id via the
                 feed_key_to_lance_id dict passed to write_articles.
                 Typically the feed URL, but can be any consistent key the
                 importer chooses (e.g. str(miniflux_feed_id)).
    guid       : unique identifier within the feed; used for deduplication
    """
    feed_key:    str
    guid:        str
    title:       str = ""
    url:         str = ""
    author:      str = ""
    content:     str = ""
    summary:     str = ""
    published_at: datetime | None = None
    is_read:     bool = False
    is_starred:  bool = False


# ── DB inspection helpers ─────────────────────────────────────────────────────

def load_existing_categories(db: DB) -> dict[str, str]:
    """Return {category_name: lance_category_id} for every row in the DB."""
    try:
        d = db.categories.to_arrow().to_pydict()
        return dict(zip(d.get("name", []), d.get("category_id", [])))
    except Exception:
        return {}


def load_feed_url_map(db: DB) -> dict[str, str]:
    """Return {feed_url: lance_feed_id} for every row in the DB."""
    try:
        d = db.feeds.to_arrow().to_pydict()
        return dict(zip(d.get("url", []), d.get("feed_id", [])))
    except Exception:
        return {}


def load_existing_feed_urls(db: DB) -> set[str]:
    """Return the set of feed URLs already in the DB."""
    return set(load_feed_url_map(db).keys())


def load_existing_guid_pairs(db: DB) -> set[tuple[str, str]]:
    """Return {(lance_feed_id, guid)} for every article already in the DB."""
    try:
        d = db.articles.to_arrow().to_pydict()
        return set(zip(d.get("feed_id", []), d.get("guid", [])))
    except Exception:
        return set()


# ── write helpers ──────────────────────────────────────────────────────────────

def write_categories(
    categories: list[CategoryRow],
    db: DB,
    dry_run: bool,
    existing: dict[str, str] | None = None,
) -> dict[str, str]:
    """Write categories / folders to LanceDB.

    Skips any category whose name already exists in the DB.
    Parent references are resolved in-order, so pass categories
    parent-first (or rely on iterating depth-first which is what
    all importers do naturally).

    Returns
    -------
    dict[str, str]
        {category_name: lance_category_id} for *all* categories -
        both newly written and already-existing - so callers can
        always resolve a name to an ID.
    """
    if existing is None:
        existing = load_existing_categories(db)

    name_to_id: dict[str, str] = dict(existing)
    new_count = 0
    skipped   = 0

    for cat in categories:
        if cat.name in name_to_id:
            skipped += 1
            continue

        parent_id = name_to_id.get(cat.parent_name or "", "")
        if not dry_run:
            db.categories.add(
                [{
                    "category_id": cat.lance_id,
                    "name":        cat.name,
                    "parent_id":   parent_id,
                    "sort_order":  cat.sort_order,
                }],
            )
        name_to_id[cat.name] = cat.lance_id
        new_count += 1

    log.info("Categories - %d new, %d already existed", new_count, skipped)
    return name_to_id


def write_feeds(
    feeds: list[FeedRow],
    db: DB,
    cat_name_map: dict[str, str],
    dry_run: bool,
    existing_urls: set[str] | None = None,
) -> dict[str, str]:
    """Write feed subscriptions to LanceDB.

    Skips any feed whose URL is already in the DB.

    Returns
    -------
    dict[str, str]
        {feed_url: lance_feed_id} for *all* feeds - both newly written
        and already-existing - so article importers can resolve URLs.
    """
    # Seed with already-existing url→id pairs
    url_to_id: dict[str, str] = load_feed_url_map(db)

    if existing_urls is None:
        existing_urls = set(url_to_id.keys())

    now     = datetime.now(timezone.utc)
    batch:  list[dict] = []
    skipped = 0

    for feed in feeds:
        if feed.url in existing_urls:
            skipped += 1
            continue

        feed_id = str(uuid.uuid4())
        url_to_id[feed.url] = feed_id
        batch.append({
            "feed_id":               feed_id,
            "title":                 feed.title or feed.url,
            "url":                   feed.url,
            "site_url":              feed.site_url,
            "icon_url":              feed.icon_url,
            "category_id":           cat_name_map.get(feed.category_name or "", ""),
            "subcategory_id":        "",
            "last_fetched":          None,
            "last_article_date":     None,
            "fetch_interval_mins":   feed.fetch_interval_mins,
            "fetch_tier":            "active",
            "tier_changed_at":       now,
            "last_successful_fetch": None,
            "error_count":           0,
            "last_error":            "",
            "created_at":            now,
        })
        existing_urls.add(feed.url)

    if batch and not dry_run:
        db.feeds.add(batch)

    log.info("Feeds - %d new, %d already existed", len(batch), skipped)
    return url_to_id


def write_articles(
    articles: Iterator[ArticleRow] | list[ArticleRow],
    db: DB,
    feed_key_to_lance_id: dict[str, str],
    dry_run: bool,
    total_hint: int | None = None,
    batch_size: int = 500,
) -> int:
    """Write articles to LanceDB with guid-level deduplication.

    Parameters
    ----------
    articles              : list or lazy iterator of ArticleRow
    feed_key_to_lance_id  : maps each ArticleRow.feed_key to a LanceDB feed_id.
                            Build this from write_feeds() return value or
                            load_feed_url_map().
    total_hint            : optional total count for the progress bar
    batch_size            : rows per LanceDB append (tune for memory vs. I/O)

    Returns
    -------
    int  - number of articles actually written
    """
    existing_pairs: set[tuple[str, str]] = (
        load_existing_guid_pairs(db) if not dry_run else set()
    )

    now          = datetime.now(timezone.utc)
    batch:       list[dict] = []
    written      = 0
    skipped_dup  = 0
    skipped_feed = 0

    with tqdm(total=total_hint, desc="articles", unit="art") as pbar:
        for art in articles:
            pbar.update(1)

            feed_id = feed_key_to_lance_id.get(art.feed_key)
            if not feed_id:
                skipped_feed += 1
                continue

            pair = (feed_id, art.guid)
            if pair in existing_pairs:
                skipped_dup += 1
                continue
            existing_pairs.add(pair)

            pub = art.published_at or now
            if pub.tzinfo is None:
                pub = pub.replace(tzinfo=timezone.utc)

            batch.append({
                "article_id":   str(uuid.uuid4()),
                "feed_id":      feed_id,
                "title":        art.title   or "",
                "url":          art.url     or "",
                "author":       art.author  or "",
                "content":      art.content or "",
                "summary":      art.summary or "",
                "published_at": pub,
                "fetched_at":   now,
                "is_read":      art.is_read,
                "is_starred":   art.is_starred,
                "guid":         art.guid,
            })

            if len(batch) >= batch_size:
                if not dry_run:
                    db.articles.add(batch)
                written += len(batch)
                batch = []

        if batch:
            if not dry_run:
                db.articles.add(batch)
            written += len(batch)

    log.info(
        "Articles - %d written, %d duplicate guids skipped, "
        "%d unknown-feed skipped",
        written, skipped_dup, skipped_feed,
    )
    return written
