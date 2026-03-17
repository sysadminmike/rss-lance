"""
LanceDB → OPML export script.

Exports all feed subscriptions (and their category/folder structure) to a
standard OPML 2.0 file that can be imported into any RSS reader.

Usage:
    python migrate/export_opml.py output.opml
    python migrate/export_opml.py output.opml --title "My Feeds"
    python migrate/export_opml.py -                          # stdout

The resulting file preserves folder hierarchy:

    <outline text="Tech">
        <outline type="rss" text="Ars Technica" xmlUrl="https://…" />
    </outline>
"""

from __future__ import annotations

import argparse
import logging
import sys
import xml.etree.ElementTree as ET
from datetime import datetime, timezone
from pathlib import Path

# Allow importing from project root
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "fetcher"))

from config import Config
from db import DB

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("export_opml")


def build_opml(db: DB, title: str = "RSS-Lance Subscriptions") -> ET.Element:
    """Read feeds and categories from LanceDB and build an OPML element tree."""
    categories = db.get_categories()
    feeds = db.get_all_feeds()

    # Build category lookup: id → record, and id → child categories
    cat_by_id: dict[str, dict] = {}
    children_of: dict[str, list[str]] = {}  # parent_id → [child cat ids]
    root_cats: list[str] = []

    for cat in categories:
        cid = cat["category_id"]
        cat_by_id[cid] = cat
        pid = cat.get("parent_id") or ""
        if pid:
            children_of.setdefault(pid, []).append(cid)
        else:
            root_cats.append(cid)

    # Sort root categories by sort_order then name
    root_cats.sort(key=lambda c: (cat_by_id[c].get("sort_order", 0), cat_by_id[c].get("name", "")))

    # Group feeds by category_id
    feeds_by_cat: dict[str, list[dict]] = {}
    uncategorised: list[dict] = []
    for feed in feeds:
        cid = feed.get("category_id") or ""
        if cid and cid in cat_by_id:
            feeds_by_cat.setdefault(cid, []).append(feed)
        else:
            uncategorised.append(feed)

    # Build XML
    opml = ET.Element("opml", version="2.0")
    head = ET.SubElement(opml, "head")
    ET.SubElement(head, "title").text = title
    ET.SubElement(head, "dateCreated").text = datetime.now(timezone.utc).strftime(
        "%a, %d %b %Y %H:%M:%S +0000"
    )

    body = ET.SubElement(opml, "body")

    def _add_feed_outline(parent: ET.Element, feed: dict) -> None:
        attrs = {
            "type": "rss",
            "text": feed.get("title") or feed.get("url", ""),
            "title": feed.get("title") or feed.get("url", ""),
            "xmlUrl": feed.get("url", ""),
        }
        if feed.get("site_url"):
            attrs["htmlUrl"] = feed["site_url"]
        ET.SubElement(parent, "outline", **attrs)

    def _add_category(parent: ET.Element, cat_id: str) -> None:
        cat = cat_by_id[cat_id]
        folder = ET.SubElement(parent, "outline", text=cat["name"], title=cat["name"])

        # Nested child categories
        child_ids = children_of.get(cat_id, [])
        child_ids.sort(key=lambda c: (cat_by_id[c].get("sort_order", 0), cat_by_id[c].get("name", "")))
        for child_id in child_ids:
            _add_category(folder, child_id)

        # Feeds in this category
        for feed in sorted(feeds_by_cat.get(cat_id, []), key=lambda f: f.get("title", "")):
            _add_feed_outline(folder, feed)

    # Write categorised feeds
    for cat_id in root_cats:
        _add_category(body, cat_id)

    # Write uncategorised feeds at top level
    for feed in sorted(uncategorised, key=lambda f: f.get("title", "")):
        _add_feed_outline(body, feed)

    return opml


def export_opml(db: DB, title: str = "RSS-Lance Subscriptions") -> str:
    """Return the OPML XML as a string."""
    opml = build_opml(db, title)
    ET.indent(opml)
    return '<?xml version="1.0" encoding="UTF-8"?>\n' + ET.tostring(
        opml, encoding="unicode"
    )


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Export LanceDB feed subscriptions to an OPML file"
    )
    parser.add_argument(
        "output",
        help="Path to the output .opml file (use '-' for stdout)",
    )
    parser.add_argument(
        "--title",
        default="RSS-Lance Subscriptions",
        help="Title to embed in the OPML <head> (default: RSS-Lance Subscriptions)",
    )
    args = parser.parse_args()

    config = Config()
    log.info("Opening LanceDB at %s", config.storage_path)
    db = DB(config)

    xml_str = export_opml(db, title=args.title)

    feeds = db.get_all_feeds()
    categories = db.get_categories()
    log.info("Exported %d feeds in %d categories", len(feeds), len(categories))

    if args.output == "-":
        sys.stdout.write(xml_str)
    else:
        out = Path(args.output)
        out.write_text(xml_str, encoding="utf-8")
        log.info("Written to %s", out)


if __name__ == "__main__":
    main()
