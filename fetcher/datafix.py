"""
RSS-Lance data-fix runner.

Applies named fixes to existing articles in the database.  Each fix is
a function that receives an article dict and returns a (possibly modified)
dict.  Fixes are registered in the FIXES dict and invoked by name:

    python fetcher/datafix.py <fixname> [--data ./data] [--dry-run]

Fixes work per-feed so that feed-level logic (like template detection)
can compare articles from the same source.  Only articles whose content
actually changed are written back, to avoid unnecessary Lance rewrites.

The --max-version flag (default: target articles below current SCHEMA_VERSION)
filters articles so only those written by an older schema get processed.
After a fix runs, affected articles are stamped with the current version.

Usage:
    .\\run.ps1 datafix strip-chrome          # strip chrome on old-version articles
    .\\run.ps1 datafix strip-chrome --dry-run # preview without writing
    .\\run.ps1 datafix strip-chrome --all     # process ALL articles regardless of version
    .\\run.ps1 datafix list                   # list available fixes
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
from typing import Callable

import pandas as pd

# Ensure fetcher/ is importable
sys.path.insert(0, os.path.dirname(__file__))

from config import Config
from db import DB, SCHEMA_VERSION, _utcnow, _escape_filter_value

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("datafix")

# Type alias: a fix function takes (list[dict], DB) → list[dict]
# It receives all articles for one feed, returns modified articles.
FixFn = Callable[[list[dict], DB], list[dict]]

# ── Fix registry ────────────────────────────────────────────────────────────

FIXES: dict[str, tuple[str, FixFn]] = {}


def register_fix(name: str, description: str):
    """Decorator to register a data fix function."""
    def decorator(fn: FixFn) -> FixFn:
        FIXES[name] = (description, fn)
        return fn
    return decorator


# ── Built-in fixes ──────────────────────────────────────────────────────────

@register_fix("strip-chrome", "Strip repeated site chrome (nav, related posts) from article content")
def fix_strip_chrome(articles: list[dict], db: DB) -> list[dict]:
    from content_cleaner import strip_site_chrome
    return strip_site_chrome(articles)


@register_fix("strip-social", "Re-run social link stripping on all articles")
def fix_strip_social(articles: list[dict], db: DB) -> list[dict]:
    from feed_parser import strip_social_links
    for art in articles:
        if art.get("content"):
            art["content"] = strip_social_links(art["content"])
        if art.get("summary"):
            art["summary"] = strip_social_links(art["summary"])
    return articles


# ── Runner ──────────────────────────────────────────────────────────────────

def run_fix(fix_name: str, db: DB, dry_run: bool = False,
            max_version: int | None = None) -> None:
    """Run a named fix across all articles, grouped by feed.

    max_version: only process articles with schema_version <= this value.
                 None means use SCHEMA_VERSION - 1 (i.e. only old articles).
                 Use -1 to process ALL articles regardless of version.
    """
    if fix_name not in FIXES:
        log.error("Unknown fix: '%s'. Available: %s",
                  fix_name, ", ".join(FIXES.keys()))
        return

    description, fix_fn = FIXES[fix_name]

    if max_version is None:
        max_version = SCHEMA_VERSION - 1

    process_all = max_version < 0
    version_label = "all versions" if process_all else f"schema_version <= {max_version}"
    log.info("Running fix '%s': %s [%s]%s",
             fix_name, description, version_label,
             " (DRY RUN)" if dry_run else "")

    # Load articles, get feed IDs, then process per-feed to limit memory usage
    df = db.articles.to_pandas()
    if df.empty:
        log.info("No articles in database.")
        return

    # Filter by schema_version
    if not process_all:
        # Articles without schema_version (NULL/NaN) are treated as version 1
        ver_col = df["schema_version"].fillna(1).astype(int) if "schema_version" in df.columns else pd.Series(1, index=df.index)
        df = df[ver_col <= max_version]
        if df.empty:
            log.info("No articles with schema_version <= %d. Nothing to fix.", max_version)
            return

    feed_ids = df["feed_id"].unique().tolist()
    total_articles = len(df)
    log.info("Found %d articles across %d feeds to process",
             total_articles, len(feed_ids))
    del df  # free the full DataFrame before per-feed processing

    total_changed = 0

    for feed_id in feed_ids:
        # Load only this feed's articles from Lance (filtered at source)
        feed_df = db.articles.to_pandas()
        feed_df = feed_df[feed_df["feed_id"] == feed_id]
        if not process_all and "schema_version" in feed_df.columns:
            ver_col = feed_df["schema_version"].fillna(1).astype(int)
            feed_df = feed_df[ver_col <= max_version]
        if feed_df.empty:
            continue
        articles = feed_df.to_dict("records")
        del feed_df  # free per-feed DataFrame
        # Snapshot originals for comparison
        originals = {a["article_id"]: (a.get("content", ""), a.get("summary", ""))
                     for a in articles}

        # Apply the fix
        fixed = fix_fn(articles, db)

        # Find what actually changed
        changed = []
        for art in fixed:
            aid = art["article_id"]
            orig_content, orig_summary = originals.get(aid, ("", ""))
            if art.get("content", "") != orig_content or art.get("summary", "") != orig_summary:
                changed.append(art)

        if not changed:
            # Even if content didn't change, stamp version on all processed articles
            if not dry_run and not process_all:
                for art in articles:
                    db.articles.update(
                        f"article_id = '{_escape_filter_value(art['article_id'])}'",
                        {"schema_version": SCHEMA_VERSION, "updated_at": _utcnow()},
                    )
            continue

        feed_title = articles[0].get("title", feed_id)[:40] if articles else feed_id
        log.info("  Feed %s: %d/%d articles changed", feed_title, len(changed), len(articles))
        total_changed += len(changed)

        if dry_run:
            for art in changed[:3]:  # show first 3 as preview
                orig_len = len(originals[art["article_id"]][0])
                new_len = len(art.get("content", ""))
                log.info("    %s: content %d → %d bytes",
                         art.get("title", "?")[:50], orig_len, new_len)
            continue

        # Write changes back — update each changed article + stamp version
        for art in changed:
            aid = art["article_id"]
            updates: dict[str, object] = {"schema_version": SCHEMA_VERSION, "updated_at": _utcnow()}
            orig_content, orig_summary = originals[aid]
            if art.get("content", "") != orig_content:
                updates["content"] = art["content"]
            if art.get("summary", "") != orig_summary:
                updates["summary"] = art["summary"]
            db.articles.update(f"article_id = '{_escape_filter_value(aid)}'", updates)

        # Stamp version on unchanged articles in this feed too
        if not process_all:
            unchanged_ids = set(a["article_id"] for a in articles) - set(a["article_id"] for a in changed)
            for aid in unchanged_ids:
                db.articles.update(f"article_id = '{_escape_filter_value(aid)}'", {"schema_version": SCHEMA_VERSION, "updated_at": _utcnow()})

    if dry_run:
        log.info("DRY RUN complete: %d articles would be modified", total_changed)
    else:
        log.info("Fix complete: %d articles modified, all processed articles stamped as v%d",
                 total_changed, SCHEMA_VERSION)


def list_fixes() -> None:
    """Print all registered fixes."""
    print(f"\nAvailable data fixes (current schema version: {SCHEMA_VERSION}):")
    print("=" * 60)
    for name, (desc, _) in sorted(FIXES.items()):
        print(f"  {name:20s}  {desc}")
    print(f"\nBy default, only articles with schema_version < {SCHEMA_VERSION} are processed.")
    print("Use --all to force processing all articles regardless of version.\n")


def main() -> None:
    parser = argparse.ArgumentParser(description="RSS-Lance data fix runner")
    parser.add_argument("fix", help="Fix name to run, or 'list' to show available fixes")
    parser.add_argument("--data", metavar="PATH", help="Data directory path")
    parser.add_argument("--dry-run", action="store_true",
                        help="Preview changes without writing")
    parser.add_argument("--all", action="store_true", dest="all_versions",
                        help="Process ALL articles regardless of schema version")
    parser.add_argument("--max-version", type=int, default=None,
                        help="Only process articles with schema_version <= N")
    args = parser.parse_args()

    if args.fix == "list":
        list_fixes()
        return

    config = Config()
    if args.data:
        config.storage_path = args.data

    max_ver = -1 if args.all_versions else args.max_version

    db = DB(config)
    run_fix(args.fix, db, dry_run=args.dry_run, max_version=max_ver)


if __name__ == "__main__":
    main()
