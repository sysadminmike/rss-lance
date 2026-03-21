#!/usr/bin/env bash
#
# RSS-Lance runtime commands (daily use)
#
# Lightweight script for running the fetcher, server, and admin tasks.
# Activates the Python venv automatically. Does NOT handle building
# or first-time setup - use build.sh for that.
#
# Usage:
#   ./run.sh fetch-once          # Fetch articles once and exit
#   ./run.sh fetch-daemon        # Run fetcher continuously
#   ./run.sh server              # Start the HTTP server
#   ./run.sh demo-data           # Insert demo RSS feeds
#   ./run.sh add-feed <url>      # Add a single feed URL
#   ./run.sh datafix strip-chrome # Fix existing article data

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

VENV_PATH="$SCRIPT_DIR/.venv"
BUILD_DIR="$SCRIPT_DIR/build"
DATA_DIR="$SCRIPT_DIR/data"
FETCHER_DIR="$SCRIPT_DIR/fetcher"
DEBUG_FLAG=""
PORT_FLAG=""

ensure_venv() {
    if [ ! -f "$VENV_PATH/bin/python" ]; then
        echo "Python venv not found at: $VENV_PATH"
        echo "Run build.sh setup first."
        exit 1
    fi
    if [ ! -f "$VENV_PATH/bin/activate" ]; then
        echo "Venv exists but activate script missing at: $VENV_PATH"
        echo "Delete .venv/ and re-run: rm -rf .venv && ./build.sh setup"
        exit 1
    fi
    source "$VENV_PATH/bin/activate"
}

cmd_fetch_once() {
    ensure_venv
    echo "Fetching articles (one-shot) ..."
    python "$FETCHER_DIR/main.py" --once
}

cmd_fetch_daemon() {
    ensure_venv
    echo "Starting feed fetcher daemon ..."
    python "$FETCHER_DIR/main.py"
}

cmd_server() {
    local exe="$BUILD_DIR/rss-lance-server"
    if [ ! -f "$exe" ]; then
        echo "Server not built. Run build.sh server first."
        exit 1
    fi
    echo "Loading RSS-Lance server (please wait) ..."
    local args=()
    if [ -n "$DEBUG_FLAG" ]; then
        args+=("--debug" "$DEBUG_FLAG")
        echo "Debug: $DEBUG_FLAG"
    fi
    if [ -n "$PORT_FLAG" ]; then
        args+=("--port" "$PORT_FLAG")
        echo "Port override: $PORT_FLAG"
    fi
    exec "$exe" "${args[@]}"
}

cmd_demo_data() {
    ensure_venv
    echo "Inserting demo feeds ..."
    python "$FETCHER_DIR/demo_feeds.py" --data "$DATA_DIR"
}

cmd_export_opml() {
    if [ -z "${1:-}" ]; then
        echo "Usage: ./run.sh export-opml <output.opml>"
        echo "  Use '-' to write to stdout"
        exit 1
    fi
    ensure_venv
    echo "Exporting OPML ..."
    python "$SCRIPT_DIR/migrate/export_opml.py" "$@"
}

cmd_datafix() {
    if [ -z "${1:-}" ]; then
        ensure_venv
        python "$FETCHER_DIR/datafix.py" list
        return
    fi
    ensure_venv
    local fix_name="$1"
    shift
    echo "Running datafix: $fix_name"
    python "$FETCHER_DIR/datafix.py" "$fix_name" --data "$DATA_DIR" "$@"
}

cmd_add_feed() {
    if [ -z "${1:-}" ]; then
        echo "Usage: ./run.sh add-feed <url>"
        exit 1
    fi
    ensure_venv
    echo "Adding feed: $1"
    python "$FETCHER_DIR/main.py" --add "$1"
}

cmd_benchmark() {
    ensure_venv
    echo "Running benchmark ..."
    python "$SCRIPT_DIR/tests/benchmark.py" "$@"
}

cmd_help() {
    cat <<EOF

RSS-Lance Runtime Commands
==========================
Usage: ./run.sh <command> [args]

Commands:
  fetch-once     Fetch all due feeds once and exit
  fetch-daemon   Run the fetcher continuously on a schedule
  server         Start the HTTP server (http://127.0.0.1:8080)
  demo-data      Insert demo RSS feeds for testing
  add-feed <url> Add a single RSS/Atom feed URL
  export-opml <file> Export all feeds to an OPML file (use '-' for stdout)
  datafix <name> Run a data fix on existing articles (or 'datafix' to list)
  benchmark      Run insertion & read benchmarks (isolated temp DB)
  help           Show this help

Options:
  --debug <categories>  Enable debug logging (client,duckdb,batch,lance,all)
  --port <number>       Override server port from config.toml

Examples:
  ./run.sh --debug all server
  ./run.sh --port 9090 server
  ./run.sh --debug client,duckdb server

EOF
}

# Parse --debug flag before command dispatch
while [[ $# -gt 0 ]]; do
    case "$1" in
        --debug)
            DEBUG_FLAG="${2:-}"
            shift 2
            ;;
        --port)
            PORT_FLAG="${2:-}"
            shift 2
            ;;
        *)
            break
            ;;
    esac
done

case "${1:-help}" in
    fetch-once)   cmd_fetch_once ;;
    fetch-daemon) cmd_fetch_daemon ;;
    server)       cmd_server ;;
    demo-data)    cmd_demo_data ;;
    add-feed)     cmd_add_feed "${2:-}" ;;
    export-opml)  shift; cmd_export_opml "$@" ;;
    datafix)      shift; cmd_datafix "$@" ;;
    benchmark)    shift; cmd_benchmark "$@" ;;
    help|*)       cmd_help ;;
esac
