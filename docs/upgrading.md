# Upgrading & Maintenance

## Upgrading an Existing Install

To upgrade an existing installation:

1. Pull the latest source (or clone fresh):
   ```bash
   cd /path/to/rss-lance-source
   git pull
   ```

2. Re-run the build, pointing at your install directory:
   ```powershell
   # Windows
   .\build.ps1 -Dir C:\Apps\rss-lance all
   ```
   ```bash
   # Linux / macOS
   ./build.sh -d /opt/rss-lance all
   ```

   This rebuilds the server binary, updates the fetcher scripts and frontend, and preserves your existing `data/` and `config.toml`.

3. Run one fetch cycle to apply any new processing to incoming articles:
   ```powershell
   .\run.ps1 fetch-once       # Windows
   ```
   ```bash
   ./run.sh fetch-once        # Linux / macOS
   ```

### Schema Migrations

Schema migrations run **automatically** the first time the fetcher opens the database after an upgrade. The fetcher tracks a `schema.version` setting and applies any pending migrations in order. No manual steps are needed.

For example, migrating from schema v1 → v3 would automatically:
- Add a `schema_version` column to the articles table
- Backfill `created_at` and `updated_at` timestamps on all tables

You'll see log lines like:

```
Migrating schema v1 → v2: adding schema_version to articles
Migrating schema v2 → v3: adding created_at/updated_at
Schema migrated to v3
```

---

## Data Fixes (datafix)

When new content-cleaning logic is added, it only applies to **newly fetched** articles. The `datafix` tool lets you apply these improvements retroactively to articles already in the database.

### Listing Available Fixes

```powershell
# Windows
.\run.ps1 datafix list
```

```bash
# Linux / macOS
./run.sh datafix list
```

### Built-in Fixes

| Fix | Description |
|---|---|
| `strip-chrome` | Strips repeated site chrome (navigation bars, related-post cards, footers) from article content by comparing articles from the same feed |
| `strip-social` | Re-runs social link stripping on all articles (Facebook, Twitter/X, LinkedIn, etc.) |

### Running a Fix

```powershell
# Windows
.\run.ps1 datafix strip-chrome
```

```bash
# Linux / macOS
./run.sh datafix strip-chrome
```

### Preview with Dry Run

Use `--dry-run` to see what would change without writing anything:

```powershell
.\run.ps1 datafix strip-chrome --dry-run       # Windows
```

```bash
./run.sh datafix strip-chrome --dry-run         # Linux / macOS
```

Output shows which feeds and articles would be modified, with before/after content sizes for the first few articles.

### Version Filtering

By default, `datafix` only processes **older articles** - those with a `schema_version` below the current version. This means running a fix twice is safe; it won't re-process articles it already touched.

To force a fix to run on **all** articles regardless of version:

```powershell
.\run.ps1 datafix strip-chrome --all            # Windows
```

```bash
./run.sh datafix strip-chrome --all             # Linux / macOS
```

### Recommended Post-Upgrade Workflow

After upgrading to a new version that adds content cleaning improvements:

```powershell
# Windows
.\run.ps1 fetch-once                            # fetch + auto-migrate schema
.\run.ps1 datafix strip-chrome                  # clean chrome from old articles
.\run.ps1 datafix strip-social                  # re-strip social links
.\run.ps1 server                                # start browsing
```

```bash
# Linux / macOS
./run.sh fetch-once
./run.sh datafix strip-chrome
./run.sh datafix strip-social
./run.sh server
```

---

## Compaction

LanceDB stores data in append-only fragments. Over time, many small fragments accumulate from individual article writes. The fetcher **automatically compacts** tables when their fragment count exceeds a configurable threshold.

Compaction merges small fragments into larger ones, improving read performance. You can tune thresholds in `config.toml`:

```toml
[compaction]
articles      = 20    # articles table grows fastest
feeds         = 50
categories    = 50
pending_feeds = 10
```

Compaction runs at the end of each fetch cycle - no manual action is needed.
