# Importing & Exporting Data

RSS-Lance can import feeds and articles from other RSS readers. All importers live in `migrate/` and share a common framework that handles duplicate detection, category hierarchy, and bulk writes.

## OPML Export

Export all your feed subscriptions to a standard OPML file that any RSS reader can import:

```shell
# Windows
.\run.ps1 export-opml subscriptions.opml

# Linux / macOS
./run.sh export-opml subscriptions.opml
```

Use `-` instead of a filename to write to stdout. Add `--title "My Feeds"` to set a custom title in the OPML header.

## OPML Import (any RSS reader)

Almost every RSS reader can export an OPML file. This is the easiest way to bring your subscriptions across.

```shell
# Windows
.\.venv\Scripts\python migrate\import_opml.py subscriptions.opml

# Linux / macOS
.venv/bin/python migrate/import_opml.py subscriptions.opml
```

OPML imports feed URLs and folder structure only (not articles). After importing, run the fetcher to populate articles:

```shell
.\run.ps1 fetch-once       # Windows
./run.sh fetch-once         # Linux / macOS
```

Flags: `--dry-run`, `--feeds-only`, `--categories-only`

## TT-RSS (Tiny Tiny RSS)

Imports categories, feeds, and all articles (with read/starred status) directly from a TT-RSS PostgreSQL database.

Configure in `config.toml`:

```toml
[migration.ttrss]
postgres_url = "postgresql://user:pass@host:5432/ttrss"
# sanitize = true    # strip dangerous HTML, tracking pixels, social links, tracking params
```

Article content is sanitized by default during import (same pipeline as the live fetcher: dangerous HTML, tracking pixels, social share links, and tracking URL parameters are all stripped). Set `sanitize = false` to import raw content as-is.

> **Note:** TT-RSS is the only importer that requires `psycopg2` (a native Postgres driver). Run `build.ps1 migrate` / `build.sh migrate` first to install it, or `pip install -r migrate/requirements.txt` manually.

```shell
# Windows
.\.venv\Scripts\python migrate\import_ttrss.py

# Linux / macOS
.venv/bin/python migrate/import_ttrss.py
```

Flags: `--dry-run`, `--feeds-only`, `--articles-only`, `--categories-only`

## FreshRSS

Imports via the Google Reader compatible API. Enable API access in FreshRSS under Settings → Authentication → Allow API access.

Configure in `config.toml`:

```toml
[migration.freshrss]
url      = "https://freshrss.example.com"
username = "admin"
password = "your-password"
```

```shell
# Windows
.\.venv\Scripts\python migrate\import_freshrss.py

# Linux / macOS
.venv/bin/python migrate/import_freshrss.py
```

Flags: `--dry-run`, `--feeds-only`, `--articles-only`, `--categories-only`

## Miniflux

Imports via the Miniflux REST API. Authentication is via API token (preferred) or HTTP basic auth.

Configure in `config.toml`:

```toml
[migration.miniflux]
url       = "https://miniflux.example.com"
api_token = "your-api-token"
```

```shell
# Windows
.\.venv\Scripts\python migrate\import_miniflux.py

# Linux / macOS
.venv/bin/python migrate/import_miniflux.py
```

Flags: `--dry-run`, `--feeds-only`, `--articles-only`, `--categories-only`

## Common Notes

- All importers skip duplicate feeds and articles that already exist in LanceDB.
- Category/folder hierarchies are preserved where the source supports them.
- Install migration dependencies first: `pip install -r migrate/requirements.txt`
