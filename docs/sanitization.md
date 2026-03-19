# Content Sanitization

RSS-Lance cleans article HTML at two levels - once when articles are fetched (Python, server-side) and again when they are displayed in the browser (JavaScript, client-side). This defence-in-depth approach means that even if one layer is bypassed, the other still protects the user.

## Python Sanitiser (fetch time)

All cleaning happens in `fetcher/content_cleaner.py` and `fetcher/feed_parser.py` before content is written to the database. The pipeline runs in this order:

### 1. Dangerous HTML Removal

Strips executable and interactive elements so they never reach the database:

| What | How |
|---|---|
| `<script>`, `<style>`, `<iframe>`, `<object>`, `<embed>`, `<applet>` | Entire tag + contents removed |
| `<form>`, `<input>`, `<textarea>`, `<select>`, `<button>` | Entire tag + contents removed |
| `<meta>`, `<base>`, `<link>`, `<noscript>` | Entire tag removed |
| Event handler attributes (`onclick`, `onerror`, `onload`, …) | Attribute removed, element kept |
| `javascript:`, `vbscript:`, `data:` URIs in `href`, `src`, `action`, etc. | Attribute removed, element kept |

### 2. Social Sharing Link Removal

Strips `<a>` tags whose `href` points to a known social-network sharing URL. The domain list includes Facebook, Twitter/X, LinkedIn, Pinterest, Reddit, Tumblr, WhatsApp, Telegram, Instagram, Threads, Buffer, Pocket, Digg, Flipboard, StumbleUpon, and Mix.

Only the `<a>` tag itself is removed - surrounding text is preserved.

### 3. Tracking Pixel Removal

Removes tiny or hidden `<img>` tags used for open-rate tracking, analytics, or fingerprinting. An image is considered a tracker if **any** of these are true:

- **Tiny dimensions:** both `width` and `height` are ≤ 3 pixels (via HTML attributes or inline CSS)
- **Hidden via CSS:** inline style contains `display: none` or `visibility: hidden`
- **Known tracker domain:** `src` points to a domain like `pixel.wp.com`, `ad.doubleclick.net`, `www.google-analytics.com`, `feeds.feedburner.com`, etc.
- **Tracking URL pattern:** `src` contains paths like `/pixel.`, `/tracking/`, `/beacon/`, `/1x1.`, `/spacer.`, or query strings with `utm_` parameters

Normal content images (photos, diagrams, icons) are not affected.

### 4. Tracking Query Parameter Stripping

Strips known tracking/campaign-attribution query parameters from `<a href>` URLs while preserving legitimate parameters (page numbers, search terms, etc.).

**Stripped parameters include:**

| Source | Parameters |
|---|---|
| Google / GA | `utm_source`, `utm_medium`, `utm_campaign`, `utm_term`, `utm_content`, `utm_id`, `gclid`, `gclsrc`, `dclid`, `gbraid`, `wbraid` |
| Meta / Facebook | `fbclid`, `fb_action_ids`, `fb_action_types`, `fb_ref`, `fb_source` |
| Microsoft / Bing | `msclkid` |
| HubSpot | `_hsenc`, `_hsmi`, `__hssc`, `__hstc`, `__hsfp` |
| Mailchimp | `mc_cid`, `mc_eid` |
| Twitter / X | `twclid` |
| LinkedIn | `li_fat_id` |
| Others | `s_cid`, `mkt_tok`, `vero_id`, `__s`, `obOrigUrl`, `ob_click_id`, `taboolaclickid`, `guccounter`, `guce_referrer`, `guce_referrer_sig` |

Any parameter starting with `utm_` is also stripped.

Example: `https://example.com/page?utm_source=newsletter&fbclid=abc&page=2` → `https://example.com/page?page=2`

### 5. Site Chrome Removal

When articles from the same feed are fetched in a batch, repeated HTML blocks (navigation bars, related-post sections, footers) are detected by hashing and removed. A block must appear in at least 2 of the 5 most recent articles to be classified as site chrome.

## JavaScript Sanitiser (display time)

The frontend sanitiser in `frontend/js/reader.js` runs when an article is rendered in the browser. Because the Python layer already handles the heavy lifting, this is a second safety net.

### Regex Pass

Applies fast regex replacements before DOM parsing:

| What | Regex |
|---|---|
| `<script>…</script>` | Removed |
| `<style>…</style>` | Removed |
| `<iframe …>` | Opening tag removed |
| `on*="…"` event handlers | Attribute removed |
| Social sharing `<a>` tags | Removed (same domain list as Python) |
### DOM-based XSS Sanitiser

After the regex pass, a full DOM-based sanitiser (`_domSanitise()`) parses the HTML into a `<template>` element and walks the entire tree:

1. **Dangerous elements removed entirely:** `<script>`, `<style>`, `<iframe>`, `<object>`, `<embed>`, `<applet>`, `<form>`, `<base>`, `<meta>`, `<link>`, `<svg>`
2. **Event handler attributes stripped:** all attributes starting with `on` (onclick, onerror, onload, onmouseover, etc.)
3. **Dangerous URI schemes stripped:** `javascript:`, `data:`, `vbscript:` in `href`, `src`, `action`, `formaction`, `xlink:href`

This provides defence-in-depth against stored XSS -- even if a malicious payload bypasses the regex pass, the DOM pass catches it.
### Social Container Removal (DOM)

After regex cleaning, a DOM-based pass removes entire social/sharing **containers** - not just individual links:

- Elements with classes or IDs matching social patterns (`share`, `sharing`, `social-icon`, `sharedaddy`, `jetpack-sharing`, `addtoany`, `a2a_kit`, etc.)
- `<ul>` / `<ol>` lists where every `<li>` links exclusively to social networks
- Headings like "Share this", "Share on", "Spread the word" (plus their next sibling)
- Empty wrapper elements left behind after removal

A safety guard prevents false positives: if this pass would remove more than 200 characters of visible text, it is skipped for that article and the original HTML is used.

### Site Chrome Removal (DOM)

Removes feed-embedded navigation and "related content" blocks by matching known class patterns (`related-post`, `topic-card`, `read-next`, `article-footer`, etc.) and headings like "Keep Exploring", "Related Articles", "You May Also Like".

### Tracking Query Parameter Stripping (DOM)

Mirrors the Python-side tracking parameter stripping as a defence-in-depth measure. Uses DOM parsing to find all `<a>` elements and strips the same set of known tracking parameters from their `href` URLs via `URL` / `searchParams`. Handles both absolute and relative URLs.

### Cleanup

After all passes, empty elements (empty `<p>`, `<div>`, `<span>`, `<ul>`, etc.) are removed and runs of 3+ `<br>` tags are collapsed to a single line break.

## Content Security Policy

The frontend includes a `<meta>` CSP tag in `frontend/index.html` that restricts resource loading:

- `default-src 'self'` -- only load scripts/fonts/etc. from the same origin
- `style-src 'self' 'unsafe-inline'` -- allow inline styles (needed for dynamic theming)
- `img-src * data:` -- allow images from any origin (RSS feeds embed external images)
- `media-src *` -- allow audio/video from any origin
- `connect-src 'self'` -- XHR/fetch only to the same origin (the API)

This prevents any injected `<script>` from loading external resources or exfiltrating data, even if it bypasses both sanitiser layers.

## Non-Content Display

Pages that display server metadata (DB status, server status, logs) use `textContent` or an `_escapeHTML()` helper that creates a text node and reads its HTML-escaped value. This prevents XSS from unexpected data_path values, table names, or log messages. These pages never use `innerHTML` for server-provided strings.

## What's Safe

Both layers are designed to preserve legitimate article content:

- Regular `<a>` links to non-social sites (with tracking query params stripped)
- Images with normal dimensions
- `<video>`, `<audio>`, `<picture>`, `<figure>`, `<figcaption>` elements
- All text formatting (`<strong>`, `<em>`, `<code>`, `<blockquote>`, headings, lists, tables, etc.)

## Testing

Python sanitiser tests are in `fetcher/tests/test_content_cleaner.py` and `fetcher/tests/test_feed_parser.py`. Frontend sanitiser tests are in `frontend/tests/sanitise.test.js`.

```shell
# Python tests
.\run.ps1 test-python       # Windows
./run.sh test-python         # Linux / macOS

# Frontend tests
.\run.ps1 test-frontend      # Windows
./run.sh test-frontend       # Linux / macOS
```
