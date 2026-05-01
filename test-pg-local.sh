#!/usr/bin/env bash
set -euo pipefail

PROJECT_NAME="pgproto3-proxy"
COMPOSE_FILE="docker-compose.pg.yaml"
NETWORK_NAME="${PROJECT_NAME}_pg-net"
METRICS_URL="http://127.0.0.1:9090/metrics"
KEEP=false

if [[ "${1:-}" == "--keep" ]]; then
  KEEP=true
fi

cleanup() {
  if [[ "${KEEP}" == "false" ]]; then
    docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" down -v >/dev/null 2>&1 || true
  else
    echo "Stack left running. To stop:"
    echo "  docker compose -p ${PROJECT_NAME} -f ${COMPOSE_FILE} down -v"
  fi
}
trap cleanup EXIT

echo "==> Starting AlloyDB Omni + proxy stack"
echo "==> Building local Linux proxy binary with Dockerized Go"
docker run --rm \
  -v "${PWD}:/src" \
  -w /src \
  golang:1.24.12-alpine \
  sh -c 'apk add --no-cache git >/dev/null && CGO_ENABLED=0 go build -o starrocks-shadow-proxy .'

docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" up -d

echo "==> Waiting for proxy readiness"
for i in {1..90}; do
  if curl -sf "http://127.0.0.1:9090/ready" >/dev/null 2>&1; then
    break
  fi
  if [[ "${i}" -eq 90 ]]; then
    docker compose -p "${PROJECT_NAME}" -f "${COMPOSE_FILE}" logs shadow-proxy
    echo "proxy did not become ready"
    exit 1
  fi
  sleep 1
done

echo "==> Running pgx integration test through proxy"
docker run --rm \
  --network "${NETWORK_NAME}" \
  -v "${PWD}:/src" \
  -w /src \
  -e "PG_PROXY_DSN=postgres://postgres:trmlabs@shadow-proxy:5432/postgres?sslmode=disable" \
  golang:1.24.12-alpine \
  sh -c 'apk add --no-cache git >/dev/null && go test -tags integration -run TestPgProxyAlloyDBOmniSmoke -count=1 ./...'

echo "==> Checking Prometheus metrics"
metrics="$(curl -s "${METRICS_URL}" | grep -E '^shadow_proxy_(pg_commands_total|pg_packets_total|queries_total|query_duration_seconds_count)' || true)"
echo "${metrics}"
if ! grep -q 'shadow_proxy_pg_commands_total' <<<"${metrics}"; then
  echo "missing shadow_proxy_pg_commands_total"
  exit 1
fi
if ! grep -q 'shadow_proxy_queries_total{target="primary"}' <<<"${metrics}"; then
  echo "missing shadow_proxy_queries_total for primary"
  exit 1
fi

echo "==> Postgres proxy smoke test passed"
