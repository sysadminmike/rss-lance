"""
Miniflux → LanceDB import script.

Imports categories, feeds, and all articles from a running Miniflux instance
using its native REST API.

Usage:
    python migrate/import_miniflux.py
    python migrate/import_miniflux.py --dry-run
    python migrate/import_miniflux.py --feeds-only
    python migrate/import_miniflux.py --articles-only
    python migrate/import_miniflux.py --categories-only

Configuration (config.toml):

    [migration.miniflux]
    url       = "https://miniflux.example.com"
    api_token = "your-api-token"          # preferred
    # username = "admin"                  # basic-auth alternative
    # password = "secret"

API authentication:
    Preferred  → X-Auth-Token header (Settings → API Keys in Miniflux UI)
    Fallback   → HTTP Basic auth (username + password)

Notes:
    - Miniflux categories are single-level (no nesting).
    - Articles are fetched with all statuses (read + unread) and starred.
    - Duplicate feeds/articles already in LanceDB are skipped automatically.
    - The Miniflux API returns entries newest-first; all pages are imported.
"""

from __future__ import annotations

import argparse
import logging
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterator

import requests

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "fetcher"))

from config import Config
from db import DB
from common import (
    CategoryRow, FeedRow, ArticleRow,
    write_categories, write_feeds, write_articles,
    load_feed_url_map,
)

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("import_miniflux")


# ── Miniflux API client ───────────────────────────────────────────────────────

class MinifluxClient:
    """Thin HTTP wrapper for the Miniflux v1 API."""

    def __init__(self, base_url: str, api_token: str | None = None,
                 username: str | None = None, password: str | None = None) -> None:
        self.base = base_url.rstrip("/")
        self.session = requests.Session()
        self.session.headers["User-Agent"] = "RSS-Lance-migrator/1.0"

        if api_token:
            self.session.headers["X-Auth-Token"] = api_token
        elif username and password:
            self.session.auth = (username, password)
        else:
            raise ValueError(
                "Provide either api_token or username+password in config.toml "
                "[migration.miniflux]"
            )

    def _get(self, path: str, **params) -> dict | list:
        url = f"{self.base}/v1{path}"
        r = self.session.get(url, params=params, timeout=30)
        r.raise_for_status()
        return r.json()

    def me(self) -> dict:
        return self._get("/me")

    def categories(self) -> list[dict]:
        data = self._get("/categories")
        return data if isinstance(data, list) else []

    def feeds(self) -> list[dict]:
        data = self._get("/feeds")
        return data if isinstance(data, list) else []

    def entries(
        self,
        limit: int = 500,
        direction: str = "asc",
        statuses: tuple[str, ...] = ("read", "unread"),
    ) -> Iterator[tuple[int, list[dict]]]:
        """Yield (total, page_of_entries) pages until exhausted."""
        offset = 0
        while True:
            params: dict = {
                "limit":     limit,
                "offset":    offset,
                "direction": direction,
            }
            for s in statuses:
                params.setdefault("status[]", [])
                if isinstance(params["status[]"], list):
                    params["status[]"].append(s)

            # requests encodes list params as repeated keys
            url = f"{self.base}/v1/entries"
            r = self.session.get(
                url,
                params=[("limit", limit), ("offset", offset),
                         ("direction", direction)]
                        + [("status[]", s) for s in statuses],
                timeout=60,
            )
            r.raise_for_status()
            data = r.json()

            total   = data.get("total", 0)
            entries = data.get("entries") or []
            yield total, entries

            offset += len(entries)
            if not entries or offset >= total:
                break


# ── data translation ──────────────────────────────────────────────────────────

def build_categories(raw: list[dict]) -> list[CategoryRow]:
    """Miniflux categories are flat (no nesting)."""
    return [
        CategoryRow(name=c["title"], sort_order=i)
        for i, c in enumerate(raw)
    ]


def build_feeds(raw: list[dict]) -> tuple[list[FeedRow], dict[int, str]]:
    """
    Returns
    -------
    feeds            : list of FeedRow
    miniflux_id_map  : {miniflux_feed_id: feed_url} for later article lookup
    """
    rows: list[FeedRow] = []
    id_map: dict[int, str] = {}

    for f in raw:
        url = f.get("feed_url") or ""
        if not url:
            continue
        cat_title = (f.get("category") or {}).get("title") or None
        rows.append(FeedRow(
            url           = url,
            title         = f.get("title") or url,
            site_url      = f.get("site_url") or "",
            icon_url      = (f.get("icon") or {}).get("data") or "",
            category_name = cat_title,
        ))
        id_map[f["id"]] = url

    return rows, id_map


def iter_articles(
    client: MinifluxClient,
    miniflux_id_to_url: dict[int, str],
) -> Iterator[tuple[int | None, ArticleRow]]:
    """
    Yield (total_hint, ArticleRow) pairs.

    total_hint is set only on the first entry of the first page so the
    caller can initialise a progress bar; all subsequent yields have None.
    """
    first = True
    total = None

    for page_total, entries in client.entries():
        if first:
            total = page_total
            first = False

        for e in entries:
            feed_url = miniflux_id_to_url.get(e.get("feed_id", 0))
            if not feed_url:
                continue

            pub_raw = e.get("published_at") or e.get("created_at")
            try:
                pub = datetime.fromisoformat(pub_raw.replace("Z", "+00:00"))
            except Exception:
                pub = None

            yield total, ArticleRow(
                feed_key   = feed_url,
                guid       = str(e.get("hash") or e.get("id")),
                title      = e.get("title") or "",
                url        = e.get("url") or "",
                author     = e.get("author") or "",
                content    = e.get("content") or "",
                summary    = "",
                published_at = pub,
                is_read    = e.get("status") == "read",
                is_starred = bool(e.get("starred")),
            )
    # signal end
    yield 0, None  # type: ignore[misc]  # sentinel


def stream_articles(
    client: MinifluxClient,
    miniflux_id_to_url: dict[int, str],
) -> tuple[int | None, Iterator[ArticleRow]]:
    """Return (total, iterator_of_ArticleRow)."""
    gen = iter_articles(client, miniflux_id_to_url)
    # Peek to get total from first page
    first_total, first_art = next(gen)

    def _combined() -> Iterator[ArticleRow]:
        if first_art is not None:
            yield first_art
        for _total, art in gen:
            if art is not None:
                yield art

    return first_total, _combined()


# ── main ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Import Miniflux data → LanceDB"
    )
    parser.add_argument("--dry-run",         action="store_true")
    parser.add_argument("--feeds-only",      action="store_true")
    parser.add_argument("--articles-only",   action="store_true")
    parser.add_argument("--categories-only", action="store_true")
    args = parser.parse_args()

    config = Config()
    mf_cfg = config.miniflux_config
    if not mf_cfg.get("url"):
        log.error(
            "miniflux.url not set in config.toml [migration.miniflux] section"
        )
        sys.exit(1)

    log.info("Connecting to Miniflux at %s …", mf_cfg["url"])
    client = MinifluxClient(
        base_url  = mf_cfg["url"],
        api_token = mf_cfg.get("api_token"),
        username  = mf_cfg.get("username"),
        password  = mf_cfg.get("password"),
    )

    try:
        me = client.me()
        log.info("Authenticated as %s (id=%s)", me.get("username"), me.get("id"))
    except Exception as exc:
        log.error("Authentication failed: %s", exc)
        sys.exit(1)

    log.info("Opening LanceDB at %s", config.storage_path)
    db = DB(config)

    do_all = not (args.feeds_only or args.articles_only or args.categories_only)

    # ── categories ──────────────────────────────────────────────────────
    cat_map: dict[str, str] = {}
    if do_all or args.categories_only:
        log.info("Fetching categories from Miniflux …")
        raw_cats = client.categories()
        log.info("  %d categories found", len(raw_cats))
        cats = build_categories(raw_cats)
        cat_map = write_categories(cats, db, dry_run=args.dry_run)

    # ── feeds ────────────────────────────────────────────────────────────
    miniflux_id_to_url: dict[int, str] = {}
    feed_url_map: dict[str, str] = {}

    if do_all or args.feeds_only:
        log.info("Fetching feeds from Miniflux …")
        raw_feeds = client.feeds()
        log.info("  %d feeds found", len(raw_feeds))
        feed_rows, miniflux_id_to_url = build_feeds(raw_feeds)
        feed_url_map = write_feeds(
            feed_rows, db, cat_map, dry_run=args.dry_run
        )

    # ── articles ─────────────────────────────────────────────────────────
    if do_all or args.articles_only:
        if not miniflux_id_to_url:
            # articles-only mode: rebuild url map from DB + from API feeds
            log.info("Rebuilding feed URL map from DB + Miniflux feeds list …")
            raw_feeds = client.feeds()
            _, miniflux_id_to_url = build_feeds(raw_feeds)
            feed_url_map = load_feed_url_map(db)

        log.info("Streaming articles from Miniflux …")
        total_hint, article_iter = stream_articles(client, miniflux_id_to_url)
        if total_hint:
            log.info("  ~%d articles to import", total_hint)
        write_articles(
            article_iter, db, feed_url_map,
            dry_run    = args.dry_run,
            total_hint = total_hint,
        )

    if args.dry_run:
        log.info("Dry run complete - no data written")
    else:
        log.info("Import complete!")


if __name__ == "__main__":
    main()
