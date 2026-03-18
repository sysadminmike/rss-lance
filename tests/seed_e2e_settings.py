#!/usr/bin/env python3
"""
Seed the Lance settings table with ALL log categories enabled.

Used by the e2e test to pre-populate the database before starting
the Go server, so every log category is on from the very first request.

Can also be run standalone:
    python seed_e2e_settings.py <config_path>
"""

from __future__ import annotations

import sys
from pathlib import Path

# Allow importing fetcher modules
ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "fetcher"))


# All log settings that should be TRUE for e2e testing.
# This includes categories that are off by default (article_processing,
# sanitization, requests, article_actions) so we get full coverage.
ALL_LOG_SETTINGS: dict[str, bool] = {
    # Fetcher logging
    "log.fetcher.enabled":              True,
    "log.fetcher.fetch_cycle":          True,
    "log.fetcher.feed_fetch":           True,
    "log.fetcher.article_processing":   True,
    "log.fetcher.compaction":           True,
    "log.fetcher.tier_changes":         True,
    "log.fetcher.errors":               True,
    "log.fetcher.sanitization":         True,
    # API server logging
    "log.api.enabled":                  True,
    "log.api.lifecycle":                True,
    "log.api.requests":                 True,
    "log.api.settings_changes":         True,
    "log.api.feed_actions":             True,
    "log.api.article_actions":          True,
    "log.api.easter_eggs":              True,
    "log.api.errors":                   True,
}


def seed_settings(config_path: str, data_path: str) -> "DB":
    """Create/open the DB via the fetcher's DB class and enable all log settings.

    Returns the DB object so the caller can reuse it for feed population.
    """
    from config import Config
    from db import DB

    config = Config(path=config_path)
    config.storage_path = data_path
    db = DB(config)

    # Write all log settings as true
    db.put_settings(ALL_LOG_SETTINGS)
    # Reload cached log settings so log_event() works immediately
    db._load_log_settings()

    return db


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print("Usage: python seed_e2e_settings.py <config_path> <data_path>")
        sys.exit(1)
    seed_settings(sys.argv[1], sys.argv[2])
    print("Settings seeded successfully.")
