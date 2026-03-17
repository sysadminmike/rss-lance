#!/usr/bin/env bash
# RSS-Lance test runner with clear per-test PASS/FAIL output.
#
# Usage:
#   ./test.sh            # Run all tests
#   ./test.sh python     # Python fetcher tests only
#   ./test.sh go         # Go API + DB tests only
#   ./test.sh frontend   # Frontend tests only (requires Node.js)
#   ./test.sh backend    # Python + Go (no frontend)
#   ./test.sh database   # Python DB integration tests only
#   ./test.sh api        # Go API tests only

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")" && pwd)"
SERVER_DIR="$PROJECT_ROOT/server"

GREEN='\033[0;32m'
RED='\033[0;31m'
CYAN='\033[0;36m'
YELLOW='\033[0;33m'
GRAY='\033[0;90m'
DARKRED='\033[0;31m'
NC='\033[0m'

TOTAL_PASSED=0
TOTAL_FAILED=0
TOTAL_SKIPPED=0
FAILED_TESTS=()
SUITES_RUN=()
SUITE="${1:-all}"

# ── Helpers ────────────────────────────────────────────────────────────────────

section() {
    echo ""
    printf "${CYAN}%s${NC}\n" "======================================================================"
    printf "${CYAN}  %s${NC}\n" "$1"
    printf "${CYAN}%s${NC}\n" "======================================================================"
    echo ""
}

subsection() {
    echo ""
    printf "${CYAN}  --- %s ---${NC}\n" "$1"
    echo ""
}

pass() {
    printf "${GREEN}  [PASS] %s${NC}\n" "$1"
    ((TOTAL_PASSED++)) || true
}

fail() {
    printf "${RED}  [FAIL] %s${NC}\n" "$1"
    if [ -n "${2:-}" ]; then
        while IFS= read -r line; do
            [ -n "$line" ] && printf "${DARKRED}         %s${NC}\n" "$line"
        done <<< "$2"
    fi
    ((TOTAL_FAILED++)) || true
    FAILED_TESTS+=("$1")
}

skip() {
    printf "${YELLOW}  [SKIP] %s${NC}\n" "$1"
    ((TOTAL_SKIPPED++)) || true
}

# ── Python Tests ───────────────────────────────────────────────────────────────

run_python_tests() {
    local test_paths=("${@:-fetcher/tests/}")
    local label="Python Fetcher Tests"
    if [ "${#test_paths[@]}" -eq 1 ] && [ "${test_paths[0]}" != "fetcher/tests/" ]; then
        label="Python Tests (${test_paths[0]})"
    fi

    section "$label"
    SUITES_RUN+=("$label")

    local python="$PROJECT_ROOT/.venv/bin/python"
    if [ ! -f "$python" ]; then
        printf "${YELLOW}  [SKIP] Python venv not found at .venv/${NC}\n"
        printf "${YELLOW}         Run: ./build.sh setup${NC}\n"
        return
    fi

    "$python" -m pytest --version &>/dev/null || {
        printf "${YELLOW}  Installing pytest...${NC}\n"
        "$PROJECT_ROOT/.venv/bin/pip" install pytest -q 2>/dev/null
    }

    cd "$PROJECT_ROOT"

    local raw_output
    raw_output=$("$python" -m pytest "${test_paths[@]}" -v --tb=short --no-header 2>&1) || true

    local in_failure=false
    local fail_lines=""
    local fail_test=""

    while IFS= read -r line; do
        local trimmed
        trimmed=$(echo "$line" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')

        if [[ "$trimmed" =~ ^(.+)[[:space:]]+(PASSED|FAILED|SKIPPED|ERROR)[[:space:]]*$ ]]; then
            # Flush pending failure
            if $in_failure && [ -n "$fail_test" ]; then
                fail "$fail_test" "$fail_lines"
                in_failure=false
                fail_lines=""
            fi

            local test_name="${BASH_REMATCH[1]}"
            local result="${BASH_REMATCH[2]}"

            # Clean up path for display
            test_name=$(echo "$test_name" | sed 's|^fetcher/tests/||; s|\.py::|  >  |')

            case "$result" in
                PASSED)  pass "$test_name" ;;
                SKIPPED) skip "$test_name" ;;
                FAILED)
                    fail_test="$test_name"
                    in_failure=true
                    fail_lines=""
                    ;;
                ERROR)
                    fail "$test_name" "Collection/import error"
                    ;;
            esac
        elif $in_failure; then
            if [[ "$trimmed" =~ ^(FAILURES|={5,}|-{5,}|[0-9]+\ (passed|failed)) ]]; then
                fail "$fail_test" "$fail_lines"
                in_failure=false
                fail_lines=""
                fail_test=""
            elif [ -n "$trimmed" ]; then
                fail_lines+="$trimmed"$'\n'
            fi
        fi
    done <<< "$raw_output"

    # Flush trailing
    if $in_failure && [ -n "$fail_test" ]; then
        fail "$fail_test" "$fail_lines"
    fi
}

# ── Go Tests ───────────────────────────────────────────────────────────────────

run_go_test_package() {
    local pkg="$1"
    local label="$2"

    subsection "$label"

    cd "$SERVER_DIR"

    local raw_output
    raw_output=$(go test "./$pkg/" -v -count=1 -timeout 300s 2>&1) || true

    local in_fail=false
    local fail_block=""
    local current_test=""

    while IFS= read -r line; do
        if [[ "$line" =~ ^[[:space:]]*---\ PASS:\ ([^[:space:]]+)\ \( ]]; then
            if $in_fail && [ -n "$current_test" ]; then
                fail "$pkg/${current_test}" "$fail_block"
                in_fail=false; fail_block=""
            fi
            pass "$pkg/${BASH_REMATCH[1]}"
        elif [[ "$line" =~ ^[[:space:]]*---\ FAIL:\ ([^[:space:]]+)\ \( ]]; then
            if $in_fail && [ -n "$current_test" ]; then
                fail "$pkg/${current_test}" "$fail_block"
            fi
            current_test="${BASH_REMATCH[1]}"
            in_fail=true
            fail_block=""
        elif [[ "$line" =~ ^[[:space:]]*---\ SKIP:\ ([^[:space:]]+)\ \( ]]; then
            if $in_fail && [ -n "$current_test" ]; then
                fail "$pkg/${current_test}" "$fail_block"
                in_fail=false; fail_block=""
            fi
            skip "$pkg/${BASH_REMATCH[1]}"
        elif $in_fail; then
            if [[ "$line" =~ ^(FAIL|ok)[[:space:]] ]] || [[ "$line" =~ ^---\ (PASS|FAIL|SKIP): ]]; then
                fail "$pkg/${current_test}" "$fail_block"
                in_fail=false; fail_block=""
            else
                fail_block+="$line"$'\n'
            fi
        fi
    done <<< "$raw_output"

    if $in_fail && [ -n "$current_test" ]; then
        fail "$pkg/${current_test}" "$fail_block"
    fi

    # Detect build failures
    if echo "$raw_output" | grep -q 'FAIL.*\[build failed\]'; then
        fail "$pkg (build)" "Compilation failed"
    fi
}

run_go_tests() {
    local packages=("${@:-api db}")

    section "Go Server Tests"
    SUITES_RUN+=("Go Server Tests")

    if ! command -v go &>/dev/null; then
        printf "${YELLOW}  [SKIP] Go not found${NC}\n"
        printf "${YELLOW}         Install from https://go.dev/dl/${NC}\n"
        return
    fi

    for pkg in ${packages[@]}; do
        run_go_test_package "$pkg" "Go $pkg/ package"
    done
}

# ── Frontend Tests ─────────────────────────────────────────────────────────────

run_frontend_tests() {
    section "Frontend Tests (Jest)"
    SUITES_RUN+=("Frontend Tests")

    if ! command -v npm &>/dev/null; then
        printf "${YELLOW}  [SKIP] Node.js/npm not found${NC}\n"
        printf "${YELLOW}         Install Node.js, then: cd frontend && npm install && npm test${NC}\n"
        return
    fi

    cd "$PROJECT_ROOT/frontend"

    if [ ! -d "node_modules" ]; then
        echo "  Installing dependencies..."
        npm install 2>/dev/null
    fi

    local raw_output
    raw_output=$(npx jest --verbose --no-color 2>&1) || true

    while IFS= read -r line; do
        # Jest individual test lines with checkmark/cross
        if [[ "$line" =~ ^[[:space:]]+[✓√]\ (.+)$ ]]; then
            local name="${BASH_REMATCH[1]}"
            name=$(echo "$name" | sed 's/ ([0-9]* *ms)$//')
            pass "$name"
        elif [[ "$line" =~ ^[[:space:]]+[✕×]\ (.+)$ ]]; then
            local name="${BASH_REMATCH[1]}"
            name=$(echo "$name" | sed 's/ ([0-9]* *ms)$//')
            fail "$name"
        fi
    done <<< "$raw_output"
}

# ── Main ───────────────────────────────────────────────────────────────────────

case "$SUITE" in
    python)   run_python_tests ;;
    go)       run_go_tests ;;
    frontend) run_frontend_tests ;;
    backend)  run_python_tests; run_go_tests ;;
    database) run_python_tests "fetcher/tests/test_db.py" ;;
    api)      run_go_tests "api" ;;
    all)
        run_python_tests
        run_go_tests
        run_frontend_tests
        ;;
    *)
        echo "Usage: $0 [all|python|go|frontend|backend|database|api]"
        exit 1
        ;;
esac

# ── Summary ────────────────────────────────────────────────────────────────────

TOTAL=$((TOTAL_PASSED + TOTAL_FAILED + TOTAL_SKIPPED))

echo ""
printf "${CYAN}%s${NC}\n" "======================================================================"
printf "${CYAN}  TEST SUMMARY${NC}\n"
printf "${CYAN}%s${NC}\n" "======================================================================"
echo ""

if [ ${#SUITES_RUN[@]} -gt 0 ]; then
    printf "${GRAY}  Suites:  %s${NC}\n" "$(IFS=', '; echo "${SUITES_RUN[*]}")"
fi
echo "  Total:   $TOTAL tests"

[ "$TOTAL_PASSED" -gt 0 ]  && printf "${GREEN}  Passed:  %d${NC}\n" "$TOTAL_PASSED"
[ "$TOTAL_FAILED" -gt 0 ]  && printf "${RED}  Failed:  %d${NC}\n" "$TOTAL_FAILED"
[ "$TOTAL_SKIPPED" -gt 0 ] && printf "${YELLOW}  Skipped: %d${NC}\n" "$TOTAL_SKIPPED"

if [ ${#FAILED_TESTS[@]} -gt 0 ]; then
    echo ""
    printf "${RED}  Failed tests:${NC}\n"
    for t in "${FAILED_TESTS[@]}"; do
        printf "${RED}    - %s${NC}\n" "$t"
    done
fi

echo ""
if [ "$TOTAL_FAILED" -eq 0 ] && [ "$TOTAL" -gt 0 ]; then
    printf "${GREEN}  ALL TESTS PASSED${NC}\n"
elif [ "$TOTAL_FAILED" -gt 0 ]; then
    printf "${RED}  SOME TESTS FAILED${NC}\n"
    echo ""
    exit 1
else
    printf "${YELLOW}  NO TESTS RUN${NC}\n"
fi
echo ""
