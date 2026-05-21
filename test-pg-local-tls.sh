#!/usr/bin/env bash
# test-pg-local-tls.sh — TLS-required smoke test for the Postgres shadow proxy.
#
# Mirrors AlloyDB's "TLS-only" posture. Backend Postgres containers are
# configured with hostssl-only pg_hba (see conf/pg_hba_tls.conf), so a
# plaintext connection attempt is rejected at the protocol layer with the
# exact error the staging AlloyDB cluster returned on 2026-05-21:
#   "no pg_hba.conf entry for host ..., no encryption"
#
# Until the proxy implements TLS termination, the proxy-path queries WILL
# fail. That is the expected baseline: this script exists so that as the
# pgproto3 / TLS termination diff comes together, we can iterate locally
# without touching staging.
#
# Phases:
#   1. Direct sslmode=require   → pass (proves the test env is alive)
#   2. Direct sslmode=disable   → must fail with "no encryption" (proves we
#                                   replicate AlloyDB's posture)
#   3. Proxy  sslmode=require   → pass once TLS termination lands
#   4. Proxy  sslmode=disable   → must fail once TLS-only enforcement is wired
#
# Usage:
#   ./certs/generate-certs.sh             # one-time
#   ./test-pg-local-tls.sh                # full cycle, tears down on exit
#   ./test-pg-local-tls.sh --keep         # leaves the stack up for manual poking
#   ./test-pg-local-tls.sh --skip-proxy   # only run direct-backend phases
#                                            (use until proxy TLS support lands)

set -euo pipefail

KEEP=false
SKIP_PROXY=false
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=true ;;
    --skip-proxy) SKIP_PROXY=true ;;
    *) echo "Unknown arg: $arg"; exit 2 ;;
  esac
done

COMPOSE_FILE="docker-compose.pg-tls.yaml"
PRIMARY_HOST=127.0.0.1
PROXY_PORT=5433
PRIMARY_PORT=15433
SHADOW_PORT=25433
METRICS_URL="http://127.0.0.1:9091/metrics"

if [[ ! -f certs/server.crt || ! -f certs/server.key ]]; then
  echo "ERROR: ./certs/server.crt and ./certs/server.key are required."
  echo "Run ./certs/generate-certs.sh first."
  exit 1
fi

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
if [[ "${SKIP_PROXY}" == true ]]; then
  # Skip the proxy build (which can fail behind corporate TLS-intercepting
  # proxies like Zscaler). Backends are vanilla postgres images, no build
  # required.
  docker compose -f "${COMPOSE_FILE}" up -d certs-init postgres-primary-tls postgres-shadow-tls
else
  docker compose -f "${COMPOSE_FILE}" up --build -d
fi

# Wait for backends to be healthy. The proxy will be brought up too but its
# health is reported separately and we tolerate it being unready (current
# transparent-forward proxy will be unhealthy against a TLS-required backend).
echo "==> Waiting for TLS backends to become healthy (up to 90s)..."
for i in {1..90}; do
  primary_healthy=$(docker inspect --format='{{.State.Health.Status}}' pg-shadow-proxy-primary-tls 2>/dev/null || echo "starting")
  shadow_healthy=$(docker inspect --format='{{.State.Health.Status}}' pg-shadow-proxy-shadow-tls 2>/dev/null || echo "starting")
  if [[ "$primary_healthy" == "healthy" && "$shadow_healthy" == "healthy" ]]; then
    echo "    primary + shadow healthy after ${i}s"
    break
  fi
  sleep 1
  if [[ $i -eq 90 ]]; then
    echo "ERROR: backends did not become healthy within 90s"
    docker compose -f "${COMPOSE_FILE}" ps
    docker compose -f "${COMPOSE_FILE}" logs postgres-primary-tls
    exit 1
  fi
done

export PGPASSWORD=trmlabs
PSQL_BASE_ARGS=(-h "${PRIMARY_HOST}" -U postgres -d trm --set=ON_ERROR_STOP=1 --no-psqlrc -t -A)

echo
echo "==> Phase 1: direct sslmode=require (must pass)"
ssl_one=$(psql "host=${PRIMARY_HOST} port=${PRIMARY_PORT} user=postgres dbname=trm sslmode=require" \
    --set=ON_ERROR_STOP=1 --no-psqlrc -t -A \
    -c "SELECT 'direct-tls' AS path, current_user, current_setting('ssl');" || true)
if ! echo "${ssl_one}" | grep -q "^direct-tls|"; then
  echo "FAIL: direct sslmode=require did not return expected row"
  echo "Got: ${ssl_one}"
  exit 1
fi
echo "    OK — ${ssl_one}"

echo
echo "==> Phase 2: direct sslmode=disable (must fail with 'no encryption')"
set +e
plain_err=$(psql "host=${PRIMARY_HOST} port=${PRIMARY_PORT} user=postgres dbname=trm sslmode=disable" \
    --set=ON_ERROR_STOP=1 --no-psqlrc -c "SELECT 1;" 2>&1)
plain_rc=$?
set -e
if [[ $plain_rc -eq 0 ]]; then
  echo "FAIL: direct sslmode=disable unexpectedly succeeded. pg_hba allows plaintext?"
  echo "Output: ${plain_err}"
  exit 1
fi
if ! echo "${plain_err}" | grep -qi "no encryption\|no pg_hba.conf entry"; then
  echo "FAIL: direct sslmode=disable failed for the wrong reason"
  echo "Got: ${plain_err}"
  exit 1
fi
echo "    OK — refused with 'no encryption' (matches AlloyDB posture)"

if [[ "${SKIP_PROXY}" == true ]]; then
  echo
  echo "==> --skip-proxy specified, stopping after backend phases."
  echo "    Direct-backend TLS posture verified. The proxy stack is up; you can"
  echo "    iterate on TLS-termination code with the backends in this state."
  exit 0
fi

echo
echo "==> Waiting for proxy /health (up to 30s; may stay unhealthy in PR #1)..."
proxy_ok=false
for i in {1..30}; do
  if curl -sf "${METRICS_URL%/metrics}/health" >/dev/null 2>&1; then
    proxy_ok=true
    echo "    proxy responding after ${i}s"
    break
  fi
  sleep 1
done
if [[ "$proxy_ok" == false ]]; then
  echo "    proxy never came up — that's expected if the build doesn't yet"
  echo "    support TLS termination. Run with --skip-proxy to bypass."
  docker compose -f "${COMPOSE_FILE}" logs shadow-proxy-tls | tail -30
  exit 1
fi

echo
echo "==> Phase 3: proxy sslmode=require (passes once TLS termination is wired)"
set +e
proxy_out=$(psql "host=${PRIMARY_HOST} port=${PROXY_PORT} user=postgres dbname=trm sslmode=require" \
    --set=ON_ERROR_STOP=1 --no-psqlrc -t -A \
    -c "SELECT 'via-proxy-tls' AS path, current_user, current_setting('ssl');" 2>&1)
proxy_rc=$?
set -e
if [[ $proxy_rc -ne 0 ]]; then
  echo "FAIL (expected until TLS termination lands):"
  echo "${proxy_out}"
  echo
  echo "==> Diagnostic — proxy logs:"
  docker compose -f "${COMPOSE_FILE}" logs shadow-proxy-tls | tail -30
  exit 3
fi
echo "    OK — ${proxy_out}"

echo
echo "==> Phase 4: proxy sslmode=disable (must fail once TLS-only enforcement is wired)"
set +e
plain_proxy=$(psql "host=${PRIMARY_HOST} port=${PROXY_PORT} user=postgres dbname=trm sslmode=disable" \
    --set=ON_ERROR_STOP=1 --no-psqlrc -c "SELECT 1;" 2>&1)
plain_proxy_rc=$?
set -e
if [[ $plain_proxy_rc -eq 0 ]]; then
  echo "FAIL: proxy accepted plaintext — should refuse like the backend does"
  echo "Output: ${plain_proxy}"
  exit 1
fi
echo "    OK — proxy refused plaintext"

echo
echo "==> Sampling proxy metrics..."
curl -s "${METRICS_URL}" | grep -E '^shadow_proxy_(pg_commands_total|pg_packets_total|queries_total|connections_total)' | head -20

echo
echo "==> All TLS phases passed."
