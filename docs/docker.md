# Docker

The Docker image bundles the Go server, Python fetcher, and frontend into a single minimal image (~150 MB). No nginx, no reverse proxy — the Go binary serves HTTP directly. Lance data is mounted from outside so you can point it at a local directory, NFS share, or S3 FUSE mount.

## Quick Start

```bash
# Build the image
docker compose build

# Insert demo feeds + fetch articles
docker compose run --rm demo-data
docker compose run --rm fetcher-once

# Start server + continuous fetcher
docker compose up -d
```

Open **http://localhost:8080**.

## Docker Compose Services

| Service | Description |
|---|---|
| `server` | Go HTTP server on port 8080 |
| `fetcher` | Python daemon that continuously polls feeds |
| `fetcher-once` | One-shot fetch (run with `docker compose run --rm`) |
| `demo-data` | Insert demo RSS feeds (run with `docker compose run --rm`) |

All services share the same `./data` volume. Replace it with any mount:

```yaml
# docker-compose.override.yml - example: NFS mount
services:
  server:
    volumes:
      - nfs-data:/data
  fetcher:
    volumes:
      - nfs-data:/data

volumes:
  nfs-data:
    driver: local
    driver_opts:
      type: nfs
      o: addr=192.168.1.50,rw,nolock
      device: ":/srv/rss-lance/data"
```

## Split Fetcher / Server Across Machines

Run the fetcher on one machine and the server on another, both pointing at the same Lance data:

```bash
# On your Linux server - fetcher only
docker run -d --name rss-fetcher \
  -v /mnt/shared-data:/data \
  rss-lance python fetcher/main.py

# On your laptop - server only
docker run -d --name rss-server \
  -p 8080:8080 \
  -v /mnt/shared-data:/data \
  rss-lance
```

Or without Docker - run the Go binary on Windows pointing at a Samba-mounted path:

```powershell
net use Z: \\linux-box\rss-data
# Edit config.toml: path = "Z:\\"
.\build\rss-lance-server.exe -config config.toml
```

## Build the Image Manually

```bash
docker build -t rss-lance .
docker run -d -p 8080:8080 -v ./data:/data --name rss-lance rss-lance
```
