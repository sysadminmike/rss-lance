# ============================================================
# RSS-Lance - multi-stage Docker build
# Produces a minimal image (~150 MB) with:
#   - Go HTTP server (CGo, embedded DuckDB)
#   - Python fetcher + lancedb
#   - Static frontend
# Lance data is expected as an external mount at /data
# ============================================================

# ---- Stage 1: Build Go server (CGo for embedded DuckDB) ----
FROM golang:1.23-bookworm AS go-builder

RUN apt-get update && apt-get install -y --no-install-recommends gcc g++ && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download

COPY server/ ./server/
RUN cd server && CGO_ENABLED=1 go build -o /rss-lance-server .

# ---- Stage 2: Install Python fetcher deps ----
FROM python:3.12-slim-bookworm AS py-builder

WORKDIR /src
COPY fetcher/requirements.txt ./fetcher/
RUN pip install --no-cache-dir --prefix=/pylibs -r fetcher/requirements.txt

# ---- Stage 3: Final minimal image ----
FROM python:3.12-slim-bookworm

# Runtime deps for CGo DuckDB
RUN apt-get update && apt-get install -y --no-install-recommends \
    libstdc++6 ca-certificates tini \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN useradd -m -s /bin/bash rss

WORKDIR /app

# Go binary
COPY --from=go-builder /rss-lance-server ./rss-lance-server

# Python libraries
COPY --from=py-builder /pylibs /usr/local

# Application code
COPY fetcher/ ./fetcher/
COPY frontend/ ./frontend/
COPY config.toml ./config.toml

# Default config: bind 0.0.0.0 inside container, data at /data
RUN sed -i 's|host = "127.0.0.1"|host = "0.0.0.0"|' config.toml \
    && sed -i 's|path = "./data"|path = "/data"|' config.toml \
    && sed -i 's|frontend_dir = "./frontend"|frontend_dir = "/app/frontend"|' config.toml

# /data is the mount point for Lance tables (local dir, NFS, Samba, S3 fuse, etc.)
VOLUME /data

EXPOSE 8080

USER rss

# Use tini as init to handle signals properly
ENTRYPOINT ["tini", "--"]

# Default: run the Go server. Override with "fetcher" or "fetcher-once" (see below).
CMD ["./rss-lance-server", "-config", "config.toml"]
