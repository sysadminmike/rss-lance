"""
FreshRSS → LanceDB import script.

Imports categories (labels/folders), feeds, and all articles from a running
FreshRSS instance using the Google Reader compatible API that FreshRSS
exposes at /api/greader.php.

Usage:
    python migrate/import_freshrss.py
    python migrate/import_freshrss.py --dry-run
    python migrate/import_freshrss.py --feeds-only
    python migrate/import_freshrss.py --articles-only
    python migrate/import_freshrss.py --categories-only

Configuration (config.toml):

    [migration.freshrss]
    url      = "https://freshrss.example.com"   # base URL (no trailing slash)
    username = "admin"
    password = "your-password"

    # Optional: override the GReader API path if your install differs
    # api_path = "/api/greader.php"   # default

Enable the API in FreshRSS → Settings → Authentication → Allow API access.

Notes:
    - Folder hierarchy is preserved: FreshRSS labels are mapped to
      CategoryRow objects with parent_name when nested folders exist.
      (FreshRSS itself is flat-label, but any folder nesting in the
      category name like "Parent/Child" will be split automatically.)
    - All articles in the reading-list stream are imported; read/starred
      status is read from each item's Google Reader state categories.
    - Duplicate feeds/articles already in LanceDB are skipped.
    - Large installs paginate via the GReader continuation token.
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
log = logging.getLogger("import_freshrss")

# Google Reader state marker prefixes
_STATE_READ    = "com.google/read"
_STATE_STARRED = "com.google/starred"


# ── FreshRSS / GReader API client ─────────────────────────────────────────────

class FreshRSSClient:
    """Minimal Google Reader API client for FreshRSS."""

    def __init__(self, base_url: str, username: str, password: str,
                 api_path: str = "/api/greader.php") -> None:
        self.base     = base_url.rstrip("/")
        self.api_base = self.base + api_path
        self.session  = requests.Session()
        self.session.headers["User-Agent"] = "RSS-Lance-migrator/1.0"
        self._auth_token: str | None = None

        self._login(username, password)

    # ── authentication ────────────────────────────────────────────────────

    def _login(self, username: str, password: str) -> None:
        url = f"{self.api_base}/accounts/ClientLogin"
        r = self.session.post(
            url,
            data={"Email": username, "Passwd": password},
            timeout=30,
        )
        r.raise_for_status()
        for line in r.text.splitlines():
            if line.startswith("Auth="):
                self._auth_token = line[5:].strip()
                break
        if not self._auth_token:
            raise ValueError(
                "GReader login succeeded but no Auth token in response.\n"
                f"Response: {r.text[:200]}"
            )
        self.session.headers["Authorization"] = (
            f"GoogleLogin auth={self._auth_token}"
        )
        log.info("GReader authentication successful")

    # ── API calls ──────────────────────────────────────────────────────────

    def _get(self, path: str, **params) -> dict:
        params.setdefault("output", "json")
        r = self.session.get(
            f"{self.api_base}{path}", params=params, timeout=60
        )
        r.raise_for_status()
        return r.json()

    def tag_list(self) -> list[dict]:
        """Return all user labels / folder tags."""
        data = self._get("/reader/api/0/tag/list")
        return data.get("tags") or []

    def subscription_list(self) -> list[dict]:
        """Return all feed subscriptions."""
        data = self._get("/reader/api/0/subscription/list")
        return data.get("subscriptions") or []

    def stream_contents(
        self,
        stream_id: str = "user/-/state/com.google/reading-list",
        page_size: int = 200,
        oldest_first: bool = True,
    ) -> Iterator[list[dict]]:
        """Paginate through a GReader stream, yielding pages of items."""
        params: dict = {
            "n":  page_size,
            "r":  "o" if oldest_first else "n",
            "xt": "user/-/state/com.google/read",  # include read items too
        }
        # Remove xt so we get everything (read + unread)
        params.pop("xt", None)

        while True:
            data = self._get(f"/reader/api/0/stream/contents/{stream_id}", **params)
            items = data.get("items") or []
            yield items

            continuation = data.get("continuation")
            if not continuation or not items:
                break
            params["c"] = continuation

    def starred_ids(self) -> set[str]:
        """Return set of item IDs that are starred (for cross-check)."""
        ids: set[str] = set()
        for page in self.stream_contents("user/-/state/com.google/starred"):
            for item in page:
                ids.add(item.get("id", ""))
        return ids


# ── data translation ───────────────────────────────────────────────────────────

def _label_name(tag_id: str) -> str | None:
    """Extract the human label from a GReader tag id.

    e.g. "user/1/label/Tech News" → "Tech News"
         "user/-/label/Tech News" → "Tech News"
         "user/-/state/..."       → None  (system tag, skip)
    """
    if "/label/" not in tag_id:
        return None
    return tag_id.split("/label/", 1)[1]


def build_categories(raw_tags: list[dict]) -> list[CategoryRow]:
    """
    Convert GReader tags to CategoryRow list.

    FreshRSS itself stores flat labels, but some users encode hierarchy
    with "Parent/Child" slashes in the label name.  We detect this and
    create intermediate parent rows automatically.
    """
    seen: dict[str, CategoryRow] = {}   # name → CategoryRow
    rows: list[CategoryRow] = []

    sort = 0
    for tag in raw_tags:
        label = _label_name(tag.get("id", ""))
        if label is None:
            continue
        label = label.strip()
        if not label or label in seen:
            continue

        # Handle "Parent/Child" slash-encoded nesting
        parts = [p.strip() for p in label.split("/") if p.strip()]
        parent_name: str | None = None
        for i, part in enumerate(parts):
            path = "/".join(parts[: i + 1])
            if path not in seen:
                cat = CategoryRow(
                    name        = path,
                    parent_name = parent_name,
                    sort_order  = sort,
                )
                seen[path] = cat
                rows.append(cat)
                sort += 1
            parent_name = path

    return rows


def build_feeds(raw_subs: list[dict]) -> tuple[list[FeedRow], dict[str, str]]:
    """
    Returns
    -------
    feeds          : list of FeedRow
    stream_id_map  : {gReader_stream_id: feed_url}
                     e.g. {"feed/https://example.com/rss": "https://example.com/rss"}
    """
    rows: list[FeedRow] = []
    stream_id_map: dict[str, str] = {}

    for sub in raw_subs:
        # The feed URL is in the `url` field (the actual HTTP URL)
        feed_url = sub.get("url") or ""
        if not feed_url:
            # Fall back to stripping the "feed/" prefix from the streamId
            stream_id = sub.get("id") or ""
            if stream_id.startswith("feed/"):
                feed_url = stream_id[5:]
        if not feed_url:
            continue

        # Build stream_id → feed_url map for article lookup
        stream_id = sub.get("id") or f"feed/{feed_url}"
        stream_id_map[stream_id] = feed_url

        # Use first category label as the folder name
        cats = sub.get("categories") or []
        cat_name: str | None = None
        if cats:
            raw_label = _label_name(cats[0].get("id", ""))
            if raw_label:
                cat_name = raw_label.strip()

        rows.append(FeedRow(
            url           = feed_url,
            title         = sub.get("title") or feed_url,
            site_url      = sub.get("htmlUrl") or "",
            icon_url      = sub.get("iconUrl") or "",
            category_name = cat_name,
        ))

    return rows, stream_id_map


def _extract_content(item: dict) -> str:
    """Pull the best content string from a GReader item."""
    # `content` is usually the full HTML
    content_obj = item.get("content")
    if isinstance(content_obj, dict):
        return content_obj.get("content") or ""
    # `summary` is the excerpt / fallback
    summary_obj = item.get("summary")
    if isinstance(summary_obj, dict):
        return summary_obj.get("content") or ""
    return ""


def _item_url(item: dict) -> str:
    canonical = item.get("canonical")
    if canonical and isinstance(canonical, list) and canonical[0]:
        return canonical[0].get("href") or ""
    alternate = item.get("alternate")
    if alternate and isinstance(alternate, list) and alternate[0]:
        return alternate[0].get("href") or ""
    return ""


def iter_articles(
    client: FreshRSSClient,
    stream_id_map: dict[str, str],
    page_size: int = 200,
) -> Iterator[ArticleRow]:
    """Stream all articles from the reading-list."""
    for page in client.stream_contents(page_size=page_size):
        for item in page:
            origin = item.get("origin") or {}
            stream_id = origin.get("streamId") or ""
            feed_url = stream_id_map.get(stream_id)

            if not feed_url:
                # Try stripping "feed/" prefix as fallback
                if stream_id.startswith("feed/"):
                    feed_url = stream_id_map.get(stream_id) or stream_id[5:]
                if not feed_url:
                    continue

            # Read / starred state from categories list
            cats = item.get("categories") or []
            is_read    = any(_STATE_READ    in c for c in cats if isinstance(c, str))
            is_starred = any(_STATE_STARRED in c for c in cats if isinstance(c, str))

            pub_ts = item.get("published")
            try:
                pub = datetime.fromtimestamp(int(pub_ts), tz=timezone.utc)
            except Exception:
                pub = None

            yield ArticleRow(
                feed_key     = feed_url,
                guid         = item.get("id") or "",
                title        = item.get("title") or "",
                url          = _item_url(item),
                author       = item.get("author") or "",
                content      = _extract_content(item),
                summary      = "",
                published_at = pub,
                is_read      = is_read,
                is_starred   = is_starred,
            )


# ── main ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Import FreshRSS data → LanceDB via GReader API"
    )
    parser.add_argument("--dry-run",         action="store_true")
    parser.add_argument("--feeds-only",      action="store_true")
    parser.add_argument("--articles-only",   action="store_true")
    parser.add_argument("--categories-only", action="store_true")
    args = parser.parse_args()

    config = Config()
    fr_cfg = config.freshrss_config
    if not fr_cfg.get("url"):
        log.error(
            "freshrss.url not set in config.toml [migration.freshrss] section"
        )
        sys.exit(1)
    if not fr_cfg.get("username") or not fr_cfg.get("password"):
        log.error(
            "freshrss.username and freshrss.password must be set in config.toml"
        )
        sys.exit(1)

    log.info("Connecting to FreshRSS at %s …", fr_cfg["url"])
    client = FreshRSSClient(
        base_url = fr_cfg["url"],
        username = fr_cfg["username"],
        password = fr_cfg["password"],
        api_path = fr_cfg.get("api_path", "/api/greader.php"),
    )

    log.info("Opening LanceDB at %s", config.storage_path)
    db = DB(config)

    do_all = not (args.feeds_only or args.articles_only or args.categories_only)

    # ── categories ──────────────────────────────────────────────────────
    cat_map: dict[str, str] = {}
    if do_all or args.categories_only:
        log.info("Fetching tag/label list from FreshRSS …")
        raw_tags = client.tag_list()
        log.info("  %d tags found (filtering system tags)", len(raw_tags))
        cats = build_categories(raw_tags)
        log.info("  %d user folders/categories", len(cats))
        cat_map = write_categories(cats, db, dry_run=args.dry_run)

    # ── feeds ────────────────────────────────────────────────────────────
    stream_id_map: dict[str, str] = {}
    feed_url_map:  dict[str, str] = {}

    if do_all or args.feeds_only:
        log.info("Fetching subscription list from FreshRSS …")
        raw_subs = client.subscription_list()
        log.info("  %d subscriptions found", len(raw_subs))
        feed_rows, stream_id_map = build_feeds(raw_subs)
        feed_url_map = write_feeds(
            feed_rows, db, cat_map, dry_run=args.dry_run
        )

    # ── articles ─────────────────────────────────────────────────────────
    if do_all or args.articles_only:
        if not stream_id_map:
            # articles-only: rebuild maps from API + DB
            log.info("Rebuilding feed maps from FreshRSS subs + LanceDB …")
            raw_subs = client.subscription_list()
            _, stream_id_map = build_feeds(raw_subs)
            feed_url_map = load_feed_url_map(db)

        log.info("Streaming articles from FreshRSS reading-list …")
        article_iter = iter_articles(client, stream_id_map)
        write_articles(
            article_iter, db, feed_url_map,
            dry_run    = args.dry_run,
            total_hint = None,   # GReader API doesn't expose a total count upfront
        )

    if args.dry_run:
        log.info("Dry run complete - no data written")
    else:
        log.info("Import complete!")


if __name__ == "__main__":
    main()
