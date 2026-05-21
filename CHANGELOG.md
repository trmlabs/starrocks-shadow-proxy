# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Postgres / pgwire transparent-forward proxy mode (`PROTOCOL=postgres`).
  - Hand-rolled pgwire v3 packet reader (no new external dependencies).
  - Per-query timing via ReadyForQuery correlation; falls back to bidirectional
    `io.Copy` for COPY connections.
  - New Prometheus metrics: `shadow_proxy_pg_commands_total`,
    `shadow_proxy_pg_packets_total`. Existing query-duration / query-error /
    bytes metrics are reused with `target="primary"`.
  - `docker-compose.pg.yaml` for local testing against `postgres:15` (primary)
    and `postgres:18` (shadow) — wire-compatible with AlloyDB Omni.
  - `test-pg-local.sh` smoke test exercising simple + extended-protocol queries.
  - Docs: `docs/POSTGRES.md` describing scope, env vars, and design tradeoffs.

### Not yet in pgwire path

- Shadow mirroring (PR #2 will wire up `ShadowWorker`).
- TLS termination (clients must use `sslmode=disable`).
- Multi-replica / HA.
- Connection-level sampling.

## [1.0.0] - 2026-03-23

Initial open-source release.

### Features

- MySQL wire-protocol proxy with packet-level forwarding (no TCP fragmentation on shadow)
- 1:1 shadow worker per client connection with bounded queue (10K packets) and backpressure
- TLS termination for client connections (MySQL protocol SSL upgrade / STARTTLS)
- Optional TLS for shadow connections (proxy as TLS client)
- Protocol-aware response parsing for accurate latency measurement
- Graceful drain on client disconnect (configurable timeout, default 60s)
- Prometheus metrics: latency histograms, query counts, error rates, connection stats, queue depth
- Optional per-query logging to GCS as JSONL with Hive-style partitioning (for BigQuery or other analytics)
- Health and readiness endpoints (`/health`, `/ready`, `/status`)
- Docker Compose environments for local testing (with and without TLS)
- Minikube setup for Kubernetes testing with StarRocks Operator
- Grafana dashboard for monitoring
