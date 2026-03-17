"""
Configuration loader for RSS-Lance.
Reads config.toml from the project root (two levels up from this file).
"""

from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Any

try:
    import tomllib  # Python 3.11+
except ImportError:
    try:
        import tomli as tomllib  # pip install tomli
    except ImportError:
        # Minimal fallback - only works for simple key=value TOML
        tomllib = None  # type: ignore[assignment]


_CLOUD_SCHEMES = ("s3://", "gs://", "az://")


def _is_cloud_uri(path: str) -> bool:
    """Return True if path is a cloud storage URI (S3, GCS, Azure)."""
    return any(path.startswith(s) for s in _CLOUD_SCHEMES)


def _find_config() -> Path:
    """Search for config.toml starting from the script's directory upward."""
    search = Path(__file__).resolve().parent
    for _ in range(4):
        candidate = search / "config.toml"
        if candidate.exists():
            return candidate
        search = search.parent
    raise FileNotFoundError(
        "config.toml not found. Copy/rename config.toml.example to config.toml in the project root."
    )


def load(path: str | Path | None = None) -> dict[str, Any]:
    """Load and return the config as a nested dict."""
    if path is None:
        path = _find_config()
    path = Path(path)

    if tomllib is None:
        raise RuntimeError(
            "No TOML parser available. Install `tomli` (`pip install tomli`) "
            "or use Python 3.11+ which includes `tomllib`."
        )

    with open(path, "rb") as fh:
        cfg = tomllib.load(fh)
    return cfg


class Config:
    """Thin wrapper giving attribute access to config sections."""

    def __init__(self, path: str | Path | None = None) -> None:
        self._data = load(path)

    # ── storage ────────────────────────────────────────────────────────────
    @property
    def storage_type(self) -> str:
        return self._data.get("storage", {}).get("type", "local")

    @property
    def storage_path(self) -> str:
        if hasattr(self, "_storage_path_override"):
            return self._storage_path_override
        raw = self._data.get("storage", {}).get("path", "./data")
        # Cloud URIs (s3://, gs://, az://) are passed through as-is
        if _is_cloud_uri(raw):
            return raw
        # Resolve relative to project root (parent of fetcher/)
        project_root = Path(__file__).resolve().parent.parent
        return str((project_root / raw).resolve())

    @storage_path.setter
    def storage_path(self, value: str) -> None:
        if _is_cloud_uri(value):
            self._storage_path_override = value
        else:
            self._storage_path_override = str(Path(value).resolve())

    @property
    def s3_region(self) -> str | None:
        return self._data.get("storage", {}).get("s3_region", None)

    @property
    def s3_endpoint(self) -> str | None:
        return self._data.get("storage", {}).get("s3_endpoint", None)

    # ── fetcher ────────────────────────────────────────────────────────────
    @property
    def fetch_interval_minutes(self) -> int:
        return int(self._data.get("fetcher", {}).get("interval_minutes", 30))

    @property
    def max_concurrent(self) -> int:
        return int(self._data.get("fetcher", {}).get("max_concurrent", 5))

    @property
    def user_agent(self) -> str:
        return self._data.get("fetcher", {}).get("user_agent", "RSS-Lance/1.0")

    # ── server ─────────────────────────────────────────────────────────────
    @property
    def server_host(self) -> str:
        return self._data.get("server", {}).get("host", "127.0.0.1")

    @property
    def server_port(self) -> int:
        return int(self._data.get("server", {}).get("port", 8080))

    # ── migration ──────────────────────────────────────────────────────────
    @property
    def ttrss_config(self) -> dict:
        """Return [migration.ttrss] as a dict (empty if not configured)."""
        return self._data.get("migration", {}).get("ttrss", {})

    @property
    def miniflux_config(self) -> dict:
        """Return [migration.miniflux] as a dict (empty if not configured)."""
        return self._data.get("migration", {}).get("miniflux", {})

    @property
    def freshrss_config(self) -> dict:
        """Return [migration.freshrss] as a dict (empty if not configured)."""
        return self._data.get("migration", {}).get("freshrss", {})