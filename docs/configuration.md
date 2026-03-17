# Configuration

Edit `config.toml` in the project root:

```toml
[storage]
type = "local"              # "local" or "s3"
path = "./data"             # local path or S3 URI (e.g. "s3://my-bucket/rss-lance")
# s3_region = "us-east-1"   # only needed if not set in ~/.aws/config
# s3_endpoint = ""           # custom endpoint for MinIO, R2, etc.
# See docs/s3.md for full cloud storage setup guide.

[fetcher]
interval_minutes = 30       # default fetch interval
max_concurrent = 5          # parallel feed fetches
user_agent = "RSS-Lance/1.0"

[server]
host = "127.0.0.1"
port = 8080
frontend_dir = "./frontend"
show_shutdown = false       # show a "Stop Server" button in the web UI
table_page_size = 200       # default page size for /api/tables/{name} (max 5000)

# Per-table compaction thresholds (number of data fragments).
# The fetcher runs compact_files() when a table exceeds its threshold.
[compaction]
articles      = 20    # grows fastest
feeds         = 50
categories    = 50
pending_feeds = 10    # small queue - keep tidy

[migration.ttrss]
# postgres_url = "postgresql://user:pass@host:5432/ttrss"

[migration.miniflux]
# url       = "https://miniflux.example.com"
# api_token = "your-api-token"

[migration.freshrss]
# url      = "https://freshrss.example.com"
# username = "admin"
# password = "your-password"
```

## Debug Logging

The Go server has categorised debug logging:

| Category | What it logs |
|---|---|
| `client` | HTTP API requests & responses |
| `duckdb` | Every SQL statement sent to DuckDB |
| `batch` | Write-cache operations (set, flush) |
| `lance` | Lance file/path operations |
| `all` | Enables all of the above |

### Usage

```powershell
# Windows
.\run.ps1 server -DebugLog all
.\run.ps1 server -DebugLog client,duckdb

# Linux/macOS
./run.sh --debug all server
./run.sh --debug client,duckdb server
```

Or pass directly to the server binary:

```powershell
.\build\rss-lance-server.exe --debug all
.\build\rss-lance-server.exe --port 9090
```

Via environment variable:

```powershell
$env:RSS_LANCE_DEBUG = "all"    # Windows PowerShell
RSS_LANCE_DEBUG=all ./run.sh server  # Linux/macOS
```

## Custom CSS

Add your own CSS rules to customise the look of RSS-Lance. Open **Other → Advanced Settings** in the sidebar to access the CSS editor. Changes are saved to the database and applied immediately.

Example:

```css
#sidebar {
  background: #1a1a2e;
}

.reader-stream-article h1 {
  font-size: 28px;
}
```

The custom CSS is stored in the `settings` table under the key `custom_css` and served at `/css/custom.css`. It loads after the built-in styles so your rules take precedence.

**Migration from file-based custom CSS:** If you previously used a `custom.css` file in your data directory, the server will still load it as a fallback. To migrate, paste your CSS into the Advanced Settings editor and save — the database value will take priority over the file.
