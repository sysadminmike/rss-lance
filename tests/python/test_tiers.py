"""
Tests for the main module's tier computation logic.
"""

import os
import sys
import unittest
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "fetcher"))

from main import _compute_new_tier, TIER_THRESHOLDS, TIER_INTERVALS


class TestComputeNewTier(unittest.TestCase):
    """Tests for _compute_new_tier."""

    def _feed(self, tier="active", days_since_article=0):
        """Create a fake feed dict for testing."""
        last_article = datetime.now(timezone.utc) - timedelta(days=days_since_article)
        return {
            "fetch_tier": tier,
            "last_article_date": last_article,
        }

    def test_active_with_new_articles_stays_active(self):
        feed = self._feed("active", days_since_article=0)
        result = _compute_new_tier(feed, had_new_articles=True)
        self.assertIsNone(result)  # No change

    def test_slowing_with_new_articles_promotes_to_active(self):
        feed = self._feed("slowing", days_since_article=5)
        result = _compute_new_tier(feed, had_new_articles=True)
        self.assertEqual(result, "active")

    def test_dormant_with_new_articles_promotes_to_active(self):
        feed = self._feed("dormant", days_since_article=100)
        result = _compute_new_tier(feed, had_new_articles=True)
        self.assertEqual(result, "active")

    def test_active_no_articles_3_days_downgrades(self):
        feed = self._feed("active", days_since_article=4)
        result = _compute_new_tier(feed, had_new_articles=False)
        self.assertEqual(result, "slowing")

    def test_active_no_articles_15_days_downgrades(self):
        feed = self._feed("active", days_since_article=15)
        result = _compute_new_tier(feed, had_new_articles=False)
        # Should downgrade - may jump tiers based on duration
        self.assertIn(result, ["slowing", "quiet"])

    def test_active_no_articles_200_days_downgrades(self):
        feed = self._feed("active", days_since_article=200)
        result = _compute_new_tier(feed, had_new_articles=False)
        # The function downgrades one tier at a time per call.
        # At 200 days the first applicable threshold triggers,
        # so it moves from active → slowing (3-day threshold fires first).
        self.assertIsNotNone(result)
        self.assertNotEqual(result, "active")

    def test_no_last_article_date_no_change(self):
        feed = {"fetch_tier": "active", "last_article_date": None}
        result = _compute_new_tier(feed, had_new_articles=False)
        self.assertIsNone(result)

    def test_missing_tier_defaults_to_active(self):
        feed = {"last_article_date": datetime.now(timezone.utc)}
        result = _compute_new_tier(feed, had_new_articles=True)
        self.assertIsNone(result)  # Already active, no change


class TestTierConstants(unittest.TestCase):
    """Verify tier configuration constants are correct."""

    def test_tier_thresholds_keys(self):
        expected = {"active", "slowing", "quiet", "dormant"}
        self.assertEqual(set(TIER_THRESHOLDS.keys()), expected)

    def test_tier_intervals_keys(self):
        expected = {"active", "slowing", "quiet", "dormant", "dead"}
        self.assertEqual(set(TIER_INTERVALS.keys()), expected)

    def test_active_interval_30(self):
        self.assertEqual(TIER_INTERVALS["active"], 30)

    def test_dead_interval_none(self):
        self.assertIsNone(TIER_INTERVALS["dead"])

    def test_thresholds_ascending(self):
        """Tier thresholds should increase: active < slowing < quiet < dormant."""
        order = ["active", "slowing", "quiet", "dormant"]
        for i in range(len(order) - 1):
            self.assertLess(
                TIER_THRESHOLDS[order[i]],
                TIER_THRESHOLDS[order[i + 1]],
                f"{order[i]} threshold should be less than {order[i+1]}",
            )


if __name__ == "__main__":
    unittest.main()
