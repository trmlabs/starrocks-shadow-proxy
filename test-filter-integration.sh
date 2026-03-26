#!/bin/bash
# Integration test for shadow query filtering against real StarRocks.
# Uses docker-compose.local.yaml (StarRocks primary + shadow) as the backend.
#
# The script manages the proxy container — stopping and restarting it with
# different filter configurations between test phases. StarRocks backends
# stay running throughout.
#
# Prerequisites:
#   - Docker with compose plugin
#   - Go toolchain (to build the proxy)
#   - mysql client
#   - curl
#
# Usage:
#   ./test-filter-integration.sh              # Starts StarRocks if not running
#   SKIP_SETUP=1 ./test-filter-integration.sh # Skip StarRocks startup (already running)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

COMPOSE_FILE="docker-compose.local.yaml"
PROXY_IMAGE="${PROXY_IMAGE:-shadow-proxy:filter-test}"
NETWORK="starrocks-shadow-proxy_sr-network"
PROXY_HOST="127.0.0.1"
PROXY_PORT="3306"
METRICS_URL="http://127.0.0.1:9090/metrics"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

PASS_COUNT=0
FAIL_COUNT=0

pass() { ((PASS_COUNT++)); echo -e "  ${GREEN}✓ PASS${NC}: $1"; }
fail() { ((FAIL_COUNT++)); echo -e "  ${RED}✗ FAIL${NC}: $1"; }
info() { echo -e "${YELLOW}>>>${NC} $1"; }

get_metric() {
    local name="$1"
    local labels="${2:-}"
    local val
    if [ -n "$labels" ]; then
        val=$(curl -s "$METRICS_URL" | grep "^${name}{${labels}}" | awk '{print $2}' | head -1)
    else
        val=$(curl -s "$METRICS_URL" | grep "^${name} " | awk '{print $2}' | head -1)
    fi
    echo "${val:-0}"
}

sum_filtered() {
    curl -s "$METRICS_URL" | grep "^shadow_proxy_shadow_filtered_total" | grep -v "^#" | awk '{sum+=$2} END{print sum+0}'
}

run_query() {
    mysql -h "$PROXY_HOST" -P "$PROXY_PORT" -u root -e "$1" 2>/dev/null
}

wait_for_proxy() {
    for i in $(seq 1 30); do
        if curl -sf http://127.0.0.1:9090/health > /dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    fail "Proxy not ready after 30s"
    return 1
}

wait_for_query() {
    info "Verifying proxy can execute queries..."
    for i in $(seq 1 20); do
        if run_query "SELECT 1" > /dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    fail "Cannot execute queries through proxy"
    docker logs shadow-proxy 2>&1 | tail -5
    return 1
}

restart_proxy() {
    local description="$1"
    shift
    local extra_env=("${@:+$@}")

    info "Restarting proxy: $description"
    set +e; docker stop shadow-proxy > /dev/null 2>&1; docker rm shadow-proxy > /dev/null 2>&1; set -e

    local env_args=(
        -e PRIMARY_HOST=primary-fe -e PRIMARY_PORT=9030
        -e PRIMARY_USER=root -e PRIMARY_PASSWORD=
        -e SHADOW_HOST=shadow-fe -e SHADOW_PORT=9030
        -e SHADOW_USER=root -e SHADOW_PASSWORD=
        -e LISTEN_ADDR=:3306 -e METRICS_PORT=:9090
        -e DEBUG_LOG=true
    )
    if [ ${#extra_env[@]} -gt 0 ]; then
        for e in "${extra_env[@]}"; do
            env_args+=(-e "$e")
        done
    fi

    docker run -d --name shadow-proxy --network "$NETWORK" \
        -p 3306:3306 -p 9090:9090 \
        "${env_args[@]}" \
        "$PROXY_IMAGE" > /dev/null 2>&1

    wait_for_proxy
    wait_for_query
}

# ========================================================
# SETUP: Ensure StarRocks is running
# ========================================================
echo ""
echo -e "${BOLD}================================================================${NC}"
echo -e "${BOLD}  Shadow Proxy Filter Integration Test (Real StarRocks)${NC}"
echo -e "${BOLD}================================================================${NC}"
echo ""

# Build the proxy Docker image from source
if [ -z "${SKIP_BUILD:-}" ]; then
    info "Building proxy Docker image..."
    if docker compose -f "$COMPOSE_FILE" build shadow-proxy > /dev/null 2>&1; then
        # Tag so we can use it with `docker run` independently of compose
        docker tag "$(docker compose -f "$COMPOSE_FILE" images shadow-proxy -q 2>/dev/null || echo '')" "$PROXY_IMAGE" 2>/dev/null || true
        info "Image built: $PROXY_IMAGE"
    elif docker images --format '{{.Repository}}:{{.Tag}}' | grep -q "$PROXY_IMAGE"; then
        info "Docker build failed (network issue?), using existing image: $PROXY_IMAGE"
    else
        echo ""
        echo "ERROR: Docker build failed and no existing image found."
        echo "If you're behind a corporate proxy, build locally first:"
        echo "  CGO_ENABLED=0 GOOS=linux GOARCH=\$(go env GOARCH) go build -o /tmp/proxy ."
        echo "  docker build -f - -t $PROXY_IMAGE . <<< 'FROM alpine:3.21"
        echo "  RUN apk --no-cache add ca-certificates && adduser -D -u 1000 appuser"
        echo "  WORKDIR /app"
        echo "  COPY /tmp/proxy ./starrocks-shadow-proxy"
        echo "  USER appuser"
        echo "  ENTRYPOINT [\"./starrocks-shadow-proxy\"]'"
        exit 1
    fi
fi

if [ "${SKIP_SETUP:-}" != "1" ]; then
    if ! docker ps | grep -q primary-fe; then
        info "Starting StarRocks clusters (this takes ~60s first time)..."
        docker compose -f "$COMPOSE_FILE" up -d primary-fe shadow-fe > /dev/null 2>&1
        info "Waiting for FEs to be healthy..."
        for i in $(seq 1 30); do
            s1=$(docker inspect primary-fe --format '{{.State.Health.Status}}' 2>/dev/null || echo "?")
            s2=$(docker inspect shadow-fe --format '{{.State.Health.Status}}' 2>/dev/null || echo "?")
            if [ "$s1" = "healthy" ] && [ "$s2" = "healthy" ]; then break; fi
            sleep 5
        done
        docker compose -f "$COMPOSE_FILE" up -d primary-be shadow-be > /dev/null 2>&1
        sleep 20
        info "StarRocks ready"
    else
        info "StarRocks already running"
    fi
fi

# ========================================================
# PHASE 1: No Filter (baseline)
# ========================================================
echo ""
echo -e "${BOLD}========================================${NC}"
echo -e "${BOLD}PHASE 1: BASELINE (No Filter)${NC}"
echo -e "${BOLD}========================================${NC}"
echo ""

restart_proxy "no filter"

# Run a mix of queries
info "Sending queries..."
run_query "SELECT 1" > /dev/null
run_query "SELECT 2" > /dev/null
run_query "SELECT 3" > /dev/null
run_query "CREATE DATABASE IF NOT EXISTS filter_test" > /dev/null
run_query "CREATE TABLE IF NOT EXISTS filter_test.t1 (id INT, name VARCHAR(100), ts DATETIME) ENGINE=OLAP DUPLICATE KEY(id) DISTRIBUTED BY HASH(id) BUCKETS 1 PROPERTIES ('replication_num' = '1')" > /dev/null 2>&1 || true
run_query "INSERT INTO filter_test.t1 VALUES (1, 'alice', NOW()), (2, 'bob', NOW())" > /dev/null
run_query "SELECT * FROM filter_test.t1" > /dev/null
run_query "DELETE FROM filter_test.t1 WHERE id = 2" > /dev/null 2>&1 || true
run_query "SHOW TABLES FROM filter_test" > /dev/null

sleep 3

primary_q=$(get_metric "shadow_proxy_queries_total" 'target="primary"')
shadow_q=$(get_metric "shadow_proxy_queries_total" 'target="shadow"')
echo "  Primary: $primary_q  Shadow: $shadow_q"

if [ "$primary_q" -gt 0 ] && [ "$primary_q" -eq "$shadow_q" ]; then
    pass "Primary ($primary_q) == Shadow ($shadow_q) — all mirrored"
else
    fail "Primary ($primary_q) != Shadow ($shadow_q)"
fi

filtered_any=$(curl -s "$METRICS_URL" | grep "^shadow_proxy_shadow_filtered_total" | grep -v "^#" || echo "")
if [ -z "$filtered_any" ]; then
    pass "No filtered metrics (filter disabled)"
else
    fail "Unexpected filtered metric: $filtered_any"
fi

# ========================================================
# PHASE 2: SQL Operation Filter (exclude INSERT_OVERWRITE, SUBMIT_TASK)
# ========================================================
echo ""
echo -e "${BOLD}========================================${NC}"
echo -e "${BOLD}PHASE 2: OPERATION FILTER${NC}"
echo -e "${BOLD}  exclude INSERT_OVERWRITE, SUBMIT_TASK${NC}"
echo -e "${BOLD}========================================${NC}"
echo ""

restart_proxy "exclude INSERT_OVERWRITE,SUBMIT_TASK" \
    "SHADOW_FILTER_MODE=exclude" \
    "SHADOW_FILTER_SQL_OPERATIONS=INSERT_OVERWRITE,SUBMIT_TASK"

info "Sending allowed queries (should mirror)..."
run_query "SELECT 'mirror_1'" > /dev/null
run_query "SELECT * FROM filter_test.t1" > /dev/null
run_query "INSERT INTO filter_test.t1 VALUES (50, 'regular_insert', NOW())" > /dev/null
run_query "SHOW TABLES FROM filter_test" > /dev/null
run_query "SELECT COUNT(*) FROM filter_test.t1" > /dev/null

info "Sending filtered queries (should NOT mirror)..."
run_query "INSERT OVERWRITE filter_test.t1 SELECT 200, 'overwrite_simple', NOW()" > /dev/null 2>&1 || true
run_query "INSERT OVERWRITE filter_test.t1 (id, name, ts) SELECT id + 1000, CONCAT('etl_', name), NOW() FROM filter_test.t1 WHERE id < 100" > /dev/null 2>&1 || true
run_query "INSERT OVERWRITE filter_test.t1 SELECT 300, 'overwrite_col', NOW()" > /dev/null 2>&1 || true
run_query "SUBMIT TASK AS INSERT INTO filter_test.t1 SELECT 400, 'task', NOW()" > /dev/null 2>&1 || true
run_query "SUBMIT /*+set_var(query_timeout=300)*/ TASK AS INSERT OVERWRITE filter_test.t1 SELECT 500, 'hint_task', NOW()" > /dev/null 2>&1 || true

sleep 3

p2_primary=$(get_metric "shadow_proxy_queries_total" 'target="primary"')
p2_shadow=$(get_metric "shadow_proxy_queries_total" 'target="shadow"')
p2_filtered=$(sum_filtered)
echo "  Primary: $p2_primary  Shadow: $p2_shadow  Filtered: $p2_filtered"

if [ "$p2_primary" -gt "$p2_shadow" ]; then
    pass "Primary ($p2_primary) > Shadow ($p2_shadow) — filter working"
else
    fail "Primary ($p2_primary) should be > Shadow ($p2_shadow)"
fi

if [ "${p2_filtered%.*}" -ge 4 ]; then
    pass "Filtered $p2_filtered queries (expected ≥4)"
else
    fail "Expected ≥4 filtered, got $p2_filtered"
fi

gap=$((p2_primary - p2_shadow))
diff=$(( gap - ${p2_filtered%.*} ))
if [ "$diff" -ge -2 ] && [ "$diff" -le 2 ]; then
    pass "Parity: primary - shadow ($gap) ≈ filtered ($p2_filtered)"
else
    fail "Parity drift: gap=$gap, filtered=$p2_filtered, diff=$diff"
fi

reason=$(curl -s "$METRICS_URL" | grep "shadow_proxy_shadow_filtered_total.*sql_operation" | grep -v "^#" || echo "")
if [ -n "$reason" ]; then
    pass "Metric reason label = sql_operation"
else
    fail "Missing sql_operation reason label"
fi

log_count=$(docker logs shadow-proxy 2>&1 | grep -c "Query filtered from shadow" || true)
if [ "$log_count" -ge 4 ]; then
    pass "Debug logs show $log_count filtered entries"
else
    fail "Expected ≥4 log entries, got $log_count"
fi

docker logs shadow-proxy 2>&1 | grep "Shadow Query Filter:" | sed 's/^/    /' || true

# Show actual filter log lines for visibility
echo ""
echo "  Filter log entries:"
docker logs shadow-proxy 2>&1 | grep "Query filtered from shadow" | sed 's/^/    /'

# ========================================================
# PHASE 3: Pattern Filter (exclude information_schema)
# ========================================================
echo ""
echo -e "${BOLD}========================================${NC}"
echo -e "${BOLD}PHASE 3: PATTERN FILTER${NC}"
echo -e "${BOLD}  exclude: (?i)information_schema${NC}"
echo -e "${BOLD}========================================${NC}"
echo ""

restart_proxy "exclude pattern: information_schema" \
    "SHADOW_FILTER_MODE=exclude" \
    "SHADOW_FILTER_PATTERNS=(?i)information_schema"

info "Sending queries..."
run_query "SELECT 'normal_1'" > /dev/null
run_query "SELECT * FROM filter_test.t1 LIMIT 3" > /dev/null
run_query "SELECT * FROM information_schema.tables WHERE TABLE_SCHEMA = 'filter_test'" > /dev/null 2>&1 || true
run_query "SELECT COLUMN_NAME FROM information_schema.columns WHERE TABLE_NAME = 't1' AND TABLE_SCHEMA = 'filter_test'" > /dev/null 2>&1 || true
run_query "SELECT 'normal_2'" > /dev/null

sleep 3

p3_primary=$(get_metric "shadow_proxy_queries_total" 'target="primary"')
p3_shadow=$(get_metric "shadow_proxy_queries_total" 'target="shadow"')
p3_filtered=$(sum_filtered)
echo "  Primary: $p3_primary  Shadow: $p3_shadow  Filtered: $p3_filtered"

if [ "$p3_primary" -gt "$p3_shadow" ]; then
    pass "Primary ($p3_primary) > Shadow ($p3_shadow) — pattern filter working"
else
    fail "Primary ($p3_primary) should be > Shadow ($p3_shadow)"
fi

if [ "${p3_filtered%.*}" -ge 2 ]; then
    pass "Pattern filter caught $p3_filtered queries"
else
    fail "Expected ≥2 pattern-filtered, got $p3_filtered"
fi

pattern_reason=$(curl -s "$METRICS_URL" | grep "shadow_proxy_shadow_filtered_total.*pattern" | grep -v "^#" || echo "")
if [ -n "$pattern_reason" ]; then
    pass "Metric reason label = pattern"
else
    fail "Missing pattern reason label"
fi

# ========================================================
# PHASE 4: Include-only SELECT
# ========================================================
echo ""
echo -e "${BOLD}========================================${NC}"
echo -e "${BOLD}PHASE 4: INCLUDE ONLY SELECT${NC}"
echo -e "${BOLD}========================================${NC}"
echo ""

restart_proxy "include SELECT only" \
    "SHADOW_FILTER_MODE=include" \
    "SHADOW_FILTER_SQL_OPERATIONS=SELECT"

info "Sending queries..."
run_query "SELECT 'should_mirror'" > /dev/null
run_query "SELECT * FROM filter_test.t1 LIMIT 1" > /dev/null
run_query "INSERT INTO filter_test.t1 VALUES (999, 'should_filter', NOW())" > /dev/null 2>&1 || true
run_query "SHOW TABLES FROM filter_test" > /dev/null 2>&1 || true
run_query "SELECT 'also_mirror'" > /dev/null

sleep 3

p4_primary=$(get_metric "shadow_proxy_queries_total" 'target="primary"')
p4_shadow=$(get_metric "shadow_proxy_queries_total" 'target="shadow"')
p4_filtered=$(sum_filtered)
echo "  Primary: $p4_primary  Shadow: $p4_shadow  Filtered: $p4_filtered"

if [ "$p4_primary" -gt "$p4_shadow" ]; then
    pass "Primary > Shadow — only SELECTs mirrored"
else
    fail "Primary ($p4_primary) should be > Shadow ($p4_shadow)"
fi

if [ "${p4_filtered%.*}" -ge 1 ]; then
    pass "Non-SELECT queries filtered ($p4_filtered)"
else
    fail "Expected ≥1 filtered in include mode"
fi

# ========================================================
# SUMMARY
# ========================================================
echo ""
echo -e "${BOLD}========================================${NC}"
echo -e "${BOLD}RESULTS: $PASS_COUNT passed, $FAIL_COUNT failed${NC}"
echo -e "${BOLD}========================================${NC}"
echo ""

if [ "$FAIL_COUNT" -gt 0 ]; then
    echo -e "${RED}SOME TESTS FAILED${NC}"
    exit 1
else
    echo -e "${GREEN}ALL TESTS PASSED${NC}"
    exit 0
fi
