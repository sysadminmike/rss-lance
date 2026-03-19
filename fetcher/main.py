"""
RSS-Lance feed fetcher daemon / one-shot runner.

Usage:
    python main.py               # daemon mode - uses schedule library
    python main.py --once        # one-shot mode - fetch all due feeds, then exit
    python main.py --add <url>   # add a feed URL then exit
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import logging
import os
import subprocess
import sys
import time
from datetime import datetime, timezone

import schedule

from config import Config
from db import DB, _escape_filter_value
from feed_parser import fetch_feed
from content_cleaner import strip_site_chrome

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("fetcher")

# ── tier thresholds (days without new articles → downgrade) ─────────────────
# These are compiled-in defaults; runtime values come from the settings table.
TIER_THRESHOLDS = {
    "active":   3,    # → slowing after 3 days
    "slowing":  14,   # → quiet after 2 weeks
    "quiet":    60,   # → dormant after 2 months
    "dormant":  180,  # → dead after 6 months
}

TIER_INTERVALS = {
    "active":  30,
    "slowing": 60 * 24,          # daily
    "quiet":   60 * 24 * 7,      # weekly
    "dormant": 60 * 24 * 30,     # monthly
    "dead":    None,
}


def _load_tier_settings(db: DB) -> tuple[dict, dict]:
    """Return (thresholds, intervals) dicts from the settings table,
    falling back to the compiled-in defaults for any missing key."""
    settings = db.get_all_settings()
    thresholds = {}
    for tier, default in TIER_THRESHOLDS.items():
        val = settings.get(f"tier.threshold.{tier}")
        thresholds[tier] = int(val) if val is not None else default
    intervals = {}
    for tier, default in TIER_INTERVALS.items():
        if default is None:
            intervals[tier] = None
            continue
        val = settings.get(f"tier.interval.{tier}")
        intervals[tier] = int(val) if val is not None else default
    # dead is always None (unfetchable)
    intervals["dead"] = None
    return thresholds, intervals


def _compute_new_tier(feed: dict, had_new_articles: bool,
                      thresholds: dict | None = None) -> str | None:
    """Return new tier name if it should change, else None.

    If *thresholds* is None, falls back to the compiled-in TIER_THRESHOLDS.
    """
    current = feed.get("fetch_tier", "active") or "active"
    t = thresholds or TIER_THRESHOLDS

    if had_new_articles and current != "active":
        # Promote back to active immediately
        return "active"

    if had_new_articles:
        return None  # already active, no change

    # Check if we should downgrade
    last_art = feed.get("last_article_date")
    if last_art is None:
        return None
    if hasattr(last_art, "tzinfo") and last_art.tzinfo is None:
        last_art = last_art.replace(tzinfo=timezone.utc)

    days_quiet = (datetime.now(timezone.utc) - last_art).days
    tier_order = ["active", "slowing", "quiet", "dormant", "dead"]

    for tier in tier_order:
        threshold = t.get(tier)
        if threshold is not None and days_quiet >= threshold:
            next_tier = tier_order[tier_order.index(tier) + 1]
            if next_tier != current and tier_order.index(next_tier) > tier_order.index(current):
                return next_tier
    return None


def fetch_one(feed: dict, db: DB, config: Config,
              tier_thresholds: dict | None = None,
              tier_intervals: dict | None = None,
              fetch_timeout: int = 20,
              user_agent: str = "RSS-Lance/1.0") -> None:
    """Fetch, parse, and store results for a single feed."""
    feed_id = feed["feed_id"]
    url = feed["url"]
    feed_title = feed.get("title") or url
    log.info("Fetching %s  (%s)", feed_title, url)

    result = fetch_feed(url, feed_id, user_agent=user_agent, timeout=fetch_timeout)

    if result.error:
        log.warning("Error fetching %s: %s", url, result.error)
        db.log_event("error", "errors", f"Fetch error: {url}",
                     json.dumps({"feed_id": feed_id, "title": feed_title, "error": result.error}))
        db.update_feed_after_fetch(feed_id, success=False, error_msg=result.error)
        return

    # Filter out already-stored articles
    existing_guids = db.get_existing_guids(feed_id)
    new_articles = [a for a in result.articles if a["guid"] not in existing_guids]

    # Strip repeated site chrome (nav, related posts, etc.) by comparing articles
    chrome_report: list[str] = []
    if len(result.articles) >= 2:
        strip_site_chrome(result.articles, report=chrome_report)

    # Log sanitization findings (per-article from feed_parser + chrome from here)
    san_report = result.sanitize_report + chrome_report
    if san_report:
        log.debug("Sanitization report for %s: %s", feed_title, "; ".join(san_report))
        db.log_event("debug", "sanitization",
                     f"Sanitization: {feed_title} — {len(san_report)} item(s) stripped",
                     json.dumps({"feed_id": feed_id, "title": feed_title,
                                 "items": san_report}))
    else:
        db.log_event("debug", "sanitization",
                     f"Sanitization: {feed_title} — nothing stripped",
                     json.dumps({"feed_id": feed_id, "title": feed_title,
                                 "articles_checked": len(result.articles)}))

    added = db.add_articles(new_articles)
    log.info("  %d new articles added (%d total in feed)", added, len(result.articles))

    db.log_event("info", "feed_fetch",
                 f"Fetched {feed_title}: {added} new articles",
                 json.dumps({"feed_id": feed_id, "title": feed_title,
                             "new": added, "total": len(result.articles)}))

    # Log each article at debug level
    for a in new_articles:
        db.log_event("debug", "article_processing",
                     f"New article: {a.get('title', '(untitled)')}",
                     json.dumps({"feed_id": feed_id, "article_id": a.get("article_id"),
                                 "guid": a.get("guid"), "url": a.get("url")}))

    # Compute dates for tier logic
    last_pub: datetime | None = None
    if new_articles:
        last_pub = max(
            (a["published_at"] for a in new_articles if a["published_at"]),
            default=None,
        )

    new_tier = _compute_new_tier(feed, had_new_articles=bool(new_articles),
                                thresholds=tier_thresholds)
    if new_tier:
        log.info("  Feed tier changed: %s -> %s", feed.get("fetch_tier"), new_tier)
        db.log_event("info", "tier_changes",
                     f"Tier change: {feed_title} {feed.get('fetch_tier')} -> {new_tier}",
                     json.dumps({"feed_id": feed_id, "title": feed_title,
                                 "old_tier": feed.get("fetch_tier"), "new_tier": new_tier}))
        # Queue interval update along with fetch-after update
        ivl = tier_intervals or TIER_INTERVALS
        new_interval = ivl.get(new_tier, 30)
        db.update_feed_after_fetch(
            feed_id,
            success=True,
            last_article_date=last_pub,
            new_tier=new_tier,
        )
        # Also queue interval change
        if db._batching:
            db._feed_updates.append((feed_id, {"fetch_interval_mins": new_interval}))
        else:
            db.feeds.update(f"feed_id = '{_escape_filter_value(feed_id)}'", {"fetch_interval_mins": new_interval})
    else:
        db.update_feed_after_fetch(
            feed_id,
            success=True,
            last_article_date=last_pub,
            new_tier=new_tier,
        )

    # Update feed title / site_url if we got better data
    if result.title and not feed.get("title"):
        if db._batching:
            db._feed_updates.append((feed_id, {
                "title": result.title,
                "site_url": result.site_url,
                "icon_url": result.icon_url,
            }))
        else:
            db.feeds.update(f"feed_id = '{_escape_filter_value(feed_id)}'", {
                "title": result.title,
                "site_url": result.site_url,
                "icon_url": result.icon_url,
            })


def run_once(db: DB, config: Config) -> None:
    """Fetch all due feeds once, respecting max_concurrent.

    All article inserts and feed metadata updates are batched into a
    single Lance write at the end of the cycle.  This reduces S3 cost
    (fewer PUT/version operations) and is the correct pattern for
    Lance - fewer, larger fragments.
    """
    feeds = db.get_feeds_due()
    log.info("Found %d feeds due for fetching", len(feeds))
    if not feeds:
        db.log_event("info", "fetch_cycle", "Fetch cycle: 0 feeds due")
        return

    # Load tier & fetch settings once per cycle
    tier_thresholds, tier_intervals = _load_tier_settings(db)
    all_settings = db.get_all_settings()
    fetch_timeout = int(all_settings.get("fetcher.fetch_timeout_secs", 20))
    max_concurrent = int(all_settings.get("fetcher.max_concurrent", 5))
    user_agent = str(all_settings.get("fetcher.user_agent", "RSS-Lance/1.0"))

    db.begin_batch()

    with concurrent.futures.ThreadPoolExecutor(
        max_workers=max_concurrent
    ) as pool:
        futures = {
            pool.submit(fetch_one, f, db, config,
                        tier_thresholds, tier_intervals, fetch_timeout,
                        user_agent): f
            for f in feeds
        }
        for fut in concurrent.futures.as_completed(futures):
            exc = fut.exception()
            if exc:
                feed = futures[fut]
                log.error("Unhandled error for %s: %s", feed.get("url"), exc)

    written = db.flush_batch()
    log_written = db.flush_log_batch()
    log.info("Batch flush: %d articles written, %d feed updates applied",
             written, len(feeds))

    db.log_event("info", "fetch_cycle",
                 f"Fetch cycle complete: {len(feeds)} feeds, {written} articles",
                 json.dumps({"feeds_due": len(feeds), "articles_written": written}))

    # Run compaction on any table whose fragments exceed its threshold
    compacted = db.compact_if_needed()
    for name, was_compacted in compacted.items():
        if was_compacted:
            log.info("Compacted table: %s", name)
            db.log_event("info", "compaction", f"Compacted table: {name}")

    # Trim old log entries
    db.trim_logs()


def add_feed(url: str, db: DB, config: Config) -> None:
    """Add a new feed URL, fetch it immediately, then exit."""
    existing = [f for f in db.get_all_feeds() if f["url"] == url]
    if existing:
        log.info("Feed already exists: %s", url)
        return

    # Fetch first to get title
    log.info("Adding and fetching: %s", url)
    user_agent = str(db.get_all_settings().get("fetcher.user_agent", "RSS-Lance/1.0"))
    result = fetch_feed(url, "tmp", user_agent=user_agent)
    feed_id = db.add_feed(
        url=url,
        title=result.title,
        site_url=result.site_url,
    )
    db.feeds.update(f"feed_id = '{_escape_filter_value(feed_id)}'", {"icon_url": result.icon_url})

    # Now store articles
    if not result.error:
        db.add_articles([
            {**a, "feed_id": feed_id, "article_id": __import__("uuid").str(uuid.uuid4())}
            if a["feed_id"] == "tmp" else {**a, "feed_id": feed_id}
            for a in result.articles
        ])
        db.update_feed_after_fetch(feed_id, success=True)
        log.info("Added feed '%s' with %d articles", result.title, len(result.articles))
    else:
        log.warning("Feed added but initial fetch failed: %s", result.error)


def _get_git_info() -> tuple[str, str]:
    """Return (commit_short, commit_date) from git, or empty strings."""
    try:
        repo_dir = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        rev = subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=repo_dir, stderr=subprocess.DEVNULL, text=True,
            timeout=30,
        ).strip()
        ts = subprocess.check_output(
            ["git", "log", "-1", "--format=%cI"],
            cwd=repo_dir, stderr=subprocess.DEVNULL, text=True,
            timeout=30,
        ).strip()
        return rev, ts
    except Exception:
        return "", ""


def main() -> None:
    parser = argparse.ArgumentParser(description="RSS-Lance feed fetcher")
    parser.add_argument("--once", action="store_true",
                        help="Fetch all due feeds once then exit (good for cron)")
    parser.add_argument("--add", metavar="URL",
                        help="Add a new feed by URL then exit")
    args = parser.parse_args()

    git_rev, git_date = _get_git_info()
    if git_rev:
        log.info("Build: %s", git_rev)
    if git_date:
        log.info("Built at: %s", git_date)

    config = Config()
    db = DB(config)

    if args.add:
        add_feed(args.add, db, config)
        return

    if args.once:
        run_once(db, config)
        return

    # Daemon mode — read interval from settings table (editable via web UI)
    all_settings = db.get_all_settings()
    interval = int(all_settings.get("fetcher.interval_minutes", 30))
    log.info("Starting RSS-Lance fetcher daemon (interval: %d min)", interval)
    run_once(db, config)  # immediate first pass

    schedule.every(interval).minutes.do(run_once, db=db, config=config)

    poll_interval = int(db.get_all_settings().get("fetcher.poll_interval_secs", 30))
    while True:
        schedule.run_pending()
        time.sleep(poll_interval)


if __name__ == "__main__":
    main()
