#!/usr/bin/env bash
#
# RSS-Lance build & setup script for Linux/FreeBSD/macOS
#
# Usage:
#   ./build.sh [-d <dir>] [--no-tests] <command>
#
# Options:
#   -d <dir>       Project directory to work in (default: script location)
#   --no-tests     Skip running tests after build (tests run by default with 'all')
#
# Commands:
#   setup        First-time setup (venv + Go deps)
#   server       Build Go server for current platform
#   server-all   Cross-compile server for all platforms
#   fetcher      Install fetcher Python deps
#   run-fetcher  Run the feed fetcher
#   run-server   Run the HTTP server
#   demo-data    Insert demo RSS feeds for testing
#   duckdb       Download DuckDB CLI into tools/
#   migrate      Install migration deps (see docs/importing.md)
#   clean        Clean build artifacts
#   all          Full build (setup + duckdb + server + demo-data)
#
# Examples:
#   ./build.sh setup                       # Use script directory
#   ./build.sh -d /opt/rss-lance all       # Build in a specific dir
#   ./build.sh --no-tests all              # Build without running tests
#   ./build.sh test                        # Run tests only

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SOURCE_ROOT="$SCRIPT_DIR"
PROJECT_ROOT="$SCRIPT_DIR"
USING_CUSTOM_DIR=false
NO_TESTS=false

# Parse optional flags
while [[ $# -gt 0 ]]; do
    case $1 in
        -d)
            mkdir -p "$2" 2>/dev/null || true
            PROJECT_ROOT="$(cd "$2" && pwd)" || { echo "Directory not found: $2" >&2; exit 1; }
            USING_CUSTOM_DIR=true
            shift 2
            ;;
        --no-tests)
            NO_TESTS=true
            shift
            ;;
        *)
            break
            ;;
    esac
done

echo "Source from: $SOURCE_ROOT"
echo "Working in:  $PROJECT_ROOT"

VENV_PATH="$PROJECT_ROOT/.venv"
BUILD_DIR="$PROJECT_ROOT/build"
DATA_DIR="$PROJECT_ROOT/data"
TOOLS_DIR="$PROJECT_ROOT/tools"

# Source directory for Go compilation (only needed at build time)
SERVER_DIR="$SOURCE_ROOT/server"

# Runtime directories - always relative to $PROJECT_ROOT so the app
# works after the source repo is deleted.
FETCHER_DIR="$PROJECT_ROOT/fetcher"
FRONTEND_DIR="$PROJECT_ROOT/frontend"
MIGRATE_DIR="$PROJECT_ROOT/migrate"
CONFIG_FILE="$PROJECT_ROOT/config.toml"

step() { echo -e "\n\033[36m== $1 ==\033[0m"; }
ok()   { echo -e "\033[32m$1\033[0m"; }
fail() { echo -e "\033[31m$1\033[0m"; exit 1; }

ensure_venv() {
    if [ ! -f "$VENV_PATH/bin/python" ]; then
        step "Creating Python virtual environment"
        python3 -m venv "$VENV_PATH"
    fi
    source "$VENV_PATH/bin/activate"
}

install_requirements() {
    local req_file="$1"
    # Fast path: try installing everything at once
    if pip install -r "$req_file" 2>/dev/null; then
        return
    fi

    echo "Bulk install failed, retrying per-package with binary fallback..."
    while IFS= read -r line || [[ -n "$line" ]]; do
        line="$(echo "$line" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
        [[ -z "$line" || "$line" == \#* ]] && continue
        if ! pip install "$line" 2>/dev/null; then
            if [[ "$line" == *-binary* ]]; then
                fallback="${line//-binary/}"
                echo "  Binary failed for $line, trying $fallback"
                pip install "$fallback" || fail "Failed to install $fallback"
            else
                fail "Failed to install $line"
            fi
        fi
    done < "$req_file"
}

copy_runtime_files() {
    # Copy Python scripts, frontend, and config into $PROJECT_ROOT
    # so the app is self-contained and works after deleting the source repo.
    [ "$SOURCE_ROOT" = "$PROJECT_ROOT" ] && return

    step "Copying runtime files to project directory"

    # fetcher/*.py + requirements.txt
    mkdir -p "$PROJECT_ROOT/fetcher"
    find "$SOURCE_ROOT/fetcher" -maxdepth 1 -type f -exec cp {} "$PROJECT_ROOT/fetcher/" \;
    echo "  Copied fetcher/ ($(find "$PROJECT_ROOT/fetcher" -maxdepth 1 -type f | wc -l) files)"

    # frontend/ (recursive)
    if [ -d "$SOURCE_ROOT/frontend" ]; then
        cp -r "$SOURCE_ROOT/frontend" "$PROJECT_ROOT/"
        echo "  Copied frontend/"
    fi

    # config.toml (only if not already present)
    if [ -f "$SOURCE_ROOT/config.toml" ] && [ ! -f "$CONFIG_FILE" ]; then
        cp "$SOURCE_ROOT/config.toml" "$CONFIG_FILE"
        echo "  Copied config.toml"
    fi

    # run.ps1 / run.sh (runtime scripts)
    for rs in run.ps1 run.sh; do
        [ -f "$SOURCE_ROOT/$rs" ] && cp "$SOURCE_ROOT/$rs" "$PROJECT_ROOT/$rs"
    done
    echo "  Copied run.ps1 / run.sh"
}

cmd_setup() {
    # Copy runtime files first so pip install reads from $PROJECT_ROOT
    copy_runtime_files

    step "Setting up Python virtual environment"
    ensure_venv

    step "Installing fetcher dependencies"
    install_requirements "$FETCHER_DIR/requirements.txt"

    step "Verifying Go installation"
    if ! command -v go &>/dev/null; then
        fail "Go is not installed. Install it from https://go.dev/dl/"
    fi
    go version

    step "Initializing Go module (if needed)"
    if [ ! -f "$SERVER_DIR/go.mod" ]; then
        pushd "$SERVER_DIR" > /dev/null
        go mod init rss-lance/server
        popd > /dev/null
    fi

    step "Creating data directory"
    mkdir -p "$DATA_DIR"

    ok "Setup complete!"
}

cmd_server() {
    step "Building Go server"
    mkdir -p "$BUILD_DIR"
    pushd "$SERVER_DIR" > /dev/null

    # Enable CGo for lancedb-go native bindings
    local arch
    arch=$(uname -m)
    local lib_dir="$SERVER_DIR/lib/linux_${arch}"
    if [ ! -d "$lib_dir" ]; then
        # Fallback: try linux_amd64 for x86_64
        if [ "$arch" = "x86_64" ]; then
            lib_dir="$SERVER_DIR/lib/linux_amd64"
        fi
    fi

    if [ -d "$lib_dir" ] && [ -f "$SERVER_DIR/include/lancedb.h" ]; then
        export CGO_ENABLED=1
        export CGO_CFLAGS="-I$SERVER_DIR/include"
        export CGO_LDFLAGS="$lib_dir/liblancedb_go.a -lm -ldl -lpthread"
        echo "  CGo enabled (lancedb-go native lib: $lib_dir)"
    else
        echo "  WARNING: lancedb-go native lib not found at $lib_dir" >&2
        echo "  Download it with: cd server && go generate ./..." >&2
        echo "  Falling back to CGO_ENABLED=0 (lancedb-go writes will not work)" >&2
        export CGO_ENABLED=0
    fi

    local build_time
    build_time=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
    local ldflags="-X main.BuildTime=${build_time}"
    if [ -n "${BUILD_VERSION:-}" ]; then
        ldflags="${ldflags} -X main.BuildVersion=${BUILD_VERSION}"
    fi
    go build -ldflags "$ldflags" -o "$BUILD_DIR/rss-lance-server" .
    popd > /dev/null
    ok "Built: build/rss-lance-server"
}

cmd_server_all() {
    step "Cross-compiling Go server for all platforms"
    mkdir -p "$BUILD_DIR"
    pushd "$SERVER_DIR" > /dev/null

    targets=(
        "windows:amd64:.exe"
        "linux:amd64:"
        "linux:arm64:"
        "darwin:amd64:"
        "darwin:arm64:"
        "freebsd:amd64:"
    )

    # Native build for current platform (CGo enabled for lancedb-go)
    local native_os
    native_os=$(uname -s | tr '[:upper:]' '[:lower:]')
    local native_arch
    native_arch=$(uname -m)
    [ "$native_arch" = "x86_64" ] && native_arch="amd64"
    [ "$native_arch" = "aarch64" ] && native_arch="arm64"
    local native_ext=""
    local native_name="rss-lance-server-${native_os}-${native_arch}${native_ext}"
    echo "  Building $native_name (CGo) ..."

    local lib_dir="$SERVER_DIR/lib/${native_os}_${native_arch}"
    if [ -d "$lib_dir" ] && [ -f "$SERVER_DIR/include/lancedb.h" ]; then
        CGO_ENABLED=1 \
        CGO_CFLAGS="-I$SERVER_DIR/include" \
        CGO_LDFLAGS="$lib_dir/liblancedb_go.a -lm -ldl -lpthread" \
        GOOS="$native_os" GOARCH="$native_arch" \
        local build_time
        build_time=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
        local ldflags="-X main.BuildTime=${build_time}"
        if [ -n "${BUILD_VERSION:-}" ]; then
            ldflags="${ldflags} -X main.BuildVersion=${BUILD_VERSION}"
        fi
        go build -ldflags "$ldflags" -o "$BUILD_DIR/$native_name" .
    else
        echo "  WARNING: native lib not found at $lib_dir, skipping native CGo build" >&2
    fi

    echo "  Cross-compiled targets require platform-specific native libs."
    echo "  Build those on their native platform or in CI."

    # Cross-compiled targets - CGo disabled (informational only)
    for target in "${targets[@]}"; do
        IFS=":" read -r os arch ext <<< "$target"
        [ "$os" = "$native_os" ] && [ "$arch" = "$native_arch" ] && continue
        echo "  Skipping $os/$arch (needs native libs on that platform)"
    done

    popd > /dev/null
    ok "All builds in: build/"
}

cmd_fetcher() {
    ensure_venv
    step "Installing fetcher dependencies"
    install_requirements "$FETCHER_DIR/requirements.txt"
}

cmd_run_fetcher() {
    ensure_venv
    step "Running feed fetcher"
    python "$FETCHER_DIR/main.py"
}

cmd_fetch_once() {
    ensure_venv
    step "Fetching articles (one-shot)"
    python "$FETCHER_DIR/main.py" --once
}

cmd_run_server() {
    local exe="$BUILD_DIR/rss-lance-server"
    if [ ! -f "$exe" ]; then
        fail "Server not built yet. Run: ./build.sh server"
    fi
    step "Starting HTTP server"
    exec "$exe"
}

cmd_migrate() {
    # Copy migrate scripts from source if needed (one-off operation)
    if [ ! -f "$MIGRATE_DIR/common.py" ]; then
        if [ -d "$SOURCE_ROOT/migrate" ]; then
            mkdir -p "$MIGRATE_DIR"
            cp "$SOURCE_ROOT/migrate/"* "$MIGRATE_DIR/"
            echo "  Copied migrate/ scripts"
        else
            fail "migrate/ directory not found in source or project."
        fi
    fi

    ensure_venv
    step "Installing migration dependencies"
    install_requirements "$MIGRATE_DIR/requirements.txt"

    echo ""
    ok "Migration deps installed. Run an importer directly:"
    echo "  python migrate/import_ttrss.py          # TT-RSS (Postgres)"
    echo "  python migrate/import_freshrss.py       # FreshRSS (API)"
    echo "  python migrate/import_miniflux.py       # Miniflux (API)"
    echo "  python migrate/import_opml.py <file>    # OPML (any reader)"
    echo ""
    echo "See docs/importing.md for configuration details."
}

cmd_migrate_cleanup() {
    step "Cleaning up migration files"

    # Remove migrate/ directory
    if [ -d "$MIGRATE_DIR" ]; then
        rm -rf "$MIGRATE_DIR"
        ok "  Removed migrate/"
    else
        echo "  migrate/ not found (already clean)"
    fi

    # Uninstall migration-only deps
    ensure_venv
    echo "  Uninstalling migration dependencies ..."
    pip uninstall -y psycopg2-binary tqdm requests 2>/dev/null || true
    ok "  Done."
}

cmd_duckdb() {
    step "Downloading DuckDB CLI"
    mkdir -p "$TOOLS_DIR"
    local duck_bin="$TOOLS_DIR/duckdb"
    if [ -f "$duck_bin" ]; then
        ok "duckdb already exists in tools/"
        return
    fi
    local ver="v1.5.0"
    local arch
    arch=$(uname -m)
    local os_name
    os_name=$(uname -s | tr '[:upper:]' '[:lower:]')
    # DuckDB uses "osx" not "darwin" in release URLs
    if [ "$os_name" = "darwin" ]; then
        os_name="osx"
        # DuckDB uses "universal" for macOS (fat binary)
        arch="universal"
    fi
    local url="https://github.com/duckdb/duckdb/releases/download/${ver}/duckdb_cli-${os_name}-${arch}.zip"
    echo "  Downloading DuckDB $ver for ${os_name}-${arch} ..."
    curl -fSL -o "$TOOLS_DIR/duckdb.zip" "$url"
    unzip -o "$TOOLS_DIR/duckdb.zip" -d "$TOOLS_DIR"
    rm -f "$TOOLS_DIR/duckdb.zip"
    chmod +x "$duck_bin" 2>/dev/null || true
    if [ -f "$duck_bin" ]; then
        ok "Installed: tools/duckdb"
    else
        fail "duckdb not found after extraction"
    fi
}

cmd_demo_data() {
    ensure_venv
    step "Inserting demo RSS feeds"
    python "$FETCHER_DIR/demo_feeds.py" --data "$DATA_DIR"
}

cmd_test() {
    step "Running test suite"
    local test_script="$SOURCE_ROOT/test.sh"
    if [ ! -f "$test_script" ]; then
        echo "  test.sh not found at $test_script" >&2
        return 1
    fi
    bash "$test_script" all
}

cmd_clean() {
    step "Cleaning build artifacts"
    rm -rf "$BUILD_DIR"
    ok "Cleaned."
}

cmd_help() {
    cat <<EOF

RSS-Lance Build Script
======================
Usage: ./build.sh [-d <dir>] [--no-tests] <command>

Options:
  -d <dir>       Project directory to work in (default: script location)
  --no-tests     Skip running tests after build (tests run by default with 'all')

Commands:
  setup        First-time setup (venv + deps + Go check)
  server       Build Go server for current platform
  server-all   Cross-compile server for Windows/Linux/macOS/FreeBSD
  fetcher      Install Python fetcher dependencies
  run-fetcher  Run the feed fetcher daemon
  fetch-once   Fetch articles once and exit
  run-server   Run the HTTP server
  demo-data    Insert demo RSS feeds into LanceDB for testing
  duckdb       Download DuckDB CLI into tools/
  migrate      Install migration deps (then run an importer directly)
  migrate-cleanup  Remove migrate scripts and their deps
  test         Run all test suites (or use test.sh directly)
  clean        Remove build artifacts
  build-minimum  Bare minimum to run the app (setup + duckdb + server)
               No tests, no demo data, no Node.js needed
  all          Full build (setup + duckdb + server + demo-data + tests)
               Use --no-tests to skip tests: ./build.sh --no-tests all
  help         Show this help

Examples:
  ./build.sh setup                       # Use script directory
  ./build.sh -d /opt/rss-lance all       # Build in a specific dir
  ./build.sh --no-tests all              # Build without running tests
  ./build.sh test                        # Run tests only

EOF
}

case "${1:-help}" in
    setup)       cmd_setup ;;
    server)      cmd_server ;;
    server-all)  cmd_server_all ;;
    fetcher)     cmd_fetcher ;;
    run-fetcher) cmd_run_fetcher ;;
    fetch-once)  cmd_fetch_once ;;
    run-server)  cmd_run_server ;;
    demo-data)   cmd_demo_data ;;
    duckdb)      cmd_duckdb ;;
    migrate)     cmd_migrate ;;
    migrate-cleanup) cmd_migrate_cleanup ;;
    test)        cmd_test ;;
    clean)       cmd_clean ;;
    build-minimum)
        cmd_setup; cmd_duckdb; cmd_server
        echo ""
        ok "Minimum build complete. Your app is ready to run:"
        echo "  1. Fetch articles:  ./run.sh fetch-once"
        echo "  2. Start server:    ./run.sh server"
        echo "  3. Open browser:    http://127.0.0.1:8080"
        echo ""
        echo "Optional: insert demo feeds with  ./build.sh demo-data"
        ;;
    all)
        cmd_setup; cmd_duckdb; cmd_server; cmd_demo_data
        if [ "$NO_TESTS" = false ]; then cmd_test; fi
        ;;
    help|*)      cmd_help ;;
esac

# Remind user to cd into the project directory when -d was used
if [ "$USING_CUSTOM_DIR" = true ] && [ "${1:-help}" != "help" ]; then
    echo ""
    echo "NOTE: Your project directory is self-contained at:"
    echo "    cd $PROJECT_ROOT"
    echo "  Use run.sh for daily commands (fetch-once, server, etc.)"
    echo "  You can delete the source repo - the app will keep working."
fi
