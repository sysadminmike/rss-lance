"""
Tests for the config module.
"""

import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "fetcher"))

from config import Config, load


class TestConfigLoad(unittest.TestCase):
    """Tests for the Config class with real TOML files."""

    def setUp(self):
        self._tmpdir = tempfile.mkdtemp()
        self._config_path = os.path.join(self._tmpdir, "config.toml")

    def tearDown(self):
        import shutil
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def _write_config(self, content: str):
        with open(self._config_path, "w") as f:
            f.write(content)

    def test_defaults(self):
        self._write_config("")
        cfg = Config(self._config_path)
        self.assertEqual(cfg.storage_type, "local")
        self.assertEqual(cfg.server_host, "127.0.0.1")
        self.assertEqual(cfg.server_port, 8080)

    def test_custom_values(self):
        self._write_config("""
[storage]
type = "s3"
path = "/opt/data"

[server]
host = "0.0.0.0"
port = 9090
""")
        cfg = Config(self._config_path)
        self.assertEqual(cfg.storage_type, "s3")
        self.assertEqual(cfg.server_host, "0.0.0.0")
        self.assertEqual(cfg.server_port, 9090)

    def test_compaction_defaults_removed(self):
        """Compaction settings are now in the settings table, not config.toml."""
        self._write_config("")
        cfg = Config(self._config_path)
        self.assertFalse(hasattr(cfg, 'compaction_thresholds'))

    def test_storage_path_setter(self):
        self._write_config("")
        cfg = Config(self._config_path)
        cfg.storage_path = "/custom/path"
        # Should resolve to absolute
        self.assertTrue(os.path.isabs(cfg.storage_path))

    def test_s3_path_not_resolved(self):
        """S3 URIs should be passed through without local path resolution."""
        self._write_config('[storage]\ntype = "s3"\npath = "s3://my-bucket/rss-lance"\n')
        cfg = Config(self._config_path)
        self.assertEqual(cfg.storage_path, "s3://my-bucket/rss-lance")

    def test_s3_path_setter(self):
        """Setting storage_path to an S3 URI should preserve it as-is."""
        self._write_config("")
        cfg = Config(self._config_path)
        cfg.storage_path = "s3://other-bucket/data"
        self.assertEqual(cfg.storage_path, "s3://other-bucket/data")

    def test_s3_config_properties(self):
        """s3_region and s3_endpoint should be readable from config."""
        self._write_config('[storage]\ntype = "s3"\npath = "s3://b/d"\ns3_region = "eu-west-1"\ns3_endpoint = "http://minio:9000"\n')
        cfg = Config(self._config_path)
        self.assertEqual(cfg.s3_region, "eu-west-1")
        self.assertEqual(cfg.s3_endpoint, "http://minio:9000")

    def test_s3_config_defaults_none(self):
        """s3_region and s3_endpoint default to None when not set."""
        self._write_config("")
        cfg = Config(self._config_path)
        self.assertIsNone(cfg.s3_region)
        self.assertIsNone(cfg.s3_endpoint)

    def test_migration_config(self):
        self._write_config("""
[migration.ttrss]
postgres_url = "postgresql://user:pass@host:5432/ttrss"

[migration.miniflux]
url = "https://miniflux.example.com"
api_token = "tok123"

[migration.freshrss]
url = "https://freshrss.example.com"
username = "admin"
password = "secret"
""")
        cfg = Config(self._config_path)
        self.assertEqual(cfg.ttrss_config["postgres_url"], "postgresql://user:pass@host:5432/ttrss")
        self.assertEqual(cfg.miniflux_config["url"], "https://miniflux.example.com")
        self.assertEqual(cfg.miniflux_config["api_token"], "tok123")
        self.assertEqual(cfg.freshrss_config["url"], "https://freshrss.example.com")

    def test_missing_migration(self):
        self._write_config("")
        cfg = Config(self._config_path)
        self.assertEqual(cfg.ttrss_config, {})
        self.assertEqual(cfg.miniflux_config, {})
        self.assertEqual(cfg.freshrss_config, {})


class TestLoadRaw(unittest.TestCase):
    """Tests for the raw load() function."""

    def test_load_returns_dict(self):
        tmpdir = tempfile.mkdtemp()
        path = os.path.join(tmpdir, "config.toml")
        with open(path, "w") as f:
            f.write('[storage]\ntype = "local"\n')
        result = load(path)
        self.assertIsInstance(result, dict)
        self.assertEqual(result["storage"]["type"], "local")
        import shutil
        shutil.rmtree(tmpdir, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
