#!/bin/bash
# Load test script for shadow proxy
# Generates SELECT queries to produce P90/P95/P99 metrics
#
# Usage:
#   ./load-test.sh              # Run 100 queries
#   ./load-test.sh 500          # Run 500 queries
#   ./load-test.sh continuous   # Run continuously until Ctrl+C

set -e

PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
PROXY_PORT="${PROXY_PORT:-3306}"
NUM_QUERIES="${1:-100}"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }

run_query() {
    local query_num=$1
    # Mix of fast and slow queries
    local delay=$(echo "scale=2; $((RANDOM % 10)) / 100" | bc)
    mysql -h "$PROXY_HOST" -P "$PROXY_PORT" -u root -N -e "SELECT $query_num, NOW(), SLEEP($delay)" 2>/dev/null
}

run_batch() {
    local count=$1
    log_info "Running $count queries through proxy at $PROXY_HOST:$PROXY_PORT..."
    
    local start_time=$(date +%s)
    
    for i in $(seq 1 $count); do
        run_query $i &
        
        # Rate limit to ~10 QPS
        if [ $((i % 10)) -eq 0 ]; then
            wait
            echo -ne "\r  Progress: $i / $count queries"
        fi
    done
    wait
    
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    
    echo ""
    log_info "Completed $count queries in ${duration}s ($(echo "scale=1; $count / $duration" | bc) QPS)"
}

run_continuous() {
    log_info "Running continuous load test (Ctrl+C to stop)..."
    log_info "Target: $PROXY_HOST:$PROXY_PORT"
    
    local count=0
    trap 'echo ""; log_info "Stopped after $count queries"; exit 0' INT
    
    while true; do
        for i in {1..10}; do
            run_query $count &
            ((count++))
        done
        wait
        echo -ne "\r  Queries sent: $count"
        sleep 1
    done
}

show_metrics() {
    echo ""
    log_info "Current Proxy Metrics:"
    curl -s http://localhost:9090/metrics 2>/dev/null | grep -E "^shadow_proxy_(queries_total|query_duration_seconds_count)" | head -10
    echo ""
    log_info "View detailed metrics at:"
    echo "  - Grafana Dashboard: http://localhost:3000/d/shadow-proxy-comparison"
    echo "  - Prometheus: http://localhost:9091"
}

# Main
case "$NUM_QUERIES" in
    continuous|c)
        run_continuous
        ;;
    *)
        if [[ "$NUM_QUERIES" =~ ^[0-9]+$ ]]; then
            run_batch "$NUM_QUERIES"
            show_metrics
        else
            echo "Usage: $0 [num_queries|continuous]"
            echo "  num_queries: Number of queries to run (default: 100)"
            echo "  continuous:  Run continuously until Ctrl+C"
            exit 1
        fi
        ;;
esac
