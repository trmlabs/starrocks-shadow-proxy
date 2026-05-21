#!/usr/bin/env bash
# test-pg-local.sh — Smoke test for the Postgres shadow proxy.
#
# Spins up the docker-compose.pg.yaml stack, waits for the proxy to become
# healthy, runs a few queries through the proxy and direct against primary,
# verifies the rows match, and dumps a slice of /metrics so a human can
# eyeball the per-query counters.
#
# Requires: docker compose, psql client, curl, jq.
#
# Usage:
#   ./test-pg-local.sh           # runs the full cycle and tears down at exit
#   ./test-pg-local.sh --keep    # leaves the stack running for manual poking

set -euo pipefail

KEEP=false
if [[ "${1:-}" == "--keep" ]]; then
  KEEP=true
fi

COMPOSE_FILE="docker-compose.pg.yaml"
PROXY_HOST=127.0.0.1
PROXY_PORT=5432
PRIMARY_PORT=15432
SHADOW_PORT=25432
METRICS_URL="http://127.0.0.1:9090/metrics"

cleanup() {
  if [[ "${KEEP}" == false ]]; then
    echo "Tearing down stack..."
    docker compose -f "${COMPOSE_FILE}" down -v >/dev/null 2>&1 || true
  else
    echo "Stack left running (--keep). To stop:"
    echo "  docker compose -f ${COMPOSE_FILE} down -v"
  fi
}
trap cleanup EXIT

echo "==> Starting docker-compose stack..."
docker compose -f "${COMPOSE_FILE}" up --build -d

echo "==> Waiting for proxy /health (up to 60s)..."
for i in {1..60}; do
  if curl -sf "${METRICS_URL%/metrics}/health" >/dev/null 2>&1; then
    echo "    proxy healthy after ${i}s"
    break
  fi
  sleep 1
  if [[ $i -eq 60 ]]; then
    echo "ERROR: proxy did not become healthy within 60s"
    docker compose -f "${COMPOSE_FILE}" logs shadow-proxy
    exit 1
  fi
done

export PGPASSWORD=trmlabs

run_psql() {
  local port=$1
  shift
  psql -h "${PROXY_HOST}" -p "${port}" -U postgres -d trm \
    --set=ON_ERROR_STOP=1 --no-psqlrc \
    -t -A "$@"
}

echo "==> Running through proxy (port ${PROXY_PORT})..."
proxy_one=$(run_psql "${PROXY_PORT}" -c "SELECT 1+1;")
proxy_now=$(run_psql "${PROXY_PORT}" -c "SELECT now()::timestamp(0);")
proxy_ver=$(run_psql "${PROXY_PORT}" -c "SHOW server_version;")
echo "    proxy SELECT 1+1     -> ${proxy_one}"
echo "    proxy SELECT now()   -> ${proxy_now}"
echo "    proxy server_version -> ${proxy_ver}"

echo "==> Running directly against primary (port ${PRIMARY_PORT})..."
direct_one=$(run_psql "${PRIMARY_PORT}" -c "SELECT 1+1;")
direct_ver=$(run_psql "${PRIMARY_PORT}" -c "SHOW server_version;")
echo "    direct SELECT 1+1     -> ${direct_one}"
echo "    direct server_version -> ${direct_ver}"

echo "==> Asserting parity..."
if [[ "${proxy_one}" != "${direct_one}" ]]; then
  echo "FAIL: SELECT 1+1 mismatch (proxy=${proxy_one} direct=${direct_one})"
  exit 1
fi
if [[ "${proxy_ver}" != "${direct_ver}" ]]; then
  echo "FAIL: server_version mismatch (proxy=${proxy_ver} direct=${direct_ver})"
  exit 1
fi
echo "    parity OK"

echo "==> Exercising prepared statements (extended protocol)..."
run_psql "${PROXY_PORT}" -c "PREPARE p1 (int) AS SELECT \$1 * 2; EXECUTE p1(21); DEALLOCATE p1;" \
  || { echo "FAIL: extended protocol path"; exit 1; }
echo "    extended protocol OK"

echo "==> Sampling proxy metrics..."
metrics_excerpt=$(curl -s "${METRICS_URL}" \
  | grep -E '^shadow_proxy_(pg_commands_total|pg_packets_total|queries_total|query_duration_seconds_count|active_connections|connections_total)' \
  | head -30)
echo "${metrics_excerpt}"

if ! echo "${metrics_excerpt}" | grep -q 'shadow_proxy_pg_commands_total'; then
  echo "FAIL: did not see shadow_proxy_pg_commands_total in /metrics"
  exit 1
fi
if ! echo "${metrics_excerpt}" | grep -q 'shadow_proxy_queries_total{target="primary"}'; then
  echo "FAIL: did not see shadow_proxy_queries_total{target=\"primary\"} in /metrics"
  exit 1
fi

echo "==> All checks passed."
