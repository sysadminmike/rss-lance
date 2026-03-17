"""
Feed-level content cleaning for RSS-Lance.

Compares articles from the same feed to detect repeated HTML blocks
(site navigation, related-post cards, footers, etc.) and strips them.
Only the most recent articles are compared so that site redesigns
don't produce stale patterns.

Also strips tracking pixels and other tiny hidden images used for
analytics, open-rate tracking, or user fingerprinting.
"""

from __future__ import annotations

import hashlib
import logging
import re
from collections import Counter
from urllib.parse import urlparse, parse_qs, urlencode, urlunparse

from bs4 import BeautifulSoup, NavigableString, Tag

log = logging.getLogger(__name__)

# How many articles to sample for template detection
SAMPLE_SIZE = 5
# A block must appear in at least this many sample articles to count as chrome
MIN_OCCURRENCES = 2
# Ignore blocks smaller than this (bytes of outer HTML) — too small to matter
MIN_BLOCK_SIZE = 80


def _normalise_block(tag: Tag) -> str:
    """Produce a normalised string for a block element, ignoring trivial
    whitespace differences but keeping structure + attributes intact."""
    # Render to string, collapse whitespace runs
    html = str(tag)
    html = re.sub(r'\s+', ' ', html).strip()
    return html


def _block_hash(html: str) -> str:
    return hashlib.sha256(html.encode("utf-8", errors="replace")).hexdigest()


def _find_chrome_hashes(articles: list[dict], field: str = "content") -> tuple[set[str], dict[str, str]]:
    """Given recent articles from ONE feed, return hashes of HTML blocks
    that are duplicated across articles (i.e. site chrome)."""

    htmls = [a[field] for a in articles if a.get(field)]
    if len(htmls) < 2:
        return set(), {}

    # Sample the most recent ones (caller should already sort, but cap here)
    sample = htmls[:SAMPLE_SIZE]

    # For each article, collect hashes of its top-level block elements
    per_article: list[set[str]] = []
    hash_to_html: dict[str, str] = {}

    for html in sample:
        soup = BeautifulSoup(html, "html.parser")
        block_hashes: set[str] = set()

        for child in soup.children:
            if isinstance(child, NavigableString):
                continue
            if not isinstance(child, Tag):
                continue
            norm = _normalise_block(child)
            if len(norm) < MIN_BLOCK_SIZE:
                continue
            h = _block_hash(norm)
            block_hashes.add(h)
            hash_to_html[h] = norm[:120]  # for debug logging

        per_article.append(block_hashes)

    # Count how many articles each block hash appears in
    counter: Counter[str] = Counter()
    for bset in per_article:
        for h in bset:
            counter[h] += 1

    chrome = {h for h, count in counter.items() if count >= MIN_OCCURRENCES}

    if chrome:
        log.debug("Detected %d chrome block(s) across %d articles",
                   len(chrome), len(sample))
        for h in chrome:
            log.debug("  chrome block: %s…", hash_to_html.get(h, "?"))

    return chrome, hash_to_html


def _strip_blocks(html: str, chrome_hashes: set[str]) -> str:
    """Remove top-level block elements whose hash is in chrome_hashes."""
    if not chrome_hashes or not html:
        return html

    soup = BeautifulSoup(html, "html.parser")
    removed = 0

    for child in list(soup.children):
        if not isinstance(child, Tag):
            continue
        norm = _normalise_block(child)
        if len(norm) < MIN_BLOCK_SIZE:
            continue
        if _block_hash(norm) in chrome_hashes:
            child.decompose()
            removed += 1

    if not removed:
        return html

    # Clean up: remove empty elements left behind
    for tag in soup.find_all(True):
        # Remove tags that are now empty (no text, no media)
        if (not tag.get_text(strip=True)
                and not tag.find(['img', 'video', 'picture', 'audio', 'svg', 'canvas'])):
            tag.decompose()

    return str(soup)


def strip_site_chrome(articles: list[dict], report: list | None = None) -> list[dict]:
    """Given a list of articles from the SAME feed, detect and remove
    repeated HTML blocks (site chrome) from content and summary fields.

    Modifies articles in-place and returns them.
    Only the most recent SAMPLE_SIZE articles are used for detection,
    but chrome is stripped from ALL articles in the batch.
    If *report* is a list, append detail strings for each chrome block found.
    """
    if len(articles) < 2:
        return articles

    # Sort by published_at descending, take the most recent for sampling
    sorted_arts = sorted(
        articles,
        key=lambda a: a.get("published_at") or "",
        reverse=True,
    )
    sample = sorted_arts[:SAMPLE_SIZE]

    # Detect chrome in content field
    content_chrome, content_previews = _find_chrome_hashes(sample, "content")
    summary_chrome, summary_previews = _find_chrome_hashes(sample, "summary")

    if not content_chrome and not summary_chrome:
        return articles

    if report is not None:
        for h in content_chrome:
            report.append(f"chrome (content): {content_previews.get(h, '?')[:100]}")
        for h in summary_chrome:
            report.append(f"chrome (summary): {summary_previews.get(h, '?')[:100]}")

    count = 0
    for art in articles:
        if content_chrome and art.get("content"):
            art["content"] = _strip_blocks(art["content"], content_chrome)
            count += 1
        if summary_chrome and art.get("summary"):
            art["summary"] = _strip_blocks(art["summary"], summary_chrome)

    log.info("Stripped site chrome from %d articles (%d content patterns, %d summary patterns)",
             count, len(content_chrome), len(summary_chrome))

    return articles


# ── tracking-pixel / tiny-image removal ──────────────────────────────────────

# Maximum pixel dimension to consider an image a probable tracker
_TRACKER_MAX_DIM = 3

# URL path/query patterns strongly associated with tracking pixels
_TRACKER_URL_PATTERNS = re.compile(
    r'(?:'
    r'/pixel[./?\-]'
    r'|/track(?:ing)?[./?\-]'
    r'|/beacon[./?\-]'
    r'|/open[./?\-]'
    r'|[?&]utm_'
    r'|/1x1[./]'
    r'|/spacer[./]'
    r'|\.gif\?.*&?(?:e|id|u|uid|cid)='
    r')',
    re.IGNORECASE,
)

# Domains whose sole purpose is email/ad tracking
_TRACKER_DOMAINS = {
    'pixel.wp.com',
    'feeds.feedburner.com',  # 1x1 tracking GIFs
    'stats.wordpress.com',
    'www.facebook.com',  # fb tracking pixel in feeds
    'connect.facebook.net',
    'analytics.twitter.com',
    'bat.bing.com',
    'ad.doubleclick.net',
    'pagead2.googlesyndication.com',
    'www.google-analytics.com',
    'mc.yandex.ru',
    'counter.yadro.ru',
}

_HIDDEN_STYLE_RE = re.compile(
    r'(?:display\s*:\s*none|visibility\s*:\s*hidden)',
    re.IGNORECASE,
)


def _parse_dim(value: str | None) -> int | None:
    """Parse an HTML dimension attribute value to an integer, or None."""
    if not value:
        return None
    value = value.strip().rstrip('px').strip()
    try:
        return int(value)
    except ValueError:
        return None


def _is_tracking_pixel(img: Tag) -> str | None:
    """Heuristic check: is this <img> a tracking pixel or tiny tracker?
    Returns the reason string if it's a tracker, or None."""
    src = img.get("src", "") or ""

    # 1. Explicit tiny dimensions in attributes
    w = _parse_dim(img.get("width"))
    h = _parse_dim(img.get("height"))
    if w is not None and h is not None and w <= _TRACKER_MAX_DIM and h <= _TRACKER_MAX_DIM:
        return f"tiny {w}x{h} attr"

    # 2. Tiny dimensions in inline style
    style = img.get("style", "")
    if style:
        w_match = re.search(r'width\s*:\s*(\d+)', style)
        h_match = re.search(r'height\s*:\s*(\d+)', style)
        if w_match and h_match:
            sw, sh = int(w_match.group(1)), int(h_match.group(1))
            if sw <= _TRACKER_MAX_DIM and sh <= _TRACKER_MAX_DIM:
                return f"tiny {sw}x{sh} style"

    # 3. Hidden via CSS
    if _HIDDEN_STYLE_RE.search(style):
        return "hidden CSS"

    # 4. Check src URL for known tracking patterns/domains
    if src:
        try:
            parsed = urlparse(src)
            host = parsed.hostname or ""
            if host in _TRACKER_DOMAINS:
                return f"tracker domain: {host}"
        except ValueError:
            pass
        if _TRACKER_URL_PATTERNS.search(src):
            return f"tracker URL pattern"

    return None


def strip_tracking_pixels(html: str, report: list | None = None) -> str:
    """Remove tracking pixels and other tiny spy images from HTML content.
    If *report* is a list, append detail strings for each pixel removed."""
    if not html:
        return html

    soup = BeautifulSoup(html, "html.parser")
    removed = 0

    for img in soup.find_all("img"):
        reason = _is_tracking_pixel(img)
        if reason:
            src = (img.get("src") or "")[:120]
            if report is not None:
                report.append(f"tracking pixel: {reason} src={src}")
            log.debug("Stripped tracking pixel (%s): %s", reason, src)
            img.decompose()
            removed += 1

    if not removed:
        return html

    log.debug("Stripped %d tracking pixel(s) total", removed)
    return str(soup)


# ── dangerous HTML / JavaScript removal ──────────────────────────────────────

# Tags that should never appear in article content
_DANGEROUS_TAGS = {"script", "style", "iframe", "object", "embed", "applet",
                   "form", "input", "textarea", "select", "button",
                   "link", "meta", "base", "noscript"}

# Attributes that execute JavaScript
_EVENT_ATTR_RE = re.compile(r'^on', re.IGNORECASE)

# javascript: / vbscript: / data: URIs in href/src/action
_DANGEROUS_URI_RE = re.compile(
    r'^\s*(?:javascript|vbscript|data)\s*:',
    re.IGNORECASE,
)

# Attributes that can carry URIs
_URI_ATTRS = {"href", "src", "action", "formaction", "xlink:href", "poster",
              "background", "dynsrc", "lowsrc"}


def strip_dangerous_html(html: str, report: list | None = None) -> str:
    """Remove script tags, event handlers, dangerous URIs, and other
    executable/interactive elements from article HTML.

    This is a server-side defence-in-depth measure applied before content
    is stored in the database, so even if the frontend sanitiser is
    bypassed, no executable content reaches the user's browser.
    If *report* is a list, append detail strings for each item removed.
    """
    if not html:
        return html

    soup = BeautifulSoup(html, "html.parser")
    changed = False

    # 1. Remove dangerous tags entirely
    for tag_name in _DANGEROUS_TAGS:
        for tag in soup.find_all(tag_name):
            snippet = str(tag)[:100]
            if report is not None:
                report.append(f"dangerous tag <{tag_name}>: {snippet}")
            log.debug("Stripped dangerous tag <%s>: %s", tag_name, snippet)
            tag.decompose()
            changed = True

    # 2. Strip event-handler attributes (onclick, onerror, onload, …)
    for tag in soup.find_all(True):
        for attr in list(tag.attrs):
            if _EVENT_ATTR_RE.match(attr):
                if report is not None:
                    report.append(f"event handler {attr} on <{tag.name}>")
                log.debug("Stripped event handler %s on <%s>", attr, tag.name)
                del tag[attr]
                changed = True

    # 3. Neutralise dangerous URIs (javascript:, vbscript:, data:)
    for tag in soup.find_all(True):
        for attr in _URI_ATTRS:
            val = tag.get(attr)
            if val and _DANGEROUS_URI_RE.match(val):
                if report is not None:
                    report.append(f"dangerous URI {attr}={val[:80]} on <{tag.name}>")
                log.debug("Stripped dangerous URI %s=%s on <%s>", attr, val[:80], tag.name)
                del tag[attr]
                changed = True

    if not changed:
        return html

    log.debug("Stripped dangerous HTML elements from content")
    return str(soup)


# ── tracking query-parameter stripping ───────────────────────────────────────

# Parameters used for cross-site tracking / campaign attribution.
# Only well-known prefixes and specific keys are listed to avoid stripping
# legitimate query strings (page numbers, search terms, etc.).
_TRACKING_PARAMS: set[str] = {
    # Google / GA
    "utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
    "utm_id", "utm_source_platform", "utm_creative_format", "utm_marketing_tactic",
    "gclid", "gclsrc", "dclid", "gbraid", "wbraid",
    # Meta / Facebook
    "fbclid", "fb_action_ids", "fb_action_types", "fb_ref", "fb_source",
    # Microsoft / Bing
    "msclkid",
    # HubSpot
    "_hsenc", "_hsmi", "__hssc", "__hstc", "__hsfp",
    # Mailchimp
    "mc_cid", "mc_eid",
    # Twitter / X
    "twclid",
    # LinkedIn
    "li_fat_id",
    # Adobe
    "s_cid",
    # Marketo
    "mkt_tok",
    # Vero
    "vero_id",
    # Drip
    "__s",
    # Outbrain / Taboola
    "obOrigUrl", "ob_click_id", "taboolaclickid",
    # Yahoo / Oath
    "guccounter", "guce_referrer", "guce_referrer_sig",
}

# Prefixes that mark a param as tracking even if not in the set above.
_TRACKING_PREFIXES: tuple[str, ...] = ("utm_",)


def _is_tracking_param(name: str) -> bool:
    lower = name.lower()
    if lower in _TRACKING_PARAMS:
        return True
    return any(lower.startswith(p) for p in _TRACKING_PREFIXES)


def _clean_url(url: str) -> str | None:
    """Return cleaned URL with tracking params removed, or None if unchanged."""
    try:
        parsed = urlparse(url)
    except ValueError:
        return None
    if not parsed.query:
        return None
    params = parse_qs(parsed.query, keep_blank_values=True)
    cleaned = {k: v for k, v in params.items() if not _is_tracking_param(k)}
    if len(cleaned) == len(params):
        return None  # nothing removed
    new_query = urlencode(cleaned, doseq=True)
    return urlunparse(parsed._replace(query=new_query))


def strip_tracking_params(html: str, report: list | None = None) -> str:
    """Strip tracking query parameters (utm_*, fbclid, etc.) from <a> hrefs."""
    soup = BeautifulSoup(html, "html.parser")
    changed = False
    for a in soup.find_all("a", href=True):
        new_url = _clean_url(a["href"])
        if new_url is not None:
            old_url = a["href"]
            a["href"] = new_url
            if report is not None:
                report.append(f"tracking params stripped from URL: {old_url[:120]}")
            log.debug("Stripped tracking params: %s -> %s", old_url[:120], new_url[:120])
            changed = True
    if not changed:
        return html
    return str(soup)
