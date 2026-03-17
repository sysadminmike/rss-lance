"""
TT-RSS Postgres → LanceDB migration script.

Usage:
    python migrate/import_ttrss.py
    python migrate/import_ttrss.py --dry-run    # count rows, don't write
    python migrate/import_ttrss.py --feeds-only
    python migrate/import_ttrss.py --articles-only
    python migrate/import_ttrss.py --categories-only

Set the Postgres URL in config.toml under [migration.ttrss] postgres_url.
"""

from __future__ import annotations

import argparse
import logging
import sys
import uuid
from datetime import datetime, timezone
from pathlib import Path

import psycopg2
import psycopg2.extras

# Allow importing from project root
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "fetcher"))

from db import DB, FEEDS_SCHEMA, ARTICLES_SCHEMA, CATEGORIES_SCHEMA
from config import Config
from common import ArticleRow, write_articles
from content_cleaner import strip_dangerous_html, strip_tracking_params, strip_tracking_pixels
from feed_parser import strip_social_links

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("migrate")


def _utc(dt: datetime | None) -> datetime | None:
    if dt is None:
        return None
    if dt.tzinfo is None:
        return dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


# ── categories ────────────────────────────────────────────────────────────────

def migrate_categories(pg, db: DB, dry_run: bool) -> dict[int, str]:
    """Returns mapping {ttrss_cat_id: lance_category_id}."""
    cur = pg.cursor(cursor_factory=psycopg2.extras.DictCursor)
    cur.execute("""
        SELECT id, title, parent_cat, order_id
        FROM ttrss_feed_categories
        ORDER BY parent_cat NULLS FIRST, id
    """)
    rows = cur.fetchall()
    log.info("Categories in TT-RSS: %d", len(rows))
    if dry_run:
        return {}

    # Two passes: top-level then children (handles single level of nesting)
    id_map: dict[int, str] = {}
    for row in rows:
        cat_id = str(uuid.uuid4())
        id_map[row["id"]] = cat_id
        parent_lance_id = id_map.get(row["parent_cat"] or 0, "")
        db.categories.add([{
            "category_id": cat_id,
            "name":        row["title"] or "Uncategorized",
            "parent_id":   parent_lance_id,
            "sort_order":  row["order_id"] or 0,
        }], schema=CATEGORIES_SCHEMA)

    log.info("Migrated %d categories", len(rows))
    return id_map


# ── feeds ─────────────────────────────────────────────────────────────────────

def migrate_feeds(pg, db: DB, cat_map: dict[int, str], dry_run: bool) -> dict[int, str]:
    """Returns mapping {ttrss_feed_id: lance_feed_id}."""
    cur = pg.cursor(cursor_factory=psycopg2.extras.DictCursor)
    cur.execute("""
        SELECT id, title, feed_url, site_url, favicon_url,
               cat_id, update_interval, last_updated, last_error
        FROM ttrss_feeds
        ORDER BY id
    """)
    rows = cur.fetchall()
    log.info("Feeds in TT-RSS: %d", len(rows))
    if dry_run:
        return {}

    id_map: dict[int, str] = {}
    batch: list[dict] = []
    now = datetime.now(timezone.utc)

    for row in rows:
        feed_id = str(uuid.uuid4())
        id_map[row["id"]] = feed_id
        batch.append({
            "feed_id":              feed_id,
            "title":                row["title"] or "",
            "url":                  row["feed_url"] or "",
            "site_url":             row["site_url"] or "",
            "icon_url":             row["favicon_url"] or "",
            "category_id":          cat_map.get(row["cat_id"] or 0, ""),
            "subcategory_id":       "",
            "last_fetched":         _utc(row["last_updated"]),
            "last_article_date":    None,
            "fetch_interval_mins":  max(row["update_interval"] or 30, 15),
            "fetch_tier":           "active",
            "tier_changed_at":      now,
            "last_successful_fetch":_utc(row["last_updated"]),
            "error_count":          0,
            "last_error":           row["last_error"] or "",
            "created_at":           now,
        })

    if batch:
        db.feeds.add(batch, schema=FEEDS_SCHEMA)
    log.info("Migrated %d feeds", len(batch))
    return id_map


# ── articles ──────────────────────────────────────────────────────────────────

def _sanitise(html: str) -> str:
    """Run the full server-side sanitisation pipeline on article HTML."""
    html = strip_dangerous_html(html)
    html = strip_social_links(html)
    html = strip_tracking_pixels(html)
    html = strip_tracking_params(html)
    return html


def migrate_articles(pg, db: DB, feed_map: dict[int, str], dry_run: bool,
                     sanitize: bool = True, batch_size: int = 500) -> None:
    if sanitize:
        log.info("Sanitization enabled (disable with sanitize = false in config.toml)")
    else:
        log.info("Sanitization disabled by config")

    # Server-side cursor streams rows without loading all into memory
    cur = pg.cursor(cursor_factory=psycopg2.extras.DictCursor, name="art_cursor")
    cur.execute("""
        SELECT
            e.id,
            e.feed_id,
            e.title,
            e.link,
            e.author,
            e.content,
            e.updated     AS published_at,
            e.guid,
            COALESCE(ue.unread, FALSE) = FALSE AS is_read,
            COALESCE(ue.marked, FALSE)          AS is_starred
        FROM ttrss_entries e
        LEFT JOIN ttrss_user_entries ue
            ON ue.ref_id = e.id AND ue.owner_uid = 1
        ORDER BY e.id
    """)

    count_cur = pg.cursor()
    count_cur.execute("""
        SELECT COUNT(*) FROM ttrss_entries e
        LEFT JOIN ttrss_user_entries ue ON ue.ref_id = e.id AND ue.owner_uid = 1
    """)
    total = count_cur.fetchone()[0]
    log.info("Articles in TT-RSS: %d", total)

    # common.write_articles handles batching, dedup, and progress bar.
    # TT-RSS uses integer feed_ids, so we key by str(ttrss_feed_id).
    str_feed_map = {str(k): v for k, v in feed_map.items()}

    def _row_iter():
        for row in cur:
            content = row["content"] or ""
            if sanitize:
                content = _sanitise(content)
            yield ArticleRow(
                feed_key     = str(row["feed_id"]),
                guid         = row["guid"] or str(row["id"]),
                title        = row["title"] or "",
                url          = row["link"] or "",
                author       = row["author"] or "",
                content      = content,
                published_at = _utc(row["published_at"]),
                is_read      = bool(row["is_read"]),
                is_starred   = bool(row["is_starred"]),
            )

    written = write_articles(
        _row_iter(), db, str_feed_map, dry_run,
        total_hint=total, batch_size=batch_size,
    )
    log.info("Migrated %d articles", written)


# ── main ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(description="Migrate TT-RSS → LanceDB")
    parser.add_argument("--dry-run",        action="store_true")
    parser.add_argument("--feeds-only",     action="store_true")
    parser.add_argument("--articles-only",  action="store_true")
    parser.add_argument("--categories-only",action="store_true")
    args = parser.parse_args()

    config = Config()
    ttrss_cfg = config.ttrss_config
    pg_url = ttrss_cfg.get("postgres_url")
    if not pg_url:
        log.error("postgres_url not set in config.toml [migration.ttrss] section")
        sys.exit(1)

    log.info("Connecting to Postgres…")
    pg = psycopg2.connect(pg_url)

    log.info("Opening LanceDB at %s", config.storage_path)
    db = DB(config)

    do_all = not (args.feeds_only or args.articles_only or args.categories_only)

    cat_map: dict[int, str] = {}
    feed_map: dict[int, str] = {}

    if do_all or args.categories_only:
        cat_map = migrate_categories(pg, db, dry_run=args.dry_run)

    if do_all or args.feeds_only:
        feed_map = migrate_feeds(pg, db, cat_map, dry_run=args.dry_run)

    if do_all or args.articles_only:
        if not feed_map:
            # Rebuild feed_map from existing Lance data
            log.info("Rebuilding feed map from existing Lance data (articles-only mode)…")
            # Not supported without knowing original TT-RSS IDs - warn and exit
            log.error("--articles-only requires a fresh run with feeds already migrated "
                      "and ttrss feed IDs recorded. Run without flags for a full migration.")
            sys.exit(1)
        sanitize = ttrss_cfg.get("sanitize", True)
        migrate_articles(pg, db, feed_map, dry_run=args.dry_run, sanitize=sanitize)

    pg.close()
    if args.dry_run:
        log.info("Dry run complete - no data written")
    else:
        log.info("Migration complete!")


if __name__ == "__main__":
    main()
