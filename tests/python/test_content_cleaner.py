"""
Tests for tracking-pixel removal in content_cleaner.
"""

import sys
import os
import unittest

# Allow imports from fetcher/
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "fetcher"))

from content_cleaner import strip_tracking_pixels, strip_dangerous_html, strip_tracking_params


class TestStripTrackingPixels(unittest.TestCase):
    """Tests for strip_tracking_pixels."""

    # ── dimension-based detection ────────────────────────────────────────

    def test_removes_1x1_pixel(self):
        html = '<p>Article</p><img src="https://example.com/img.gif" width="1" height="1">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Article</p>")

    def test_removes_0x0_pixel(self):
        html = '<p>Text</p><img src="https://example.com/t.png" width="0" height="0">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Text</p>")

    def test_removes_tiny_2x2(self):
        html = '<p>Body</p><img src="https://example.com/s.gif" width="2" height="2">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Body</p>")

    def test_removes_3x1(self):
        html = '<p>Body</p><img src="https://example.com/s.gif" width="3" height="1">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Body</p>")

    def test_preserves_normal_image(self):
        html = '<p>Look:</p><img src="https://example.com/photo.jpg" width="640" height="480">'
        self.assertEqual(strip_tracking_pixels(html), html)

    def test_preserves_image_without_dimensions(self):
        """Images with no width/height and non-tracking URLs should be kept."""
        html = '<img src="https://example.com/cat.jpg" alt="cat">'
        self.assertEqual(strip_tracking_pixels(html), html)

    def test_removes_pixel_dimensions_with_px_suffix(self):
        html = '<p>Hi</p><img src="https://example.com/t.gif" width="1px" height="1px">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Hi</p>")

    # ── inline style detection ───────────────────────────────────────────

    def test_removes_tiny_inline_style(self):
        html = '<p>Content</p><img src="https://example.com/t.gif" style="width:1px;height:1px">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Content</p>")

    def test_removes_display_none(self):
        html = '<p>Content</p><img src="https://example.com/t.gif" style="display: none">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Content</p>")

    def test_removes_visibility_hidden(self):
        html = '<p>Content</p><img src="https://example.com/t.gif" style="visibility:hidden">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Content</p>")

    # ── URL pattern detection ────────────────────────────────────────────

    def test_removes_pixel_url(self):
        html = '<p>Read</p><img src="https://example.com/pixel.gif">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Read</p>")

    def test_removes_tracking_url(self):
        html = '<p>Read</p><img src="https://example.com/tracking/open?id=123">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Read</p>")

    def test_removes_beacon_url(self):
        html = '<p>Body</p><img src="https://example.com/beacon/view?uid=abc">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Body</p>")

    def test_removes_utm_params(self):
        html = '<p>More</p><img src="https://example.com/img.gif?utm_source=newsletter">'
        self.assertEqual(strip_tracking_pixels(html), "<p>More</p>")

    def test_removes_spacer_gif(self):
        html = '<p>Hello</p><img src="https://example.com/spacer.gif">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Hello</p>")

    def test_removes_1x1_url_pattern(self):
        html = '<p>Yo</p><img src="https://example.com/1x1.png">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Yo</p>")

    # ── known tracking domains ───────────────────────────────────────────

    def test_removes_pixel_wp_com(self):
        html = '<p>Post</p><img src="https://pixel.wp.com/g.gif?v=wpcom">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Post</p>")

    def test_removes_doubleclick(self):
        html = '<p>News</p><img src="https://ad.doubleclick.net/activity;dc_iu=abc">'
        self.assertEqual(strip_tracking_pixels(html), "<p>News</p>")

    def test_removes_google_analytics(self):
        html = '<p>Story</p><img src="https://www.google-analytics.com/__utm.gif?utmn=123">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Story</p>")

    def test_removes_feedburner_tracker(self):
        html = '<p>Feed</p><img src="https://feeds.feedburner.com/~r/somefeed/~4/abc" height="1" width="1">'
        self.assertEqual(strip_tracking_pixels(html), "<p>Feed</p>")

    # ── edge cases ───────────────────────────────────────────────────────

    def test_empty_string(self):
        self.assertEqual(strip_tracking_pixels(""), "")

    def test_no_images(self):
        html = "<p>No images here</p>"
        self.assertEqual(strip_tracking_pixels(html), html)

    def test_removes_multiple_trackers(self):
        html = (
            '<p>Article</p>'
            '<img src="https://pixel.wp.com/g.gif" width="1" height="1">'
            '<img src="https://example.com/tracking/open?id=1">'
            '<img src="https://example.com/photo.jpg" width="800" height="600">'
        )
        result = strip_tracking_pixels(html)
        self.assertIn("<p>Article</p>", result)
        self.assertIn("photo.jpg", result)
        self.assertNotIn("pixel.wp.com", result)
        self.assertNotIn("tracking/open", result)

    def test_preserves_small_but_not_tiny_image(self):
        """A 16x16 favicon-style image should survive."""
        html = '<img src="https://example.com/icon.png" width="16" height="16">'
        self.assertEqual(strip_tracking_pixels(html), html)

    def test_only_width_no_height_not_removed(self):
        """Need both dimensions to be tiny; one alone is not enough."""
        html = '<img src="https://example.com/banner.gif" width="1">'
        self.assertEqual(strip_tracking_pixels(html), html)


class TestStripDangerousHtml(unittest.TestCase):
    """Tests for strip_dangerous_html."""

    # ── script removal ───────────────────────────────────────────────────

    def test_removes_script_tag(self):
        html = '<p>Hello</p><script>alert("xss")</script><p>World</p>'
        self.assertEqual(strip_dangerous_html(html), "<p>Hello</p><p>World</p>")

    def test_removes_script_with_attributes(self):
        html = '<script type="text/javascript" src="https://evil.com/x.js"></script><p>Ok</p>'
        self.assertEqual(strip_dangerous_html(html), "<p>Ok</p>")

    def test_removes_multiple_scripts(self):
        html = '<script>a()</script><p>Safe</p><script>b()</script>'
        self.assertEqual(strip_dangerous_html(html), "<p>Safe</p>")

    # ── style removal ────────────────────────────────────────────────────

    def test_removes_style_tag(self):
        html = '<style>body{display:none}</style><p>Content</p>'
        self.assertEqual(strip_dangerous_html(html), "<p>Content</p>")

    # ── iframe / embed / object removal ──────────────────────────────────

    def test_removes_iframe(self):
        html = '<p>Before</p><iframe src="https://evil.com"></iframe><p>After</p>'
        self.assertEqual(strip_dangerous_html(html), "<p>Before</p><p>After</p>")

    def test_removes_object_tag(self):
        html = '<p>Text</p><object data="x.swf"></object>'
        self.assertEqual(strip_dangerous_html(html), "<p>Text</p>")

    def test_removes_embed_tag(self):
        html = '<p>Text</p><embed src="x.swf">'
        self.assertEqual(strip_dangerous_html(html), "<p>Text</p>")

    # ── form elements ────────────────────────────────────────────────────

    def test_removes_form(self):
        html = '<p>Article</p><form action="https://evil.com"><input type="text"><button>Submit</button></form>'
        self.assertEqual(strip_dangerous_html(html), "<p>Article</p>")

    # ── event handler attributes ─────────────────────────────────────────

    def test_strips_onclick(self):
        html = '<a href="https://example.com" onclick="evil()">Link</a>'
        result = strip_dangerous_html(html)
        self.assertNotIn("onclick", result)
        self.assertIn("Link", result)
        self.assertIn("https://example.com", result)

    def test_strips_onerror(self):
        html = '<img src="photo.jpg" onerror="alert(1)">'
        result = strip_dangerous_html(html)
        self.assertNotIn("onerror", result)
        self.assertIn("photo.jpg", result)

    def test_strips_onload(self):
        html = '<body onload="steal()"><p>Content</p></body>'
        result = strip_dangerous_html(html)
        self.assertNotIn("onload", result)
        self.assertIn("Content", result)

    def test_strips_onmouseover(self):
        html = '<div onmouseover="track()"><p>Text</p></div>'
        result = strip_dangerous_html(html)
        self.assertNotIn("onmouseover", result)
        self.assertIn("Text", result)

    # ── dangerous URIs ───────────────────────────────────────────────────

    def test_strips_javascript_uri_href(self):
        html = '<a href="javascript:alert(1)">Click</a>'
        result = strip_dangerous_html(html)
        self.assertNotIn("javascript:", result)
        self.assertIn("Click", result)

    def test_strips_javascript_uri_with_spaces(self):
        html = '<a href="  javascript:void(0)">X</a>'
        result = strip_dangerous_html(html)
        self.assertNotIn("javascript:", result)

    def test_strips_vbscript_uri(self):
        html = '<a href="vbscript:MsgBox(1)">Click</a>'
        result = strip_dangerous_html(html)
        self.assertNotIn("vbscript:", result)

    def test_strips_data_uri_in_src(self):
        html = '<img src="data:text/html,<script>alert(1)</script>">'
        result = strip_dangerous_html(html)
        self.assertNotIn("data:", result)

    # ── meta / base / link tags ──────────────────────────────────────────

    def test_removes_meta_tag(self):
        html = '<meta http-equiv="refresh" content="0;url=https://evil.com"><p>Text</p>'
        self.assertEqual(strip_dangerous_html(html), "<p>Text</p>")

    def test_removes_base_tag(self):
        html = '<base href="https://evil.com"><p>Article</p>'
        self.assertEqual(strip_dangerous_html(html), "<p>Article</p>")

    def test_removes_link_tag(self):
        html = '<link rel="stylesheet" href="https://evil.com/steal.css"><p>Read</p>'
        self.assertEqual(strip_dangerous_html(html), "<p>Read</p>")

    # ── preserves safe content ───────────────────────────────────────────

    def test_preserves_safe_html(self):
        html = '<p>This is <strong>safe</strong> with <a href="https://example.com">links</a>.</p>'
        self.assertEqual(strip_dangerous_html(html), html)

    def test_preserves_images(self):
        html = '<p>Look:</p><img src="https://example.com/photo.jpg" alt="cat">'
        self.assertEqual(strip_dangerous_html(html), html)

    def test_preserves_video(self):
        html = '<video src="https://example.com/vid.mp4" controls></video>'
        self.assertEqual(strip_dangerous_html(html), html)

    # ── edge cases ───────────────────────────────────────────────────────

    def test_empty_string(self):
        self.assertEqual(strip_dangerous_html(""), "")

    def test_no_dangerous_content(self):
        html = "<p>Clean article</p>"
        self.assertEqual(strip_dangerous_html(html), html)

    def test_combined_attacks(self):
        html = (
            '<script>steal()</script>'
            '<p onclick="evil()">Good content</p>'
            '<a href="javascript:void(0)">Safe text</a>'
            '<iframe src="https://evil.com"></iframe>'
            '<style>.hide{display:none}</style>'
        )
        result = strip_dangerous_html(html)
        self.assertNotIn("<script", result)
        self.assertNotIn("onclick", result)
        self.assertNotIn("javascript:", result)
        self.assertNotIn("<iframe", result)
        self.assertNotIn("<style", result)
        self.assertIn("Good content", result)
        self.assertIn("Safe text", result)


class TestStripTrackingParams(unittest.TestCase):
    """Tests for strip_tracking_params."""

    def test_strips_utm_source(self):
        html = '<a href="https://example.com/page?utm_source=twitter&page=1">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("utm_source", result)
        self.assertIn("page=1", result)
        self.assertIn("example.com/page", result)

    def test_strips_fbclid(self):
        html = '<a href="https://example.com/?fbclid=abc123">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("fbclid", result)

    def test_strips_gclid(self):
        html = '<a href="https://example.com/?gclid=xyz">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("gclid", result)

    def test_strips_msclkid(self):
        html = '<a href="https://example.com/?msclkid=abc">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("msclkid", result)

    def test_strips_hsenc(self):
        html = '<a href="https://example.com/?_hsenc=abc&_hsmi=def">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("_hsenc", result)
        self.assertNotIn("_hsmi", result)

    def test_strips_mailchimp_params(self):
        html = '<a href="https://example.com/?mc_cid=abc&mc_eid=def">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("mc_cid", result)
        self.assertNotIn("mc_eid", result)

    def test_strips_multiple_utm_params(self):
        html = '<a href="https://example.com/page?utm_source=x&utm_medium=y&utm_campaign=z">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("utm_source", result)
        self.assertNotIn("utm_medium", result)
        self.assertNotIn("utm_campaign", result)

    def test_preserves_non_tracking_params(self):
        html = '<a href="https://example.com/search?q=hello&page=2&sort=date">Link</a>'
        result = strip_tracking_params(html)
        self.assertIn("q=hello", result)
        self.assertIn("page=2", result)
        self.assertIn("sort=date", result)

    def test_preserves_url_without_params(self):
        html = '<a href="https://example.com/article">Link</a>'
        result = strip_tracking_params(html)
        self.assertIn("https://example.com/article", result)

    def test_preserves_non_link_elements(self):
        html = '<p>Hello <strong>world</strong></p>'
        result = strip_tracking_params(html)
        self.assertIn("Hello", result)
        self.assertIn("world", result)

    def test_strips_mixed_tracking_and_legit(self):
        html = '<a href="https://example.com/page?utm_source=fb&fbclid=abc&id=42">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("utm_source", result)
        self.assertNotIn("fbclid", result)
        self.assertIn("id=42", result)

    def test_handles_multiple_links(self):
        html = (
            '<a href="https://a.com/?utm_source=x">A</a>'
            '<a href="https://b.com/?page=1">B</a>'
            '<a href="https://c.com/?gclid=y&q=test">C</a>'
        )
        result = strip_tracking_params(html)
        self.assertNotIn("utm_source", result)
        self.assertNotIn("gclid", result)
        self.assertIn("page=1", result)
        self.assertIn("q=test", result)

    def test_report_populated(self):
        html = '<a href="https://example.com/?utm_source=test">Link</a>'
        report = []
        strip_tracking_params(html, report=report)
        self.assertTrue(len(report) > 0)
        self.assertTrue(any("tracking params" in r for r in report))

    def test_report_empty_when_nothing_stripped(self):
        html = '<a href="https://example.com/page">Link</a>'
        report = []
        strip_tracking_params(html, report=report)
        self.assertEqual(len(report), 0)

    def test_case_insensitive_param_matching(self):
        html = '<a href="https://example.com/?UTM_SOURCE=test">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("UTM_SOURCE", result)

    def test_strips_custom_utm_prefix_params(self):
        html = '<a href="https://example.com/?utm_custom_thing=val&page=1">Link</a>'
        result = strip_tracking_params(html)
        self.assertNotIn("utm_custom_thing", result)
        self.assertIn("page=1", result)


if __name__ == "__main__":
    unittest.main()
