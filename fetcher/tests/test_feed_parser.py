"""
Tests for the feed_parser module.
Uses unittest.mock to avoid real HTTP requests.
"""

import unittest
from datetime import datetime, timezone
from unittest.mock import MagicMock, patch

import sys
import os

# Allow imports from fetcher/
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from feed_parser import (
    ParsedFeed,
    _content,
    _favicon,
    _guid,
    _text,
    _to_utc,
    fetch_feed,
    strip_social_links,
)


class TestToUtc(unittest.TestCase):
    """Tests for _to_utc helper."""

    def test_none_returns_none(self):
        self.assertIsNone(_to_utc(None))

    def test_valid_struct_time(self):
        import time
        t = time.strptime("2024-01-15 12:30:00", "%Y-%m-%d %H:%M:%S")
        result = _to_utc(t)
        self.assertIsNotNone(result)
        self.assertIsNone(result.tzinfo)

    def test_overflow_returns_none(self):
        """Extreme dates should return None, not crash."""
        import time
        # struct_time with year 0 or very large numbers
        t = time.struct_time((0, 1, 1, 0, 0, 0, 0, 1, -1))
        result = _to_utc(t)
        # Should return None or a datetime, but not raise
        # (behaviour depends on platform)


class TestGuid(unittest.TestCase):
    """Tests for _guid helper."""

    def test_id_field(self):
        entry = {"id": "unique-123"}
        self.assertEqual(_guid(entry), "unique-123")

    def test_link_fallback(self):
        entry = {"link": "https://example.com/article"}
        self.assertEqual(_guid(entry), "https://example.com/article")

    def test_hash_fallback(self):
        entry = {"title": "Hello", "published": "2024-01-01"}
        guid = _guid(entry)
        self.assertEqual(len(guid), 40)  # SHA1 hex digest

    def test_empty_entry(self):
        entry = {}
        guid = _guid(entry)
        self.assertEqual(len(guid), 40)


class TestText(unittest.TestCase):
    """Tests for _text helper."""

    def test_simple_string(self):
        entry = {"title": "Hello World"}
        self.assertEqual(_text(entry, "title"), "Hello World")

    def test_object_with_value(self):
        obj = MagicMock()
        obj.value = "Rich content"
        entry = {"summary": obj}
        self.assertEqual(_text(entry, "summary"), "Rich content")

    def test_missing_key(self):
        entry = {}
        self.assertEqual(_text(entry, "title", "summary"), "")

    def test_multiple_keys_first_match(self):
        entry = {"summary": "Sum"}
        self.assertEqual(_text(entry, "title", "summary"), "Sum")


class TestContent(unittest.TestCase):
    """Tests for _content helper."""

    def test_html_content(self):
        entry = {
            "content": [
                {"type": "text/html", "value": "<p>HTML</p>"},
                {"type": "text/plain", "value": "plain"},
            ]
        }
        self.assertEqual(_content(entry), "<p>HTML</p>")

    def test_single_content(self):
        entry = {"content": [{"value": "Only one"}]}
        self.assertEqual(_content(entry), "Only one")

    def test_fallback_to_summary(self):
        entry = {"summary": "Just a summary"}
        self.assertEqual(_content(entry), "Just a summary")

    def test_empty_entry(self):
        entry = {}
        self.assertEqual(_content(entry), "")

    def test_strips_social_links_from_html_content(self):
        entry = {
            "content": [
                {"type": "text/html", "value": '<p>Article</p><a href="https://facebook.com/sharer">Share</a>'},
            ]
        }
        self.assertEqual(_content(entry), "<p>Article</p>")

    def test_strips_social_links_from_summary_fallback(self):
        entry = {"summary": '<p>Text</p><a href="https://twitter.com/intent/tweet?url=x">Tweet</a>'}
        self.assertEqual(_content(entry), "<p>Text</p>")


class TestStripSocialLinks(unittest.TestCase):
    """Tests for strip_social_links."""

    def test_removes_facebook_share(self):
        html = '<p>Read</p><a href="https://www.facebook.com/sharer.php?u=https%3A%2F%2Fexample.com"><svg viewBox="0 0 24 24"><path d="M9 8"></path></svg></a>'
        self.assertEqual(strip_social_links(html), "<p>Read</p>")

    def test_removes_twitter_link(self):
        html = '<a href="https://twitter.com/intent/tweet?url=foo">Tweet</a><p>Content</p>'
        self.assertEqual(strip_social_links(html), "<p>Content</p>")

    def test_removes_x_dot_com_link(self):
        html = '<a href="https://x.com/share?text=hi">Post</a><p>Content</p>'
        self.assertEqual(strip_social_links(html), "<p>Content</p>")

    def test_removes_linkedin_share(self):
        html = '<a href="https://www.linkedin.com/shareArticle?url=foo">Share</a>'
        self.assertEqual(strip_social_links(html), "")

    def test_removes_multiple_social_links(self):
        html = '<p>Text</p><a href="https://facebook.com/sharer">FB</a><a href="https://pinterest.com/pin/create">Pin</a><a href="https://reddit.com/submit">Reddit</a>'
        self.assertEqual(strip_social_links(html), "<p>Text</p>")

    def test_preserves_non_social_links(self):
        html = '<p>Visit <a href="https://example.com">our site</a></p>'
        self.assertEqual(strip_social_links(html), html)

    def test_empty_string(self):
        self.assertEqual(strip_social_links(""), "")

    def test_no_links(self):
        html = "<p>No links here</p>"
        self.assertEqual(strip_social_links(html), html)


class TestFavicon(unittest.TestCase):
    """Tests for _favicon helper."""

    def test_icon_field(self):
        parsed = MagicMock()
        parsed.feed = {"icon": "https://example.com/icon.png"}
        self.assertEqual(_favicon(parsed), "https://example.com/icon.png")

    def test_logo_field(self):
        parsed = MagicMock()
        parsed.feed = {"logo": "https://example.com/logo.png"}
        self.assertEqual(_favicon(parsed), "https://example.com/logo.png")

    def test_generated_from_link(self):
        parsed = MagicMock()
        parsed.feed = {"link": "https://example.com/blog"}
        self.assertEqual(_favicon(parsed), "https://example.com/favicon.ico")

    def test_no_link(self):
        parsed = MagicMock()
        parsed.feed = {}
        self.assertEqual(_favicon(parsed), "")


class TestFetchFeed(unittest.TestCase):
    """Tests for fetch_feed using mocked HTTP."""

    @patch("feed_parser.requests.get")
    def test_successful_fetch(self, mock_get):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        mock_resp.ok = True
        mock_resp.headers = {}
        mock_resp.content = b"""<?xml version="1.0"?>
        <rss version="2.0">
          <channel>
            <title>Test Feed</title>
            <link>https://example.com</link>
            <item>
              <title>Article 1</title>
              <link>https://example.com/1</link>
              <guid>guid-1</guid>
              <description>Summary of article 1</description>
            </item>
          </channel>
        </rss>"""
        mock_get.return_value = mock_resp

        result = fetch_feed("https://example.com/feed.xml", "feed-123")
        self.assertEqual(result.title, "Test Feed")
        self.assertEqual(result.error, "")
        self.assertEqual(len(result.articles), 1)
        self.assertEqual(result.articles[0]["feed_id"], "feed-123")
        self.assertEqual(result.articles[0]["title"], "Article 1")
        self.assertFalse(result.articles[0]["is_read"])
        self.assertFalse(result.articles[0]["is_starred"])

    @patch("feed_parser.requests.get")
    def test_http_410_gone(self, mock_get):
        mock_resp = MagicMock()
        mock_resp.status_code = 410
        mock_resp.ok = False
        mock_get.return_value = mock_resp

        result = fetch_feed("https://example.com/feed.xml", "feed-123")
        self.assertEqual(result.http_status, 410)
        self.assertIn("410 Gone", result.error)
        self.assertEqual(len(result.articles), 0)

    @patch("feed_parser.requests.get")
    def test_http_500_error(self, mock_get):
        mock_resp = MagicMock()
        mock_resp.status_code = 500
        mock_resp.ok = False
        mock_get.return_value = mock_resp

        result = fetch_feed("https://example.com/feed.xml", "feed-123")
        self.assertEqual(result.http_status, 500)
        self.assertIn("500", result.error)

    @patch("feed_parser.requests.get")
    def test_timeout(self, mock_get):
        import requests as req
        mock_get.side_effect = req.exceptions.Timeout()

        result = fetch_feed("https://example.com/feed.xml", "feed-123")
        self.assertIn("timed out", result.error)

    @patch("feed_parser.requests.get")
    def test_connection_error(self, mock_get):
        import requests as req
        mock_get.side_effect = req.exceptions.ConnectionError("DNS failed")

        result = fetch_feed("https://example.com/feed.xml", "feed-123")
        self.assertIn("Connection error", result.error)

    @patch("feed_parser.requests.get")
    def test_atom_feed(self, mock_get):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        mock_resp.ok = True
        mock_resp.headers = {}
        mock_resp.content = b"""<?xml version="1.0" encoding="utf-8"?>
        <feed xmlns="http://www.w3.org/2005/Atom">
          <title>Atom Feed</title>
          <link href="https://atom.example.com"/>
          <entry>
            <title>Atom Entry 1</title>
            <link href="https://atom.example.com/1"/>
            <id>atom-guid-1</id>
            <summary>Atom summary</summary>
          </entry>
        </feed>"""
        mock_get.return_value = mock_resp

        result = fetch_feed("https://atom.example.com/feed", "feed-456")
        self.assertEqual(result.title, "Atom Feed")
        self.assertEqual(len(result.articles), 1)
        self.assertEqual(result.articles[0]["title"], "Atom Entry 1")

    @patch("feed_parser.requests.get")
    def test_malformed_feed_no_entries(self, mock_get):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        mock_resp.ok = True
        mock_resp.headers = {}
        mock_resp.content = b"<html><body>Not a feed</body></html>"
        mock_get.return_value = mock_resp

        result = fetch_feed("https://example.com/notfeed", "feed-789")
        # feedparser may set bozo=True with no entries, or parse 0 entries
        # Either way, articles should be empty
        self.assertEqual(len(result.articles), 0)


class TestParsedFeed(unittest.TestCase):
    """Tests for ParsedFeed data class."""

    def test_defaults(self):
        pf = ParsedFeed()
        self.assertEqual(pf.title, "")
        self.assertEqual(pf.site_url, "")
        self.assertEqual(pf.icon_url, "")
        self.assertEqual(pf.articles, [])
        self.assertEqual(pf.error, "")
        self.assertEqual(pf.http_status, 0)


if __name__ == "__main__":
    unittest.main()
