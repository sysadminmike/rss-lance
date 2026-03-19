"""
lance_writer.py - Persistent LanceDB write sidecar for the Go server.

Spawned once by the Go server as a long-lived subprocess. Reads JSON-line
commands on stdin, writes JSON-line responses on stdout. All Lance CUD
(Create/Update/Delete) operations are routed through this process so the
Go binary does not need the lancedb-go native Rust library.

Protocol:
    -> {"cmd": "info"}
    <- {"ok": true, "info": {"pid": 123, ...}}

    -> {"cmd": "put_setting", "key": "k", "value": "v"}
    <- {"ok": true}

    -> {"cmd": "bad"}
    <- {"ok": false, "error": "unknown command: bad"}

Usage:
    .venv/bin/python tools/lance_writer.py <data_path>
"""

from __future__ import annotations

import json
import os
import re
import sys
import time
import uuid
from datetime import datetime, timezone
from typing import Any


# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #

_SAFE_ID_RE = re.compile(r"^[a-fA-F0-9-]+$")


def _escape(val: str) -> str:
    """Escape a string value for LanceDB filter expressions."""
    if not _SAFE_ID_RE.match(val):
        return val.replace("'", "''")
    return val


def _utcnow() -> datetime:
    """Return current UTC time as a timezone-naive datetime."""
    return datetime.now(timezone.utc).replace(tzinfo=None)


def _respond(obj: dict) -> None:
    """Write a single JSON line to stdout and flush."""
    sys.stdout.write(json.dumps(obj, default=str) + "\n")
    sys.stdout.flush()


def _ok(**extra: Any) -> None:
    """Send a success response."""
    resp: dict[str, Any] = {"ok": True}
    resp.update(extra)
    _respond(resp)


def _err(msg: str) -> None:
    """Send an error response."""
    _respond({"ok": False, "error": msg})


# --------------------------------------------------------------------------- #
# Writer
# --------------------------------------------------------------------------- #

class LanceWriter:
    """Manages LanceDB table handles and dispatches commands."""

    def __init__(self, data_path: str) -> None:
        import lancedb
        import pyarrow  # noqa: F401 - imported for version reporting

        self._start_time = time.monotonic()
        self._data_path = data_path
        self._db = lancedb.connect(data_path)

        # Cache table handles (opened lazily)
        self._tables: dict[str, Any] = {}

    # -- table access -------------------------------------------------------

    def _table(self, name: str) -> Any:
        """Open a table lazily and cache the handle."""
        if name not in self._tables:
            self._tables[name] = self._db.open_table(name)
        return self._tables[name]

    def _table_exists(self, name: str) -> bool:
        """Check if a Lance table directory exists on disk."""
        tbl_dir = os.path.join(self._data_path, f"{name}.lance")
        return os.path.isdir(tbl_dir)

    # -- info ---------------------------------------------------------------

    def cmd_info(self, _req: dict) -> None:
        import lancedb
        import pyarrow

        _ok(info={
            "pid": os.getpid(),
            "lancedb_version": lancedb.__version__,
            "pyarrow_version": pyarrow.__version__,
            "uptime_seconds": int(time.monotonic() - self._start_time),
            "data_path": self._data_path,
        })

    # -- settings -----------------------------------------------------------

    def cmd_put_setting(self, req: dict) -> None:
        key = req["key"]
        value = req["value"]
        now = _utcnow()
        self._table("settings").update(
            f"key = '{_escape(key)}'",
            {"value": value, "updated_at": now},
        )
        _ok()

    def cmd_put_settings_batch(self, req: dict) -> None:
        settings = req["settings"]  # dict of key -> value
        now = _utcnow()

        # Group keys by value for efficient batching
        groups: dict[str, list[str]] = {}
        for k, v in settings.items():
            groups.setdefault(v, []).append(k)

        tbl = self._table("settings")
        for val, keys in groups.items():
            if len(keys) == 1:
                filt = f"key = '{_escape(keys[0])}'"
            else:
                quoted = ", ".join(f"'{_escape(k)}'" for k in keys)
                filt = f"key IN ({quoted})"
            tbl.update(filt, {"value": val, "updated_at": now})
        _ok()

    def cmd_insert_setting(self, req: dict) -> None:
        now = _utcnow()
        self._table("settings").add([{
            "key": req["key"],
            "value": req["value"],
            "created_at": now,
            "updated_at": now,
        }])
        _ok()

    def cmd_insert_settings(self, req: dict) -> None:
        settings = req["settings"]  # dict of key -> value
        if not settings:
            _ok()
            return
        now = _utcnow()
        rows = [
            {"key": k, "value": v, "created_at": now, "updated_at": now}
            for k, v in settings.items()
        ]
        self._table("settings").add(rows)
        _ok()

    # -- articles -----------------------------------------------------------

    def cmd_update_article(self, req: dict) -> None:
        article_id = req["article_id"]
        updates = dict(req["updates"])
        updates["updated_at"] = _utcnow()
        filt = f"article_id = '{_escape(article_id)}'"
        self._table("articles").update(filt, updates)
        _ok()

    def cmd_set_article_read(self, req: dict) -> None:
        article_id = req["article_id"]
        is_read = bool(req["is_read"])
        filt = f"article_id = '{_escape(article_id)}'"
        self._table("articles").update(filt, {
            "is_read": is_read,
            "updated_at": _utcnow(),
        })
        _ok()

    def cmd_set_article_starred(self, req: dict) -> None:
        article_id = req["article_id"]
        is_starred = bool(req["is_starred"])
        filt = f"article_id = '{_escape(article_id)}'"
        self._table("articles").update(filt, {
            "is_starred": is_starred,
            "updated_at": _utcnow(),
        })
        _ok()

    def cmd_mark_all_read(self, req: dict) -> None:
        feed_id = req["feed_id"]
        filt = f"feed_id = '{_escape(feed_id)}' AND is_read = false"
        self._table("articles").update(filt, {
            "is_read": True,
            "updated_at": _utcnow(),
        })
        _ok()

    def cmd_flush_overrides(self, req: dict) -> None:
        overrides = req["overrides"]  # dict of article_id -> {is_read?, is_starred?}
        if not overrides:
            _ok()
            return

        # Group by identical update payload for batching
        groups: dict[tuple, list[str]] = {}
        for aid, ov in overrides.items():
            # Build a hashable key for the update payload
            parts: list[tuple[str, Any]] = []
            if "is_read" in ov:
                parts.append(("is_read", bool(ov["is_read"])))
            if "is_starred" in ov:
                parts.append(("is_starred", bool(ov["is_starred"])))
            key = tuple(parts)
            if key:
                groups.setdefault(key, []).append(aid)

        tbl = self._table("articles")
        now = _utcnow()
        for payload_key, ids in groups.items():
            updates: dict[str, Any] = dict(payload_key)
            updates["updated_at"] = now

            if len(ids) == 1:
                filt = f"article_id = '{_escape(ids[0])}'"
            else:
                quoted = ", ".join(f"'{_escape(i)}'" for i in ids)
                filt = f"article_id IN ({quoted})"
            tbl.update(filt, updates)
        _ok()

    # -- pending feeds ------------------------------------------------------

    def cmd_insert_pending_feed(self, req: dict) -> None:
        now = _utcnow()
        self._table("pending_feeds").add([{
            "url": req["url"],
            "category_id": req.get("category_id", ""),
            "requested_at": now,
            "created_at": now,
            "updated_at": now,
        }])
        _ok()

    def cmd_delete_pending_feed(self, req: dict) -> None:
        url = req["url"]
        filt = f"url = '{_escape(url)}'"
        self._table("pending_feeds").delete(filt)
        _ok()

    # -- logs ---------------------------------------------------------------

    def cmd_insert_logs(self, req: dict) -> None:
        entries = req["entries"]
        if not entries:
            _ok()
            return

        if not self._table_exists("log_api"):
            _ok()  # table not created yet (fetcher creates it on first run)
            return

        now = _utcnow()
        rows = []
        for e in entries:
            ts = e.get("timestamp")
            if ts and isinstance(ts, str):
                try:
                    ts = datetime.fromisoformat(ts.replace("Z", "+00:00")).replace(tzinfo=None)
                except (ValueError, TypeError):
                    ts = now
            else:
                ts = now
            rows.append({
                "log_id": e.get("log_id", str(uuid.uuid4())),
                "timestamp": ts,
                "level": e.get("level", "info"),
                "category": e.get("category", ""),
                "message": e.get("message", ""),
                "details": e.get("details", ""),
                "created_at": now,
            })
        self._table("log_api").add(rows)
        _ok()

    def cmd_delete_old_logs(self, req: dict) -> None:
        filt = req["filter"]
        if not self._table_exists("log_api"):
            _ok()
            return
        self._table("log_api").delete(filt)
        _ok()

    # -- metadata -----------------------------------------------------------

    def cmd_table_exists(self, req: dict) -> None:
        name = req.get("table", "log_api")
        _ok(exists=self._table_exists(name))

    def cmd_table_meta(self, req: dict) -> None:
        name = req["table"]
        if not self._table_exists(name):
            _ok(version=0, columns=[], indexes=[])
            return

        tbl = self._table(name)
        schema = tbl.schema

        columns = [
            {"name": f.name, "type": str(f.type)}
            for f in schema
        ]

        # lancedb Python API: version and indexes
        version = 0
        try:
            version = tbl.version
        except Exception:
            pass

        indexes: list[dict] = []
        try:
            idx_list = tbl.list_indices()
            for idx in idx_list:
                indexes.append({
                    "name": getattr(idx, "name", ""),
                    "columns": getattr(idx, "columns", []),
                    "index_type": getattr(idx, "index_type", ""),
                })
        except Exception:
            pass

        _ok(version=version, columns=columns, indexes=indexes)

    # -- dispatch -----------------------------------------------------------

    COMMANDS = {
        "info":                 "cmd_info",
        "put_setting":          "cmd_put_setting",
        "put_settings_batch":   "cmd_put_settings_batch",
        "insert_setting":       "cmd_insert_setting",
        "insert_settings":      "cmd_insert_settings",
        "update_article":       "cmd_update_article",
        "set_article_read":     "cmd_set_article_read",
        "set_article_starred":  "cmd_set_article_starred",
        "mark_all_read":        "cmd_mark_all_read",
        "flush_overrides":      "cmd_flush_overrides",
        "insert_pending_feed":  "cmd_insert_pending_feed",
        "delete_pending_feed":  "cmd_delete_pending_feed",
        "insert_logs":          "cmd_insert_logs",
        "delete_old_logs":      "cmd_delete_old_logs",
        "table_exists":         "cmd_table_exists",
        "table_meta":           "cmd_table_meta",
    }

    def dispatch(self, line: str) -> None:
        """Parse a JSON-line command and call the appropriate handler."""
        try:
            req = json.loads(line)
        except json.JSONDecodeError as e:
            _err(f"invalid JSON: {e}")
            return

        cmd = req.get("cmd", "")
        handler_name = self.COMMANDS.get(cmd)
        if not handler_name:
            _err(f"unknown command: {cmd}")
            return

        try:
            getattr(self, handler_name)(req)
        except Exception as e:
            _err(f"{cmd}: {e}")


# --------------------------------------------------------------------------- #
# Main loop
# --------------------------------------------------------------------------- #

def main() -> None:
    if len(sys.argv) < 2:
        print("Usage: lance_writer.py <data_path>", file=sys.stderr)
        sys.exit(1)

    data_path = sys.argv[1]

    # Redirect stderr to avoid polluting the JSON protocol on stdout.
    # The Go process captures stderr separately for logging.
    # Keep stdout clean for the JSON-line protocol.

    try:
        writer = LanceWriter(data_path)
    except Exception as e:
        # Fatal startup error - report on stderr and exit
        print(f"FATAL: Failed to connect to LanceDB at {data_path}: {e}", file=sys.stderr)
        sys.exit(1)

    # Signal readiness
    _ok(ready=True)

    # Read commands from stdin, one JSON object per line
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        writer.dispatch(line)


if __name__ == "__main__":
    main()
