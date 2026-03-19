"""
Tests for the lance_writer.py sidecar script.

Uses a temporary LanceDB directory to test real read/write operations via the
same JSON-line protocol the Go server will use.  Each test calls the writer's
dispatch method directly (in-process) rather than spawning a subprocess, so
tests are fast and deterministic.
"""

import json
import os
import shutil
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from io import StringIO
from unittest.mock import patch

# Add tools/ to path so we can import lance_writer
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "tools"))
# Add fetcher/ to path for schema definitions
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "fetcher"))

import lancedb
import pyarrow as pa

from db import (
    ARTICLES_SCHEMA,
    CATEGORIES_SCHEMA,
    FEEDS_SCHEMA,
    LOG_SCHEMA,
    PENDING_FEEDS_SCHEMA,
    SETTINGS_SCHEMA,
)
from lance_writer import LanceWriter, _escape


# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #

def _utcnow():
    return datetime.now(timezone.utc).replace(tzinfo=None)


def _capture_response(writer, cmd_dict):
    """Dispatch a command and capture the JSON response."""
    buf = StringIO()
    with patch("sys.stdout", buf):
        writer.dispatch(json.dumps(cmd_dict))
    return json.loads(buf.getvalue().strip())


def _seed_settings(db, settings):
    """Insert settings rows using the fetcher's schema."""
    now = _utcnow()
    tbl = db.open_table("settings")
    rows = [{"key": k, "value": v, "created_at": now, "updated_at": now}
            for k, v in settings.items()]
    tbl.add(rows)


def _seed_articles(db, articles):
    """Insert article rows with required columns."""
    now = _utcnow()
    tbl = db.open_table("articles")
    rows = []
    for a in articles:
        row = {
            "article_id": a["article_id"],
            "feed_id": a.get("feed_id", "feed-1"),
            "title": a.get("title", "Test"),
            "url": a.get("url", "http://example.com"),
            "author": a.get("author", ""),
            "content": a.get("content", ""),
            "summary": a.get("summary", ""),
            "published_at": a.get("published_at", now),
            "fetched_at": a.get("fetched_at", now),
            "is_read": a.get("is_read", False),
            "is_starred": a.get("is_starred", False),
            "guid": a.get("guid", a["article_id"]),
            "schema_version": a.get("schema_version", 3),
            "created_at": now,
            "updated_at": now,
        }
        rows.append(row)
    tbl.add(rows)


# --------------------------------------------------------------------------- #
# Base test class
# --------------------------------------------------------------------------- #

class LanceWriterTestBase(unittest.TestCase):
    """Base class that creates a temp LanceDB with all tables."""

    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        # Create all tables using the fetcher schemas
        conn = lancedb.connect(self._tmpdir)
        for name, schema in [
            ("feeds", FEEDS_SCHEMA),
            ("articles", ARTICLES_SCHEMA),
            ("categories", CATEGORIES_SCHEMA),
            ("pending_feeds", PENDING_FEEDS_SCHEMA),
            ("settings", SETTINGS_SCHEMA),
            ("log_api", LOG_SCHEMA),
            ("log_fetcher", LOG_SCHEMA),
        ]:
            conn.create_table(name, schema=schema)
        self._db = conn
        self.writer = LanceWriter(self._tmpdir)

    def tearDown(self):
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def cmd(self, cmd_dict):
        """Shorthand: dispatch and return response dict."""
        return _capture_response(self.writer, cmd_dict)


# --------------------------------------------------------------------------- #
# Tests: info
# --------------------------------------------------------------------------- #

class TestInfo(LanceWriterTestBase):

    def test_info_returns_versions(self):
        resp = self.cmd({"cmd": "info"})
        self.assertTrue(resp["ok"])
        info = resp["info"]
        self.assertIn("pid", info)
        self.assertIn("lancedb_version", info)
        self.assertIn("pyarrow_version", info)
        self.assertIn("uptime_seconds", info)
        self.assertEqual(info["data_path"], self._tmpdir)
        self.assertIsInstance(info["pid"], int)
        self.assertGreater(info["pid"], 0)


# --------------------------------------------------------------------------- #
# Tests: settings
# --------------------------------------------------------------------------- #

class TestSettings(LanceWriterTestBase):

    def test_put_setting(self):
        _seed_settings(self._db, {"theme": "dark"})
        resp = self.cmd({"cmd": "put_setting", "key": "theme", "value": "light"})
        self.assertTrue(resp["ok"])

        # Verify
        rows = self._db.open_table("settings").to_pandas()
        row = rows[rows["key"] == "theme"].iloc[0]
        self.assertEqual(row["value"], "light")

    def test_put_settings_batch(self):
        _seed_settings(self._db, {"a": "old", "b": "old", "c": "other"})
        resp = self.cmd({
            "cmd": "put_settings_batch",
            "settings": {"a": "new", "b": "new", "c": "changed"},
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("settings").to_pandas()
        self.assertEqual(rows[rows["key"] == "a"].iloc[0]["value"], "new")
        self.assertEqual(rows[rows["key"] == "b"].iloc[0]["value"], "new")
        self.assertEqual(rows[rows["key"] == "c"].iloc[0]["value"], "changed")

    def test_insert_setting(self):
        resp = self.cmd({"cmd": "insert_setting", "key": "new_key", "value": "val"})
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("settings").to_pandas()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows.iloc[0]["key"], "new_key")

    def test_insert_settings_batch(self):
        resp = self.cmd({
            "cmd": "insert_settings",
            "settings": {"k1": "v1", "k2": "v2", "k3": "v3"},
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("settings").to_pandas()
        self.assertEqual(len(rows), 3)


# --------------------------------------------------------------------------- #
# Tests: articles
# --------------------------------------------------------------------------- #

class TestArticles(LanceWriterTestBase):

    def setUp(self):
        super().setUp()
        _seed_articles(self._db, [
            {"article_id": "art-1", "feed_id": "feed-1", "is_read": False, "is_starred": False},
            {"article_id": "art-2", "feed_id": "feed-1", "is_read": False, "is_starred": False},
            {"article_id": "art-3", "feed_id": "feed-2", "is_read": False, "is_starred": True},
        ])

    def test_set_article_read(self):
        resp = self.cmd({"cmd": "set_article_read", "article_id": "art-1", "is_read": True})
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("articles").to_pandas()
        art1 = rows[rows["article_id"] == "art-1"].iloc[0]
        self.assertTrue(art1["is_read"])

    def test_set_article_starred(self):
        resp = self.cmd({"cmd": "set_article_starred", "article_id": "art-2", "is_starred": True})
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("articles").to_pandas()
        art2 = rows[rows["article_id"] == "art-2"].iloc[0]
        self.assertTrue(art2["is_starred"])

    def test_mark_all_read(self):
        resp = self.cmd({"cmd": "mark_all_read", "feed_id": "feed-1"})
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("articles").to_pandas()
        feed1 = rows[rows["feed_id"] == "feed-1"]
        self.assertTrue(all(feed1["is_read"]))
        # feed-2 articles should be unchanged
        feed2 = rows[rows["feed_id"] == "feed-2"]
        self.assertFalse(all(feed2["is_read"]))

    def test_update_article(self):
        resp = self.cmd({
            "cmd": "update_article",
            "article_id": "art-1",
            "updates": {"title": "New Title"},
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("articles").to_pandas()
        art1 = rows[rows["article_id"] == "art-1"].iloc[0]
        self.assertEqual(art1["title"], "New Title")

    def test_flush_overrides_mixed(self):
        resp = self.cmd({
            "cmd": "flush_overrides",
            "overrides": {
                "art-1": {"is_read": True},
                "art-2": {"is_read": True, "is_starred": True},
                "art-3": {"is_starred": False},
            },
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("articles").to_pandas()
        art1 = rows[rows["article_id"] == "art-1"].iloc[0]
        art2 = rows[rows["article_id"] == "art-2"].iloc[0]
        art3 = rows[rows["article_id"] == "art-3"].iloc[0]

        self.assertTrue(art1["is_read"])
        self.assertTrue(art2["is_read"])
        self.assertTrue(art2["is_starred"])
        self.assertFalse(art3["is_starred"])

    def test_flush_overrides_empty(self):
        resp = self.cmd({"cmd": "flush_overrides", "overrides": {}})
        self.assertTrue(resp["ok"])


# --------------------------------------------------------------------------- #
# Tests: pending feeds
# --------------------------------------------------------------------------- #

class TestPendingFeeds(LanceWriterTestBase):

    def test_insert_and_delete_pending_feed(self):
        resp = self.cmd({
            "cmd": "insert_pending_feed",
            "url": "https://example.com/feed.xml",
            "category_id": "cat-1",
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("pending_feeds").to_pandas()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows.iloc[0]["url"], "https://example.com/feed.xml")

        # Delete it
        resp = self.cmd({
            "cmd": "delete_pending_feed",
            "url": "https://example.com/feed.xml",
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("pending_feeds").to_pandas()
        self.assertEqual(len(rows), 0)


# --------------------------------------------------------------------------- #
# Tests: logs
# --------------------------------------------------------------------------- #

class TestLogs(LanceWriterTestBase):

    def test_insert_logs(self):
        resp = self.cmd({
            "cmd": "insert_logs",
            "entries": [
                {"log_id": "log-1", "level": "info", "category": "test",
                 "message": "hello", "details": "{}"},
                {"log_id": "log-2", "level": "error", "category": "test",
                 "message": "fail", "details": '{"code": 500}'},
            ],
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("log_api").to_pandas()
        self.assertEqual(len(rows), 2)

    def test_insert_logs_empty(self):
        resp = self.cmd({"cmd": "insert_logs", "entries": []})
        self.assertTrue(resp["ok"])

    def test_delete_old_logs(self):
        # Insert some logs first
        self.cmd({
            "cmd": "insert_logs",
            "entries": [
                {"log_id": "log-1", "level": "info", "category": "test",
                 "message": "old", "details": "",
                 "timestamp": "2020-01-01T00:00:00Z"},
                {"log_id": "log-2", "level": "info", "category": "test",
                 "message": "new", "details": "",
                 "timestamp": "2030-01-01T00:00:00Z"},
            ],
        })

        resp = self.cmd({
            "cmd": "delete_old_logs",
            "filter": "timestamp < timestamp '2025-01-01'",
        })
        self.assertTrue(resp["ok"])

        rows = self._db.open_table("log_api").to_pandas()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows.iloc[0]["log_id"], "log-2")


# --------------------------------------------------------------------------- #
# Tests: metadata
# --------------------------------------------------------------------------- #

class TestMetadata(LanceWriterTestBase):

    def test_table_exists_true(self):
        resp = self.cmd({"cmd": "table_exists", "table": "settings"})
        self.assertTrue(resp["ok"])
        self.assertTrue(resp["exists"])

    def test_table_exists_false(self):
        resp = self.cmd({"cmd": "table_exists", "table": "nonexistent"})
        self.assertTrue(resp["ok"])
        self.assertFalse(resp["exists"])

    def test_table_meta(self):
        resp = self.cmd({"cmd": "table_meta", "table": "settings"})
        self.assertTrue(resp["ok"])
        self.assertIsInstance(resp["columns"], list)
        self.assertGreater(len(resp["columns"]), 0)
        col_names = [c["name"] for c in resp["columns"]]
        self.assertIn("key", col_names)
        self.assertIn("value", col_names)


# --------------------------------------------------------------------------- #
# Tests: error handling
# --------------------------------------------------------------------------- #

class TestErrorHandling(LanceWriterTestBase):

    def test_unknown_command(self):
        resp = self.cmd({"cmd": "nonexistent"})
        self.assertFalse(resp["ok"])
        self.assertIn("unknown command", resp["error"])

    def test_invalid_json(self):
        buf = StringIO()
        with patch("sys.stdout", buf):
            self.writer.dispatch("not json at all")
        resp = json.loads(buf.getvalue().strip())
        self.assertFalse(resp["ok"])
        self.assertIn("invalid JSON", resp["error"])

    def test_missing_required_field(self):
        resp = self.cmd({"cmd": "put_setting"})  # missing key and value
        self.assertFalse(resp["ok"])
        self.assertIn("error", resp)


# --------------------------------------------------------------------------- #
# Tests: escape function
# --------------------------------------------------------------------------- #

class TestEscape(unittest.TestCase):

    def test_hex_ids_pass_through(self):
        self.assertEqual(_escape("abc123"), "abc123")
        self.assertEqual(_escape("550e8400-e29b-41d4-a716-446655440000"),
                         "550e8400-e29b-41d4-a716-446655440000")

    def test_single_quotes_escaped(self):
        self.assertEqual(_escape("it's a test"), "it''s a test")

    def test_url_with_quotes(self):
        self.assertEqual(_escape("http://x.com/a?b='c'"), "http://x.com/a?b=''c''")


if __name__ == "__main__":
    unittest.main()
