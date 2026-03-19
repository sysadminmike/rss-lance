"""
RSS/Atom feed parsing for RSS-Lance.
Uses feedparser - battle-tested, tolerant of malformed feeds.
"""

from __future__ import annotations

import hashlib
import logging
import re
import uuid
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
from time import mktime, struct_time
from typing import Generator

import feedparser
import requests

from content_cleaner import strip_dangerous_html, strip_tracking_params, strip_tracking_pixels
from db import SCHEMA_VERSION, _utcnow

log = logging.getLogger(__name__)

# ── social-link stripping ────────────────────────────────────────────────────

SOCIAL_DOMAINS = [
    'facebook.com', 'twitter.com', 'x.com', 'linkedin.com',
    'pinterest.com', 'reddit.com', 'tumblr.com', 'whatsapp.com',
    'wa.me', 't.me', 'threads.net', 'instagram.com', 'buffer.com',
    'getpocket.com', 'stumbleupon.com', 'digg.com', 'flipboard.com',
    'mix.com',
]

_SOCIAL_RE = re.compile(
    r'<a\b[^>]*\bhref=["\'][^"\']*(?:'
    + '|'.join(re.escape(d) for d in SOCIAL_DOMAINS)
    + r')[^"\']*["\'][^>]*>[\s\S]*?</a>',
    re.IGNORECASE,
)


def strip_social_links(html: str, report: list | None = None) -> str:
    """Remove <a> tags linking to social network sharing URLs.
    If *report* is a list, append detail strings for each link removed."""
    found = _SOCIAL_RE.findall(html)
    if found and report is not None:
        for match in found:
            report.append(f"social link: {match[:100]}")
        log.debug("Stripped %d social link(s)", len(found))
    return _SOCIAL_RE.sub('', html)


# ── helpers ──────────────────────────────────────────────────────────────────

def _to_utc(t: struct_time | None) -> datetime | None:
    if t is None:
        return None
    try:
        dt = datetime.fromtimestamp(mktime(t), tz=timezone.utc)
        return dt.replace(tzinfo=None)
    except (OSError, OverflowError, ValueError):
        return None


def _guid(entry: feedparser.FeedParserDict, feed_id: str = "") -> str:
    """Derive a stable GUID for an entry."""
    if entry.get("id"):
        return entry["id"]
    if entry.get("link"):
        return entry["link"]
    # Fallback: hash title + published + feed_id to avoid collisions
    raw = (feed_id + entry.get("title", "") + entry.get("published", "")).encode()
    return hashlib.sha1(raw).hexdigest()


def _text(entry: feedparser.FeedParserDict, *keys: str) -> str:
    for k in keys:
        val = entry.get(k)
        if val:
            if hasattr(val, "value"):
                return str(val.value)
            return str(val)
    return ""


# ── public API ───────────────────────────────────────────────────────────────

class ParsedFeed:
    """Result of fetching and parsing one feed URL."""

    __slots__ = ("title", "site_url", "icon_url", "articles", "error", "http_status",
                 "sanitize_report")

    def __init__(self) -> None:
        self.title: str = ""
        self.site_url: str = ""
        self.icon_url: str = ""
        self.articles: list[dict] = []
        self.error: str = ""
        self.http_status: int = 0
        self.sanitize_report: list[str] = []


def fetch_feed(url: str, feed_id: str, user_agent: str = "RSS-Lance/1.0",
               timeout: int = 20) -> ParsedFeed:
    """Fetch and parse a feed URL. Returns a ParsedFeed regardless of errors."""
    result = ParsedFeed()
    try:
        resp = requests.get(
            url,
            headers={"User-Agent": user_agent},
            timeout=timeout,
            allow_redirects=True,
        )
        result.http_status = resp.status_code
        if resp.status_code == 410:
            result.error = "HTTP 410 Gone - feed has been permanently removed"
            return result
        if not resp.ok:
            result.error = f"HTTP {resp.status_code}"
            return result

        parsed = feedparser.parse(resp.content, response_headers=resp.headers)

        if parsed.bozo and not parsed.entries:
            result.error = f"Feed parse error: {parsed.bozo_exception}"
            return result

        result.title    = parsed.feed.get("title", "")
        result.site_url = parsed.feed.get("link", "")
        result.icon_url = _favicon(parsed)

        now = _utcnow()
        for entry in parsed.entries:
            art_report: list[str] = []
            result.articles.append({
                "article_id":   str(uuid.uuid4()),
                "feed_id":      feed_id,
                "title":        _text(entry, "title"),
                "url":          entry.get("link", ""),
                "author":       _text(entry, "author"),
                "content":      _content(entry, report=art_report),
                "summary":      _sanitise(_text(entry, "summary")),
                "published_at": _to_utc(entry.get("published_parsed"))
                                 or _to_utc(entry.get("updated_parsed"))
                                 or now,
                "fetched_at":   now,
                "is_read":      False,
                "is_starred":   False,
                "guid":         _guid(entry, feed_id),
                "schema_version": SCHEMA_VERSION,
                "created_at":   now,
                "updated_at":   now,
            })
            if art_report:
                title = _text(entry, "title") or "(untitled)"
                result.sanitize_report.append(
                    f"{title}: {'; '.join(art_report)}"
                )

    except requests.exceptions.Timeout:
        result.error = "Request timed out"
    except requests.exceptions.ConnectionError as exc:
        result.error = f"Connection error: {exc}"
    except Exception as exc:
        log.exception("Unexpected error fetching %s", url)
        result.error = str(exc)

    return result


def _sanitise(html: str, report: list | None = None) -> str:
    """Run the full server-side sanitisation pipeline on article HTML.
    If *report* is a list, append detail strings for each item removed."""
    html = strip_dangerous_html(html, report=report)
    html = strip_social_links(html, report=report)
    html = strip_tracking_pixels(html, report=report)
    html = strip_tracking_params(html, report=report)
    return html


def _content(entry: feedparser.FeedParserDict, report: list | None = None) -> str:
    """Return the richest available content string for an entry."""
    content_list = entry.get("content")
    if content_list:
        # Prefer text/html
        for c in content_list:
            if c.get("type") == "text/html":
                return _sanitise(c.get("value", ""), report=report)
        return _sanitise(content_list[0].get("value", ""), report=report)
    return _sanitise(_text(entry, "summary_detail", "summary"), report=report)


def _favicon(parsed: feedparser.FeedParserDict) -> str:
    """Best-effort favicon extraction."""
    icon = parsed.feed.get("icon") or parsed.feed.get("logo")
    if icon:
        return icon
    link = parsed.feed.get("link", "")
    if link:
        from urllib.parse import urlparse
        parts = urlparse(link)
        return f"{parts.scheme}://{parts.netloc}/favicon.ico"
    return ""
