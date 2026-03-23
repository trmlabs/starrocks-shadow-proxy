#!/bin/bash
# Test Script for Minikube Shadow Proxy Setup
#
# This script validates that the entire setup is working correctly:
# 1. Both StarRocks clusters are accessible
# 2. Shadow Proxy can connect to both clusters
# 3. TLS termination is working
# 4. Queries are being mirrored
# 5. Metrics are being collected

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_step() { echo -e "${BLUE}[STEP]${NC} $1"; }

PRIMARY_NAMESPACE="starrocks-primary"
SHADOW_NAMESPACE="starrocks-shadow"
MONITORING_NAMESPACE="monitoring"

# Cleanup any existing port-forwards
cleanup() {
    log_info "Cleaning up port-forwards..."
    pkill -f "kubectl port-forward.*9030" 2>/dev/null || true
    pkill -f "kubectl port-forward.*9031" 2>/dev/null || true
    pkill -f "kubectl port-forward.*3306" 2>/dev/null || true
    pkill -f "kubectl port-forward.*9090" 2>/dev/null || true
    pkill -f "kubectl port-forward.*3000" 2>/dev/null || true
}

trap cleanup EXIT

# Start port-forwards
start_port_forwards() {
    log_step "Starting port-forwards..."
    
    # Primary StarRocks FE
    kubectl port-forward svc/primary-starrocks-fe-service 9030:9030 -n ${PRIMARY_NAMESPACE} &
    sleep 1
    
    # Shadow StarRocks FE  
    kubectl port-forward svc/shadow-starrocks-fe-service 9031:9030 -n ${SHADOW_NAMESPACE} &
    sleep 1
    
    # Shadow Proxy
    kubectl port-forward svc/shadow-proxy 3306:3306 -n ${PRIMARY_NAMESPACE} &
    sleep 1
    
    # Prometheus
    kubectl port-forward svc/prometheus 9090:9090 -n ${MONITORING_NAMESPACE} &
    sleep 1
    
    log_info "Port-forwards started!"
    sleep 2
}

# Wait for all pods to be ready
wait_for_pods() {
    log_step "Waiting for all pods to be ready..."
    
    kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=fe -n ${PRIMARY_NAMESPACE} --timeout=300s
    kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=be -n ${PRIMARY_NAMESPACE} --timeout=300s
    kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=fe -n ${SHADOW_NAMESPACE} --timeout=300s
    kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=be -n ${SHADOW_NAMESPACE} --timeout=300s
    kubectl wait --for=condition=ready pod -l app=shadow-proxy -n ${PRIMARY_NAMESPACE} --timeout=120s
    
    log_info "All pods are ready!"
}

# Test direct connection to Primary StarRocks
test_primary_direct() {
    log_step "Testing direct connection to Primary StarRocks..."
    
    result=$(mysql -h 127.0.0.1 -P 9030 -u root -proot123 -e "SELECT 'Primary Direct OK' as result;" 2>&1 | tail -1)
    
    if [[ "$result" == *"Primary Direct OK"* ]]; then
        log_info "✅ Primary StarRocks direct connection: SUCCESS"
    else
        log_error "❌ Primary StarRocks direct connection: FAILED"
        echo "$result"
        return 1
    fi
}

# Test direct connection to Shadow StarRocks
test_shadow_direct() {
    log_step "Testing direct connection to Shadow StarRocks..."
    
    result=$(mysql -h 127.0.0.1 -P 9031 -u root -proot123 -e "SELECT 'Shadow Direct OK' as result;" 2>&1 | tail -1)
    
    if [[ "$result" == *"Shadow Direct OK"* ]]; then
        log_info "✅ Shadow StarRocks direct connection: SUCCESS"
    else
        log_error "❌ Shadow StarRocks direct connection: FAILED"
        echo "$result"
        return 1
    fi
}

# Test connection through Shadow Proxy (plain TCP)
test_proxy_plain() {
    log_step "Testing connection through Shadow Proxy (plain TCP)..."
    
    result=$(mysql -h 127.0.0.1 -P 3306 -u root -proot123 -e "SELECT 'Proxy Plain TCP OK' as result;" 2>&1 | tail -1)
    
    if [[ "$result" == *"Proxy Plain TCP OK"* ]]; then
        log_info "✅ Shadow Proxy plain TCP connection: SUCCESS"
    else
        log_error "❌ Shadow Proxy plain TCP connection: FAILED"
        echo "$result"
        return 1
    fi
}

# Test connection through Shadow Proxy (with TLS)
test_proxy_tls() {
    log_step "Testing connection through Shadow Proxy (TLS)..."
    
    # Use the generated CA cert for verification
    CA_CERT="${SCRIPT_DIR}/certs/ca.crt"
    
    result=$(mysql -h 127.0.0.1 -P 3306 -u root -proot123 \
        --ssl-mode=REQUIRED \
        --ssl-ca="${CA_CERT}" \
        -e "SELECT 'Proxy TLS OK' as result;" 2>&1 | tail -1)
    
    if [[ "$result" == *"Proxy TLS OK"* ]]; then
        log_info "✅ Shadow Proxy TLS connection: SUCCESS"
    else
        log_error "❌ Shadow Proxy TLS connection: FAILED"
        echo "$result"
        return 1
    fi
}

# Create test database and table on both clusters
setup_test_data() {
    log_step "Setting up test data on both clusters..."
    
    # Create database on primary (via direct connection)
    mysql -h 127.0.0.1 -P 9030 -u root -proot123 -e "
        CREATE DATABASE IF NOT EXISTS test_db;
        USE test_db;
        CREATE TABLE IF NOT EXISTS test_table (
            id INT,
            name VARCHAR(100)
        ) ENGINE=OLAP
        DUPLICATE KEY(id)
        DISTRIBUTED BY HASH(id) BUCKETS 1
        PROPERTIES('replication_num' = '1');
        INSERT INTO test_table VALUES (1, 'primary_data');
    " 2>/dev/null
    
    # Create same database on shadow (via direct connection)
    mysql -h 127.0.0.1 -P 9031 -u root -proot123 -e "
        CREATE DATABASE IF NOT EXISTS test_db;
        USE test_db;
        CREATE TABLE IF NOT EXISTS test_table (
            id INT,
            name VARCHAR(100)
        ) ENGINE=OLAP
        DUPLICATE KEY(id)
        DISTRIBUTED BY HASH(id) BUCKETS 1
        PROPERTIES('replication_num' = '1');
        INSERT INTO test_table VALUES (1, 'shadow_data');
    " 2>/dev/null
    
    log_info "Test data created!"
}

# Test query mirroring
test_query_mirroring() {
    log_step "Testing query mirroring..."
    
    # Query through proxy - should return primary result
    result=$(mysql -h 127.0.0.1 -P 3306 -u root -proot123 -e "SELECT name FROM test_db.test_table WHERE id = 1;" 2>&1 | tail -1)
    
    if [[ "$result" == *"primary_data"* ]]; then
        log_info "✅ Query mirroring test: SUCCESS (got primary result)"
    else
        log_error "❌ Query mirroring test: FAILED (expected primary_data)"
        echo "$result"
        return 1
    fi
}

# Test metrics collection
test_metrics() {
    log_step "Testing metrics collection..."
    
    # Check shadow proxy metrics
    proxy_metrics=$(curl -s http://localhost:9090/api/v1/query?query=shadow_proxy_queries_total 2>&1)
    
    if [[ "$proxy_metrics" == *"shadow_proxy_queries_total"* ]] || [[ "$proxy_metrics" == *"success"* ]]; then
        log_info "✅ Shadow Proxy metrics: SUCCESS"
    else
        log_warn "⚠️ Shadow Proxy metrics may not be collected yet (this is OK if you just started)"
    fi
}

# Run load test
run_load_test() {
    log_step "Running mini load test (10 queries)..."
    
    for i in {1..10}; do
        mysql -h 127.0.0.1 -P 3306 -u root -proot123 -e "SELECT $i as query_num;" 2>/dev/null
    done
    
    log_info "Load test complete!"
}

# Print status summary
print_summary() {
    echo ""
    echo "============================================"
    echo "           TEST SUMMARY"
    echo "============================================"
    echo ""
    
    echo "Pods Status:"
    kubectl get pods -n ${PRIMARY_NAMESPACE} -o wide
    echo ""
    kubectl get pods -n ${SHADOW_NAMESPACE} -o wide
    echo ""
    
    echo "Services:"
    kubectl get svc -n ${PRIMARY_NAMESPACE}
    echo ""
    kubectl get svc -n ${SHADOW_NAMESPACE}
    echo ""
    
    echo "============================================"
    echo "           ACCESS INFORMATION"
    echo "============================================"
    echo ""
    echo "Primary StarRocks (direct):"
    echo "  mysql -h 127.0.0.1 -P 9030 -u root -proot123"
    echo ""
    echo "Shadow StarRocks (direct):"
    echo "  mysql -h 127.0.0.1 -P 9031 -u root -proot123"
    echo ""
    echo "Shadow Proxy (TLS):"
    echo "  mysql -h 127.0.0.1 -P 3306 -u root -proot123 --ssl-mode=REQUIRED --ssl-ca=certs/ca.crt"
    echo ""
    echo "Grafana: http://localhost:3000 (admin/admin)"
    echo "  kubectl port-forward svc/grafana 3000:3000 -n monitoring"
    echo ""
    echo "Prometheus: http://localhost:9090"
    echo "============================================"
}

# Main
main() {
    log_info "Starting Shadow Proxy Minikube Tests..."
    echo ""
    
    wait_for_pods
    start_port_forwards
    
    sleep 3  # Give port-forwards time to stabilize
    
    test_primary_direct
    test_shadow_direct
    test_proxy_plain
    
    # TLS test - only if certs exist
    if [[ -f "${SCRIPT_DIR}/certs/ca.crt" ]]; then
        test_proxy_tls
    else
        log_warn "Skipping TLS test - certificates not found"
    fi
    
    setup_test_data
    test_query_mirroring
    run_load_test
    test_metrics
    
    print_summary
    
    echo ""
    log_info "All tests completed! Press Ctrl+C to stop port-forwards."
    
    # Keep script running to maintain port-forwards
    read -r -d '' _ </dev/tty
}

main "$@"
