"""
LanceDB read/write operations for RSS-Lance fetcher.
"""

from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Any


def _utcnow() -> datetime:
    """Return current UTC time as a timezone-naive datetime (for Lance storage)."""
    return datetime.now(timezone.utc).replace(tzinfo=None)

import json
import lancedb
import pyarrow as pa

from config import Config

SCHEMA_VERSION = 3

# All timestamps are stored as UTC without timezone annotation.
_TS = pa.timestamp("us")

# ── Arrow schemas ───────────────────────────────────────────────────────────

FEEDS_SCHEMA = pa.schema([
    pa.field("feed_id",              pa.string()),
    pa.field("title",                pa.string()),
    pa.field("url",                  pa.string()),
    pa.field("site_url",             pa.string()),
    pa.field("icon_url",             pa.string()),
    pa.field("category_id",          pa.string()),
    pa.field("subcategory_id",       pa.string()),
    pa.field("last_fetched",         _TS),
    pa.field("last_article_date",    _TS),
    pa.field("fetch_interval_mins",  pa.int32()),
    pa.field("fetch_tier",           pa.string()),        # active/slowing/quiet/dormant/dead
    pa.field("tier_changed_at",      _TS),
    pa.field("last_successful_fetch",_TS),
    pa.field("error_count",          pa.int32()),
    pa.field("last_error",           pa.string()),
    pa.field("created_at",           _TS),
    pa.field("updated_at",           _TS),
])

ARTICLES_SCHEMA = pa.schema([
    pa.field("article_id",      pa.string()),
    pa.field("feed_id",         pa.string()),
    pa.field("title",           pa.string()),
    pa.field("url",             pa.string()),
    pa.field("author",          pa.string()),
    pa.field("content",         pa.string()),
    pa.field("summary",         pa.string()),
    pa.field("published_at",    _TS),
    pa.field("fetched_at",      _TS),
    pa.field("is_read",         pa.bool_()),
    pa.field("is_starred",      pa.bool_()),
    pa.field("guid",            pa.string()),
    pa.field("schema_version",  pa.int32()),
    pa.field("created_at",      _TS),
    pa.field("updated_at",      _TS),
])

CATEGORIES_SCHEMA = pa.schema([
    pa.field("category_id", pa.string()),
    pa.field("name",        pa.string()),
    pa.field("parent_id",   pa.string()),
    pa.field("sort_order",  pa.int32()),
    pa.field("created_at",  _TS),
    pa.field("updated_at",  _TS),
])

PENDING_FEEDS_SCHEMA = pa.schema([
    pa.field("url",           pa.string()),
    pa.field("category_id",   pa.string()),
    pa.field("requested_at",  _TS),
    pa.field("created_at",    _TS),
    pa.field("updated_at",    _TS),
])

SETTINGS_SCHEMA = pa.schema([
    pa.field("key",        pa.string()),
    pa.field("value",      pa.string()),
    pa.field("created_at", _TS),
    pa.field("updated_at", _TS),
])

# Shared log schema - identical between log_fetcher and log_api tables
LOG_SCHEMA = pa.schema([
    pa.field("log_id",     pa.string()),
    pa.field("timestamp",  _TS),
    pa.field("level",      pa.string()),   # debug / info / warn / error
    pa.field("category",   pa.string()),   # grouped category name
    pa.field("message",    pa.string()),
    pa.field("details",    pa.string()),   # optional JSON blob
    pa.field("created_at", _TS),
])

# Default settings seeded on first run
DEFAULT_SETTINGS: dict[str, Any] = {
    "schema.version":             SCHEMA_VERSION,

    "compaction.articles":        20,
    "compaction.feeds":           50,
    "compaction.categories":      50,
    "compaction.pending_feeds":   10,
    "compaction.log_fetcher":     20,
    "compaction.log_api":         20,
    "ui.theme":                   "dark",
    "ui.show_article_list":       True,
    "ui.auto_read":               True,
    # ── Log settings: fetcher ──
    "log.fetcher.enabled":              True,
    "log.fetcher.fetch_cycle":          True,   # log each fetch cycle summary
    "log.fetcher.feed_fetch":           True,   # log each feed fetched + article count
    "log.fetcher.article_processing":   False,  # debug: each article processed
    "log.fetcher.compaction":           True,   # compaction events
    "log.fetcher.tier_changes":         True,   # tier up/downgrades
    "log.fetcher.sanitization":         False,  # debug: sanitization details (tracking pixels, dangerous HTML, social links, chrome)
    "log.fetcher.errors":               True,   # fetch errors
    # ── Log settings: API server ──
    "log.api.enabled":                  True,
    "log.api.lifecycle":                True,   # server start/stop
    "log.api.requests":                 False,  # all API requests (noisy)
    "log.api.settings_changes":         True,   # settings changes
    "log.api.feed_actions":             True,   # add feed, mark-all-read, etc.
    "log.api.article_actions":          False,  # read/star individual articles (noisy)
    "log.api.easter_eggs":               True,   # duck hunt and other easter eggs
    "log.api.errors":                   True,   # error responses
    # ── Log retention ──
    "log.max_entries":                  10000,
    # ── Write cache (server: article read/star batching) ──
    "cache.flush_threshold":            20,
    "cache.flush_interval_secs":        120,
    # ── Log buffer (server: batched log inserts) ──
    "log_buffer.flush_threshold":       20,
    "log_buffer.flush_interval_secs":   30,
    # ── Tier thresholds (days without new articles before downgrade) ──
    "tier.threshold.active":            3,
    "tier.threshold.slowing":           14,
    "tier.threshold.quiet":             60,
    "tier.threshold.dormant":           180,
    # ── Tier fetch intervals (minutes between fetches per tier) ──
    "tier.interval.active":             30,
    "tier.interval.slowing":            1440,
    "tier.interval.quiet":              10080,
    "tier.interval.dormant":            43200,
    # ── Fetcher tuning ──
    "fetcher.interval_minutes":         30,
    "fetcher.max_concurrent":           5,
    "fetcher.user_agent":               "RSS-Lance/1.0",
    "fetcher.fetch_timeout_secs":       20,
    "fetcher.poll_interval_secs":       30,
    "server.table_page_size":           200,
    # ── Custom CSS ──
    "custom_css":                       "",
}


class DB:
    """Wrapper around LanceDB tables used by the fetcher.

    Supports batched writes: call begin_batch() before a fetch cycle,
    accumulate writes via add_articles() / queue_feed_update(), then
    call flush_batch() once to write everything in a single Lance
    append + update per table.  This reduces S3 PUT costs and is the
    correct write pattern for Lance (fewer, larger fragments).

    After each cycle, compact_if_needed() checks the number of data
    fragments in each table and runs LanceDB compact_files() +
    cleanup_old_versions() when the count exceeds the per-table
    threshold from the settings table (compaction.* keys).
    """

    def __init__(self, config: Config) -> None:
        self._db = lancedb.connect(config.storage_path)
        self.feeds     = self._open_or_create("feeds",      FEEDS_SCHEMA)
        self.articles  = self._open_or_create("articles",   ARTICLES_SCHEMA)
        self.categories= self._open_or_create("categories", CATEGORIES_SCHEMA)
        self.pending_feeds = self._open_or_create("pending_feeds", PENDING_FEEDS_SCHEMA)
        self.settings  = self._open_or_create("settings",   SETTINGS_SCHEMA)
        self.log_fetcher = self._open_or_create("log_fetcher", LOG_SCHEMA)
        self.log_api = self._open_or_create("log_api", LOG_SCHEMA)

        self._log_settings: dict[str, bool] = {}  # cached log settings
        self._log_batch: list[dict] = []           # buffered log entries

        # Batch state
        self._batching = False
        self._article_batch: list[dict] = []
        self._feed_updates: list[tuple[str, dict]] = []  # (feed_id, updates)

        # Seed default settings on first run and run migrations
        self._ensure_settings()
        self._migrate_schema()
        self._load_log_settings()

    def _open_or_create(self, name: str, schema: pa.Schema):
        try:
            return self._db.open_table(name)
        except Exception:
            return self._db.create_table(name, schema=schema)

    def _ensure_settings(self) -> None:
        """Seed default settings for any keys not yet in the table."""
        import logging
        log = logging.getLogger("fetcher.db")
        df = self.settings.to_pandas()
        existing_keys = set(df["key"].tolist()) if not df.empty else set()
        now = _utcnow()
        to_insert = []
        for key, val in DEFAULT_SETTINGS.items():
            if key not in existing_keys:
                to_insert.append({
                    "key": key,
                    "value": json.dumps(val),
                    "created_at": now,
                    "updated_at": now,
                })
        if to_insert:
            self.settings.add(to_insert)
            log.info("Seeded %d default settings", len(to_insert))

    def _migrate_schema(self) -> None:
        """Run schema migrations based on the stored schema version."""
        import logging
        log = logging.getLogger("fetcher.db")
        stored = self.get_setting("schema.version")
        stored_ver = int(stored) if stored is not None else 1

        if stored_ver >= SCHEMA_VERSION:
            return

        # Migration v1 → v2: add schema_version column to articles
        if stored_ver < 2:
            log.info("Migrating schema v%d → v2: adding schema_version to articles", stored_ver)
            df = self.articles.to_pandas()
            if not df.empty and "schema_version" not in df.columns:
                self.articles.update("schema_version IS NULL", {"schema_version": 1})
                log.info("  Stamped %d existing articles as schema_version=1", len(df))

        # Migration v2 → v3: add created_at / updated_at to all tables
        if stored_ver < 3:
            log.info("Migrating schema v%d → v3: adding created_at/updated_at", stored_ver)
            now = _utcnow()
            for name, table in [("feeds", self.feeds), ("articles", self.articles),
                                ("categories", self.categories), ("pending_feeds", self.pending_feeds),
                                ("settings", self.settings)]:
                df = table.to_pandas()
                if df.empty:
                    continue
                if "created_at" not in df.columns or df["created_at"].isna().any():
                    table.update("created_at IS NULL", {"created_at": now})
                if "updated_at" not in df.columns or df["updated_at"].isna().any():
                    table.update("updated_at IS NULL", {"updated_at": now})
                log.info("  Backfilled created_at/updated_at on %s", name)

        # Update stored version
        self.put_setting("schema.version", SCHEMA_VERSION)
        log.info("Schema migrated to v%d", SCHEMA_VERSION)

    # ── settings ─────────────────────────────────────────────────────────

    def get_all_settings(self) -> dict[str, Any]:
        """Return all settings as {key: parsed_value}."""
        df = self.settings.to_pandas()
        if df.empty:
            return {}
        result = {}
        for _, row in df.iterrows():
            try:
                result[row["key"]] = json.loads(row["value"])
            except (json.JSONDecodeError, TypeError):
                result[row["key"]] = row["value"]
        return result

    def get_setting(self, key: str) -> Any:
        """Return a single setting value, or None if not set."""
        df = self.settings.to_pandas()
        if df.empty:
            return None
        match = df[df["key"] == key]
        if match.empty:
            return None
        try:
            return json.loads(match.iloc[0]["value"])
        except (json.JSONDecodeError, TypeError):
            return match.iloc[0]["value"]

    def put_setting(self, key: str, value: Any) -> None:
        """Insert or update a single setting."""
        now = _utcnow()
        json_val = json.dumps(value)
        df = self.settings.to_pandas()
        if not df.empty and key in df["key"].values:
            self.settings.update(f"key = '{key}'", {
                "value": json_val,
                "updated_at": now,
            })
        else:
            self.settings.add([{
                "key": key,
                "value": json_val,
                "created_at": now,
                "updated_at": now,
            }])

    def put_settings(self, settings: dict[str, Any]) -> None:
        """Insert or update multiple settings."""
        for key, val in settings.items():
            self.put_setting(key, val)

    # ── batch lifecycle ──────────────────────────────────────────────────

    def begin_batch(self) -> None:
        """Start accumulating writes instead of flushing immediately."""
        self._batching = True
        self._article_batch.clear()
        self._feed_updates.clear()

    def flush_batch(self) -> int:
        """Write all accumulated articles and feed updates in one go.

        Returns the number of articles written.
        """
        articles_written = 0

        # 1. Single bulk append for all new articles
        if self._article_batch:
            self.articles.add(self._article_batch)
            articles_written = len(self._article_batch)

        # 2. Apply feed metadata updates (these are row-level updates,
        #    but at least we've deferred them to the end of the cycle)
        for feed_id, updates in self._feed_updates:
            self.feeds.update(f"feed_id = '{feed_id}'", updates)

        self._article_batch.clear()
        self._feed_updates.clear()
        self._batching = False
        return articles_written

    # ── feeds ────────────────────────────────────────────────────────────

    def get_all_feeds(self) -> list[dict]:
        return self.feeds.to_pandas().to_dict("records")

    def get_feeds_due(self) -> list[dict]:
        """Return feeds whose next fetch time has passed."""
        import pandas as pd
        df = self.feeds.to_pandas()
        if df.empty:
            return []
        now = datetime.now(timezone.utc)
        due = []
        for row in df.to_dict("records"):
            if row.get("fetch_tier") == "dead":
                continue
            last = row.get("last_fetched")
            if last is None or pd.isna(last):
                due.append(row)
                continue
            if hasattr(last, "tzinfo") and last.tzinfo is None:
                last = last.replace(tzinfo=timezone.utc)
            interval = row.get("fetch_interval_mins") or 30
            elapsed = (now - last).total_seconds() / 60
            if elapsed >= interval:
                due.append(row)
        return due

    def add_feed(self, url: str, title: str = "", site_url: str = "",
                 category_id: str = "") -> str:
        feed_id = str(uuid.uuid4())
        now = _utcnow()
        row = {
            "feed_id": feed_id, "title": title, "url": url,
            "site_url": site_url, "icon_url": "", "category_id": category_id,
            "subcategory_id": "", "last_fetched": None, "last_article_date": None,
            "fetch_interval_mins": 30, "fetch_tier": "active",
            "tier_changed_at": now, "last_successful_fetch": None,
            "error_count": 0, "last_error": "",
            "created_at": now, "updated_at": now,
        }
        self.feeds.add([row])
        return feed_id

    def update_feed_after_fetch(self, feed_id: str, success: bool,
                                error_msg: str = "",
                                last_article_date: datetime | None = None,
                                new_tier: str | None = None) -> None:
        now = _utcnow()
        if success:
            updates: dict[str, Any] = {
                "last_fetched": now,
                "last_successful_fetch": now,
                "error_count": 0,
                "last_error": "",
                "updated_at": now,
            }
            if last_article_date:
                updates["last_article_date"] = last_article_date
            if new_tier:
                updates["fetch_tier"] = new_tier
                updates["tier_changed_at"] = now
        else:
            # Increment error_count
            df = self.feeds.to_pandas()
            row = df[df["feed_id"] == feed_id]
            cur_count = int(row["error_count"].iloc[0]) if not row.empty else 0
            updates = {
                "last_fetched": now,
                "error_count": cur_count + 1,
                "last_error": error_msg,
                "updated_at": now,
            }
        if self._batching:
            self._feed_updates.append((feed_id, updates))
            return
        self.feeds.update(f"feed_id = '{feed_id}'", updates)

    # ── articles ─────────────────────────────────────────────────────────

    def get_existing_guids(self, feed_id: str) -> set[str]:
        df = self.articles.to_pandas()
        if df.empty:
            return set()
        return set(df[df["feed_id"] == feed_id]["guid"].tolist())

    def add_articles(self, rows: list[dict]) -> int:
        if not rows:
            return 0
        if self._batching:
            self._article_batch.extend(rows)
            return len(rows)
        self.articles.add(rows)
        return len(rows)

    def mark_article_read(self, article_id: str) -> None:
        self.articles.update(f"article_id = '{article_id}'", {
            "is_read": True, "updated_at": _utcnow(),
        })

    def mark_article_starred(self, article_id: str, starred: bool = True) -> None:
        self.articles.update(f"article_id = '{article_id}'", {
            "is_starred": starred, "updated_at": _utcnow(),
        })

    # ── categories ───────────────────────────────────────────────────────

    def get_categories(self) -> list[dict]:
        return self.categories.to_pandas().to_dict("records")

    def add_category(self, name: str, parent_id: str = "",
                     sort_order: int = 0) -> str:
        cat_id = str(uuid.uuid4())
        now = _utcnow()
        self.categories.add(
            [{"category_id": cat_id, "name": name,
              "parent_id": parent_id, "sort_order": sort_order,
              "created_at": now, "updated_at": now}],
        )
        return cat_id

    # ── logging ───────────────────────────────────────────────────────────

    def _load_log_settings(self) -> None:
        """Cache log settings from the settings table."""
        all_settings = self.get_all_settings()
        self._log_settings = {
            k: bool(v)
            for k, v in all_settings.items()
            if k.startswith("log.fetcher.")
        }

    def _should_log(self, category: str) -> bool:
        """Check if a log category is enabled."""
        if not self._log_settings.get("log.fetcher.enabled", True):
            return False
        return self._log_settings.get(f"log.fetcher.{category}", False)

    def log_event(
        self,
        level: str,
        category: str,
        message: str,
        details: str = "",
    ) -> None:
        """Write a log entry to the log_fetcher table.

        If batching is active, entries are buffered and flushed with flush_log_batch().
        """
        if not self._should_log(category):
            return
        now = _utcnow()
        row = {
            "log_id": str(uuid.uuid4()),
            "timestamp": now,
            "level": level,
            "category": category,
            "message": message,
            "details": details,
            "created_at": now,
        }
        if self._batching:
            self._log_batch.append(row)
        else:
            self.log_fetcher.add([row])

    def flush_log_batch(self) -> int:
        """Write buffered log entries. Called alongside flush_batch()."""
        if not self._log_batch:
            return 0
        count = len(self._log_batch)
        self.log_fetcher.add(self._log_batch)
        self._log_batch.clear()
        return count

    def trim_logs(self, max_entries: int | None = None) -> int:
        """Remove oldest log entries beyond the configured threshold.

        Supports two modes controlled by the ``log.retention_mode`` setting:
        - ``"count"`` (default): keep at most *max_entries* rows.
        - ``"age"``: delete rows older than ``log.max_age_days`` days.

        Returns count deleted.
        """
        mode = self.get_setting("log.retention_mode") or "count"

        if mode == "age":
            return self._trim_logs_by_age()

        # --- count-based mode (original behaviour) ---
        if max_entries is None:
            max_entries = self.get_setting("log.max_entries")
            if max_entries is None:
                max_entries = 10000
            max_entries = int(max_entries)
        if max_entries <= 0:
            return 0  # 0 means retain all logs
        import pandas as pd
        df = self.log_fetcher.to_pandas()
        if len(df) <= max_entries:
            return 0
        # Keep only the newest max_entries rows
        df = df.sort_values("timestamp", ascending=False)
        to_delete = df.iloc[max_entries:]
        if to_delete.empty:
            return 0
        oldest_keep = df.iloc[max_entries - 1]["timestamp"]
        self.log_fetcher.delete(f"timestamp < timestamp '{oldest_keep}'")
        return len(to_delete)

    def _trim_logs_by_age(self) -> int:
        """Delete fetcher log entries older than ``log.max_age_days`` days."""
        max_age = self.get_setting("log.max_age_days")
        if max_age is None:
            max_age = 30
        max_age = int(max_age)
        if max_age <= 0:
            return 0
        from datetime import datetime, timedelta, timezone
        cutoff = datetime.now(timezone.utc) - timedelta(days=max_age)
        cutoff_str = cutoff.strftime("%Y-%m-%dT%H:%M:%S.%f")
        import pandas as pd
        df = self.log_fetcher.to_pandas()
        before = len(df)
        self.log_fetcher.delete(f"timestamp < timestamp '{cutoff_str}'")
        df_after = self.log_fetcher.to_pandas()
        return before - len(df_after)

    # ── compaction ───────────────────────────────────────────────────────

    def _fragment_count(self, table) -> int:
        """Return the number of data fragments in a Lance table."""
        try:
            stats = table.stats()
            # LanceDB Python stats() returns an object with num_data_files
            return getattr(stats, "num_data_files", 0) or 0
        except Exception:
            # Fallback: count files on disk (works for local storage)
            try:
                import os
                table_path = os.path.join(str(table._dataset_uri), "data")
                if os.path.isdir(table_path):
                    return len(os.listdir(table_path))
            except Exception:
                pass
            return 0

    def compact_if_needed(self) -> dict[str, bool]:
        """Check each table's fragment count against its threshold.

        Thresholds are read from the settings table (compaction.* keys).
        If over the threshold, run compact_files() and
        cleanup_old_versions().  Returns a dict of
        {table_name: was_compacted}.
        """
        import logging
        log = logging.getLogger("fetcher.compaction")

        # Read thresholds from settings table
        all_settings = self.get_all_settings()
        defaults = {"articles": 20, "feeds": 50, "categories": 50, "pending_feeds": 10,
                   "log_fetcher": 20, "log_api": 20}
        thresholds: dict[str, int] = {}
        for name, default in defaults.items():
            val = all_settings.get(f"compaction.{name}")
            try:
                thresholds[name] = int(val) if val is not None else default
            except (ValueError, TypeError):
                thresholds[name] = default

        tables = {
            "articles":      self.articles,
            "feeds":         self.feeds,
            "categories":    self.categories,
            "pending_feeds": self.pending_feeds,
            "log_fetcher":   self.log_fetcher,
            "log_api":       self.log_api,
        }
        results: dict[str, bool] = {}

        for name, table in tables.items():
            threshold = thresholds.get(name, 50)
            frags = self._fragment_count(table)
            if frags >= threshold:
                log.info("Compacting %s (%d fragments >= threshold %d)",
                         name, frags, threshold)
                try:
                    table.compact_files()
                    table.cleanup_old_versions()
                    new_frags = self._fragment_count(table)
                    log.info("  %s compacted: %d → %d fragments",
                             name, frags, new_frags)
                    results[name] = True
                except Exception as exc:
                    log.warning("  Compaction failed for %s: %s", name, exc)
                    results[name] = False
            else:
                results[name] = False

        return results
