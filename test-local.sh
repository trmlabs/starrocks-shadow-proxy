#!/bin/bash
# Test local Docker environment
#
# Usage:
#   ./test-local.sh
#
# Starts the full environment (docker-compose.local.yaml) if not running,
# then runs basic connectivity and query tests.
#
# For filter-specific integration tests, use:
#   ./test-filter-integration.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

echo "=== Shadow Proxy Local Testing ==="
echo ""

# Check if containers are running
if ! docker ps | grep -q shadow-proxy; then
    echo "Starting Docker environment..."
    docker compose -f docker-compose.local.yaml up --build -d
    echo "Waiting for services to be ready (60s)..."
    sleep 60
fi

echo "=== Container Status ==="
docker ps --format "table {{.Names}}\t{{.Status}}" | grep -E "(primary|shadow|proxy|prometheus|grafana)"

echo ""
echo "=== Test 1: Connect via Proxy ==="
mysql -h 127.0.0.1 -P 3306 -u root -e "SELECT 'Proxy connection OK' as result;" 2>&1 || echo "FAILED"

echo ""
echo "=== Test 2: Connect directly to Primary ==="
mysql -h 127.0.0.1 -P 9030 -u root -e "SELECT 'Primary connection OK' as result;" 2>&1 || echo "FAILED"

echo ""
echo "=== Test 3: Connect directly to Shadow ==="
mysql -h 127.0.0.1 -P 9031 -u root -e "SELECT 'Shadow connection OK' as result;" 2>&1 || echo "FAILED"

echo ""
echo "=== Test 4: Run test query through proxy ==="
mysql -h 127.0.0.1 -P 3306 -u root -e "SELECT 'test query via proxy', NOW() as time;" 2>&1 || echo "FAILED"

echo ""
echo "=== Proxy Metrics ==="
curl -s http://127.0.0.1:9090/metrics | grep -E "shadow_proxy_(queries_total|query_errors_total|active_connections|shadow_filtered_total)" | grep -v "^#" | head -15 || echo "Could not fetch metrics"

echo ""
echo "=== Testing Complete ==="
echo ""
echo "Access points:"
echo "  Proxy:      mysql -h 127.0.0.1 -P 3306 -u root"
echo "  Primary:    mysql -h 127.0.0.1 -P 9030 -u root"
echo "  Shadow:     mysql -h 127.0.0.1 -P 9031 -u root"
echo "  Grafana:    http://localhost:3000 (admin/admin)"
echo "  Prometheus: http://localhost:9091"
echo ""
echo "For filter integration tests: ./test-filter-integration.sh"
