"""
OPML → LanceDB import script.

Imports feed subscriptions (and their folder/category structure) from any
standard OPML file.  Almost every RSS reader can export OPML, including
Feedly, Inoreader, NewsBlur, Miniflux, FreshRSS, NetNewsWire, Reeder, etc.

Usage:
    python migrate/import_opml.py subscriptions.opml
    python migrate/import_opml.py subscriptions.opml --dry-run
    python migrate/import_opml.py subscriptions.opml --feeds-only
    python migrate/import_opml.py subscriptions.opml --categories-only

Notes:
    - OPML contains only feed metadata, not articles.
    - Nested folders are supported (any depth); they are flattened to
      parent/child pairs in LanceDB (deeper nesting collapses to the
      nearest ancestor that has been written).
    - Duplicate feeds (same xmlUrl already present) are skipped.
"""

from __future__ import annotations

import argparse
import logging
import sys
import xml.etree.ElementTree as ET
from pathlib import Path
from typing import Iterator

# Allow importing from project root
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "fetcher"))

from config import Config
from db import DB
from common import (
    CategoryRow, FeedRow,
    write_categories, write_feeds,
)

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("import_opml")


# ── OPML parsing ──────────────────────────────────────────────────────────────

def _iter_outlines(
    node: ET.Element, parent_folder: str | None = None
) -> Iterator[CategoryRow | FeedRow]:
    """Recursively yield CategoryRow / FeedRow from an OPML outline tree."""
    for child in node.findall("outline"):
        xml_url = child.get("xmlUrl") or child.get("xmlurl") or ""
        text    = (child.get("title") or child.get("text") or "").strip()

        if xml_url:
            html_url = child.get("htmlUrl") or child.get("htmlurl") or ""
            yield FeedRow(
                url           = xml_url.strip(),
                title         = text,
                site_url      = html_url.strip(),
                category_name = parent_folder,
            )
        else:
            if text:
                yield CategoryRow(name=text, parent_name=parent_folder)
                yield from _iter_outlines(child, parent_folder=text)
            else:
                yield from _iter_outlines(child, parent_folder=parent_folder)


def parse_opml(path: Path) -> tuple[list[CategoryRow], list[FeedRow], str]:
    tree = ET.parse(path)
    root = tree.getroot()

    body = root.find("body")
    if body is None:
        raise ValueError("OPML file has no <body> element")

    title = ""
    head  = root.find("head")
    if head is not None:
        title = head.findtext("title") or ""

    categories: list[CategoryRow] = []
    feeds:       list[FeedRow]    = []
    seen_cats:   set[str]         = set()

    for item in _iter_outlines(body):
        if isinstance(item, CategoryRow):
            if item.name not in seen_cats:
                categories.append(item)
                seen_cats.add(item.name)
        else:
            feeds.append(item)

    return categories, feeds, title.strip()





# ── main ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Import an OPML subscription file into LanceDB"
    )
    parser.add_argument("opml_file", type=Path, help="Path to the .opml file")
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Parse and count only; do not write anything",
    )
    parser.add_argument(
        "--feeds-only",
        action="store_true",
        help="Import feeds only (skip creating categories)",
    )
    parser.add_argument(
        "--categories-only",
        action="store_true",
        help="Import categories/folders only",
    )
    args = parser.parse_args()

    if not args.opml_file.exists():
        log.error("File not found: %s", args.opml_file)
        sys.exit(1)

    log.info("Parsing %s …", args.opml_file)
    categories, feeds, title = parse_opml(args.opml_file)
    title_str = f" ({title})" if title else ""
    log.info("OPML%s - %d folders, %d feeds", title_str, len(categories), len(feeds))

    config = Config()
    log.info("Opening LanceDB at %s", config.storage_path)
    db = DB(config)

    do_all = not (args.feeds_only or args.categories_only)
    cat_map: dict[str, str] = {}

    if do_all or args.categories_only:
        cat_map = write_categories(categories, db, dry_run=args.dry_run)

    if do_all or args.feeds_only:
        write_feeds(feeds, db, cat_map, dry_run=args.dry_run)

    if args.dry_run:
        log.info("Dry run complete - no data written")
    else:
        log.info("Import complete!")


if __name__ == "__main__":
    main()
