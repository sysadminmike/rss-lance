"""
OPML export → import round-trip test.

Creates a temporary LanceDB with categories and feeds, exports to OPML,
wipes the database, re-imports from the exported OPML, and verifies that
feeds and categories survive the round trip.

Run:
    python -m pytest migrate/test_opml_roundtrip.py -v
    python migrate/test_opml_roundtrip.py
"""

from __future__ import annotations

import os
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

# Ensure fetcher and migrate are importable
ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "fetcher"))
sys.path.insert(0, str(ROOT / "migrate"))

from db import DB
from export_opml import export_opml
from import_opml import parse_opml
from common import write_categories, write_feeds


class _FakeConfig:
    """Minimal Config replacement for DB tests."""

    def __init__(self, path: str):
        self.storage_path = path
        self.compaction_thresholds = {
            "articles": 999,
            "feeds": 999,
            "categories": 999,
            "pending_feeds": 999,
        }


class TestOpmlRoundTrip(unittest.TestCase):
    """Export feeds → OPML → clear DB → import → verify match."""

    def setUp(self):
        self.temp_dir = tempfile.mkdtemp(prefix="opml_rt_")
        self.data_path = os.path.join(self.temp_dir, "data")
        os.makedirs(self.data_path)
        self.opml_path = os.path.join(self.temp_dir, "export.opml")

    def tearDown(self):
        shutil.rmtree(self.temp_dir, ignore_errors=True)

    def _make_db(self) -> DB:
        return DB(_FakeConfig(self.data_path))

    def _populate(self, db: DB) -> tuple[dict[str, str], set[str]]:
        """Insert test categories and feeds. Returns (cat_name_map, feed_urls)."""
        # Categories: Tech (root), AI (child of Tech), News (root)
        cat_tech = db.add_category("Tech")
        cat_ai = db.add_category("AI", parent_id=cat_tech)
        cat_news = db.add_category("News")

        # Feeds
        db.add_feed(url="https://example.com/tech.xml", title="Tech Daily", site_url="https://example.com/tech", category_id=cat_tech)
        db.add_feed(url="https://example.com/ai.xml", title="AI Weekly", site_url="https://example.com/ai", category_id=cat_ai)
        db.add_feed(url="https://example.com/news.xml", title="World News", site_url="https://example.com/news", category_id=cat_news)
        db.add_feed(url="https://example.com/misc.xml", title="Misc Blog", site_url="https://example.com/misc")

        feed_urls = {
            "https://example.com/tech.xml",
            "https://example.com/ai.xml",
            "https://example.com/news.xml",
            "https://example.com/misc.xml",
        }
        cat_names = {"Tech", "AI", "News"}
        return cat_names, feed_urls

    def _wipe_data(self):
        """Delete and recreate the data directory (fresh DB)."""
        shutil.rmtree(self.data_path)
        os.makedirs(self.data_path)

    # ── tests ────────────────────────────────────────────────────────────

    def test_round_trip_preserves_feeds(self):
        """Feeds survive export → clear → import."""
        db = self._make_db()
        _, orig_urls = self._populate(db)

        # Export
        xml = export_opml(db)
        Path(self.opml_path).write_text(xml, encoding="utf-8")

        # Verify XML is parseable
        categories, feeds, title = parse_opml(Path(self.opml_path))
        exported_urls = {f.url for f in feeds}
        self.assertEqual(exported_urls, orig_urls, "Exported feed URLs should match originals")

        # Wipe DB and re-import
        self._wipe_data()
        db2 = self._make_db()
        cat_map = write_categories(categories, db2, dry_run=False)
        write_feeds(feeds, db2, cat_map, dry_run=False)

        # Verify
        reimported_feeds = db2.get_all_feeds()
        reimported_urls = {f["url"] for f in reimported_feeds}
        self.assertEqual(reimported_urls, orig_urls, "Re-imported feed URLs should match originals")

    def test_round_trip_preserves_categories(self):
        """Category names survive export → clear → import."""
        db = self._make_db()
        orig_cat_names, _ = self._populate(db)

        xml = export_opml(db)
        Path(self.opml_path).write_text(xml, encoding="utf-8")

        categories, feeds, _ = parse_opml(Path(self.opml_path))
        exported_cat_names = {c.name for c in categories}
        self.assertEqual(exported_cat_names, orig_cat_names)

        self._wipe_data()
        db2 = self._make_db()
        write_categories(categories, db2, dry_run=False)

        reimported = db2.get_categories()
        reimported_names = {c["name"] for c in reimported}
        self.assertEqual(reimported_names, orig_cat_names)

    def test_round_trip_preserves_category_hierarchy(self):
        """Parent→child relationships survive the round trip."""
        db = self._make_db()
        self._populate(db)

        xml = export_opml(db)
        Path(self.opml_path).write_text(xml, encoding="utf-8")

        categories, _, _ = parse_opml(Path(self.opml_path))

        # AI should have parent_name == "Tech"
        ai_cat = next((c for c in categories if c.name == "AI"), None)
        self.assertIsNotNone(ai_cat, "AI category should exist in exported OPML")
        self.assertEqual(ai_cat.parent_name, "Tech", "AI parent should be Tech")

    def test_round_trip_preserves_feed_category_assignment(self):
        """Feeds keep their category assignments through the round trip."""
        db = self._make_db()
        self._populate(db)

        xml = export_opml(db)
        Path(self.opml_path).write_text(xml, encoding="utf-8")

        categories, feeds, _ = parse_opml(Path(self.opml_path))

        # Build feed→category map from parsed OPML
        feed_cats = {f.url: f.category_name for f in feeds}
        self.assertEqual(feed_cats["https://example.com/tech.xml"], "Tech")
        self.assertEqual(feed_cats["https://example.com/ai.xml"], "AI")
        self.assertEqual(feed_cats["https://example.com/news.xml"], "News")
        self.assertIsNone(feed_cats["https://example.com/misc.xml"])

        # Full reimport and verify via DB
        self._wipe_data()
        db2 = self._make_db()
        cat_map = write_categories(categories, db2, dry_run=False)
        write_feeds(feeds, db2, cat_map, dry_run=False)

        reimported_feeds = db2.get_all_feeds()
        reimported_cats = db2.get_categories()
        cat_id_to_name = {c["category_id"]: c["name"] for c in reimported_cats}

        for feed in reimported_feeds:
            url = feed["url"]
            cat_name = cat_id_to_name.get(feed.get("category_id"))
            if url == "https://example.com/tech.xml":
                self.assertEqual(cat_name, "Tech")
            elif url == "https://example.com/ai.xml":
                self.assertEqual(cat_name, "AI")
            elif url == "https://example.com/news.xml":
                self.assertEqual(cat_name, "News")
            elif url == "https://example.com/misc.xml":
                # Uncategorised feeds have empty or missing category_id
                self.assertIn(cat_name, (None, ""))

    def test_round_trip_preserves_feed_titles_and_site_urls(self):
        """Feed titles and site URLs survive the round trip."""
        db = self._make_db()
        self._populate(db)

        xml = export_opml(db)
        Path(self.opml_path).write_text(xml, encoding="utf-8")

        _, feeds, _ = parse_opml(Path(self.opml_path))
        feed_map = {f.url: f for f in feeds}

        self.assertEqual(feed_map["https://example.com/tech.xml"].title, "Tech Daily")
        self.assertEqual(feed_map["https://example.com/tech.xml"].site_url, "https://example.com/tech")
        self.assertEqual(feed_map["https://example.com/ai.xml"].title, "AI Weekly")
        self.assertEqual(feed_map["https://example.com/misc.xml"].title, "Misc Blog")

    def test_export_empty_db(self):
        """Exporting an empty DB produces valid OPML with no outlines."""
        db = self._make_db()
        xml = export_opml(db)

        self.assertIn("<?xml", xml)
        self.assertIn("<opml", xml)
        self.assertIn("<body", xml)

        p = Path(self.opml_path).with_name("empty.opml")
        p.write_text(xml, encoding="utf-8")
        categories, feeds, title = parse_opml(p)
        self.assertEqual(len(feeds), 0)
        self.assertEqual(len(categories), 0)

    def test_idempotent_double_import(self):
        """Importing the same OPML twice doesn't create duplicates."""
        db = self._make_db()
        self._populate(db)

        xml = export_opml(db)
        Path(self.opml_path).write_text(xml, encoding="utf-8")

        self._wipe_data()
        db2 = self._make_db()
        categories, feeds, _ = parse_opml(Path(self.opml_path))

        # Import once
        cat_map = write_categories(categories, db2, dry_run=False)
        write_feeds(feeds, db2, cat_map, dry_run=False)

        # Import again (should skip all)
        cat_map2 = write_categories(categories, db2, dry_run=False)
        write_feeds(feeds, db2, cat_map2, dry_run=False)

        reimported = db2.get_all_feeds()
        self.assertEqual(len(reimported), 4, "Double import should not duplicate feeds")

        reimported_cats = db2.get_categories()
        self.assertEqual(len(reimported_cats), 3, "Double import should not duplicate categories")


if __name__ == "__main__":
    unittest.main()
