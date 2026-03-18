"""
Tests for the DB class.
Uses a temporary LanceDB directory to test real read/write operations.
"""

import os
import shutil
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from unittest.mock import MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "fetcher"))

from db import DB, FEEDS_SCHEMA, ARTICLES_SCHEMA, CATEGORIES_SCHEMA, PENDING_FEEDS_SCHEMA


class _FakeConfig:
    """Minimal Config replacement for DB tests."""

    def __init__(self, path):
        self.storage_path = path
        self.compaction_thresholds = {
            "articles": 20,
            "feeds": 50,
            "categories": 50,
            "pending_feeds": 10,
        }


class TestDBInit(unittest.TestCase):
    """Test DB initialization creates tables."""

    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        self.db = DB(_FakeConfig(self._tmpdir))

    def tearDown(self):
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def test_tables_created(self):
        """All four tables should exist after init."""
        self.assertIsNotNone(self.db.feeds)
        self.assertIsNotNone(self.db.articles)
        self.assertIsNotNone(self.db.categories)
        self.assertIsNotNone(self.db.pending_feeds)


class TestDBFeeds(unittest.TestCase):
    """Tests for feed CRUD operations."""

    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        self.db = DB(_FakeConfig(self._tmpdir))

    def tearDown(self):
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def test_add_feed(self):
        feed_id = self.db.add_feed(
            url="https://example.com/rss",
            title="Example Feed",
            site_url="https://example.com",
        )
        self.assertIsNotNone(feed_id)
        feeds = self.db.get_all_feeds()
        self.assertEqual(len(feeds), 1)
        self.assertEqual(feeds[0]["url"], "https://example.com/rss")
        self.assertEqual(feeds[0]["title"], "Example Feed")

    def test_add_multiple_feeds(self):
        self.db.add_feed(url="https://a.com/rss", title="A")
        self.db.add_feed(url="https://b.com/rss", title="B")
        feeds = self.db.get_all_feeds()
        self.assertEqual(len(feeds), 2)

    def test_get_feeds_due_all_new(self):
        """Feeds with no last_fetched should be due."""
        self.db.add_feed(url="https://due.com/rss", title="Due")
        due = self.db.get_feeds_due()
        self.assertEqual(len(due), 1)

    def test_get_feeds_due_skips_dead(self):
        """Dead feeds should not be returned."""
        feed_id = self.db.add_feed(url="https://dead.com/rss", title="Dead")
        self.db.feeds.update(f"feed_id = '{feed_id}'", {"fetch_tier": "dead"})
        due = self.db.get_feeds_due()
        self.assertEqual(len(due), 0)


class TestDBArticles(unittest.TestCase):
    """Tests for article operations."""

    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        self.db = DB(_FakeConfig(self._tmpdir))
        self._feed_id = self.db.add_feed(url="https://test.com/rss", title="Test")

    def tearDown(self):
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def _make_article(self, guid="guid-1"):
        now = datetime.now(timezone.utc)
        return {
            "article_id": f"art-{guid}",
            "feed_id": self._feed_id,
            "title": f"Article {guid}",
            "url": f"https://test.com/{guid}",
            "author": "Author",
            "content": "<p>Content</p>",
            "summary": "Summary",
            "published_at": now,
            "fetched_at": now,
            "is_read": False,
            "is_starred": False,
            "guid": guid,
        }

    def test_add_articles(self):
        count = self.db.add_articles([self._make_article("g1")])
        self.assertEqual(count, 1)

    def test_add_empty_list(self):
        count = self.db.add_articles([])
        self.assertEqual(count, 0)

    def test_get_existing_guids(self):
        self.db.add_articles([self._make_article("g1"), self._make_article("g2")])
        guids = self.db.get_existing_guids(self._feed_id)
        self.assertIn("g1", guids)
        self.assertIn("g2", guids)

    def test_get_existing_guids_empty(self):
        guids = self.db.get_existing_guids(self._feed_id)
        self.assertEqual(guids, set())

    def test_mark_article_read(self):
        self.db.add_articles([self._make_article("g1")])
        self.db.mark_article_read("art-g1")
        df = self.db.articles.to_pandas()
        row = df[df["article_id"] == "art-g1"].iloc[0]
        self.assertTrue(row["is_read"])

    def test_mark_article_starred(self):
        self.db.add_articles([self._make_article("g1")])
        self.db.mark_article_starred("art-g1", starred=True)
        df = self.db.articles.to_pandas()
        row = df[df["article_id"] == "art-g1"].iloc[0]
        self.assertTrue(row["is_starred"])

    def test_unstar_article(self):
        self.db.add_articles([self._make_article("g1")])
        self.db.mark_article_starred("art-g1", starred=True)
        self.db.mark_article_starred("art-g1", starred=False)
        df = self.db.articles.to_pandas()
        row = df[df["article_id"] == "art-g1"].iloc[0]
        self.assertFalse(row["is_starred"])


class TestDBBatch(unittest.TestCase):
    """Tests for batch write operations."""

    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        self.db = DB(_FakeConfig(self._tmpdir))
        self._feed_id = self.db.add_feed(url="https://batch.com/rss", title="Batch")

    def tearDown(self):
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def _make_article(self, guid):
        now = datetime.now(timezone.utc)
        return {
            "article_id": f"art-{guid}",
            "feed_id": self._feed_id,
            "title": f"Article {guid}",
            "url": f"https://batch.com/{guid}",
            "author": "Author",
            "content": "<p>Content</p>",
            "summary": "Summary",
            "published_at": now,
            "fetched_at": now,
            "is_read": False,
            "is_starred": False,
            "guid": guid,
        }

    def test_batch_articles(self):
        self.db.begin_batch()
        self.db.add_articles([self._make_article("b1"), self._make_article("b2")])
        # Not yet written
        df = self.db.articles.to_pandas()
        pre_count = len(df)

        written = self.db.flush_batch()
        self.assertEqual(written, 2)

        df = self.db.articles.to_pandas()
        self.assertEqual(len(df), pre_count + 2)

    def test_batch_feed_updates(self):
        self.db.begin_batch()
        self.db.update_feed_after_fetch(self._feed_id, success=True)
        # No exception; verify flush completes
        written = self.db.flush_batch()
        self.assertEqual(written, 0)  # No articles in this batch


class TestDBCategories(unittest.TestCase):
    """Tests for category operations."""

    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        self.db = DB(_FakeConfig(self._tmpdir))

    def tearDown(self):
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def test_add_category(self):
        cat_id = self.db.add_category("Tech")
        self.assertIsNotNone(cat_id)
        cats = self.db.get_categories()
        self.assertEqual(len(cats), 1)
        self.assertEqual(cats[0]["name"], "Tech")

    def test_add_subcategory(self):
        parent_id = self.db.add_category("Tech")
        child_id = self.db.add_category("AI", parent_id=parent_id)
        cats = self.db.get_categories()
        self.assertEqual(len(cats), 2)
        child = [c for c in cats if c["category_id"] == child_id][0]
        self.assertEqual(child["parent_id"], parent_id)


class TestSchemas(unittest.TestCase):
    """Verify Arrow schemas are well-formed."""

    def test_feeds_schema_fields(self):
        names = [f.name for f in FEEDS_SCHEMA]
        self.assertIn("feed_id", names)
        self.assertIn("url", names)
        self.assertIn("title", names)
        self.assertIn("fetch_tier", names)
        self.assertIn("error_count", names)
        self.assertIn("created_at", names)
        self.assertIn("updated_at", names)

    def test_articles_schema_fields(self):
        names = [f.name for f in ARTICLES_SCHEMA]
        self.assertIn("article_id", names)
        self.assertIn("feed_id", names)
        self.assertIn("is_read", names)
        self.assertIn("is_starred", names)
        self.assertIn("guid", names)
        self.assertIn("created_at", names)
        self.assertIn("updated_at", names)

    def test_categories_schema_fields(self):
        names = [f.name for f in CATEGORIES_SCHEMA]
        self.assertIn("category_id", names)
        self.assertIn("name", names)
        self.assertIn("parent_id", names)
        self.assertIn("created_at", names)
        self.assertIn("updated_at", names)

    def test_pending_feeds_schema_fields(self):
        names = [f.name for f in PENDING_FEEDS_SCHEMA]
        self.assertIn("url", names)
        self.assertIn("category_id", names)
        self.assertIn("created_at", names)
        self.assertIn("updated_at", names)


if __name__ == "__main__":
    unittest.main()
